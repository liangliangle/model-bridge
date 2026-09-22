package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"modelbridge/internal/audit"
	"modelbridge/internal/config"
	"modelbridge/internal/sse"
)

// 协议矩阵的端到端验证：3 入口 × 3 渠道协议 = 9 格，每格都跑非流式与流式。
//
// 每格断言四件事：
//  1. 上游收到的是**渠道协议**形态的请求体（而不是入口形态）；
//  2. 下游收到的是**入口协议**形态的响应体 / 事件流，且正文来自上游；
//  3. 流式时上游请求带 stream=true，usage 用的是上游真实值；
//  4. 审计落库，且记下了实际使用的渠道。

// matrixClients 描述三个入口：路径、请求体、非流式与流式响应的形态特征。
var matrixClients = map[string]struct {
	path      string
	body      string
	nonStream string // 非流式响应里必须出现的特征
	stream    string // 流式响应里必须出现的特征
}{
	"chat": {
		path:      "/v1/chat/completions",
		body:      chatRequestBodyWithSystem,
		nonStream: `"object":"chat.completion"`,
		stream:    "chat.completion.chunk",
	},
	"messages": {
		path:      "/v1/messages",
		body:      messagesRequestBody,
		nonStream: `"type":"message"`,
		stream:    "message_start",
	},
	"responses": {
		path:      "/v1/responses",
		body:      responsesRequestBody,
		nonStream: `"object":"response"`,
		stream:    "response.created",
	},
}

// matrixChannels 描述三个渠道协议：provider 与 mock 上游路径。
var matrixChannels = map[string]struct {
	provider config.ProviderType
	mockPath string
}{
	"chat":      {config.ProviderOpenAI, "/chat"},
	"messages":  {config.ProviderAnthropic, "/messages"},
	"responses": {config.ProviderOpenAIResponses, "/responses"},
}

// chatRequestBodyWithSystem 是带 system 消息的 Chat 请求（与另外两个入口的请求体对齐，
// 便于用同一组判别式断言出站协议）。
const chatRequestBodyWithSystem = `{
  "model": "gpt-test",
  "max_tokens": 64,
  "messages": [
    {"role": "system", "content": "be brief"},
    {"role": "user", "content": "hi"}
  ],
  "stream": %s
}`

// newMatrixHarness 建一个只有一个渠道的 harness，渠道指向 mock 的对应协议路径。
func newMatrixHarness(t *testing.T, channelKey string) *harness {
	t.Helper()
	spec := matrixChannels[channelKey]
	h := newHarness(t, channelSpec{"ch-" + channelKey, spec.provider, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL(spec.mockPath)
		return nil
	})
	return h
}

// assertUpstreamProtocol 断言上游收到的请求体确实是**渠道协议**形态。
//
// 判别式刻意用「只有该协议才有的字段」：三种协议的请求体都带 model / stream，
// 因此不能靠这些通用字段区分。
func assertUpstreamProtocol(t *testing.T, req RecordedRequest, channelKey string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v\n%s", err, req.Body)
	}
	_, hasMessages := body["messages"]
	_, hasInput := body["input"]
	_, hasSystem := body["system"]
	_, hasInstructions := body["instructions"]
	_, hasMaxTokens := body["max_tokens"]

	switch channelKey {
	case "chat":
		// Chat 形态：messages 数组，没有 Anthropic 的 system / Responses 的 input。
		if !hasMessages || hasInput || hasSystem {
			t.Fatalf("上游应收到 Chat 形态请求体，实际 %s", req.Body)
		}
	case "messages":
		// Anthropic 形态：system + 必填的 max_tokens，没有 Responses 的 input。
		if hasInput || !hasSystem || !hasMaxTokens {
			t.Fatalf("上游应收到 Anthropic 形态请求体，实际 %s", req.Body)
		}
	case "responses":
		// Responses 形态：input 数组 + instructions，没有 Chat 的 messages。
		if hasMessages || !hasInput || !hasInstructions {
			t.Fatalf("上游应收到 Responses 形态请求体，实际 %s", req.Body)
		}
	}
}

// assertClientNonStreamBody 断言下游响应体是**入口协议**形态且正文来自上游。
func assertClientNonStreamBody(t *testing.T, clientKey string, body map[string]any) {
	t.Helper()
	switch clientKey {
	case "chat":
		if body["object"] != "chat.completion" {
			t.Fatalf("下游应为 chat.completion，实际 %#v", body["object"])
		}
		if got := chatMessageText(t, body); got != mockContent {
			t.Fatalf("正文 = %q, want %q", got, mockContent)
		}
	case "messages":
		if body["type"] != "message" {
			t.Fatalf("下游应为 message 形态，实际 %#v", body["type"])
		}
		if got := extractBlockText(t, body); got != mockContent {
			t.Fatalf("正文 = %q, want %q", got, mockContent)
		}
	case "responses":
		if body["object"] != "response" {
			t.Fatalf("下游应为 response 形态，实际 %#v", body["object"])
		}
		if got := responsesOutputText(t, body); got != mockContent {
			t.Fatalf("正文 = %q, want %q", got, mockContent)
		}
	}
}

