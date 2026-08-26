//! MCP 上游 OAuth 2.1 支持：中继作为 OAuth client 完成完整授权流程。
//!
//! 流程：发现(401→PRM→AS metadata) → DCR 动态注册 → PKCE 授权 → 回调换 token
//! → 转发时注入 Bearer，过期自动刷新。
//!
//! token 持久化在 McpServerConfig.oauth（随 config.yaml 落盘）；
//! 仅 PKCE/state 的 pending 表与 per-server 刷新锁放内存。

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use axum::extract::{Query, State};
use axum::response::Html;
use base64::Engine;
use parking_lot::RwLock;
use rand::Rng;
use serde::Deserialize;
use serde_json::json;
use sha2::{Digest, Sha256};

use crate::channel::config::{McpServerConfig, OAuthData};
use crate::proxy::server::AppState;

/// access token 提前刷新的余量（秒）
const EXPIRY_SKEW_SECS: i64 = 60;
/// pending 授权请求的存活时间（秒），超时作废
const PENDING_TTL_SECS: i64 = 600;
/// Background refresh interval. This is deliberately much shorter than the
/// usual access-token lifetime, while keeping network traffic negligible.
pub const TOKEN_REFRESH_INTERVAL_SECS: u64 = 300;

// ========== 内存状态 ==========

/// 一次进行中的授权请求（start 创建，callback 消费）
#[derive(Clone)]
pub struct PendingAuth {
    pub server_id: String,
    pub code_verifier: String,
    pub redirect_uri: String,
    pub resource: String,
    pub token_endpoint: String,
    pub client_id: String,
    pub client_secret: Option<String>,
    pub scope: Option<String>,
    pub created_at: i64,
}

/// OAuth 运行时存储
#[derive(Default)]
pub struct OAuthStore {
    /// state → 进行中的授权请求（按 state 隔离，天然支持多 server 并发授权）
    pub pending: HashMap<String, PendingAuth>,
    /// server_id → 刷新锁（防并发双刷新使 AS 轮换的 refresh_token 失效）
    pub refresh_locks: HashMap<String, Arc<tokio::sync::Mutex<()>>>,
}

pub type OAuthStoreHandle = Arc<RwLock<OAuthStore>>;

/// 启动时构造空 store（token 在 config 里，无需预热）
pub fn new_store_from_config() -> OAuthStoreHandle {
    Arc::new(RwLock::new(OAuthStore::default()))
}

fn now_secs() -> i64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs() as i64).unwrap_or(0)
}

// ========== token 错误 ==========

/// 转发期 token 获取失败：需要重新授权
pub struct NeedsReauth(pub String);

// ========== PKCE / state ==========

fn base64url(bytes: &[u8]) -> String {
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes)
}

fn random_token(len: usize) -> String {
    let mut rng = rand::thread_rng();
    let bytes: Vec<u8> = (0..len).map(|_| rng.gen::<u8>()).collect();
    base64url(&bytes)
}

/// 生成 PKCE (code_verifier, code_challenge=S256)
fn gen_pkce() -> (String, String) {
    let verifier = random_token(48); // base64url 后约 64 字符
    let mut hasher = Sha256::new();
    hasher.update(verifier.as_bytes());
    let challenge = base64url(&hasher.finalize());
    (verifier, challenge)
}

// ========== 发现链 ==========

/// 取 URL 的 origin（scheme://host[:port]）
fn origin_of(u: &str) -> Option<String> {
    let parsed = url::Url::parse(u).ok()?;
    let scheme = parsed.scheme();
    let host = parsed.host_str()?;
    match parsed.port() {
        Some(p) => Some(format!("{}://{}:{}", scheme, host, p)),
        None => Some(format!("{}://{}", scheme, host)),
    }
}

