import SwiftUI

struct ContentHeightPreferenceKey: PreferenceKey {
    static var defaultValue: CGFloat = 0
    static func reduce(value: inout CGFloat, nextValue: () -> CGFloat) {
        value = max(value, nextValue())
    }
}

struct MainView: View {
    @ObservedObject var vm: MenuBarViewModel
    @State private var contentHeight: CGFloat = 300

    var body: some View {
        VStack(spacing: 0) {
            // Header
            HStack {
                HStack(spacing: 8) {
                    RoundedRectangle(cornerRadius: 6)
                        .fill(LinearGradient(colors: [.blue, .indigo], startPoint: .topLeading, endPoint: .bottomTrailing))
                        .frame(width: 26, height: 26)
                        .overlay(Text("MB").font(.system(size: 11, weight: .bold)).foregroundColor(.white))
                    
                    VStack(alignment: .leading, spacing: 1) {
                        Text("Model Bridge")
                            .font(.system(size: 13, weight: .bold))
                        HStack(spacing: 4) {
                            Circle()
                                .fill(vm.isConnected ? Color.green : Color.red)
                                .frame(width: 6, height: 6)
                            Text(vm.isConnected ? vm.serverUrl.replacingOccurrences(of: "https?://", with: "", options: .regularExpression) : "未连接")
                                .font(.system(size: 10))
                                .foregroundColor(.secondary)
                                .lineLimit(1)
                        }
                    }
                }
                
                Spacer()
                
                HStack(spacing: 4) {
                    Button(action: { vm.fetchData() }) {
                        Image(systemName: "arrow.clockwise")
                            .font(.system(size: 12))
                    }
                    .buttonStyle(.plain)
                    .frame(width: 24, height: 24)
                    .help("刷新数据")
                    
                    Button(action: {
                        withAnimation(.spring(response: 0.28, dampingFraction: 0.85)) {
                            vm.showSettings.toggle()
                        }
                    }) {
                        Image(systemName: "gearshape")
                            .font(.system(size: 12))
                            .foregroundColor(vm.showSettings ? .blue : .primary)
                    }
                    .buttonStyle(.plain)
                    .frame(width: 24, height: 24)
                    .help("远程设置")
                    
                    Button(action: {
                        NSApplication.shared.terminate(nil)
                    }) {
                        Image(systemName: "power")
                            .font(.system(size: 12))
                            .foregroundColor(.secondary)
                    }
                    .buttonStyle(.plain)
                    .frame(width: 24, height: 24)
                    .help("退出程序")
                }
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 10)
            .background(Color(NSColor.windowBackgroundColor).opacity(0.6))
            
            Divider()
            
            // 动态高度主体区域
            if vm.showSettings {
                SettingsView(vm: vm)
                    .padding(14)
                    .transition(.opacity.combined(with: .move(edge: .top)))
            } else {
                ScrollView(.vertical, showsIndicators: contentHeight > 500) {
                    VStack(spacing: 12) {
                        // Hero Token Card
                        TokenHeroCard(stats: vm.stats)
                        
                        // Channels Reordering Section
                        VStack(alignment: .leading, spacing: 6) {
                            HStack {
                                Text("渠道与调用优先级")
                                    .font(.system(size: 11, weight: .bold))
                                    .foregroundColor(.secondary)
                                    .textCase(.uppercase)
                                Spacer()
                                Text("按住 ⠿ 拖拽调整优先级")
                                    .font(.system(size: 10))
                                    .foregroundColor(.blue)
                            }
                            
                            // Channel Drag and Drop List
                            VStack(spacing: 6) {
                                ForEach(vm.channels) { channel in
                                    ChannelRowView(
                                        channel: channel,
                                        health: vm.healthMap[channel.id],
                                        onToggle: { vm.toggleChannel(channel) },
                                        onDropTarget: { sourceId in
                                            withAnimation(.spring(response: 0.25, dampingFraction: 0.8)) {
                                                vm.moveChannel(sourceId: sourceId, targetId: channel.id)
                                            }
                                        }
                                    )
                                }
                            }
                        }
                    }
                    .padding(14)
                    .background(
                        GeometryReader { proxy in
                            Color.clear.preference(
                                key: ContentHeightPreferenceKey.self,
                                value: proxy.size.height
                            )
                        }
                    )
                }
                .frame(height: max(180, min(contentHeight, 520)))
                .onPreferenceChange(ContentHeightPreferenceKey.self) { newHeight in
                    withAnimation(.spring(response: 0.28, dampingFraction: 0.85)) {
                        self.contentHeight = newHeight
                    }
                }
            }
            
            Divider()
            
            // Footer
            HStack {
                let activeCount = vm.channels.filter { $0.enabled }.count
                Text("\(activeCount) / \(vm.channels.count) 渠道活跃")
                    .font(.system(size: 11, design: .monospaced))
                    .foregroundColor(.secondary)
                
                Spacer()
                
                Button(action: {
                    if let url = URL(string: vm.serverUrl) {
                        NSWorkspace.shared.open(url)
                    }
                }) {
                    HStack(spacing: 3) {
                        Text("打开远端控制台")
                        Image(systemName: "arrow.up.right")
                    }
                    .font(.system(size: 11, weight: .medium))
                    .foregroundColor(.blue)
                }
                .buttonStyle(.plain)
            }
            .padding(.horizontal, 14)
            .padding(.vertical, 8)
            .background(Color(NSColor.windowBackgroundColor).opacity(0.4))
        }
        .frame(width: 380)
        .animation(.spring(response: 0.28, dampingFraction: 0.85), value: vm.showSettings)
        .animation(.spring(response: 0.28, dampingFraction: 0.85), value: vm.channels.count)
    }
}

