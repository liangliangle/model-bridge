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

网关以 **Chat Completions 作为唯一的转换枢纽**。渠道侧允许的协议取决于请求方的协议：

| 请求方（入口） | Chat 渠道 | Anthropic (Messages) 渠道 | Responses 渠道 |
|---|---|---|---|
| Chat（`/v1/chat/completions`） | 透传 | ✗ | ✗ |
| Messages（`/v1/messages`） | 转换 | 透传 | ✗ |
| Responses（`/v1/responses`） | 转换 | ✗ | 透传 |

规则一句话：**渠道侧要么与请求方同协议（字节透传），要么是 Chat 渠道（做转换）。**

为什么这样限定：

- 把 Messages / Responses 请求**拍平**成 Chat 是机械且安全的；反过来**从 Chat 造出** Messages / Responses 的完整语义（thinking、cache_control、Responses 的 item 生命周期）才是易错方向。
- Messages ↔ Responses 需要两跳串联（中间经过 Chat），保真度差，同样不支持。

由此带来两个行为，配置时需要知道：

- **协议过滤优先于优先级**。不可服务的渠道会被直接排除——例如 Chat 请求方永远用不上你的 Claude 渠道，即使它 `priority: 1`。剩余候选之间仍严格按 `priority` 排序。
- 没有渠道能服务该请求协议时，网关在**派发前**返回 `400` 并说明原因，不会静默降级、也不会白白消耗一次上游调用。

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

- [Rust](https://rustup.rs/) (1.75+)
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

# 3. 编译后端（会将 dist/ 嵌入二进制）
cd src-tauri
cargo build --release

# 产物位于 src-tauri/target/release/model-bridge
```

> 必须先 `pnpm build` 再 `cargo build`，因为 Rust 编译时会把 `dist/` 以 `include_bytes!` 嵌入二进制。

#### 开发模式

前端热重载开发（不含后端）：

```bash
pnpm dev
```

后端单独运行（使用已编译的前端）：

```bash
cd src-tauri
cargo run
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
