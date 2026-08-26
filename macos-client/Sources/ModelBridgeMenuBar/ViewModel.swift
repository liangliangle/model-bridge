import Foundation
import SwiftUI

@MainActor
public class MenuBarViewModel: ObservableObject {
    @AppStorage("remote_server_url") public var serverUrl: String = "http://localhost:8080"
    @AppStorage("admin_token") public var adminToken: String = ""

    @Published public var stats: AuditStats?
    @Published public var channels: [ChannelEditData] = []
    @Published public var healthMap: [String: ChannelHealthInfo] = [:]
    @Published public var isConnected: Bool = false
    @Published public var isRefreshing: Bool = false
    @Published public var errorMessage: String?
    @Published public var showSettings: Bool = false

    private var pollTimer: Timer?

    public init() {
        startPolling()
    }

    public var formattedTotalTokens: String {
        FormatUtils.formatTokens(stats?.totalTokens, compact: true)
    }

    public func startPolling() {
        pollTimer?.invalidate()
        fetchData()
        pollTimer = Timer.scheduledTimer(withTimeInterval: 5.0, repeats: true) { [weak self] _ in
            Task { @MainActor [weak self] in
                self?.fetchData()
            }
        }
    }

    public func fetchData() {
        Task {
            isRefreshing = true
            defer { isRefreshing = false }
            
            let base = serverUrl.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            guard let statsUrl = URL(string: "\(base)/api/stats?period=today"),
                  let healthUrl = URL(string: "\(base)/api/channels/health?period=today"),
                  let configUrl = URL(string: "\(base)/api/config") else {
                errorMessage = "无效的服务器 URL"
                isConnected = false
                return
            }

            var request = URLRequest(url: statsUrl)
            if !adminToken.isEmpty {
                request.addValue("Bearer \(adminToken)", forHTTPHeaderField: "Authorization")
            }

            do {
                // Fetch stats
                let (statsData, statsResp) = try await URLSession.shared.data(for: request)
                if (statsResp as? HTTPURLResponse)?.statusCode == 200 {
                    self.stats = try JSONDecoder().decode(AuditStats.self, from: statsData)
                    self.isConnected = true
                    self.errorMessage = nil
                } else {
                    self.isConnected = false
                }

                // Fetch health
                var healthReq = URLRequest(url: healthUrl)
                if !adminToken.isEmpty { healthReq.addValue("Bearer \(adminToken)", forHTTPHeaderField: "Authorization") }
                let (healthData, _) = try await URLSession.shared.data(for: healthReq)
                if let healthList = try? JSONDecoder().decode([ChannelHealthInfo].self, from: healthData) {
                    var map: [String: ChannelHealthInfo] = [:]
                    for h in healthList { map[h.id] = h }
                    self.healthMap = map
                }

                // Fetch channels config
                var configReq = URLRequest(url: configUrl)
                if !adminToken.isEmpty { configReq.addValue("Bearer \(adminToken)", forHTTPHeaderField: "Authorization") }
                let (configData, _) = try await URLSession.shared.data(for: configReq)
                if let config = try? JSONDecoder().decode(FullConfigResponse.self, from: configData) {
                    self.channels = config.channels.sorted(by: { $0.priority < $1.priority })
                }
            } catch {
                self.isConnected = false
                self.errorMessage = error.localizedDescription
            }
        }
    }

    /// 拖拽重排序后保存到远程服务器 (按 channel ID 移动)
    public func moveChannel(sourceId: String, targetId: String) {
        guard let sourceIndex = channels.firstIndex(where: { $0.id == sourceId }),
              let targetIndex = channels.firstIndex(where: { $0.id == targetId }),
              sourceIndex != targetIndex else { return }
        
        let item = channels.remove(at: sourceIndex)
        channels.insert(item, at: targetIndex)
        for i in 0..<channels.count {
            channels[i].priority = i + 1
        }
        syncChannelsToRemote()
    }

    /// 切换渠道启用/停用
    public func toggleChannel(_ channel: ChannelEditData) {
        if let idx = channels.firstIndex(where: { $0.id == channel.id }) {
            channels[idx].enabled.toggle()
            syncChannel(channels[idx])
        }
    }

    private func syncChannel(_ channel: ChannelEditData) {
        Task {
            let base = serverUrl.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            guard let url = URL(string: "\(base)/api/config/channel") else { return }
            var req = URLRequest(url: url)
            req.httpMethod = "POST"
            req.addValue("application/json", forHTTPHeaderField: "Content-Type")
            if !adminToken.isEmpty { req.addValue("Bearer \(adminToken)", forHTTPHeaderField: "Authorization") }
            req.httpBody = try? JSONEncoder().encode(channel)
            _ = try? await URLSession.shared.data(for: req)
        }
    }

    private func syncChannelsToRemote() {
        for ch in channels {
            syncChannel(ch)
        }
    }
}
