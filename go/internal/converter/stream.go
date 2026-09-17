package converter

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// 本文件实现**流式方向**（对应 Rust converter/stream.rs）：
//
//   - scanSSE                              SSE 分帧（ocgo main.go::readSSE，line 2856）
//   - chatToAnthropicStream                Chat 上游流 → Anthropic 事件流
//     （Rust stream.rs::ChatToAnthropicStream，line 1287）
//   - responsesEmitter + chatToResponsesStream
//     Chat 上游流 → Responses 事件流
//     （Rust stream.rs::ResponsesEmitter line 162 + ChatCompletionsToResponsesStream line 616）
//   - replayChat / replayAnthropic / replayResponses
//     完整响应对象重放为 SSE（Rust stream.rs::replay_response_as_sse，line 1884）
//
// 两条流都遵守同一条时序约定（converter.go 顶部「两者冲突时以 Rust 契约为准」第 5 条）：
// 终局事件推迟到 [DONE] / EOF，因为 OpenAI 的 usage-only chunk（choices:[]）位于
// finish_reason 之后、[DONE] 之前；提前收尾会把上游真实 usage 丢掉。

// ==================== SSE 分帧 ====================

// sseEvent 是一个已分帧的 SSE 事件：事件名（可能为空）与 data 载荷。
// 对应 Rust stream.rs::SseEvent（line 51）。
type sseEvent struct {
	// Event 是 `event:` 行的值，未给出时为空串。
	Event string
	// Data 是全部 `data:` 行按 "\n" 拼接后的载荷（可能为 "[DONE]"）。
	Data string
}

// scanSSE 按行扫描 SSE 并逐个交付完整事件。
//
// 移植自 ocgo main.go::readSSE（line 2856），保留其全部语义：
//   - 空行结束一个事件；只有 `data:` 行的事件才交付（纯 event 行不成事件）；
//   - `data:` 行按行拼接为载荷；
//   - 载荷为 `[DONE]` 时结束扫描，且**不**交付给 handle（收尾由调用方的 finalize 负责）；
//   - handle 返回错误时立即中止并原样返回该错误。
//
// 与 ocgo 的差异：ocgo 用 bufio.Scanner（单 token 上限 64KB，且按 chunk 读），
// 这里用 bufio.Reader 按行缓冲，事件被任意 chunk 边界切开都能正确组帧
// （对应 Rust stream.rs::SseBuffer（line 46）跨 chunk 的健壮性）。
func scanSSE(r io.Reader, handle func(sseEvent) error) error {
	br := bufio.NewReader(r)
	event := ""
	var data []string

	// flush 交付一个完整事件；返回 (是否结束扫描, 错误)。
	flush := func() (bool, error) {
		if len(data) == 0 {
			event = ""
			return false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		ev := sseEvent{Event: event, Data: payload}
		event = ""
		if payload == "[DONE]" {
			return true, nil
		}
		if err := handle(ev); err != nil {
			return true, err
		}
		return false, nil
	}

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\n")
			trimmed = strings.TrimRight(trimmed, "\r")
			switch {
			case trimmed == "":
				stop, herr := flush()
				if herr != nil {
					return herr
				}
				if stop {
					return nil
				}
			case strings.HasPrefix(trimmed, "event:"):
				event = strings.TrimSpace(trimmed[len("event:"):])
			case strings.HasPrefix(trimmed, "data:"):
				data = append(data, strings.TrimSpace(trimmed[len("data:"):]))
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if _, herr := flush(); herr != nil {
		return herr
	}
	return nil
}

// streamConverter 是一条流式方向的状态机。
// 对应 Rust stream.rs::StreamConverter（line 15，`process_chunk` 换成按事件驱动）。
type streamConverter interface {
	// processEvent 处理一个上游事件，返回若干条已编码的下游 SSE 字节块。
	processEvent(ev sseEvent) [][]byte
	// finalize 是流结束时的收尾（补齐终局事件，并带上已累加的真实 usage）。
	finalize() [][]byte
	// usage 返回累计观察到的**上游** usage（Anthropic 口径，见 Usage）。
	usage() Usage
}

// sseFrame 编码一个下游 SSE 事件：`event: <name>\ndata: <json>\n\n`。
//
// 事件名为空时只写 data 行。Rust stream.rs::sse_line("") 会多写一行空的 `event: `，
// 这里按「同一条实时流应有的形态」省略该行：Chat 上游的实时帧本来就没有 event 行
// （见 ocgo 的 `data: ...` 帧与 OpenAI 官方流），下游 SDK 也按 data 行解析。
func sseFrame(event string, data any) []byte {
	payload, err := marshalJSON(data)
	if err != nil {
		payload = []byte("{}")
	}
	if event == "" {
		return []byte("data: " + string(payload) + "\n\n")
	}
	return []byte("event: " + event + "\ndata: " + string(payload) + "\n\n")
}

// ==================== Chat 上游 → Anthropic 事件流 ====================

// chatToAnthropicStream 把 Chat 上游的流翻译成 Anthropic Messages 事件流。
//
// 对应 Rust stream.rs::ChatToAnthropicStream（line 1287），事件顺序固定为：
//
//	message_start → (content_block_start / content_block_delta / content_block_stop)*
//	→ message_delta（携带 usage 与 stop_reason）→ message_stop
//
// 工具调用发出 tool_use 内容块 + input_json_delta 片段。
// 收尾（message_delta/message_stop）推迟到 [DONE] / finalize：上游 usage chunk
// 位于 finish_reason 之后，提前收尾会丢掉真实 usage。
type chatToAnthropicStream struct {
	id    string
	model string

	started bool
	stopped bool

	// nextIndex 是下一个内容块下标；openBlock 是当前未闭合的块。
	nextIndex      int
	openBlock      *int
	textIndex      *int
	reasoningIndex *int
	// toolBlocks：Chat 的 tool_calls[].index → 内容块下标。
	toolBlocks map[int]int
	hasTool    bool

	// pendingStop 暂存 finish_reason 推导出的 stop_reason，收尾时才写出。
	pendingStop string

	// acc 是上游 usage 累加（Anthropic 口径）。
	acc streamUsage
	// reasoning/toolCallIDs 仅用于 ocgo main.go::cacheReasoningContent 的回显缓存。
	reasoning   strings.Builder
	toolCallIDs []string
}

// newChatToAnthropicStream 创建 chat → messages 的流式转换器。
// 对应 Rust stream.rs::ChatToAnthropicStream::new（model 由调用方给出，
// 为空时回退到上游首个 chunk 里的 model）。
func newChatToAnthropicStream(model string) streamConverter {
	return &chatToAnthropicStream{
		id:         genID("msg_"),
		model:      model,
		toolBlocks: map[int]int{},
	}
}

// ensureStarted 发出 message_start（幂等）。
// 对应 Rust stream.rs::ChatToAnthropicStream::ensure_started（line 1336）。
func (s *chatToAnthropicStream) ensureStarted(out *[][]byte) {
	if s.started {
		return
	}
	s.started = true
	*out = append(*out, sseFrame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          s.id,
			"type":        "message",
			"role":        "assistant",
			"model":       s.model,
			"content":     []any{},
			"stop_reason": nil,
			"usage":       s.acc.toAnthropicMessageUsage(),
		},
	}))
}