// MARK: - Subviews

struct TokenHeroCard: View {
    let stats: AuditStats?

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("今日 Token 概览")
                    .font(.system(size: 11, weight: .bold))
                    .foregroundColor(.secondary)
                    .textCase(.uppercase)
                Spacer()
                Text(String(format: "%.1f%% Cache Rate", stats?.cacheRate ?? 0))
                    .font(.system(size: 10, weight: .semibold))
                    .foregroundColor(.cyan)
                    .padding(.horizontal, 6)
                    .padding(.vertical, 2)
                    .background(Color.cyan.opacity(0.12))
                    .cornerRadius(4)
            }
            
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(FormatUtils.formatTokens(stats?.totalTokens))
                    .font(.system(size: 24, weight: .bold, design: .monospaced))
                Text("Tokens")
                    .font(.system(size: 12))
                    .foregroundColor(.secondary)
                
                if let total = stats?.totalTokens, total >= 10_000 {
                    Text("(\(FormatUtils.formatNumberWithComma(total)))")
                        .font(.system(size: 10, design: .monospaced))
                        .foregroundColor(.secondary.opacity(0.8))
                }
            }
            
            // Progress segment
            GeometryReader { geo in
                let total = max(1, stats?.totalTokens ?? 1)
                let inW = (Double(stats?.input_tokens ?? 0) / Double(total)) * geo.size.width
                let outW = (Double(stats?.output_tokens ?? 0) / Double(total)) * geo.size.width
                let cacheW = (Double(stats?.cache_read_tokens ?? 0) / Double(total)) * geo.size.width
                
                HStack(spacing: 2) {
                    Rectangle().fill(Color.blue).frame(width: inW)
                    Rectangle().fill(Color.purple).frame(width: outW)
                    Rectangle().fill(Color.green).frame(width: cacheW)
                }
                .cornerRadius(3)
            }
            .frame(height: 5)
            
            // Stats detail row
            HStack {
                VStack(alignment: .leading, spacing: 1) {
                    Text("输入 Prompt").font(.system(size: 9)).foregroundColor(.secondary)
                    Text(FormatUtils.formatTokens(stats?.input_tokens)).font(.system(size: 11, weight: .medium, design: .monospaced))
                }
                Spacer()
                VStack(alignment: .leading, spacing: 1) {
                    Text("输出 Completion").font(.system(size: 9)).foregroundColor(.secondary)
                    Text(FormatUtils.formatTokens(stats?.output_tokens)).font(.system(size: 11, weight: .medium, design: .monospaced))
                }
                Spacer()
                VStack(alignment: .leading, spacing: 1) {
                    Text("缓存命中 Read").font(.system(size: 9)).foregroundColor(.secondary)
                    Text(FormatUtils.formatTokens(stats?.cache_read_tokens)).font(.system(size: 11, weight: .medium, design: .monospaced))
                }
            }
            .padding(.top, 4)
        }
        .padding(12)
        .background(RoundedRectangle(cornerRadius: 10).fill(Color(NSColor.controlBackgroundColor).opacity(0.7)))
        .overlay(RoundedRectangle(cornerRadius: 10).stroke(Color.primary.opacity(0.08), lineWidth: 1))
    }
}

