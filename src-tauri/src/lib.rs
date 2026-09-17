//! Model Bridge — 大模型 API 代理与模型映射工具
//!
//! 模块结构：
//! - `audit`    — 审计日志（SQLite 存储；详情大字段仅保留最近 N 条，见 `audit::db::BODIES_RETAIN_LATEST`）
//! - `channel`  — 渠道管理（配置、健康检查、故障转移）
//! - `commands` — 管理 API（供前端调用的 HTTP 接口）
//! - `provider` — 提供商协议适配（OpenAI、Anthropic、通用兼容）
//! - `proxy`    — 代理核心（axum HTTP 服务、请求路由）
//! - `static_files` — 前端静态资源（编译时嵌入二进制）

pub mod audit;
pub mod channel;
pub mod converter;
pub mod cost;
pub mod commands;
pub mod mcp;
pub mod provider;
pub mod proxy;
pub mod static_files;