// closeOpenBlock 关闭当前打开的内容块（幂等）。
// 对应 Rust stream.rs::ChatToAnthropicStream::close_open_block（line 1358）。
func (s *chatToAnthropicStream) closeOpenBlock(out *[][]byte) {
	if s.openBlock == nil {
		return
	}
	index := *s.openBlock
	s.openBlock = nil
	*out = append(*out, sseFrame("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": index,
	}))
}

// ensureTextBlock 打开（或复用）text 内容块。对应 Rust 同名方法（line 1367）。
func (s *chatToAnthropicStream) ensureTextBlock(out *[][]byte) {
	s.ensureStarted(out)
	if s.openBlock != nil && s.textIndex != nil && *s.openBlock == *s.textIndex {
		return
	}
	s.closeOpenBlock(out)
	index := 0
	if s.textIndex != nil {
		index = *s.textIndex
	} else {
		index = s.nextIndex
		s.nextIndex++
		s.textIndex = &index
	}
	*out = append(*out, sseFrame("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	s.openBlock = &index
}

// ensureReasoningBlock 打开（或复用）thinking 内容块。对应 Rust 同名方法（line 1393）。
func (s *chatToAnthropicStream) ensureReasoningBlock(out *[][]byte) {
	s.ensureStarted(out)
	if s.reasoningIndex != nil && s.openBlock != nil && *s.openBlock == *s.reasoningIndex {
		return
	}
	s.closeOpenBlock(out)
	index := 0
	if s.reasoningIndex != nil {
		index = *s.reasoningIndex
	} else {
		index = s.nextIndex
		s.nextIndex++
		s.reasoningIndex = &index
	}
	*out = append(*out, sseFrame("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	}))
	s.openBlock = &index
}

// processEvent 处理一个上游 Chat 事件。对应 Rust
// stream.rs::ChatToAnthropicStream::process_chunk（line 1420）。
func (s *chatToAnthropicStream) processEvent(ev sseEvent) [][]byte {
	var out [][]byte
	if ev.Data == "[DONE]" {
		s.finishInto(&out, s.stopReason())
		return out
	}
	chunk, err := decodeObject([]byte(ev.Data))
	if err != nil {
		// 非法 JSON / 非对象载荷：跳过（与 Rust `serde_json::from_str(...).Err(_) => continue` 一致）。
		return nil
	}
	if s.model == "" {
		if model, ok := asString(chunk["model"]); ok {
			s.model = model
		}
	}
	// usage chunk 的 choices 为空数组，必须在取 choice 之前捕获。
	if usage, ok := asMap(chunk["usage"]); ok {
		s.acc.mergeChat(usage)
	}

	choice := firstChatChoice(chunk)
	var delta map[string]any
	if choice != nil {
		delta, _ = asMap(choice["delta"])
	}
	if delta != nil {
		if reasoning, _ := asString(delta["reasoning_content"]); reasoning != "" {
			s.ensureReasoningBlock(&out)
			index := 0
			if s.reasoningIndex != nil {
				index = *s.reasoningIndex
			}
			s.reasoning.WriteString(reasoning)
			out = append(out, sseFrame("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
			}))
		}
		if content, _ := asString(delta["content"]); content != "" {
			s.ensureTextBlock(&out)
			index := 0
			if s.textIndex != nil {
				index = *s.textIndex
			}
			out = append(out, sseFrame("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "text_delta", "text": content},
			}))
		}
		if toolCalls, ok := asArray(delta["tool_calls"]); ok {
			for _, raw := range toolCalls {
				call, ok := asMap(raw)
				if !ok {
					continue
				}
				toolIndex := intOr(call["index"], 0)
				function, _ := asMap(call["function"])
				if _, exists := s.toolBlocks[toolIndex]; !exists {
					s.closeOpenBlock(&out)
					s.ensureStarted(&out)
					blockIndex := s.nextIndex
					s.nextIndex++
					s.toolBlocks[toolIndex] = blockIndex
					s.hasTool = true
					callID, _ := asString(call["id"])
					if callID == "" {
						callID = genID("call_")
					}
					name := ""
					if function != nil {
						name, _ = asString(function["name"])
					}
					s.toolCallIDs = append(s.toolCallIDs, callID)
					out = append(out, sseFrame("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": blockIndex,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    callID,
							"name":  name,
							"input": map[string]any{},
						},
					}))
					s.openBlock = &blockIndex
				}
				if function != nil {
					if args, _ := asString(function["arguments"]); args != "" {
						if blockIndex, exists := s.toolBlocks[toolIndex]; exists {
							out = append(out, sseFrame("content_block_delta", map[string]any{
								"type":  "content_block_delta",
								"index": blockIndex,
								"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
							}))
						}
					}
				}
			}
		}
	}
	if choice != nil {
		// 暂存结束原因，推迟到 [DONE] / finalize 收尾，以便带上真实 usage。
		if raw, present := choice["finish_reason"]; present {
			if finishReason, ok := asString(raw); ok {
				s.pendingStop = finishToAnthropicStop(finishReason)
			}
		}
	}
	return out
}

