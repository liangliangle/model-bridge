package converter

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// 本文件对应 Rust converter/common.rs 的共享工具，外加 ocgo main.go 里同类的
// 取值辅助。每个函数上方标注了对应实现。

// ==================== 结束原因映射（Rust common.rs）====================

// finishReasonToStatus 对应 Rust common.rs::finish_reason_to_status：
// Chat finish_reason → Responses status。
func finishReasonToStatus(finishReason string) string {
	switch finishReason {
	case "length", "content_filter", "refusal":
		return "incomplete"
	default:
		return "completed"
	}
}

// statusToFinishReason 对应 Rust common.rs::status_to_finish_reason：
// Responses status → Chat finish_reason。
func statusToFinishReason(status string) string {
	if status == "incomplete" {
		return "length"
	}
	return "stop"
}

// anthropicStopToFinish 对应 Rust common.rs::anthropic_stop_to_finish：
// Anthropic stop_reason → Chat finish_reason。
func anthropicStopToFinish(stop string) string {
	switch stop {
	case "max_tokens", "compaction":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// finishToAnthropicStop 对应 Rust common.rs::finish_to_anthropic_stop：
// Chat finish_reason → Anthropic stop_reason。
func finishToAnthropicStop(finishReason string) string {
	switch finishReason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// ==================== usage 字段读取（ocgo main.go 的 intField/cachedTokens）====================

// intField 移植自 ocgo main.go::intField。
func intField(fields map[string]any, name string) int {
	v, ok := fields[name]
	if !ok {
		return 0
	}
	n, _ := asInt(v)
	return int(n)
}

// intFromAny 移植自 ocgo main.go::intFromAny。
func intFromAny(v any) int {
	n, _ := asInt(v)
	return int(n)
}

// cachedTokens 移植自 ocgo main.go::cachedTokens：按
// prompt_tokens_details / input_tokens_details → cache_read_input_tokens →
// cached_tokens 的顺序取缓存命中量。
func cachedTokens(fields map[string]any) int {
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if nested, ok := asMap(fields[key]); ok {
			if n := intField(nested, "cached_tokens"); n > 0 {
				return n
			}
		}
	}
	if n := intField(fields, "cache_read_input_tokens"); n > 0 {
		return n
	}
	return intField(fields, "cached_tokens")
}

// ==================== usage 换算（Rust common.rs）====================
//
// 口径（与 Rust 完全一致，注意与 ocgo 的差异）：
//   - Chat / Responses 的 prompt_tokens / input_tokens **含**缓存命中；
//   - Anthropic 的 input_tokens **不含**（cache_read_input_tokens 单列）。
// 因此读入 Chat/Responses usage 时要扣缓存，写出 Anthropic 形态时不再加回，
// 写出 Responses 形态时把缓存加回 input。

// chatUsageToResponses 对应 Rust common.rs::chat_usage_to_responses。
func chatUsageToResponses(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	input := intField(usage, "prompt_tokens")
	output := intField(usage, "completion_tokens")
	total := intField(usage, "total_tokens")
	if total == 0 {
		total = input + output
	}
	out := map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"total_tokens":  total,
	}
	if cached := nestedInt(usage, "prompt_tokens_details", "cached_tokens"); cached > 0 {
		out["input_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoning := nestedInt(usage, "completion_tokens_details", "reasoning_tokens"); reasoning > 0 {
		out["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return out
}

// responsesUsageToChat 对应 Rust common.rs::responses_usage_to_chat。
func responsesUsageToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	input := intField(usage, "input_tokens")
	output := intField(usage, "output_tokens")
	total := intField(usage, "total_tokens")
	if total == 0 {
		total = input + output
	}
	out := map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": output,
		"total_tokens":      total,
	}
	if cached := nestedInt(usage, "input_tokens_details", "cached_tokens"); cached > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoning := nestedInt(usage, "output_tokens_details", "reasoning_tokens"); reasoning > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	return out
}

// anthropicUsageToChat 对应 Rust common.rs::anthropic_usage_to_chat。
func anthropicUsageToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	input := intField(usage, "input_tokens")
	cacheRead := intField(usage, "cache_read_input_tokens")
	cacheCreation := intField(usage, "cache_creation_input_tokens")
	output := intField(usage, "output_tokens")
	prompt := input + cacheRead + cacheCreation
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": output,
		"total_tokens":      prompt + output,
	}
	if cacheRead > 0 || cacheCreation > 0 {
		details := map[string]any{}
		if cacheRead > 0 {
			details["cached_tokens"] = cacheRead
		}
		if cacheCreation > 0 {
			details["cache_creation_tokens"] = cacheCreation
		}
		out["prompt_tokens_details"] = details
	}
	return out
}

