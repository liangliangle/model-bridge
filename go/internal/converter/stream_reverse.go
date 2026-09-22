package converter

import (
	"strings"
	"time"

	"modelbridge/internal/sse"
)

// stream_reverse.go —— **流式方向的逆向转换：富协议上游 → Chat 事件流**。
//
//	messagesToChatStream  ：Anthropic Messages 事件流 → Chat chunk 流
//	responsesToChatStream ：OpenAI Responses 事件流 → Chat chunk 流
//
// 它们是 stream.go 里 chatToAnthropicStream / chatToResponsesStream 的逆，
// 服务于「Chat 客户端 ← 富协议渠道」，以及经 Chat 的两跳（如 messages 客户端 ← responses 渠道，
// 此时它们是流水线的第一个阶段）。
//
// 两条都遵守既有流式约定（见 stream.go 顶部）：
//   - 首个 chunk 带 role；
//   - 终局 chunk（finish_reason）与 usage chunk 推迟到收尾，因为富协议的 usage 也只在末尾出现；
//   - 不写 `data: [DONE]`——那是流水线在「客户端是 Chat」时统一补的（见 pipeline.go）。

// chatChunkFrame 组装一个 Chat SSE 帧（delta + finish_reason）。
func chatChunkFrame(id, model string, created int64, delta map[string]any, finish any) []byte {
	return sseFrame("", map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	})
}

// chatUsageFrame 组装 Chat 的 usage-only 帧（choices 为空数组，与 OpenAI 的收尾帧同形）。
func chatUsageFrame(id, model string, created int64, usage map[string]any) []byte {
	return sseFrame("", map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{},
		"usage":   usage,
	})
}

// chatToolCallDelta 组装 Chat 的工具调用增量（首帧带 id/name，后续帧只带 arguments 分片）。
func chatToolCallDelta(index int, callID, name, arguments string) map[string]any {
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if arguments != "" {
		fn["arguments"] = arguments
	}
	call := map[string]any{"index": index, "function": fn}
	if callID != "" {
		call["id"] = callID
		call["type"] = "function"
	}
	return map[string]any{"tool_calls": []any{call}}
}

// ==================== Anthropic Messages 上游 → Chat ====================

// messagesToChatStream 把 Anthropic Messages 事件流翻译成 Chat chunk 流。
//
// 事件映射：
//
//	message_start              → 记 id / model / 输入 usage（首个 chunk 补 role）
//	content_block_start        → tool_use 块转成 tool_calls 首帧（带 id / name）
//	content_block_delta        → text_delta → content；thinking_delta → reasoning_content；
//	                             input_json_delta → tool_calls[].function.arguments 分片
//	content_block_stop         → 无需输出
//	message_delta              → 记 stop_reason 与输出 usage（终局推迟到收尾）
//	message_stop               → 无需输出
type messagesToChatStream struct {
	id      string
	model   string
	created int64

	started bool

	// toolIndex：Anthropic 内容块下标 → Chat 的 tool_calls 下标。
	toolIndex map[int]int
	nextTool  int

	pendingStop string
	acc         streamUsage
}

func newMessagesToChatStream(model string) streamConverter {
	return &messagesToChatStream{
		id:        genID("chatcmpl-"),
		model:     model,
		created:   time.Now().Unix(),
		toolIndex: map[int]int{},
	}
}

// ensureStarted 输出首个 chunk（带 role），幂等。
func (s *messagesToChatStream) ensureStarted(out *[][]byte) {
	if s.started {
		return
	}
	s.started = true
	*out = append(*out, chatChunkFrame(s.id, s.model, s.created, map[string]any{"role": "assistant"}, nil))
}

