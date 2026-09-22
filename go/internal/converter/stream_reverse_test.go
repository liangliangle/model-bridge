package converter

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"modelbridge/internal/sse"
)

// 逆向流式转换（stream_reverse.go）与流水线（pipeline.go）的测试。
//
// 覆盖三件事：
//  1. 富协议事件流 → Chat chunk 流的逐事件映射（含工具调用分片与推理内容）；
//  2. 任意 chunk 边界下输出逐字节一致（与既有 chat→rich 方向同样的不变式）；
//  3. 两跳时 usage 仍是**上游真实值**（中间态是合成的，不能当账单口径）。

// ==================== 断言辅助 ====================

// chatChunkPayloads 解析下游 Chat SSE 里的每个 chunk 载荷（不含 [DONE]）。
func chatChunkPayloads(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, ev := range sse.Events(string(raw)) {
		if ev.Data == "" {
			continue
		}
		obj := mustObject(t, ev.Data)
		out = append(out, obj)
	}
	return out
}

// chatDeltaText 拼接所有 chunk 的 choices[0].delta.content。
func chatDeltaText(t *testing.T, chunks []map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, chunk := range chunks {
		for _, delta := range chatDeltas(t, chunk) {
			if text, ok := asString(delta["content"]); ok {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

// chatReasoningText 拼接所有 chunk 的 reasoning_content。
func chatReasoningText(t *testing.T, chunks []map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, chunk := range chunks {
		for _, delta := range chatDeltas(t, chunk) {
			if text, ok := asString(delta["reasoning_content"]); ok {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

// chatDeltas 取一个 chunk 的 choices[].delta（无 choices 时返回 nil）。
func chatDeltas(t *testing.T, chunk map[string]any) []map[string]any {
	t.Helper()
	choices, ok := asArray(chunk["choices"])
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, raw := range choices {
		choice, ok := asMap(raw)
		if !ok {
			continue
		}
		if delta, ok := asMap(choice["delta"]); ok {
			out = append(out, delta)
		}
	}
	return out
}

// chatFinishReasons 收集非空的 finish_reason。
func chatFinishReasons(t *testing.T, chunks []map[string]any) []string {
	t.Helper()
	out := []string{}
	for _, chunk := range chunks {
		choices, ok := asArray(chunk["choices"])
		if !ok {
			continue
		}
		for _, raw := range choices {
			choice, _ := asMap(raw)
			if fr, ok := asString(choice["finish_reason"]); ok && fr != "" {
				out = append(out, fr)
			}
		}
	}
	return out
}

// chatStreamUsage 取 usage-only chunk 的 usage 对象。
func chatStreamUsage(t *testing.T, chunks []map[string]any) map[string]any {
	t.Helper()
	for _, chunk := range chunks {
		if choices, ok := asArray(chunk["choices"]); ok && len(choices) == 0 {
			if usage, ok := asMap(chunk["usage"]); ok {
				return usage
			}
		}
	}
	t.Fatalf("没有找到 usage-only chunk")
	return nil
}

// chatToolCalls 汇总 tool_calls 增量：按 index 分组，拼出 id/name/arguments。
func chatToolCalls(t *testing.T, chunks []map[string]any) []map[string]any {
	t.Helper()
	type acc struct {
		id, name string
		args     strings.Builder
	}
	order := []int{}
	byIndex := map[int]*acc{}
	for _, chunk := range chunks {
		for _, delta := range chatDeltas(t, chunk) {
			calls, ok := asArray(delta["tool_calls"])
			if !ok {
				continue
			}
			for _, craw := range calls {
				call, _ := asMap(craw)
				index := intOr(call["index"], 0)
				entry, exists := byIndex[index]
				if !exists {
					entry = &acc{}
					byIndex[index] = entry
					order = append(order, index)
				}
				if id, ok := asString(call["id"]); ok && id != "" {
					entry.id = id
				}
				if fn, ok := asMap(call["function"]); ok {
					if name, ok := asString(fn["name"]); ok && name != "" {
						entry.name = name
					}
					if args, ok := asString(fn["arguments"]); ok {
						entry.args.WriteString(args)
					}
				}
			}
		}
	}
	out := make([]map[string]any, 0, len(order))
	for _, index := range order {
		entry := byIndex[index]
		out = append(out, map[string]any{
			"index":     index,
			"id":        entry.id,
			"name":      entry.name,
			"arguments": entry.args.String(),
		})
	}
	return out
}

// ==================== 测试用的上游事件流 ====================

// anthropicTextStream 是一段普通的 Anthropic 文本流（含 usage）。
const anthropicTextStream = `event: message_start` + "\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-up","content":[],"stop_reason":null,"usage":{"input_tokens":11,"output_tokens":0}}}` + "\n\n" +
	`event: content_block_start` + "\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	`event: content_block_delta` + "\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	`event: content_block_delta` + "\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world!"}}` + "\n\n" +
	`event: content_block_stop` + "\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	`event: message_delta` + "\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	`event: message_stop` + "\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// anthropicToolStream 是一段带工具调用与推理内容的 Anthropic 流。
const anthropicToolStream = `event: message_start` + "\n" +
	`data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-up","content":[],"stop_reason":null,"usage":{"input_tokens":20,"output_tokens":0}}}` + "\n\n" +
	`event: content_block_start` + "\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
	`event: content_block_delta` + "\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pon"}}` + "\n\n" +
	`event: content_block_delta` + "\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"dering"}}` + "\n\n" +
	`event: content_block_delta` + "\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}` + "\n\n" +
	`event: content_block_stop` + "\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	`event: content_block_start` + "\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}` + "\n\n" +
	`event: content_block_delta` + "\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"loc\":\""}}` + "\n\n" +
	`event: content_block_delta` + "\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"SF\"}"}}` + "\n\n" +
	`event: content_block_stop` + "\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	`event: message_delta` + "\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}` + "\n\n" +
	`event: message_stop` + "\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// responsesTextStream 是一段普通的 Responses 事件流（含逐段正文与终局 usage）。
const responsesTextStream = `event: response.created` + "\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"gpt-up","output":[]}}` + "\n\n" +
	`event: response.output_item.added` + "\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
	`event: response.output_text.delta` + "\n" +
	`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}` + "\n\n" +
	`event: response.output_text.delta` + "\n" +
	`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":", world!"}` + "\n\n" +
	`event: response.completed` + "\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-up","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello, world!","annotations":[]}]}],"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}}` + "\n\n"