// chatUsageToAnthropic 对应 Rust common.rs::chat_usage_to_anthropic：
// Anthropic 的 input_tokens 不含缓存命中，因此要从 Chat 的 prompt_tokens 里扣除。
func chatUsageToAnthropic(usage map[string]any) map[string]any {
	input := 0
	output := 0
	cacheRead := 0
	if usage != nil {
		input = intField(usage, "prompt_tokens")
		output = intField(usage, "completion_tokens")
		cacheRead = nestedInt(usage, "prompt_tokens_details", "cached_tokens")
	}
	realInput := input - cacheRead
	if realInput < 0 {
		realInput = 0
	}
	out := map[string]any{
		"input_tokens":  realInput,
		"output_tokens": output,
	}
	if cacheRead > 0 {
		out["cache_read_input_tokens"] = cacheRead
	}
	return out
}

// nestedInt 读取 obj[key1][key2] 的整数值。
func nestedInt(obj map[string]any, key1, key2 string) int {
	nested, ok := asMap(obj[key1])
	if !ok {
		return 0
	}
	return intField(nested, key2)
}

// ==================== 流式 usage 累加（Rust common.rs::StreamUsage）====================

// streamUsage 是流式 usage 累加器，对照 Rust common.rs::StreamUsage。
//
// 三套协议下发 usage 的位置不同：Chat 在末尾一个 choices:[] 的 chunk；
// Responses 在 response.completed 等事件的 response.usage；Anthropic 在
// message_start（输入）与 message_delta（累计输出）。入口协议只在收尾事件里
// 输出 usage，而上游可能更晚才给到，因此这里先累加、收尾时统一输出。
//
// 合并策略：仅当读到**非零**值时覆盖，后到的缺字段事件不会清掉先到的值。
// 内部 InputTokens 采用 Anthropic 口径（已扣除缓存命中）。
type streamUsage struct {
	inputTokens         *int
	outputTokens        *int
	cacheReadTokens     *int
	cacheCreationTokens *int
}

func setIfPositive(target **int, value int) {
	if value > 0 {
		v := value
		*target = &v
	}
}

func (u *streamUsage) setInput(value int)       { setIfPositive(&u.inputTokens, value) }
func (u *streamUsage) setOutput(value int)      { setIfPositive(&u.outputTokens, value) }
func (u *streamUsage) setCacheRead(value int)   { setIfPositive(&u.cacheReadTokens, value) }
func (u *streamUsage) setCacheCreate(value int) { setIfPositive(&u.cacheCreationTokens, value) }

// mergeChat 对应 Rust common.rs::StreamUsage::merge_chat。
// Chat 的 prompt_tokens 含缓存命中，而 Anthropic 的 input_tokens 不含，
// 因此这里扣除 cached 后再存入，避免下游重复计数。
func (u *streamUsage) mergeChat(usage map[string]any) {
	if usage == nil {
		return
	}
	cached := nestedInt(usage, "prompt_tokens_details", "cached_tokens")
	if total, ok := asInt(usage["prompt_tokens"]); ok {
		real := int(total) - cached
		if real < 0 {
			real = 0
		}
		u.setInput(real)
	}
	u.setOutput(intField(usage, "completion_tokens"))
	u.setCacheRead(cached)
}

// mergeResponses 对应 Rust common.rs::StreamUsage::merge_responses。
func (u *streamUsage) mergeResponses(usage map[string]any) {
	if usage == nil {
		return
	}
	cached := nestedInt(usage, "input_tokens_details", "cached_tokens")
	if total, ok := asInt(usage["input_tokens"]); ok {
		real := int(total) - cached
		if real < 0 {
			real = 0
		}
		u.setInput(real)
	}
	u.setOutput(intField(usage, "output_tokens"))
	u.setCacheRead(cached)
}

