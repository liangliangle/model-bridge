// Package e2e 端到端验证：以 mock 上游 HTTP server 驱动真实的后端入口，
// 覆盖三个代理入口的流式与非流式路径，以及渠道优先级与故障转移。
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// mockContent 是 mock 上游返回的文本内容。所有断言都以它为准，
// 用来证明下游看到的内容确实来自上游而不是被本地拼出来的。
const mockContent = "Hello, world!"

// mockReasoning 是 mock 上游返回的推理内容（拆成两段下发，便于断言增量拼接）。
const mockReasoning = "pondering"

// mockTool* 是 mock 上游工具调用的固定值。三个协议各自的 call id 前缀不同
// （Chat 用 call_、Anthropic 用 toolu_、Responses 用 fc_），以便断言转换是否
// 正确地在协议之间搬运标识符。
const (
	mockToolName   = "get_weather"
	mockToolArgs   = `{"loc":"SF"}`
	mockToolChatID = "call_mock"
	mockToolAnthID = "toolu_mock"
	mockToolRespID = "fc_mock"
	mockSignature  = "sig-mock"
)

// mockPromptTokens / mockCompletionTokens 是 mock 上游上报的用量。
// 取非整十的数值，避免与「硬编码 0」或常见默认值混淆。
const (
	mockPromptTokens     = 11
	mockCompletionTokens = 5
)

// RecordedRequest 是 mock 收到的原始请求。
type RecordedRequest struct {
	Path   string
	Method string
	Header http.Header
	Body   []byte
}

// MockUpstream 是一个按路径分别以三种协议作答的假上游。
//
// 路径：
//
//	/chat        以 OpenAI Chat Completions 作答（流式时按分块下发）
//	/messages    以 Anthropic Messages 作答
//	/responses   以 OpenAI Responses 作答
//	/chat-fail   固定返回 500，用于故障转移测试
//
// 查询参数（都可省略，省略时是当前已有的最小形态，输出与加入参数前逐字节一致）：
//
//	variant=tools       改为回一个工具调用（流式按参数分片）
//	variant=reasoning   改为回推理内容 + 正文（Anthropic 带 signature）
//	error=<shape>       不发内容，只在首块里发一个错误事件，随后结束
//	sse=off             即使请求 stream=true 也回非流式的单个 JSON 体
//
// error 的 shape 与协议无关（可以刻意给 /chat 配 Responses 形态的错误，
// 用来验证错误探测本身是按形态而不是按路径判断的）：
//
//	event       `event:error` 行 + 非 JSON 载荷（DashScope/Qwen 形态）
//	chat        {"error":{"message":...}}                （Chat 形态）
//	anthropic   {"type":"error","error":{"message":...}} （Anthropic 形态）
//	responses   {"type":"error","message":...}           （Responses 形态，信息在顶层）
//	failed      {"type":"response.failed","response":{...error...}}
type MockUpstream struct {
	Server *httptest.Server

	mu       sync.Mutex
	requests []RecordedRequest
}

// NewMockUpstream 启动假上游。
func NewMockUpstream() *MockUpstream {
	m := &MockUpstream{}
	m.Server = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

// Close 关闭假上游。
func (m *MockUpstream) Close() { m.Server.Close() }

// URL 返回指定路径的完整地址。
func (m *MockUpstream) URL(path string) string { return m.Server.URL + path }

// URLWith 返回带查询参数的完整地址（例如 URLWith("/chat", "variant=tools")）。
func (m *MockUpstream) URLWith(path, query string) string {
	if query == "" {
		return m.URL(path)
	}
	return m.Server.URL + path + "?" + query
}

// Requests 返回收到的全部请求（副本）。
func (m *MockUpstream) Requests() []RecordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// LastRequest 返回最后一个请求；没有请求时 ok 为 false。
func (m *MockUpstream) LastRequest() (RecordedRequest, bool) {
	reqs := m.Requests()
	if len(reqs) == 0 {
		return RecordedRequest{}, false
	}
	return reqs[len(reqs)-1], true
}

// RequestPaths 按顺序返回收到的路径，用于断言渠道尝试顺序。
func (m *MockUpstream) RequestPaths() []string {
	reqs := m.Requests()
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Path)
	}
	return out
}

// Reset 清空记录。
func (m *MockUpstream) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = nil
}

// mockVariant 是需要的内容形态。
type mockVariant struct {
	tools     bool
	reasoning bool
}

func parseVariant(r *http.Request) mockVariant {
	switch r.URL.Query().Get("variant") {
	case "tools":
		return mockVariant{tools: true}
	case "reasoning":
		return mockVariant{reasoning: true}
	default:
		return mockVariant{}
	}
}

