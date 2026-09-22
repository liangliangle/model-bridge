package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"modelbridge/internal/audit"
	"modelbridge/internal/config"
	"modelbridge/internal/sse"
)

// 流式的边界情形：首块错误要不要换渠道、上游回 JSON 怎么办、工具与推理内容怎么搬。
// 这些路径此前没有端到端覆盖（详见 mock_upstream.go 的查询参数说明）。

// ==================== 首块错误 → 换渠道 ====================

// TestStreamFirstChunkErrorFailsOver 首块只有错误事件、且还没有任何内容时，
// 必须认为「还没写给客户端」，从而换到下一个渠道重试。
//
// 形态与路径刻意正交：给 Anthropic 渠道配 Responses 形态的错误，验证判定是按
// **事件形态**而不是按路径做的（矩阵全开后上游协议与入口协议可以不同）。
func TestStreamFirstChunkErrorFailsOver(t *testing.T) {
	shapes := []string{"event", "chat", "anthropic", "responses", "failed"}
	for _, shape := range shapes {
		t.Run("shape="+shape, func(t *testing.T) {
			h := newHarness(t,
				channelSpec{"bad", config.ProviderAnthropic, "PLACEHOLDER", 1},
				channelSpec{"good", config.ProviderAnthropic, "PLACEHOLDER", 2},
			)
			h.state.Update(func(cfg *config.AppConfig) error {
				cfg.Channels[0].Endpoint.URL = h.mock.URLWith("/messages", "error="+shape)
				cfg.Channels[1].Endpoint.URL = h.mock.URL("/messages")
				return nil
			})

			resp, raw := h.post("/v1/messages", body(messagesRequestBody, true))
			if resp.StatusCode != 200 {
				t.Fatalf("期望 200（换渠道后成功），实际 %d: %s", resp.StatusCode, raw)
			}

			paths := h.mock.RequestPaths()
			if len(paths) != 2 {
				t.Fatalf("应当尝试两个渠道（首个报错后换渠道），实际请求 %v", paths)
			}
			if got := anthropicStreamText(t, parseSSE(t, raw)); got != mockContent {
				t.Fatalf("应当由第二个渠道产出内容 %q，实际 %q", mockContent, got)
			}

			// 审计必须记下这次失败尝试（否则用户看不到上游其实报过错）。
			assertAuditErrorMentions(t, h, shape)
		})
	}
}

// TestStreamErrorAfterContentDoesNotFailOver 反向用例：首块里已经有内容时不得换渠道，
// 否则客户端会看到两段拼接的输出。此时错误事件原样透传给客户端。
func TestStreamErrorAfterContentDoesNotFailOver(t *testing.T) {
	h := newHarness(t,
		channelSpec{"bad", config.ProviderAnthropic, "PLACEHOLDER", 1},
		channelSpec{"good", config.ProviderAnthropic, "PLACEHOLDER", 2},
	)
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URLWith("/messages", "error=event&err_after=1")
		cfg.Channels[1].Endpoint.URL = h.mock.URL("/messages")
		return nil
	})

	resp, raw := h.post("/v1/messages", body(messagesRequestBody, true))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	if paths := h.mock.RequestPaths(); len(paths) != 1 {
		t.Fatalf("已经有内容下发时不得换渠道，实际请求 %v", paths)
	}
	if !strings.Contains(raw, "event:error") {
		t.Fatalf("上游的错误事件应当原样透传给客户端，实际:\n%s", raw)
	}
	// 内容前缀已经下发（这正是不能换渠道的原因）。
	if !strings.Contains(raw, "partial") {
		t.Fatalf("首块的内容应当已经下发，实际:\n%s", raw)
	}
}

// ==================== 上游对流式请求回 JSON → 降级重放 ====================

// TestStreamDegradesToReplayWhenUpstreamReturnsJSON 上游忽略 stream=true、
// 直接回一个完整 JSON 体时，代理要把它重放成入口协议的 SSE 事件流（而不是把
// 一个 JSON 体当 SSE 丢给客户端）。
func TestStreamDegradesToReplayWhenUpstreamReturnsJSON(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URLWith("/chat", "sse=off")
		return nil
	})

	resp, raw := h.post("/v1/messages", body(messagesRequestBody, true))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("降级重放也必须是 SSE，实际 Content-Type %s", ct)
	}

	up, ok := h.mock.LastRequest()
	if !ok {
		t.Fatal("上游没有收到请求")
	}
	var upBody map[string]any
	if err := json.Unmarshal(up.Body, &upBody); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v", err)
	}
	if upBody["stream"] != true {
		t.Fatalf("入口是流式，上游请求应带 stream=true，实际 %v", upBody["stream"])
	}

	events := parseSSE(t, raw)
	types := sseTypes(t, events)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("降级重放的事件序列不正确\n实际: %v\n期望: %v", types, want)
	}
	if got := anthropicStreamText(t, events); got != mockContent {
		t.Fatalf("重放内容应为 %q，实际 %q", mockContent, got)
	}
	writeEvidence(t, "stream-degraded-replay.sse", []byte(raw))
}

