package converter

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 本文件实现**请求方向**的转换：Messages → Chat、Responses → Chat，以及两条出站
// 请求共用的收尾（流式 usage、图片闸门、tool 消息清洗、跨格式校验、Anthropic
// prompt caching 断点注入）。
//
// 每个函数上方都标注了对应实现：
//
//   - ocgo cmd/ocgo/main.go：纯映射机制（内容块映射、推理强度折叠、工具消息清洗、
//     tool_result 内容预算、Responses 内建工具、出站图片闸门）。
//   - Rust src-tauri/src/converter/request.rs：契约（namespace / custom 工具展平与
//     反向映射、跨格式校验、cache_control 保留、prompt caching 断点）。
//
// 与 ocgo 的差异（以 Rust 契约为准，见 converter.go 包注释）：
//
//  1. cache_control 全程保留：不移植 ocgo 的 stripCacheControl（~1348），也不调用它；
//  2. 出站 stream / stream_options 由 RequestOptions.Stream 决定（规则 2）；
//  3. namespace / custom 工具展平写满 session.nsReverse（规则 3）；
//  4. RequestOptions.Model 非空时覆盖出站 model（规则 4）。
//
// 未移植的 ocgo 片段及原因（避免死代码）：
//
//   - applyRawChatReasoningEffort / rawChatReasoningEffort 作用在 ocgo 的**原始 Chat
//     透传体**上（就地写回并删除来源键）。本仓库的 Chat 透传由 proxy.ReplaceModelInJSON
//     完成、不进转换器，而转换方向从不把来源键搬进出站体，因此折叠结果一致，
//     由 downstreamReasoningEffort 承担（见该函数注释）；
//   - validateImageSupport / requestHasImages 与 finalizeChatRequest 的图片闸门重复
//     （converter.go::finalizeChatRequest 已覆盖）；
//   - reasoningEffortFromRaw 的职责被 downstreamReasoningEffort(values ...any) 吸收
//     （map 形态下无需先解 RawMessage）；
//   - anthropicImageURL 改用 common.go::anthropicImageToOpenAI（同一实现，Rust 侧同名）。

const (
	// defaultAnthropicMaxTokens 对应 Rust request.rs::DEFAULT_ANTHROPIC_MAX_TOKENS。
	defaultAnthropicMaxTokens = 4096
	// maxAnthropicToolResultContentChars 对应 ocgo main.go::maxAnthropicToolResultContentChars。
	maxAnthropicToolResultContentChars = 120000
	// unavailableToolResultContent 对应 ocgo main.go::unavailableToolResultContent。
	unavailableToolResultContent = "Tool result unavailable."
)

// errNilSession 表示在 nil session 上做 Responses 转换（需要 nsReverse 状态）。
var errNilSession = errors.New("converter: responsesToChat requires a session")

// ==================== Messages 请求 → Chat 请求 ====================

