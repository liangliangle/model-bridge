package converter

import (
	"strings"
	"testing"
)

// 逆向映射（request_reverse.go / response_reverse.go）的测试。
//
// 这些是「Chat 当客户端 / 渠道是富协议」以及经 Chat 两跳所依赖的原语。
// 断言按字段逐项给出，并且同时断言**有损处理的 note**——有损可以接受，
// 但必须能定位到是哪一跳丢的什么（见 plan 的「有损清单」）。

// chatRequest 从 JSON 文本构造一个 Chat 请求对象。
func chatRequest(t *testing.T, raw string) map[string]any {
	t.Helper()
	return mustObject(t, raw)
}

// notesOf 返回会话收集到的有损处理说明。
func notesOf(s *convSession) []string { return s.notes }

// hasNoteContaining 判断 note 列表里是否有包含某片段的条目。
func hasNoteContaining(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}

// ==================== chat → messages（请求方向）====================

func TestChatRequestToAnthropicBasics(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToAnthropic(chatRequest(t, `{
		"model": "gpt-test",
		"max_tokens": 64,
		"temperature": 0.3,
		"top_p": 0.9,
		"stop": ["END"],
		"user": "u-1",
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"}
		]
	}`), RequestOptions{Model: "claude-x", Stream: true})
	if err != nil {
		t.Fatalf("chatRequestToAnthropic: %v", err)
	}

	if out["model"] != "claude-x" {
		t.Fatalf("model = %v, want claude-x（opts.Model 优先）", out["model"])
	}
	if out["max_tokens"] != int64(64) && out["max_tokens"] != 64 {
		t.Fatalf("max_tokens = %v", out["max_tokens"])
	}
	if out["stream"] != true {
		t.Fatalf("stream = %v, want true", out["stream"])
	}

	// system 用文本块数组承载。
	system, ok := asArray(out["system"])
	if !ok || len(system) != 1 {
		t.Fatalf("system 应为一块数组，实际 %#v", out["system"])
	}
	if block, _ := asMap(system[0]); block["text"] != "be brief" {
		t.Fatalf("system 文本 = %v", system[0])
	}

	// messages 只保留 user / assistant，且 content 一律是块数组。
	messages, ok := asArray(out["messages"])
	if !ok || len(messages) != 2 {
		t.Fatalf("messages 应有 2 条，实际 %#v", out["messages"])
	}
	first, _ := asMap(messages[0])
	if first["role"] != "user" {
		t.Fatalf("第一条 role = %v", first["role"])
	}
	blocks, _ := asArray(first["content"])
	block, _ := asMap(blocks[0])
	if block["type"] != "text" || block["text"] != "hi" {
		t.Fatalf("user 内容块 = %#v", blocks)
	}

	// 采样参数与 stop / user 的映射（数值是 json.Number，比较时统一成 float）。
	if temp, _ := asFloat(out["temperature"]); temp != 0.3 {
		t.Fatalf("temperature 应透传，实际 %v", out["temperature"])
	}
	if topP, _ := asFloat(out["top_p"]); topP != 0.9 {
		t.Fatalf("top_p 应透传，实际 %v", out["top_p"])
	}
	seqs, _ := asArray(out["stop_sequences"])
	if len(seqs) != 1 || seqs[0] != "END" {
		t.Fatalf("stop → stop_sequences 失败：%#v", out["stop_sequences"])
	}
	metadata, _ := asMap(out["metadata"])
	if metadata["user_id"] != "u-1" {
		t.Fatalf("user → metadata.user_id 失败：%#v", out["metadata"])
	}
}

