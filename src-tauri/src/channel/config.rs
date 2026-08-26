use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::Path;

/// 应用全局配置，包含监听地址、渠道列表、故障转移策略等
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AppConfig {
    #[serde(default = "default_port")]
    pub listen_port: u16, // 监听端口，默认 8080
    #[serde(default = "default_host")]
    pub listen_host: String, // 监听地址，默认 0.0.0.0
    /// 可选的公网域名或完整的外部 URL，面板中将优先使用该地址显示代理端点
    #[serde(default)]
    pub public_url: Option<String>,
    /// 开启后打印完整的请求/响应体到控制台，便于调试协议转换
    #[serde(default)]
    pub debug: bool,
    /// 自定义 /v1/models 返回的模型列表。为空则自动从渠道映射中收集。
    #[serde(default)]
    pub models: Vec<String>,
    #[serde(default)]
    pub channels: Vec<ChannelConfig>, // 渠道配置列表
    #[serde(default)]
    pub failover: FailoverConfig, // 故障转移配置
    #[serde(default)]
    pub ultimate_fallback: Option<UltimateFallback>, // 终极兜底渠道
    #[serde(default)]
    pub auth: AuthConfig, // 鉴权配置
    #[serde(default)]
    pub mcp_servers: Vec<McpServerConfig>, // MCP 中继上游 server 列表
    #[serde(default)]
    pub skill_manager: SkillManagerConfig, // Skill 统一管理配置
    /// 审计日志主表保留天数：None 或 0 = 永久留存；>0 = 删除超过 N 天的记录。
    /// 详情大字段（请求/响应体）始终只保留最近 1000 条，见 audit::db::BODIES_RETAIN_LATEST。
    #[serde(default)]
    pub audit_retention_days: Option<u32>,
}

/// 鉴权配置
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct AuthConfig {
    /// 代理端点的访问令牌列表，为空则不鉴权
    #[serde(default)]
    pub proxy_tokens: Vec<String>,
    /// 管理后台令牌，为空则不鉴权
    #[serde(default)]
    pub admin_token: Option<String>,
}

/// 默认监听地址（绑定本机，防止未鉴权时暴露到网络）
fn default_host() -> String {
    "127.0.0.1".to_string()
}
/// 默认监听端口
fn default_port() -> u16 {
    8080
}

/// 单个渠道的配置，定义供应商、接口地址、模型映射等
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ChannelConfig {
    pub id: String, // 渠道唯一标识
    pub name: String, // 渠道显示名称
    pub provider: ProviderType, // 供应商类型
    /// 完整的 API endpoint URL，不同接口类型分别配置
    pub endpoint: EndpointConfig,
    #[serde(default)]
    pub api_key: String, // API 密钥
    #[serde(default = "default_priority")]
    pub priority: u32, // 优先级，数值越小优先级越高
    #[serde(default = "default_true")]
    pub enabled: bool, // 是否启用该渠道
    pub fallback_model: String, // 无映射时使用的兜底模型名称
    #[serde(default)]
    pub model_mapping: HashMap<String, String>, // 模型别名到实际模型名的映射
    #[serde(default = "default_timeout")]
    pub timeout_ms: u64, // 请求超时时间（毫秒）
    #[serde(default)]
    pub rate_limit: Option<u32>, // 速率限制（每秒请求数）
    /// 自定义请求头，会覆盖默认生成的 header（如 Authorization、Content-Type 等）
    #[serde(default)]
    pub custom_headers: HashMap<String, String>,
    /// 是否在发送前移除 thinking/redacted_thinking 内容块
    #[serde(default)]
    pub strip_thinking: bool,
    /// 单渠道内重试次数（0 = 不重试，直接切换下一个渠道）
    #[serde(default)]
    pub retry_count: u32,
    /// 重试间隔（毫秒），默认 500
    #[serde(default = "default_retry_delay")]
    pub retry_delay_ms: u64,
    /// 强制覆盖请求体的 output_config.effort 值（如 "max"）。
    /// 为 None 时不覆盖，保留请求原始值。
    #[serde(default)]
    pub force_effort: Option<String>,
    /// 仅对 Anthropic 目标渠道生效：当转换后的请求中不存在任何 cache_control
    /// 断点时，网关自动注入 prompt caching 断点。默认开启。
    #[serde(default = "default_true")]
    pub auto_cache: bool,
}

