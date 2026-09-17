package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"modelbridge/internal/config"
)

// ==================== 非流式 ====================

func TestNonStreamMessagesToMessagesChannel(t *testing.T) {
	h := newHarness(t, channelSpec{"messages-ch", config.ProviderAnthropic, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL("/messages")
		return nil
	})

	resp, raw := h.post("/v1/messages", body(messagesRequestBody, false))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}

	up, ok := h.mock.LastRequest()
	if !ok {
		t.Fatal("mock 上游没有收到请求")
	}
	if up.Path != "/messages" {
		t.Fatalf("应透传到 messages 渠道，实际 %s", up.Path)
	}
	assertUpstreamModel(t, up)

	down := assertNonStreamJSON(t, "application/json", resp, raw)
	if down["type"] != "message" {
		t.Fatalf("透传响应应为 Anthropic message，实际 %v", down["type"])
	}
	content := asList(t, down["content"], "content")
	if text := content[0].(map[string]any)["text"]; text != mockContent {
		t.Fatalf("文本内容应为 %q，实际 %v", mockContent, text)
	}
	usage := down["usage"].(map[string]any)
	if got := num(t, usage["input_tokens"], "input_tokens"); got != mockPromptTokens {
		t.Fatalf("input_tokens 应为 %d，实际 %v", mockPromptTokens, got)
	}

	writeEvidenceJSON(t, "nonstream-messages.json", map[string]any{
		"case":              "messages 入口 → messages 渠道（透传）",
		"downstream_status": resp.StatusCode,
		"upstream_request":  json.RawMessage(up.Body),
		"downstream_body":   json.RawMessage(raw),
	})
}

func TestNonStreamMessagesToChatChannel(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
		return nil
	})

	resp, raw := h.post("/v1/messages", body(messagesRequestBody, false))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}

	up, _ := h.mock.LastRequest()
	if up.Path != "/chat" {
		t.Fatalf("应转换后发往 chat 渠道，实际 %s", up.Path)
	}
	assertUpstreamModel(t, up)

	// 发往上游的请求体必须是 Chat 形状：system 已折进 messages。
	var upBody map[string]any
	if err := json.Unmarshal(up.Body, &upBody); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v", err)
	}
	msgs := asList(t, upBody["messages"], "messages")
	if len(msgs) == 0 {
		t.Fatal("上游 messages 为空")
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("system 应折进首条 system 消息，实际 %v", first["role"])
	}
	if first["content"] != "be brief" {
		t.Fatalf("system 内容应为 be brief，实际 %v", first["content"])
	}
	if _, hasSystem := upBody["system"]; hasSystem {
		t.Fatal("Chat 请求体不应保留顶层 system 字段")
	}

	// 下游必须是 Anthropic message 对象，内容来自上游。
	down := assertNonStreamJSON(t, "application/json", resp, raw)
	if down["type"] != "message" {
		t.Fatalf("下游应为 Anthropic message，实际 %v", down["type"])
	}
	content := asList(t, down["content"], "content")
	if text := content[0].(map[string]any)["text"]; text != mockContent {
		t.Fatalf("文本内容应为 %q，实际 %v", mockContent, text)
	}
	usage := down["usage"].(map[string]any)
	if got := num(t, usage["input_tokens"], "input_tokens"); got != mockPromptTokens {
		t.Fatalf("usage.input_tokens 应为 %d，实际 %v", mockPromptTokens, got)
	}
	if got := num(t, usage["output_tokens"], "output_tokens"); got != mockCompletionTokens {
		t.Fatalf("usage.output_tokens 应为 %d，实际 %v", mockCompletionTokens, got)
	}

	writeEvidenceJSON(t, "nonstream-messages.json", map[string]any{
		"case":              "messages 入口 → chat 渠道（协议转换）",
		"downstream_status": resp.StatusCode,
		"upstream_request":  json.RawMessage(up.Body),
		"downstream_body":   json.RawMessage(raw),
	})
}