// responsesToolStream 是一段带推理摘要与函数调用的 Responses 事件流。
const responsesToolStream = `event: response.created` + "\n" +
	`data: {"type":"response.created","response":{"id":"resp_2","object":"response","status":"in_progress","model":"gpt-up","output":[]}}` + "\n\n" +
	`event: response.output_item.added` + "\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}` + "\n\n" +
	`event: response.reasoning_summary_text.delta` + "\n" +
	`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"pon"}` + "\n\n" +
	`event: response.reasoning_summary_text.delta` + "\n" +
	`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"dering"}` + "\n\n" +
	`event: response.output_item.done` + "\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"pondering"}]}}` + "\n\n" +
	`event: response.output_item.added` + "\n" +
	`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}` + "\n\n" +
	`event: response.function_call_arguments.delta` + "\n" +
	`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"loc\":\""}` + "\n\n" +
	`event: response.function_call_arguments.delta` + "\n" +
	`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"SF\"}"}` + "\n\n" +
	`event: response.completed` + "\n" +
	`data: {"type":"response.completed","response":{"id":"resp_2","object":"response","status":"completed","model":"gpt-up","output":[{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"pondering"}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"loc\":\"SF\"}","status":"completed"}],"usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28}}}` + "\n\n"

// ==================== messages → chat ====================