// stopReason 给出收尾用的 stop_reason：上游未给 finish_reason 时退化为 end_turn。
func (s *chatToAnthropicStream) stopReason() string {
	if s.pendingStop != "" {
		return s.pendingStop
	}
	return "end_turn"
}

// finishInto 补齐 message_delta + message_stop（幂等）。
// 对应 Rust stream.rs::ChatToAnthropicStream::finish_into（line 1551）。
func (s *chatToAnthropicStream) finishInto(out *[][]byte, stopReason string) {
	if s.stopped {
		return
	}
	s.stopped = true
	s.ensureStarted(out)
	s.closeOpenBlock(out)
	// 出现过工具调用而 stop_reason 仍是 end_turn 时，按契约报 tool_use。
	if s.hasTool && stopReason == "end_turn" {
		stopReason = "tool_use"
	}
	*out = append(*out, sseFrame("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": s.acc.toAnthropicDeltaUsage(),
	}))
	*out = append(*out, sseFrame("message_stop", map[string]any{"type": "message_stop"}))
	// 对照 ocgo main.go::streamAnthropic 收尾处的 cacheReasoningContent：
	// 记下本次工具调用的 reasoning，供后续 anthropic→chat 请求回显。
	cacheReasoningContent(s.toolCallIDs, s.reasoning.String())
}

// finalize 在流结束（[DONE] 或 EOF）时收尾。
func (s *chatToAnthropicStream) finalize() [][]byte {
	var out [][]byte
	s.finishInto(&out, s.stopReason())
	return out
}

// usage 返回累计的上游 usage（Anthropic 口径）。
func (s *chatToAnthropicStream) usage() Usage { return s.acc.usage() }

// ==================== Chat 上游 → Responses 事件流 ====================

// partKind 区分 Responses 里两种 output_text part。
type partKind int

