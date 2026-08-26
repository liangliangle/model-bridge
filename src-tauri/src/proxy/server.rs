//! HTTP 服务器：注册路由，入口 handler 只负责创建 RequestContext。

use axum::{
    extract::{DefaultBodyLimit, Request, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::{get, post},
    Json, Router,
};
use reqwest::Client;
use serde_json::{json, Value};
use std::sync::Arc;
use tokio::sync::oneshot;
use tower_http::cors::CorsLayer;
use tower_http::catch_panic::CatchPanicLayer;

use crate::audit::db::AuditDb;
use crate::channel::config::AppConfig;
use crate::channel::health::HealthMap;
use crate::proxy::context::{InputFormat, RequestContext};
use crate::proxy::router::route_request;

pub struct AppState {
    pub config: Arc<parking_lot::RwLock<AppConfig>>,
    pub health_map: HealthMap,
    pub audit_db: Arc<AuditDb>,
    pub http_client: Client,
    pub oauth: crate::mcp::oauth::OAuthStoreHandle,
}

pub async fn start_server(state: Arc<AppState>, shutdown_rx: oneshot::Receiver<()>) -> Result<(), String> {
    let config = state.config.read();
    let port = config.listen_port;
    let host = config.listen_host.clone();
    drop(config);

    let app = Router::new()
        .route("/v1/chat/completions", post(handle_openai_chat))
        .route("/v1/completions", post(handle_openai_chat))
        .route("/v1/responses", post(handle_openai_responses))
        .route("/v1/messages", post(handle_anthropic_messages))
        .route("/v1/models", get(handle_list_models))
        // MCP 中继端点：单路径支持 POST/GET/DELETE
        .route(
            "/mcp/{server_id}",
            post(crate::mcp::mcp_post)
                .get(crate::mcp::mcp_get)
                .delete(crate::mcp::mcp_delete),
        )
        // OAuth 回调（AS 重定向到此，不走 admin 鉴权，靠 state 校验）
        .route("/oauth/callback", get(crate::mcp::oauth::oauth_callback_handler))
        .route("/api/stats", get(crate::commands::get_stats))
        .route("/api/stats/heatmap", get(crate::commands::get_token_heatmap))
        .route("/api/auth/status", get(crate::commands::get_auth_status))
        .route("/api/audit", get(crate::commands::get_audit_logs))
        .route("/api/audit/detail", get(crate::commands::get_audit_detail))
        .route("/api/audit/db/status", get(crate::commands::get_audit_db_status))
        .route("/api/audit/cleanup", post(crate::commands::force_cleanup_audit))
        .route("/api/channels", get(crate::commands::get_channels))
        .route("/api/channels/health", get(crate::commands::get_channel_health))
        .route("/api/config", get(crate::commands::get_full_config))
        .route("/api/config/channel", post(crate::commands::save_channel))
        .route("/api/config/channel/delete", post(crate::commands::delete_channel))
        .route("/api/config/failover", post(crate::commands::save_failover_config))
        .route("/api/config/settings", post(crate::commands::save_settings))
        .route("/api/config/channel/test", post(crate::commands::test_channel))
        .route("/api/config/mcp", post(crate::commands::save_mcp_server))
        .route("/api/config/mcp/delete", post(crate::commands::delete_mcp_server))
        .route("/api/config/mcp/oauth/start", post(crate::commands::start_mcp_oauth))
        .route("/api/config/mcp/oauth/status", get(crate::commands::get_mcp_oauth_status))
        .route("/api/config/mcp/tools/fetch", post(crate::commands::fetch_mcp_tools))
        .route("/api/config/mcp/tools/toggle", post(crate::commands::toggle_mcp_tool))
        // Skill 统一管理
        .route("/api/skills", get(crate::skill::get_skills))
        .route("/api/skills/content", get(crate::skill::get_skill_content).post(crate::skill::save_skill_content))
        .route("/api/skills/toggle", post(crate::skill::toggle_skill))
        .route("/api/skills/distribute", post(crate::skill::distribute_skill))
        .route("/api/skills/import", post(crate::skill::import_skill))
        .route("/api/config/skill/central", post(crate::skill::save_skill_central))
        .route("/api/config/skill/agent", post(crate::skill::save_skill_agent))
        .route("/api/config/skill/agent/delete", post(crate::skill::delete_skill_agent))
        .fallback(crate::static_files::static_handler)
        .layer(DefaultBodyLimit::max(64 * 1024 * 1024)) // 64MB — allow large conversation payloads
        .layer(axum::middleware::from_fn(log_oversized_body))
        .layer(CorsLayer::permissive())
        .layer(CatchPanicLayer::custom(|_: Box<dyn std::any::Any + Send>| {
            let body = json!({"error": {"message": "Internal server error (panic)", "type": "server_error"}});
            (StatusCode::INTERNAL_SERVER_ERROR, Json(body)).into_response()
        }))
        .with_state(state);

    let addr = format!("{}:{}", host, port);
    let listener = tokio::net::TcpListener::bind(&addr).await
        .map_err(|e| format!("Failed to bind to {}: {}", addr, e))?;
    tracing::info!("Proxy server listening on {}", addr);

    axum::serve(listener, app)
        .with_graceful_shutdown(async { let _ = shutdown_rx.await; tracing::info!("Shutting down"); })
        .await.map_err(|e| format!("Server error: {}", e))?;
    Ok(())
}

// ========== 鉴权 ==========

/// 从 Authorization 头提取 Bearer token
pub fn extract_bearer_token(headers: &HeaderMap) -> Option<&str> {
    headers
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
}

/// 检查代理端点鉴权。返回 None 表示通过，Some(Response) 表示拒绝。
fn check_proxy_auth(state: &AppState, headers: &HeaderMap) -> Option<Response> {
    let tokens = &state.config.read().auth.proxy_tokens;
    if tokens.is_empty() {
        return None;
    }
    match extract_bearer_token(headers) {
        Some(token) if tokens.iter().any(|t| t == token) => None,
        _ => Some(auth_error_response("Invalid or missing API key")),
    }
}

/// 检查管理 API 鉴权
pub fn check_admin_auth(state: &AppState, headers: &HeaderMap) -> Option<Response> {
    let admin_token = state.config.read().auth.admin_token.clone();
    match admin_token {
        None => None,
        Some(ref t) if t.is_empty() => None,
        Some(ref expected) => match extract_bearer_token(headers) {
            Some(token) if token == expected => None,
            _ => Some(auth_error_response("Invalid or missing admin token")),
        },
    }
}

fn auth_error_response(message: &str) -> Response {
    (
        StatusCode::UNAUTHORIZED,
        Json(json!({"error": {"message": message, "type": "auth_error"}})),
    ).into_response()
}

/// 记录被 body 大小限制拒绝（413）的请求，打印声明的 Content-Length 与实际限制。
/// 该中间件位于 DefaultBodyLimit 外层，因此能捕获其产生的 413 响应。
async fn log_oversized_body(req: Request, next: axum::middleware::Next) -> Response {
    let path = req.uri().path().to_string();
    let declared = req
        .headers()
        .get(axum::http::header::CONTENT_LENGTH)
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.parse::<u64>().ok());
    let resp = next.run(req).await;
    if resp.status() == StatusCode::PAYLOAD_TOO_LARGE {
        const LIMIT: u64 = 64 * 1024 * 1024;
        match declared {
            Some(n) => tracing::warn!(
                "Request rejected (413 Payload Too Large): path={} content-length={} bytes ({:.2} MB), limit={} bytes ({} MB)",
                path, n, n as f64 / 1024.0 / 1024.0, LIMIT, LIMIT / 1024 / 1024
            ),
            None => tracing::warn!(
                "Request rejected (413 Payload Too Large): path={} content-length unknown (chunked/missing), limit={} bytes ({} MB)",
                path, LIMIT, LIMIT / 1024 / 1024
            ),
        }
    }
    resp
}

