//! MCP 中继的请求头处理：双向透传 Mcp-Session-Id / MCP-Protocol-Version 等，
//! 并按渠道配置注入或透传 Authorization。

use axum::http::HeaderMap;
use reqwest::header;

use crate::channel::config::McpServerConfig;

/// 不应透传给上游的 hop-by-hop / 自动管理的请求头
const SKIP_HEADERS: &[&str] = &[
    "host",
    "content-length",
    "transfer-encoding",
    "connection",
    "accept-encoding", // 避免上游返回 gzip，缓冲 tools/list 时还要解压
    "authorization",   // 单独处理（注入或透传）
];

/// 构建转发到上游 MCP server 的请求头：
/// 1. 透传客户端原始 headers（跳过 hop-by-hop 和 authorization）
///    —— Mcp-Session-Id / MCP-Protocol-Version / Accept 都会被原样带上
/// 2. Authorization：auth_token 有值则注入 Bearer，否则透传客户端的 Authorization
/// 3. 叠加渠道自定义 headers（最后，可覆盖前面的）
pub fn build_mcp_forward_headers(
    mut builder: reqwest::RequestBuilder,
    client_headers: &HeaderMap,
    server: &McpServerConfig,
    oauth_token: Option<&str>,
) -> reqwest::RequestBuilder {
    // 步骤 1：透传客户端 headers
    for (key, value) in client_headers.iter() {
        let name = key.as_str().to_lowercase();
        if SKIP_HEADERS.contains(&name.as_str()) {
            continue;
        }
        if let Ok(v) = value.to_str() {
            builder = builder.header(key.clone(), v);
        }
    }

    // 步骤 2：Authorization 优先级：OAuth token > 静态 auth_token > 透传客户端
    // 注意：OAuth 启用时绝不回退透传客户端 token（MCP spec 禁止 token passthrough）
    match oauth_token {
        Some(token) if !token.is_empty() => {
            builder = builder.header(header::AUTHORIZATION, format!("Bearer {}", token));
        }
        _ => match &server.auth_token {
            Some(token) if !token.is_empty() => {
                builder = builder.header(header::AUTHORIZATION, format!("Bearer {}", token));
            }
            _ => {
                // 透传客户端原始 Authorization（若有）
                if let Some(auth) = client_headers.get(header::AUTHORIZATION) {
                    if let Ok(v) = auth.to_str() {
                        builder = builder.header(header::AUTHORIZATION, v);
                    }
                }
            }
        },
    }

    // 步骤 3：叠加自定义 headers
    for (k, v) in &server.custom_headers {
        builder = builder.header(k.as_str(), v.as_str());
    }

    builder
}

/// 需要从上游响应回传给客户端的 MCP 相关响应头
const COPY_BACK_HEADERS: &[&str] = &["mcp-session-id", "mcp-protocol-version"];

/// 将上游响应中的 MCP 会话相关头复制到返回给客户端的响应 builder 上。
/// 尤其是 initialize 阶段下发的 Mcp-Session-Id，必须回传，否则客户端后续请求会失败。
pub fn copy_mcp_response_headers(
    mut builder: axum::http::response::Builder,
    upstream_headers: &reqwest::header::HeaderMap,
) -> axum::http::response::Builder {
    for name in COPY_BACK_HEADERS {
        if let Some(val) = upstream_headers.get(*name) {
            if let Ok(v) = val.to_str() {
                builder = builder.header(*name, v);
            }
        }
    }
    builder
}