const (
	partText partKind = iota
	partReasoning
)

// toolCallState 是流式 function_call 的累积状态。
// 对应 Rust stream.rs::ToolCallState（line 150）。
type toolCallState struct {
	callID string
	// name 是**写出用**的名字：namespace 子工具还原为 subtool，custom 工具保持原名。
	name      string
	namespace string
	custom    bool

	outputIndex int
	args        string
	done        bool
}

// item 构造该项的 output item；arguments 由调用方决定（新增时为空串，收尾时为 "{}" 兜底）。
func (t *toolCallState) item(status, arguments string) map[string]any {
	var item map[string]any
	if t.custom {
		item = map[string]any{
			"type":    "custom_tool_call",
			"id":      t.callID,
			"call_id": t.callID,
			"name":    t.name,
			"input":   unwrapCustomToolArguments(arguments),
			"status":  status,
		}
	} else {
		item = map[string]any{
			"type":      "function_call",
			"id":        t.callID,
			"call_id":   t.callID,
			"name":      t.name,
			"arguments": arguments,
			"status":    status,
		}
	}
	if t.namespace != "" {
		item["namespace"] = t.namespace
	}
	return item
}

// argumentsOrDefault 返回累积参数，空时用 "{}" 兜底（对应 Rust `if t.args.is_empty()`）。
func (t *toolCallState) argumentsOrDefault() string {
	if t.args == "" {
		return "{}"
	}
	return t.args
}

// responsesEmitter 是生成 Responses 事件的共享状态机。
// 对应 Rust stream.rs::ResponsesEmitter（line 162）。
//
// 事件家族与顺序（契约第 6 条）：
//
//	response.created → response.in_progress
//	→ (message) response.output_item.added → response.content_part.added
//	   → response.output_text.delta* → response.output_text.done → response.content_part.done
//	   → response.output_item.done
//	→ (tool) response.output_item.added → response.function_call_arguments.delta*
//	   → response.function_call_arguments.done → response.output_item.done
//	→ response.completed（response.usage **恒在**，未知时为零值对象）
type responsesEmitter struct {
	responseID string
	itemID     string
	model      string
	status     string
	seq        uint64

	sentCreated     bool
	sentItemAdded   bool
	anyPartOpened   bool
	contentIndex    int
	hasPartOpen     bool
	partOpen        partKind
	currentText     string
	accumulatedText string
	// accumulatedReasoning 单独累积，收尾时排在同一 message item 的正文之前。
	accumulatedReasoning string

	tools           []toolCallState
	nextOutputIndex int
	finished        bool

	// nsReverse：flat 工具名 → (namespace, subtool)；namespace 位置为
	// CUSTOM_TOOL_NAMESPACE_MARKER 时表示 custom 工具。
	nsReverse map[string]nsEntry
	// acc 是上游 usage 累加（Anthropic 口径），收尾写入 response.completed.response.usage。
	acc streamUsage
}

// newResponsesEmitter 构造发射器。对应 Rust stream.rs::ResponsesEmitter::new_with_ns（line 191）。
func newResponsesEmitter(model string, ns map[string]nsEntry) responsesEmitter {
	return responsesEmitter{
		responseID:      genID("resp_"),
		itemID:          genID("msg_"),
		model:           model,
		status:          "completed",
		nextOutputIndex: 1, // 0 号下标留给 message item
		nsReverse:       ns,
	}
}

// emit 编码一个 Responses 事件：data 里补 type 与自增 sequence_number。
// 对应 Rust stream.rs::ResponsesEmitter::emit（line 214）。
func (e *responsesEmitter) emit(event string, data map[string]any) []byte {
	data["type"] = event
	data["sequence_number"] = e.seq
	e.seq++
	return sseFrame(event, data)
}

// baseResponse 构造 response 对象。对应 Rust 同名方法（line 228）。
func (e *responsesEmitter) baseResponse(status string, output any) map[string]any {
	return map[string]any{
		"id":         e.responseID,
		"object":     "response",
		"created_at": 0,
		"model":      e.model,
		"status":     status,
		"output":     output,
	}
}

// finalResponse 收尾响应体：在 baseResponse 之上带上 usage。
// 对应 Rust stream.rs::ResponsesEmitter::final_response（line 244）：
// usage 是 Responses 的必填字段，未知时给零值对象而不是省略 / null。
func (e *responsesEmitter) finalResponse(status string, output any) map[string]any {
	resp := e.baseResponse(status, output)
	resp["usage"] = e.acc.toResponsesUsage()
	return resp
}