func TestNonStreamResponsesEntry(t *testing.T) {
	t.Run("to_responses_channel", func(t *testing.T) {
		h := newHarness(t, channelSpec{"responses-ch", config.ProviderOpenAIResponses, "PLACEHOLDER", 1})
		h.state.Update(func(cfg *config.AppConfig) error {
			cfg.Channels[0].Endpoint.URL = h.mock.URL("/responses")
			return nil
		})

		resp, raw := h.post("/v1/responses", body(responsesRequestBody, false))
		if resp.StatusCode != 200 {
			t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
		}
		up, _ := h.mock.LastRequest()
		if up.Path != "/responses" {
			t.Fatalf("应透传到 responses 渠道，实际 %s", up.Path)
		}
		down := assertNonStreamJSON(t, "application/json", resp, raw)
		if down["object"] != "response" {
			t.Fatalf("下游应为 Responses 对象，实际 %v", down["object"])
		}
		if got := outputText(t, down); got != mockContent {
			t.Fatalf("文本内容应为 %q，实际 %v", mockContent, got)
		}
		writeEvidenceJSON(t, "nonstream-responses.json", map[string]any{
			"case":              "responses 入口 → responses 渠道（透传）",
			"downstream_status": resp.StatusCode,
			"upstream_request":  json.RawMessage(up.Body),
			"downstream_body":   json.RawMessage(raw),
		})
	})

	t.Run("to_chat_channel", func(t *testing.T) {
		h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
		h.state.Update(func(cfg *config.AppConfig) error {
			cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
			return nil
		})

		resp, raw := h.post("/v1/responses", body(responsesRequestBody, false))
		if resp.StatusCode != 200 {
			t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
		}
		up, _ := h.mock.LastRequest()
		if up.Path != "/chat" {
			t.Fatalf("应转换后发往 chat 渠道，实际 %s", up.Path)
		}
		assertUpstreamModel(t, up)

		var upBody map[string]any
		if err := json.Unmarshal(up.Body, &upBody); err != nil {
			t.Fatalf("上游请求体不是合法 JSON: %v", err)
		}
		msgs := asList(t, upBody["messages"], "messages")
		if len(msgs) == 0 {
			t.Fatal("上游 messages 为空")
		}
		if role := msgs[0].(map[string]any)["role"]; role != "system" {
			t.Fatalf("instructions 应折进 system 消息，实际首条角色 %v", role)
		}
		if _, hasInput := upBody["input"]; hasInput {
			t.Fatal("Chat 请求体不应保留 Responses 的 input 字段")
		}

		down := assertNonStreamJSON(t, "application/json", resp, raw)
		if down["object"] != "response" {
			t.Fatalf("下游应为 Responses 对象，实际 %v", down["object"])
		}
		if got := outputText(t, down); got != mockContent {
			t.Fatalf("文本内容应为 %q，实际 %v", mockContent, got)
		}
		usage, ok := down["usage"].(map[string]any)
		if !ok {
			t.Fatalf("Responses 下游必须带 usage，实际 %#v", down["usage"])
		}
		if got := num(t, usage["input_tokens"], "input_tokens"); got != mockPromptTokens {
			t.Fatalf("usage.input_tokens 应为 %d，实际 %v", mockPromptTokens, got)
		}
		if got := num(t, usage["output_tokens"], "output_tokens"); got != mockCompletionTokens {
			t.Fatalf("usage.output_tokens 应为 %d，实际 %v", mockCompletionTokens, got)
		}

		writeEvidenceJSON(t, "nonstream-responses.json", map[string]any{
			"case":              "responses 入口 → chat 渠道（协议转换）",
			"downstream_status": resp.StatusCode,
			"upstream_request":  json.RawMessage(up.Body),
			"downstream_body":   json.RawMessage(raw),
		})
	})
}

