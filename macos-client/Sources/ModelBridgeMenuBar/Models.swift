import Foundation

// MARK: - API 数据模型

public struct AuditStats: Codable {
    public let total_requests: Int
    public let input_tokens: Int
    public let output_tokens: Int
    public let cache_read_tokens: Int
    public let cache_creation_tokens: Int
    public let error_count: Int
    public let success_count: Int

    public var totalTokens: Int {
        input_tokens + output_tokens + cache_read_tokens
    }

    public var cacheRate: Double {
        let total = input_tokens + cache_read_tokens
        guard total > 0 else { return 0 }
        return (Double(cache_read_tokens) / Double(total)) * 100.0
    }
}

public struct ChannelHealthInfo: Codable, Identifiable {
    public let id: String
    public let name: String
    public let state: String
    public let avg_first_byte_ms: Int?
    public let avg_latency_ms: Int?
    public let success_rate: Int?
    public let total_requests: Int
    public let recent_failures: Int
    public let input_tokens: Int
    public let output_tokens: Int
    public let cache_read_tokens: Int
    public let cache_creation_tokens: Int
}

public struct ChannelEditData: Codable, Identifiable, Equatable {
    public let id: String
    public var name: String
    public var provider: String
    public var url: String
    public var api_key: String
    public var priority: Int
    public var enabled: Bool
    public var fallback_model: String
    public var model_mapping: [String: String]
    public var timeout_ms: Int
    public var custom_headers: [String: String]?
    public var strip_thinking: Bool?
    public var retry_count: Int?
    public var retry_delay_ms: Int?
    public var force_effort: String?
    public var auto_cache: Bool?
}

public struct FullConfigResponse: Codable {
    public let listen_port: Int
    public var channels: [ChannelEditData]
}