// ensureStarted 发出 response.created + response.in_progress（幂等）。
// 对应 Rust 同名方法（line 252）。
func (e *responsesEmitter) ensureStarted(out *[][]byte) {
	if e.sentCreated {
		return
	}
	e.sentCreated = true
	*out = append(*out, e.emit("response.created", map[string]any{
		"response": e.baseResponse("in_progress", []any{}),
	}))
	*out = append(*out, e.emit("response.in_progress", map[string]any{
		"response": e.baseResponse("in_progress", []any{}),
	}))
}

// ensureItemAdded 发出 message item 的 response.output_item.added（幂等）。
// 对应 Rust 同名方法（line 262）。
func (e *responsesEmitter) ensureItemAdded(out *[][]byte) {
	if e.sentItemAdded {
		return
	}
	e.sentItemAdded = true
	*out = append(*out, e.emit("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"type":    "message",
			"id":      e.itemID,
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
	}))
}

// partValue 构造空的 output_text part。对应 Rust 同名方法（line 282）。
func partValue(kind partKind) map[string]any {
	if kind == partReasoning {
		return map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{map[string]any{"type": "reasoning"}},
		}
	}
	return map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
}

// openPart 打开一个新的 content part。对应 Rust 同名方法（line 291）。
func (e *responsesEmitter) openPart(kind partKind, out *[][]byte) {
	if e.anyPartOpened {
		e.contentIndex++
	}
	e.anyPartOpened = true
	e.currentText = ""
	*out = append(*out, e.emit("response.content_part.added", map[string]any{
		"item_id":       e.itemID,
		"output_index":  0,
		"content_index": e.contentIndex,
		"part":          partValue(kind),
	}))
	e.partOpen = kind
	e.hasPartOpen = true
}

// closePart 关闭当前 content part（幂等）。对应 Rust 同名方法（line 312）。
func (e *responsesEmitter) closePart(out *[][]byte) {
	if !e.hasPartOpen {
		return
	}
	kind := e.partOpen
	*out = append(*out, e.emit("response.output_text.done", map[string]any{
		"item_id":       e.itemID,
		"output_index":  0,
		"content_index": e.contentIndex,
		"text":          e.currentText,
	}))
	part := partValue(kind)
	part["text"] = e.currentText
	*out = append(*out, e.emit("response.content_part.done", map[string]any{
		"item_id":       e.itemID,
		"output_index":  0,
		"content_index": e.contentIndex,
		"part":          part,
	}))
	e.hasPartOpen = false
}

// ensurePart 确保当前打开的是指定 kind 的 part。对应 Rust 同名方法（line 344）。
func (e *responsesEmitter) ensurePart(kind partKind, out *[][]byte) {
	if e.hasPartOpen && e.partOpen == kind {
		return
	}
	if e.hasPartOpen {
		e.closePart(out)
	}
	e.ensureItemAdded(out)
	e.openPart(kind, out)
}

// textDelta 追加一段文本增量。对应 Rust 同名方法（line 355）。
func (e *responsesEmitter) textDelta(kind partKind, delta string, out *[][]byte) {
	e.ensureStarted(out)
	e.ensurePart(kind, out)
	e.currentText += delta
	if kind == partReasoning {
		e.accumulatedReasoning += delta
	} else {
		e.accumulatedText += delta
	}
	*out = append(*out, e.emit("response.output_text.delta", map[string]any{
		"item_id":       e.itemID,
		"output_index":  0,
		"content_index": e.contentIndex,
		"delta":         delta,
	}))
}

// toolAdded 发出一个工具调用的 response.output_item.added，并在 nsReverse 里
// 还原 namespace / custom 身份。对应 Rust 同名方法（line 376）。
func (e *responsesEmitter) toolAdded(callID, flatName string, out *[][]byte) {
	e.ensureStarted(out)
	if e.hasPartOpen {
		e.closePart(out)
	}
	outputIndex := e.nextOutputIndex
	e.nextOutputIndex++

	name := flatName
	namespace := ""
	custom := false
	if entry, known := e.nsReverse[flatName]; known {
		if entry.namespace == CUSTOM_TOOL_NAMESPACE_MARKER {
			name = entry.subtool
			custom = true
		} else {
			name = entry.subtool
			namespace = entry.namespace
		}
	}
	state := toolCallState{
		callID:      callID,
		name:        name,
		namespace:   namespace,
		custom:      custom,
		outputIndex: outputIndex,
	}
	e.tools = append(e.tools, state)
	*out = append(*out, e.emit("response.output_item.added", map[string]any{
		"output_index": outputIndex,
		"item":         state.item("in_progress", ""),
	}))
}

// toolArgsDelta 追加一段函数参数增量。对应 Rust 同名方法（line 435）。
func (e *responsesEmitter) toolArgsDelta(callID, delta string, out *[][]byte) {
	for i := range e.tools {
		if e.tools[i].callID != callID {
			continue
		}
		e.tools[i].args += delta
		*out = append(*out, e.emit("response.function_call_arguments.delta", map[string]any{
			"item_id":      e.tools[i].callID,
			"output_index": e.tools[i].outputIndex,
			"delta":        delta,
		}))
		return
	}
}

