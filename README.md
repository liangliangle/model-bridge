# Model Bridge

大模型 API 代理网关 —— 统一入口、模型映射、自动故障转移。

单个二进制文件，前端管理界面编译时嵌入，开箱即用。

## 功能

- **多协议代理** — 同时支持 OpenAI Chat Completions / Responses API 和 Anthropic Messages API
- **模型映射** — 客户端发 `gpt-4`，后端按配置路由到 Claude、DeepSeek、Qwen 等任意模型
- **多渠道故障转移** — 按优先级自动切换，内置熔断器（Circuit Breaker）与恢复探测
- **单渠道重试** — 每个渠道可独立配置重试次数和间隔
- **审计日志** — SQLite 存储全部请求/响应，支持按模型、渠道、状态码筛选
- **Web 管理界面** — 仪表盘、渠道配置、审计查看，内嵌在二进制中
- **Token 鉴权** — 代理端点和管理后台可分别配置访问令牌
- **环境变量替换** — 配置文件支持 `${VAR}` 和 `${VAR:-default}` 语法

## 支持的供应商

| Provider | 类型 | 示例 |
|---|---|---|
| OpenAI | `openai` | GPT-4o、GPT-4o-mini |
| Anthropic | `anthropic` | Claude Opus、Sonnet、Haiku |
| DeepSeek | `openai_compatible` | deepseek-chat、deepseek-reasoner |
| 通义千问 | `openai_compatible`（或 `dashscope`） | qwen-max、qwen-plus |
| Ollama | `openai_compatible` | llama3、codellama |
| 任意 OpenAI 兼容接口 | `openai_compatible` | — |

## 协议转换矩阵

**任意协议互转**：三种协议的请求方都可以落到三种协议的渠道上，转换以 **Chat Completions 为唯一枢纽**（最多两跳）。

| 请求方（入口） | Chat 渠道 | Anthropic (Messages) 渠道 | Responses 渠道 |
|---|---|---|---|
| Chat（`/v1/chat/completions`） | 透传 | 单跳转换 | 单跳转换 |
| Messages（`/v1/messages`） | 单跳转换 | 透传 | 两跳（经 Chat） |
| Responses（`/v1/responses`） | 单跳转换 | 两跳（经 Chat） | 透传 |

为什么经 Chat 中转：把 Messages / Responses 请求**拍平**成 Chat 是机械且安全的，反过来**从 Chat 造出**富协议语义才是易错方向；因此两个富协议之间不复刻第三套直连映射，而是把两条已验证的单跳串起来。

由此带来三个行为，配置时需要知道：

- **协议不参与渠道决策**：候选渠道只看是否启用与健康，顺序**完全**由 `priority` 决定。优先级高的渠道即使协议与请求方不同也会被选中，并为此付出一次（或两次）协议转换。若你希望某类请求走原生协议渠道，请用 `priority` 表达。
- 唯一的选路失败是「没有启用且健康的渠道」（`503`）。矩阵全开后不再有「渠道协议都不匹配」这种失败。
- 流式是**真增量**：逐事件翻译并立即下发，不会为了转换而先攒完整个响应。

### 有损清单

转换本身是字段级映射，但有几处是协议之间**没有对应概念**的，属于已知有损。无法表达且会改变调用方语义的字段会直接返回 `400`（错误信息点名字段与目标协议）；其余会被丢弃或改写，并在日志里以 `[convert] … losses=…` 记录一次（`debug: false` 时也记录，因为这是"结果与请求不完全一致"的唯一线索）。

| 场景 | 处理 | 说明 |
|---|---|---|
| Chat 的 `n > 1` | **400** | 目标协议每次请求只返回一条结果 |
| Chat 的 `logprobs` / `top_logprobs` | **400** | 目标协议没有 token 级对数概率 |
| Responses 的 `previous_response_id` / `conversation` | **400** | 会话状态无法跨协议续接 |
| 只有 `encrypted_content`、没有可读 `summary` 的 Responses reasoning 项 | **400** | 没有可搬运的文本 |
| assistant 历史里的 `reasoning_content` → Messages / Responses | 丢弃并记日志 | **thinking 块的 `signature` 无法伪造**（上游会校验），因此不造 thinking 块；这也意味着「经 Chat 的往返不可能无损」 |
| Messages 的 `thinking` 块 → Chat | 只保留文本（签名丢弃） | 同上，反向 |
| Chat 的 `seed` / `frequency_penalty` / `presence_penalty` / `logit_bias` | 丢弃并记日志 | 目标协议无对应参数 |
| Chat 的 `stop` → Responses | 丢弃并记日志 | Responses 没有 stop sequences |
| Anthropic 的 `top_k` / `redacted_thinking` | 丢弃并记日志 | Chat 侧无承载位置 |
| Chat 未给 `max_tokens` → Messages | 补默认值并记日志 | Anthropic 的 `max_tokens` 必填，兜底值为 4096 |
| 缓存写入量（`cache_creation_input_tokens`） | 跨协议时并入输入量 | 只有 Anthropic 区分"缓存写入"；**审计与计费仍按上游原始值统计**，不受影响 |

注意：**审计与成本统计始终取上游原始响应**，因此上表的有损只影响「转换后回给客户端的 usage 数值」，不影响账单口径。

## 快速开始

### 使用预编译二进制

```bash
# 直接运行
./model-bridge

# 首次启动会自动生成配置文件: ~/.model-bridge/config.yaml
# 编辑配置后重启即可
```

### 从源码编译

#### 前置依赖