func TestChatRequestToAnthropicToolsAndResults(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToAnthropic(chatRequest(t, `{
		"model": "m",
		"max_tokens": 32,
		"tools": [{"type":"function","function":{"name":"get_weather","description":"天气","parameters":{"type":"object","properties":{"loc":{"type":"string"}}}}}],
		"tool_choice": "required",
		"messages": [
			{"role": "user", "content": "天气"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"loc\":\"SF\"}"}},
				{"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{\"loc\":\"NY\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "晴"},
			{"role": "tool", "tool_call_id": "call_2", "content": "阴"}
		]
	}`), RequestOptions{})
	if err != nil {
		t.Fatalf("chatRequestToAnthropic: %v", err)
	}

	// tools：function 嵌套展平成 input_schema。
	tools, _ := asArray(out["tools"])
	tool, _ := asMap(tools[0])
	if tool["name"] != "get_weather" || tool["description"] != "天气" {
		t.Fatalf("tool = %#v", tool)
	}
	if _, ok := asMap(tool["input_schema"]); !ok {
		t.Fatalf("input_schema 缺失：%#v", tool)
	}
	// tool_choice：required → any。
	choice, _ := asMap(out["tool_choice"])
	if choice["type"] != "any" {
		t.Fatalf("tool_choice = %#v, want any", out["tool_choice"])
	}

	messages, _ := asArray(out["messages"])
	if len(messages) != 3 {
		t.Fatalf("应有 user / assistant / user(tool_result) 三条，实际 %d 条：%#v", len(messages), messages)
	}

	// assistant 的两个 tool_calls → 两个 tool_use 块。
	assistant, _ := asMap(messages[1])
	blocks, _ := asArray(assistant["content"])
	if len(blocks) != 2 {
		t.Fatalf("assistant 应有 2 个 tool_use 块，实际 %#v", blocks)
	}
	use, _ := asMap(blocks[0])
	if use["type"] != "tool_use" || use["id"] != "call_1" || use["name"] != "get_weather" {
		t.Fatalf("tool_use = %#v", use)
	}
	if input, _ := asMap(use["input"]); input["loc"] != "SF" {
		t.Fatalf("arguments 未解析成 input：%#v", use["input"])
	}

	// 连续两条 tool 消息必须合并成**一条** user 消息里的两个 tool_result 块
	// （Anthropic 要求 tool_result 紧跟在对应的 tool_use 之后）。
	results, _ := asMap(messages[2])
	if results["role"] != "user" {
		t.Fatalf("tool_result 消息的 role = %v", results["role"])
	}
	resultBlocks, _ := asArray(results["content"])
	if len(resultBlocks) != 2 {
		t.Fatalf("两个 tool_result 应合并进同一条 user 消息，实际 %#v", resultBlocks)
	}
	r0, _ := asMap(resultBlocks[0])
	if r0["type"] != "tool_result" || r0["tool_use_id"] != "call_1" || r0["content"] != "晴" {
		t.Fatalf("tool_result = %#v", r0)
	}
}

func TestChatRequestToAnthropicToolChoiceNoneDropsTools(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToAnthropic(chatRequest(t, `{
		"model": "m", "max_tokens": 8, "tool_choice": "none",
		"tools": [{"type":"function","function":{"name":"f","parameters":{}}}],
		"messages": [{"role":"user","content":"hi"}]
	}`), RequestOptions{})
	if err != nil {
		t.Fatalf("chatRequestToAnthropic: %v", err)
	}
	// Anthropic 没有 none 取值：只能通过不下发 tools 表达。
	if _, present := out["tools"]; present {
		t.Fatalf("tool_choice=none 时不应下发 tools：%#v", out["tools"])
	}
	if _, present := out["tool_choice"]; present {
		t.Fatalf("tool_choice=none 时不应下发 tool_choice")
	}
	if !hasNoteContaining(notesOf(s), "tool_choice=none") {
		t.Fatalf("应记录 note：%v", notesOf(s))
	}
}

func TestChatRequestToAnthropicThinking(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToAnthropic(chatRequest(t, `{
		"model": "m", "max_tokens": 1024, "reasoning_effort": "high",
		"temperature": 0.7,
		"messages": [{"role":"user","content":"hi"}]
	}`), RequestOptions{})
	if err != nil {
		t.Fatalf("chatRequestToAnthropic: %v", err)
	}
	thinking, ok := asMap(out["thinking"])
	if !ok || thinking["type"] != "enabled" {
		t.Fatalf("thinking = %#v", out["thinking"])
	}
	if thinking["budget_tokens"] != effortToBudget("high") {
		t.Fatalf("budget = %v, want %v", thinking["budget_tokens"], effortToBudget("high"))
	}
	// 开启 thinking 时 Anthropic 不接受 temperature。
	if _, present := out["temperature"]; present {
		t.Fatalf("开启 thinking 时不应带 temperature")
	}
	if !hasNoteContaining(notesOf(s), "temperature") {
		t.Fatalf("应记录丢弃 temperature 的 note：%v", notesOf(s))
	}
}

