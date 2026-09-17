use std::collections::HashMap;
use std::sync::Arc;

use axum::extract::State;
use axum::http::HeaderMap;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde_json::json;

use crate::channel::config::{AppConfig, AuthConfig, ChannelConfig, EndpointConfig, McpServerConfig, ModelPrice, ProviderType, UltimateFallback, default_true};
use crate::channel::health::ChannelHealth;
use crate::proxy::server::{check_admin_auth, AppState};

// ========== 数据结构 ==========

/// 渠道概要信息（不含敏感的 API Key），用于前端展示
#[derive(Debug, Clone, serde::Serialize)]
pub struct ChannelInfo {
    pub id: String,
    pub name: String,
    pub provider: String,
    pub url: String,
    pub priority: u32,
    pub enabled: bool,
    pub fallback_model: String,
    pub model_mapping: HashMap<String, String>,
}

/// 渠道健康状态信息，用于前端监控面板展示
#[derive(Debug, Clone, serde::Serialize)]
pub struct ChannelHealthInfo {
    pub id: String,
    pub name: String,
    pub state: String,
    pub avg_first_byte_ms: Option<u64>,
    pub avg_latency_ms: Option<u64>,
    pub success_rate: Option<u64>,
    pub total_requests: u64,
    pub recent_failures: u64,
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cache_read_tokens: u64,
    pub cache_creation_tokens: u64,
}

/// 渠道编辑数据（含 API Key），用于新增/修改渠道的前后端交互
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct ChannelEditData {
    pub id: String,
    pub name: String,
    pub provider: String,
    pub url: String,
    pub api_key: String,
    pub priority: u32,
    pub enabled: bool,
    pub fallback_model: String,
    pub model_mapping: HashMap<String, String>,
    pub timeout_ms: u64,
    #[serde(default)]
    pub custom_headers: HashMap<String, String>,
    #[serde(default)]
    pub strip_thinking: bool,
    #[serde(default)]
    pub retry_count: u32,
    #[serde(default = "default_retry_delay_edit")]
    pub retry_delay_ms: u64,
    #[serde(default)]
    pub force_effort: Option<String>,
    #[serde(default = "default_true_edit")]
    pub auto_cache: bool,
}

fn default_retry_delay_edit() -> u64 { 500 }

/// 故障转移配置的编辑数据
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct FailoverEditData {
    /// 候选渠道数上限；`0` = 不限制。兼容旧字段名 `max_retries`（旧前端仍可能提交）。
    #[serde(alias = "max_retries")]
    pub max_failover_channels: u32,
    pub retry_timeout_ms: u64,
    pub failure_threshold: u32,
    pub recovery_interval_sec: u64,
    pub probe_requests: u32,
}

/// 完整配置的视图数据
#[derive(Debug, Clone, serde::Serialize)]
pub struct ConfigViewData {
    pub listen_port: u16,
    pub listen_host: String,
    pub public_url: Option<String>,
    pub models: Vec<String>,
    pub channels: Vec<ChannelEditData>,
    pub failover: FailoverEditData,
    pub auth: AuthEditData,
    pub ultimate_fallback_channel: Option<String>,
    pub mcp_servers: Vec<McpServerEditData>,
    /// 审计日志保留天数：0 = 永久留存
    pub audit_retention_days: u32,
}

/// MCP 中继 server 编辑数据
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct McpServerEditData {
    pub id: String,
    pub name: String,
    pub endpoint: String,
    #[serde(default)]
    pub auth_token: Option<String>,
    #[serde(default)]
    pub custom_headers: HashMap<String, String>,
    #[serde(default)]
    pub blocked_tools: Vec<String>,
    #[serde(default = "default_true_edit")]
    pub enabled: bool,
    #[serde(default)]
    pub oauth_enabled: bool,
    #[serde(default)]
    pub cached_tools: Vec<crate::channel::config::ToolInfo>,
}

fn default_true_edit() -> bool { true }

/// 鉴权配置编辑数据
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct AuthEditData {
    pub proxy_tokens: Vec<String>,
    pub admin_token: Option<String>,
}

// ========== 管理后台 API 处理函数 ==========