/// Endpoint 配置
/// 用户填写渠道 API 的完整 URL，根据 provider 类型不同含义不同：
/// - openai → Chat Completions 接口（如 https://api.openai.com/v1/chat/completions）
/// - openai_responses → Responses API 接口（如 https://api.openai.com/v1/responses）
/// - anthropic → Messages API 接口（如 https://api.anthropic.com/v1/messages）
///
/// 序列化始终输出新格式（`url` 字段）。
/// 反序列化兼容两种格式：
/// - 新格式：`url: "https://..."`
/// - 旧格式：`chat_completions: "https://..."`（+ 可选 `responses: "https://..."`）
///   旧格式中若同时存在 `responses`，优先使用 `responses`（对应 openai_responses 渠道）。
#[derive(Debug, Clone, Serialize)]
pub struct EndpointConfig {
    /// 渠道 API 的完整 URL（必填）
    pub url: String,
}

impl<'de> serde::Deserialize<'de> for EndpointConfig {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        // 用 serde_json::Value 做灵活解析，同时兼容 YAML 和 JSON
        let value = serde_json::Value::deserialize(deserializer)?;

        // 新格式：url 字段
        if let Some(url) = value.get("url").and_then(|v| v.as_str()) {
            return Ok(EndpointConfig { url: url.to_string() });
        }

        // 旧格式：chat_completions（+ 可选 responses）
        if let Some(cc) = value.get("chat_completions").and_then(|v| v.as_str()) {
            // 若同时存在 responses 字段，优先使用（对应 openai_responses 渠道）
            let url = value
                .get("responses")
                .and_then(|v| v.as_str())
                .unwrap_or(cc)
                .to_string();
            return Ok(EndpointConfig { url });
        }

        Err(serde::de::Error::custom(
            "endpoint must have 'url' or 'chat_completions' field",
        ))
    }
}

/// 默认优先级
fn default_priority() -> u32 {
    10
}
/// 默认启用
fn default_true() -> bool {
    true
}
/// 默认超时时间（毫秒）
fn default_timeout() -> u64 {
    300000
}
/// 默认重试间隔（毫秒）
fn default_retry_delay() -> u64 {
    500
}

/// 模型供应商类型枚举
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "snake_case")]
pub enum ProviderType {
    Openai,            // OpenAI Chat Completions
    OpenaiResponses,   // OpenAI Responses API
    Anthropic,         // Anthropic Messages API
}

/// 故障转移配置，控制重试次数、超时和熔断策略
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FailoverConfig {
    #[serde(default = "default_max_retries")]
    pub max_retries: u32, // 最大重试次数
    #[serde(default = "default_retry_timeout")]
    pub retry_timeout_ms: u64, // 单次重试超时时间（毫秒）
    #[serde(default)]
    pub circuit_breaker: CircuitBreakerConfig, // 熔断器配置
}

impl Default for FailoverConfig {
    fn default() -> Self {
        Self {
            max_retries: default_max_retries(),
            retry_timeout_ms: default_retry_timeout(),
            circuit_breaker: CircuitBreakerConfig::default(),
        }
    }
}

/// 默认最大重试次数
fn default_max_retries() -> u32 {
    3
}
/// 默认重试超时时间（毫秒）
fn default_retry_timeout() -> u64 {
    5000
}

/// 熔断器配置，控制渠道何时被熔断以及何时尝试恢复
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CircuitBreakerConfig {
    #[serde(default = "default_failure_threshold")]
    pub failure_threshold: u32, // 触发熔断的连续失败次数
    #[serde(default = "default_recovery_interval")]
    pub recovery_interval_sec: u64, // 熔断后尝试恢复的间隔（秒）
    #[serde(default = "default_probe_requests")]
    pub probe_requests: u32, // 恢复阶段需要的连续成功请求数
}