func TestChatRequestToAnthropicMaxTokensDefault(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToAnthropic(chatRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`), RequestOptions{})
	if err != nil {
		t.Fatalf("chatRequestToAnthropic: %v", err)
	}
	if got, _ := asInt(out["max_tokens"]); got != defaultAnthropicMaxTokens {
		t.Fatalf("max_tokens = %v, want %d（Anthropic 必填的兜底值）", out["max_tokens"], defaultAnthropicMaxTokens)
	}
	if !hasNoteContaining(notesOf(s), "max_tokens") {
		t.Fatalf("补默认值时应记 note：%v", notesOf(s))
	}
}

func TestChatRequestToAnthropicRejectsUnrepresentableFields(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "n>1",
			body:  `{"model":"m","max_tokens":8,"n":3,"messages":[{"role":"user","content":"hi"}]}`,
			field: "n",
		},
		{
			name:  "logprobs",
			body:  `{"model":"m","max_tokens":8,"logprobs":true,"messages":[{"role":"user","content":"hi"}]}`,
			field: "logprobs",
		},
		{
			name:  "top_logprobs",
			body:  `{"model":"m","max_tokens":8,"top_logprobs":5,"messages":[{"role":"user","content":"hi"}]}`,
			field: "top_logprobs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newConvSession()
			_, err := s.chatRequestToAnthropic(chatRequest(t, tc.body), RequestOptions{})
			fieldErr, ok := err.(*UnsupportedFieldError)
			if !ok {
				t.Fatalf("err = %v (%T), want *UnsupportedFieldError", err, err)
			}
			if fieldErr.Field != tc.field || fieldErr.Target != FormatAnthropic {
				t.Fatalf("错误信息应点名字段与目标协议，实际 %+v", fieldErr)
			}
			// 错误文案要能直接给调用方看（代理层会包成 400 invalid_request）。
			if !strings.Contains(fieldErr.Error(), tc.field) || !strings.Contains(fieldErr.Error(), "messages") {
				t.Fatalf("错误文案不可定位：%q", fieldErr.Error())
			}
		})
	}
}

func TestChatRequestToAnthropicDropsAndNotes(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToAnthropic(chatRequest(t, `{
		"model": "m", "max_tokens": 64,
		"seed": 7, "frequency_penalty": 0.5, "presence_penalty": 0.5,
		"response_format": {"type":"json_object"},
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"a","reasoning_content":"我的推理"}
		]
	}`), RequestOptions{})
	if err != nil {
		t.Fatalf("chatRequestToAnthropic: %v", err)
	}
	for _, field := range []string{"seed", "frequency_penalty", "presence_penalty", "response_format"} {
		if _, present := out[field]; present {
			t.Fatalf("字段 %q 不应出现在 Anthropic 请求里", field)
		}
		if !hasNoteContaining(notesOf(s), field) {
			t.Fatalf("字段 %q 被丢弃时应记 note：%v", field, notesOf(s))
		}
	}
	// 推理内容：Anthropic 的 thinking 块需要有效签名，不能伪造 → 丢弃 + note。
	if !hasNoteContaining(notesOf(s), "reasoning_content") {
		t.Fatalf("丢弃推理内容时应记 note：%v", notesOf(s))
	}
	assistant, _ := asMap(secondMessage(t, out))
	if _, present := assistant["reasoning_content"]; present {
		t.Fatalf("不应把 reasoning_content 带进 Anthropic 请求")
	}
	blocks, _ := asArray(assistant["content"])
	if len(blocks) != 1 {
		t.Fatalf("assistant 只应有正文块，实际 %#v", blocks)
	}
}

func TestChatRequestToAnthropicImages(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToAnthropic(chatRequest(t, `{
		"model": "m", "max_tokens": 64,
		"messages": [{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
		]}]
	}`), RequestOptions{})
	if err != nil {
		t.Fatalf("chatRequestToAnthropic: %v", err)
	}
	first, _ := asMap(firstMessage(t, out))
	blocks, _ := asArray(first["content"])
	if len(blocks) != 2 {
		t.Fatalf("应有 text + image 两块，实际 %#v", blocks)
	}
	image, _ := asMap(blocks[1])
	if image["type"] != "image" {
		t.Fatalf("image 块 = %#v", image)
	}
	source, _ := asMap(image["source"])
	if source["type"] != "base64" || source["media_type"] != "image/png" || source["data"] != "AAAA" {
		t.Fatalf("image.source = %#v", source)
	}
}

// ==================== chat → responses（请求方向）====================

func TestChatRequestToResponsesBasics(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToResponses(chatRequest(t, `{
		"model": "gpt-test",
		"max_tokens": 128,
		"temperature": 0.2,
		"response_format": {"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object"},"strict":true}},
		"reasoning_effort": "medium",
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "developer", "content": "second system"},
			{"role": "user", "content": "hi"}
		]
	}`), RequestOptions{Model: "gpt-5", Stream: true})
	if err != nil {
		t.Fatalf("chatRequestToResponses: %v", err)
	}

	if out["model"] != "gpt-5" {
		t.Fatalf("model = %v", out["model"])
	}
	// system + developer 都落到 instructions（多段用空行连接）。
	if out["instructions"] != "be brief\n\nsecond system" {
		t.Fatalf("instructions = %q", out["instructions"])
	}
	if got, _ := asInt(out["max_output_tokens"]); got != 128 {
		t.Fatalf("max_tokens → max_output_tokens 失败：%v", out["max_output_tokens"])
	}
	if out["stream"] != true {
		t.Fatalf("stream = %v", out["stream"])
	}
	if temp, _ := asFloat(out["temperature"]); temp != 0.2 {
		t.Fatalf("temperature 应透传，实际 %v", out["temperature"])
	}

	reasoning, _ := asMap(out["reasoning"])
	if reasoning["effort"] != "medium" {
		t.Fatalf("reasoning.effort = %#v", out["reasoning"])
	}
	text, _ := asMap(out["text"])
	format, _ := asMap(text["format"])
	if format["type"] != "json_schema" || format["name"] != "out" || format["strict"] != true {
		t.Fatalf("response_format → text.format 失败：%#v", format)
	}

	// input：一条 user message 项，正文用 input_text part。
	items, _ := asArray(out["input"])
	if len(items) != 1 {
		t.Fatalf("input 应有 1 项，实际 %#v", items)
	}
	item, _ := asMap(items[0])
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("input item = %#v", item)
	}
	parts, _ := asArray(item["content"])
	part, _ := asMap(parts[0])
	if part["type"] != "input_text" || part["text"] != "hi" {
		t.Fatalf("input part = %#v", parts)
	}
}

func TestChatRequestToResponsesToolRoundTrip(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToResponses(chatRequest(t, `{
		"model": "m",
		"tools": [{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"tool_choice": {"type":"function","function":{"name":"f"}},
		"messages": [
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "done"}
		]
	}`), RequestOptions{})
	if err != nil {
		t.Fatalf("chatRequestToResponses: %v", err)
	}

	tools, _ := asArray(out["tools"])
	tool, _ := asMap(tools[0])
	// Responses 的工具是扁平的：name/parameters 直接在顶层。
	if tool["type"] != "function" || tool["name"] != "f" {
		t.Fatalf("tool = %#v", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Fatalf("Responses 工具不应有 function 嵌套：%#v", tool)
	}
	choice, _ := asMap(out["tool_choice"])
	if choice["type"] != "function" || choice["name"] != "f" {
		t.Fatalf("tool_choice = %#v", out["tool_choice"])
	}

	items, _ := asArray(out["input"])
	if len(items) != 3 {
		t.Fatalf("应有 message / function_call / function_call_output 三项，实际 %d：%#v", len(items), items)
	}
	call, _ := asMap(items[1])
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "f" {
		t.Fatalf("function_call 项 = %#v", call)
	}
	if call["arguments"] != `{"a":1}` {
		t.Fatalf("arguments = %v", call["arguments"])
	}
	result, _ := asMap(items[2])
	if result["type"] != "function_call_output" || result["call_id"] != "call_1" || result["output"] != "done" {
		t.Fatalf("function_call_output 项 = %#v", result)
	}
}

func TestChatRequestToResponsesDropsAndNotes(t *testing.T) {
	s := newConvSession()
	out, err := s.chatRequestToResponses(chatRequest(t, `{
		"model": "m", "stop": ["x"], "seed": 1, "logit_bias": {"1": 2},
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"a","reasoning_content":"推理"}
		]
	}`), RequestOptions{})
	if err != nil {
		t.Fatalf("chatRequestToResponses: %v", err)
	}
	for _, field := range []string{"stop", "seed", "logit_bias"} {
		if _, present := out[field]; present {
			t.Fatalf("字段 %q 不应出现在 Responses 请求里", field)
		}
		if !hasNoteContaining(notesOf(s), field) {
			t.Fatalf("字段 %q 被丢弃时应记 note：%v", field, notesOf(s))
		}
	}
	if !hasNoteContaining(notesOf(s), "reasoning_content") {
		t.Fatalf("丢弃推理内容时应记 note：%v", notesOf(s))
	}
	// reasoning_content 不能变成 input 里的项。
	for _, raw := range mustArray(t, out["input"]) {
		item, _ := asMap(raw)
		if item["type"] == "reasoning" {
			t.Fatalf("不应伪造 reasoning 项：%#v", item)
		}
	}
}

// ==================== 响应方向：messages → chat ====================

func TestAnthropicResponseToChat(t *testing.T) {
	s := newConvSession()
	out := s.anthropicResponseToChat(mustObject(t, `{
		"id": "msg_1",
		"type": "message",
		"role": "assistant",
		"model": "claude-x",
		"content": [
			{"type":"thinking","thinking":"推理","signature":"sig"},
			{"type":"text","text":"你好"},
			{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "cache_read_input_tokens": 4, "output_tokens": 5}
	}`), "claude-x")

	if out["object"] != "chat.completion" {
		t.Fatalf("object = %v", out["object"])
	}
	if out["id"] != "msg_1" {
		t.Fatalf("id 应沿用上游 id，实际 %v", out["id"])
	}
	choice := firstChatChoice(out)
	if choice == nil {
		t.Fatalf("缺少 choices[0]")
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("stop_reason=tool_use → finish_reason 应为 tool_calls，实际 %v", choice["finish_reason"])
	}
	message, _ := asMap(choice["message"])
	if message["role"] != "assistant" || message["content"] != "你好" {
		t.Fatalf("message = %#v", message)
	}
	if message["reasoning_content"] != "推理" {
		t.Fatalf("thinking → reasoning_content 失败：%#v", message["reasoning_content"])
	}
	calls, _ := asArray(message["tool_calls"])
	call, _ := asMap(calls[0])
	if call["id"] != "toolu_1" || call["type"] != "function" {
		t.Fatalf("tool_call = %#v", call)
	}
	fn, _ := asMap(call["function"])
	if fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("function = %#v", fn)
	}
	// usage：缓存命中并回 prompt_tokens（10 + 4）。
	usage, _ := asMap(out["usage"])
	if got, _ := asInt(usage["prompt_tokens"]); got != 14 {
		t.Fatalf("prompt_tokens = %v, want 14（input + cache_read）", usage["prompt_tokens"])
	}
	if got, _ := asInt(usage["completion_tokens"]); got != 5 {
		t.Fatalf("completion_tokens = %v", usage["completion_tokens"])
	}
}

func TestAnthropicResponseToChatRedactedThinkingIsNoted(t *testing.T) {
	s := newConvSession()
	out := s.anthropicResponseToChat(mustObject(t, `{
		"id":"msg_1","type":"message","role":"assistant","model":"m",
		"content":[{"type":"redacted_thinking","data":"zzz"},{"type":"text","text":"hi"}],
		"stop_reason":"end_turn"
	}`), "m")
	message, _ := asMap(firstChatChoice(out)["message"])
	if message["content"] != "hi" {
		t.Fatalf("正文应保留：%#v", message["content"])
	}
	if !hasNoteContaining(notesOf(s), "redacted_thinking") {
		t.Fatalf("丢弃加密推理块时应记 note：%v", notesOf(s))
	}
}

// ==================== 响应方向：responses → chat ====================

func TestResponsesResponseToChat(t *testing.T) {
	s := newConvSession()
	out := s.responsesResponseToChat(mustObject(t, `{
		"id": "resp_1",
		"object": "response",
		"status": "completed",
		"model": "gpt-5",
		"created_at": 1700000000,
		"output": [
			{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"先想"}]},
			{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[
				{"type":"output_text","text":"答案","annotations":[]}
			]},
			{"id":"fc_1","type":"function_call","call_id":"call_9","name":"f","arguments":"{\"a\":1}","status":"completed"}
		],
		"usage": {"input_tokens": 20, "output_tokens": 7, "total_tokens": 27, "input_tokens_details":{"cached_tokens":5}}
	}`), "gpt-5")

	if out["id"] != "resp_1" || out["created"] != int64(1700000000) {
		t.Fatalf("id/created 应沿用上游：%v / %v", out["id"], out["created"])
	}
	choice := firstChatChoice(out)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("有工具调用时 finish_reason 应为 tool_calls，实际 %v", choice["finish_reason"])
	}
	message, _ := asMap(choice["message"])
	if message["content"] != "答案" {
		t.Fatalf("content = %#v", message["content"])
	}
	if message["reasoning_content"] != "先想" {
		t.Fatalf("reasoning 项 → reasoning_content 失败：%#v", message["reasoning_content"])
	}
	calls, _ := asArray(message["tool_calls"])
	call, _ := asMap(calls[0])
	fn, _ := asMap(call["function"])
	if call["id"] != "call_9" || fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("tool_call = %#v", call)
	}
	usage, _ := asMap(out["usage"])
	if got, _ := asInt(usage["prompt_tokens"]); got != 20 {
		t.Fatalf("prompt_tokens = %v", usage["prompt_tokens"])
	}
	details, _ := asMap(usage["prompt_tokens_details"])
	if got, _ := asInt(details["cached_tokens"]); got != 5 {
		t.Fatalf("cached_tokens 应保留：%#v", usage["prompt_tokens_details"])
	}
}

// TestResponsesResponseToChatReasoningPartAnnotation 覆盖我们自己写出的推理标记约定：
// toResponsesWithNS 把推理内容写成带 annotations:[{type:reasoning}] 的 output_text part，
// 反向必须认出来（否则往返会把推理混进正文）。
func TestResponsesResponseToChatReasoningPartAnnotation(t *testing.T) {
	s := newConvSession()
	out := s.responsesResponseToChat(mustObject(t, `{
		"id":"resp_1","object":"response","status":"completed","model":"m",
		"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[
			{"type":"output_text","text":"想","annotations":[{"type":"reasoning"}]},
			{"type":"output_text","text":"说","annotations":[]}
		]}]
	}`), "m")
	message, _ := asMap(firstChatChoice(out)["message"])
	if message["reasoning_content"] != "想" {
		t.Fatalf("带 reasoning 标记的 part 应进 reasoning_content：%#v", message)
	}
	if message["content"] != "说" {
		t.Fatalf("正文应只含非推理 part：%#v", message["content"])
	}
}

func TestResponsesResponseToChatIncomplete(t *testing.T) {
	s := newConvSession()
	out := s.responsesResponseToChat(mustObject(t, `{
		"id":"resp_1","object":"response","status":"incomplete","model":"m",
		"output":[{"id":"msg_1","type":"message","role":"assistant","status":"incomplete",
			"content":[{"type":"output_text","text":"半句","annotations":[]}]}]
	}`), "m")
	if got := firstChatChoice(out)["finish_reason"]; got != "length" {
		t.Fatalf("status=incomplete → finish_reason 应为 length，实际 %v", got)
	}
}

// ==================== 两跳：请求方向 ====================

// TestTwoHopRequestMessagesToResponses 固定 messages → chat → responses 的请求折叠：
// 最终出站体必须是 Responses 形态，且 Chat 中枢的中间产物（stream_options 等）不得残留。
func TestTwoHopRequestMessagesToResponses(t *testing.T) {
	s := newConvSession()
	plan := NewPlan(FormatAnthropic, FormatResponses)
	raw, err := s.buildUpstreamRequest(plan, []byte(`{
		"model": "gpt-test",
		"max_tokens": 64,
		"system": "be brief",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}]},
			{"role": "user", "content": [{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		]
	}`), RequestOptions{Model: "gpt-5", Stream: true})
	if err != nil {
		t.Fatalf("两跳请求转换失败：%v", err)
	}
	out := mustObject(t, string(raw))

	if out["instructions"] != "be brief" {
		t.Fatalf("system 应落到 instructions，实际 %#v", out["instructions"])
	}
	if out["stream"] != true {
		t.Fatalf("stream = %v", out["stream"])
	}
	// 中间态是 Chat：不得把 Chat 专属字段带到 Responses 出站体上。
	for _, field := range []string{"messages", "max_tokens", "stream_options"} {
		if _, present := out[field]; present {
			t.Fatalf("两跳后不应残留 Chat 字段 %q：%#v", field, out[field])
		}
	}
	items, _ := asArray(out["input"])
	types := []string{}
	for _, rawItem := range items {
		item, _ := asMap(rawItem)
		types = append(types, itemType(item))
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "message") || !strings.Contains(joined, "function_call") || !strings.Contains(joined, "function_call_output") {
		t.Fatalf("两跳后 input 项应含 message / function_call / function_call_output，实际 %v", types)
	}
}

// TestTwoHopResponseResponsesToMessages 固定 responses → chat → messages 的响应折叠。
func TestTwoHopResponseResponsesToMessages(t *testing.T) {
	s := newConvSession()
	plan := NewPlan(FormatAnthropic, FormatResponses)
	raw, err := s.convertNonStreamResponse(plan, []byte(`{
		"id":"resp_1","object":"response","status":"completed","model":"gpt-5",
		"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed",
			"content":[{"type":"output_text","text":"你好","annotations":[]}]}],
		"usage":{"input_tokens":9,"output_tokens":3,"total_tokens":12}
	}`), "gpt-5")
	if err != nil {
		t.Fatalf("两跳响应转换失败：%v", err)
	}
	out := mustObject(t, string(raw))
	if out["type"] != "message" {
		t.Fatalf("应为 messages 形态，实际 %#v", out["type"])
	}
	if got := extractText(out["content"]); got != "你好" {
		t.Fatalf("正文 = %q", got)
	}
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", out["stop_reason"])
	}
	usage, _ := asMap(out["usage"])
	// 两跳的账目：input_tokens 在 Responses 侧含缓存命中，Anthropic 侧不含；
	// 这里没有缓存，因此数值应原样保留（缓存场景见 usage_test.go 的往返测试）。
	if got, _ := asInt(usage["input_tokens"]); got != 9 {
		t.Fatalf("input_tokens = %v, want 9", usage["input_tokens"])
	}
}

// TestReplayResponseAsSSEUsesClientFormat 上游对流式请求回非 SSE JSON 时，
// 重放必须按**客户端**协议产出事件流（两跳时这一点尤其容易写反）。
func TestReplayResponseAsSSEUsesClientFormat(t *testing.T) {
	cases := []struct {
		name    string
		client  ApiFormat
		channel ApiFormat
		body    string
		want    string // 期望出现在事件流里的特征
	}{
		{
			name:   "chat 客户端 ← messages 渠道",
			client: FormatOpenAIChat, channel: FormatAnthropic,
			body: `{"id":"msg_1","type":"message","role":"assistant","model":"m",
				"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
				"usage":{"input_tokens":1,"output_tokens":2}}`,
			want: "chat.completion.chunk",
		},
		{
			name:   "messages 客户端 ← responses 渠道（两跳）",
			client: FormatAnthropic, channel: FormatResponses,
			body: `{"id":"resp_1","object":"response","status":"completed","model":"m",
				"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed",
					"content":[{"type":"output_text","text":"hi","annotations":[]}]}]}`,
			want: "message_start",
		},
		{
			name:   "responses 客户端 ← messages 渠道（两跳）",
			client: FormatResponses, channel: FormatAnthropic,
			body: `{"id":"msg_1","type":"message","role":"assistant","model":"m",
				"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`,
			want: "response.created",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newConvSession()
			plan := NewPlan(tc.client, tc.channel)
			raw, err := s.fullResponseToSSE(plan, []byte(tc.body), "m")
			if err != nil {
				t.Fatalf("fullResponseToSSE: %v", err)
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Fatalf("重放输出里应含 %q（按客户端协议），实际:\n%s", tc.want, raw)
			}
		})
	}
}

// ==================== 小工具 ====================

// itemType 取 Responses input 项的 type（缺省视为 message）。
func itemType(item map[string]any) string {
	if t, ok := asString(item["type"]); ok && t != "" {
		return t
	}
	return "message"
}

func firstMessage(t *testing.T, anthropicReq map[string]any) any {
	t.Helper()
	messages := mustArray(t, anthropicReq["messages"])
	if len(messages) == 0 {
		t.Fatalf("messages 为空")
	}
	return messages[0]
}

func secondMessage(t *testing.T, anthropicReq map[string]any) any {
	t.Helper()
	messages := mustArray(t, anthropicReq["messages"])
	if len(messages) < 2 {
		t.Fatalf("messages 少于 2 条：%#v", messages)
	}
	return messages[1]
}

func mustArray(t *testing.T, v any) []any {
	t.Helper()
	items, ok := asArray(v)
	if !ok {
		t.Fatalf("不是数组：%#v", v)
	}
	return items
}
