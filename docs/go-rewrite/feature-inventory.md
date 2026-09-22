# model-bridge 功能清单（Go 重写的契约基准）

本文件是 Go 重写的**对照基准**：先冻结契约，再写实现，避免边写边改口径。

来源（权威顺序）：

| 来源 | 用途 |
|---|---|
| `src-tauri/src/proxy/server.rs` | 路由表（端点全集） |
| `src/lib/tauri.ts` | 前端命令 → 端点映射（证明每个端点都被 UI 使用） |
| `src-tauri/src/commands.rs` | 管理 API 响应结构 |
| `src/components/*.tsx`、`src/App.tsx` | UI 可见功能与所需字段 |
| `config.example.yaml` | 配置字段与语义 |
| `ocgo/cmd/ocgo/main.go` | 协议转换的照抄来源 |

约定：Go 重写**不改**前端（`src/`）与 macOS 客户端的任何字节，因此下述端点路径、方法、查询参数、JSON 字段名均为**必须逐字保持**的对外契约。

---

## 1. 代理协议入口（5 条）

| 方法 | 路径 | 入口协议 | 说明 |
|---|---|---|---|
| POST | `/v1/chat/completions` | Chat | OpenAI Chat Completions |
| POST | `/v1/completions` | Chat | 与上者同一处理器（legacy 别名） |
| POST | `/v1/responses` | Responses | OpenAI Responses API |
| POST | `/v1/messages` | Messages | Anthropic Messages API |
| GET | `/v1/models` | — | 模型列表；配置为空时自动从渠道 `model_mapping` 收集 |

代理入口鉴权：`auth.proxy_tokens` 非空时要求 `Authorization: Bearer <token>`。

### 1.1 协议转换矩阵

以 Chat 为唯一转换枢纽，**单跳**：

| 客户端入口 | Chat 渠道 | Messages 渠道 | Responses 渠道 |
|---|---|---|---|
| Chat | 透传 | ✗ | ✗ |
| Messages | 转换 | 透传 | ✗ |
| Responses | 转换 | ✗ | 透传 |

规则：渠道侧要么与请求方同协议（字节透传），要么是 Chat 渠道（做转换）。不可服务的组合在**派发前**返回错误，不静默降级。

> **与 OBJECTIVE 的偏差，需记录**：OBJECTIVE 要求「协议转换完全照抄 ocgo」。ocgo 的矩阵更宽（含 Responses→Anthropic 两跳、Chat→Anthropic），且 ocgo 无渠道概念。本清单以「照抄 ocgo 的**映射函数**」为界，矩阵与渠道路由保留本仓库现状。详见 `## Deviations`（写入规划文件）。

### 1.2 转换实现（照抄来源：`ocgo/cmd/ocgo/main.go`）

| ocgo 函数 | 行号 | Go 重写对应 |
|---|---|---|
| `proxyMessages` | 1001 | messages 入口编排 |
| `proxyChatCompletions` | 1057 | chat 入口编排 |
| `proxyResponses` | 1116 | responses 入口编排 |
| `convertRequest` | 1586 | Messages → Chat 请求映射 |
| `responsesToChat` | 1603 | Responses → Chat 请求映射 |
| `chatToAnthropic` | 1650 | Chat → Messages 请求映射 |
| `contentToOpenAI` | 2219 | 消息内容块展开（text/image/tool_use/tool_result） |
| `streamAnthropic` | 2496 | Chat 流 → Messages SSE |
| `writeAnthropicResponse` | 2607 | Chat 非流式 → Messages 响应 |
| `parseAnthropicResponse` | 2631 | Messages 响应解析 |
| `writeChatCompletionsResponseFromAnthropic` | 2665 | Messages 非流式 → Chat 响应 |
| `writeResponsesResponseFromAnthropic` | 2677 | Messages 非流式 → Responses 响应 |
| `streamChatCompletionsFromAnthropic` | 2690 | Messages 流 → Chat SSE |
| `streamResponsesFromAnthropic` | 2755 | Messages 流 → Responses SSE |
| `streamResponses` | 2891 | Chat 流 → Responses SSE |
| `writeResponsesResponse` | 3048 | Chat 非流式 → Responses 响应 |