// assertClientStream 断言下游事件流是**入口协议**形态，并返回是否看到真实 usage。
func assertClientStream(t *testing.T, clientKey string, raw string) {
	t.Helper()
	events := parseSSE(t, raw)
	switch clientKey {
	case "chat":
		if !strings.Contains(raw, "chat.completion.chunk") {
			t.Fatalf("下游应为 chat chunk 流，实际:\n%s", raw)
		}
		if got := chatStreamText(t, events); got != mockContent {
			t.Fatalf("正文 = %q, want %q", got, mockContent)
		}
		if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
			t.Fatalf("Chat 方向应以 [DONE] 结束")
		}
	case "messages":
		if got := anthropicStreamText(t, events); got != mockContent {
			t.Fatalf("正文 = %q, want %q", got, mockContent)
		}
		// usage 出现的位置取决于是否发生转换：真实 Anthropic 上游把输入量放在
		// message_start，转换路径（Chat 上游只在末尾给累计值）把它放在 message_delta。
		// 这里只断言「下游能看到上游的真实用量」，不限定它落在哪个事件上。
		if got := anthropicStreamInputTokens(t, events); got != mockPromptTokens {
			t.Fatalf("input tokens = %v, want %d（上游真实值）", got, mockPromptTokens)
		}
		if got := anthropicStreamOutputTokens(t, events); got != mockCompletionTokens {
			t.Fatalf("output tokens = %v, want %d", got, mockCompletionTokens)
		}
	case "responses":
		text, usage := responsesStreamSummary(t, events)
		if text != mockContent {
			t.Fatalf("正文 = %q, want %q", text, mockContent)
		}
		if got := num(t, usage["input_tokens"], "response.completed usage.input_tokens"); got != mockPromptTokens {
			t.Fatalf("response.completed.input_tokens = %v, want %d", got, mockPromptTokens)
		}
	}
}

// assertAuditRow 断言审计落库且记下了实际渠道。
func assertAuditRow(t *testing.T, h *harness, channelID string) {
	t.Helper()
	rows, err := h.state.Audit.QueryList(audit.QueryListParams{Limit: 10})
	if err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("审计没有落库")
	}
	for _, row := range rows {
		if row.ActualChannel != nil && *row.ActualChannel == channelID {
			return
		}
	}
	t.Fatalf("审计里没有渠道 %q 的记录：%+v", channelID, rows)
}

// ==================== 非流式：9 格 ====================

func TestProtocolMatrixNonStream(t *testing.T) {
	for clientKey, client := range matrixClients {
		for channelKey := range matrixChannels {
			t.Run(clientKey+"→"+channelKey, func(t *testing.T) {
				h := newMatrixHarness(t, channelKey)

				resp, raw := h.post(client.path, body(client.body, false))
				if resp.StatusCode != 200 {
					t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
				}

				up, ok := h.mock.LastRequest()
				if !ok {
					t.Fatal("上游没有收到请求")
				}
				assertUpstreamModel(t, up)
				assertUpstreamProtocol(t, up, channelKey)

				if !strings.Contains(raw, client.nonStream) {
					t.Fatalf("下游响应不是入口协议形态（应含 %s）：\n%s", client.nonStream, raw)
				}
				assertClientNonStreamBody(t, clientKey, decode(t, raw))
				assertAuditRow(t, h, "ch-"+channelKey)

				writeEvidenceJSON(t, "matrix-nonstream-"+clientKey+"-"+channelKey+".json", decode(t, raw))
			})
		}
	}
}

// ==================== 流式：9 格 ====================

func TestProtocolMatrixStream(t *testing.T) {
	for clientKey, client := range matrixClients {
		for channelKey := range matrixChannels {
			t.Run(clientKey+"→"+channelKey, func(t *testing.T) {
				h := newMatrixHarness(t, channelKey)

				resp, raw := h.post(client.path, body(client.body, true))
				if resp.StatusCode != 200 {
					t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
				}
				if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
					t.Fatalf("流式响应 Content-Type 应为 text/event-stream，实际 %s", ct)
				}

				up, ok := h.mock.LastRequest()
				if !ok {
					t.Fatal("上游没有收到请求")
				}
				assertUpstreamModel(t, up)
				assertUpstreamProtocol(t, up, channelKey)
				var upBody map[string]any
				if err := json.Unmarshal(up.Body, &upBody); err != nil {
					t.Fatalf("上游请求体不是合法 JSON: %v", err)
				}
				if upBody["stream"] != true {
					t.Fatalf("入口是流式，上游请求应带 stream=true，实际 %v", upBody["stream"])
				}

				if !strings.Contains(raw, client.stream) {
					t.Fatalf("下游事件流不是入口协议形态（应含 %s）：\n%s", client.stream, raw)
				}
				assertClientStream(t, clientKey, raw)
				assertAuditRow(t, h, "ch-"+channelKey)

				writeEvidence(t, "matrix-stream-"+clientKey+"-"+channelKey+".sse", []byte(raw))
			})
		}
	}
}