impl Default for CircuitBreakerConfig {
    fn default() -> Self {
        Self {
            failure_threshold: default_failure_threshold(),
            recovery_interval_sec: default_recovery_interval(),
            probe_requests: default_probe_requests(),
        }
    }
}

/// 默认失败阈值
fn default_failure_threshold() -> u32 {
    3
}
/// 默认恢复间隔（秒）
fn default_recovery_interval() -> u64 {
    30
}
/// 默认探测请求数
fn default_probe_requests() -> u32 {
    2
}

/// 终极兜底配置，当所有渠道不可用时使用指定渠道
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UltimateFallback {
    pub channel: String, // 兜底渠道的 ID
}

/// MCP 中继上游 server 配置。每个 server 对应一个 /mcp/{id} 端点，
/// 转发到上游 MCP server，并按黑名单裁剪工具。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct McpServerConfig {
    pub id: String,       // server 唯一标识，对应 /mcp/{id} 路径
    pub name: String,     // 显示名称
    pub endpoint: String, // 上游 MCP server 的完整 endpoint URL
    /// 注入到上游的 Bearer token。为 None 则透传客户端的 Authorization。
    #[serde(default)]
    pub auth_token: Option<String>,
    /// 自定义请求头，转发时叠加（可覆盖默认头）
    #[serde(default)]
    pub custom_headers: HashMap<String, String>,
    /// 工具黑名单：tools/list 中删除这些工具，tools/call 调用这些工具时拒绝（按 name 精确匹配）
    #[serde(default)]
    pub blocked_tools: Vec<String>,
    #[serde(default = "default_true")]
    pub enabled: bool,
    /// 是否对上游启用 OAuth 2.1 流程。开启后忽略 auth_token，使用 oauth 中的 access_token。
    #[serde(default)]
    pub oauth_enabled: bool,
    /// OAuth 发现/注册/token 的持久化快照（重启复用，避免重新发现/注册）
    #[serde(default)]
    pub oauth: Option<OAuthData>,
    /// 上游工具列表快照（主动握手拉取后缓存，详情页读它而非实时拉）
    #[serde(default)]
    pub cached_tools: Vec<ToolInfo>,
}

/// MCP 工具信息快照（从上游 tools/list 拉取）
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct ToolInfo {
    pub name: String,
    #[serde(default)]
    pub description: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_schema: Option<serde_json::Value>,
}

/// OAuth 2.1 流程的持久化数据：发现结果 + DCR 注册 + token。
/// 全部字段可选，随 AppConfig 序列化进 config.yaml。
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct OAuthData {
    // 发现 + DCR 结果
    #[serde(default)]
    pub authorization_endpoint: Option<String>,
    #[serde(default)]
    pub token_endpoint: Option<String>,
    #[serde(default)]
    pub registration_endpoint: Option<String>,
    #[serde(default)]
    pub resource: Option<String>, // RFC8707 canonical URI
    #[serde(default)]
    pub scope: Option<String>,
    #[serde(default)]
    pub redirect_uri: Option<String>, // DCR/authorize/token 三处必须逐字一致
    #[serde(default)]
    pub client_id: Option<String>,
    #[serde(default)]
    pub client_secret: Option<String>, // 机密客户端才有
    // token（重启不丢；access 可能已过期，转发时懒刷新）
    #[serde(default)]
    pub access_token: Option<String>,
    #[serde(default)]
    pub refresh_token: Option<String>,
    #[serde(default)]
    pub token_expires_at: Option<i64>, // Unix 秒
}

/// Skill 统一管理配置：扫描各 agent 的 skill 目录，以中心库为源做软链接启停/分发。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SkillManagerConfig {
    /// 中心库路径（被管理 skill 的真实存储），默认 ~/.agents/skills
    #[serde(default = "default_central_lib")]
    pub central_lib: String,
    /// 纳入扫描的 agent skill 根目录列表（内置 + 用户自定义）
    #[serde(default = "default_skill_agents")]
    pub agents: Vec<SkillAgentTarget>,
}

