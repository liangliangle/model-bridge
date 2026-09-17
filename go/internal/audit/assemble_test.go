package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

// 流式响应组装：审计详情里展示的应当是「拼合后的完整响应对象」，
// 而不是逐条 SSE 事件。对应 Rust `assemble_streaming_response`（db.rs:1258）。

func decodeAssembled(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("组装结果不是合法 JSON: %v\n%s", err, raw)
	}
	return out
}

func TestAssembleAnthropicStream(t *testing.T) {
	raw := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":11,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", world!"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	got := decodeAssembled(t, assembleStreamingResponse(raw))
	if got["type"] != "message" || got["role"] != "assistant" {
		t.Fatalf("应组装为 Anthropic message 对象: %#v", got)
	}
	if got["id"] != "msg_1" || got["model"] != "claude-x" {
		t.Fatalf("id/model 应从 message_start 取到: %#v", got)
	}
	content := got["content"].([]any)
	if text := content[0].(map[string]any)["text"]; text != "Hello, world!" {
		t.Fatalf("文本应为增量拼接结果，实际 %v", text)
	}
	if got["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason 应取自 message_delta，实际 %v", got["stop_reason"])
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(5) {
		t.Fatalf("usage 应为 input=11 output=5，实际 %#v", usage)
	}
}

func TestAssembleChatStream(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"content":", world!"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	got := decodeAssembled(t, assembleStreamingResponse(raw))
	if got["object"] != "chat.completion" {
		t.Fatalf("应组装为 chat.completion 对象: %#v", got)
	}
	if got["id"] != "chatcmpl-1" || got["model"] != "gpt-x" {
		t.Fatalf("id/model 应保留: %#v", got)
	}
	choices := got["choices"].([]any)
	choice := choices[0].(map[string]any)
	if msg := choice["message"].(map[string]any); msg["content"] != "Hello, world!" {
		t.Fatalf("文本应为增量拼接结果，实际 %v", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason 应保留，实际 %v", choice["finish_reason"])
	}
	usage := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(11) || usage["completion_tokens"] != float64(5) || usage["total_tokens"] != float64(16) {
		t.Fatalf("usage 应取自尾部 usage chunk，实际 %#v", usage)
	}
}

func TestAssembleResponsesStreamUsesTerminalEvent(t *testing.T) {
	// 真实上游的终态事件把完整 response 嵌在 `response` 字段下（与 mock 上游一致）。
	terminal := `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-x","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello, world!","annotations":[]}]}],"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}}`
	raw := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"gpt-x","output":[]}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		"",
		`event: response.completed`,
		"data: " + terminal,
		"",
	}, "\n")

	got := decodeAssembled(t, assembleStreamingResponse(raw))
	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("应直接采用终态事件里的完整 response 对象: %#v", got)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(5) {
		t.Fatalf("终态 usage 应被保留，实际 %#v", usage)
	}
	output := got["output"].([]any)
	content := output[0].(map[string]any)["content"].([]any)
	if text := content[0].(map[string]any)["text"]; text != "Hello, world!" {
		t.Fatalf("文本应为终态对象里的值，实际 %v", text)
	}
}

func TestAssembleResponsesStreamWithoutTerminalEvent(t *testing.T) {
	raw := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_2","model":"gpt-y","output":[]}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hi"}`,
		"",
	}, "\n")

	got := decodeAssembled(t, assembleStreamingResponse(raw))
	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("无终态事件时应构造最小完整对象: %#v", got)
	}
	if got["id"] != "resp_2" || got["model"] != "gpt-y" {
		t.Fatalf("id/model 应从其它事件补齐: %#v", got)
	}
	output := got["output"].([]any)
	content := output[0].(map[string]any)["content"].([]any)
	if text := content[0].(map[string]any)["text"]; text != "Hi" {
		t.Fatalf("文本应为 delta 拼接结果，实际 %v", text)
	}
}

func TestAssembleErrorEventReturnsErrorJSON(t *testing.T) {
	raw := strings.Join([]string{
		"event: error",
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		"",
	}, "\n")

	got := assembleStreamingResponse(raw)
	if !strings.Contains(got, "overloaded_error") {
		t.Fatalf("错误事件应原样返回（pretty 打印），实际 %s", got)
	}
	decoded := decodeAssembled(t, got)
	if decoded["type"] != "error" {
		t.Fatalf("应保持 error 结构，实际 %#v", decoded)
	}
}

func TestAssembleUnrecognizedStreamReturnsRaw(t *testing.T) {
	raw := "not an sse stream at all"
	if got := assembleStreamingResponse(raw); got != raw {
		t.Fatalf("无法识别时应原样返回，实际 %q", got)
	}
}