/// 步骤1：探测上游获取 Protected Resource Metadata URL。
/// 发最小请求触发 401，从 WWW-Authenticate 解析 resource_metadata；无则回退 well-known。
async fn discover_prm_url(client: &reqwest::Client, endpoint: &str) -> Result<String, String> {
    let resp = client
        .post(endpoint)
        .header("Content-Type", "application/json")
        .header("Accept", "application/json, text/event-stream")
        .body(r#"{"jsonrpc":"2.0","id":0,"method":"ping"}"#)
        .send()
        .await
        .map_err(|e| format!("探测上游失败: {}", e))?;

    if let Some(wa) = resp.headers().get("www-authenticate").and_then(|v| v.to_str().ok()) {
        if let Some(url) = parse_resource_metadata(wa) {
            return Ok(url);
        }
    }
    // 回退：<origin>/.well-known/oauth-protected-resource
    let origin = origin_of(endpoint).ok_or_else(|| "无法解析 endpoint origin".to_string())?;
    Ok(format!("{}/.well-known/oauth-protected-resource", origin))
}

/// 从 WWW-Authenticate 头解析 resource_metadata="..." 的值
fn parse_resource_metadata(header: &str) -> Option<String> {
    let key = "resource_metadata=";
    let start = header.find(key)? + key.len();
    let rest = &header[start..];
    let rest = rest.trim_start_matches('"');
    let end = rest.find('"').unwrap_or(rest.len());
    let val = &rest[..end];
    if val.is_empty() { None } else { Some(val.to_string()) }
}

#[derive(Deserialize)]
struct PrmDoc {
    #[serde(default)]
    authorization_servers: Vec<String>,
}

/// 步骤2：拉 Protected Resource Metadata（RFC9728）
async fn fetch_prm(client: &reqwest::Client, prm_url: &str, endpoint: &str) -> Result<(String, String), String> {
    let doc: PrmDoc = client
        .get(prm_url)
        .header("Accept", "application/json")
        .send().await.map_err(|e| format!("拉取 PRM 失败: {}", e))?
        .json().await.map_err(|e| format!("解析 PRM 失败: {}", e))?;

    let as_url = doc.authorization_servers.into_iter().next()
        .ok_or_else(|| "PRM 未提供 authorization_servers".to_string())?;
    // resource 始终用 MCP endpoint 本身（精确到 server 路径，RFC8707 最精确 URI）。
    // 不采用 PRM 的 resource 字段：多 server 网关（如 mcp.alibaba-inc.com）的 PRM
    // 可能只返回域名级 resource，导致网关无法区分具体 server 而报错。
    let resource = endpoint.trim_end_matches('/').to_string();
    Ok((as_url, resource))
}

#[derive(Deserialize)]
struct AsMetadata {
    authorization_endpoint: String,
    token_endpoint: String,
    #[serde(default)]
    registration_endpoint: Option<String>,
}

/// 步骤3：拉 Authorization Server Metadata（RFC8414，回退 OIDC）
async fn fetch_as_metadata(client: &reqwest::Client, as_url: &str) -> Result<AsMetadata, String> {
    let base = as_url.trim_end_matches('/');
    let candidates = [
        format!("{}/.well-known/oauth-authorization-server", base),
        format!("{}/.well-known/openid-configuration", base),
    ];
    let mut last_err = String::new();
    for url in &candidates {
        match client.get(url).header("Accept", "application/json").send().await {
            Ok(resp) if resp.status().is_success() => {
                match resp.json::<AsMetadata>().await {
                    Ok(meta) => return Ok(meta),
                    Err(e) => last_err = format!("解析 AS metadata 失败: {}", e),
                }
            }
            Ok(resp) => last_err = format!("AS metadata {} 返回 {}", url, resp.status()),
            Err(e) => last_err = format!("请求 AS metadata 失败: {}", e),
        }
    }
    Err(last_err)
}

#[derive(Deserialize)]
struct DcrResponse {
    client_id: String,
    #[serde(default)]
    client_secret: Option<String>,
}

/// 步骤4：动态客户端注册（RFC7591）
async fn register_client(
    client: &reqwest::Client,
    registration_endpoint: &str,
    redirect_uri: &str,
) -> Result<(String, Option<String>), String> {
    let body = json!({
        "client_name": "Model Bridge",
        "redirect_uris": [redirect_uri],
        "grant_types": ["authorization_code", "refresh_token"],
        "response_types": ["code"],
        "token_endpoint_auth_method": "none",
    });
    let resp = client
        .post(registration_endpoint)
        .header("Content-Type", "application/json")
        .json(&body)
        .send().await.map_err(|e| format!("DCR 请求失败: {}", e))?;
    let status = resp.status();
    if !status.is_success() {
        let text = resp.text().await.unwrap_or_default();
        return Err(format!("DCR 返回 {}: {}", status, text));
    }
    let dcr: DcrResponse = resp.json().await.map_err(|e| format!("解析 DCR 响应失败: {}", e))?;
    Ok((dcr.client_id, dcr.client_secret))
}

// ========== 触发授权（start）==========

/// 计算回调 redirect_uri（loopback 用 127.0.0.1，RFC8252）
fn redirect_uri_for(state: &Arc<AppState>) -> String {
    let port = state.config.read().listen_port;
    format!("http://127.0.0.1:{}/oauth/callback", port)
}

/// 触发 OAuth 授权流程，返回供前端打开的 authorize URL。
pub async fn start_oauth_flow(state: &Arc<AppState>, server_id: &str) -> Result<String, String> {
    // 取 server 配置（clone 出锁）
    let server = {
        let cfg = state.config.read();
        cfg.mcp_servers.iter().find(|s| s.id == server_id).cloned()
    };
    let server = server.ok_or_else(|| format!("MCP server 不存在: {}", server_id))?;

    let client = &state.http_client;
    let redirect_uri = redirect_uri_for(state);

    // 发现链（全部锁外）
    let prm_url = discover_prm_url(client, &server.endpoint).await?;
    let (as_url, resource) = fetch_prm(client, &prm_url, &server.endpoint).await?;
    let meta = fetch_as_metadata(client, &as_url).await?;

    let registration_endpoint = meta.registration_endpoint.clone()
        .ok_or_else(|| "上游 AS 不支持动态注册（无 registration_endpoint），当前仅支持 DCR".to_string())?;

    // 复用已注册的 client_id（若有）
    let existing = server.oauth.clone().unwrap_or_default();
    let (client_id, client_secret) = match &existing.client_id {
        Some(cid) if !cid.is_empty() => (cid.clone(), existing.client_secret.clone()),
        _ => register_client(client, &registration_endpoint, &redirect_uri).await?,
    };

    // PKCE + state
    let (verifier, challenge) = gen_pkce();
    let oauth_state = random_token(24);

    // 持久化发现/注册结果（token 暂空）
    let oauth_data = OAuthData {
        authorization_endpoint: Some(meta.authorization_endpoint.clone()),
        token_endpoint: Some(meta.token_endpoint.clone()),
        registration_endpoint: Some(registration_endpoint),
        resource: Some(resource.clone()),
        scope: existing.scope.clone(),
        redirect_uri: Some(redirect_uri.clone()),
        client_id: Some(client_id.clone()),
        client_secret: client_secret.clone(),
        access_token: existing.access_token.clone(),
        refresh_token: existing.refresh_token.clone(),
        token_expires_at: existing.token_expires_at,
    };
    persist_oauth(state, server_id, oauth_data)?;

    // 存 pending（内存）
    {
        let mut store = state.oauth.write();
        // 顺带清理过期 pending
        let now = now_secs();
        store.pending.retain(|_, p| now - p.created_at < PENDING_TTL_SECS);
        store.pending.insert(oauth_state.clone(), PendingAuth {
            server_id: server_id.to_string(),
            code_verifier: verifier,
            redirect_uri: redirect_uri.clone(),
            resource: resource.clone(),
            token_endpoint: meta.token_endpoint.clone(),
            client_id: client_id.clone(),
            client_secret,
            scope: existing.scope.clone(),
            created_at: now_secs(),
        });
    }

    // 构造 authorize URL
    let mut auth_url = url::Url::parse(&meta.authorization_endpoint)
        .map_err(|e| format!("authorization_endpoint 非法: {}", e))?;
    {
        let mut qp = auth_url.query_pairs_mut();
        qp.append_pair("response_type", "code");
        qp.append_pair("client_id", &client_id);
        qp.append_pair("redirect_uri", &redirect_uri);
        qp.append_pair("code_challenge", &challenge);
        qp.append_pair("code_challenge_method", "S256");
        qp.append_pair("state", &oauth_state);
        qp.append_pair("resource", &resource);
        if let Some(scope) = &existing.scope {
            if !scope.is_empty() {
                qp.append_pair("scope", scope);
            }
        }
    }
    Ok(auth_url.to_string())
}

// ========== 回调（callback）==========

#[derive(Deserialize)]
pub struct CallbackParams {
    #[serde(default)]
    pub code: Option<String>,
    #[serde(default)]
    pub state: Option<String>,
    #[serde(default)]
    pub error: Option<String>,
    #[serde(default)]
    pub error_description: Option<String>,
}

#[derive(Deserialize)]
struct TokenResponse {
    access_token: String,
    #[serde(default)]
    refresh_token: Option<String>,
    #[serde(default)]
    expires_in: Option<i64>,
}

/// OAuth 回调 handler（GET /oauth/callback）
pub async fn oauth_callback_handler(
    State(state): State<Arc<AppState>>,
    Query(params): Query<CallbackParams>,
) -> Html<String> {
    match complete_oauth_flow(&state, params).await {
        Ok(name) => Html(result_page("授权成功", &format!("MCP server「{}」已授权，可关闭此窗口。", name), true)),
        Err(e) => Html(result_page("授权失败", &e, false)),
    }
}

async fn complete_oauth_flow(state: &Arc<AppState>, params: CallbackParams) -> Result<String, String> {
    if let Some(err) = params.error {
        let desc = params.error_description.unwrap_or_default();
        return Err(format!("授权服务器返回错误: {} {}", err, desc));
    }
    let code = params.code.ok_or_else(|| "回调缺少 code 参数".to_string())?;
    let oauth_state = params.state.ok_or_else(|| "回调缺少 state 参数".to_string())?;

    // 取出并消费 pending（一次性）
    let pending = {
        let mut store = state.oauth.write();
        store.pending.remove(&oauth_state)
    };
    let pending = pending.ok_or_else(|| "无效或已过期的 state（可能是 CSRF 或超时）".to_string())?;
    if now_secs() - pending.created_at >= PENDING_TTL_SECS {
        return Err("授权请求已超时，请重新发起".to_string());
    }

    // 换 token（锁外）
    let client = &state.http_client;
    let mut form: Vec<(String, String)> = vec![
        ("grant_type".into(), "authorization_code".into()),
        ("code".into(), code),
        ("redirect_uri".into(), pending.redirect_uri.clone()),
        ("code_verifier".into(), pending.code_verifier.clone()),
        ("client_id".into(), pending.client_id.clone()),
        ("resource".into(), pending.resource.clone()),
    ];
    if let Some(secret) = &pending.client_secret {
        form.push(("client_secret".into(), secret.clone()));
    }

    let resp = client
        .post(&pending.token_endpoint)
        .form(&form)
        .send().await.map_err(|e| format!("换取 token 失败: {}", e))?;
    let status = resp.status();
    if !status.is_success() {
        let text = resp.text().await.unwrap_or_default();
        return Err(format!("token endpoint 返回 {}: {}", status, text));
    }
    let tok: TokenResponse = resp.json().await.map_err(|e| format!("解析 token 响应失败: {}", e))?;

    // 写回 config 并持久化
    let server_name = {
        let cfg = state.config.read();
        cfg.mcp_servers.iter().find(|s| s.id == pending.server_id).map(|s| s.name.clone())
    }.unwrap_or_else(|| pending.server_id.clone());

    let mut data = current_oauth(state, &pending.server_id).unwrap_or_default();
    data.access_token = Some(tok.access_token);
    data.refresh_token = tok.refresh_token.or(data.refresh_token);
    data.token_expires_at = tok.expires_in.map(|e| now_secs() + e);
    persist_oauth(state, &pending.server_id, data)?;

    Ok(server_name)
}

// ========== 转发期 token 获取 / 刷新 ==========

/// 转发前调用：返回应注入的 access token。
/// - 未启用 OAuth → Ok(None)（走原有 auth_token/透传逻辑）
/// - token 有效 → Ok(Some(token))
/// - 临近过期且有 refresh → 刷新后返回
/// - 无 token 或刷新失败 → Err(NeedsReauth)
pub async fn ensure_valid_token(state: &Arc<AppState>, server: &McpServerConfig) -> Result<Option<String>, NeedsReauth> {
    if !server.oauth_enabled {
        return Ok(None);
    }
    let data = server.oauth.clone().unwrap_or_default();
    let access = data.access_token.clone();
    let expires_at = data.token_expires_at;

    let needs_refresh = match (&access, expires_at) {
        (None, _) => true,
        (Some(_), Some(exp)) => now_secs() + EXPIRY_SKEW_SECS >= exp,
        (Some(_), None) => false, // 无过期信息，乐观使用
    };

    if !needs_refresh {
        return Ok(Some(access.unwrap()));
    }

    // 需要刷新：必须有 refresh_token
    let refresh = data.refresh_token.clone()
        .ok_or_else(|| NeedsReauth(format!("server {} 未授权或 token 已过期", server.id)))?;

    // per-server 刷新锁
    let lock = {
        let mut store = state.oauth.write();
        store.refresh_locks.entry(server.id.clone())
            .or_insert_with(|| Arc::new(tokio::sync::Mutex::new(())))
            .clone()
    };
    let _guard = lock.lock().await;

    // 拿到锁后重新读 config：可能已被其它请求刷新过
    let fresh = current_oauth(state, &server.id).unwrap_or_default();
    if let (Some(tok), Some(exp)) = (&fresh.access_token, fresh.token_expires_at) {
        if now_secs() + EXPIRY_SKEW_SECS < exp {
            return Ok(Some(tok.clone()));
        }
    }

    let token_endpoint = fresh.token_endpoint.clone()
        .ok_or_else(|| NeedsReauth(format!("server {} 缺少 token_endpoint", server.id)))?;
    let client_id = fresh.client_id.clone()
        .ok_or_else(|| NeedsReauth(format!("server {} 缺少 client_id", server.id)))?;
    let resource = fresh.resource.clone().unwrap_or_default();

    let mut form: Vec<(String, String)> = vec![
        ("grant_type".into(), "refresh_token".into()),
        ("refresh_token".into(), refresh),
        ("client_id".into(), client_id),
    ];
    if !resource.is_empty() {
        form.push(("resource".into(), resource));
    }
    if let Some(secret) = &fresh.client_secret {
        form.push(("client_secret".into(), secret.clone()));
    }

    let resp = state.http_client.post(&token_endpoint).form(&form).send().await
        .map_err(|e| NeedsReauth(format!("刷新 token 网络错误: {}", e)))?;
    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        if status == reqwest::StatusCode::BAD_REQUEST
            || status == reqwest::StatusCode::UNAUTHORIZED
            || body.contains("invalid_client")
            || body.contains("client expired")
            || body.contains("客户端已过期")
        {
            // DCR clients can have a shorter lifetime than refresh tokens. Remove
            // the stale registration so the next authorization flow performs DCR
            // again instead of reusing the expired client_id forever.
            clear_oauth_registration(state, &server.id).map_err(NeedsReauth)?;
        }
        return Err(NeedsReauth(format!("刷新 token 失败（{}），请重新授权", status)));
    }
    let tok: TokenResponse = resp.json().await
        .map_err(|e| NeedsReauth(format!("解析刷新响应失败: {}", e)))?;

    let mut updated = fresh;
    let new_access = tok.access_token.clone();
    updated.access_token = Some(tok.access_token);
    updated.refresh_token = tok.refresh_token.or(updated.refresh_token);
    updated.token_expires_at = tok.expires_in.map(|e| now_secs() + e);
    persist_oauth(state, &server.id, updated).map_err(NeedsReauth)?;

    Ok(Some(new_access))
}

