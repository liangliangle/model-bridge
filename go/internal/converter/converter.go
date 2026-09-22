// Package converter 实现三套协议的**单跳**转换，以 OpenAI Chat Completions 为唯一中枢：
//
//	请求方向：messages → chat、responses → chat
//	响应方向：chat → messages、chat → responses
//
// 同协议之间是字节透传，由代理层负责，不在本包范围内（与 Rust
// src-tauri/src/converter/mod.rs 的协议矩阵一致）。矩阵的唯一事实来源是
// ApiFormat.CanRouteTo（对应 Rust `ApiFormat::can_route_to`）：渠道侧要么与请求方
// 同协议（透传），要么是 Chat（本包转换）。
//
// 实现由两处合并而来（每个函数上方都标注了对应实现，便于逐一对照）：
//
//   - ocgo cmd/ocgo/main.go：纯映射机制（内容块映射、推理强度折叠、工具消息清洗、
//     tool_result 内容预算、Responses 内建工具、usage 字段读取等）。
//   - 本仓库 Rust src-tauri/src/converter：协议矩阵、usage 口径（common.rs）、
//     namespace / custom 工具的双向还原、Responses 事件生命周期与流式收尾时机。
//
// 两者冲突时以 Rust 契约为准：
//
//  1. cache_control 在两个方向都保留（不移植 ocgo 的 stripCacheControl）；
//  2. 非流式 Anthropic 形态响应必须带 tool_use 块（不沿用 ocgo 丢弃 tool_calls 的写法）；
//  3. Responses 的 item 生命周期与「response.completed 必带 usage 对象」；
//  4. namespace 展平 + custom 工具往返（靠 session 状态）；
//  5. 流式收尾推迟到 [DONE]/EOF，以携带上游真实 usage（不写死 0）；
//  6. custom_tool_call 输出项 + unwrap_custom_tool_arguments。
//
// 其余纯映射细节（内容取值形态、工具 schema 兜底、工具消息清洗、推理强度归一化）
// 沿用 ocgo。不支持的组合一律返回 *UnsupportedConversion，不做静默透传。
package converter

import (
	"io"
	"strings"

	"modelbridge/internal/sse"
)

// ApiFormat 是三套协议的统一格式标识（对应 Rust converter/mod.rs::ApiFormat）。
type ApiFormat int

const (
	// FormatOpenAIChat 是 OpenAI Chat Completions，也是唯一的转换中枢。
	FormatOpenAIChat ApiFormat = iota
	// FormatResponses 是 OpenAI Responses API。
	FormatResponses
	// FormatAnthropic 是 Anthropic Messages API。
	FormatAnthropic
)

// String 返回协议短名："chat" | "responses" | "messages"。
func (f ApiFormat) String() string {
	switch f {
	case FormatOpenAIChat:
		return "chat"
	case FormatResponses:
		return "responses"
	case FormatAnthropic:
		return "messages"
	default:
		return "unknown"
	}
}

// ParseApiFormat 解析协议名。接受 "chat"、"openai_chat"、"openai"、"responses"、
// "messages"、"anthropic"（大小写与首尾空白不敏感）。
func ParseApiFormat(s string) (ApiFormat, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "chat", "openai_chat", "openai":
		return FormatOpenAIChat, true
	case "responses":
		return FormatResponses, true
	case "messages", "anthropic":
		return FormatAnthropic, true
	default:
		return FormatOpenAIChat, false
	}
}

// CanRouteTo 判定该入口格式能否路由到目标渠道格式——协议转换矩阵的唯一事实来源
// （对应 Rust `ApiFormat::can_route_to`）。规则一句话：目标侧要么与入口同协议
// （字节透传），要么是 Chat（做转换）。
func (f ApiFormat) CanRouteTo(to ApiFormat) bool {
	return f == to || to == FormatOpenAIChat
}

// UnsupportedConversion 表示协议矩阵不支持的「入口协议 → 渠道协议」组合。
// 对应 Rust converter/mod.rs::UnsupportedConversion。
type UnsupportedConversion struct {
	From ApiFormat
	To   ApiFormat
}

func (e *UnsupportedConversion) Error() string {
	return "unsupported protocol conversion " + e.From.String() + " -> " + e.To.String()
}

