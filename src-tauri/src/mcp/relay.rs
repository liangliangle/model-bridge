//! MCP 中继转发核心：把客户端请求转发到上游 MCP server，
//! 按响应类型分流（JSON 缓冲 / SSE 流式透传），并在 tools/list 时过滤工具。

use std::sync::Arc;
use std::time::{Duration, Instant};

use axum::body::Body;
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use futures::stream::StreamExt;
use parking_lot::Mutex as ParkingMutex;
use serde_json::Value;

use crate::audit::db::TokenUsage;
use crate::channel::config::McpServerConfig;
use crate::mcp::filter;
use crate::mcp::headers::{build_mcp_forward_headers, copy_mcp_response_headers};
use crate::proxy::server::AppState;

/// SSE 流空闲超时（连续无数据则断开）
const SSE_IDLE_TIMEOUT: Duration = Duration::from_secs(300);

/// 转发 POST 请求到上游 MCP server。
/// - method/req_id 已从 body 解析好（用于审计和 tools/list 过滤判断）
/// - is_tools_list 为 true 时走过滤路径
pub async fn forward_post(
    state: &Arc<AppState>,
    server: &McpServerConfig,
    client_headers: &HeaderMap,
    body: String,
    method: &str,
    req_id: Value,
    audit_id: i64,
) -> Response {
    let start = Instant::now();

    // OAuth：转发前确保有有效 token（过期则自动刷新）
    let oauth_token = match crate::mcp::oauth::ensure_valid_token(state, server).await {
        Ok(t) => t,
        Err(e) => {
            let _ = state.audit_db.update_error(audit_id, 401, 0, &e.0);
            return mcp_error_response(&req_id, -32001, &format!("MCP OAuth: {}", e.0));
        }
    };

    let builder = state
        .http_client
        .post(&server.endpoint)
        .header("Content-Type", "application/json")
        .body(body);
    let builder = build_mcp_forward_headers(builder, client_headers, server, oauth_token.as_deref());

    let resp = match builder.send().await {
        Ok(r) => r,
        Err(e) => {
            let msg = if e.is_timeout() {
                "timeout".to_string()
            } else {
                format!("network_error: {}", e)
            };
            let latency = start.elapsed().as_millis() as u64;
            let _ = state.audit_db.update_error(audit_id, 502, latency, &msg);
            return mcp_error_response(&req_id, -32000, &msg);
        }
    };

    let status = resp.status().as_u16();
    let upstream_headers = resp.headers().clone();
    if status == 401 && server.oauth_enabled {
        let _ = crate::mcp::oauth::clear_oauth_registration(state, &server.id);
        let _ = state.audit_db.update_error(audit_id, 401, start.elapsed().as_millis() as u64, "MCP OAuth client rejected; registration cleared");
    }
    let ct = upstream_headers
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    let is_sse = ct.contains("text/event-stream");

    // tools/list：缓冲 → 过滤 → 回 application/json
    if method == "tools/list" {
        return filter_tools_list(
            state, server, resp, status, &upstream_headers, &req_id, audit_id, start,
        )
        .await;
    }

    if is_sse {
        return passthrough_sse(state, resp, status, &upstream_headers, audit_id, start).await;
    }

    passthrough_json(state, resp, status, &upstream_headers, audit_id, start).await
}

