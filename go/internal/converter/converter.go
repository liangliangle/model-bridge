// Package converter 实现三套协议的互转：任意入口协议 → 任意渠道协议。
//
// 转换以 OpenAI Chat Completions 为唯一中枢（见 plan.go::Hub）：
//
//	同协议       ：字节透传，由代理层负责，不在本包范围内
//	其一为 Chat  ：单跳（请求方向 4 条边 + 响应方向 4 条边，见 direction.go）
//	两个富协议   ：两跳，经 Chat 串联（例如 messages → chat → responses）
//
// 一条路径由 NewPlan 规划，请求方向与响应方向都从它折叠得出，调用点不再自己拼装方向
// （历史上方向写反过一次，见 hop.go 顶部）。矩阵不再是「能不能」的判定，而是「走哪条路」。
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
	"fmt"
	"strings"
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

// UnsupportedConversion 表示协议矩阵不支持的「入口协议 → 渠道协议」组合。
// 对应 Rust converter/mod.rs::UnsupportedConversion。
type UnsupportedConversion struct {
	From ApiFormat
	To   ApiFormat
}

func (e *UnsupportedConversion) Error() string {
	return "unsupported protocol conversion " + e.From.String() + " -> " + e.To.String()
}

// UnsupportedFieldError 表示入口请求里的某个字段在目标协议里**无法表达**。
//
// 与「有损但可用」（记 note 后照常转换）的区别：这里丢弃会改变调用方依赖的语义，
// 例如 Chat 的 n>1 在 Anthropic 上只能得到一条 choice。静默丢会让调用方拿到形状不对的
// 结果，因此直接拒绝，并把字段名与目标协议写进错误信息（代理层会转成 400
// invalid_request，调用方据此能定位到具体字段）。
type UnsupportedFieldError struct {
	Field  string
	Target ApiFormat
	Reason string
}

func (e *UnsupportedFieldError) Error() string {
	return fmt.Sprintf("field %q cannot be honored by a %s channel: %s", e.Field, e.Target, e.Reason)
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

	// notes 记录本次转换里「有损但可用」的处理：被丢弃的字段、被替代的默认值、
	// 被规整的形态等。由 Hop.Notes() 暴露给代理层打日志——有损可以接受，
	// 但必须能定位到是哪一跳、哪个字段（见 plan 的「有损清单」）。
	notes []string
}

// note 记录一条有损处理说明（自动去重，避免逐条消息刷屏）。
func (s *convSession) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, existing := range s.notes {
		if existing == msg {
			return
		}
	}
	s.notes = append(s.notes, msg)
}

// noteEdge 记录一条带方向的有损处理说明，便于在日志里直接看出是哪一跳丢的什么。
func (s *convSession) noteEdge(from, to ApiFormat, format string, args ...any) {
	s.note("[%s→%s] %s", from, to, fmt.Sprintf(format, args...))
}

// newConvSession 创建一个空的转换会话状态。
func newConvSession() *convSession {
	return &convSession{nsReverse: map[string]nsEntry{}}
}

// buildUpstreamRequest 把下游请求体转换为上游请求体：按路径逐跳施加请求方向的映射
// （见 direction.go 的 mapRequest）。
//
// 单跳与两跳走的是同一段代码——两跳只是多折叠一条边，因此不存在「两跳专用路径」。
func (s *convSession) buildUpstreamRequest(plan Plan, body []byte, opts RequestOptions) ([]byte, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	for _, edge := range plan.RequestEdges() {
		obj, err = mapRequest(s, edge.From, edge.To, obj, opts)
		if err != nil {
			return nil, err
		}
	}
	return marshalJSON(obj)
}

// convertNonStreamResponse 把上游非流式响应体转换为下游响应体：
// 按路径**逆序**逐跳施加响应方向的映射（见 direction.go 的 mapResponse）。
func (s *convSession) convertNonStreamResponse(plan Plan, body []byte, model string) ([]byte, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	for _, edge := range plan.ResponseEdges() {
		obj, err = mapResponse(s, edge.From, edge.To, obj, model)
		if err != nil {
			return nil, err
		}
	}
	return marshalJSON(obj)
}

// fullResponseToSSE 把上游**完整的非 SSE JSON 响应**重放成下游 SSE 事件流。
// 代理在上游对「流式请求」返回了单个 JSON 体时使用它。
//
// 先按路径逆序折叠响应方向的映射（同协议时没有边，等价于纯重放），
// 再按**客户端协议**重放成该协议的事件流（见 direction.go 的 replayAs）。
func (s *convSession) fullResponseToSSE(plan Plan, body []byte, model string) ([]byte, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	for _, edge := range plan.ResponseEdges() {
		obj, err = mapResponse(s, edge.From, edge.To, obj, model)
		if err != nil {
			return nil, err
		}
	}
	return replayAs(obj, plan.Client()), nil
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

// finalizeChatBody 收尾出站 Chat 体：图片能力门禁 + 去掉 image detail。
// 对应 ocgo main.go::prepareChatBody 的尾部（rawChatBodyHasImages / validateImageSupport /
// stripRawChatImageDetails），以及 proxyMessages 里的 validateImageSupport 调用。
//
// 不含序列化：路径折叠只需要改对象，序列化由折叠的最后一步统一做。
func finalizeChatBody(out map[string]any, opts RequestOptions) error {
	if rawChatBodyHasImages(out) {
		if !opts.SupportsImages {
			return unsupportedImageModelError(modelOf(out))
		}
		stripRawChatImageDetails(out)
	}
	return nil
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