/// Refresh all OAuth-enabled MCP servers whose access token is near expiry.
/// This makes low-traffic MCP servers recover before their next user request.
pub async fn refresh_expiring_tokens(state: &Arc<AppState>) {
    let servers: Vec<McpServerConfig> = {
        let cfg = state.config.read();
        cfg.mcp_servers
            .iter()
            .filter(|server| server.oauth_enabled)
            .cloned()
            .collect()
    };

    let now = now_secs();
    for server in servers {
        let oauth = server.oauth.clone().unwrap_or_default();
        let should_refresh = oauth.access_token.is_some()
            && oauth.refresh_token.is_some()
            && oauth.token_expires_at
                .map(|expires_at| now + EXPIRY_SKEW_SECS >= expires_at)
                .unwrap_or(false);
        if !should_refresh {
            continue;
        }

        match ensure_valid_token(state, &server).await {
            Ok(Some(_)) => tracing::info!(server_id = %server.id, "MCP OAuth token refreshed by background task"),
            Ok(None) => {}
            Err(error) => tracing::warn!(server_id = %server.id, error = %error.0, "MCP OAuth background refresh failed"),
        }
    }
}

/// Clear a dynamic client registration and its tokens while retaining discovery
/// metadata. The next start_oauth_flow call will register a fresh client.
pub fn clear_oauth_registration(state: &Arc<AppState>, server_id: &str) -> Result<(), String> {
    let mut data = current_oauth(state, server_id).unwrap_or_default();
    data.client_id = None;
    data.client_secret = None;
    data.access_token = None;
    data.refresh_token = None;
    data.token_expires_at = None;
    persist_oauth(state, server_id, data)
}