// convertRequest 把 Messages（Anthropic）请求转换为 Chat 请求。
//
// 移植自 ocgo main.go::convertRequest（~1586）；system / content 归一化沿用
// ocgo normalizeAnthropicSystem（~1223）、normalizeAnthropicContent（~1238）与
// ensureAnthropicRequestDefaults（~1370），消息体映射参考 Rust
// request.rs::anthropic_to_openai（line 398）。
//
// 与 ocgo 的差异：
//   - cache_control 原样保留（规则 1）；
//   - system 为块数组且带 cache_control 时用 content part 数组承载
//     （Rust request.rs::anthropic_system_to_chat_content，line 473）；
//   - 出站 stream / stream_options 由 opts.Stream 决定（规则 2，ocgo 用 ar.Stream）；
//   - thinking 块落成 reasoning_content 而不是丢弃（Rust anthropic_message_to_chat）。
func convertRequest(req map[string]any, opts RequestOptions) (map[string]any, error) {
	if req == nil {
		return nil, errNotObject
	}
	// 浅拷贝后补默认值：normalize 系列只重建嵌套值，不改写 req 的既有内容，
	// 因此不会就地修改入参（ocgo 的 handler 改的是自己解码出的结构体）。
	normalized := cloneMap(req)
	ensureAnthropicRequestDefaults(normalized)

	out := map[string]any{"model": pickModel(req, opts.Model)}

	messages := make([]any, 0, 1)
	if content := anthropicSystemToChatContent(normalized["system"]); content != nil {
		messages = append(messages, map[string]any{"role": "system", "content": content})
	}
	if blocks, ok := asArray(normalized["messages"]); ok {
		for _, block := range blocks {
			msg, ok := asMap(block)
			if !ok {
				continue
			}
			messages = append(messages, contentToOpenAI(msg)...)
		}
	}
	out["messages"] = messages

	// max_tokens → max_tokens（规则 5；ensureAnthropicRequestDefaults 已补默认值）。
	if mt, ok := asInt(normalized["max_tokens"]); ok {
		out["max_tokens"] = mt
	}

	// 同名透传（Rust anthropic_to_openai 的 temperature / top_p / top_k / metadata）。
	for _, key := range []string{"temperature", "top_p", "top_k", "metadata"} {
		if v, ok := normalized[key]; ok && v != nil {
			out[key] = v
		}
	}

	// stop_sequences → stop（规则 5）。
	if stop, ok := normalized["stop_sequences"]; ok && stop != nil {
		out["stop"] = stop
	}

	// tools：Anthropic tool（name / description / input_schema）→ Chat function tool。
	if tools, ok := asArray(normalized["tools"]); ok {
		converted := make([]any, 0, len(tools))
		for _, tool := range tools {
			if ct := anthropicToolToChat(tool); ct != nil {
				converted = append(converted, ct)
			}
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}

	if tc, ok := normalized["tool_choice"]; ok && tc != nil {
		out["tool_choice"] = anthropicToolChoiceToChat(tc)
	}

	// 推理强度折叠：thinking / reasoning / output_config / ... → 单个 reasoning_effort（规则 8）。
	if effort := downstreamReasoningEffort(
		normalized["reasoning"], normalized["thinking"], normalized["output_config"],
		normalized["reasoning_effort"], normalized["effort"], normalized["level"], normalized["depth"],
	); effort != "" {
		out["reasoning_effort"] = effort
	}

	prepareOutgoingChat(normalized, out, opts)
	return out, nil
}

// anthropicSystemToChatContent 对应 Rust request.rs::anthropic_system_to_chat_content（line 473）：
// system 无 cache_control 时退化为纯字符串，有则用 content part 数组承载断点；
// 无可用文本时返回 nil（不产出 system 消息）。
func anthropicSystemToChatContent(system any) any {
	blocks := normalizeAnthropicSystem(system)
	if len(blocks) == 0 {
		return nil
	}
	hasCacheControl := false
	for _, block := range blocks {
		if m, ok := asMap(block); ok {
			if _, ok := m["cache_control"]; ok {
				hasCacheControl = true
				break
			}
		}
	}
	if !hasCacheControl {
		// 与 ocgo normalizeAnthropicSystem 一致：退化成 systemText 的拼接结果。
		text := systemText(blocks)
		if text == "" {
			return nil
		}
		return text
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		m, _ := asMap(block)
		text, _ := asString(m["text"])
		part := map[string]any{"type": "text", "text": text}
		attachCacheControl(part, m)
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// contentToOpenAI 移植自 ocgo main.go::contentToOpenAI（~2219），并吸收 Rust
// request.rs::anthropic_message_to_chat（line 508）：一条 Anthropic 消息的 content
// 块 → 若干 Chat 消息（tool_use → assistant.tool_calls，tool_result → tool 消息，
// text / image → content 部分，thinking → reasoning_content）。
//
// 与 ocgo 的差异：text / image_url 部分带上的 cache_control 会一并透传（规则 1）；
// thinking 块不再被丢弃。
func contentToOpenAI(msg map[string]any) []any {
	role, _ := asString(msg["role"])
	if role == "" {
		role = "user"
	}
	content := normalizeAnthropicContent(msg["content"])
	if s, ok := asString(content); ok {
		return []any{map[string]any{"role": role, "content": s}}
	}
	blocks, ok := asArray(content)
	if !ok {
		// 非字符串也非块数组（正常请求不会出现）：按 ocgo 的做法原样承载 JSON 文本。
		return []any{map[string]any{"role": role, "content": jsonString(content)}}
	}

	var text strings.Builder
	parts := make([]any, 0, len(blocks))
	hasImage := false
	calls := make([]any, 0, len(blocks))
	callIDs := make([]string, 0, len(blocks))
	toolMessages := make([]any, 0, len(blocks))
	var reasoning strings.Builder

	for _, b := range blocks {
		block, ok := asMap(b)
		if !ok {
			continue
		}
		typ, _ := asString(block["type"])
		switch typ {
		case "text":
			t, _ := asString(block["text"])
			text.WriteString(t)
			if t != "" {
				part := map[string]any{"type": "text", "text": t}
				attachCacheControl(part, block)
				parts = append(parts, part)
			}
		case "image":
			source, ok := asMap(block["source"])
			if !ok {
				continue
			}
			// anthropicImageURL（ocgo ~2289）的空值守卫：既无 url 也无 data 时跳过该块。
			url, _ := asString(source["url"])
			data, _ := asString(source["data"])
			if url == "" && data == "" {
				continue
			}
			hasImage = true
			part := anthropicImageToOpenAI(source)
			attachCacheControl(part, block)
			parts = append(parts, part)
		case "tool_use":
			id, _ := asString(block["id"])
			name, _ := asString(block["name"])
			args := "{}"
			if input, ok := block["input"]; ok && input != nil {
				args = jsonString(input)
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			})
			callIDs = append(callIDs, id)
		case "tool_result":
			id, _ := asString(block["tool_use_id"])
			toolMessages = append(toolMessages, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      blockText(block["content"]),
			})
		case "thinking":
			if t, ok := asString(block["thinking"]); ok {
				reasoning.WriteString(t)
			}
		}
	}

	if len(calls) > 0 {
		out := assistantToolCallsMessage(calls, callIDs)
		out["content"] = openAIContentValue(text.String(), parts, hasImage)
		if reasoning.Len() > 0 {
			out["reasoning_content"] = reasoning.String()
		}
		return []any{out}
	}
	if len(toolMessages) > 0 {
		out := make([]any, 0, len(toolMessages)+1)
		out = append(out, toolMessages...)
		// Anthropic 允许同一条消息里既有 tool_result 又有正文（例如用户追问）；
		// ocgo 为保住这段正文追加一条同 role 消息，否则模型会重复回答上一个工具结果。
		if userText := strings.TrimSpace(text.String()); userText != "" {
			out = append(out, map[string]any{"role": role, "content": userText})
		}
		return out
	}
	out := map[string]any{"role": role, "content": openAIContentValue(text.String(), parts, hasImage)}
	if reasoning.Len() > 0 {
		out["reasoning_content"] = reasoning.String()
	}
	return []any{out}
}

// openAIContentValue 移植自 ocgo main.go::openAIContentValue（~2282）：有图片时用
// content part 数组，否则用纯文本。
//
// 与 ocgo 的差异（规则 1）：只要任一 text 部分带 cache_control，就保留数组形态。
// ocgo 无条件退化成纯文本，会把 text 块上的缓存断点丢掉；Rust
// request.rs::anthropic_message_to_chat（line 594）的 content_val 分支同样只在
// 「单个无 cache_control 的 text 部分」时才退化成字符串。
func openAIContentValue(text string, parts []any, hasImage bool) any {
	if hasImage || len(parts) == 0 {
		if hasImage {
			return parts
		}
		return text
	}
	for _, part := range parts {
		m, ok := asMap(part)
		if !ok {
			return parts
		}
		if typ, _ := asString(m["type"]); typ != "text" {
			return parts
		}
		if _, ok := m["cache_control"]; ok {
			return parts
		}
	}
	return text
}

// assistantToolCallsMessage 移植自 ocgo main.go::assistantToolCallsMessage（~1925）：
// assistant 的 tool_call 消息；reasoning_content 取自 ocgo 的 tool_call id 回显缓存
// （common.go::cachedReasoningContent，规则 10）。没有 cache 时该助手会补一个最小
// 占位，避免 Moonshot/Kimi 之类在开启 thinking 后拒绝缺少 reasoning_content 的历史。
func assistantToolCallsMessage(calls []any, callIDs []string) map[string]any {
	out := map[string]any{
		"role":       "assistant",
		"content":    nil,
		"tool_calls": calls,
	}
	if reasoning := cachedReasoningContent(callIDs); reasoning != "" {
		out["reasoning_content"] = reasoning
	}
	return out
}

// anthropicToolToChat 移植自 Rust request.rs::anthropic_tool_to_chat（line 615）与
// ocgo main.go::convertRequest（~1586）的 tools 映射：name / description / input_schema
// → Chat function 工具，工具对象上的 cache_control 一并带上（规则 1）。
func anthropicToolToChat(tool any) map[string]any {
	m, ok := asMap(tool)
	if !ok {
		return nil
	}
	name, _ := asString(m["name"])
	if strings.TrimSpace(name) == "" {
		return nil
	}
	fn := map[string]any{"name": name}
	if desc, ok := m["description"]; ok && desc != nil {
		fn["description"] = desc
	}
	fn["parameters"] = toolParametersOrDefault(m["input_schema"])
	out := map[string]any{"type": "function", "function": fn}
	attachCacheControl(out, m)
	return out
}

// anthropicToolChoiceToChat 对应 Rust request.rs::anthropic_tool_choice_to_chat（line 633）：
// Anthropic tool_choice → Chat tool_choice。
func anthropicToolChoiceToChat(tc any) any {
	m, ok := asMap(tc)
	if !ok {
		if s, ok := asString(tc); ok {
			return s
		}
		return "auto"
	}
	switch t, _ := asString(m["type"]); t {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name, _ := asString(m["name"])
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	default:
		return "auto"
	}
}

// toolParametersOrDefault 移植自 ocgo main.go::toolParametersOrDefault（~1643）：
// 缺失 / null 的工具 schema 兜底为 {"type":"object","properties":{}}。
func toolParametersOrDefault(schema any) any {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return schema
}

// attachCacheControl 对应 Rust request.rs::attach_cache_control（line 874）：
// 源对象带 cache_control 时透传到目标对象上（规则 1）。
func attachCacheControl(target, source map[string]any) {
	if source == nil {
		return
	}
	if cc, ok := source["cache_control"]; ok {
		target["cache_control"] = cc
	}
}

// ensureAnthropicRequestDefaults 移植自 ocgo main.go::ensureAnthropicRequestDefaults（~1370）：
// max_tokens 缺失时兜底 4096。ocgo 在这里还会做模型映射（resolveToolModel），本仓库没有
// 模型映射表，出站模型由 RequestOptions.Model 决定（规则 4）。
//
// 注意：ocgo 只在 `ar.MaxTokens == 0` 时兜底，因此显式的 0 也会被抬到 4096，这里
// 只在字段缺失时兜底，行为对正常请求一致。
func ensureAnthropicRequestDefaults(req map[string]any) {
	if _, ok := asInt(req["max_tokens"]); !ok {
		req["max_tokens"] = defaultAnthropicMaxTokens
	}
}

// ==================== Anthropic system / content 归一化 ====================
//
// 对应 ocgo main.go::normalizeAnthropicSystem（~1223）与 normalizeAnthropicContent（~1238），
// 这两者在 ocgo 里是「转发给 Anthropic 上游前」的归一化；本仓库的转换方向同样需要它们
// 的机制（已知块类型过滤 + tool_result 内容预算）。
//
// 与 ocgo 的差异：ocgo 的 rawJSONAny（~1337）会调用 stripCacheControl，规则 1 要求保留
// cache_control，因此这里的 rawJSONAny 不再剥离，块内的 cache_control 由调用点显式搬运。

// normalizeAnthropicSystem 移植自 ocgo main.go::normalizeAnthropicSystem（~1223）：
// 把 system（字符串或块数组）归一化成文本块数组。ocgo 会进一步压成纯字符串（因而丢掉
// cache_control），这里保留块数组以便 anthropicSystemToChatContent 保住断点（规则 1）。
// 无可用文本时返回 nil。
func normalizeAnthropicSystem(system any) []any {
	if system == nil {
		return nil
	}
	if s, ok := asString(system); ok {
		if s == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": s}}
	}
	blocks, ok := asArray(system)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		block, ok := asMap(b)
		if !ok {
			continue
		}
		if typ, _ := asString(block["type"]); typ != "text" {
			continue
		}
		normalizedBlock := map[string]any{"type": "text"}
		copyRawJSONField(normalizedBlock, block, "text")
		copyRawJSONField(normalizedBlock, block, "cache_control")
		out = append(out, normalizedBlock)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeAnthropicContent 移植自 ocgo main.go::normalizeAnthropicContent（~1238）：
// 把 content 归一化成「字符串 + 已知块类型数组」两种形态，并对 tool_result 的内容做
// 预算截断（truncateToolResultContent）。
//
// 与 ocgo 的差异：块内除 type 外的字段按 ocgo 的 copyRawJSONField 逐个搬运，并额外搬运
// cache_control（规则 1）；thinking 块同样归一化（Rust 会保留它，ocgo 会丢弃）。
// 没有已知块时返回空字符串，与 ocgo 的 `marshalJSON("")` 一致。
func normalizeAnthropicContent(content any) any {
	if content == nil {
		return ""
	}
	if s, ok := asString(content); ok {
		return s
	}
	blocks, ok := asArray(content)
	if !ok {
		return content
	}
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		block, ok := asMap(b)
		if !ok {
			continue
		}
		typ, _ := asString(block["type"])
		switch typ {
		case "text":
			normalized := map[string]any{"type": "text"}
			copyRawJSONField(normalized, block, "text")
			copyRawJSONField(normalized, block, "cache_control")
			out = append(out, normalized)
		case "image":
			normalized := map[string]any{"type": "image"}
			copyRawJSONField(normalized, block, "source")
			copyRawJSONField(normalized, block, "cache_control")
			out = append(out, normalized)
		case "tool_use":
			normalized := map[string]any{"type": "tool_use"}
			copyRawJSONField(normalized, block, "id")
			copyRawJSONField(normalized, block, "name")
			copyRawJSONField(normalized, block, "input")
			out = append(out, normalized)
		case "tool_result":
			normalized := map[string]any{"type": "tool_result"}
			copyRawJSONField(normalized, block, "tool_use_id")
			copyAnthropicToolResultContent(normalized, block)
			copyRawJSONField(normalized, block, "is_error")
			copyRawJSONField(normalized, block, "cache_control")
			out = append(out, normalized)
		case "thinking":
			normalized := map[string]any{"type": "thinking"}
			copyRawJSONField(normalized, block, "thinking")
			copyRawJSONField(normalized, block, "signature")
			copyRawJSONField(normalized, block, "cache_control")
			out = append(out, normalized)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return out
}

// copyAnthropicToolResultContent 移植自 ocgo main.go::copyAnthropicToolResultContent（~1285）：
// 复制 tool_result.content，并做内容预算截断。
func copyAnthropicToolResultContent(dst, src map[string]any) {
	if v, ok := rawJSONAny(src["content"]); ok {
		dst["content"] = truncateToolResultContent(v)
	}
}

// truncateToolResultContent 移植自 ocgo main.go::truncateToolResultContent（~1291）。
func truncateToolResultContent(v any) any {
	remaining := maxAnthropicToolResultContentChars
	return truncateToolResultContentValue(v, &remaining)
}

// truncateToolResultContentValue 移植自 ocgo main.go::truncateToolResultContentValue（~1296）。
func truncateToolResultContentValue(v any, remaining *int) any {
	switch x := v.(type) {
	case string:
		return truncateStringToBudget(x, remaining)
	case []any:
		out := make([]any, 0, len(x))
		for _, item := range x {
			out = append(out, truncateToolResultContentValue(item, remaining))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = truncateToolResultContentValue(item, remaining)
		}
		return out
	default:
		return v
	}
}

// truncateStringToBudget 移植自 ocgo main.go::truncateStringToBudget（~1317）：
// 按剩余预算按 rune 截断，并附上省略提示（提示文案与 ocgo 完全一致，便于对照）。
func truncateStringToBudget(s string, remaining *int) string {
	if *remaining <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= *remaining {
		*remaining -= len(runes)
		return s
	}
	kept := *remaining
	*remaining = 0
	return string(runes[:kept]) + fmt.Sprintf("\n\n[ocgo truncated tool_result content: omitted %d characters]", len(runes)-kept)
}

// copyRawJSONField 移植自 ocgo main.go::copyRawJSONField（~1331）：
// 源对象存在且非 null 时把该字段搬到目标对象上。
func copyRawJSONField(dst, src map[string]any, key string) {
	if v, ok := rawJSONAny(src[key]); ok {
		dst[key] = v
	}
}

// rawJSONAny 移植自 ocgo main.go::rawJSONAny（~1337）。
// 与 ocgo 的差异：ocgo 在这里调用 stripCacheControl（~1348），本端口按规则 1 原样返回，
// 因此 cache_control 不会被剥离。
func rawJSONAny(v any) (any, bool) {
	if v == nil {
		return nil, false
	}
	return v, true
}

// systemText 移植自 ocgo main.go::systemText（~2318）：system 字符串或块数组 → 纯文本。
func systemText(system any) string {
	if s, ok := asString(system); ok {
		return s
	}
	return blockText(system)
}

// blockText 移植自 ocgo main.go::blockText（~2329）：块数组 → 各 text 字段拼接；
// 非字符串非数组时退化为 JSON 文本（ocgo 的 `string(raw)`）。
func blockText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, item := range x {
			m, ok := asMap(item)
			if !ok {
				continue
			}
			if t, ok := asString(m["text"]); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	default:
		return jsonString(v)
	}
}

// ==================== Responses 请求 → Chat 请求 ====================

// responsesToChat 移植自 ocgo main.go::responsesToChat（~1603），并对齐 Rust
// request.rs::responses_to_openai_with_ns（line 53）与 collect_responses_tools（line 170）：
// instructions → system 消息；input 项映射为 Chat 消息；tools 展平（function / custom /
// namespace / web_search / mcp / 其它内建）并写满 s.nsReverse（规则 3）。
//
// 与 ocgo 的差异：内建工具不再落成 Anthropic 形态（responseBuiltinToolToAnthropic），
// 而是按规则 6 落成 Chat function 工具；reasoning 项保留成 reasoning_content。
func (s *convSession) responsesToChat(req map[string]any, opts RequestOptions) (map[string]any, error) {
	if req == nil {
		return nil, errNotObject
	}
	if s == nil {
		return nil, errNilSession
	}
	if s.nsReverse == nil {
		// 零值 session 也要能用：否则 registerCustomTool/flattenNamespaceSubtool 会写 nil map。
		s.nsReverse = map[string]nsEntry{}
	}

	out := map[string]any{"model": pickModel(req, opts.Model)}

	messages := make([]any, 0, 1)
	if instructions, ok := asString(req["instructions"]); ok && instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	messages = append(messages, responsesInputToMessages(req["input"])...)
	out["messages"] = messages

	// max_output_tokens → max_tokens（Rust responses_to_openai_with_ns）。
	if mt, ok := asInt(req["max_output_tokens"]); ok {
		out["max_tokens"] = mt
	}

	if tools := collectResponsesTools(req); len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, t := range tools {
			tool, ok := asMap(t)
			if !ok {
				continue
			}
			ttype, _ := asString(tool["type"])
			if ttype == "" {
				ttype = "function"
			}
			switch ttype {
			case "function":
				if ct := responsesToolToChat(tool); ct != nil {
					converted = append(converted, ct)
				}
			case "custom":
				ct := responsesCustomToolToChat(tool)
				if ct == nil {
					continue
				}
				name, _ := asString(tool["name"])
				// 规则 3：custom 工具在 nsReverse 的 namespace 位写 CUSTOM_TOOL_NAMESPACE_MARKER，
				// 响应方向据此把 function 调用还原成 custom_tool_call。
				registerCustomTool(name, s.nsReverse)
				converted = append(converted, ct)
			case "namespace":
				nsName, _ := asString(tool["name"])
				subtools, _ := asArray(tool["tools"])
				for _, sub := range subtools {
					subtool, ok := asMap(sub)
					if !ok {
						continue
					}
					if flat, ok := flattenNamespaceSubtool(nsName, subtool, s.nsReverse); ok {
						converted = append(converted, flat)
					}
				}
			case "web_search", "web_search_preview":
				converted = append(converted, map[string]any{"type": "web_search"})
			case "mcp":
				converted = append(converted, cloneValue(tool))
			default:
				if ct := responseBuiltinToolToChat(tool); ct != nil {
					converted = append(converted, ct)
				}
			}
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}

	if tc, ok := req["tool_choice"]; ok && tc != nil {
		out["tool_choice"] = normalizeToolChoiceToChat(tc)
	}

	// text.format → response_format（Rust text_format_to_response_format）。
	if text, ok := asMap(req["text"]); ok {
		if rf := textFormatToResponseFormat(text["format"]); rf != nil {
			out["response_format"] = rf
		}
	}

	// 推理强度折叠（规则 8）：Responses 的 reasoning.effort / output_config / thinking / ...
	if effort := downstreamReasoningEffort(
		req["reasoning"], req["thinking"], req["output_config"],
		req["reasoning_effort"], req["effort"], req["level"], req["depth"],
	); effort != "" {
		out["reasoning_effort"] = effort
	}

	// 请求级字段透传（Rust responses_to_openai_with_ns）。
	for _, key := range []string{"temperature", "top_p", "user", "metadata", "store", "service_tier"} {
		if v, ok := req[key]; ok && v != nil {
			out[key] = v
		}
	}

	prepareOutgoingChat(req, out, opts)
	return out, nil
}

// collectResponsesTools 移植自 Rust request.rs::collect_responses_tools（line 170）：
// Responses 的工具既可以声明在顶层 tools，也可以放在 input 里的 additional_tools 项
// （Codex 的控制工具走后者），两处都要收。
func collectResponsesTools(req map[string]any) []any {
	tools := make([]any, 0, 4)
	if top, ok := asArray(req["tools"]); ok {
		tools = append(tools, top...)
	}
	if items, ok := asArray(req["input"]); ok {
		for _, item := range items {
			m, ok := asMap(item)
			if !ok {
				continue
			}
			if typ, _ := asString(m["type"]); typ != "additional_tools" {
				continue
			}
			if additional, ok := asArray(m["tools"]); ok {
				tools = append(tools, additional...)
			}
		}
	}
	return tools
}

// responsesToolToChat 对应 Rust request.rs::responses_tool_to_chat（line 343）：
// Responses function 工具 → Chat function 工具。
func responsesToolToChat(tool map[string]any) map[string]any {
	name, _ := asString(tool["name"])
	if name == "" {
		return nil
	}
	fn := map[string]any{"name": name}
	if desc, ok := tool["description"]; ok && desc != nil {
		fn["description"] = desc
	}
	fn["parameters"] = toolParametersOrDefault(tool["parameters"])
	return map[string]any{"type": "function", "function": fn}
}

// responsesCustomToolToChat 对应 Rust request.rs::responses_custom_tool_to_chat（line 360）：
// custom（自由文本 / 语法）工具在 Chat 侧承载为只带一个 content 字符串参数的 function
// 工具（LiteLLM 的 Responses 适配器同形），响应方向再按注册的标记还原成 custom_tool_call。
func responsesCustomToolToChat(tool map[string]any) map[string]any {
	name, _ := asString(tool["name"])
	if name == "" {
		return nil
	}
	description, _ := asString(tool["description"])
	if format, ok := asMap(tool["format"]); ok {
		syntax, _ := asString(format["syntax"])
		definition, _ := asString(format["definition"])
		if definition != "" {
			description += "\n\nFormat:\n```" + syntax + "\n" + definition + "\n```"
		}
	}
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The " + name + " content following the specified format",
			},
		},
		"required": []any{"content"},
	}
	fn := map[string]any{"name": name, "parameters": parameters}
	if description != "" {
		fn["description"] = description
	}
	return map[string]any{"type": "function", "function": fn}
}