// ==================== 工具调用与推理内容 ====================

// TestConvertedStreamCarriesToolCalls Chat 上游分片下发工具参数时，
// 转换后的 Anthropic 流必须给出 tool_use 块，且分片能拼成完整 arguments。
func TestConvertedStreamCarriesToolCalls(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URLWith("/chat", "variant=tools")
		return nil
	})

	resp, raw := h.post("/v1/messages", body(messagesRequestBody, true))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	events := parseSSE(t, raw)

	if got := anthropicToolName(t, events); got != mockToolName {
		t.Fatalf("tool_use 名称应为 %q，实际 %q", mockToolName, got)
	}
	if got := anthropicToolID(t, events); got != mockToolChatID {
		t.Fatalf("tool_use id 应沿用上游 id %q，实际 %q", mockToolChatID, got)
	}
	if got := anthropicToolInputJSON(t, events); got != mockToolArgs {
		t.Fatalf("input_json_delta 拼接应为 %q，实际 %q", mockToolArgs, got)
	}
	if got := anthropicStreamStopReason(t, events); got != "tool_use" {
		t.Fatalf("stop_reason 应为 tool_use，实际 %q", got)
	}
	writeEvidence(t, "stream-chat-tools-to-messages.sse", []byte(raw))
}

// TestConvertedStreamCarriesReasoning Chat 上游的 reasoning_content 必须
// 落成 Anthropic 的 thinking 块，且排在正文块之前。
func TestConvertedStreamCarriesReasoning(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URLWith("/chat", "variant=reasoning")
		return nil
	})

	resp, raw := h.post("/v1/messages", body(messagesRequestBody, true))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	events := parseSSE(t, raw)

	if got := anthropicThinkingText(t, events); got != mockReasoning {
		t.Fatalf("thinking 增量拼接应为 %q，实际 %q", mockReasoning, got)
	}
	if got := anthropicStreamText(t, events); got != mockContent {
		t.Fatalf("正文增量拼接应为 %q，实际 %q", mockContent, got)
	}
	// 块顺序：thinking 在前、text 在后（与上游给的顺序一致）。
	if got := anthropicBlockTypes(t, events); strings.Join(got, ",") != "thinking,text" {
		t.Fatalf("内容块顺序应为 thinking,text，实际 %v", got)
	}

	// 注意：Chat 上游只有 reasoning_content 字符串，没有签名，因此转换出的
	// thinking 块不会带 signature —— 这是「经 Chat 有损」的具体表现，文档已记录。
	if sigs := anthropicThinkingSignatures(t, events); len(sigs) != 0 {
		t.Fatalf("Chat 上游没有签名，不应凭空造出 signature，实际 %v", sigs)
	}
	writeEvidence(t, "stream-chat-reasoning-to-messages.sse", []byte(raw))
}

// TestNonStreamConvertedToolCall Chat 非流式的 tool_calls 必须落成 Anthropic tool_use 块。
func TestNonStreamConvertedToolCall(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URLWith("/chat", "variant=tools")
		return nil
	})

	resp, raw := h.post("/v1/messages", body(messagesRequestBody, false))
	body := assertNonStreamJSON(t, "application/json", resp, raw)
	blocks := asList(t, body["content"], "content")
	if len(blocks) != 1 {
		t.Fatalf("应当只有一个 tool_use 块，实际 %d 个: %v", len(blocks), blocks)
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != mockToolName {
		t.Fatalf("内容块应为 tool_use/%s，实际 %v", mockToolName, block)
	}
	if block["id"] != mockToolChatID {
		t.Fatalf("tool_use id 应为 %q，实际 %v", mockToolChatID, block["id"])
	}
	input, _ := block["input"].(map[string]any)
	if input["loc"] != "SF" {
		t.Fatalf("tool_use input 应解析自 arguments，实际 %v", block["input"])
	}
	if got := body["stop_reason"]; got != "tool_use" {
		t.Fatalf("stop_reason 应为 tool_use，实际 %v", got)
	}
	writeEvidenceJSON(t, "nonstream-chat-tools-to-messages.json", body)
}

// ==================== 断言辅助 ====================

// auditErrorText 是各错误形态应当被识别出的信息（见 DetectSSEErrorInPrefix）。
var auditErrorText = map[string]string{
	"event":     "SSE error event: mock upstream boom",
	"chat":      "mock stream failed",
	"anthropic": "mock overloaded",
	"responses": "mock responses error",
	"failed":    "mock response failed",
}

