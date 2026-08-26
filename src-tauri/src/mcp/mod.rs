//! MCP 中继子系统：作为 MCP 客户端与上游 MCP server 之间的反向代理，
//! 在转发过程中按渠道配置的黑名单裁剪工具（tools/list 删除、tools/call 拒绝）。
//!
//! - `relay`   — 转发核心（POST/GET/DELETE，JSON 缓冲与 SSE 流式透传）
//! - `filter`  — 黑名单过滤与 JSON-RPC 响应提取
//! - `headers` — MCP 请求/响应头双向透传与 Authorization 注入

pub mod filter;
pub mod headers;
pub mod oauth;
pub mod relay;
pub mod tools;

use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use serde_json::{json, Value};

use crate::channel::config::McpServerConfig;
use crate::proxy::server::AppState;

/// 按 server_id 查找已启用的 MCP server 配置；找不到返回 404 + JSON-RPC error
fn lookup_server(state: &Arc<AppState>, server_id: &str) -> Result<McpServerConfig, Response> {
    let config = state.config.read();
    match config.find_mcp_server(server_id) {
        Some(s) => Ok(s.clone()),
        None => {
            let body = json!({
                "jsonrpc": "2.0",
                "id": null,
                "error": {
                    "code": -32600,
                    "message": format!("MCP server not found: {}", server_id)
                }
            });
            Err((StatusCode::NOT_FOUND, axum::Json(body)).into_response())
        }
    }
}

/// 提取 user-agent 供审计
fn user_agent(headers: &HeaderMap) -> Option<String> {
    headers
        .get(axum::http::header::USER_AGENT)
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string())
}

/// POST /mcp/{server_id} —— 客户端发送 JSON-RPC 消息
pub async fn mcp_post(
    State(state): State<Arc<AppState>>,
    Path(server_id): Path<String>,
    headers: HeaderMap,
    body: String,
) -> Response {
    let server = match lookup_server(&state, &server_id) {
        Ok(s) => s,
        Err(resp) => return resp,
    };

    // 解析 JSON-RPC：method / id / params.name。解析不出来则按透传处理（method 为空）。
    let parsed: Value = serde_json::from_str(&body).unwrap_or(Value::Null);
    let method = parsed
        .get("method")
        .and_then(|m| m.as_str())
        .unwrap_or("")
        .to_string();
    let req_id = parsed.get("id").cloned().unwrap_or(Value::Null);

    // 拦截 A：tools/call 调用黑名单工具 → 直接拒绝，不转发上游
    if method == "tools/call" {
        if let Some(name) = parsed.pointer("/params/name").and_then(|v| v.as_str()) {
            if server.blocked_tools.iter().any(|t| t == name) {
                // 审计：记录被拦截的调用
                let audit_id = state
                    .audit_db
                    .create(
                        "POST",
                        &format!("/mcp/{}", server_id),
                        &format!("tools/call:{}", name),
                        None,
                        Some(&body),
                        user_agent(&headers).as_deref(),
                    )
                    .unwrap_or(0);
                let _ = state
                    .audit_db
                    .update_error(audit_id, 200, 0, &format!("blocked tool: {}", name));
                return filter::reject_tool_call(&req_id, name);
            }
        }
    }

    // 审计占位：alias_model 字段复用为 MCP method 名
    let alias = if method.is_empty() { "mcp" } else { &method };
    let audit_id = state
        .audit_db
        .create(
            "POST",
            &format!("/mcp/{}", server_id),
            alias,
            None,
            Some(&body),
            user_agent(&headers).as_deref(),
        )
        .unwrap_or(0);

    relay::forward_post(&state, &server, &headers, body, &method, req_id, audit_id).await
}

/// GET /mcp/{server_id} —— 客户端打开 SSE 流接收服务端推送
pub async fn mcp_get(
    State(state): State<Arc<AppState>>,
    Path(server_id): Path<String>,
    headers: HeaderMap,
) -> Response {
    let server = match lookup_server(&state, &server_id) {
        Ok(s) => s,
        Err(resp) => return resp,
    };

    let audit_id = state
        .audit_db
        .create(
            "GET",
            &format!("/mcp/{}", server_id),
            "mcp:sse",
            None,
            None,
            user_agent(&headers).as_deref(),
        )
        .unwrap_or(0);

    relay::forward_get(&state, &server, &headers, audit_id).await
}

/// DELETE /mcp/{server_id} —— 终止会话
pub async fn mcp_delete(
    State(state): State<Arc<AppState>>,
    Path(server_id): Path<String>,
    headers: HeaderMap,
) -> Response {
    let server = match lookup_server(&state, &server_id) {
        Ok(s) => s,
        Err(resp) => return resp,
    };

    let audit_id = state
        .audit_db
        .create(
            "DELETE",
            &format!("/mcp/{}", server_id),
            "mcp:delete",
            None,
            None,
            user_agent(&headers).as_deref(),
        )
        .unwrap_or(0);

    relay::forward_delete(&state, &server, &headers, audit_id).await
}
