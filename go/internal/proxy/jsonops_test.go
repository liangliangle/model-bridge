package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"modelbridge/internal/config"
)

// 这些用例逐一对照 Rust `proxy/executor.rs` 的 #[cfg(test)] 模块，
// 保证请求体小改造的行为与重写前一致。

func TestDetectSSEErrorDashscopeFormat(t *testing.T) {
	sse := "id:1\nevent:error\n:HTTP_STATUS/429\ndata:{\"request_id\":\"566e3621\",\"code\":\"Throttling.AllocationQuota\",\"message\":\"Allocated quota exceeded\"}\n\n"
	msg, found := DetectSSEErrorInPrefix([]byte(sse))
	if !found {
		t.Fatal("expected an error to be detected")
	}
	if !strings.Contains(msg, "Throttling.AllocationQuota") && !strings.Contains(msg, "SSE error event") {
		t.Fatalf("unexpected message: %q", msg)
	}
}

func TestDetectSSEErrorAnthropicFormat(t *testing.T) {
	sse := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	msg, found := DetectSSEErrorInPrefix([]byte(sse))
	if !found || !strings.Contains(msg, "Overloaded") {
		t.Fatalf("expected Overloaded error, got found=%v msg=%q", found, msg)
	}
}

func TestDetectSSEErrorOpenAIFormat(t *testing.T) {
	sse := "data: {\"error\":{\"message\":\"Rate limit exceeded\",\"type\":\"rate_limit_error\"}}\n\n"
	msg, found := DetectSSEErrorInPrefix([]byte(sse))
	if !found || !strings.Contains(msg, "Rate limit") {
		t.Fatalf("expected rate limit error, got found=%v msg=%q", found, msg)
	}
}

func TestDetectSSEErrorNormalContentIsNotAnError(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	if msg, found := DetectSSEErrorInPrefix([]byte(sse)); found {
		t.Fatalf("normal content must not be treated as an error, got %q", msg)
	}
}

func TestDetectSSEErrorAnthropicNormalIsNotAnError(t *testing.T) {
	sse := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n"
	if msg, found := DetectSSEErrorInPrefix([]byte(sse)); found {
		t.Fatalf("normal anthropic event must not be treated as an error, got %q", msg)
	}
}

func TestSSEHasContentOutputOpenAI(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	if !SSEHasContentOutput([]byte(sse)) {
		t.Fatal("expected content output")
	}
}

func TestSSEHasContentOutputAnthropic(t *testing.T) {
	sse := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n"
	if !SSEHasContentOutput([]byte(sse)) {
		t.Fatal("expected content output")
	}
}

func TestSSEHasContentOutputErrorOnly(t *testing.T) {
	sse := "id:1\nevent:error\ndata:{\"code\":\"Throttling\",\"message\":\"quota exceeded\"}\n\n"
	if SSEHasContentOutput([]byte(sse)) {
		t.Fatal("error-only stream must not count as content output")
	}
}

func TestSSEHasContentOutputEmptyDelta(t *testing.T) {
	// 只带 role 的 delta 不算内容输出
	sse := "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	if SSEHasContentOutput([]byte(sse)) {
		t.Fatal("role-only delta must not count as content output")
	}
}

func TestStripThinkingBlocksRemovesAllThinking(t *testing.T) {
	raw := []byte(`{
		"model": "claude-3-7",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "old reasoning"},
				{"type": "text", "text": "old answer"}
			]},
			{"role": "user", "content": "again"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "recent reasoning"},
				{"type": "text", "text": "recent answer"}
			]}
		]
	}`)
	out := StripThinkingBlocks(raw)
	got := decodeObject(out)
	msgs, _ := got["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			if block["type"] == "thinking" || block["type"] == "redacted_thinking" {
				t.Fatalf("thinking block survived: %v", block)
			}
		}
	}
}

func TestStripThinkingBlocksAlsoRemovesRedactedThinking(t *testing.T) {
	raw := []byte(`{
		"model": "claude-3-7",
		"thinking": {"type": "enabled", "budget_tokens": 1024},
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "redacted_thinking", "data": "REDACTED"},
				{"type": "text", "text": "answer"}
			]}
		]
	}`)
	out := StripThinkingBlocks(raw)
	got := decodeObject(out)
	msgs, _ := got["messages"].([]any)
	blocks, _ := msgs[1].(map[string]any)["content"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block["type"] == "redacted_thinking" {
			t.Fatal("redacted_thinking must also be removed")
		}
	}
}