func TestNonStreamChatEntry(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
		return nil
	})

	resp, raw := h.post("/v1/chat/completions", body(chatRequestBody, false))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	up, _ := h.mock.LastRequest()
	assertUpstreamModel(t, up)

	var upBody map[string]any
	_ = json.Unmarshal(up.Body, &upBody)
	if _, hasMessages := upBody["messages"]; !hasMessages {
		t.Fatal("透传请求体应保留 messages")
	}
	// 透传路径保持客户端原始请求体：客户端没要 stream_options，就不应被注入。
	if _, injected := upBody["stream_options"]; injected {
		t.Fatal("透传路径不应注入 stream_options（与重写前行为一致）")
	}

	down := assertNonStreamJSON(t, "application/json", resp, raw)
	choices := asList(t, down["choices"], "choices")
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != mockContent {
		t.Fatalf("文本内容应为 %q，实际 %v", mockContent, msg["content"])
	}

	writeEvidenceJSON(t, "nonstream-chat.json", map[string]any{
		"case":              "chat 入口 → chat 渠道（透传）",
		"downstream_status": resp.StatusCode,
		"upstream_request":  json.RawMessage(up.Body),
		"downstream_body":   json.RawMessage(raw),
	})
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// outputText 取出 Responses 对象里的第一段文本。
func outputText(t *testing.T, resp map[string]any) string {
	t.Helper()
	output := asList(t, resp["output"], "output")
	if len(output) == 0 {
		t.Fatal("output 为空")
	}
	item := output[0].(map[string]any)
	content := asList(t, item["content"], "output[0].content")
	if len(content) == 0 {
		t.Fatal("output[0].content 为空")
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

// ==================== 流式 ====================

func TestStreamMessagesEntry(t *testing.T) {
	t.Run("to_chat_channel", func(t *testing.T) {
		h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
		h.state.Update(func(cfg *config.AppConfig) error {
			cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
			return nil
		})

		resp, raw := h.post("/v1/messages", body(messagesRequestBody, true))
		if resp.StatusCode != 200 {
			t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Fatalf("流式响应 Content-Type 应为 text/event-stream，实际 %s", ct)
		}

		up, _ := h.mock.LastRequest()
		var upBody map[string]any
		if err := json.Unmarshal(up.Body, &upBody); err != nil {
			t.Fatalf("上游请求体不是合法 JSON: %v", err)
		}
		if upBody["stream"] != true {
			t.Fatalf("上游请求应带 stream=true，实际 %v", upBody["stream"])
		}
		// 转换路径必须显式要求上游下发 usage，否则下游 usage 恒为 0。
		so, ok := upBody["stream_options"].(map[string]any)
		if !ok || so["include_usage"] != true {
			t.Fatalf("转换路径应注入 stream_options.include_usage=true，实际 %#v", upBody["stream_options"])
		}

		events := parseSSE(t, raw)
		types := sseTypes(t, events)
		want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
		if strings.Join(types, ",") != strings.Join(want, ",") {
			t.Fatalf("事件序列不正确\n实际: %v\n期望: %v", types, want)
		}

		if got := anthropicStreamText(t, events); got != mockContent {
			t.Fatalf("增量拼接应为 %q，实际 %q", mockContent, got)
		}
		// message_start 必须带 usage 对象（Anthropic 协议要求）；此时上游的 usage
		// 尾部 chunk 还没到，所以数值可以是 0 —— 与 Rust 实现一致。
		if !anthropicStreamStartHasUsage(t, events) {
			t.Fatal("message_start 必须带 usage 对象")
		}
		// 真实用量必须落在 message_delta 上：Chat 上游只在结束时给一次累计值，
		// 转换器把它推迟到终止事件，而不是写死 0。
		deltaUsage := anthropicStreamDeltaUsage(t, events)
		if got := num(t, deltaUsage["input_tokens"], "message_delta usage.input_tokens"); got != mockPromptTokens {
			t.Fatalf("message_delta 的 input_tokens 应为 %d（真实值，非硬编码 0），实际 %v", mockPromptTokens, got)
		}
		if got := num(t, deltaUsage["output_tokens"], "message_delta usage.output_tokens"); got != mockCompletionTokens {
			t.Fatalf("message_delta 的 output_tokens 应为 %d（真实值，非硬编码 0），实际 %v", mockCompletionTokens, got)
		}

		writeEvidence(t, "stream-messages.sse", []byte(raw))
	})

	t.Run("to_messages_channel", func(t *testing.T) {
		h := newHarness(t, channelSpec{"messages-ch", config.ProviderAnthropic, "PLACEHOLDER", 1})
		h.state.Update(func(cfg *config.AppConfig) error {
			cfg.Channels[0].Endpoint.URL = h.mock.URL("/messages")
			return nil
		})

		resp, raw := h.post("/v1/messages", body(messagesRequestBody, true))
		if resp.StatusCode != 200 {
			t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Fatalf("流式响应 Content-Type 应为 text/event-stream，实际 %s", ct)
		}
		events := parseSSE(t, raw)
		types := sseTypes(t, events)
		want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
		if strings.Join(types, ",") != strings.Join(want, ",") {
			t.Fatalf("透传事件序列不正确\n实际: %v\n期望: %v", types, want)
		}
		if got := anthropicStreamText(t, events); got != mockContent {
			t.Fatalf("增量拼接应为 %q，实际 %q", mockContent, got)
		}
	})
}

func TestStreamResponsesEntry(t *testing.T) {
	t.Run("to_chat_channel", func(t *testing.T) {
		h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
		h.state.Update(func(cfg *config.AppConfig) error {
			cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
			return nil
		})

		resp, raw := h.post("/v1/responses", body(responsesRequestBody, true))
		if resp.StatusCode != 200 {
			t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Fatalf("流式响应 Content-Type 应为 text/event-stream，实际 %s", ct)
		}

		up, _ := h.mock.LastRequest()
		var upBody map[string]any
		_ = json.Unmarshal(up.Body, &upBody)
		so, ok := upBody["stream_options"].(map[string]any)
		if !ok || so["include_usage"] != true {
			t.Fatalf("responses→chat 转换路径应注入 include_usage，实际 %#v", upBody["stream_options"])
		}

		events := parseSSE(t, raw)
		types := sseTypes(t, events)
		if len(types) == 0 {
			t.Fatalf("下游没有任何事件；原始响应：\n%s", raw)
		}
		if types[0] != "response.created" {
			t.Fatalf("首个事件应为 response.created，实际 %v", types)
		}
		if !containsString(types, "response.completed") {
			t.Fatalf("必须出现 response.completed，实际 %v", types)
		}
		text, usage := responsesStreamSummary(t, events)
		if text != mockContent {
			t.Fatalf("增量拼接应为 %q，实际 %q", mockContent, text)
		}
		if usage == nil {
			t.Fatal("response.completed 必须带 usage")
		}
		if got := num(t, usage["input_tokens"], "input_tokens"); got != mockPromptTokens {
			t.Fatalf("usage.input_tokens 应为 %d，实际 %v", mockPromptTokens, got)
		}
		if got := num(t, usage["output_tokens"], "output_tokens"); got != mockCompletionTokens {
			t.Fatalf("usage.output_tokens 应为 %d（真实值，非硬编码 0），实际 %v", mockCompletionTokens, got)
		}

		writeEvidence(t, "stream-responses.sse", []byte(raw))
	})

	t.Run("to_responses_channel", func(t *testing.T) {
		h := newHarness(t, channelSpec{"responses-ch", config.ProviderOpenAIResponses, "PLACEHOLDER", 1})
		h.state.Update(func(cfg *config.AppConfig) error {
			cfg.Channels[0].Endpoint.URL = h.mock.URL("/responses")
			return nil
		})

		resp, raw := h.post("/v1/responses", body(responsesRequestBody, true))
		if resp.StatusCode != 200 {
			t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
		}
		events := parseSSE(t, raw)
		text, usage := responsesStreamSummary(t, events)
		if text != mockContent {
			t.Fatalf("透传增量拼接应为 %q，实际 %q", mockContent, text)
		}
		if usage == nil || num(t, usage["output_tokens"], "output_tokens") != mockCompletionTokens {
			t.Fatalf("透传应保留 usage，实际 %#v", usage)
		}
	})
}

func TestStreamChatEntry(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
		return nil
	})

	resp, raw := h.post("/v1/chat/completions", body(chatRequestBody, true))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("流式响应 Content-Type 应为 text/event-stream，实际 %s", ct)
	}
	events := parseSSE(t, raw)
	if len(events) == 0 {
		t.Fatal("下游没有任何事件")
	}
	if got := chatStreamText(t, events); got != mockContent {
		t.Fatalf("增量拼接应为 %q，实际 %q", mockContent, got)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Fatal("透传必须保留 [DONE] 结束标志")
	}
	writeEvidence(t, "stream-chat.sse", []byte(raw))
}