// ========== 辅助：读写 config 中的 oauth ==========

/// 读取某 server 当前的 OAuthData
fn current_oauth(state: &Arc<AppState>, server_id: &str) -> Option<OAuthData> {
    let cfg = state.config.read();
    cfg.mcp_servers.iter().find(|s| s.id == server_id).and_then(|s| s.oauth.clone())
}

/// 写入某 server 的 OAuthData 并落盘
fn persist_oauth(state: &Arc<AppState>, server_id: &str, data: OAuthData) -> Result<(), String> {
    {
        let mut cfg = state.config.write();
        if let Some(s) = cfg.mcp_servers.iter_mut().find(|s| s.id == server_id) {
            s.oauth = Some(data);
        } else {
            return Err(format!("MCP server 不存在: {}", server_id));
        }
    }
    crate::commands::save_config_to_disk(&state.config.read())
}

/// 查询某 server 的授权状态（供状态 API）
pub fn oauth_status(state: &Arc<AppState>, server_id: &str) -> (bool, Option<i64>, bool) {
    let data = current_oauth(state, server_id).unwrap_or_default();
    let has_token = data.access_token.is_some();
    let expires_at = data.token_expires_at;
    let expired = match expires_at {
        Some(exp) => now_secs() >= exp,
        None => false,
    };
    // authorized = 有 access_token 且(未过期 或 有 refresh_token 可刷新)
    let authorized = has_token && (!expired || data.refresh_token.is_some());
    let needs_reauth = !has_token || (expired && data.refresh_token.is_none());
    (authorized, expires_at, needs_reauth)
}

// ========== 回调结果页 ==========

fn result_page(title: &str, message: &str, ok: bool) -> String {
    let color = if ok { "#059669" } else { "#dc2626" };
    format!(
        r#"<!DOCTYPE html><html><head><meta charset="utf-8"><title>{title}</title>
<style>body{{font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#f1f5f9}}
.card{{background:#fff;padding:32px 40px;border-radius:12px;box-shadow:0 2px 12px rgba(0,0,0,.08);text-align:center;max-width:420px}}
h1{{color:{color};font-size:20px;margin:0 0 12px}}p{{color:#475569;font-size:14px;line-height:1.6;margin:0}}</style></head>
<body><div class="card"><h1>{title}</h1><p>{message}</p></div></body></html>"#,
        title = title, message = message, color = color
    )
}
