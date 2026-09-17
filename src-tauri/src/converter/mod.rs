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

    /// 该入口格式能否路由到该渠道格式——**协议转换矩阵的唯一事实来源**。
    ///
    /// 规则一句话：渠道侧要么与请求方同协议（字节透传），要么是 Chat（做转换）。
    /// 推理：Chat 是最不具表现力的一套，把富协议**拍平**成 Chat 是机械且安全的；
    /// 反过来**从 Chat 造出**富协议（Anthropic 的 thinking / cache_control、
    /// Responses 的 item 生命周期）才是易错的方向，且 Anthropic ↔ Responses
    /// 必须两跳串联，保真度差。这两类一律不支持。
    ///
    /// | 请求方 \ 渠道 | Chat | Anthropic | Responses |
    /// |---|---|---|---|
    /// | Chat       | ✓ 透传 | ✗ | ✗ |
    /// | Anthropic  | ✓ 转换 | ✓ 透传 | ✗ |
    /// | Responses  | ✓ 转换 | ✗ | ✓ 透传 |
    pub fn can_route_to(self, to: ApiFormat) -> bool {
        self == to || to == ApiFormat::OpenAIChat
    }
}

/// 不受支持的「入口协议 → 渠道协议」组合。
///
/// 由 [`ApiFormat::can_route_to`] 判定。路由层会先按矩阵过滤渠道，
/// 因此正常情况下不会到达转换层；这里是兜底，避免退化成静默透传或错误转换。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct UnsupportedConversion {
    pub from: ApiFormat,
    pub to: ApiFormat,
}

impl std::fmt::Display for UnsupportedConversion {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "unsupported protocol conversion {:?} -> {:?}",
            self.from, self.to
        )
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

    /// 该 (入口协议, 渠道协议) 组合是否受协议矩阵支持。
    /// 路由层用它过滤渠道，与 [`Self::plan`] 共用 [`ApiFormat::can_route_to`]。
    pub fn is_supported(input: InputFormat, provider: &ProviderType) -> bool {
        ApiFormat::from_input(input).can_route_to(ApiFormat::from_provider(provider))
    }

    /// 构造转换计划（做受支持性检查，executor 使用）：
    /// - `Ok(None)`：入口与渠道同协议 → 字节透传
    /// - `Ok(Some(fc))`：受支持的跨协议转换
    /// - `Err(..)`：矩阵不支持该组合
    pub fn plan(
        input: InputFormat,
        provider: &ProviderType,
    ) -> Result<Option<Self>, UnsupportedConversion> {
        let from = ApiFormat::from_input(input);
        let to = ApiFormat::from_provider(provider);
        if !from.can_route_to(to) {
            return Err(UnsupportedConversion { from, to });
        }
        if from == to {
            return Ok(None);
        }
        Ok(Some(Self {
            from,
            to,
            ns_reverse: new_ns_reverse_map(),
        }))
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
