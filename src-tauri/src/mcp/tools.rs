//! 主动握手拉取上游 MCP server 的工具列表。
//! 流程：initialize → notifications/initialized → tools/list（分页）。
//! 复用 OAuth 鉴权、头注入、JSON-RPC 响应解析。

use std::sync::Arc;

use axum::http::HeaderMap;
use reqwest::RequestBuilder;
use serde_json::json;

use crate::channel::config::{McpServerConfig, ToolInfo};
use crate::mcp::filter;
use crate::mcp::headers::build_mcp_forward_headers;
use crate::proxy::server::AppState;

/// 拉取工具的错误：区分"需授权"与"普通失败"，便于前端分别提示
pub enum FetchToolsError {
    NeedsAuth(String),
    Failed(String),
}

const PROTOCOL_VERSION: &str = "2025-06-18";
/// 分页防御上限
const MAX_PAGES: i64 = 100;

/// 截断过长文本（错误信息/日志用），按字符边界
fn truncate(s: &str) -> String {
    s.chars().take(500).collect()
}

/// 在 build_mcp_forward_headers 之后追加 MCP 会话头（它本身不管 session/protocol-version）
fn add_session_headers(mut b: RequestBuilder, session: &Option<String>, proto: &str) -> RequestBuilder {
    if let Some(sid) = session {
        b = b.header("Mcp-Session-Id", sid.as_str());
    }
    b.header("MCP-Protocol-Version", proto)
}

/// 构造一个带认证 + Accept 的 POST builder
fn post_builder(state: &Arc<AppState>, server: &McpServerConfig, oauth_token: Option<&str>, body: String) -> RequestBuilder {
    let empty = HeaderMap::new();
    let builder = state
        .http_client
        .post(&server.endpoint)
        .header("Content-Type", "application/json")
        .header("Accept", "application/json, text/event-stream")
        .body(body);
    build_mcp_forward_headers(builder, &empty, server, oauth_token)
}

/// 主动握手拉取工具列表
pub async fn fetch_tools(
    state: &Arc<AppState>,
    server: &McpServerConfig,
) -> Result<Vec<ToolInfo>, FetchToolsError> {
    // 步骤 0：鉴权
    let oauth_token = match crate::mcp::oauth::ensure_valid_token(state, server).await {
        Ok(t) => t,
        Err(e) => return Err(FetchToolsError::NeedsAuth(e.0)),
    };
    let token = oauth_token.as_deref();

    // 步骤 1：initialize
    let init_body = json!({
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {
            "protocolVersion": PROTOCOL_VERSION,
            "capabilities": {},
            "clientInfo": { "name": "model-bridge", "version": "0.1.0" }
        }
    });
    let resp = post_builder(state, server, token, init_body.to_string())
        .send()
        .await
        .map_err(|e| FetchToolsError::Failed(format!("initialize 网络错误: {}", e)))?;
    let status = resp.status();
    // 从响应头抓 session id（部分上游 stateless 不下发，为 None 时后续不加该头）
    let session_id = resp
        .headers()
        .get("mcp-session-id")
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_string());
    let text = resp
        .text()
        .await
        .map_err(|e| FetchToolsError::Failed(format!("读取 initialize 响应失败: {}", e)))?;
    if !status.is_success() {
        return Err(FetchToolsError::Failed(format!("initialize 返回 {}: {}", status, truncate(&text))));
    }
    let init_rpc = filter::extract_jsonrpc_response(&text, &json!(1))
        .ok_or_else(|| FetchToolsError::Failed(format!("initialize 响应无法解析: {}", truncate(&text))))?;
    if let Some(err) = init_rpc.get("error") {
        return Err(FetchToolsError::Failed(format!("initialize 返回错误: {}", err)));
    }
    // 协议版本：跟随上游响应，缺省用默认
    let proto = init_rpc
        .pointer("/result/protocolVersion")
        .and_then(|v| v.as_str())
        .unwrap_or(PROTOCOL_VERSION)
        .to_string();

    // 步骤 2：notifications/initialized（失败容错，不致命）
    let notif_body = json!({ "jsonrpc": "2.0", "method": "notifications/initialized" });
    let notif_builder = post_builder(state, server, token, notif_body.to_string());
    let notif_builder = add_session_headers(notif_builder, &session_id, &proto);
    let _ = notif_builder.send().await;

    // 步骤 3：tools/list 分页循环
    let mut tools: Vec<ToolInfo> = Vec::new();
    let mut cursor: Option<String> = None;
    let mut next_id: i64 = 2;
    loop {
        let params = match &cursor {
            Some(c) => json!({ "cursor": c }),
            None => json!({}),
        };
        let req = json!({ "jsonrpc": "2.0", "id": next_id, "method": "tools/list", "params": params });
        let builder = post_builder(state, server, token, req.to_string());
        let builder = add_session_headers(builder, &session_id, &proto);
        let resp = builder
            .send()
            .await
            .map_err(|e| FetchToolsError::Failed(format!("tools/list 网络错误: {}", e)))?;
        let status = resp.status();
        let text = resp
            .text()
            .await
            .map_err(|e| FetchToolsError::Failed(format!("读取 tools/list 响应失败: {}", e)))?;
        if !status.is_success() {
            return Err(FetchToolsError::Failed(format!("tools/list 返回 {}: {}", status, truncate(&text))));
        }
        let rpc = filter::extract_jsonrpc_response(&text, &json!(next_id))
            .ok_or_else(|| FetchToolsError::Failed(format!("tools/list 响应无法解析: {}", truncate(&text))))?;
        if let Some(err) = rpc.get("error") {
            return Err(FetchToolsError::Failed(format!("上游 tools/list 错误: {}", err)));
        }
        if let Some(arr) = rpc.pointer("/result/tools").and_then(|v| v.as_array()) {
            for t in arr {
                let name = t.get("name").and_then(|v| v.as_str()).unwrap_or("").to_string();
                if name.is_empty() {
                    continue;
                }
                tools.push(ToolInfo {
                    name,
                    description: t.get("description").and_then(|v| v.as_str()).map(String::from),
                    input_schema: t.get("inputSchema").cloned(),
                });
            }
        }
        cursor = rpc
            .pointer("/result/nextCursor")
            .and_then(|v| v.as_str())
            .map(String::from);
        next_id += 1;
        if cursor.is_none() || next_id > MAX_PAGES {
            break;
        }
    }

    Ok(tools)
}