// mergeAnthropic 对应 Rust common.rs::StreamUsage::merge_anthropic。
func (u *streamUsage) mergeAnthropic(usage map[string]any) {
	if usage == nil {
		return
	}
	u.setInput(intField(usage, "input_tokens"))
	u.setOutput(intField(usage, "output_tokens"))
	u.setCacheRead(intField(usage, "cache_read_input_tokens"))
	u.setCacheCreate(intField(usage, "cache_creation_input_tokens"))
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// toAnthropicMessageUsage 对应 Rust StreamUsage::to_anthropic_message_usage。
func (u *streamUsage) toAnthropicMessageUsage() map[string]any {
	out := map[string]any{
		"input_tokens":  derefInt(u.inputTokens),
		"output_tokens": derefInt(u.outputTokens),
	}
	if u.cacheReadTokens != nil {
		out["cache_read_input_tokens"] = *u.cacheReadTokens
	}
	if u.cacheCreationTokens != nil {
		out["cache_creation_input_tokens"] = *u.cacheCreationTokens
	}
	return out
}

// toAnthropicDeltaUsage 对应 Rust StreamUsage::to_anthropic_delta_usage
// （与 message_usage 同形：Anthropic 的 message_delta 承载累计用量）。
func (u *streamUsage) toAnthropicDeltaUsage() map[string]any {
	return u.toAnthropicMessageUsage()
}

// toResponsesUsage 对应 Rust StreamUsage::to_responses_usage。
// Responses 的 input_tokens 与 Chat 一致**含**缓存命中，因此把 merge 时扣除的
// cached / creation 加回去，保证跨协议往返不失真。
func (u *streamUsage) toResponsesUsage() map[string]any {
	cached := derefInt(u.cacheReadTokens)
	creation := derefInt(u.cacheCreationTokens)
	input := derefInt(u.inputTokens) + cached + creation
	output := derefInt(u.outputTokens)
	out := map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"total_tokens":  input + output,
	}
	details := map[string]any{}
	if cached > 0 {
		details["cached_tokens"] = cached
	}
	// Responses 无缓存写入概念，但 Anthropic 上游会给出，保留以免丢信息。
	if creation > 0 {
		details["cache_creation_tokens"] = creation
	}
	if len(details) > 0 {
		out["input_tokens_details"] = details
	}
	return out
}

// toChatUsage 输出 Chat 形态的 usage。
//
// 与 toResponsesUsage 同理：Chat 的 prompt_tokens **含**缓存命中，而 mergeChat /
// mergeAnthropic / mergeResponses 在累加时把命中量扣到了 cacheRead 里，
// 因此这里要加回去（数学与 anthropicUsageToChat 完全一致，两跳时才不会重复扣）。
func (u *streamUsage) toChatUsage() map[string]any {
	cached := derefInt(u.cacheReadTokens)
	creation := derefInt(u.cacheCreationTokens)
	prompt := derefInt(u.inputTokens) + cached + creation
	completion := derefInt(u.outputTokens)
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
	details := map[string]any{}
	if cached > 0 {
		details["cached_tokens"] = cached
	}
	if creation > 0 {
		details["cache_creation_tokens"] = creation
	}
	if len(details) > 0 {
		out["prompt_tokens_details"] = details
	}
	return out
}

// usage 把累加器导出为对外 Usage（Anthropic 口径，见 Usage 注释）。
func (u *streamUsage) usage() Usage {
	return Usage{
		InputTokens:         derefInt(u.inputTokens),
		OutputTokens:        derefInt(u.outputTokens),
		CacheReadTokens:     derefInt(u.cacheReadTokens),
		CacheCreationTokens: derefInt(u.cacheCreationTokens),
	}
}

// ==================== id 生成（Rust common.rs::gen_id）====================

// genID 对应 Rust common.rs::gen_id：前缀 + 24 位随机十六进制。
func genID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// 退化路径：用时间戳填充，保证仍返回可用 id，不 panic。
		binary.BigEndian.PutUint64(buf[:8], uint64(time.Now().UnixNano()))
	}
	return prefix + hex.EncodeToString(buf)
}

// ==================== 工具名脱敏与映射（Rust common.rs）====================

// sanitizeToolName 对应 Rust common.rs::sanitize_tool_name：
// 脱敏为 Anthropic 允许的 ^[a-zA-Z0-9_-]{1,128}$。
func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "_"
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

// toolNameMap 对应 Rust common.rs::ToolNameMap：原始名 ↔ 脱敏名双向映射。
type toolNameMap struct {
	fwd map[string]string // 原始名 → 脱敏名
	rev map[string]string // 脱敏名 → 原始名
}