// responseBuiltinToolToChat 对应 ocgo main.go::responseBuiltinToolToAnthropic（~1622）的
// 内建工具识别，按规则 6 落成 Chat function 工具：file_search / computer / code_interpreter /
// web_fetch 等在 Chat 侧没有等价类型，转成只带名字、描述与声明 schema 的 function 工具，
// 避免「声明的工具被静默丢弃」（Rust 测试 test_real_codex_request_preserves_tools 的口径）。
//
// web_search / web_search_preview 不在本函数里：与 Rust responses_to_openai_with_ns 一致
// 保留 {"type":"web_search"}；mcp 原样透传；未知类型仍按 Rust 的做法丢弃。
func responseBuiltinToolToChat(tool map[string]any) map[string]any {
	ttype, _ := asString(tool["type"])
	def, ok := builtinToolDefaults[ttype]
	if !ok {
		return nil
	}
	name, _ := asString(tool["name"])
	if name == "" {
		name = def
	}
	fn := map[string]any{"name": name, "parameters": toolParametersOrDefault(tool["parameters"])}
	if desc, ok := tool["description"]; ok && desc != nil {
		fn["description"] = desc
	}
	return map[string]any{"type": "function", "function": fn}
}

// builtinToolDefaults 是 Responses 内建工具类型 → Chat 侧 function 工具名。
// web_search / web_search_preview / mcp 由 responsesToChat 单独处理，不在此表内。
var builtinToolDefaults = map[string]string{
	"web_fetch":             "web_fetch",
	"web_extractor":         "web_fetch",
	"file_search":           "file_search",
	"computer":              "computer",
	"computer_use":          "computer_use",
	"computer_use_preview":  "computer_use",
	"code_interpreter":      "code_interpreter",
	"image_generation":      "image_generation",
	"local_shell":           "local_shell",
	"apply_patch":           "apply_patch",
	"container":             "container",
	"tool_search":           "tool_search",
	"chatkit_tool":          "chatkit_tool",
	"image_edit":            "image_edit",
	"audio_transcription":   "audio_transcription",
	"audio_speech":          "audio_speech",
	"computer_use_20250124": "computer_use",
}