impl Default for SkillManagerConfig {
    fn default() -> Self {
        Self {
            central_lib: default_central_lib(),
            agents: default_skill_agents(),
        }
    }
}

/// 一个 agent 的 skill 根目录
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SkillAgentTarget {
    pub name: String,      // 唯一标识：claude / cursor / 自定义
    pub root_path: String, // skill 根目录，支持 ~ 和 ${HOME}
    #[serde(default = "default_true")]
    pub enabled: bool, // 是否纳入扫描
    #[serde(default)]
    pub builtin: bool, // 标记默认配置项，允许在 Agent 管理中删除
    /// 该 agent 是否原生直接读取中心库（~/.agents/skills）。
    /// 为 true 时中心库 skill 对它自动全部生效，无需也不应建软链接到 root_path。
    #[serde(default)]
    pub native_agents_dir: bool,
}

fn default_central_lib() -> String {
    "~/.agents/skills".to_string()
}

fn default_skill_agents() -> Vec<SkillAgentTarget> {
    vec![
        SkillAgentTarget { name: "claude".to_string(), root_path: "~/.claude/skills".to_string(), enabled: true, builtin: true, native_agents_dir: false },
        SkillAgentTarget { name: "cursor".to_string(), root_path: "~/.cursor/skills".to_string(), enabled: true, builtin: true, native_agents_dir: false },
    ]
}

impl AppConfig {
    /// 从 YAML 文件加载配置，支持 ${VAR} 和 ${VAR:-default} 环境变量替换
    pub fn load_from_file(path: &Path) -> Result<Self, String> {
        let content = std::fs::read_to_string(path)
            .map_err(|e| format!("Failed to read config file: {}", e))?;
        let expanded = expand_env_vars(&content);
        serde_yaml::from_str(&expanded)
            .map_err(|e| format!("Failed to parse config: {}", e))
    }

    /// 生成默认配置实例
    pub fn default_config() -> Self {
        Self {
            listen_port: 8080,
            listen_host: "127.0.0.1".to_string(),
            public_url: None,
            debug: false,
            models: vec![],
            channels: vec![],
            failover: FailoverConfig::default(),
            ultimate_fallback: None,
            auth: AuthConfig::default(),
            mcp_servers: vec![],
            skill_manager: SkillManagerConfig::default(),
            audit_retention_days: None, // 默认永久留存
        }
    }

    /// 按 id 查找已启用的 MCP server 配置
    pub fn find_mcp_server(&self, id: &str) -> Option<&McpServerConfig> {
        self.mcp_servers.iter().find(|s| s.id == id && s.enabled)
    }

    /// 获取按优先级排序的已启用渠道列表（数值越小优先级越高）
    pub fn sorted_channels(&self) -> Vec<&ChannelConfig> {
        let mut channels: Vec<&ChannelConfig> = self.channels.iter()
            .filter(|c| c.enabled)
            .collect();
        channels.sort_by_key(|c| c.priority);
        channels
    }
}

impl ChannelConfig {
    /// 解析模型名称：优先查找映射表，未命中则使用兜底模型
    pub fn resolve_model(&self, alias: &str) -> (String, MappingSource) {
        if let Some(mapped) = self.model_mapping.get(alias) {
            (mapped.clone(), MappingSource::Explicit)
        } else {
            (self.fallback_model.clone(), MappingSource::Fallback)
        }
    }
}

/// 模型映射来源，标识模型名是从映射表匹配还是使用兜底
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "snake_case")]
pub enum MappingSource {
    Explicit, // 来自映射表的精确匹配
    Fallback, // 使用兜底模型
}