// buildToolNameMaps 对应 Rust common.rs::build_tool_name_maps：
// 依工具列表构建双向映射，重名时用数字后缀消歧。
func buildToolNameMaps(tools []any) toolNameMap {
	out := toolNameMap{fwd: map[string]string{}, rev: map[string]string{}}
	used := map[string]int{}
	for _, t := range tools {
		tool, ok := asMap(t)
		if !ok {
			continue
		}
		orig := ""
		if fn, ok := asMap(tool["function"]); ok {
			orig, _ = asString(fn["name"])
		}
		if orig == "" {
			orig, _ = asString(tool["name"])
		}
		if orig == "" {
			continue
		}
		if _, ok := out.fwd[orig]; ok {
			continue
		}
		base := sanitizeToolName(orig)
		candidate := base
		for {
			if _, taken := out.rev[candidate]; !taken {
				break
			}
			used[base]++
			candidate = base + "_" + itoa(used[base])
		}
		out.fwd[orig] = candidate
		out.rev[candidate] = orig
	}
	return out
}

// ==================== 文本提取（Rust common.rs::extract_text）====================

// extractText 对应 Rust common.rs::extract_text：
// 从 content（字符串或数组）中提取纯文本，数组内取各 text / *_text 字段拼接。
func extractText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := asMap(part); ok {
				if t, ok := asString(m["text"]); ok {
					b.WriteString(t)
					continue
				}
			}
			if s, ok := asString(part); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

// ==================== 多模态图片转换（Rust common.rs）====================

// openaiImageToAnthropic 对应 Rust common.rs::openai_image_to_anthropic：
// OpenAI image_url（{url, detail} 或纯字符串）→ Anthropic image source block。
func openaiImageToAnthropic(imageURL any) map[string]any {
	url := ""
	if m, ok := asMap(imageURL); ok {
		url, _ = asString(m["url"])
	}
	if url == "" {
		url, _ = asString(imageURL)
	}
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		if meta, data, found := strings.Cut(rest, ","); found {
			mediaType := meta
			if mt, _, ok := strings.Cut(meta, ";"); ok && mt != "" {
				mediaType = mt
			}
			if mediaType == "" {
				mediaType = "image/png"
			}
			return map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       data,
				},
			}
		}
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": url},
	}
}

// anthropicImageToOpenAI 对应 Rust common.rs::anthropic_image_to_openai：
// base64 → data URI；url → 直接 url。
func anthropicImageToOpenAI(source map[string]any) map[string]any {
	stype, _ := asString(source["type"])
	if stype == "base64" {
		mediaType, _ := asString(source["media_type"])
		if mediaType == "" {
			mediaType = "image/png"
		}
		data, _ := asString(source["data"])
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + mediaType + ";base64," + data},
		}
	}
	url, _ := asString(source["url"])
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": url},
	}
}

// ==================== reasoning_effort → thinking budget（Rust common.rs）====================

// effortToBudget 对应 Rust common.rs::effort_to_budget。
// 冻结矩阵里 chat→messages 只做响应方向，因此本函数当前不参与转换路径，
// 但按 Rust 契约保留（reviewer 可逐行对照）。
func effortToBudget(effort string) int64 {
	switch effort {
	case "minimal", "low":
		return 1024
	case "medium":
		return 2048
	case "high":
		return 4096
	case "xhigh":
		return 8192
	case "max":
		return 16384
	default:
		return 2048
	}
}

// ==================== namespace 工具展平与反向映射（Rust common.rs）====================

// nsEntry 是 flat 工具名 → (namespace_name, subtool_name) 的反向映射值。
// custom 工具用 namespace 位置存放 CUSTOM_TOOL_NAMESPACE_MARKER。
type nsEntry struct {
	namespace string
	subtool   string
}

// CUSTOM_TOOL_NAMESPACE_MARKER 对应 Rust common.rs::CUSTOM_TOOL_NAMESPACE_MARKER：
// 在反向映射里标记 Responses 的 custom 工具（与 namespace 的 subtool 区分开）。
const CUSTOM_TOOL_NAMESPACE_MARKER = "__model_bridge_custom_tool__"