func TestMessagesToChatStream(t *testing.T) {
	raw, usage := runStreamFrom(t, newConvSession(), FormatOpenAIChat, FormatAnthropic, "claude-x",
		anthropicTextStream, []int{len(anthropicTextStream)})
	chunks := chatChunkPayloads(t, raw)

	// 首个 chunk 必须带 role。
	deltas := chatDeltas(t, chunks[0])
	if len(deltas) != 1 || deltas[0]["role"] != "assistant" {
		t.Fatalf("首个 chunk 应带 role=assistant，实际 %#v", chunks[0])
	}
	if got := chatDeltaText(t, chunks); got != "Hello, world!" {
		t.Fatalf("正文拼接 = %q", got)
	}
	if reasons := chatFinishReasons(t, chunks); len(reasons) != 1 || reasons[0] != "stop" {
		t.Fatalf("finish_reason = %v, want [stop]", reasons)
	}
	// Chat 客户端以 data: [DONE] 结束。
	if !strings.HasSuffix(string(raw), "data: [DONE]\n\n") {
		t.Fatalf("Chat 方向应以 [DONE] 结束，实际结尾：%q", tailOf(raw, 40))
	}
	// usage chunk：Chat 的 prompt_tokens 含缓存命中（这里没有缓存，等于 input_tokens）。
	chatUsage := chatStreamUsage(t, chunks)
	if got, _ := asInt(chatUsage["prompt_tokens"]); got != 11 {
		t.Fatalf("prompt_tokens = %v, want 11", chatUsage["prompt_tokens"])
	}
	if got, _ := asInt(chatUsage["completion_tokens"]); got != 5 {
		t.Fatalf("completion_tokens = %v, want 5", chatUsage["completion_tokens"])
	}
	// 返回的 usage 是**上游**口径（Anthropic：input 不含缓存命中）。
	if usage.InputTokens != 11 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestMessagesToChatStreamToolsAndReasoning(t *testing.T) {
	raw, usage := runStreamFrom(t, newConvSession(), FormatOpenAIChat, FormatAnthropic, "claude-x",
		anthropicToolStream, []int{len(anthropicToolStream)})
	chunks := chatChunkPayloads(t, raw)

	if got := chatReasoningText(t, chunks); got != "pondering" {
		t.Fatalf("thinking_delta → reasoning_content 失败：%q", got)
	}
	calls := chatToolCalls(t, chunks)
	if len(calls) != 1 {
		t.Fatalf("应有 1 个工具调用，实际 %#v", calls)
	}
	call := calls[0]
	if call["id"] != "toolu_1" || call["name"] != "get_weather" {
		t.Fatalf("tool_call = %#v", call)
	}
	if call["arguments"] != `{"loc":"SF"}` {
		t.Fatalf("arguments 分片未拼成完整 JSON：%q", call["arguments"])
	}
	if reasons := chatFinishReasons(t, chunks); len(reasons) != 1 || reasons[0] != "tool_calls" {
		t.Fatalf("stop_reason=tool_use → finish_reason 应为 tool_calls，实际 %v", reasons)
	}
	if usage.OutputTokens != 8 {
		t.Fatalf("usage.OutputTokens = %d, want 8", usage.OutputTokens)
	}
	// 签名在 Chat 侧无处安放：不应出现在任何 delta 里。
	if strings.Contains(string(raw), "signature") {
		t.Fatalf("Chat 事件流里不应出现 signature：\n%s", raw)
	}
}

// ==================== responses → chat ====================

func TestResponsesToChatStream(t *testing.T) {
	raw, usage := runStreamFrom(t, newConvSession(), FormatOpenAIChat, FormatResponses, "gpt-5",
		responsesTextStream, []int{len(responsesTextStream)})
	chunks := chatChunkPayloads(t, raw)

	if got := chatDeltaText(t, chunks); got != "Hello, world!" {
		t.Fatalf("正文拼接 = %q", got)
	}
	if reasons := chatFinishReasons(t, chunks); len(reasons) != 1 || reasons[0] != "stop" {
		t.Fatalf("finish_reason = %v, want [stop]", reasons)
	}
	if !strings.HasSuffix(string(raw), "data: [DONE]\n\n") {
		t.Fatalf("Chat 方向应以 [DONE] 结束")
	}
	chatUsage := chatStreamUsage(t, chunks)
	if got, _ := asInt(chatUsage["prompt_tokens"]); got != 11 {
		t.Fatalf("prompt_tokens = %v", chatUsage["prompt_tokens"])
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestResponsesToChatStreamToolsAndReasoning(t *testing.T) {
	raw, usage := runStreamFrom(t, newConvSession(), FormatOpenAIChat, FormatResponses, "gpt-5",
		responsesToolStream, []int{len(responsesToolStream)})
	chunks := chatChunkPayloads(t, raw)

	if got := chatReasoningText(t, chunks); got != "pondering" {
		t.Fatalf("reasoning_summary_text.delta → reasoning_content 失败：%q", got)
	}
	calls := chatToolCalls(t, chunks)
	if len(calls) != 1 {
		t.Fatalf("应有 1 个工具调用，实际 %#v", calls)
	}
	if calls[0]["id"] != "call_1" || calls[0]["name"] != "get_weather" {
		t.Fatalf("tool_call = %#v", calls[0])
	}
	if calls[0]["arguments"] != `{"loc":"SF"}` {
		t.Fatalf("arguments = %q", calls[0]["arguments"])
	}
	if reasons := chatFinishReasons(t, chunks); len(reasons) != 1 || reasons[0] != "tool_calls" {
		t.Fatalf("有工具调用时 finish_reason 应为 tool_calls，实际 %v", reasons)
	}
	if usage.InputTokens != 20 || usage.OutputTokens != 8 {
		t.Fatalf("usage = %+v", usage)
	}
}

// TestResponsesToChatStreamOnlyFinalEvent 覆盖「上游没有逐段下发正文、只在终局对象里给全文」
// 的情况：必须兜底把正文补出来，否则客户端会拿到空回复。
func TestResponsesToChatStreamOnlyFinalEvent(t *testing.T) {
	stream := `event: response.created` + "\n" +
		`data: {"type":"response.created","response":{"id":"resp_9","object":"response","status":"in_progress","model":"gpt-up","output":[]}}` + "\n\n" +
		`event: response.completed` + "\n" +
		`data: {"type":"response.completed","response":{"id":"resp_9","object":"response","status":"completed","model":"gpt-up","output":[{"id":"msg_9","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"buffered answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

	raw, _ := runStreamFrom(t, newConvSession(), FormatOpenAIChat, FormatResponses, "gpt-5", stream, []int{len(stream)})
	chunks := chatChunkPayloads(t, raw)
	if got := chatDeltaText(t, chunks); got != "buffered answer" {
		t.Fatalf("只给终局事件时应兜底补出正文，实际 %q", got)
	}
	if reasons := chatFinishReasons(t, chunks); len(reasons) != 1 || reasons[0] != "stop" {
		t.Fatalf("finish_reason = %v", reasons)
	}
}

// ==================== 不变式 ====================

// TestReverseStreamChunkBoundaryInvariance 固定与既有方向同样的不变式：
// 同一段上游字节流按任意 chunk 边界切开，输出必须逐字节一致。
func TestReverseStreamChunkBoundaryInvariance(t *testing.T) {
	cases := []struct {
		name    string
		client  ApiFormat
		channel ApiFormat
		payload string
	}{
		{"messages → chat", FormatOpenAIChat, FormatAnthropic, anthropicTextStream},
		{"messages → chat（工具+推理）", FormatOpenAIChat, FormatAnthropic, anthropicToolStream},
		{"responses → chat", FormatOpenAIChat, FormatResponses, responsesTextStream},
		{"responses → chat（工具+推理）", FormatOpenAIChat, FormatResponses, responsesToolStream},
		{"messages → chat → responses（两跳）", FormatResponses, FormatAnthropic, anthropicTextStream},
		{"responses → chat → messages（两跳）", FormatAnthropic, FormatResponses, responsesTextStream},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := []byte(tc.payload)
			baseline, baselineUsage := runStreamFrom(t, newConvSession(), tc.client, tc.channel, "m",
				tc.payload, []int{len(full)})
			if len(baseline) == 0 {
				t.Fatalf("基线输出为空")
			}

			// id 是一次性生成的随机值（genID），比较前归一化——与既有方向同一套做法。
			// created 在 Anthropic 上游没有对应字段时取当前时间，同样归一化。
			want := normalizeVolatile(baseline)

			// 按每个字节偏移切一刀（首片大小 = offset）。
			for offset := 1; offset < len(full); offset++ {
				got, gotUsage := runStreamFrom(t, newConvSession(), tc.client, tc.channel, "m",
					tc.payload, []int{offset})
				if !bytes.Equal(normalizeVolatile(got), want) {
					t.Fatalf("offset %d 输出与基线不一致\n实际: %s\n基线: %s", offset, got, baseline)
				}
				if gotUsage != baselineUsage {
					t.Fatalf("offset %d usage 不一致：%+v vs %+v", offset, gotUsage, baselineUsage)
				}
			}

			// 逐字节喂（最坏情况）。
			sizes := make([]int, len(full))
			for i := range sizes {
				sizes[i] = 1
			}
			got, _ := runStreamFrom(t, newConvSession(), tc.client, tc.channel, "m", tc.payload, sizes)
			if !bytes.Equal(normalizeVolatile(got), want) {
				t.Fatalf("逐字节喂输出与基线不一致\n实际: %s\n基线: %s", got, baseline)
			}
		})
	}
}

// TestTwoHopStreamUsageStaysUpstream 两跳的 usage 必须是上游真实值：
// responses 渠道 → chat → messages 客户端时，message_delta 上的数值应等于上游。
func TestTwoHopStreamUsageStaysUpstream(t *testing.T) {
	raw, usage := runStreamFrom(t, newConvSession(), FormatAnthropic, FormatResponses, "m",
		responsesTextStream, []int{len(responsesTextStream)})

	events := sse.Events(string(raw))
	var deltaUsage map[string]any
	for _, ev := range events {
		payload := mustObject(t, ev.Data)
		if payload["type"] != "message_delta" {
			continue
		}
		deltaUsage, _ = asMap(payload["usage"])
	}
	if deltaUsage == nil {
		t.Fatalf("两跳后应产出 message_delta 且带 usage")
	}
	if got, _ := asInt(deltaUsage["input_tokens"]); got != 11 {
		t.Fatalf("message_delta.input_tokens = %v, want 11（上游真实值）", deltaUsage["input_tokens"])
	}
	if got, _ := asInt(deltaUsage["output_tokens"]); got != 5 {
		t.Fatalf("message_delta.output_tokens = %v, want 5", deltaUsage["output_tokens"])
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 5 {
		t.Fatalf("返回 usage = %+v", usage)
	}
}

// volatileFieldMatcher 匹配 Chat chunk 里随处理时刻变化的 created 字段。
var volatileFieldMatcher = regexp.MustCompile(`"created":\d+`)

// normalizeVolatile 把「一次性生成」的字段归一化，便于比较两次运行的字节输出：
// 随机 id（genID）与取当前时间的 created。
func normalizeVolatile(raw []byte) []byte {
	return volatileFieldMatcher.ReplaceAll(normalizeIDs(raw), []byte(`"created":0`))
}

// tailOf 返回字节串结尾的 n 个字节（用于失败信息）。
func tailOf(raw []byte, n int) string {
	if len(raw) <= n {
		return string(raw)
	}
	return string(raw[len(raw)-n:])
}