// toolDone 收尾一个工具调用（幂等）。对应 Rust 同名方法（line 453）。
func (e *responsesEmitter) toolDone(callID string, out *[][]byte) {
	index := -1
	for i := range e.tools {
		if e.tools[i].callID == callID && !e.tools[i].done {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	e.tools[index].done = true
	state := &e.tools[index]
	arguments := state.argumentsOrDefault()
	*out = append(*out, e.emit("response.function_call_arguments.done", map[string]any{
		"item_id":      state.callID,
		"output_index": state.outputIndex,
		"arguments":    arguments,
	}))
	*out = append(*out, e.emit("response.output_item.done", map[string]any{
		"output_index": state.outputIndex,
		"item":         state.item("completed", arguments),
	}))
}

// buildFinalOutput 构造 response.completed 里的 output 数组。
// 对应 Rust 同名方法（line 519）：reasoning part 排在正文之前，随后是各工具项。
func (e *responsesEmitter) buildFinalOutput() []any {
	items := []any{}
	parts := []any{}
	if e.accumulatedReasoning != "" {
		part := partValue(partReasoning)
		part["text"] = e.accumulatedReasoning
		parts = append(parts, part)
	}
	if e.accumulatedText != "" {
		part := partValue(partText)
		part["text"] = e.accumulatedText
		parts = append(parts, part)
	}
	if len(parts) > 0 {
		items = append(items, map[string]any{
			"type":    "message",
			"id":      e.itemID,
			"status":  "completed",
			"role":    "assistant",
			"content": parts,
		})
	}
	for i := range e.tools {
		items = append(items, e.tools[i].item("completed", e.tools[i].argumentsOrDefault()))
	}
	return items
}

// finish 收尾：补齐未关闭的 part / 未完成的工具项，再发 response.completed。
// 对应 Rust stream.rs::ResponsesEmitter::finish（line 579）。
func (e *responsesEmitter) finish(out *[][]byte) {
	if e.finished {
		return
	}
	e.finished = true
	e.ensureStarted(out)
	if e.hasPartOpen {
		e.closePart(out)
	}
	pending := make([]string, 0, len(e.tools))
	for i := range e.tools {
		if !e.tools[i].done {
			pending = append(pending, e.tools[i].callID)
		}
	}
	for _, callID := range pending {
		e.toolDone(callID, out)
	}
	output := e.buildFinalOutput()
	if e.sentItemAdded {
		for _, raw := range output {
			item, ok := asMap(raw)
			if !ok {
				continue
			}
			if itemType, _ := asString(item["type"]); itemType == "message" {
				*out = append(*out, e.emit("response.output_item.done", map[string]any{
					"output_index": 0,
					"item":         item,
				}))
				break
			}
		}
	}
	*out = append(*out, e.emit("response.completed", map[string]any{
		"response": e.finalResponse(e.status, output),
	}))
}

// chatToResponsesStream 把 Chat 上游的流翻译成 Responses 事件流。
// 对应 Rust stream.rs::ChatCompletionsToResponsesStream（line 616）。
type chatToResponsesStream struct {
	emitter responsesEmitter
	// toolIndexToCallID：Chat 的 tool_calls[].index → call id（后续增量只带 index/arguments）。
	toolIndexToCallID map[int]string
}

// newChatToResponsesStream 创建 chat → responses 的流式转换器；
// ns 是 Session.nsReverse（flat 工具名 → namespace/subtool）。
func newChatToResponsesStream(model string, ns map[string]nsEntry) streamConverter {
	return &chatToResponsesStream{
		emitter:           newResponsesEmitter(model, ns),
		toolIndexToCallID: map[int]string{},
	}
}

// processEvent 处理一个上游 Chat 事件。
// 对应 Rust stream.rs::ChatCompletionsToResponsesStream::process_chunk（line 638）。
func (s *chatToResponsesStream) processEvent(ev sseEvent) [][]byte {
	var out [][]byte
	if ev.Data == "[DONE]" {
		s.emitter.finish(&out)
		return out
	}
	chunk, err := decodeObject([]byte(ev.Data))
	if err != nil {
		return nil
	}
	if s.emitter.model == "" {
		if model, ok := asString(chunk["model"]); ok {
			s.emitter.model = model
		}
	}
	// 上游 usage chunk（choices 为空数组）必须先于收尾累加。
	if usage, ok := asMap(chunk["usage"]); ok {
		s.emitter.acc.mergeChat(usage)
	}
	s.emitter.ensureStarted(&out)

	choice := firstChatChoice(chunk)
	var delta map[string]any
	if choice != nil {
		delta, _ = asMap(choice["delta"])
	}
	if delta != nil {
		if reasoning, _ := asString(delta["reasoning_content"]); reasoning != "" {
			s.emitter.textDelta(partReasoning, reasoning, &out)
		}
		if content, _ := asString(delta["content"]); content != "" {
			s.emitter.textDelta(partText, content, &out)
		}
		if toolCalls, ok := asArray(delta["tool_calls"]); ok {
			for i, raw := range toolCalls {
				call, ok := asMap(raw)
				if !ok {
					continue
				}
				index := intOr(call["index"], i)
				function, _ := asMap(call["function"])
				callID, _ := asString(call["id"])
				if callID != "" {
					if _, known := s.toolIndexToCallID[index]; !known {
						name := ""
						if function != nil {
							name, _ = asString(function["name"])
						}
						s.toolIndexToCallID[index] = callID
						s.emitter.toolAdded(callID, name, &out)
					}
				}
				if knownCallID, known := s.toolIndexToCallID[index]; known && function != nil {
					if args, _ := asString(function["arguments"]); args != "" {
						s.emitter.toolArgsDelta(knownCallID, args, &out)
					}
				}
			}
		}
	}
	if choice != nil {
		// 只记录状态，收尾推迟到 [DONE] / finalize()：上游的 usage chunk
		// 位于 finish_reason 之后，提前收尾会把 usage 丢掉。
		if raw, present := choice["finish_reason"]; present {
			if finishReason, ok := asString(raw); ok {
				s.emitter.status = finishReasonToStatus(finishReason)
			}
		}
	}
	return out
}

// finalize 在流结束（[DONE] 或 EOF）时收尾。
func (s *chatToResponsesStream) finalize() [][]byte {
	var out [][]byte
	s.emitter.finish(&out)
	return out
}

// usage 返回累计的上游 usage（Anthropic 口径）。
func (s *chatToResponsesStream) usage() Usage { return s.emitter.acc.usage() }

// ==================== 完整响应对象重放为 SSE ====================

// replayChat 把**完整的 Chat 响应对象**重放成 Chat SSE 流。
// 对应 Rust stream.rs::replay_chat（line 2028）：
// role chunk → reasoning / content / tool_calls chunk → finish chunk → `data: [DONE]`。
//
// 与 Rust 的差异：Rust 的重放不写 usage chunk；这里在上游带 usage 时补一个
// `choices: []` 的 usage chunk（位于 finish chunk 之后、[DONE] 之前），
// 与「同一条实时流（代理请求了 stream_options.include_usage）应有的形态」一致。
func replayChat(resp map[string]any) []byte {
	var out bytes.Buffer
	id := genID("chatcmpl-")
	if v, ok := asString(resp["id"]); ok {
		id = v
	}
	model, _ := asString(resp["model"])
	created := intField(resp, "created")

	choice := firstChatChoice(resp)
	var message map[string]any
	finishReason := "stop"
	if choice != nil {
		message, _ = asMap(choice["message"])
		if fr, ok := asString(choice["finish_reason"]); ok {
			finishReason = fr
		}
	}
	base := func(delta map[string]any, finish any) map[string]any {
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

	out.Write(sseFrame("", base(map[string]any{"role": "assistant"}, nil)))
	if message != nil {
		if reasoning, ok := asString(message["reasoning_content"]); ok && reasoning != "" {
			out.Write(sseFrame("", base(map[string]any{"reasoning_content": reasoning}, nil)))
		}
		// extractText 兼容字符串形态（上游 Chat 的常态）与数组形态。
		if text := extractText(message["content"]); text != "" {
			out.Write(sseFrame("", base(map[string]any{"content": text}, nil)))
		}
		if toolCalls, ok := asArray(message["tool_calls"]); ok && len(toolCalls) > 0 {
			out.Write(sseFrame("", base(map[string]any{"tool_calls": toolCalls}, nil)))
		}
	}
	out.Write(sseFrame("", base(map[string]any{}, finishReason)))
	if usage, ok := asMap(resp["usage"]); ok {
		out.Write(sseFrame("", map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   usage,
		}))
	}
	out.WriteString("data: [DONE]\n\n")
	return out.Bytes()
}

// replayAnthropic 把**完整的 Anthropic 响应对象**重放成 Anthropic SSE 流。
// 对应 Rust stream.rs::replay_anthropic（line 2095）：
// message_start → 逐块 content_block_start/delta/stop → message_delta（带 usage）→ message_stop。
func replayAnthropic(resp map[string]any) []byte {
	var out bytes.Buffer
	id := genID("msg_")
	if v, ok := asString(resp["id"]); ok {
		id = v
	}
	model, _ := asString(resp["model"])
	stopReason := "end_turn"
	if v, ok := asString(resp["stop_reason"]); ok {
		stopReason = v
	}
	usage, ok := asMap(resp["usage"])
	if !ok {
		usage = map[string]any{"input_tokens": 0, "output_tokens": 0}
	}

	out.Write(sseFrame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usage,
		},
	}))

	content, _ := asArray(resp["content"])
	for index, raw := range content {
		block, ok := asMap(raw)
		if !ok {
			continue
		}
		blockType, _ := asString(block["type"])
		switch blockType {
		case "text":
			text, _ := asString(block["text"])
			out.Write(sseFrame("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "text", "text": ""},
			}))
			if text != "" {
				out.Write(sseFrame("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{"type": "text_delta", "text": text},
				}))
			}
			out.Write(sseFrame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			}))
		case "thinking":
			thinking, _ := asString(block["thinking"])
			out.Write(sseFrame("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}))
			if thinking != "" {
				out.Write(sseFrame("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
				}))
			}
			out.Write(sseFrame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			}))
		case "tool_use":
			out.Write(sseFrame("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": block,
			}))
			if input, present := block["input"]; present {
				out.Write(sseFrame("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": jsonString(input)},
				}))
			}
			out.Write(sseFrame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			}))
		default:
			// 未知块型整体透传（与 Rust 的 default 分支一致）。
			out.Write(sseFrame("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": block,
			}))
			out.Write(sseFrame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			}))
		}
	}

	out.Write(sseFrame("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usage,
	}))
	out.Write(sseFrame("message_stop", map[string]any{"type": "message_stop"}))
	return out.Bytes()
}