// normalizeToolChoiceToChat 对应 Rust request.rs::normalize_tool_choice_to_chat（line 1173）：
// Responses tool_choice → Chat tool_choice。
func normalizeToolChoiceToChat(tc any) any {
	if s, ok := asString(tc); ok {
		return s
	}
	m, ok := asMap(tc)
	if !ok {
		return "auto"
	}
	switch t, _ := asString(m["type"]); t {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "required", "any":
		return "required"
	case "function":
		name, _ := asString(m["name"])
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	default:
		return tc
	}
}

// textFormatToResponseFormat 对应 Rust request.rs::text_format_to_response_format（line 1212）：
// Responses text.format → Chat response_format。
func textFormatToResponseFormat(format any) map[string]any {
	m, ok := asMap(format)
	if !ok {
		return nil
	}
	switch t, _ := asString(m["type"]); t {
	case "json_schema":
		name, _ := asString(m["name"])
		if name == "" {
			name = "response_schema"
		}
		schema := m["schema"]
		if schema == nil {
			schema = map[string]any{}
		}
		strict, _ := asBool(m["strict"])
		return map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   name,
				"schema": schema,
				"strict": strict,
			},
		}
	case "json_object":
		return map[string]any{"type": "json_object"}
	default:
		return nil
	}
}