func TestReplaceModelInJSONKeepsOtherFields(t *testing.T) {
	raw := []byte(`{"model":"alias","stream":true,"max_tokens":123}`)
	out := decodeObject(ReplaceModelInJSON(raw, "upstream-model"))
	if out["model"] != "upstream-model" {
		t.Fatalf("model not replaced: %v", out["model"])
	}
	if out["stream"] != true {
		t.Fatal("stream field must be preserved")
	}
	if n, ok := out["max_tokens"].(json.Number); !ok || n.String() != "123" {
		t.Fatalf("max_tokens must be preserved verbatim, got %#v", out["max_tokens"])
	}
}

func TestReplaceModelInJSONLeavesInvalidBodyAlone(t *testing.T) {
	raw := []byte("not json")
	if got := string(ReplaceModelInJSON(raw, "m")); got != "not json" {
		t.Fatalf("invalid body must be returned unchanged, got %q", got)
	}
}

func TestSetStreamInJSONRemovesFieldWhenNotStreaming(t *testing.T) {
	out := decodeObject(SetStreamInJSON([]byte(`{"model":"m","stream":true}`), false))
	if _, exists := out["stream"]; exists {
		t.Fatal("stream field must be removed for non-streaming requests")
	}
}

func TestSetStreamInJSONAddsFieldWhenStreaming(t *testing.T) {
	out := decodeObject(SetStreamInJSON([]byte(`{"model":"m"}`), true))
	if out["stream"] != true {
		t.Fatal("stream field must be set to true")
	}
}

func TestSanitizeAnthropicBodyRemovesTemperature(t *testing.T) {
	out := decodeObject(SanitizeAnthropicBody([]byte(`{"model":"m","temperature":0.7,"messages":[]}`)))
	if _, exists := out["temperature"]; exists {
		t.Fatal("temperature must be removed for anthropic channels")
	}
}

func TestForceEffortInJSONCreatesOutputConfig(t *testing.T) {
	out := decodeObject(ForceEffortInJSON([]byte(`{"model":"m"}`), "high"))
	oc, ok := out["output_config"].(map[string]any)
	if !ok || oc["effort"] != "high" {
		t.Fatalf("expected output_config.effort=high, got %#v", out["output_config"])
	}
}

func TestForceEffortInJSONPreservesOtherOutputConfigKeys(t *testing.T) {
	out := decodeObject(ForceEffortInJSON([]byte(`{"output_config":{"format":"json","effort":"low"}}`), "max"))
	oc, _ := out["output_config"].(map[string]any)
	if oc["effort"] != "max" {
		t.Fatalf("effort not overridden: %#v", oc)
	}
	if oc["format"] != "json" {
		t.Fatalf("sibling key lost: %#v", oc)
	}
}

func TestCollectForwardedHeadersSkipsAuthAndHopByHop(t *testing.T) {
	h := http.Header{
		"Authorization":   {"Bearer secret"},
		"X-Api-Key":       {"k"},
		"Host":            {"example.com"},
		"Accept-Encoding": {"gzip"},
		"User-Agent":      {"codex/1.0"},
		"X-Custom":        {"v"},
	}
	ch := &config.ChannelConfig{CustomHeaders: map[string]string{"X-Extra": "1", "X-Custom": "override"}}
	dst := map[string]string{}
	CollectForwardedHeaders(dst, h, ch)

	for _, forbidden := range []string{"authorization", "x-api-key", "host", "accept-encoding"} {
		if _, exists := dst[forbidden]; exists {
			t.Fatalf("header %q must not be forwarded", forbidden)
		}
	}
	if dst["user-agent"] != "codex/1.0" {
		t.Fatalf("user-agent should be forwarded, got %q", dst["user-agent"])
	}
	if dst["x-custom"] != "override" {
		t.Fatalf("custom_headers must override forwarded headers, got %q", dst["x-custom"])
	}
	if dst["x-extra"] != "1" {
		t.Fatalf("custom header missing: %#v", dst)
	}
}

func TestPrettyJSONFallsBackToRaw(t *testing.T) {
	if got := PrettyJSON("not json"); got != "not json" {
		t.Fatalf("expected raw fallback, got %q", got)
	}
	if got := PrettyJSON(`{"a":1}`); !strings.Contains(got, "\n") {
		t.Fatalf("expected indented output, got %q", got)
	}
}