func (m *MockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, RecordedRequest{
		Path:   r.URL.Path,
		Method: r.Method,
		Header: r.Header.Clone(),
		Body:   body,
	})
	m.mu.Unlock()

	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	stream := false
	if v, ok := parsed["stream"].(bool); ok {
		stream = v
	}
	includeUsage := false
	if so, ok := parsed["stream_options"].(map[string]any); ok {
		if v, ok := so["include_usage"].(bool); ok {
			includeUsage = v
		}
	}

	variant := parseVariant(r)
	errShape := r.URL.Query().Get("error")
	// err_after=1：先发一个内容事件，再发错误事件。用来验证「已经有内容 → 不能换渠道」
	// 这条判定的反面（首块里就已经有内容，因此不得再重试）。
	errAfter := r.URL.Query().Get("err_after") != ""
	// sse=off：请求是流式，但上游回一个完整的 JSON 体（走降级重放路径）。
	asSSE := stream && r.URL.Query().Get("sse") != "off"

	switch r.URL.Path {
	case "/chat-fail":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"mock upstream is down","type":"server_error"}}`))
	case "/chat":
		if errShape != "" {
			emitErrorStream(w, errShape, "chat", errAfter)
			return
		}
		if asSSE {
			m.streamChat(w, includeUsage, variant)
		} else {
			writeJSON(w, chatBody(variant))
		}
	case "/messages":
		if errShape != "" {
			emitErrorStream(w, errShape, "messages", errAfter)
			return
		}
		if asSSE {
			m.streamMessages(w, variant)
		} else {
			writeJSON(w, messagesBody(variant))
		}
	case "/responses":
		if errShape != "" {
			emitErrorStream(w, errShape, "responses", errAfter)
			return
		}
		if asSSE {
			m.streamResponses(w, variant)
		} else {
			writeJSON(w, responsesBody(variant))
		}
	default:
		http.NotFound(w, r)
	}
}

// ========== 非流式响应体（三种协议 × 内容形态）==========

func chatUsage() map[string]any {
	return map[string]any{
		"prompt_tokens":     mockPromptTokens,
		"completion_tokens": mockCompletionTokens,
		"total_tokens":      mockPromptTokens + mockCompletionTokens,
	}
}

func chatBody(v mockVariant) map[string]any {
	message := map[string]any{"role": "assistant", "content": mockContent}
	finish := "stop"
	if v.reasoning {
		message["reasoning_content"] = mockReasoning
	}
	if v.tools {
		message["content"] = ""
		message["tool_calls"] = []any{map[string]any{
			"id":   mockToolChatID,
			"type": "function",
			"function": map[string]any{
				"name":      mockToolName,
				"arguments": mockToolArgs,
			},
		}}
		finish = "tool_calls"
	}
	return map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": 1,
		"model":   "up-model",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": chatUsage(),
	}
}

func messagesBody(v mockVariant) map[string]any {
	content := []any{}
	stop := "end_turn"
	if v.reasoning {
		content = append(content, map[string]any{
			"type":      "thinking",
			"thinking":  mockReasoning,
			"signature": mockSignature,
		})
	}
	if v.tools {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    mockToolAnthID,
			"name":  mockToolName,
			"input": map[string]any{"loc": "SF"},
		})
		stop = "tool_use"
	} else {
		content = append(content, map[string]any{"type": "text", "text": mockContent})
	}
	return map[string]any{
		"id":          "msg_mock",
		"type":        "message",
		"role":        "assistant",
		"model":       "up-model",
		"content":     content,
		"stop_reason": stop,
		"usage": map[string]any{
			"input_tokens":  mockPromptTokens,
			"output_tokens": mockCompletionTokens,
		},
	}
}

func responsesBody(v mockVariant) map[string]any {
	output := []any{}
	if v.reasoning {
		output = append(output, map[string]any{
			"id":     "rs_mock",
			"type":   "reasoning",
			"status": "completed",
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": mockReasoning,
			}},
		})
	}
	if v.tools {
		output = append(output, map[string]any{
			"id":        mockToolRespID,
			"type":      "function_call",
			"call_id":   mockToolChatID,
			"name":      mockToolName,
			"arguments": mockToolArgs,
			"status":    "completed",
		})
	} else {
		output = append(output, map[string]any{
			"id":     "msg_mock",
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        mockContent,
				"annotations": []any{},
			}},
		})
	}
	return map[string]any{
		"id":         "resp_mock",
		"object":     "response",
		"created_at": 1,
		"model":      "up-model",
		"status":     "completed",
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  mockPromptTokens,
			"output_tokens": mockCompletionTokens,
			"total_tokens":  mockPromptTokens + mockCompletionTokens,
		},
	}
}

// ========== 错误流：只在首块发一个错误事件 ==========

// emitErrorStream 按 shape 发一个错误事件后结束。
// 用于验证「首块含错误且尚无内容 → 可以换渠道」这条判定。
//
// proto 是路径对应的协议（chat/messages/responses），决定 contentFirst 时先发哪一种
// 内容事件；contentFirst 为真时先发一个内容事件再发错误，用来验证该判定的反面。
func emitErrorStream(w http.ResponseWriter, shape, proto string, contentFirst bool) {
	emit := sseStart(w)

	if contentFirst {
		switch proto {
		case "messages":
			emit(`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
			emit(`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n")
		case "responses":
			emit(`data: {"type":"response.output_text.delta","item_id":"msg_mock","output_index":0,"content_index":0,"delta":"partial"}` + "\n\n")
		default:
			emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n", chatBase))
		}
	}

	switch shape {
	case "event":
		emit("event:error\ndata: mock upstream boom\n\n")
	case "chat":
		emit(`data: {"error":{"message":"mock stream failed","type":"server_error"}}` + "\n\n")
	case "anthropic":
		emit(`data: {"type":"error","error":{"type":"overloaded_error","message":"mock overloaded"}}` + "\n\n")
	case "responses":
		emit(`data: {"type":"error","code":"server_error","message":"mock responses error"}` + "\n\n")
	case "failed":
		emit(`data: {"type":"response.failed","response":{"id":"resp_mock","status":"failed","error":{"code":"server_error","message":"mock response failed"}}}` + "\n\n")
	default:
		emit(`data: {"error":{"message":"unknown mock error shape: ` + shape + `"}}` + "\n\n")
	}
}

// ========== 流式作答：刻意分多个 chunk 下发，且逐块 flush ==========

// sseStart 准备 SSE 响应头并立即 flush，让下游能观察到「分块」而不是一次性整块。
func sseStart(w http.ResponseWriter) func(string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush(w)
	return func(chunk string) {
		_, _ = io.WriteString(w, chunk)
		flush(w)
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// chatBase 是 Chat chunk 的公共前缀。
const chatBase = `"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1,"model":"up-model"`

func (m *MockUpstream) streamChat(w http.ResponseWriter, includeUsage bool, v mockVariant) {
	emit := sseStart(w)
	emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n", chatBase))

	if v.reasoning {
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"pon\"},\"finish_reason\":null}]}\n\n", chatBase))
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"dering\"},\"finish_reason\":null}]}\n\n", chatBase))
	}

	if v.tools {
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"%s\",\"type\":\"function\",\"function\":{\"name\":\"%s\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n",
			chatBase, mockToolChatID, mockToolName))
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"loc\\\":\\\"\"}}]},\"finish_reason\":null}]}\n\n", chatBase))
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"SF\\\"}\"}}]},\"finish_reason\":null}]}\n\n", chatBase))
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n", chatBase))
	} else {
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n", chatBase))
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"content\":\", world!\"},\"finish_reason\":null}]}\n\n", chatBase))
		emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", chatBase))
	}

	if includeUsage {
		emit(fmt.Sprintf("data: {%s,\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n",
			chatBase, mockPromptTokens, mockCompletionTokens, mockPromptTokens+mockCompletionTokens))
	}
	emit("data: [DONE]\n\n")
}