// Usage 是一次请求的 token 用量。
//
// 口径与 Rust converter/common.rs::StreamUsage 一致：
//   - InputTokens 采用 Anthropic 口径，**不含**缓存命中部分；
//   - Chat / Responses 载荷里的 input/prompt tokens 含缓存命中，读入时会扣除；
//   - 写出 Anthropic 形态时不再加回；写出 Responses 形态时把缓存命中加回 input。
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

// RequestOptions 描述一次上游请求转换的可调项。
type RequestOptions struct {
	// Model 是写入出站请求体的上游模型名；为空时保留入站请求里的 model。
	Model string
	// Stream 是本次请求是否流式（以它为准写入出站体的 stream 字段）。
	Stream bool
	// SupportsImages 为 false 时，带图片的请求必须直接失败。
	SupportsImages bool
}

// session 承载一次代理请求的转换状态：namespace/custom 工具的反向映射在转换
// **请求**时写入，转换**响应**或流式时读回。请求与响应两个方向共用同一个实例，
// 由 Hop 持有（见 hop.go）。session 不保证并发安全。
//
// 四个方法的参数都是「源格式 → 目标格式」，请求方向与响应方向正好相反；同型参数
// 可互换正是 hop.go 要消除的隐患，因此这些方法**不导出**，外部一律走 Hop。
//
// 对应 Rust converter/mod.rs::FormatConverter 中的 ns_reverse。
type convSession struct {
	// nsReverse: flat_name → (namespace_name, subtool_name)。
	// custom 工具的 namespace 位置存放 CUSTOM_TOOL_NAMESPACE_MARKER。
	nsReverse map[string]nsEntry
}

// newConvSession 创建一个空的转换会话状态。
func newConvSession() *convSession {
	return &convSession{nsReverse: map[string]nsEntry{}}
}

// buildUpstreamRequest 把下游请求体（in 格式）转换为上游请求体（out 格式）。
// 仅支持 messages->chat 与 responses->chat；其它组合返回 *UnsupportedConversion。
func (s *convSession) buildUpstreamRequest(in, out ApiFormat, body []byte, opts RequestOptions) ([]byte, error) {
	if out != FormatOpenAIChat || (in != FormatAnthropic && in != FormatResponses) {
		return nil, &UnsupportedConversion{From: in, To: out}
	}
	reqObj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	var converted map[string]any
	if in == FormatAnthropic {
		converted, err = convertRequest(reqObj, opts)
	} else {
		converted, err = s.responsesToChat(reqObj, opts)
	}
	if err != nil {
		return nil, err
	}
	return finalizeChatRequest(converted, opts)
}

// convertNonStreamResponse 把上游非流式响应体（in 格式）转换为下游响应体（out 格式）。
// 仅支持 chat->messages 与 chat->responses；其它组合返回 *UnsupportedConversion。
func (s *convSession) convertNonStreamResponse(in, out ApiFormat, body []byte, model string) ([]byte, error) {
	if in != FormatOpenAIChat || (out != FormatAnthropic && out != FormatResponses) {
		return nil, &UnsupportedConversion{From: in, To: out}
	}
	resp, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	var converted map[string]any
	switch out {
	case FormatAnthropic:
		converted = openAIToAnthropicResponse(resp, model)
	default:
		converted = s.toResponsesWithNS(resp, model)
	}
	return marshalJSON(converted)
}

// convertStreamResponse 读取 in 格式的上游 SSE（r），写出 out 格式的下游 SSE（w）。
// flush 非 nil 时，每写出一个下游事件后调用一次。
// 返回累计观察到的**上游** usage（Anthropic 口径，缓存命中单列，见 Usage）。
//
// 仅支持 chat->messages 与 chat->responses（上游永远是中枢 Chat）。
func (s *convSession) convertStreamResponse(in, out ApiFormat, r io.Reader, w io.Writer, model string, flush func()) (Usage, error) {
	if in != FormatOpenAIChat || (out != FormatAnthropic && out != FormatResponses) {
		return Usage{}, &UnsupportedConversion{From: in, To: out}
	}
	var conv streamConverter
	if out == FormatAnthropic {
		conv = newChatToAnthropicStream(model)
	} else {
		conv = newChatToResponsesStream(model, s.nsReverse)
	}
	writeAll := func(chunks [][]byte) error {
		for _, chunk := range chunks {
			if _, err := w.Write(chunk); err != nil {
				return err
			}
			if flush != nil {
				flush()
			}
		}
		return nil
	}
	err := sse.Scan(r, func(ev sse.Event) error {
		return writeAll(conv.processEvent(ev))
	})
	if err != nil {
		return conv.usage(), err
	}
	if err := writeAll(conv.finalize()); err != nil {
		return conv.usage(), err
	}
	return conv.usage(), nil
}