// ==================== Responses 输入项 → Chat 消息 ====================

// responsesInputToMessages 移植自 ocgo main.go::responsesInputToMessages（~1872），并吸收
// Rust request.rs::responses_input_items_to_messages（line 190）的 reasoning /
// custom_tool_call / agent_message 处理。
//
// 与 ocgo 的差异：
//   - function_call / custom_tool_call 直接并入上一条 assistant 消息（Rust 的相邻合并），
//     而不是攒到 function_call_output 再一次性吐出，这样 assistant 的正文、reasoning 与
//     tool_calls 落在同一条消息里，天然满足 Chat 的 tool 消息邻接约束（规则 9）；
//   - reasoning 项保留成 reasoning_content，而不是被丢弃（规则 6）。
func responsesInputToMessages(input any) []any {
	if s, ok := asString(input); ok {
		return []any{map[string]any{"role": "user", "content": s}}
	}
	items, ok := asArray(input)
	if !ok {
		if input == nil {
			return nil
		}
		return []any{map[string]any{"role": "user", "content": jsonString(input)}}
	}

	out := make([]any, 0, len(items))
	for _, item := range items {
		it, ok := asMap(item)
		if !ok {
			continue
		}
		itype, _ := asString(it["type"])
		switch itype {
		case "message", "":
			role, _ := asString(it["role"])
			if role == "developer" {
				role = "system"
			}
			if role == "" {
				role = "user"
			}
			out = append(out, map[string]any{"role": role, "content": responsesContent(it["content"])})
		case "reasoning":
			text := reasoningItemText(it)
			if text == "" {
				continue
			}
			if last, ok := lastAssistantMessage(out); ok {
				last["reasoning_content"] = text
				continue
			}
			out = append(out, map[string]any{
				"role":              "assistant",
				"content":           nil,
				"reasoning_content": text,
			})
		case "function_call", "custom_tool_call":
			callID, _ := asString(it["call_id"])
			if callID == "" {
				callID, _ = asString(it["id"])
			}
			name, _ := asString(it["name"])
			args := responseFunctionArguments(it, itype)
			call := map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}
			if last, ok := lastAssistantMessage(out); ok {
				// 相邻的多次 function_call 合并进同一条 assistant 消息（Rust 行为）。
				if existing, ok := asArray(last["tool_calls"]); ok {
					last["tool_calls"] = append(existing, call)
				} else {
					last["tool_calls"] = []any{call}
				}
				continue
			}
			out = append(out, assistantToolCallsMessage([]any{call}, []string{callID}))
		case "function_call_output", "custom_tool_call_output":
			callID, _ := asString(it["call_id"])
			output, ok := it["output"]
			if !ok || output == nil {
				output = it["content"]
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      responsesContentText(output),
			})
		case "agent_message":
			content, ok := it["content"]
			if !ok || content == nil {
				content = it["text"]
			}
			if content == nil {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": extractText(content)})
		default:
			// 其它 item（additional_tools、web_search_call 等）不是消息，按 ocgo 忽略。
		}
	}
	return out
}

