//! 供应商工具函数模块。
//!
//! 历史上此模块承载各供应商的「发送 + 协议转换」逻辑，现已由 `crate::converter`
//! 统一处理协议互转、由 `crate::proxy::executor` 统一处理请求发送。
//! 此处仅保留 executor 仍在复用的 header 收集/序列化工具。

use crate::channel::config::ChannelConfig;
use axum::http::HeaderMap;

/// 将 reqwest HeaderMap 序列化为 JSON 字符串
pub fn headers_to_json(headers: &reqwest::header::HeaderMap) -> String {
    let map: std::collections::HashMap<String, String> = headers.iter()
        .filter_map(|(k, v)| v.to_str().ok().map(|val| (k.to_string(), val.to_string())))
        .collect();
    serde_json::to_string(&map).unwrap_or_else(|_| "{}".to_string())
}

/// 收集透传 headers 和 custom_headers 到 HashMap（覆盖语义：后写覆盖前写）。
/// 跳过 hop-by-hop headers 和认证头（认证头由调用方单独设置）。
pub fn collect_forwarded_headers(
    headers: &mut std::collections::HashMap<String, String>,
    caller_headers: &HeaderMap,
    channel: &ChannelConfig,
) {
    let skip_headers = ["host", "content-length", "transfer-encoding", "connection",
                        "authorization", "x-api-key", "accept-encoding"];

    for (key, value) in caller_headers.iter() {
        let name = key.as_str().to_lowercase();
        if skip_headers.contains(&name.as_str()) {
            continue;
        }
        if let Ok(v) = value.to_str() {
            headers.insert(name, v.to_string());
        }
    }

    for (key, value) in &channel.custom_headers {
        headers.insert(key.to_lowercase(), value.clone());
    }
}