**照抄时必须保留的 ocgo 行为**（这些是它的价值所在）：

- `reasoning_effort` 折叠：`thinking`/`reasoning`/`output_config`/`reasoning_effort`/`effort`/`level`/`depth` → 单个 `reasoning_effort`（`downstreamReasoningEffort`，1444）
- 工具消息消毒：清理孤儿 `tool_calls`、缺失 `tool_result` 补占位（`sanitizeOAIToolMessages` 1935 / `sanitizeRawChatToolMessages` 2001）
- `reasoning_content` 按 tool-call id 回显缓存（2104/2122）
- tool 结果截断（`truncateToolResultContent` 1291）
- 图像能力校验与 detail 剥离（1541/1555）
- 流式 usage 真实下发（`anthropicUsage`/`anthropicDeltaUsage`/`openAIUsage`/`responsesUsage`）
- `stream_options.include_usage`：Chat 目标渠道的流式请求需注入（见 `## Deviations`，本仓库已修，照抄 ocgo 时不可丢）

---

## 2. MCP 中继与 OAuth 回调

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/mcp/{server_id}` | **无**（见下方修正） | JSON-RPC 转发到上游 MCP server |
| GET | `/mcp/{server_id}` | **无** | SSE / 会话读取 |
| DELETE | `/mcp/{server_id}` | **无** | 会话终止 |
| GET | `/oauth/callback` | **无**（靠 `state` 参数校验） | OAuth 2.1 授权服务器重定向目标 |

> **修正（实现期核对源码后）**：原先此处写的是「代理 token」。核对 `src-tauri/src/proxy/server.rs`
> 后确认：`check_proxy_auth` 只在 `/v1/*` 四个 handler 内被调用，MCP 三条路由与 `/oauth/callback`
> 都没有挂任何鉴权中间件，`mcp_post`/`mcp_get`/`mcp_delete` 自身也不做校验。Go 重写按「与现状一致」
> 保持同样行为（不加鉴权），未擅自收紧。

功能要点：

- 按 `blocked_tools` 黑名单裁剪：`tools/list` 中删除，`tools/call` 时拒绝（按 name 精确匹配）
- 头部注入：`auth_token` 配置后注入 `Authorization: Bearer`；留空则透传客户端 Authorization
- OAuth 2.1（可选）：发现 → 动态客户端注册（DCR）→ 授权 → token；快照持久化到配置，重启复用

---

## 3. 管理 API（`/api/*`，共 24 个端点）

统一约定：以 `admin_token` 鉴权（`Authorization: Bearer`，未配置则免鉴权）；错误响应体为 `{"error": "..."}`；成功为 JSON 对象或数组。

### 3.1 GET（11）

| 路径 | 查询参数 | 响应要点 | UI 消费者 |
|---|---|---|---|
| `/api/auth/status` | — | `{required: bool, valid: bool}` | `AuthPage` |
| `/api/stats` | `period`, `channel` | 统计概览（请求数、token 等） | `Dashboard` |
| `/api/stats/heatmap` | — | 按天聚合的 token 用量 | `Dashboard` |
| `/api/audit` | `model_filter`, `channel_filter`, `status_filter`, `actual_model_filter`, `path_filter`, `time_from`, `time_to`, `limit`(默认 100) | 审计条目数组（轻量，不含详情大字段） | `AuditPanel` |
| `/api/audit/detail` | `id` | 单条完整报文；不存在时 `{"error":"Not found"}` | `AuditDetail` |
| `/api/audit/db/status` | — | `{size_bytes, total_records, detail_records}` | `Settings` |
| `/api/channels` | — | 渠道简略信息数组 | `ChannelConfig` |
| `/api/channels/health` | — | 渠道健康状态（状态机 + 延迟） | `ChannelConfig` |
| `/api/config` | — | 完整配置（含渠道详情与 API Key）+ `ultimate_fallback_channel` | `ChannelConfig`/`Settings` |
| `/api/config/mcp/oauth/status` | — | MCP server OAuth 授权状态 | `McpConfig` |
| `/api/model-prices` | — | 模型价格数组 | `ModelPriceConfig` |

### 3.2 POST（13）

| 路径 | 请求体 | 说明 |
|---|---|---|
| `/api/audit/cleanup` | — | 删超期记录 + 详情只留最近 1000 条 + `VACUUM` 回收空间 |
| `/api/config/channel` | `ChannelEditData` | 新建/更新渠道 |
| `/api/config/channel/delete` | `{channelId}` | 删除渠道 |
| `/api/config/failover` | `FailoverEditData` | 保存故障转移配置 |
| `/api/config/settings` | 设置对象 | 端口、host、模型列表、`public_url`、`audit_retention_days` 等 |
| `/api/config/channel/test` | 渠道配置 | 连通性测试 |
| `/api/config/mcp` | `McpServerEditData` | 新建/更新 MCP server |
| `/api/config/mcp/delete` | `{id}` | 删除 MCP server |
| `/api/config/mcp/oauth/start` | `{id}` | 触发 OAuth 授权 |
| `/api/config/mcp/tools/fetch` | `{id}` | 主动拉取上游工具列表并缓存 |
| `/api/config/mcp/tools/toggle` | `{id, tool_name, enabled}` | 启用/禁用单个工具 |
| `/api/model-prices` | `ModelPrice` | 新建/更新模型价格 |
| `/api/model-prices/delete` | `{model}` | 删除模型价格 |

> 注：`/api/model-prices` 同时注册 GET 与 POST（前端 `get_model_prices` / `save_model_price` 复用同一路径）。
> `tauri.ts` 的 POST 约定：若 `args` 的第一个值是对象则用它作为 body，否则用整个 `args` —— 因此 `delete_channel` 一类接收 `{channelId}` 裸字段。

---

## 4. 用户可见功能（8 项）

前端导航为 6 个入口（`src/App.tsx`）：仪表盘 `/`、审计日志 `/audit`、渠道配置 `/channels`、MCP 中继 `/mcp`、模型价格 `/prices`、系统设置 `/settings`。

### 4.1 渠道与优先级（`/channels`）

- 渠道字段：`id`, `name`, `provider`(`openai`/`openai_responses`/`anthropic`), `endpoint.url`, `api_key`, `priority`（数值小者优先）, `enabled`, `fallback_model`, `model_mapping`, `timeout_ms`, `rate_limit`, `custom_headers`, `strip_thinking`, `retry_count`, `retry_delay_ms`, `force_effort`, `auto_cache`
- 拖拽调整优先级；模型映射表编辑
- 路由顺序：**先按协议矩阵硬过滤**，剩余候选**严格按 `priority`**（相同则保持配置顺序）

### 4.2 故障转移与熔断（`/settings` 的故障转移区）

- `failover.max_failover_channels`：单次请求最多尝试几个渠道；`0` = 不限制（**非**重试次数）
- `channels[].retry_count` + `retry_delay_ms`：单渠道内重试
- 熔断器：`failure_threshold`、`recovery_interval_sec`、`probe_requests`；状态 `Healthy`/`Degraded`/`Unhealthy`/`Recovering`
- `ultimate_fallback.channel`：全部失败后的最终兜底（同样受协议矩阵约束）

### 4.3 审计日志（`/audit`）

- SQLite 落库；记录请求/响应报文、路由链、重试次数、token、成本、耗时、状态码
- 列表按 model/channel/status/actual_model/path/时间范围筛选
- 详情大字段独立表，始终只保留最近 1000 条（`BODIES_RETAIN_LATEST`）；`audit_retention_days`（0 = 永久）
- 支持强制清理 + `VACUUM`

### 4.4 统计与热力图（`/`）

- 概览：按 `period`（默认 today）与 `channel` 过滤的请求数、token、成本等
- 热力图：按天聚合的 token 用量

### 4.5 模型定价与成本（`/prices`）

- `model_prices[]`：`model`（精确匹配 `actual_model`）、`input_per_mtok`、`output_per_mtok`、`cache_read_per_mtok`、`cache_write_per_mtok`、`enabled`
- 成本公式（单位 USD）：`(input*in + output*out + cache_read*read_rate + cache_creation*write_rate) / 1e6`；`cache_*_per_mtok` 缺省回退 `input_per_mtok`；无匹配价格则为空

### 4.6 MCP 中继（`/mcp`）

- 每个 server 一个 `/mcp/{id}` 端点；上游工具列表主动拉取后缓存
- 工具黑名单裁剪；自定义请求头；OAuth 2.1 可选

### 4.7 鉴权

- 两套独立令牌：`auth.proxy_tokens`（代理入口）、`auth.admin_token`（管理 API）
- 管理端 401 时前端清除本地 token 并跳转 `AuthPage`
- `/oauth/callback` 不走 admin 鉴权（靠 state 校验）

### 4.8 内嵌前端

- 前端构建产物在编译期嵌入二进制（Rust 用 `include_bytes!`；Go 用 `//go:embed`）
- `/` 返回 HTML 页面；静态资源按路径返回，MIME 按扩展名推断
- 未匹配路由回落到静态资源处理器

---

## 5. 配置 Schema（`~/.model-bridge/config.yaml`）

顶层：`listen_port`(8080)、`listen_host`(127.0.0.1)、`public_url`、`debug`、`models[]`、`channels[]`、`failover`、`ultimate_fallback`、`auth`、`mcp_servers[]`、`model_prices[]`、`audit_retention_days`

- 支持 `${VAR}` 与 `${VAR:-default}` 环境变量展开
- 默认配置：首次启动若文件不存在则写入默认模板
- **兼容性要求**：`failover.max_retries` 需继续作为 `max_failover_channels` 的别名被接受；序列化时写新名

---

## 6. Go 实现映射（建议模块划分）

| Go 包 | 对应 Rust | 职责 |
|---|---|---|
| `internal/config` | `channel/config.rs` | YAML 加载、`${VAR}` 展开、默认模板、渠道模型解析 |
| `internal/converter` | `converter/` | 纯映射函数（请求/响应/SSE），不碰网络 |
| `internal/converter/sse` | `stream.rs` 的 `SseBuffer` | 跨 chunk 边界切事件 |
| `internal/channel` | `channel/health.rs` `failover.rs` | 健康状态机、熔断、路由选择（矩阵过滤 + 优先级） |
| `internal/proxy` | `proxy/` | 路由、执行、流式转发、错误检测 |
| `internal/audit` | `audit/` | SQLite 落库与查询、统计、热力图 |
| `internal/cost` | `cost/` | 成本计算 |
| `internal/mcp` | `mcp/` | 中继、工具裁剪、OAuth |
| `internal/api` | `commands.rs` | 管理 API 24 端点 |
| `internal/web` | `static_files.rs` | 内嵌前端 |
| `cmd/model-bridge` | `main.rs` | 启动、优雅退出、PID 文件 |

设计约束（来自规划 Implementation approach）：

- **转换逻辑与 I/O 分离**：映射为纯函数，上游 I/O 在外层 → mock 上游只需替换 I/O 边界
- **SSE 解析独立**：用「被切成任意边界的字节流」测试
- **逐函数移植 ocgo，不要重新设计**
- **上游地址必须可注入**：来自渠道配置，mock 上游直接作为渠道端点接入

---

## 7. 验证对应关系（规划 Verification plan → 本清单）

| 验证项 | 覆盖的本清单章节 |
|---|---|
| 1 功能清单完整性 | 全文 |
| 2 构建与启动 | §4.8、§3 |
| 3 非流式转换 | §1.1、§1.2 |
| 4 流式转换 | §1.2 流式行 |
| 5 管理 API 契约 | §3.1、§3.2 |
| 6 渠道优先级与故障转移 | §4.1、§4.2 |
| 7 审计与成本落库 | §4.3、§4.5 |
| 8 MCP 中继 | §2、§4.6 |