// ========== 代理端点 Handler ==========

async fn handle_openai_chat(State(state): State<Arc<AppState>>, headers: HeaderMap, body: String) -> Response {
    if let Some(reject) = check_proxy_auth(&state, &headers) { return reject; }
    let ctx = RequestContext::from_raw(InputFormat::OpenAI, "/v1/chat/completions", headers, body);
    route_request(state, ctx).await
}

async fn handle_openai_responses(State(state): State<Arc<AppState>>, headers: HeaderMap, body: String) -> Response {
    if let Some(reject) = check_proxy_auth(&state, &headers) { return reject; }
    let ctx = RequestContext::from_raw(InputFormat::Responses, "/v1/responses", headers, body);
    route_request(state, ctx).await
}

async fn handle_anthropic_messages(State(state): State<Arc<AppState>>, headers: HeaderMap, body: String) -> Response {
    if let Some(reject) = check_proxy_auth(&state, &headers) { return reject; }
    let ctx = RequestContext::from_raw(InputFormat::Anthropic, "/v1/messages", headers, body);
    route_request(state, ctx).await
}

async fn handle_list_models(State(state): State<Arc<AppState>>, headers: HeaderMap) -> Response {
    if let Some(reject) = check_proxy_auth(&state, &headers) { return reject; }
    let config = state.config.read();
    let mut models: Vec<String> = if !config.models.is_empty() {
        config.models.clone()
    } else {
        let mut collected = Vec::new();
        for ch in &config.channels {
            if !ch.enabled { continue; }
            for alias in ch.model_mapping.keys() {
                if !collected.contains(alias) { collected.push(alias.clone()); }
            }
        }
        collected
    };
    models.sort();
    let data: Vec<Value> = models.iter().map(|m| json!({"id": m, "object": "model", "owned_by": "model-bridge"})).collect();
    Json(json!({"object": "list", "data": data})).into_response()
}
