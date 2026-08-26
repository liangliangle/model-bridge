//! 协议互转模块：OpenAI Chat / OpenAI Responses / Anthropic Messages 三套协议两两互转。
//! 以 Chat Completion 为 canonical 中间表示；请求/响应/流式全覆盖。

pub mod common;
pub mod request;
pub mod response;
pub mod stream;

use serde_json::Value;

use crate::channel::config::ProviderType;
use crate::proxy::context::InputFormat;

use common::{new_ns_reverse_map, NsReverseMap};

/// 三套协议的统一格式标识。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ApiFormat {
    OpenAIChat,
    OpenAIResponses,
    Anthropic,
}

impl ApiFormat {
    pub fn from_input(f: InputFormat) -> Self {
        match f {
            InputFormat::OpenAI => ApiFormat::OpenAIChat,
            InputFormat::Responses => ApiFormat::OpenAIResponses,
            InputFormat::Anthropic => ApiFormat::Anthropic,
        }
    }

    pub fn from_provider(p: &ProviderType) -> Self {
        match p {
            ProviderType::Openai => ApiFormat::OpenAIChat,
            ProviderType::OpenaiResponses => ApiFormat::OpenAIResponses,
            ProviderType::Anthropic => ApiFormat::Anthropic,
        }
    }
}

/// 格式转换器：编排「入口格式 ↔ Chat ↔ 目标 provider 格式」两跳转换。
/// 持有请求级 ns_reverse 映射，用于 namespace 工具的双向转换。
#[derive(Debug, Clone)]
pub struct FormatConverter {
    from: ApiFormat,
    to: ApiFormat,
    ns_reverse: NsReverseMap,
}

impl FormatConverter {
    /// 格式一致 → None（透传，executor 走原有字节透传路径）。
    pub fn from_formats(input: InputFormat, provider: &ProviderType) -> Option<Self> {
        let from = ApiFormat::from_input(input);
        let to = ApiFormat::from_provider(provider);
        if from == to {
            return None;
        }
        Some(Self {
            from,
            to,
            ns_reverse: new_ns_reverse_map(),
        })
    }

    pub fn from_format(&self) -> ApiFormat {
        self.from
    }

    pub fn to_format(&self) -> ApiFormat {
        self.to
    }

    pub fn ns_reverse(&self) -> &NsReverseMap {
        &self.ns_reverse
    }

    /// 请求转换：入口格式 → Chat（canonical）→ 目标格式。
    pub fn convert_request(&self, body: &Value, actual_model: &str) -> Value {
        let chat = match self.from {
            ApiFormat::OpenAIChat => body.clone(),
            ApiFormat::OpenAIResponses => {
                request::responses_to_openai_with_ns(body, actual_model, &self.ns_reverse)
            }
            ApiFormat::Anthropic => request::anthropic_to_openai(body, actual_model),
        };
        match self.to {
            ApiFormat::OpenAIChat => request::finalize_openai(chat, actual_model),
            ApiFormat::OpenAIResponses => request::openai_to_responses(&chat, actual_model),
            ApiFormat::Anthropic => request::openai_to_anthropic(&chat, actual_model),
        }
    }

    /// 响应转换：provider 响应（to 格式）→ Chat → 入口格式（from）。
    pub fn convert_response(&self, body: &Value, model: &str) -> Value {
        let chat = match self.to {
            ApiFormat::OpenAIChat => body.clone(),
            ApiFormat::OpenAIResponses => response::responses_to_openai_response(body, model),
            ApiFormat::Anthropic => response::anthropic_to_openai_response(body, model),
        };
        match self.from {
            ApiFormat::OpenAIChat => chat,
            ApiFormat::OpenAIResponses => {
                response::to_responses_with_ns(&chat, model, &self.ns_reverse)
            }
            ApiFormat::Anthropic => response::openai_to_anthropic_response(&chat, model),
        }
    }

    /// 创建对应方向的流式转换器。
    pub fn create_stream_converter(&self) -> Box<dyn stream::StreamConverter> {
        stream::build(self.from, self.to, self.ns_reverse.clone())
    }

    /// 降级路径：上游对流式请求返回了非 SSE 的完整 JSON 时，
    /// 先把它（to 格式）转成入口格式（from），再组装成入口格式的完整 SSE 事件流字节。
    pub fn full_response_to_sse(&self, upstream_full: &Value, model: &str) -> Vec<u8> {
        let converted = self.convert_response(upstream_full, model);
        stream::replay_response_as_sse(self.from, &converted)
    }
}