// TestTwoHopCombinationsAreConversions 明确固定「两个富协议之间是两跳转换」这一事实：
// messages ↔ responses 两格都必须发生真实转换（而不是被误当成透传或直接报错）。
func TestTwoHopCombinationsAreConversions(t *testing.T) {
	cases := []struct {
		clientKey  string
		channelKey string
	}{
		{"messages", "responses"},
		{"responses", "messages"},
	}

	for _, tc := range cases {
		t.Run(tc.clientKey+"→"+tc.channelKey, func(t *testing.T) {
			h := newMatrixHarness(t, tc.channelKey)
			client := matrixClients[tc.clientKey]

			resp, raw := h.post(client.path, body(client.body, false))
			if resp.StatusCode != 200 {
				t.Fatalf("两跳组合应当可用，实际 %d: %s", resp.StatusCode, raw)
			}
			up, _ := h.mock.LastRequest()
			// 出站体必须是渠道协议（即确实发生了转换）。
			assertUpstreamProtocol(t, up, tc.channelKey)
			// 下游体必须是入口协议。
			if !strings.Contains(raw, client.nonStream) {
				t.Fatalf("下游响应不是入口协议形态：\n%s", raw)
			}
			// 按入口协议取正文（两种富协议的正文位置不同，不能混用提取器）。
			switch tc.clientKey {
			case "messages":
				if got := extractBlockText(t, decode(t, raw)); got != mockContent {
					t.Fatalf("两跳正文 = %q, want %q", got, mockContent)
				}
			case "responses":
				if got := responsesOutputText(t, decode(t, raw)); got != mockContent {
					t.Fatalf("两跳正文 = %q, want %q", got, mockContent)
				}
			}
		})
	}
}

// ==================== 断言辅助 ====================

// extractBlockText 取 Anthropic 形态响应里 text 块的文本。
func extractBlockText(t *testing.T, body map[string]any) string {
	t.Helper()
	blocks := asList(t, body["content"], "content")
	var b strings.Builder
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := block["text"].(string); ok {
			b.WriteString(text)
		}
	}
	return b.String()
}

// chatMessageText 取 Chat 形态响应里 choices[0].message.content。
func chatMessageText(t *testing.T, body map[string]any) string {
	t.Helper()
	choices := asList(t, body["choices"], "choices")
	if len(choices) == 0 {
		t.Fatal("choices 为空")
	}
	choice := choices[0].(map[string]any)
	message, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatalf("choices[0].message 不是对象：%#v", choice["message"])
	}
	text, _ := message["content"].(string)
	return text
}

// anthropicStreamUsage 汇总 Anthropic 事件流里的用量：message_start.message.usage
// 与 message_delta.usage 都可能携带（取决于上游是原生 Anthropic 还是被转换过）。
func anthropicStreamUsage(t *testing.T, events []sse.Event) map[string]any {
	t.Helper()
	merged := map[string]any{}
	for _, ev := range events {
		payload := eventPayloadOf(t, ev)
		switch payload["type"] {
		case "message_start":
			if message, ok := payload["message"].(map[string]any); ok {
				if usage, ok := message["usage"].(map[string]any); ok {
					for k, v := range usage {
						merged[k] = v
					}
				}
			}
		case "message_delta":
			if usage, ok := payload["usage"].(map[string]any); ok {
				for k, v := range usage {
					merged[k] = v
				}
			}
		}
	}
	return merged
}

// anthropicStreamInputTokens / OutputTokens 从汇总用量里取输入 / 输出量。
func anthropicStreamInputTokens(t *testing.T, events []sse.Event) float64 {
	t.Helper()
	return num(t, anthropicStreamUsage(t, events)["input_tokens"], "anthropic input_tokens")
}

func anthropicStreamOutputTokens(t *testing.T, events []sse.Event) float64 {
	t.Helper()
	return num(t, anthropicStreamUsage(t, events)["output_tokens"], "anthropic output_tokens")
}

// eventPayloadOf 解析一个事件的 JSON 载荷（空载荷返回空对象）。
func eventPayloadOf(t *testing.T, ev sse.Event) map[string]any {
	t.Helper()
	if ev.Data == "" {
		return map[string]any{}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("事件载荷不是合法 JSON: %v\n%s", err, ev.Data)
	}
	return payload
}

// responsesOutputText 取 Responses 形态响应里 message 项的 output_text。
func responsesOutputText(t *testing.T, body map[string]any) string {
	t.Helper()
	items := asList(t, body["output"], "output")
	var b strings.Builder
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "message" {
			continue
		}
		parts, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, praw := range parts {
			part, ok := praw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}
