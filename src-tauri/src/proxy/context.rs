//! 请求上下文：封装原始请求的所有信息，贯穿整个处理流水线。
//! 关键设计：保留原始请求体的字节级表示，不经过 serde 往返序列化。

use axum::http::{HeaderMap, header};
use serde_json::Value;

/// 入口请求格式
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InputFormat {
    OpenAI,     // /v1/chat/completions
    Anthropic,  // /v1/messages
    Responses,  // /v1/responses
}

impl InputFormat {
    /// 协议名，用于错误提示与日志（与用户侧习惯称呼一致）。
    pub fn protocol_name(&self) -> &'static str {
        match self {
            InputFormat::OpenAI => "chat",
            InputFormat::Anthropic => "messages",
            InputFormat::Responses => "responses",
        }
    }

    /// 该入口协议下，渠道侧允许的协议类型描述（用于「无可用渠道」的错误提示）。
    pub fn allowed_provider_hint(&self) -> &'static str {
        match self {
            InputFormat::OpenAI => "a chat channel",
            InputFormat::Anthropic => "a messages or chat channel",
            InputFormat::Responses => "a responses or chat channel",
        }
    }
}

/// 请求上下文
#[derive(Debug, Clone)]
pub struct RequestContext {
    pub input_format: InputFormat,
    pub path: String,
    pub model_alias: String,
    pub is_stream: bool,
    /// 原始请求体字符串（未经 serde 往返，保留字节级一致性）
    pub raw_body: String,
    /// 解析后的请求体（用于格式转换场景，从 raw_body 解析而来）
    pub parsed_body: Value,
    pub parse_error: Option<String>,
    pub original_headers: HeaderMap,
    pub original_headers_str: Option<String>,
    pub user_agent: Option<String>,
}

impl RequestContext {
    /// 从原始请求字符串创建上下文（保留原始字节）
    pub fn from_raw(
        input_format: InputFormat,
        path: &str,
        headers: HeaderMap,
        raw_body: String,
    ) -> Self {
        // 最小解析：只提取 model 和 stream 字段
        let (parsed_body, parse_error) = match serde_json::from_str(&raw_body) {
            Ok(value) => (value, None),
            Err(error) => (Value::Object(serde_json::Map::new()), Some(error.to_string())),
        };

        let model_alias = parsed_body.get("model")
            .and_then(|m| m.as_str())
            .unwrap_or("unknown")
            .to_string();

        let is_stream = parsed_body.get("stream")
            .and_then(|s| s.as_bool())
            .unwrap_or(false);

        let user_agent = headers.get(header::USER_AGENT)
            .and_then(|v| v.to_str().ok())
            .map(|s| s.to_string());

        let original_headers_str = {
            let map: std::collections::HashMap<String, String> = headers.iter()
                .filter_map(|(k, v)| v.to_str().ok().map(|val| (k.to_string(), val.to_string())))
                .collect();
            serde_json::to_string(&map).ok()
        };

        Self {
            input_format,
            path: path.to_string(),
            model_alias,
            is_stream,
            raw_body,
            parsed_body,
            parse_error,
            original_headers: headers,
            original_headers_str,
            user_agent,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderMap;

    #[test]
    fn invalid_json_is_recorded_as_parse_error() {
        let ctx = RequestContext::from_raw(
            InputFormat::Responses,
            "/v1/responses",
            HeaderMap::new(),
            "{invalid".to_string(),
        );
        assert!(ctx.parse_error.is_some());
        assert_eq!(ctx.model_alias, "unknown");
    }
}