// ==================== 事件解析小工具 ====================

func anthropicStreamText(t *testing.T, events []sseEvent) string {
	t.Helper()
	var sb strings.Builder
	for _, ev := range events {
		var payload map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue
		}
		if payload["type"] != "content_block_delta" {
			continue
		}
		delta, _ := payload["delta"].(map[string]any)
		if text, ok := delta["text"].(string); ok {
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// anthropicStreamStartHasUsage 断言 message_start.message.usage 存在且带必需字段。
func anthropicStreamStartHasUsage(t *testing.T, events []sseEvent) bool {
	t.Helper()
	for _, ev := range events {
		var payload map[string]any
		_ = json.Unmarshal([]byte(ev.Data), &payload)
		if payload["type"] != "message_start" {
			continue
		}
		msg, _ := payload["message"].(map[string]any)
		usage, ok := msg["usage"].(map[string]any)
		if !ok {
			return false
		}
		_, hasIn := usage["input_tokens"]
		_, hasOut := usage["output_tokens"]
		return hasIn && hasOut
	}
	t.Fatal("没有找到 message_start")
	return false
}

// anthropicStreamDeltaUsage 返回 message_delta 的累计 usage。
func anthropicStreamDeltaUsage(t *testing.T, events []sseEvent) map[string]any {
	t.Helper()
	for _, ev := range events {
		var payload map[string]any
		_ = json.Unmarshal([]byte(ev.Data), &payload)
		if payload["type"] != "message_delta" {
			continue
		}
		usage, ok := payload["usage"].(map[string]any)
		if !ok {
			t.Fatalf("message_delta 必须带 usage: %s", ev.Data)
		}
		return usage
	}
	t.Fatal("没有找到 message_delta")
	return nil
}

// responsesStreamSummary 汇总 Responses 事件流：增量文本 + 终态 usage。
func responsesStreamSummary(t *testing.T, events []sseEvent) (string, map[string]any) {
	t.Helper()
	var sb strings.Builder
	var usage map[string]any
	for _, ev := range events {
		var payload map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue
		}
		switch payload["type"] {
		case "response.output_text.delta":
			if delta, ok := payload["delta"].(string); ok {
				sb.WriteString(delta)
			}
		case "response.completed":
			resp, _ := payload["response"].(map[string]any)
			if u, ok := resp["usage"].(map[string]any); ok {
				usage = u
			}
		}
	}
	return sb.String(), usage
}

func chatStreamText(t *testing.T, events []sseEvent) string {
	t.Helper()
	var sb strings.Builder
	for _, ev := range events {
		if ev.Data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue
		}
		choices, ok := payload["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		if text, ok := delta["content"].(string); ok {
			sb.WriteString(text)
		}
	}
	return sb.String()
}