// assertAuditErrorMentions 断言审计里留下了这次上游失败的记录，
// 且信息就是错误探测识别出的那条（证明「探测 → 记审计 → 换渠道」是一条链）。
func assertAuditErrorMentions(t *testing.T, h *harness, shape string) {
	t.Helper()
	want := auditErrorText[shape]
	if want == "" {
		t.Fatalf("测试用例缺少 shape=%s 的期望文案", shape)
	}
	rows, err := h.state.Audit.QueryList(audit.QueryListParams{Limit: 20})
	if err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	for _, row := range rows {
		if row.ErrorMessage == nil {
			continue
		}
		if strings.Contains(*row.ErrorMessage, want) {
			return
		}
	}
	t.Fatalf("审计里没有出现 %q 的错误记录，实际 %d 行", want, len(rows))
}

// anthropicBlockTypes 返回 content_block_start 事件里的块类型（按出现顺序）。
func anthropicBlockTypes(t *testing.T, events []sse.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		payload := eventPayload(t, ev)
		if payload["type"] != "content_block_start" {
			continue
		}
		block, _ := payload["content_block"].(map[string]any)
		if name, ok := block["type"].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

// anthropicToolName 返回 tool_use 块的名称。
func anthropicToolName(t *testing.T, events []sse.Event) string {
	t.Helper()
	block := anthropicToolBlock(t, events)
	name, _ := block["name"].(string)
	return name
}

// anthropicToolID 返回 tool_use 块的 id。
func anthropicToolID(t *testing.T, events []sse.Event) string {
	t.Helper()
	block := anthropicToolBlock(t, events)
	id, _ := block["id"].(string)
	return id
}

func anthropicToolBlock(t *testing.T, events []sse.Event) map[string]any {
	t.Helper()
	for _, ev := range events {
		payload := eventPayload(t, ev)
		if payload["type"] != "content_block_start" {
			continue
		}
		block, _ := payload["content_block"].(map[string]any)
		if block["type"] == "tool_use" {
			return block
		}
	}
	t.Fatalf("没有找到 tool_use 内容块")
	return nil
}

// anthropicToolInputJSON 拼接 input_json_delta 的 partial_json 片段。
func anthropicToolInputJSON(t *testing.T, events []sse.Event) string {
	t.Helper()
	var sb strings.Builder
	for _, ev := range events {
		payload := eventPayload(t, ev)
		if payload["type"] != "content_block_delta" {
			continue
		}
		delta, _ := payload["delta"].(map[string]any)
		if delta["type"] != "input_json_delta" {
			continue
		}
		if s, ok := delta["partial_json"].(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// anthropicThinkingText 拼接 thinking_delta 的文本。
func anthropicThinkingText(t *testing.T, events []sse.Event) string {
	t.Helper()
	var sb strings.Builder
	for _, ev := range events {
		payload := eventPayload(t, ev)
		if payload["type"] != "content_block_delta" {
			continue
		}
		delta, _ := payload["delta"].(map[string]any)
		if delta["type"] != "thinking_delta" {
			continue
		}
		if s, ok := delta["thinking"].(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// anthropicThinkingSignatures 收集 thinking 块上出现的 signature（若有）。
func anthropicThinkingSignatures(t *testing.T, events []sse.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		payload := eventPayload(t, ev)
		block, _ := payload["content_block"].(map[string]any)
		if block["type"] != "thinking" {
			continue
		}
		if sig, ok := block["signature"].(string); ok && sig != "" {
			out = append(out, sig)
		}
		if payload["type"] != "content_block_delta" {
			continue
		}
		delta, _ := payload["delta"].(map[string]any)
		if sig, ok := delta["signature"].(string); ok && sig != "" {
			out = append(out, sig)
		}
	}
	return out
}

// anthropicStreamStopReason 取 message_delta 上的 stop_reason。
func anthropicStreamStopReason(t *testing.T, events []sse.Event) string {
	t.Helper()
	for _, ev := range events {
		payload := eventPayload(t, ev)
		if payload["type"] != "message_delta" {
			continue
		}
		delta, _ := payload["delta"].(map[string]any)
		if s, ok := delta["stop_reason"].(string); ok {
			return s
		}
	}
	t.Fatalf("没有找到 message_delta.stop_reason")
	return ""
}

// eventPayload 解析一个 SSE 事件的 JSON 载荷。
func eventPayload(t *testing.T, ev sse.Event) map[string]any {
	t.Helper()
	if ev.Data == "" || ev.Data == sse.Done {
		return map[string]any{}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("SSE 事件载荷不是合法 JSON: %v\n%s", err, ev.Data)
	}
	return payload
}