/// 展开配置中的环境变量引用：${VAR} 和 ${VAR:-default}
fn expand_env_vars(content: &str) -> String {
    let mut result = String::with_capacity(content.len());
    let mut chars = content.chars().peekable();
    while let Some(ch) = chars.next() {
        if ch == '$' && chars.peek() == Some(&'{') {
            chars.next(); // consume '{'
            let mut expr = String::new();
            while let Some(&c) = chars.peek() {
                if c == '}' { chars.next(); break; }
                expr.push(c);
                chars.next();
            }
            let (var, default) = if let Some(pos) = expr.find(":-") {
                (&expr[..pos], &expr[pos + 2..])
            } else {
                (expr.as_str(), "")
            };
            result.push_str(&std::env::var(var).unwrap_or_else(|_| default.to_string()));
        } else {
            result.push(ch);
        }
    }
    result
}

/// 返回默认的 YAML 配置模板字符串
pub fn default_config_yaml() -> &'static str {
    r#"# Model Bridge 配置文件
listen_port: 8080

# Debug 模式：打印完整的请求/响应体到控制台（含协议转换前后的对比）
# 需配合 RUST_LOG=debug 启动，例如：RUST_LOG=debug ./model-bridge
# debug: true

# 鉴权配置（为空则不启用鉴权）
# 支持环境变量：api_key: "${OPENAI_API_KEY}"
auth:
  # 代理端点访问令牌（客户端需在 Authorization: Bearer <token> 中携带）
  proxy_tokens: []
  # 管理后台令牌（为空则不启用管理鉴权）
  # admin_token: "your-admin-secret"

channels:
  - id: "openai-primary"
    name: "OpenAI (Chat)"
    provider: openai
    endpoint:
      url: "https://api.openai.com/v1/chat/completions"
    api_key: "sk-xxx"
    priority: 1
    enabled: true
    fallback_model: "gpt-4o-mini"
    model_mapping:
      "gpt-4": "gpt-4o"
      "gpt-4o": "gpt-4o"
      "gpt-4o-mini": "gpt-4o-mini"

  # OpenAI Responses API 渠道（直接透传 Responses 格式）
  # - id: "openai-responses"
  #   name: "OpenAI (Responses)"
  #   provider: openai_responses
  #   endpoint:
  #     url: "https://api.openai.com/v1/responses"
  #   api_key: "sk-xxx"
  #   priority: 2
  #   enabled: true
  #   fallback_model: "gpt-4o"

  - id: "anthropic-main"
    name: "Anthropic (Messages)"
    provider: anthropic
    endpoint:
      url: "https://api.anthropic.com/v1/messages"
    api_key: "sk-ant-xxx"
    priority: 2
    enabled: true
    fallback_model: "claude-sonnet-4-6"
    model_mapping:
      "gpt-4": "claude-opus-4-6"
      "gpt-4o": "claude-sonnet-4-6"
      "gpt-4o-mini": "claude-haiku-4"

failover:
  max_retries: 3
  retry_timeout_ms: 5000
  circuit_breaker:
    failure_threshold: 3
    recovery_interval_sec: 30
    probe_requests: 2

# MCP 中继：转发到上游 MCP server 并按黑名单裁剪工具
# 客户端接入地址为 http://<host>:<port>/mcp/<id>
# mcp_servers:
#   - id: "my-mcp"
#     name: "示例 MCP Server"
#     endpoint: "https://example.com/mcp"
#     # auth_token 配置后会注入 Authorization: Bearer；留空则透传客户端的 Authorization
#     auth_token: "${MCP_TOKEN}"
#     enabled: true
#     # 黑名单工具（按 name 精确匹配），tools/list 中删除、tools/call 时拒绝
#     blocked_tools:
#       - "unused_tool_a"
#       - "unused_tool_b"

# Skill 统一管理：扫描各 agent 的 skill 目录，以中心库为源做软链接启停/分发
# skill_manager:
#   central_lib: "~/.agents/skills"   # 中心库（被管理 skill 的真实存储）
#   agents:
#     - name: "claude"
#       root_path: "~/.claude/skills"
#       enabled: true
#       builtin: true
#     - name: "cursor"
#       root_path: "~/.cursor/skills"
#       enabled: true
#       builtin: true
"#
}
