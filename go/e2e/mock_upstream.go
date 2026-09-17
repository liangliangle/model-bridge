// Package e2e 端到端验证：以 mock 上游 HTTP server 驱动真实的后端入口，
// 覆盖三个代理入口的流式与非流式路径，以及渠道优先级与故障转移。
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// mockContent 是 mock 上游返回的文本内容。所有断言都以它为准，
// 用来证明下游看到的内容确实来自上游而不是被本地拼出来的。
const mockContent = "Hello, world!"

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

	switch r.URL.Path {
	case "/chat-fail":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"mock upstream is down","type":"server_error"}}`))
	case "/chat":
		if stream {
			m.streamChat(w, includeUsage)
		} else {
			writeJSON(w, map[string]any{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion",
				"created": 1,
				"model":   "up-model",
				"choices": []any{map[string]any{
					"index":         0,
					"message":       map[string]any{"role": "assistant", "content": mockContent},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{
					"prompt_tokens":     mockPromptTokens,
					"completion_tokens": mockCompletionTokens,
					"total_tokens":      mockPromptTokens + mockCompletionTokens,
				},
			})
		}
	case "/messages":
		if stream {
			m.streamMessages(w)
		} else {
			writeJSON(w, map[string]any{
				"id":          "msg_mock",
				"type":        "message",
				"role":        "assistant",
				"model":       "up-model",
				"content":     []any{map[string]any{"type": "text", "text": mockContent}},
				"stop_reason": "end_turn",
				"usage": map[string]any{
					"input_tokens":  mockPromptTokens,
					"output_tokens": mockCompletionTokens,
				},
			})
		}
	case "/responses":
		if stream {
			m.streamResponses(w)
		} else {
			writeJSON(w, map[string]any{
				"id":         "resp_mock",
				"object":     "response",
				"created_at": 1,
				"model":      "up-model",
				"status":     "completed",
				"output": []any{map[string]any{
					"id":     "msg_mock",
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []any{map[string]any{
						"type":        "output_text",
						"text":        mockContent,
						"annotations": []any{},
					}},
				}},
				"usage": map[string]any{
					"input_tokens":  mockPromptTokens,
					"output_tokens": mockCompletionTokens,
					"total_tokens":  mockPromptTokens + mockCompletionTokens,
				},
			})
		}
	default:
		http.NotFound(w, r)
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

func (m *MockUpstream) streamChat(w http.ResponseWriter, includeUsage bool) {
	emit := sseStart(w)
	base := `"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1,"model":"up-model"`
	emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n", base))
	emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n", base))
	emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{\"content\":\", world!\"},\"finish_reason\":null}]}\n\n", base))
	emit(fmt.Sprintf("data: {%s,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", base))
	if includeUsage {
		emit(fmt.Sprintf("data: {%s,\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n",
			base, mockPromptTokens, mockCompletionTokens, mockPromptTokens+mockCompletionTokens))
	}
	emit("data: [DONE]\n\n")
}

func (m *MockUpstream) streamMessages(w http.ResponseWriter) {
	emit := sseStart(w)
	emit("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_mock\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"up-model\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n")
	emit("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	emit("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n")
	emit("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\", world!\"}}\n\n")
	emit("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	emit("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
	emit("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func (m *MockUpstream) streamResponses(w http.ResponseWriter) {
	emit := sseStart(w)
	emit("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_mock\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"up-model\",\"output\":[]}}\n\n")
	emit("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_mock\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n")
	emit("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_mock\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello\"}\n\n")
	emit("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_mock\",\"output_index\":0,\"content_index\":0,\"delta\":\", world!\"}\n\n")
	emit("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_mock\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"up-model\",\"output\":[{\"id\":\"msg_mock\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello, world!\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":11,\"output_tokens\":5,\"total_tokens\":16}}}\n\n")
	emit("data: [DONE]\n\n")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
