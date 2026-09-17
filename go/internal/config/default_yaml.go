package config

// DefaultConfigYAML 是首次启动时写入 ~/.model-bridge/config.yaml 的默认配置模板。
// 内容与 Rust `channel::config::default_config_yaml` 逐字一致（含注释与空行），
// 由脚本从 Rust 源抽取，请勿手工编辑。
func DefaultConfigYAML() string {
	return defaultConfigYAML
}

const defaultConfigYAML = `# Model Bridge 配置文件
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
  # 单次请求最多尝试几个候选渠道（跨渠道上限）；0 = 不限制。
  # 注意：单渠道内的重试次数由 channels[].retry_count 控制，二者独立。
  max_failover_channels: 3
  retry_timeout_ms: 5000
  circuit_breaker:
    failure_threshold: 3
    recovery_interval_sec: 30
    probe_requests: 2

# 模型定价表（按实际模型精确匹配），用于成本计算。单价单位：美元 / 百万 token。
# model_prices:
#   - model: "gpt-4o"
#     input_per_mtok: 2.5
#     output_per_mtok: 10.0
#     cache_read_per_mtok: 1.25
#   - model: "claude-sonnet-4-6"
#     input_per_mtok: 3.0
#     output_per_mtok: 15.0
#     cache_read_per_mtok: 0.30
#     cache_write_per_mtok: 3.75

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

`
