package converter

import (
	"bytes"
	"strings"
	"testing"
)

// 本文件覆盖 Hop 这一层门面：方向由「下游协议 + 上游协议」一次确定，
// 请求与响应共用同一个对象。重点回归的是「方向写反」这个曾经真实发生的缺陷。

func TestNewHopValidatesMatrix(t *testing.T) {
	// 同协议：合法的透传方向。
	hop, err := NewHop(FormatOpenAIChat.AsClient(), FormatOpenAIChat.AsChannel())
	if err != nil {
		t.Fatalf("chat → chat: %v", err)
	}
	if hop.Converts() {
		t.Fatalf("同协议不应要求转换")
	}
	if hop.Client() != FormatOpenAIChat || hop.Channel() != FormatOpenAIChat {
		t.Fatalf("角色取回错误: client=%v channel=%v", hop.Client(), hop.Channel())
	}

	// 渠道是转换枢纽：合法。
	for _, c := range []ApiFormat{FormatAnthropic, FormatResponses} {
		hop, err := NewHop(c.AsClient(), FormatOpenAIChat.AsChannel())
		if err != nil {
			t.Fatalf("%v → chat: %v", c, err)
		}
		if !hop.Converts() {
			t.Fatalf("%v → chat 应当要求转换", c)
		}
	}

	// 矩阵外：两个方向都必须拒绝。把上下游写反正是走到这里的路径之一。
	if _, err := NewHop(FormatAnthropic.AsClient(), FormatResponses.AsChannel()); err == nil {
		t.Fatalf("messages → responses 必须返回 *UnsupportedConversion")
	} else if _, ok := err.(*UnsupportedConversion); !ok {
		t.Fatalf("错误类型 = %T, want *UnsupportedConversion", err)
	}
	if _, err := NewHop(FormatOpenAIChat.AsClient(), FormatAnthropic.AsChannel()); err == nil {
		t.Fatalf("chat → messages 必须返回 *UnsupportedConversion")
	}
}

// TestHopResponseDirection 是方向缺陷的回归测试：
// 入口是 messages、渠道是 chat，Hop 的响应转换必须产出 Anthropic 形态，
// 而不是把方向写成「上游 → 下游」后原样吐出 chat.completion。
func TestHopResponseDirection(t *testing.T) {
	hop, err := NewHop(FormatAnthropic.AsClient(), FormatOpenAIChat.AsChannel())
	if err != nil {
		t.Fatalf("NewHop: %v", err)
	}

	// 请求方向：messages → chat。
	upstream, err := hop.BuildUpstreamRequest([]byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`), RequestOptions{Model: "up"})
	if err != nil {
		t.Fatalf("BuildUpstreamRequest: %v", err)
	}
	if obj := mustObject(t, string(upstream)); obj["messages"] == nil {
		t.Fatalf("出站请求不是 Chat 形态: %s", upstream)
	}

	chat := []byte(`{"id":"c1","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)

	// 响应方向：chat → messages。
	raw, err := hop.NonStreamResponse(chat, "claude-x")
	if err != nil {
		t.Fatalf("NonStreamResponse: %v", err)
	}
	messages := mustObject(t, string(raw))
	if got, _ := asString(messages["type"]); got != "message" {
		t.Fatalf("响应形态 = %q, want message（方向写反会得到 chat.completion）", got)
	}
	if got, _ := asString(messages["model"]); got != "claude-x" {
		t.Fatalf("model = %q, want claude-x", got)
	}
	if types := blockTypes(t, messages["content"]); len(types) != 1 || types[0] != "text" {
		t.Fatalf("content 块 = %v", types)
	}

	// 同一条流：chat → messages 事件流。
	var buf bytes.Buffer
	payload := "data: " + `{"id":"c1","model":"up","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: " + `{"id":"c1","model":"up","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}` + "\n\n" +
		"data: [DONE]\n\n"
	usage, err := hop.StreamResponse(strings.NewReader(payload), &buf, "claude-x", nil)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	names := eventNames(parseSSE(t, buf.Bytes()))
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if !equalStrings(names, want) {
		t.Fatalf("事件序列 = %v, want %v", names, want)
	}
	if usage.InputTokens != 3 || usage.OutputTokens != 1 {
		t.Fatalf("usage = %+v", usage)
	}
}

// TestHopReplayAndPassthrough 覆盖「上游对流式请求返回完整 JSON」的重放路径，
// 以及同协议透传时按入口格式重放。
func TestHopReplayAndPassthrough(t *testing.T) {
	chat := []byte(`{"id":"c1","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)

	// 跨协议：重放为入口协议（responses）的事件流。
	hop, err := NewHop(FormatResponses.AsClient(), FormatOpenAIChat.AsChannel())
	if err != nil {
		t.Fatalf("NewHop: %v", err)
	}
	raw, err := hop.ReplayResponseAsSSE(chat, "gpt-5")
	if err != nil {
		t.Fatalf("ReplayResponseAsSSE: %v", err)
	}
	names := eventNames(parseSSE(t, raw))
	if len(names) == 0 || names[0] != "response.created" || names[len(names)-1] != "response.completed" {
		t.Fatalf("responses 重放事件 = %v", names)
	}

	// 同协议：不转换，按入口格式重放（chat）。
	same, err := NewHop(FormatOpenAIChat.AsClient(), FormatOpenAIChat.AsChannel())
	if err != nil {
		t.Fatalf("NewHop: %v", err)
	}
	raw, err = same.ReplayResponseAsSSE(chat, "gpt-4o")
	if err != nil {
		t.Fatalf("ReplayResponseAsSSE(chat→chat): %v", err)
	}
	if !bytes.Contains(raw, []byte("chat.completion.chunk")) || !bytes.HasSuffix(raw, []byte("data: [DONE]\n\n")) {
		t.Fatalf("chat 重放输出异常:\n%s", raw)
	}
}