- [Go](https://go.dev/dl/) (1.23+)
- [Node.js](https://nodejs.org/) (18+)
- [pnpm](https://pnpm.io/) (`npm install -g pnpm`)

#### 一键打包

```bash
./build.sh
```

产出在 `release/` 目录：

```
release/
├── model-bridge          ← 单文件，内含前端
└── config.example.yaml
```

#### 手动分步编译

```bash
# 1. 安装前端依赖
pnpm install

# 2. 构建前端（输出到 dist/）
pnpm build

# 3. 编译后端（会把 dist/ 同步进 go/internal/web/dist 后嵌入二进制）
cd go
./build.sh

# 产物位于 go/bin/model-bridge
```

> 必须先 `pnpm build`：Go 用 `//go:embed` 内嵌前端，而 `go:embed` 不能引用模块目录之外的文件，
> 所以 `go/build.sh` 会先把仓库根的 `dist/` 复制进 `go/internal/web/dist/` 再编译。

#### 开发模式

前端热重载开发（不含后端）：

```bash
pnpm dev
```

后端单独运行（使用已编译的前端）：

```bash
cd go
go build -p 1 -o bin/model-bridge ./cmd/model-bridge
./bin/model-bridge
```

或者用根目录的脚本一条命令完成「停止旧进程 → 编译 → 后台启动」：

```bash
./restart.sh
```

## 配置

配置文件路径：`~/.model-bridge/config.yaml`

首次启动自动生成默认模板，也可从 `config.example.yaml` 复制。支持 `${ENV_VAR}` 环境变量替换。

### 最小配置示例

```yaml
listen_port: 8080

channels:
  - id: "my-openai"
    name: "OpenAI"
    provider: openai
    endpoint:
      chat_completions: "https://api.openai.com/v1/chat/completions"
    api_key: "${OPENAI_API_KEY}"
    priority: 1
    enabled: true
    fallback_model: "gpt-4o-mini"
    model_mapping:
      "gpt-4": "gpt-4o"
```

### 配置项说明

#### 顶层配置

| 字段 | 说明 | 默认值 |
|---|---|---|
| `listen_port` | 监听端口 | `8080` |
| `listen_host` | 监听地址 | `127.0.0.1` |
| `models` | 自定义 `/v1/models` 返回列表，为空则从渠道映射自动收集 | `[]` |

#### 渠道配置 (`channels[]`)

| 字段 | 说明 | 默认值 |
|---|---|---|
| `id` | 渠道唯一标识 | — |
| `name` | 显示名称 | — |
| `provider` | 供应商类型：`openai` / `anthropic` / `dashscope` / `openai_compatible` | — |
| `endpoint.chat_completions` | 完整的 API URL | — |
| `api_key` | API 密钥，支持 `${ENV}` | — |
| `priority` | 优先级，数值越小越优先 | `10` |
| `enabled` | 是否启用 | `true` |
| `fallback_model` | 模型映射未命中时使用的兜底模型 | — |
| `model_mapping` | 模型别名 → 实际模型名的映射表 | `{}` |
| `timeout_ms` | 请求超时（毫秒） | `300000` |
| `retry_count` | 单渠道内重试次数 | `0` |
| `retry_delay_ms` | 重试间隔（毫秒） | `500` |
| `custom_headers` | 自定义请求头 | `{}` |
| `strip_thinking` | 是否移除 thinking 内容块 | `false` |

#### 故障转移配置 (`failover`)

| 字段 | 说明 | 默认值 |
|---|---|---|
| `max_failover_channels` | 单次请求最多尝试几个候选渠道（跨渠道上限）；`0` = 不限制 | `3` |
| `retry_timeout_ms` | 单次重试超时 | `5000` |
| `circuit_breaker.failure_threshold` | 触发熔断的连续失败次数 | `3` |
| `circuit_breaker.recovery_interval_sec` | 熔断后恢复探测间隔（秒） | `30` |
| `circuit_breaker.probe_requests` | 恢复阶段需连续成功的请求数 | `2` |

> **两个不同维度的「重试」**：`failover.max_failover_channels` 约束一次请求最多尝试**几个渠道**（跨渠道）；
> `channels[].retry_count` 约束同一个渠道内**重试几次**。二者相互独立——
> 一次请求的总尝试次数上限约为 `max_failover_channels × (1 + retry_count)`。
>
> 早于本版本配置中的 `max_retries` 仍可正常加载，它等价于现在的 `max_failover_channels`。

#### 鉴权配置 (`auth`)

```yaml
auth:
  # 代理端点令牌（客户端需携带 Authorization: Bearer <token>）
  proxy_tokens:
    - "your-proxy-token"
  # 管理后台令牌（为空则不鉴权）
  admin_token: "your-admin-secret"
```

## API 端点

启动后可用的端点（默认 `http://localhost:8080`）：

### 代理 API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | OpenAI Chat Completions 格式 |
| POST | `/v1/completions` | OpenAI Completions 格式 |
| POST | `/v1/responses` | OpenAI Responses API 格式 |
| POST | `/v1/messages` | Anthropic Messages API 格式 |
| GET | `/v1/models` | 可用模型列表 |

### 管理界面

浏览器访问 `http://localhost:8080` 即可打开管理界面。

## 使用示例

### 作为 OpenAI 兼容代理

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

请求中的 `gpt-4` 会按渠道优先级和模型映射自动路由到实际的后端模型。

### 配合 OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="your-proxy-token",  # 对应 auth.proxy_tokens
)

response = client.chat.completions.create(
    model="gpt-4",
    messages=[{"role": "user", "content": "Hello"}],
)
```

## 数据存储

| 文件 | 路径 | 说明 |
|---|---|---|
| 配置文件 | `~/.model-bridge/config.yaml` | YAML 格式，通过管理界面修改会自动保存 |
| 审计数据库 | `~/.model-bridge/audit.db` | SQLite，自动清理 30 天前的记录 |

## License

MIT