// fullResponseToSSE 把上游**完整的非 SSE JSON 响应**（in 格式）重放成下游 SSE 事件流（out 格式）。
// 代理在上游对「流式请求」返回了单个 JSON 体时使用它。
//
// in != out 时先走 ConvertNonStreamResponse（受同一矩阵约束）；in == out 时不做转换，
// 仅按该入口格式重放（这不是协议转换，矩阵未被放宽）。
func (s *convSession) fullResponseToSSE(in, out ApiFormat, body []byte, model string) ([]byte, error) {
	resp, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	if in != out {
		converted, err := s.convertNonStreamResponse(in, out, body, model)
		if err != nil {
			return nil, err
		}
		resp, err = decodeObject(converted)
		if err != nil {
			return nil, err
		}
	}
	switch out {
	case FormatResponses:
		return replayResponses(resp), nil
	case FormatAnthropic:
		return replayAnthropic(resp), nil
	default:
		return replayChat(resp), nil
	}
}

// EnsureChatStreamUsage 在「流式」的 Chat 请求体上注入 stream_options.include_usage=true。
// 非流式请求、非对象请求体一律原样返回；既有 stream_options 键会被保留。
// 对应 ocgo main.go::requestStreamingUsage（prepareChatBody 的前半段）。
func EnsureChatStreamUsage(body []byte) ([]byte, error) {
	req, err := decodeObject(body)
	if err != nil {
		// 不是对象（或不是合法 JSON）：保持原样，交给下游报错。
		return body, nil
	}
	if !requestStreamingUsage(req) {
		return body, nil
	}
	out, err := marshalJSON(req)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ValidateResponsesCrossFormat 拒绝无法跨协议保真的 Responses 请求。
// 对应 Rust converter/request.rs::validate_responses_cross_format（line 29）：
// previous_response_id / conversation 无法跨协议续接；
// 只有 encrypted_content 而没有 summary 的 reasoning 项无法搬运。
func ValidateResponsesCrossFormat(body []byte) error {
	req, err := decodeObject(body)
	if err != nil {
		// 非对象请求体：与 Rust 的 Value::get 语义一致，无需拒绝。
		return nil
	}
	return validateResponsesCrossFormat(req)
}

// InjectAnthropicCacheBreakpoints 向 Anthropic 形态的请求体注入 prompt caching 断点。
// 幂等：请求体中已存在任何 cache_control 时不改动。
// 见 Rust converter/request.rs::inject_anthropic_cache_breakpoints（line 1319）：
// 最多 4 个断点（system 末块、tools 末个、最后一条消息末块、消息数≥2 时倒数第二条
// user 消息末块）。
func InjectAnthropicCacheBreakpoints(body []byte) ([]byte, error) {
	req, err := decodeObject(body)
	if err != nil {
		return body, nil
	}
	if !injectAnthropicCacheBreakpoints(req) {
		return body, nil
	}
	out, err := marshalJSON(req)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// finalizeChatRequest 收尾 Chat 请求体：图片能力门禁 + 去掉 image detail，然后序列化。
// 对应 ocgo main.go::prepareChatBody 的尾部（rawChatBodyHasImages / validateImageSupport /
// stripRawChatImageDetails），以及 proxyMessages 里的 validateImageSupport 调用。
func finalizeChatRequest(out map[string]any, opts RequestOptions) ([]byte, error) {
	if rawChatBodyHasImages(out) {
		if !opts.SupportsImages {
			return nil, unsupportedImageModelError(modelOf(out))
		}
		stripRawChatImageDetails(out)
	}
	return marshalJSON(out)
}

// modelOf 取出站请求体里的 model（仅用于错误信息）。
func modelOf(req map[string]any) string {
	model, _ := asString(req["model"])
	return model
}

// pickModel 选择出站模型名：opts.Model 非空时优先，否则保留入站 model。
func pickModel(incoming map[string]any, want string) string {
	if want != "" {
		return want
	}
	model, _ := asString(incoming["model"])
	return model
}