func (m *MockUpstream) streamMessages(w http.ResponseWriter, v mockVariant) {
	emit := sseStart(w)
	emit(`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_mock","type":"message","role":"assistant","model":"up-model","content":[],"stop_reason":null,"usage":{"input_tokens":11,"output_tokens":0}}}` + "\n\n")

	nextIndex := 0
	if v.reasoning {
		emit(`event: content_block_start` + "\n" +
			fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, nextIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":"pon"}}`, nextIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":"dering"}}`, nextIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":"%s"}}`, nextIndex, mockSignature) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, nextIndex) + "\n\n")
		nextIndex++
	}

	stopReason := "end_turn"
	if v.tools {
		emit(`event: content_block_start` + "\n" +
			fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"%s","name":"%s","input":{}}}`, nextIndex, mockToolAnthID, mockToolName) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":"{\"loc\":\""}}`, nextIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":"SF\"}"}}`, nextIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, nextIndex) + "\n\n")
		stopReason = "tool_use"
	} else {
		emit(`event: content_block_start` + "\n" +
			fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, nextIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":"Hello"}}`, nextIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":", world!"}}`, nextIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, nextIndex) + "\n\n")
	}

	emit(`event: message_delta` + "\n" +
		fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":"%s"},"usage":{"output_tokens":%d}}`, stopReason, mockCompletionTokens) + "\n\n")
	emit(`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n")
}

func (m *MockUpstream) streamResponses(w http.ResponseWriter, v mockVariant) {
	emit := sseStart(w)
	emit(`event: response.created` + "\n" +
		`data: {"type":"response.created","response":{"id":"resp_mock","object":"response","status":"in_progress","model":"up-model","output":[]}}` + "\n\n")

	// 推理项（可选）：作为第 0 个 output item。
	outputIndex := 0
	outputItems := []string{}
	if v.reasoning {
		emit(fmt.Sprintf(`data: {"type":"response.output_item.added","output_index":%d,"item":{"id":"rs_mock","type":"reasoning","status":"in_progress","summary":[]}}`, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.reasoning_summary_part.added","item_id":"rs_mock","output_index":%d,"summary_index":0,"part":{"type":"summary_text","text":""}}`, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_mock","output_index":%d,"summary_index":0,"delta":"pon"}`, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_mock","output_index":%d,"summary_index":0,"delta":"dering"}`, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.reasoning_summary_text.done","item_id":"rs_mock","output_index":%d,"summary_index":0,"text":"%s"}`, outputIndex, mockReasoning) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.reasoning_summary_part.done","item_id":"rs_mock","output_index":%d,"summary_index":0,"part":{"type":"summary_text","text":"%s"}}`, outputIndex, mockReasoning) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.output_item.done","output_index":%d,"item":{"id":"rs_mock","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"%s"}]}}`, outputIndex, mockReasoning) + "\n\n")
		outputItems = append(outputItems, fmt.Sprintf(`{"id":"rs_mock","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"%s"}]}`, mockReasoning))
		outputIndex++
	}

	if v.tools {
		emit(fmt.Sprintf(`data: {"type":"response.output_item.added","output_index":%d,"item":{"id":"%s","type":"function_call","call_id":"%s","name":"%s","arguments":"","status":"in_progress"}}`, outputIndex, mockToolRespID, mockToolChatID, mockToolName) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.function_call_arguments.delta","item_id":"%s","output_index":%d,"delta":"{\"loc\":\""}`, mockToolRespID, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.function_call_arguments.delta","item_id":"%s","output_index":%d,"delta":"SF\"}"}`, mockToolRespID, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.function_call_arguments.done","item_id":"%s","output_index":%d,"arguments":"%s"}`, mockToolRespID, outputIndex, mockToolArgs) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.output_item.done","output_index":%d,"item":{"id":"%s","type":"function_call","call_id":"%s","name":"%s","arguments":"%s","status":"completed"}}`, outputIndex, mockToolRespID, mockToolChatID, mockToolName, mockToolArgs) + "\n\n")
		outputItems = append(outputItems, fmt.Sprintf(`{"id":"%s","type":"function_call","call_id":"%s","name":"%s","arguments":"%s","status":"completed"}`, mockToolRespID, mockToolChatID, mockToolName, mockToolArgs))
	} else {
		emit(fmt.Sprintf(`data: {"type":"response.output_item.added","output_index":%d,"item":{"id":"msg_mock","type":"message","role":"assistant","status":"in_progress","content":[]}}`, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.content_part.added","item_id":"msg_mock","output_index":%d,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.output_text.delta","item_id":"msg_mock","output_index":%d,"content_index":0,"delta":"Hello"}`, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.output_text.delta","item_id":"msg_mock","output_index":%d,"content_index":0,"delta":", world!"}`, outputIndex) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.output_text.done","item_id":"msg_mock","output_index":%d,"content_index":0,"text":"%s"}`, outputIndex, mockContent) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.content_part.done","item_id":"msg_mock","output_index":%d,"content_index":0,"part":{"type":"output_text","text":"%s","annotations":[]}}`, outputIndex, mockContent) + "\n\n")
		emit(fmt.Sprintf(`data: {"type":"response.output_item.done","output_index":%d,"item":{"id":"msg_mock","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"%s","annotations":[]}]}}`, outputIndex, mockContent) + "\n\n")
		outputItems = append(outputItems, fmt.Sprintf(`{"id":"msg_mock","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"%s","annotations":[]}]}`, mockContent))
	}

	emit(fmt.Sprintf(`data: {"type":"response.completed","response":{"id":"resp_mock","object":"response","status":"completed","model":"up-model","output":[%s],"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}`,
		strings.Join(outputItems, ","), mockPromptTokens, mockCompletionTokens, mockPromptTokens+mockCompletionTokens) + "\n\n")
	emit("data: [DONE]\n\n")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