// flattenNamespaceSubtool 对应 Rust common.rs::flatten_namespace_subtool：
// 把 namespace 内的 subtool 展平成 Chat function tool，命名 `{ns}__{sub}`，
// 同时写入反向映射；冲突时加 `_2`/`_3` 后缀消歧。
func flattenNamespaceSubtool(nsName string, subtool map[string]any, nsMap map[string]nsEntry) (map[string]any, bool) {
	subName, ok := asString(subtool["name"])
	if !ok {
		return nil, false
	}
	flatRaw := sanitizeToolName(nsName) + "__" + sanitizeToolName(subName)
	flatBase := sanitizeToolName(flatRaw)
	candidate := flatBase
	suffix := 2
	for {
		if _, taken := nsMap[candidate]; !taken {
			break
		}
		candidate = flatBase + "_" + itoa(suffix)
		suffix++
	}
	nsMap[candidate] = nsEntry{namespace: nsName, subtool: subName}

	params := subtool["parameters"]
	if params == nil {
		params = map[string]any{"type": "object"}
	}
	fn := map[string]any{
		"name":       candidate,
		"parameters": params,
	}
	if desc, ok := subtool["description"]; ok {
		fn["description"] = desc
	}
	return map[string]any{"type": "function", "function": fn}, true
}

// registerCustomTool 对应 Rust common.rs::register_custom_tool：
// Responses custom 工具在 Chat 承载期间保留原名，以便响应侧还原成 custom_tool_call。
func registerCustomTool(name string, nsMap map[string]nsEntry) {
	if name != "" {
		nsMap[name] = nsEntry{namespace: CUSTOM_TOOL_NAMESPACE_MARKER, subtool: name}
	}
}

// unwrapCustomToolArguments 对应 Rust common.rs::unwrap_custom_tool_arguments：
// 拆掉 custom 工具在 Chat 侧的函数外壳（{"content": "..."}）。
// 非法 / 非对象 JSON 本身就是合法的 custom 载荷，原样返回。
func unwrapCustomToolArguments(arguments string) string {
	if arguments == "" {
		return ""
	}
	v, ok := decodeAny(arguments)
	if !ok {
		return arguments
	}
	obj, ok := asMap(v)
	if !ok {
		return arguments
	}
	if content, ok := asString(obj["content"]); ok {
		return content
	}
	return arguments
}

// ==================== reasoning_content 回显缓存（ocgo main.go）====================

// reasoningContentCache 对应 ocgo main.go::reasoningContentCache：
// 以 tool_call id 为键缓存 assistant 的 reasoning_content。
// Moonshot/Kimi 在开启 thinking 时会拒绝缺少 reasoning_content 的
// assistant tool-call 历史，因此跨请求回显（因此是包级状态，与 ocgo 一致）。
var reasoningContentCache = struct {
	sync.Mutex
	byCallID map[string]string
}{byCallID: map[string]string{}}

// cachedReasoningContent 移植自 ocgo main.go::cachedReasoningContent。
func cachedReasoningContent(callIDs []string) string {
	reasoningContentCache.Lock()
	defer reasoningContentCache.Unlock()
	for _, id := range callIDs {
		if reasoning := reasoningContentCache.byCallID[id]; reasoning != "" {
			return reasoning
		}
	}
	if len(callIDs) > 0 {
		// 上游流式有时不给 reasoning_content，补一个最小占位，避免被拒。
		return "Tool call requested."
	}
	return ""
}

// cacheReasoningContent 移植自 ocgo main.go::cacheReasoningContent。
func cacheReasoningContent(callIDs []string, reasoning string) {
	if reasoning == "" || len(callIDs) == 0 {
		return
	}
	reasoningContentCache.Lock()
	defer reasoningContentCache.Unlock()
	for _, id := range callIDs {
		if id != "" {
			reasoningContentCache.byCallID[id] = reasoning
		}
	}
}

// ==================== 小工具 ====================

// itoa 是 strconv.Itoa 的别名，避免为此引入额外 import 噪音。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// intOr 取整数值，缺失时返回 def。
func intOr(v any, def int) int {
	if n, ok := asInt(v); ok {
		return int(n)
	}
	return def
}

// cloneMap 浅拷贝一个 JSON 对象（避免就地修改入参）。
func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cloneValue 浅拷贝 JSON 值（对象/数组复制一层，标量原样）。
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		copy(out, x)
		return out
	default:
		return v
	}
}

// jsonString 把任意值序列化为紧凑 JSON 字符串（对应 ocgo 的 string(raw) 语义）。
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