/// Report whether admin authentication is enabled and whether the current
/// request carries a valid token. The token itself is never returned.
pub async fn get_auth_status(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> Response {
    let admin_token = state.config.read().auth.admin_token.clone();
    let required = admin_token.as_deref().is_some_and(|token| !token.is_empty());
    let valid = !required || check_admin_auth(&state, &headers).is_none();
    Json(json!({"required": required, "valid": valid})).into_response()
}

#[derive(Debug, serde::Deserialize)]
pub struct StatsQuery {
    pub period: Option<String>,
    pub channel: Option<String>,
}

pub async fn get_stats(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    axum::extract::Query(params): axum::extract::Query<StatsQuery>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let period = params.period.as_deref().unwrap_or("today");
    match state.audit_db.get_stats(period, params.channel.as_deref()) {
        Ok(stats) => Json(json!(stats)).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
pub struct AuditListQuery {
    pub model_filter: Option<String>,
    pub channel_filter: Option<String>,
    pub status_filter: Option<String>,
    pub actual_model_filter: Option<String>,
    pub path_filter: Option<String>,
    pub time_from: Option<String>,
    pub time_to: Option<String>,
    pub limit: Option<u32>,
}

pub async fn get_audit_logs(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    axum::extract::Query(params): axum::extract::Query<AuditListQuery>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    match state.audit_db.query_list(
        params.model_filter.as_deref(),
        params.channel_filter.as_deref(),
        params.status_filter.as_deref(),
        params.actual_model_filter.as_deref(),
        params.path_filter.as_deref(),
        params.time_from.as_deref(),
        params.time_to.as_deref(),
        params.limit.unwrap_or(100),
    ) {
        Ok(entries) => Json(json!(entries)).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
pub struct AuditDetailQuery {
    pub id: i64,
}

pub async fn get_audit_detail(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    axum::extract::Query(params): axum::extract::Query<AuditDetailQuery>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    match state.audit_db.get_by_id(params.id) {
        Ok(Some(entry)) => Json(json!(entry)).into_response(),
        Ok(None) => Json(json!({"error": "Not found"})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

/// 审计数据库概况（文件大小、记录数），供设置页展示
pub async fn get_audit_db_status(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    match state.audit_db.status() {
        Ok(status) => Json(json!(status)).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

/// 强制清理审计数据：删除超期记录（保留天数取配置，0 = 永久留存则跳过）
/// + 详情只保留最近 1000 条 + VACUUM 立即回收磁盘空间。
/// VACUUM 在大库上可能持续数分钟，因此放到阻塞线程执行，避免卡住 tokio 工作线程。
pub async fn force_cleanup_audit(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let retention_days = state.config.read().audit_retention_days.unwrap_or(0);
    let db = state.audit_db.clone();
    let result = tokio::task::spawn_blocking(move || {
        db.force_cleanup(retention_days, crate::audit::db::BODIES_RETAIN_LATEST)
    }).await;
    match result {
        Ok(Ok(r)) => Json(json!(r)).into_response(),
        Ok(Err(e)) => Json(json!({"error": e})).into_response(),
        Err(e) => Json(json!({"error": format!("cleanup task panicked: {}", e)})).into_response(),
    }
}

pub async fn get_channels(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let config = state.config.read();
    let channels: Vec<ChannelInfo> = config.channels.iter().map(|c| ChannelInfo {
        id: c.id.clone(),
        name: c.name.clone(),
        provider: serde_json::to_value(&c.provider).ok().and_then(|v| v.as_str().map(|s| s.to_string())).unwrap_or_else(|| format!("{:?}", c.provider).to_lowercase()),
        url: c.endpoint.url.clone(),
        priority: c.priority,
        enabled: c.enabled,
        fallback_model: c.fallback_model.clone(),
        model_mapping: c.model_mapping.clone(),
    }).collect();
    Json(json!(channels)).into_response()
}

/// Token 用量热力图查询参数
#[derive(Debug, serde::Deserialize)]
pub struct TokenHeatmapQuery {
    pub days: Option<u32>,
}

/// 返回按天聚合的 token 用量，供前端渲染 GitHub 风格活动热力图
pub async fn get_token_heatmap(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    axum::extract::Query(params): axum::extract::Query<TokenHeatmapQuery>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let days = params.days.unwrap_or(91);
    match state.audit_db.get_token_heatmap(days) {
        Ok(data) => Json(json!(data)).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
pub struct ChannelHealthQuery {
    pub period: Option<String>,
}

pub async fn get_channel_health(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    axum::extract::Query(params): axum::extract::Query<ChannelHealthQuery>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let period = params.period.as_deref().unwrap_or("today");
    let channels: Vec<(String, String)> = {
        let config = state.config.read();
        config.sorted_channels().iter()
            .map(|c| (c.id.clone(), c.name.clone()))
            .collect()
    };

    let health_map = state.health_map.read();

    let infos: Vec<ChannelHealthInfo> = channels.iter().map(|(id, name)| {
        let health = health_map.get(id);
        let circuit_state = health
            .map(|h| format!("{:?}", h.state).to_lowercase())
            .unwrap_or_else(|| "healthy".to_string());
        let db_stats = state.audit_db.get_channel_stats_by_period(id, period).unwrap_or_default();
        ChannelHealthInfo {
            id: id.clone(),
            name: name.clone(),
            state: circuit_state,
            avg_first_byte_ms: db_stats.avg_first_byte_ms,
            avg_latency_ms: db_stats.avg_latency_ms,
            success_rate: db_stats.success_rate,
            total_requests: db_stats.total_requests,
            recent_failures: db_stats.error_count,
            input_tokens: db_stats.input_tokens,
            output_tokens: db_stats.output_tokens,
            cache_read_tokens: db_stats.cache_read_tokens,
            cache_creation_tokens: db_stats.cache_creation_tokens,
        }
    }).collect();
    Json(json!(infos)).into_response()
}

pub async fn get_full_config(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let config = state.config.read();
    let data = ConfigViewData {
        listen_port: config.listen_port,
        listen_host: config.listen_host.clone(),
        public_url: config.public_url.clone(),
        models: config.models.clone(),
        channels: config.channels.iter().map(|c| ChannelEditData {
            id: c.id.clone(),
            name: c.name.clone(),
            provider: serde_json::to_value(&c.provider).ok().and_then(|v| v.as_str().map(|s| s.to_string())).unwrap_or_else(|| format!("{:?}", c.provider).to_lowercase()),
            url: c.endpoint.url.clone(),
            api_key: c.api_key.clone(),
            priority: c.priority,
            enabled: c.enabled,
            fallback_model: c.fallback_model.clone(),
            model_mapping: c.model_mapping.clone(),
            timeout_ms: c.timeout_ms,
            custom_headers: c.custom_headers.clone(),
            strip_thinking: c.strip_thinking,
            retry_count: c.retry_count,
            retry_delay_ms: c.retry_delay_ms,
            force_effort: c.force_effort.clone(),
            auto_cache: c.auto_cache,
        }).collect(),
        failover: FailoverEditData {
            max_failover_channels: config.failover.max_failover_channels,
            retry_timeout_ms: config.failover.retry_timeout_ms,
            failure_threshold: config.failover.circuit_breaker.failure_threshold,
            recovery_interval_sec: config.failover.circuit_breaker.recovery_interval_sec,
            probe_requests: config.failover.circuit_breaker.probe_requests,
        },
        auth: AuthEditData {
            proxy_tokens: config.auth.proxy_tokens.clone(),
            admin_token: config.auth.admin_token.clone(),
        },
        ultimate_fallback_channel: config.ultimate_fallback.as_ref().map(|f| f.channel.clone()),
        mcp_servers: config.mcp_servers.iter().map(|s| McpServerEditData {
            id: s.id.clone(),
            name: s.name.clone(),
            endpoint: s.endpoint.clone(),
            auth_token: s.auth_token.clone(),
            custom_headers: s.custom_headers.clone(),
            blocked_tools: s.blocked_tools.clone(),
            enabled: s.enabled,
            oauth_enabled: s.oauth_enabled,
            cached_tools: s.cached_tools.clone(),
        }).collect(),
        audit_retention_days: config.audit_retention_days.unwrap_or(0),
    };
    Json(json!(data)).into_response()
}

pub async fn save_channel(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(channel): Json<ChannelEditData>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let provider = match parse_provider(&channel.provider) {
        Ok(p) => p,
        Err(e) => return Json(json!({"error": e})).into_response(),
    };

    let new_channel = ChannelConfig {
        id: channel.id.clone(),
        name: channel.name,
        provider,
        endpoint: EndpointConfig {
            url: channel.url,
        },
        api_key: channel.api_key,
        priority: channel.priority,
        enabled: channel.enabled,
        fallback_model: channel.fallback_model,
        model_mapping: channel.model_mapping,
        timeout_ms: channel.timeout_ms,
        custom_headers: channel.custom_headers,
        strip_thinking: channel.strip_thinking,
        rate_limit: None,
        retry_count: channel.retry_count,
        retry_delay_ms: channel.retry_delay_ms,
        force_effort: channel.force_effort.filter(|s| !s.is_empty()),
        auto_cache: channel.auto_cache,
    };

    {
        let mut config = state.config.write();
        if let Some(existing) = config.channels.iter_mut().find(|c| c.id == channel.id) {
            *existing = new_channel;
        } else {
            config.channels.push(new_channel);
        }
    }

    {
        let mut health = state.health_map.write();
        health.entry(channel.id).or_insert_with(ChannelHealth::new);
    }

    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DeleteChannelReq {
    pub channel_id: String,
}

pub async fn delete_channel(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<DeleteChannelReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    {
        let mut config = state.config.write();
        config.channels.retain(|c| c.id != req.channel_id);
    }
    {
        let mut health = state.health_map.write();
        health.remove(&req.channel_id);
    }

    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

// ========== MCP 中继 server 管理 ==========

pub async fn save_mcp_server(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(server): Json<McpServerEditData>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }

    {
        let mut config = state.config.write();
        if let Some(existing) = config.mcp_servers.iter_mut().find(|s| s.id == server.id) {
            // 更新可编辑字段，保留已有 oauth 授权数据（不能被编辑保存清掉）
            existing.name = server.name;
            existing.endpoint = server.endpoint;
            existing.auth_token = server.auth_token.filter(|s| !s.is_empty());
            existing.custom_headers = server.custom_headers;
            existing.blocked_tools = server.blocked_tools;
            existing.enabled = server.enabled;
            existing.oauth_enabled = server.oauth_enabled;
            // existing.oauth 保持不变
        } else {
            config.mcp_servers.push(McpServerConfig {
                id: server.id.clone(),
                name: server.name,
                endpoint: server.endpoint,
                auth_token: server.auth_token.filter(|s| !s.is_empty()),
                custom_headers: server.custom_headers,
                blocked_tools: server.blocked_tools,
                enabled: server.enabled,
                oauth_enabled: server.oauth_enabled,
                oauth: None,
                cached_tools: vec![],
            });
        }
    }

    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DeleteMcpServerReq {
    pub server_id: String,
}

pub async fn delete_mcp_server(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<DeleteMcpServerReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    {
        let mut config = state.config.write();
        config.mcp_servers.retain(|s| s.id != req.server_id);
    }

    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

// ========== MCP 工具列表 ==========

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct FetchToolsReq {
    pub server_id: String,
}

/// 主动握手拉取上游工具列表并缓存
pub async fn fetch_mcp_tools(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<FetchToolsReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    // 按 id 直接查（不用 find_mcp_server，它带 enabled 过滤）
    let server = {
        state.config.read().mcp_servers.iter().find(|s| s.id == req.server_id).cloned()
    };
    let server = match server {
        Some(s) => s,
        None => return Json(json!({"error": "MCP server 不存在"})).into_response(),
    };

    match crate::mcp::tools::fetch_tools(&state, &server).await {
        Ok(tools) => {
            {
                let mut config = state.config.write();
                if let Some(s) = config.mcp_servers.iter_mut().find(|s| s.id == req.server_id) {
                    s.cached_tools = tools.clone();
                }
            }
            if let Err(e) = save_config_to_disk(&state.config.read()) {
                return Json(json!({"error": e})).into_response();
            }
            Json(json!({"success": true, "tools": tools})).into_response()
        }
        Err(crate::mcp::tools::FetchToolsError::NeedsAuth(m)) =>
            Json(json!({"error": m, "needsAuth": true})).into_response(),
        Err(crate::mcp::tools::FetchToolsError::Failed(m)) =>
            Json(json!({"error": m})).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ToggleToolReq {
    pub server_id: String,
    pub tool: String,
    pub enabled: bool,
}

/// 启用/禁用单个工具（映射到 blocked_tools 增删）
pub async fn toggle_mcp_tool(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<ToggleToolReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    {
        let mut config = state.config.write();
        let s = match config.mcp_servers.iter_mut().find(|s| s.id == req.server_id) {
            Some(s) => s,
            None => return Json(json!({"error": "MCP server 不存在"})).into_response(),
        };
        if req.enabled {
            s.blocked_tools.retain(|t| t != &req.tool);
        } else if !s.blocked_tools.contains(&req.tool) {
            s.blocked_tools.push(req.tool.clone());
        }
    }
    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

// ========== MCP OAuth ==========

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct StartOauthReq {
    pub server_id: String,
}

/// 触发 OAuth 授权流程，返回 authorize URL 供前端打开
pub async fn start_mcp_oauth(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<StartOauthReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    match crate::mcp::oauth::start_oauth_flow(&state, &req.server_id).await {
        Ok(url) => Json(json!({"authorizeUrl": url})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct OauthStatusQuery {
    pub server_id: String,
}

/// 查询某 MCP server 的 OAuth 授权状态
pub async fn get_mcp_oauth_status(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    axum::extract::Query(params): axum::extract::Query<OauthStatusQuery>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let (authorized, expires_at, needs_reauth) = crate::mcp::oauth::oauth_status(&state, &params.server_id);
    Json(json!({
        "authorized": authorized,
        "expiresAt": expires_at,
        "needsReauth": needs_reauth,
    })).into_response()
}

pub async fn save_failover_config(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(failover): Json<FailoverEditData>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    {
        let mut config = state.config.write();
        config.failover.max_failover_channels = failover.max_failover_channels;
        config.failover.retry_timeout_ms = failover.retry_timeout_ms;
        config.failover.circuit_breaker.failure_threshold = failover.failure_threshold;
        config.failover.circuit_breaker.recovery_interval_sec = failover.recovery_interval_sec;
        config.failover.circuit_breaker.probe_requests = failover.probe_requests;
    }

    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

// ========== 系统设置 ==========

#[derive(Debug, Clone, serde::Deserialize)]
pub struct SystemSettings {
    pub listen_port: u16,
    pub listen_host: String,
    #[serde(default)]
    pub public_url: Option<String>,
    pub models: Vec<String>,
    #[serde(default)]
    pub auth: Option<AuthEditData>,
    #[serde(default)]
    pub ultimate_fallback_channel: Option<String>,
    /// 审计日志保留天数：0 = 永久留存
    #[serde(default)]
    pub audit_retention_days: Option<u32>,
}

pub async fn save_settings(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(settings): Json<SystemSettings>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    {
        let mut config = state.config.write();
        config.listen_port = settings.listen_port;
        config.listen_host = settings.listen_host;
        config.public_url = settings.public_url.filter(|s| !s.is_empty());
        config.models = settings.models;
        if let Some(auth) = settings.auth {
            config.auth = AuthConfig {
                proxy_tokens: auth.proxy_tokens,
                admin_token: auth.admin_token,
            };
        }
        config.ultimate_fallback = settings.ultimate_fallback_channel
            .filter(|s| !s.is_empty())
            .map(|channel| UltimateFallback { channel });
        config.audit_retention_days = settings.audit_retention_days;
    }

    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

// ========== 渠道连通性测试 ==========

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TestChannelReq {
    pub channel_id: String,
}

pub async fn test_channel(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    body: String,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let req: TestChannelReq = match serde_json::from_str(&body) {
        Ok(r) => r,
        Err(e) => return Json(json!({"success": false, "error": format!("Bad request: {}", e)})).into_response(),
    };

    // 查找目标渠道配置
    let channel = {
        let config = state.config.read();
        config.channels.iter().find(|c| c.id == req.channel_id).cloned()
    };
    let channel = match channel {
        Some(c) => c,
        None => return Json(json!({"success": false, "error": "Channel not found"})).into_response(),
    };

    let url = &channel.endpoint.url;

    // 按渠道类型构造对应格式的测试报文与认证头，与真实转发请求保持一致
    let test_body;
    let mut builder = state.http_client.post(url)
        .timeout(std::time::Duration::from_secs(15))
        .header("Content-Type", "application/json");

    match channel.provider {
        ProviderType::Anthropic => {
            // Anthropic Messages API 格式 + x-api-key 认证
            test_body = json!({
                "model": &channel.fallback_model,
                "max_tokens": 16,
                "messages": [{"role": "user", "content": "ping"}],
            });
            builder = builder
                .header("x-api-key", &channel.api_key)
                .header("anthropic-version", "2023-06-01");
        }
        ProviderType::OpenaiResponses => {
            // OpenAI Responses API：必填 input 字段（不是 Chat Completions 的 messages）
            test_body = json!({
                "model": &channel.fallback_model,
                "input": "ping",
            });
            if !channel.api_key.is_empty() {
                builder = builder.header("Authorization", format!("Bearer {}", channel.api_key));
            }
        }
        _ => {
            // OpenAI Chat Completions 格式 + Bearer 认证
            test_body = json!({
                "model": &channel.fallback_model,
                "max_tokens": 16,
                "messages": [{"role": "user", "content": "ping"}],
            });
            if !channel.api_key.is_empty() {
                builder = builder.header("Authorization", format!("Bearer {}", channel.api_key));
            }
        }
    }

    // 叠加渠道自定义请求头（与真实转发请求一致，可能覆盖上面的默认头）
    for (k, v) in &channel.custom_headers {
        builder = builder.header(k.as_str(), v.as_str());
    }

    let start = std::time::Instant::now();
    let result = builder.json(&test_body).send().await;
    let latency_ms = start.elapsed().as_millis() as u64;

    match result {
        Ok(resp) => {
            let status = resp.status().as_u16();
            let resp_body = resp.text().await.unwrap_or_default();
            Json(json!({
                "success": status < 400,
                "status_code": status,
                "latency_ms": latency_ms,
                "response_body": resp_body,
            })).into_response()
        }
        Err(e) => {
            Json(json!({
                "success": false,
                "error": format!("{}", e),
                "latency_ms": latency_ms,
            })).into_response()
        }
    }
}

// ========== 模型价格管理 ==========

/// 列出所有模型定价
pub async fn get_model_prices(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    let prices = state.config.read().model_prices.clone();
    Json(json!(prices)).into_response()
}

#[derive(Debug, serde::Deserialize)]
pub struct ModelPriceReq {
    pub model: String,
    #[serde(default)]
    pub input_per_mtok: f64,
    #[serde(default)]
    pub output_per_mtok: f64,
    #[serde(default)]
    pub cache_read_per_mtok: Option<f64>,
    #[serde(default)]
    pub cache_write_per_mtok: Option<f64>,
    #[serde(default = "default_true")]
    pub enabled: bool,
}

/// 保存（新增或更新）单个模型定价
pub async fn save_model_price(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<ModelPriceReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    if req.model.trim().is_empty() {
        return Json(json!({"error": "模型名不能为空"})).into_response();
    }
    let price = ModelPrice {
        model: req.model.trim().to_string(),
        input_per_mtok: req.input_per_mtok,
        output_per_mtok: req.output_per_mtok,
        cache_read_per_mtok: req.cache_read_per_mtok,
        cache_write_per_mtok: req.cache_write_per_mtok,
        enabled: req.enabled,
    };
    {
        let mut config = state.config.write();
        if let Some(existing) = config.model_prices.iter_mut().find(|p| p.model == price.model) {
            *existing = price;
        } else {
            config.model_prices.push(price);
        }
    }
    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DeleteModelPriceReq {
    pub model: String,
}

/// 删除单个模型定价
pub async fn delete_model_price(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(req): Json<DeleteModelPriceReq>,
) -> Response {
    if let Some(reject) = check_admin_auth(&state, &headers) { return reject; }
    {
        let mut config = state.config.write();
        config.model_prices.retain(|p| p.model != req.model);
    }
    match save_config_to_disk(&state.config.read()) {
        Ok(_) => Json(json!({"success": true})).into_response(),
        Err(e) => Json(json!({"error": e})).into_response(),
    }
}

// ========== 辅助函数 ==========

fn parse_provider(s: &str) -> Result<ProviderType, String> {
    match s.to_lowercase().as_str() {
        "openai" => Ok(ProviderType::Openai),
        "openai_responses" | "openairesponses" => Ok(ProviderType::OpenaiResponses),
        "anthropic" => Ok(ProviderType::Anthropic),
        _ => Err(format!("Unknown provider: {}", s)),
    }
}

fn get_config_path() -> std::path::PathBuf {
    let home = std::env::var("HOME")
        .or_else(|_| std::env::var("USERPROFILE"))
        .unwrap_or_else(|_| ".".to_string());
    std::path::PathBuf::from(home).join(".model-bridge").join("config.yaml")
}

pub(crate) fn save_config_to_disk(config: &AppConfig) -> Result<(), String> {
    let path = get_config_path();
    let yaml = serde_yaml::to_string(config)
        .map_err(|e| format!("Failed to serialize config: {}", e))?;
    std::fs::write(&path, yaml)
        .map_err(|e| format!("Failed to write config file: {}", e))?;
    tracing::info!("Config saved to {:?}", path);
    Ok(())
}