struct ChannelRowView: View {
    let channel: ChannelEditData
    let health: ChannelHealthInfo?
    let onToggle: () -> Void
    let onDropTarget: (String) -> Void

    @State private var isTargeted: Bool = false

    var body: some View {
        HStack(spacing: 8) {
            // Drag Handle Icon
            Image(systemName: "line.3.horizontal")
                .foregroundColor(isTargeted ? .blue : .secondary.opacity(0.8))
                .font(.system(size: 12, weight: .semibold))
                .frame(width: 14)
            
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 4) {
                    Circle()
                        .fill(channel.enabled ? (health?.state == "unhealthy" ? Color.red : Color.green) : Color.gray)
                        .frame(width: 6, height: 6)
                    Text(channel.name)
                        .font(.system(size: 12, weight: .semibold))
                        .lineLimit(1)
                    Text(channel.provider.uppercased())
                        .font(.system(size: 8, weight: .bold, design: .monospaced))
                        .padding(.horizontal, 4)
                        .padding(.vertical, 1)
                        .background(Color.blue.opacity(0.1))
                        .foregroundColor(.blue)
                        .cornerRadius(3)
                }
                
                HStack(spacing: 6) {
                    Text("⏱ \(FormatUtils.formatLatency(health?.avg_latency_ms))")
                    Text("•")
                    Text("\(FormatUtils.formatTokens((health?.input_tokens ?? 0) + (health?.output_tokens ?? 0))) tok")
                    Text("•")
                    Text("\(FormatUtils.formatNumberWithComma(health?.total_requests)) req")
                }
                .font(.system(size: 10, design: .monospaced))
                .foregroundColor(.secondary)
            }
            
            Spacer()
            
            Toggle("", isOn: Binding(
                get: { channel.enabled },
                set: { _ in onToggle() }
            ))
            .toggleStyle(.switch)
            .controlSize(.mini)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 8)
        .background(
            RoundedRectangle(cornerRadius: 8)
                .fill(isTargeted ? Color.blue.opacity(0.15) : Color(NSColor.controlBackgroundColor).opacity(0.5))
        )
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .stroke(isTargeted ? Color.blue : Color.primary.opacity(0.06), lineWidth: isTargeted ? 2 : 1)
        )
        .opacity(channel.enabled ? 1.0 : 0.6)
        .draggable(channel.id) {
            // 拖拽悬浮预览
            HStack(spacing: 6) {
                Image(systemName: "line.3.horizontal")
                Text(channel.name).font(.system(size: 12, weight: .bold))
            }
            .padding(8)
            .background(Color(NSColor.windowBackgroundColor))
            .cornerRadius(8)
            .shadow(radius: 6)
        }
        .dropDestination(for: String.self) { items, _ in
            guard let droppedId = items.first, droppedId != channel.id else { return false }
            onDropTarget(droppedId)
            return true
        } isTargeted: { targeted in
            withAnimation(.easeInOut(duration: 0.15)) {
                self.isTargeted = targeted
            }
        }
    }
}

struct SettingsView: View {
    @ObservedObject var vm: MenuBarViewModel

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("远程网关设置")
                .font(.system(size: 12, weight: .bold))
            
            VStack(alignment: .leading, spacing: 4) {
                Text("远程服务器 URL:").font(.system(size: 11)).foregroundColor(.secondary)
                TextField("http://ip:8080 或 https://domain", text: $vm.serverUrl)
                    .textFieldStyle(.roundedBorder)
            }
            
            VStack(alignment: .leading, spacing: 4) {
                Text("Admin Token / API Key:").font(.system(size: 11)).foregroundColor(.secondary)
                SecureField("留空表示不需要", text: $vm.adminToken)
                    .textFieldStyle(.roundedBorder)
            }
            
            HStack {
                Button("完成") {
                    withAnimation(.spring(response: 0.28, dampingFraction: 0.85)) {
                        vm.showSettings = false
                    }
                    vm.fetchData()
                }
                .buttonStyle(.borderedProminent)
                .controlSize(.small)
                
                Button("重置为本地") {
                    vm.serverUrl = "http://localhost:8080"
                    vm.adminToken = ""
                }
                .controlSize(.small)
            }
            .padding(.top, 4)
        }
    }
}