// reasoningItemText 对应 Rust request.rs::responses_input_items_to_messages 的 reasoning 分支：
// content / summary 里各块的 text 拼接（encrypted_content 与 provider 绑定，不搬运）。
func reasoningItemText(item map[string]any) string {
	var b strings.Builder
	for _, key := range []string{"content", "summary"} {
		parts, ok := asArray(item[key])
		if !ok {
			continue
		}
		for _, part := range parts {
			m, ok := asMap(part)
			if !ok {
				continue
			}
			if t, ok := asString(m["text"]); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// responseFunctionArguments 对应 Rust responses_input_items_to_messages 的 arguments 处理：
// custom_tool_call 用 {"content": <input>} 外壳承载（unwrapCustomToolArguments 的逆操作），
// function_call 直接取 arguments / input。
func responseFunctionArguments(item map[string]any, itype string) string {
	value, ok := item[stringKeyInput]
	if !ok || value == nil {
		value = item["arguments"]
	}
	text := ""
	switch v := value.(type) {
	case nil:
	case string:
		text = v
	default:
		text = jsonString(v)
	}
	if itype == "custom_tool_call" {
		return jsonString(map[string]any{"content": text})
	}
	if value == nil {
		return "{}"
	}
	return text
}

// stringKeyInput 是 item 里承载函数入参的键名（function_call / custom_tool_call 通用）。
const stringKeyInput = "input"

// lastAssistantMessage 取消息列表末尾的 assistant 消息（Rust 的「相邻合并」判断）。
func lastAssistantMessage(messages []any) (map[string]any, bool) {
	if len(messages) == 0 {
		return nil, false
	}
	msg, ok := asMap(messages[len(messages)-1])
	if !ok {
		return nil, false
	}
	if role, _ := asString(msg["role"]); role != "assistant" {
		return nil, false
	}
	return msg, true
}

// responsesContent 移植自 ocgo main.go::responsesContent（~2135）：
// Responses 的 content（字符串或内容块数组）→ Chat content（纯文本或 content part 数组）。
// 与 Rust responses_content_parts_to_chat（line 302）一致地额外处理 refusal /
// input_file / input_audio（后者退化成文本占位，避免静默丢用户输入）。
func responsesContent(content any) any {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	parts, ok := asArray(content)
	if !ok {
		return jsonString(content)
	}
	var text strings.Builder
	out := make([]any, 0, len(parts))
	hasImage := false
	for _, part := range parts {
		m, ok := asMap(part)
		if !ok {
			continue
		}
		switch typ, _ := asString(m["type"]); typ {
		case "input_text", "output_text", "text":
			for _, key := range []string{"text", "output_text"} {
				if t, ok := asString(m[key]); ok {
					text.WriteString(t)
					out = append(out, map[string]any{"type": "text", "text": t})
					break
				}
			}
		case "refusal":
			if t, ok := asString(m["refusal"]); ok {
				text.WriteString(t)
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		case "input_image", "image_url":
			if url := responsesImageURL(m); url != "" {
				hasImage = true
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "input_file", "input_audio":
			placeholder := jsonString(part)
			text.WriteString(placeholder)
			out = append(out, map[string]any{"type": "text", "text": placeholder})
		}
	}
	if hasImage {
		return out
	}
	if len(out) == 0 {
		return jsonString(content)
	}
	return text.String()
}

// responsesImageURL 移植自 ocgo main.go::responsesImageURL（~2176）：
// input_image / image_url 部分 → URL（image_url 可以是字符串或 {url} 对象）。
func responsesImageURL(part map[string]any) string {
	url := ""
	if s, ok := asString(part["image_url"]); ok {
		url = s
	}
	if url == "" {
		if m, ok := asMap(part["image_url"]); ok {
			url, _ = asString(m["url"])
		}
	}
	if url == "" {
		url, _ = asString(part["url"])
	}
	return url
}

// responsesContentText 移植自 ocgo main.go::responsesContentText（~2195）：
// function_call_output 的 output → 纯文本。
func responsesContentText(output any) string {
	switch v := output.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	parts, ok := asArray(output)
	if !ok {
		return jsonString(output)
	}
	var b strings.Builder
	for _, part := range parts {
		m, ok := asMap(part)
		if !ok {
			continue
		}
		for _, key := range []string{"text", "output_text"} {
			if t, ok := asString(m[key]); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// ==================== 出站请求收尾 ====================

// prepareOutgoingChat 对应 ocgo main.go::prepareChatBody（~1385）在转换路径上需要做的收尾：
// 流式 usage（requestStreamingUsage）、tool 消息清洗（sanitizeRawChatToolMessages）。
// ocgo 的模型映射、图片闸门与序列化分别由 RequestOptions.Model（规则 4）与门面的
// finalizeChatRequest（converter.go）承担。
func prepareOutgoingChat(in, out map[string]any, opts RequestOptions) {
	// 入口请求里若有 stream_options（部分客户端会带），连同兄弟键一起保留（规则 2）。
	if so, ok := in["stream_options"]; ok && so != nil {
		out["stream_options"] = so
	}
	stripNulls(out)
	applyChatStreamIntent(out, opts.Stream)
	sanitizeRawChatToolMessages(out)
}

// applyChatStreamIntent 写入出站 Chat 体的 stream / stream_options（规则 2）。
// 对应 Rust proxy/executor.rs::set_stream_in_json_str（line 694）与
// ensure_chat_stream_usage（line 715）：以 opts.Stream 为准——为真时写上 stream=true 并
// 调用 requestStreamingUsage 打开 include_usage（既有 stream_options 兄弟键保留），
// 为假时移除 stream（转换路径不保留入口的流式意图）。
func applyChatStreamIntent(out map[string]any, stream bool) {
	if !stream {
		delete(out, "stream")
		return
	}
	out["stream"] = true
	requestStreamingUsage(out)
}

// requestStreamingUsage 移植自 ocgo main.go::requestStreamingUsage（~1510）：
// 流式请求需要（且需要打开）stream_options.include_usage 时返回 true 并就地打开；
// 非流式、或已开启时返回 false。既有 stream_options 兄弟键一律保留。
func requestStreamingUsage(req map[string]any) bool {
	streaming, _ := asBool(req["stream"])
	if !streaming {
		return false
	}
	options, ok := asMap(req["stream_options"])
	if !ok {
		options = map[string]any{}
		req["stream_options"] = options
	}
	if enabled, _ := asBool(options["include_usage"]); enabled {
		return false
	}
	options["include_usage"] = true
	return true
}

// rawChatBodyHasImages 移植自 ocgo main.go::rawChatBodyHasImages（~1526）：
// 出站 Chat 体里是否带图片。
func rawChatBodyHasImages(req map[string]any) bool {
	messages, _ := asArray(req["messages"])
	for _, item := range messages {
		msg, ok := asMap(item)
		if !ok {
			continue
		}
		if contentHasImage(msg["content"]) {
			return true
		}
	}
	return false
}

// contentHasImage 移植自 ocgo main.go::contentHasImage（~1850）：
// content part 数组里是否有带 URL 的 image_url / input_image。
func contentHasImage(content any) bool {
	parts, ok := asArray(content)
	if !ok {
		return false
	}
	for _, part := range parts {
		m, ok := asMap(part)
		if !ok {
			continue
		}
		switch typ, _ := asString(m["type"]); typ {
		case "image_url":
			if s, ok := asString(m["image_url"]); ok && s != "" {
				return true
			}
			if image, ok := asMap(m["image_url"]); ok {
				if url, _ := asString(image["url"]); url != "" {
					return true
				}
			}
		case "input_image":
			return true
		}
	}
	return false
}

// stripRawChatImageDetails 移植自 ocgo main.go::stripRawChatImageDetails（~1555）：
// 去掉 content part 及其 image_url 上的 detail（ocgo 的返回值这里不需要，调用方不再使用）。
func stripRawChatImageDetails(req map[string]any) {
	messages, _ := asArray(req["messages"])
	for _, item := range messages {
		msg, ok := asMap(item)
		if !ok {
			continue
		}
		parts, _ := asArray(msg["content"])
		for _, part := range parts {
			p, ok := asMap(part)
			if !ok {
				continue
			}
			delete(p, "detail")
			if image, ok := asMap(p["image_url"]); ok {
				delete(image, "detail")
			}
		}
	}
}

// unsupportedImageModelError 移植自 ocgo main.go::unsupportedImageModelError（~1548）：
// 模型不支持图片时的错误文案（模型名为空时用 "unknown"）。
func unsupportedImageModelError(model string) error {
	if model == "" {
		model = "unknown"
	}
	return fmt.Errorf("model %s does not support image inputs", model)
}

// ==================== tool 消息清洗 ====================

// rawChatMessageInfo 移植自 ocgo main.go::rawChatMessageInfo（~2072）。
type rawChatMessageInfo struct {
	role        string
	toolCallID  string
	toolCallIDs []string
}

// parseRawChatMessage 移植自 ocgo main.go::parseRawChatMessage（~2078）：
// 取出 role / tool_call_id / tool_calls 里的 id 顺序（去重、去空）。
func parseRawChatMessage(msg map[string]any) rawChatMessageInfo {
	role, _ := asString(msg["role"])
	toolCallID, _ := asString(msg["tool_call_id"])
	info := rawChatMessageInfo{role: role, toolCallID: toolCallID}
	calls, ok := asArray(msg["tool_calls"])
	if !ok {
		return info
	}
	seen := map[string]bool{}
	for _, call := range calls {
		m, ok := asMap(call)
		if !ok {
			continue
		}
		id, _ := asString(m["id"])
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			info.toolCallIDs = append(info.toolCallIDs, id)
			seen[id] = true
		}
	}
	return info
}

// rawToolPlaceholderMessage 移植自 ocgo main.go::rawToolPlaceholderMessage（~2099）：
// 缺失工具结果的保守占位。
func rawToolPlaceholderMessage(callID string) map[string]any {
	return map[string]any{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      unavailableToolResultContent,
	}
}

// sanitizeRawChatToolMessages 移植自 ocgo main.go::sanitizeRawChatToolMessages（~2001）：
// 只在确实需要丢弃孤儿 tool 消息或补占位时才替换 messages 数组，其余字段原样保留。
func sanitizeRawChatToolMessages(body map[string]any) {
	messages, ok := asArray(body["messages"])
	if !ok {
		return
	}
	sanitized, changed := sanitizeRawChatMessages(messages)
	if !changed {
		return
	}
	body["messages"] = sanitized
}

// sanitizeRawChatMessages 移植自 ocgo main.go::sanitizeRawChatMessages（~2030），
// 与 sanitizeOAIToolMessages（~1935）是同一算法的两种输入形态，本端口里 messages 就是
// map 列表，因此合并成一个：强制 Chat 的「assistant.tool_calls 之后紧跟其 tool 结果」不变式。
// 孤儿 / 迟到的 tool 消息被丢弃，缺失的结果补 Tool result unavailable.（规则 9）。
func sanitizeRawChatMessages(messages []any) ([]any, bool) {
	out := make([]any, 0, len(messages))
	changed := false
	for i := 0; i < len(messages); {
		msg, ok := asMap(messages[i])
		if !ok {
			// 非对象消息原样保留，不做判断。
			out = append(out, messages[i])
			i++
			continue
		}
		info := parseRawChatMessage(msg)
		if info.role == "tool" {
			// 没有紧跟在 assistant.tool_calls 之后的 tool 消息是孤儿 / 迟到结果。
			changed = true
			i++
			continue
		}
		out = append(out, messages[i])
		if info.role != "assistant" || len(info.toolCallIDs) == 0 {
			i++
			continue
		}

		seen := map[string]bool{}
		j := i + 1
		for j < len(messages) {
			next, ok := asMap(messages[j])
			if !ok {
				break
			}
			nextInfo := parseRawChatMessage(next)
			if nextInfo.role != "tool" {
				break
			}
			if hasCallID(info.toolCallIDs, nextInfo.toolCallID) && !seen[nextInfo.toolCallID] {
				out = append(out, messages[j])
				seen[nextInfo.toolCallID] = true
			} else {
				changed = true
			}
			j++
		}
		for _, id := range info.toolCallIDs {
			if !seen[id] {
				out = append(out, rawToolPlaceholderMessage(id))
				changed = true
			}
		}
		i = j
	}
	return out, changed
}

// hasCallID 是 ocgo main.go::containsString（~1989）在 []string 上的等价物。
func hasCallID(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ==================== reasoning_effort 折叠 ====================

// downstreamReasoningEffort 移植自 ocgo main.go::downstreamReasoningEffort（~1444）与
// reasoningEffortFromRaw（~1453）：多来源取第一个可用值并归一化。map 形态下数值已经是
// any，无需像 ocgo 那样先解 RawMessage，因此两者合并成一个函数。
func downstreamReasoningEffort(values ...any) string {
	for _, value := range values {
		if effort := reasoningEffortFromAny(value); effort != "" {
			return normalizeReasoningEffort(effort)
		}
	}
	return ""
}

// reasoningEffortFromAny 移植自 ocgo main.go::reasoningEffortFromAny（~1464）：
// 从字符串 / 数字 / 嵌套对象里取出推理强度；`{"type":"enabled"}` 视为 high。
//
// 与 ocgo 的差异：ocgo 只列了 float64 一种数字形态，而本仓库的 decodeObject 开启了
// UseNumber（数值是 json.Number），因此数字统一交给 asFloat 取。
func reasoningEffortFromAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		for _, key := range []string{"effort", "level", "depth", "reasoning_effort"} {
			if effort := reasoningEffortFromAny(t[key]); effort != "" {
				return effort
			}
		}
		if typ, _ := asString(t["type"]); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			return "high"
		}
		for _, key := range []string{"reasoning", "thinking", "output_config"} {
			if effort := reasoningEffortFromAny(t[key]); effort != "" {
				return effort
			}
		}
	default:
		if n, ok := asFloat(v); ok {
			return formatReasoningNumber(n)
		}
	}
	return ""
}

// formatReasoningNumber 移植自 ocgo main.go::formatReasoningNumber（~1488）。
func formatReasoningNumber(n float64) string {
	if n == float64(int64(n)) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

// normalizeReasoningEffort 移植自 ocgo main.go::normalizeReasoningEffort（~1495）：
// 各种强度的别称统一成 minimal / low / medium / high（无法识别时保留原值）。
func normalizeReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "0", "minimal", "min", "none", "off", "disabled", "false":
		return "minimal"
	case "1", "low", "light":
		return "low"
	case "2", "medium", "med", "normal", "default":
		return "medium"
	case "3", "4", "high", "xhigh", "max", "maximum", "deep", "true", "enabled":
		return "high"
	default:
		return strings.TrimSpace(effort)
	}
}

// ==================== Responses 跨格式校验 ====================

// validateResponsesCrossFormat 对应 Rust request.rs::validate_responses_cross_format（line 29）：
// previous_response_id / conversation 无法跨协议续接；只有 encrypted_content 而没有可读
// summary 的 reasoning 项无法搬运。错误文案与 Rust 逐字一致（规则 7）。
// 注意 `include: ["reasoning.encrypted_content"]` 只是可选响应字段的开关，不拒绝
// （Rust 注释明确说明）。
func validateResponsesCrossFormat(req map[string]any) error {
	if req == nil {
		return nil
	}
	for _, key := range []string{"previous_response_id", "conversation"} {
		if v, ok := req[key]; ok && v != nil {
			return fmt.Errorf("Responses field '%s' cannot be continued across protocol conversion", key)
		}
	}
	items, ok := asArray(req["input"])
	if !ok {
		return nil
	}
	for _, item := range items {
		m, ok := asMap(item)
		if !ok {
			continue
		}
		if typ, _ := asString(m["type"]); typ != "reasoning" {
			continue
		}
		if _, has := m["encrypted_content"]; !has {
			continue
		}
		summary, isArray := asArray(m["summary"])
		if !isArray || len(summary) == 0 {
			return errors.New("Responses encrypted reasoning has no portable summary for protocol conversion")
		}
	}
	return nil
}

// ==================== Anthropic prompt caching 断点注入 ====================

// ephemeralCacheControl 对应 Rust request.rs::ephemeral（line 1259）。
func ephemeralCacheControl() map[string]any {
	return map[string]any{"type": "ephemeral"}
}

// injectAnthropicCacheBreakpoints 对应 Rust request.rs::inject_anthropic_cache_breakpoints
// （line 1319）：向已是 Anthropic 形态的请求体自动注入 prompt caching 断点。幂等——
// 请求体中已存在任何 cache_control 时不做改动。注入策略（最多 4 个断点）：system 末块、
// tools 末个、最后一条消息末块、消息数 ≥2 时倒数第二条 user 消息末块。
// 返回是否发生了改动（Go 门面据此决定是否需要重新序列化）。
func injectAnthropicCacheBreakpoints(req map[string]any) bool {
	if req == nil || bodyHasCacheControl(req) {
		return false
	}
	budget := 4
	changed := false

	// system：末块。
	if budget > 0 {
		if system, ok := req["system"]; ok {
			switch v := system.(type) {
			case string:
				if v != "" {
					req["system"] = []any{map[string]any{
						"type":          "text",
						"text":          v,
						"cache_control": ephemeralCacheControl(),
					}}
					budget--
					changed = true
				}
			case []any:
				for i := len(v) - 1; i >= 0; i-- {
					block, ok := asMap(v[i])
					if !ok {
						continue
					}
					if typ, _ := asString(block["type"]); typ != "text" {
						continue
					}
					block["cache_control"] = ephemeralCacheControl()
					budget--
					changed = true
					break
				}
			}
		}
	}

	// tools：末个。
	if budget > 0 {
		if tools, ok := asArray(req["tools"]); ok && len(tools) > 0 {
			if last, ok := asMap(tools[len(tools)-1]); ok {
				last["cache_control"] = ephemeralCacheControl()
				budget--
				changed = true
			}
		}
	}

	// messages：最后一条末块；消息数 ≥2 时倒数第二条 user 消息末块。
	if budget > 0 {
		if messages, ok := asArray(req["messages"]); ok && len(messages) > 0 {
			n := len(messages)
			if n >= 2 {
				for i := n - 2; i >= 0; i-- {
					msg, ok := asMap(messages[i])
					if !ok {
						continue
					}
					if role, _ := asString(msg["role"]); role != "user" {
						continue
					}
					if setCacheControlOnLastBlock(msg) {
						budget--
						changed = true
					}
					break
				}
			}
			if budget > 0 {
				if last, ok := asMap(messages[n-1]); ok {
					if setCacheControlOnLastBlock(last) {
						budget--
						changed = true
					}
				}
			}
		}
	}
	return changed
}

// bodyHasCacheControl 对应 Rust request.rs::body_has_cache_control（line 1277）：
// 扫描 Anthropic 请求体的 system / messages / tools 是否已含任何 cache_control。
func bodyHasCacheControl(body map[string]any) bool {
	for _, key := range []string{"system", "messages", "tools"} {
		if valueHasCacheControl(body[key]) {
			return true
		}
	}
	return false
}

// valueHasCacheControl 对应 Rust request.rs::value_has_cache_control（line 1263）：递归查找。
func valueHasCacheControl(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["cache_control"]; ok {
			return true
		}
		for _, value := range x {
			if valueHasCacheControl(value) {
				return true
			}
		}
	case []any:
		for _, value := range x {
			if valueHasCacheControl(value) {
				return true
			}
		}
	}
	return false
}

// setCacheControlOnLastBlock 对应 Rust request.rs::set_cache_control_on_last_block_of_message
// （line 1291）：把消息最后一个 content 块打上 cache_control；content 是字符串时先转成
// 单元素 text 数组。成功打点返回 true。
func setCacheControlOnLastBlock(msg map[string]any) bool {
	content, ok := msg["content"]
	if !ok {
		return false
	}
	if s, ok := asString(content); ok {
		msg["content"] = []any{map[string]any{
			"type":          "text",
			"text":          s,
			"cache_control": ephemeralCacheControl(),
		}}
		return true
	}
	blocks, ok := asArray(content)
	if !ok || len(blocks) == 0 {
		return false
	}
	last, ok := asMap(blocks[len(blocks)-1])
	if !ok {
		return false
	}
	last["cache_control"] = ephemeralCacheControl()
	return true
}