func (s *messagesToChatStream) processEvent(ev sse.Event) [][]byte {
	if ev.Data == sse.Done {
		return s.finalize()
	}
	payload, err := decodeObject([]byte(ev.Data))
	if err != nil {
		return nil
	}

	var out [][]byte
	switch t, _ := asString(payload["type"]); t {
	case "message_start":
		if message, ok := asMap(payload["message"]); ok {
			if id, ok := asString(message["id"]); ok && id != "" {
				s.id = id
			}
			if model, ok := asString(message["model"]); ok && model != "" {
				s.model = model
			}
			if usage, ok := asMap(message["usage"]); ok {
				s.acc.mergeAnthropic(usage)
			}
		}

	case "content_block_start":
		index := intOr(payload["index"], 0)
		block, _ := asMap(payload["content_block"])
		if blockType, _ := asString(block["type"]); blockType == "tool_use" {
			callID, _ := asString(block["id"])
			if callID == "" {
				callID = genID("call_")
			}
			name, _ := asString(block["name"])
			s.toolIndex[index] = s.nextTool
			s.nextTool++
			s.ensureStarted(&out)
			out = append(out, sseFrame("", chatChunkFramePayload(s.id, s.model, s.created,
				chatToolCallDelta(s.toolIndex[index], callID, name, ""), nil)))
		}

	case "content_block_delta":
		index := intOr(payload["index"], 0)
		delta, _ := asMap(payload["delta"])
		deltaType, _ := asString(delta["type"])
		switch deltaType {
		case "text_delta":
			if text, _ := asString(delta["text"]); text != "" {
				s.ensureStarted(&out)
				out = append(out, chatChunkFrame(s.id, s.model, s.created, map[string]any{"content": text}, nil))
			}
		case "thinking_delta":
			if thinking, _ := asString(delta["thinking"]); thinking != "" {
				s.ensureStarted(&out)
				out = append(out, chatChunkFrame(s.id, s.model, s.created, map[string]any{"reasoning_content": thinking}, nil))
			}
		case "signature_delta":
			// 签名在 Chat 侧无处安放（有损，已在文档的「有损清单」里说明）。
		case "input_json_delta":
			fragment, _ := asString(delta["partial_json"])
			if fragment == "" {
				break
			}
			toolIndex, known := s.toolIndex[index]
			if !known {
				// 上游没给 content_block_start（异常流）：按顺序补一个下标，避免参数丢失。
				toolIndex = s.nextTool
				s.nextTool++
				s.toolIndex[index] = toolIndex
			}
			s.ensureStarted(&out)
			out = append(out, sseFrame("", chatChunkFramePayload(s.id, s.model, s.created,
				chatToolCallDelta(toolIndex, "", "", fragment), nil)))
		}

	case "message_delta":
		if delta, ok := asMap(payload["delta"]); ok {
			if stop, ok := asString(delta["stop_reason"]); ok && stop != "" {
				s.pendingStop = stop
			}
		}
		if usage, ok := asMap(payload["usage"]); ok {
			s.acc.mergeAnthropic(usage)
		}
	}

	return out
}

func (s *messagesToChatStream) finalize() [][]byte {
	var out [][]byte
	s.ensureStarted(&out)
	out = append(out, chatChunkFrame(s.id, s.model, s.created, map[string]any{}, finishReasonOf(s.pendingStop)))
	out = append(out, chatUsageFrame(s.id, s.model, s.created, s.acc.toChatUsage()))
	return out
}

func (s *messagesToChatStream) usage() Usage { return s.acc.usage() }

// finishReasonOf 把 Anthropic 的 stop_reason 映射为 Chat 的 finish_reason（缺省 stop）。
func finishReasonOf(stopReason string) string {
	if stopReason == "" {
		return "stop"
	}
	return anthropicStopToFinish(stopReason)
}

// chatChunkFramePayload 与 chatChunkFrame 同形，但接受已构造好的 delta 对象
// （工具调用增量的形状不适合再包一层）。
func chatChunkFramePayload(id, model string, created int64, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
}

// ==================== OpenAI Responses 上游 → Chat ====================

// responsesToChatStream 把 Responses 事件流翻译成 Chat chunk 流。
//
// 事件映射（Responses 的事件模型最丰富，这里按「增量事件直译 + 终局推迟」处理）：
//
//	response.created                        → 记 id / model
//	response.output_item.added（function_call）→ tool_calls 首帧（带 call_id / name）
//	response.output_text.delta              → content
//	response.reasoning_summary_text.delta   → reasoning_content
//	response.reasoning_text.delta           → reasoning_content
//	response.function_call_arguments.delta  → tool_calls[].function.arguments 分片
//	response.custom_tool_call_input.delta   → 同上（custom 工具按 function 工具承载）
//	response.completed / incomplete / failed→ 记 status 与 usage（终局推迟到收尾）
type responsesToChatStream struct {
	id      string
	model   string
	created int64

	started bool
	// emittedContent 记录是否已经下发过正文，用于兜底「上游只给终局事件」的情况。
	emittedContent bool

	// toolIndex：Responses 的 output_index → Chat 的 tool_calls 下标。
	toolIndex map[int]int
	nextTool  int

	status string
	acc    streamUsage
}

func newResponsesToChatStream(model string) streamConverter {
	return &responsesToChatStream{
		id:        genID("chatcmpl-"),
		model:     model,
		created:   time.Now().Unix(),
		toolIndex: map[int]int{},
	}
}

func (s *responsesToChatStream) ensureStarted(out *[][]byte) {
	if s.started {
		return
	}
	s.started = true
	*out = append(*out, chatChunkFrame(s.id, s.model, s.created, map[string]any{"role": "assistant"}, nil))
}

