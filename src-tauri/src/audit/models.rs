use serde::{Deserialize, Serialize};

/// 审计日志完整条目（从数据库读取）
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuditEntry {
    pub id: i64,
    pub timestamp: i64, // UTC 毫秒时间戳
    pub method: String,
    pub path: String,
    pub request_headers: Option<String>,
    pub forwarded_request_headers: Option<String>,
    pub request_body: Option<String>,
    pub forwarded_request_body: Option<String>,
    pub alias_model: String,
    pub actual_channel: Option<String>,
    pub actual_model: Option<String>,
    pub mapping_source: Option<String>,
    pub upstream_response_headers: Option<String>,
    pub response_headers: Option<String>,
    pub upstream_response_body: Option<String>,
    pub response_body: Option<String>,
    pub status_code: Option<u16>,
    pub first_byte_ms: Option<u64>,
    pub latency_ms: Option<u64>,
    pub input_tokens: Option<u64>,
    pub output_tokens: Option<u64>,
    pub cache_read_tokens: Option<u64>,
    pub cache_creation_tokens: Option<u64>,
    pub retry_count: u32,
    pub failover_chain: Option<String>,
    pub error_message: Option<String>,
    pub user_agent: Option<String>,
}