/// tools/list 过滤：缓冲完整响应，提取 JSON-RPC response，删除黑名单工具，回 JSON。
async fn filter_tools_list(
    state: &Arc<AppState>,
    server: &McpServerConfig,
    resp: reqwest::Response,
    status: u16,
    upstream_headers: &reqwest::header::HeaderMap,
    req_id: &Value,
    audit_id: i64,
    start: Instant,
) -> Response {
    let text = match resp.text().await {
        Ok(t) => t,
        Err(e) => {
            let latency = start.elapsed().as_millis() as u64;
            let msg = format!("read upstream failed: {}", e);
            let _ = state.audit_db.update_error(audit_id, 502, latency, &msg);
            return mcp_error_response(req_id, -32000, &msg);
        }
    };

    // 提取 JSON-RPC response 并过滤；提取失败则原样透传上游文本
    let (out_body, ct_out) = match filter::extract_jsonrpc_response(&text, req_id) {
        Some(mut rpc) => {
            filter::filter_tools_in_response(&mut rpc, &server.blocked_tools);
            (
                serde_json::to_string(&rpc).unwrap_or(text.clone()),
                "application/json",
            )
        }
        None => {
            // 解析不出来：保守透传，Content-Type 跟随上游（可能是 SSE）
            let ct = upstream_headers
                .get("content-type")
                .and_then(|v| v.to_str().ok())
                .unwrap_or("application/json");
            (text.clone(), if ct.contains("event-stream") { "text/event-stream" } else { "application/json" })
        }
    };

    let latency = start.elapsed().as_millis() as u64;
    let _ = state.audit_db.update_response(
        audit_id,
        status,
        latency,
        Some(&headers_json(upstream_headers)),
        Some(&text),
        Some(r#"{"content-type":"application/json"}"#),
        Some(&out_body),
        &TokenUsage::default(),
        None,
    );

    let builder = Response::builder()
        .status(StatusCode::from_u16(status).unwrap_or(StatusCode::OK))
        .header("Content-Type", ct_out);
    let builder = copy_mcp_response_headers(builder, upstream_headers);
    builder.body(Body::from(out_body)).unwrap()
}

/// 非流式响应：缓冲全部字节，原 Content-Type 回传。
async fn passthrough_json(
    state: &Arc<AppState>,
    resp: reqwest::Response,
    status: u16,
    upstream_headers: &reqwest::header::HeaderMap,
    audit_id: i64,
    start: Instant,
) -> Response {
    let ct = upstream_headers
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("application/json")
        .to_string();

    let text = match resp.text().await {
        Ok(t) => t,
        Err(e) => {
            let latency = start.elapsed().as_millis() as u64;
            let msg = format!("read upstream failed: {}", e);
            let _ = state.audit_db.update_error(audit_id, 502, latency, &msg);
            return (StatusCode::BAD_GATEWAY, msg).into_response();
        }
    };

    let latency = start.elapsed().as_millis() as u64;
    let _ = state.audit_db.update_response(
        audit_id,
        status,
        latency,
        Some(&headers_json(upstream_headers)),
        Some(&text),
        None,
        Some(&text),
        &TokenUsage::default(),
        None,
    );

    let builder = Response::builder()
        .status(StatusCode::from_u16(status).unwrap_or(StatusCode::OK))
        .header("Content-Type", ct);
    let builder = copy_mcp_response_headers(builder, upstream_headers);
    builder.body(Body::from(text)).unwrap()
}

/// 流式响应：字节级透传上游 SSE 流给客户端，累积内容供审计。
/// 照搬 proxy::executor 的 unfold + idle timeout 模式。
pub async fn passthrough_sse(
    state: &Arc<AppState>,
    resp: reqwest::Response,
    status: u16,
    upstream_headers: &reqwest::header::HeaderMap,
    audit_id: i64,
    start: Instant,
) -> Response {
    let first_byte = start.elapsed().as_millis() as u64;
    let _ = state.audit_db.update_first_byte(
        audit_id,
        first_byte,
        Some(&headers_json(upstream_headers)),
        Some(&headers_json(upstream_headers)),
    );

    let audit_db = state.audit_db.clone();
    let stream = resp.bytes_stream();
    let collected = Arc::new(ParkingMutex::new(Vec::<u8>::new()));
    let collected_clone = collected.clone();
    let collected_for_audit = collected.clone();

    let (done_tx, done_rx) = tokio::sync::oneshot::channel::<()>();
    let done_tx = Arc::new(ParkingMutex::new(Some(done_tx)));
    let done_tx_clone = done_tx.clone();

    let byte_stream = futures::stream::unfold(
        (Box::pin(stream), SSE_IDLE_TIMEOUT, collected_clone, done_tx_clone),
        |(mut stream, timeout, collected, done_tx)| async move {
            match tokio::time::timeout(timeout, stream.next()).await {
                Ok(Some(Ok(bytes))) => {
                    collected.lock().extend_from_slice(&bytes);
                    Some((Ok(bytes), (stream, timeout, collected, done_tx)))
                }
                Ok(Some(Err(e))) => {
                    if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                    Some((Err(e), (stream, timeout, collected, done_tx)))
                }
                Ok(None) => {
                    if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                    None
                }
                Err(_) => {
                    tracing::warn!("MCP SSE idle timeout ({}s)", timeout.as_secs());
                    if let Some(tx) = done_tx.lock().take() { let _ = tx.send(()); }
                    None
                }
            }
        },
    );

    let stream_start = start;
    let upstream_status_for_audit = status;
    tokio::spawn(async move {
        let _ = done_rx.await;
        let total_latency = stream_start.elapsed().as_millis() as u64;
        let raw_bytes = collected_for_audit.lock().clone();
        let text = String::from_utf8_lossy(&raw_bytes).to_string();
        let _ = audit_db.update_streaming_response(audit_id, &text, None, Some(upstream_status_for_audit), total_latency, None);
    });

    let builder = Response::builder()
        .status(StatusCode::from_u16(status).unwrap_or(StatusCode::OK))
        .header("Content-Type", "text/event-stream")
        .header("Cache-Control", "no-cache")
        .header("Connection", "keep-alive");
    let builder = copy_mcp_response_headers(builder, upstream_headers);
    builder.body(Body::from_stream(byte_stream)).unwrap()
}

/// 转发 GET 请求（客户端打开 SSE 长连接接收服务端推送）。
pub async fn forward_get(
    state: &Arc<AppState>,
    server: &McpServerConfig,
    client_headers: &HeaderMap,
    audit_id: i64,
) -> Response {
    let start = Instant::now();
    let oauth_token = match crate::mcp::oauth::ensure_valid_token(state, server).await {
        Ok(t) => t,
        Err(e) => {
            let _ = state.audit_db.update_error(audit_id, 401, 0, &e.0);
            return (StatusCode::UNAUTHORIZED, format!("MCP OAuth: {}", e.0)).into_response();
        }
    };
    let builder = state.http_client.get(&server.endpoint);
    let builder = build_mcp_forward_headers(builder, client_headers, server, oauth_token.as_deref());

    let resp = match builder.send().await {
        Ok(r) => r,
        Err(e) => {
            let latency = start.elapsed().as_millis() as u64;
            let msg = format!("network_error: {}", e);
            let _ = state.audit_db.update_error(audit_id, 502, latency, &msg);
            return (StatusCode::BAD_GATEWAY, msg).into_response();
        }
    };

    let status = resp.status().as_u16();
    let upstream_headers = resp.headers().clone();
    if status == 401 && server.oauth_enabled {
        let _ = crate::mcp::oauth::clear_oauth_registration(state, &server.id);
        let _ = state.audit_db.update_error(audit_id, 401, start.elapsed().as_millis() as u64, "MCP OAuth client rejected; registration cleared");
    }

    // 405 表示上游不提供 GET SSE 通道，原样回传
    if status == 405 {
        let latency = start.elapsed().as_millis() as u64;
        let _ = state.audit_db.update_response(
            audit_id, status, latency,
            Some(&headers_json(&upstream_headers)), None, None, None,
            &TokenUsage::default(),
            None,
        );
        return StatusCode::METHOD_NOT_ALLOWED.into_response();
    }

    passthrough_sse(state, resp, status, &upstream_headers, audit_id, start).await
}

/// 转发 DELETE 请求（终止会话），缓冲回传上游状态码与 body。
pub async fn forward_delete(
    state: &Arc<AppState>,
    server: &McpServerConfig,
    client_headers: &HeaderMap,
    audit_id: i64,
) -> Response {
    let start = Instant::now();
    let oauth_token = match crate::mcp::oauth::ensure_valid_token(state, server).await {
        Ok(t) => t,
        Err(e) => {
            let _ = state.audit_db.update_error(audit_id, 401, 0, &e.0);
            return (StatusCode::UNAUTHORIZED, format!("MCP OAuth: {}", e.0)).into_response();
        }
    };
    let builder = state.http_client.delete(&server.endpoint);
    let builder = build_mcp_forward_headers(builder, client_headers, server, oauth_token.as_deref());

    let resp = match builder.send().await {
        Ok(r) => r,
        Err(e) => {
            let latency = start.elapsed().as_millis() as u64;
            let msg = format!("network_error: {}", e);
            let _ = state.audit_db.update_error(audit_id, 502, latency, &msg);
            return (StatusCode::BAD_GATEWAY, msg).into_response();
        }
    };

    let status = resp.status().as_u16();
    let upstream_headers = resp.headers().clone();
    if status == 401 && server.oauth_enabled {
        let _ = crate::mcp::oauth::clear_oauth_registration(state, &server.id);
        let _ = state.audit_db.update_error(audit_id, 401, start.elapsed().as_millis() as u64, "MCP OAuth client rejected; registration cleared");
    }
    let text = resp.text().await.unwrap_or_default();

    let latency = start.elapsed().as_millis() as u64;
    let _ = state.audit_db.update_response(
        audit_id, status, latency,
        Some(&headers_json(&upstream_headers)), Some(&text), None, Some(&text),
        &TokenUsage::default(),
        None,
    );

    let builder = Response::builder()
        .status(StatusCode::from_u16(status).unwrap_or(StatusCode::OK));
    let builder = copy_mcp_response_headers(builder, &upstream_headers);
    builder.body(Body::from(text)).unwrap()
}

/// 构造一个 JSON-RPC error 形式的 HTTP 响应（网络层错误等）
fn mcp_error_response(req_id: &Value, code: i64, message: &str) -> Response {
    let body = serde_json::json!({
        "jsonrpc": "2.0",
        "id": req_id,
        "error": { "code": code, "message": message }
    });
    (StatusCode::BAD_GATEWAY, axum::Json(body)).into_response()
}

/// 序列化 reqwest 响应头为 JSON 字符串（审计用）
fn headers_json(headers: &reqwest::header::HeaderMap) -> String {
    let map: std::collections::HashMap<String, String> = headers
        .iter()
        .filter_map(|(k, v)| v.to_str().ok().map(|val| (k.to_string(), val.to_string())))
        .collect();
    serde_json::to_string(&map).unwrap_or_else(|_| "{}".to_string())
}