func (s *responsesToChatStream) processEvent(ev sse.Event) [][]byte {
	if ev.Data == sse.Done {
		return s.finalize()
	}
	payload, err := decodeObject([]byte(ev.Data))
	if err != nil {
		return nil
	}

	var out [][]byte
	switch t, _ := asString(payload["type"]); t {
	case "response.created", "response.in_progress":
		if resp, ok := asMap(payload["response"]); ok {
			if id, ok := asString(resp["id"]); ok && id != "" {
				s.id = id
			}
			if model, ok := asString(resp["model"]); ok && model != "" {
				s.model = model
			}
			// 上游给了创建时间就沿用（Chat 的 created 语义就是响应创建时间），
			// 这样转换结果可复现，而不是随我们的处理时刻漂移。
			s.created = chatCreatedFrom(resp, s.created)
		}

	case "response.output_item.added":
		item, _ := asMap(payload["item"])
		switch itemType, _ := asString(item["type"]); itemType {
		case "function_call", "custom_tool_call":
			outputIndex := intOr(payload["output_index"], 0)
			callID, _ := asString(item["call_id"])
			if callID == "" {
				callID, _ = asString(item["id"])
			}
			if callID == "" {
				callID = genID("call_")
			}
			name, _ := asString(item["name"])
			s.toolIndex[outputIndex] = s.nextTool
			s.nextTool++
			s.ensureStarted(&out)
			out = append(out, sseFrame("", chatChunkFramePayload(s.id, s.model, s.created,
				chatToolCallDelta(s.toolIndex[outputIndex], callID, name, ""), nil)))
		}

	case "response.output_text.delta":
		if text, _ := asString(payload["delta"]); text != "" {
			s.ensureStarted(&out)
			s.emittedContent = true
			out = append(out, chatChunkFrame(s.id, s.model, s.created, map[string]any{"content": text}, nil))
		}

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if thinking, _ := asString(payload["delta"]); thinking != "" {
			s.ensureStarted(&out)
			out = append(out, chatChunkFrame(s.id, s.model, s.created, map[string]any{"reasoning_content": thinking}, nil))
		}

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		fragment, _ := asString(payload["delta"])
		if fragment == "" {
			break
		}
		outputIndex := intOr(payload["output_index"], 0)
		toolIndex, known := s.toolIndex[outputIndex]
		if !known {
			toolIndex = s.nextTool
			s.nextTool++
			s.toolIndex[outputIndex] = toolIndex
		}
		s.ensureStarted(&out)
		out = append(out, sseFrame("", chatChunkFramePayload(s.id, s.model, s.created,
			chatToolCallDelta(toolIndex, "", "", fragment), nil)))

	case "response.completed", "response.incomplete", "response.failed":
		if resp, ok := asMap(payload["response"]); ok {
			if status, ok := asString(resp["status"]); ok && status != "" {
				s.status = status
			}
			if usage, ok := asMap(resp["usage"]); ok {
				s.acc.mergeResponses(usage)
			}
			// 兜底：上游没有逐段下发正文（只给了终局对象）时，把正文补出来，
			// 否则 Chat 客户端会拿到一个空回复。
			if !s.emittedContent {
				if text := outputTextOf(resp); text != "" {
					s.ensureStarted(&out)
					s.emittedContent = true
					out = append(out, chatChunkFrame(s.id, s.model, s.created, map[string]any{"content": text}, nil))
				}
			}
		}
	}

	return out
}

func (s *responsesToChatStream) finalize() [][]byte {
	var out [][]byte
	s.ensureStarted(&out)
	finish := statusToFinishReason(s.status)
	if s.nextTool > 0 {
		finish = "tool_calls"
	}
	out = append(out, chatChunkFrame(s.id, s.model, s.created, map[string]any{}, finish))
	out = append(out, chatUsageFrame(s.id, s.model, s.created, s.acc.toChatUsage()))
	return out
}

func (s *responsesToChatStream) usage() Usage { return s.acc.usage() }

// outputTextOf 拼接 Responses 响应对象里所有 message 项的 output_text（忽略推理标记的 part）。
func outputTextOf(resp map[string]any) string {
	items, ok := asArray(resp["output"])
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, raw := range items {
		item, ok := asMap(raw)
		if !ok {
			continue
		}
		if itemType, _ := asString(item["type"]); itemType != "message" {
			continue
		}
		parts, ok := asArray(item["content"])
		if !ok {
			continue
		}
		for _, praw := range parts {
			part, ok := asMap(praw)
			if !ok {
				continue
			}
			if partHasReasoningAnnotation(part) {
				continue
			}
			if text, ok := asString(part["text"]); ok {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}