// replayResponses 把**完整的 Responses 响应对象**重放成 Responses SSE 流。
// 对应 Rust stream.rs::replay_responses（line 1901）：
// response.created / response.in_progress → 逐 output item 的
// added / content_part.added / output_text.delta / output_text.done / content_part.done / done
// → response.completed（带 usage）。
func replayResponses(resp map[string]any) []byte {
	var out bytes.Buffer
	seq := uint64(0)
	emit := func(event string, data map[string]any) {
		data["type"] = event
		data["sequence_number"] = seq
		seq++
		out.Write(sseFrame(event, data))
	}

	id := genID("resp_")
	if v, ok := asString(resp["id"]); ok {
		id = v
	}
	model, _ := asString(resp["model"])
	status := "completed"
	if v, ok := asString(resp["status"]); ok {
		status = v
	}
	newResponse := func(state string, output any) map[string]any {
		return map[string]any{
			"id":         id,
			"object":     "response",
			"created_at": 0,
			"model":      model,
			"status":     state,
			"output":     output,
		}
	}

	emit("response.created", map[string]any{"response": newResponse("in_progress", []any{})})
	emit("response.in_progress", map[string]any{"response": newResponse("in_progress", []any{})})

	output, ok := asArray(resp["output"])
	if !ok {
		output = []any{}
	}
	for outputIndex, raw := range output {
		item, ok := asMap(raw)
		if !ok {
			continue
		}
		itemID := genID("msg_")
		if v, ok := asString(item["id"]); ok {
			itemID = v
		}
		itemType, _ := asString(item["type"])
		if itemType == "" {
			itemType = "message"
		}
		if itemType != "message" {
			// function_call / custom_tool_call / reasoning 等：整体添加/完成。
			emit("response.output_item.added", map[string]any{
				"output_index": outputIndex,
				"item":         item,
			})
			emit("response.output_item.done", map[string]any{
				"output_index": outputIndex,
				"item":         item,
			})
			continue
		}
		emit("response.output_item.added", map[string]any{
			"output_index": outputIndex,
			"item": map[string]any{
				"type":    "message",
				"id":      itemID,
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		})
		parts, _ := asArray(item["content"])
		for contentIndex, rawPart := range parts {
			part, _ := asMap(rawPart)
			text := ""
			if part != nil {
				text, _ = asString(part["text"])
			}
			emit("response.content_part.added", map[string]any{
				"item_id":       itemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part":          part,
			})
			if text != "" {
				emit("response.output_text.delta", map[string]any{
					"item_id":       itemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
			}
			emit("response.output_text.done", map[string]any{
				"item_id":       itemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"text":          text,
			})
			emit("response.content_part.done", map[string]any{
				"item_id":       itemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part":          part,
			})
		}
		emit("response.output_item.done", map[string]any{
			"output_index": outputIndex,
			"item":         item,
		})
	}

	completed := newResponse(status, output)
	// usage 恒在：上游给了就搬运，没给给零值对象（Responses 规范里的必填字段）。
	usage, ok := asMap(resp["usage"])
	if !ok {
		usage = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	completed["usage"] = usage
	emit("response.completed", map[string]any{"response": completed})
	return out.Bytes()
}
