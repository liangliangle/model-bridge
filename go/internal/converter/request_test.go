package converter

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 本文件只测试 request.go 里的请求方向转换（Messages → Chat、Responses → Chat）与共用的
// 出站收尾（流式 usage、图片闸门、tool 消息清洗、跨格式校验、prompt caching 断点注入）。
// 响应 / 流式方向由 response_test.go / stream_test.go 覆盖。

// mustDecode 是本测试文件的 JSON 入口（与 decodeObject 同口径：UseNumber）。
func mustDecode(t *testing.T, raw string) map[string]any {
	t.Helper()
	obj, err := decodeObject([]byte(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return obj
}

// jsonText 把值序列化为紧凑 JSON（map 键按字典序），用于形状比对。
func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := marshalJSON(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// convertMessages 是 Messages → Chat 的测试入口。
func convertMessages(t *testing.T, body string, opts RequestOptions) map[string]any {
	t.Helper()
	out, err := convertRequest(mustDecode(t, body), opts)
	if err != nil {
		t.Fatalf("convertRequest: %v", err)
	}
	return out
}

// convertResponses 是 Responses → Chat 的测试入口。
func convertResponses(t *testing.T, s *convSession, body string, opts RequestOptions) map[string]any {
	t.Helper()
	out, err := s.responsesToChat(mustDecode(t, body), opts)
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}
	return out
}

// messageList 取出出站体的 messages 数组。
func messageList(t *testing.T, out map[string]any) []any {
	t.Helper()
	messages, ok := asArray(out["messages"])
	if !ok {
		t.Fatalf("messages is not an array: %s", jsonText(t, out["messages"]))
	}
	return messages
}

// at 取出 messages[i] 对象。
func at(t *testing.T, messages []any, i int) map[string]any {
	t.Helper()
	if i >= len(messages) {
		t.Fatalf("messages[%d] missing (len=%d)", i, len(messages))
	}
	msg, ok := asMap(messages[i])
	if !ok {
		t.Fatalf("messages[%d] is not an object: %s", i, jsonText(t, messages[i]))
	}
	return msg
}

func containsStringValue(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// ==================== Messages → Chat ====================

func TestConvertRequestSystemFolding(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantFirstMsg string
	}{
		{
			name:         "字符串 system 折叠成 system 消息",
			body:         `{"model":"m","max_tokens":8,"system":"你是助手","messages":[{"role":"user","content":"hi"}]}`,
			wantFirstMsg: `{"content":"你是助手","role":"system"}`,
		},
		{
			name:         "无 cache_control 的 system 块合并成纯字符串",
			body:         `{"model":"m","max_tokens":8,"system":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"messages":[{"role":"user","content":"hi"}]}`,
			wantFirstMsg: `{"content":"AB","role":"system"}`,
		},
		{
			name:         "带 cache_control 的 system 块保留数组形态（规则 1）",
			body:         `{"model":"m","max_tokens":8,"system":[{"type":"text","text":"A","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`,
			wantFirstMsg: `{"content":[{"cache_control":{"type":"ephemeral"},"text":"A","type":"text"}],"role":"system"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := convertMessages(t, tt.body, RequestOptions{})
			messages := messageList(t, out)
			if got := jsonText(t, at(t, messages, 0)); got != tt.wantFirstMsg {
				t.Fatalf("第一条消息 = %s, want %s", got, tt.wantFirstMsg)
			}
		})
	}

	t.Run("空 system 不产生 system 消息", func(t *testing.T) {
		out := convertMessages(t, `{"model":"m","max_tokens":8,"system":"","messages":[{"role":"user","content":"hi"}]}`, RequestOptions{})
		messages := messageList(t, out)
		if len(messages) != 1 {
			t.Fatalf("messages len = %d, want 1: %s", len(messages), jsonText(t, messages))
		}
		if got := jsonText(t, at(t, messages, 0)); got != `{"content":"hi","role":"user"}` {
			t.Fatalf("消息 = %s", got)
		}
	})

	t.Run("只有非 text 块的 system 不产生消息", func(t *testing.T) {
		out := convertMessages(t, `{"model":"m","max_tokens":8,"system":[{"type":"image","source":{"type":"url","url":"http://x"}}],"messages":[{"role":"user","content":"hi"}]}`, RequestOptions{})
		if messages := messageList(t, out); len(messages) != 1 {
			t.Fatalf("messages len = %d, want 1", len(messages))
		}
	})
}

func TestConvertRequestContentBlocks(t *testing.T) {
	body := `{
		"model": "claude-3",
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "看图"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAA"}}
			]},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "想想"},
				{"type": "text", "text": "调用工具"},
				{"type": "tool_use", "id": "t1", "name": "fn", "input": {"a": 1}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": [{"type": "text", "text": "结果"}]},
				{"type": "text", "text": "继续"}
			]}
		]
	}`
	out := convertMessages(t, body, RequestOptions{})
	messages := messageList(t, out)
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4: %s", len(messages), jsonText(t, messages))
	}

	// 1) text + image → content part 数组（base64 → data URI）。
	wantUser := `{"content":[{"text":"看图","type":"text"},{"image_url":{"url":"data:image/png;base64,AAA"},"type":"image_url"}],"role":"user"}`
	if got := jsonText(t, at(t, messages, 0)); got != wantUser {
		t.Fatalf("user 消息 = %s, want %s", got, wantUser)
	}

	// 2) thinking → reasoning_content，tool_use → tool_calls，text → content。
	assistant := at(t, messages, 1)
	if got, _ := asString(assistant["reasoning_content"]); got != "想想" {
		t.Fatalf("reasoning_content = %q, want 想想", got)
	}
	if got, _ := asString(assistant["content"]); got != "调用工具" {
		t.Fatalf("assistant content = %q", got)
	}
	calls, ok := asArray(assistant["tool_calls"])
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %s", jsonText(t, assistant["tool_calls"]))
	}
	wantCall := `{"function":{"arguments":"{\"a\":1}","name":"fn"},"id":"t1","type":"function"}`
	if got := jsonText(t, calls[0]); got != wantCall {
		t.Fatalf("tool_call = %s, want %s", got, wantCall)
	}

	// 3) tool_result → 独立的 tool 消息，正文保留成紧随其后的 user 消息。
	if got := jsonText(t, at(t, messages, 2)); got != `{"content":"结果","role":"tool","tool_call_id":"t1"}` {
		t.Fatalf("tool 消息 = %s", got)
	}
	if got := jsonText(t, at(t, messages, 3)); got != `{"content":"继续","role":"user"}` {
		t.Fatalf("user 消息 = %s", got)
	}
}

func TestConvertRequestToolCallWithoutThinkingUsesPlaceholder(t *testing.T) {
	// ocgo 的 assistantToolCallsMessage 在没有回显缓存时补最小占位（common.go::cachedReasoningContent）。
	body := `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"fn","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]}`
	out := convertMessages(t, body, RequestOptions{})
	assistant := at(t, messageList(t, out), 0)
	if got, _ := asString(assistant["reasoning_content"]); got != "Tool call requested." {
		t.Fatalf("reasoning_content = %q, want 占位", got)
	}
	if assistant["content"] != "" {
		t.Fatalf("content = %s, want 空字符串", jsonText(t, assistant["content"]))
	}
}

func TestConvertRequestReasoningCacheEcho(t *testing.T) {
	// 规则 10：reasoning_content 回显缓存按 tool_call id 工作。
	cacheReasoningContent([]string{"echo-1"}, "缓存里的推理")
	body := `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"echo-1","name":"fn","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"echo-1","content":"ok"}]}
	]}`
	out := convertMessages(t, body, RequestOptions{})
	assistant := at(t, messageList(t, out), 0)
	if got, _ := asString(assistant["reasoning_content"]); got != "缓存里的推理" {
		t.Fatalf("reasoning_content = %q, want 缓存里的推理", got)
	}

	// Responses 方向同样命中缓存。
	cached := convertResponses(t, newConvSession(), `{"input":[{"type":"function_call","call_id":"echo-1","name":"fn","arguments":"{}"}]}`, RequestOptions{})
	if got, _ := asString(at(t, messageList(t, cached), 0)["reasoning_content"]); got != "缓存里的推理" {
		t.Fatalf("responses reasoning_content = %q", got)
	}
}

func TestConvertRequestToolsAndToolChoice(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"do.thing","description":"做事","input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}]}`
	out := convertMessages(t, body, RequestOptions{})
	tools, ok := asArray(out["tools"])
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %s", jsonText(t, out["tools"]))
	}
	wantTool := `{"function":{"description":"做事","name":"do.thing","parameters":{"properties":{"x":{"type":"string"}},"type":"object"}},"type":"function"}`
	if got := jsonText(t, tools[0]); got != wantTool {
		t.Fatalf("tool = %s, want %s", got, wantTool)
	}

	// 缺 input_schema 时兜底（ocgo toolParametersOrDefault）。
	bare := convertMessages(t, `{"model":"m","max_tokens":8,"messages":[],"tools":[{"name":"f"}]}`, RequestOptions{})
	bareTools, _ := asArray(bare["tools"])
	if got := jsonText(t, bareTools[0]); got != `{"function":{"name":"f","parameters":{"properties":{},"type":"object"}},"type":"function"}` {
		t.Fatalf("兜底 tool = %s", got)
	}

	tests := []struct {
		name       string
		choice     string
		wantChoice string
	}{
		{"auto", `{"type":"auto"}`, `"auto"`},
		{"any → required", `{"type":"any"}`, `"required"`},
		{"none", `{"type":"none"}`, `"none"`},
		{"tool → function", `{"type":"tool","name":"do.thing"}`, `{"function":{"name":"do.thing"},"type":"function"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			converted := convertMessages(t, `{"model":"m","max_tokens":8,"messages":[],"tool_choice":`+tt.choice+`}`, RequestOptions{})
			if got := jsonText(t, converted["tool_choice"]); got != tt.wantChoice {
				t.Fatalf("tool_choice = %s, want %s", got, tt.wantChoice)
			}
		})
	}
}

func TestConvertRequestStopSequencesAndMaxTokens(t *testing.T) {
	body := `{"model":"claude-3","max_tokens":1024,"temperature":0.5,"top_p":0.9,"top_k":20,
		"stop_sequences":["STOP","END"],"metadata":{"user_id":"u1"},"messages":[{"role":"user","content":"hi"}]}`
	out := convertMessages(t, body, RequestOptions{})
	if got := jsonText(t, out["stop"]); got != `["STOP","END"]` {
		t.Fatalf("stop = %s", got)
	}
	if out["stop_sequences"] != nil {
		t.Fatalf("stop_sequences must not be forwarded: %s", jsonText(t, out["stop_sequences"]))
	}
	if n, _ := asInt(out["max_tokens"]); n != 1024 {
		t.Fatalf("max_tokens = %v", out["max_tokens"])
	}
	if got := jsonText(t, out["temperature"]); got != "0.5" {
		t.Fatalf("temperature = %s", got)
	}
	if got := jsonText(t, out["top_k"]); got != "20" {
		t.Fatalf("top_k = %s", got)
	}
	if got := jsonText(t, out["metadata"]); got != `{"user_id":"u1"}` {
		t.Fatalf("metadata = %s", got)
	}

	// max_tokens 缺失时兜底 4096（ocgo ensureAnthropicRequestDefaults）。
	fallback := convertMessages(t, `{"model":"m","messages":[]}`, RequestOptions{})
	if n, _ := asInt(fallback["max_tokens"]); n != defaultAnthropicMaxTokens {
		t.Fatalf("max_tokens = %v, want %d", fallback["max_tokens"], defaultAnthropicMaxTokens)
	}
}

func TestConvertRequestReasoningEffortFolding(t *testing.T) {
	tests := []struct {
		name  string
		extra string
		want  string
	}{
		{"thinking enabled", `"thinking":{"type":"enabled"}`, "high"},
		{"thinking enabled + budget", `"thinking":{"type":"enabled","budget_tokens":5000}`, "high"},
		{"thinking level", `"thinking":{"level":"low"}`, "low"},
		{"output_config", `"output_config":{"effort":"low"}`, "low"},
		{"reasoning_effort 别名", `"reasoning_effort":"xhigh"`, "high"},
		{"effort", `"effort":"minimal"`, "minimal"},
		{"level 数字", `"level":3`, "high"},
		{"depth 嵌套", `"depth":{"depth":"medium"}`, "medium"},
		{"无来源", ``, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":8,"messages":[]`
			if tt.extra != "" {
				body += "," + tt.extra
			}
			body += "}"
			out := convertMessages(t, body, RequestOptions{})
			got, _ := asString(out["reasoning_effort"])
			if got != tt.want {
				t.Fatalf("reasoning_effort = %q, want %q", got, tt.want)
			}
			// 规则 8：来源键不得出现在出站体里。
			for _, key := range []string{"thinking", "output_config", "reasoning", "effort", "level", "depth", "reasoning_effort"} {
				if key == "reasoning_effort" {
					continue
				}
				if v, ok := out[key]; ok {
					t.Fatalf("来源键 %s 泄漏到出站体: %s", key, jsonText(t, v))
				}
			}
		})
	}

	// Responses 方向的 reasoning.effort 同样折叠（Codex 夹具形态）。
	out := convertResponses(t, newConvSession(), `{"model":"gpt-5-codex","input":"hi","reasoning":{"effort":"medium","summary":"auto"}}`, RequestOptions{})
	if got, _ := asString(out["reasoning_effort"]); got != "medium" {
		t.Fatalf("reasoning_effort = %q, want medium", got)
	}
	if _, ok := out["reasoning"]; ok {
		t.Fatalf("reasoning 不应出现在出站体: %s", jsonText(t, out))
	}
}

func TestConvertRequestModelOverride(t *testing.T) {
	body := `{"model":"claude-3","max_tokens":8,"messages":[]}`
	if got, _ := asString(convertMessages(t, body, RequestOptions{})["model"]); got != "claude-3" {
		t.Fatalf("空 opts.Model 应保留入站 model，got %q", got)
	}
	if got, _ := asString(convertMessages(t, body, RequestOptions{Model: "upstream-1"})["model"]); got != "upstream-1" {
		t.Fatalf("opts.Model 应覆盖入站 model，got %q", got)
	}
	// Responses 方向同理。
	responses := `{"model":"gpt-4o","input":"hi"}`
	if got, _ := asString(convertResponses(t, newConvSession(), responses, RequestOptions{Model: "upstream-2"})["model"]); got != "upstream-2" {
		t.Fatalf("responses model = %q", got)
	}
	if got, _ := asString(convertResponses(t, newConvSession(), responses, RequestOptions{})["model"]); got != "gpt-4o" {
		t.Fatalf("responses model = %q", got)
	}
}

func TestConvertRequestCacheControlPreserved(t *testing.T) {
	body := `{
		"model": "m", "max_tokens": 8,
		"system": [{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages": [{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],
		"tools": [{"name":"f","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]
	}`
	out := convertMessages(t, body, RequestOptions{})
	system := at(t, messageList(t, out), 0)
	content, _ := asArray(system["content"])
	block, _ := asMap(content[0])
	if got := jsonText(t, block["cache_control"]); got != `{"type":"ephemeral"}` {
		t.Fatalf("system cache_control = %s", got)
	}
	user := at(t, messageList(t, out), 1)
	parts, _ := asArray(user["content"])
	part, _ := asMap(parts[0])
	if got := jsonText(t, part["cache_control"]); got != `{"type":"ephemeral"}` {
		t.Fatalf("text part cache_control = %s", got)
	}
	tools, _ := asArray(out["tools"])
	tool, _ := asMap(tools[0])
	if got := jsonText(t, tool["cache_control"]); got != `{"type":"ephemeral"}` {
		t.Fatalf("tool cache_control = %s", got)
	}
}

// ==================== Responses → Chat ====================

func TestResponsesNamespaceFlattening(t *testing.T) {
	req := `{
		"model": "gpt-5-codex",
		"instructions": "sys",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
			{"type": "additional_tools", "tools": [
				{"type": "namespace", "name": "ns_a", "tools": [
					{"type": "function", "name": "tool.one", "description": "one", "parameters": {"type": "object"}},
					{"type": "function", "name": "tool.one", "description": "dup", "parameters": {"type": "object"}}
				]},
				{"type": "namespace", "name": "ns.b", "tools": [
					{"type": "function", "name": "tool.two", "parameters": {"type": "object"}}
				]}
			]}
		]
	}`
	session := newConvSession()
	out := convertResponses(t, session, req, RequestOptions{})

	tools, ok := asArray(out["tools"])
	if !ok || len(tools) != 3 {
		t.Fatalf("tools = %s", jsonText(t, out["tools"]))
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		m, _ := asMap(tool)
		fn, _ := asMap(m["function"])
		name, _ := asString(fn["name"])
		names = append(names, name)
	}
	for _, want := range []string{"ns_a__tool_one", "ns_a__tool_one_2", "ns_b__tool_two"} {
		if !containsStringValue(names, want) {
			t.Fatalf("展平后的工具名缺少 %q：%v", want, names)
		}
	}

	// nsReverse 回来的是**原始** namespace / subtool 名（含点号），冲突项加 _2 后缀。
	wantReverse := map[string]nsEntry{
		"ns_a__tool_one":   {namespace: "ns_a", subtool: "tool.one"},
		"ns_a__tool_one_2": {namespace: "ns_a", subtool: "tool.one"},
		"ns_b__tool_two":   {namespace: "ns.b", subtool: "tool.two"},
	}
	if !reflect.DeepEqual(session.nsReverse, wantReverse) {
		t.Fatalf("nsReverse = %#v, want %#v", session.nsReverse, wantReverse)
	}

	t.Run("两个 session 不共享状态", func(t *testing.T) {
		s1, s2 := newConvSession(), newConvSession()
		convertResponses(t, s1, `{"input":"a","tools":[{"type":"namespace","name":"ns_1","tools":[{"type":"function","name":"t1"}]}]}`, RequestOptions{})
		convertResponses(t, s2, `{"input":"b","tools":[{"type":"namespace","name":"ns_2","tools":[{"type":"function","name":"t2"}]}]}`, RequestOptions{})
		if _, ok := s1.nsReverse["ns_1__t1"]; !ok {
			t.Fatalf("s1 缺少自己的展平名: %#v", s1.nsReverse)
		}
		if _, ok := s1.nsReverse["ns_2__t2"]; ok {
			t.Fatalf("s1 泄漏了 s2 的状态: %#v", s1.nsReverse)
		}
		if _, ok := s2.nsReverse["ns_2__t2"]; !ok {
			t.Fatalf("s2 缺少自己的展平名: %#v", s2.nsReverse)
		}
		if _, ok := s2.nsReverse["ns_1__t1"]; ok {
			t.Fatalf("s2 泄漏了 s1 的状态: %#v", s2.nsReverse)
		}
	})

	t.Run("零值 session 也能用", func(t *testing.T) {
		var zero convSession
		convertResponses(t, &zero, `{"input":"a","tools":[{"type":"namespace","name":"ns_z","tools":[{"type":"function","name":"tz"}]}]}`, RequestOptions{})
		if _, ok := zero.nsReverse["ns_z__tz"]; !ok {
			t.Fatalf("零值 session 未写入 nsReverse")
		}
	})
}

func TestResponsesNamespaceNameSanitization(t *testing.T) {
	longName := strings.Repeat("a", 200)
	req := `{"input":"t","tools":[
		{"type":"namespace","name":"ns.with.dots","tools":[{"type":"function","name":"sub.tool","parameters":{"type":"object"}}]},
		{"type":"namespace","name":"` + longName + `","tools":[{"type":"function","name":"tool","parameters":{"type":"object"}}]}
	]}`
	session := newConvSession()
	out := convertResponses(t, session, req, RequestOptions{})
	tools, _ := asArray(out["tools"])
	if len(tools) != 2 {
		t.Fatalf("tools len = %d, want 2", len(tools))
	}
	for _, tool := range tools {
		m, _ := asMap(tool)
		fn, _ := asMap(m["function"])
		name, _ := asString(fn["name"])
		if len(name) > 128 {
			t.Fatalf("展平后的工具名过长（%d）：%q", len(name), name)
		}
		for _, r := range name {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
			if !ok {
				t.Fatalf("展平后的工具名含非法字符：%q", name)
			}
		}
	}
	if _, ok := session.nsReverse["ns_with_dots__sub_tool"]; !ok {
		t.Fatalf("点号 namespace/subtool 应脱敏成 ns_with_dots__sub_tool：%#v", session.nsReverse)
	}
	if entry := session.nsReverse["ns_with_dots__sub_tool"]; entry.namespace != "ns.with.dots" || entry.subtool != "sub.tool" {
		// 反向映射里保存的是原始名，交给响应方向还原。
		t.Fatalf("nsReverse 应保存原始名，got %#v", entry)
	}
}

func TestResponsesCustomToolRegistration(t *testing.T) {
	req := `{"input":"t","tools":[
		{"type":"custom","name":"exec","description":"Run code","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}
	]}`
	session := newConvSession()
	out := convertResponses(t, session, req, RequestOptions{})
	tools, _ := asArray(out["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools = %s", jsonText(t, out["tools"]))
	}
	fn, _ := asMap(at(t, tools, 0)["function"])
	name, _ := asString(fn["name"])
	if name != "exec" {
		t.Fatalf("custom 工具名 = %q", name)
	}
	desc, _ := asString(fn["description"])
	if !strings.Contains(desc, "Format:") {
		t.Fatalf("custom 工具描述应带 Format 附录：%q", desc)
	}
	params, _ := asMap(fn["parameters"])
	props, _ := asMap(params["properties"])
	content, _ := asMap(props["content"])
	if got, _ := asString(content["type"]); got != "string" {
		t.Fatalf("custom 工具 content 参数类型 = %q, want string", got)
	}
	if got := session.nsReverse["exec"]; got != (nsEntry{namespace: CUSTOM_TOOL_NAMESPACE_MARKER, subtool: "exec"}) {
		t.Fatalf("nsReverse[exec] = %#v", got)
	}
}

func TestResponsesInputItems(t *testing.T) {
	req := `{
		"model": "gpt-4o",
		"instructions": "你是助手",
		"max_output_tokens": 256,
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "北京天气"}]},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "先想一下"}]},
			{"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": "{\"location\":\"北京\"}"},
			{"type": "function_call_output", "call_id": "c1", "output": "晴"}
		],
		"text": {"format": {"type": "json_schema", "name": "answer", "schema": {"type": "object"}, "strict": true}}
	}`
	out := convertResponses(t, newConvSession(), req, RequestOptions{})
	if n, _ := asInt(out["max_tokens"]); n != 256 {
		t.Fatalf("max_tokens = %v", out["max_tokens"])
	}
	messages := messageList(t, out)
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4: %s", len(messages), jsonText(t, messages))
	}
	if got := jsonText(t, at(t, messages, 0)); got != `{"content":"你是助手","role":"system"}` {
		t.Fatalf("system = %s", got)
	}
	if got := jsonText(t, at(t, messages, 1)); got != `{"content":"北京天气","role":"user"}` {
		t.Fatalf("user = %s", got)
	}
	// reasoning 项并入上一条 assistant（这里是新建的 assistant），function_call 再并入同一条。
	assistant := at(t, messages, 2)
	if got, _ := asString(assistant["reasoning_content"]); got != "先想一下" {
		t.Fatalf("reasoning_content = %q", got)
	}
	calls, _ := asArray(assistant["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %s", jsonText(t, assistant["tool_calls"]))
	}
	if got := jsonText(t, at(t, messages, 3)); got != `{"content":"晴","role":"tool","tool_call_id":"c1"}` {
		t.Fatalf("tool = %s", got)
	}

	rf, ok := asMap(out["response_format"])
	if !ok {
		t.Fatalf("response_format = %s", jsonText(t, out["response_format"]))
	}
	js, _ := asMap(rf["json_schema"])
	if name, _ := asString(js["name"]); name != "answer" {
		t.Fatalf("response_format.json_schema.name = %q", name)
	}
	if strict, _ := asBool(js["strict"]); !strict {
		t.Fatalf("response_format.json_schema.strict 应为 true")
	}
}

func TestResponsesInputStringAndToolChoice(t *testing.T) {
	out := convertResponses(t, newConvSession(), `{"model":"gpt-4o","input":"北京天气","tool_choice":{"type":"function","name":"f"}}`, RequestOptions{})
	messages := messageList(t, out)
	if got := jsonText(t, at(t, messages, 0)); got != `{"content":"北京天气","role":"user"}` {
		t.Fatalf("user = %s", got)
	}
	if got := jsonText(t, out["tool_choice"]); got != `{"function":{"name":"f"},"type":"function"}` {
		t.Fatalf("tool_choice = %s", got)
	}
	if got := jsonText(t, convertResponses(t, newConvSession(), `{"input":"x","tool_choice":"auto"}`, RequestOptions{})["tool_choice"]); got != `"auto"` {
		t.Fatalf("tool_choice = %s", got)
	}
}

func TestResponsesBuiltinTools(t *testing.T) {
	req := `{"input":"t","tools":[
		{"type":"function","name":"get_weather","parameters":{"type":"object"}},
		{"type":"web_search","external_web_access":false},
		{"type":"web_search_preview"},
		{"type":"file_search","vector_store_ids":["vs_1"]},
		{"type":"computer"},
		{"type":"mcp","server_label":"srv"},
		{"type":"totally_unknown","name":"nope"}
	]}`
	out := convertResponses(t, newConvSession(), req, RequestOptions{})
	tools, _ := asArray(out["tools"])
	names := make([]string, 0, len(tools))
	types := make([]string, 0, len(tools))
	for _, tool := range tools {
		m, _ := asMap(tool)
		typ, _ := asString(m["type"])
		types = append(types, typ)
		if fn, ok := asMap(m["function"]); ok {
			name, _ := asString(fn["name"])
			names = append(names, name)
		}
	}
	// web_search 保留成 Chat 侧内建类型（Rust responses_to_openai_with_ns 行为，测试锁定）。
	webSearch := 0
	for _, typ := range types {
		if typ == "web_search" {
			webSearch++
		}
	}
	if webSearch != 2 {
		t.Fatalf("web_search 工具数 = %d, want 2（types=%v）", webSearch, types)
	}
	// mcp 原样透传，未知类型丢弃（Rust 行为）。
	if !containsStringValue(types, "mcp") {
		t.Fatalf("mcp 工具应原样透传：%v", types)
	}
	if containsStringValue(names, "nope") {
		t.Fatalf("未知内建类型不应落成工具：%v", names)
	}
	// 规则 6：file_search / computer 这类内建工具落成 function 工具，而不是被丢弃。
	for _, want := range []string{"get_weather", "file_search", "computer"} {
		if !containsStringValue(names, want) {
			t.Fatalf("缺少 function 工具 %q：%v", want, names)
		}
	}
}

// TestCodexFixtureToolAccounting 用仓库里固定的 Codex Responses 夹具核对工具口径：
// 所有声明的工具都必须到达出站体，且反向映射条目数 =
// namespace 子工具数 + custom 工具数。
//
// 夹具原本放在 Rust 侧的 tests/fixtures/ 下，删除 Rust 代码时搬到了本包的 testdata/。
// 失配即为硬失败（不再 Skip），避免夹具丢失后测试被静默跳过、覆盖度无声下降。
func TestCodexFixtureToolAccounting(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "codex_responses_request.json"))
	if err != nil {
		t.Fatalf("夹具不可用：%v", err)
	}
	req, err := decodeObject(raw)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	session := newConvSession()
	out, err := session.responsesToChat(req, RequestOptions{Stream: true})
	if err != nil {
		t.Fatalf("responsesToChat: %v", err)
	}

	tools, _ := asArray(out["tools"])
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		m, _ := asMap(tool)
		if fn, ok := asMap(m["function"]); ok {
			name, _ := asString(fn["name"])
			names = append(names, name)
			continue
		}
		typ, _ := asString(m["type"])
		names = append(names, typ)
	}
	// namespace 2 个子工具 + function 2 + custom 1 + web_search 1 = 6。
	if len(tools) != 6 {
		t.Fatalf("tools(%d) = %v, want 6", len(tools), names)
	}
	for _, want := range []string{"mcp__repo__search_files", "mcp__repo__read_file", "shell", "apply_patch", "update_plan", "web_search"} {
		if !containsStringValue(names, want) {
			t.Fatalf("缺少工具 %q：%v", want, names)
		}
	}
	// nsReverse = namespace 子工具 2 + custom 1 = 3（Rust expected_reverse_count）。
	if len(session.nsReverse) != 3 {
		t.Fatalf("nsReverse(%d) = %#v", len(session.nsReverse), session.nsReverse)
	}
	if got := session.nsReverse["apply_patch"]; got != (nsEntry{namespace: CUSTOM_TOOL_NAMESPACE_MARKER, subtool: "apply_patch"}) {
		t.Fatalf("custom 工具未按标记注册：%#v", got)
	}
	if got := session.nsReverse["mcp__repo__read_file"]; got != (nsEntry{namespace: "mcp__repo", subtool: "read_file"}) {
		t.Fatalf("namespace 子工具未注册：%#v", got)
	}
	if got, _ := asString(out["reasoning_effort"]); got != "medium" {
		t.Fatalf("reasoning_effort = %q, want medium", got)
	}
	if streaming, _ := asBool(out["stream"]); !streaming {
		t.Fatal("stream 未打开")
	}
	if got := jsonText(t, out["stream_options"]); got != `{"include_usage":true}` {
		t.Fatalf("stream_options = %s", got)
	}
	if store, ok := asBool(out["store"]); !ok || store {
		t.Fatalf("store 应透传 false：%s", jsonText(t, out["store"]))
	}
}

// ==================== tool 消息清洗（规则 9）====================

func TestSanitizeToolMessages(t *testing.T) {
	t.Run("缺失 tool_result 补占位", func(t *testing.T) {
		out := convertResponses(t, newConvSession(), `{"input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`, RequestOptions{})
		messages := messageList(t, out)
		if len(messages) != 2 {
			t.Fatalf("messages = %s", jsonText(t, messages))
		}
		if got := jsonText(t, at(t, messages, 1)); got != `{"content":"`+unavailableToolResultContent+`","role":"tool","tool_call_id":"c1"}` {
			t.Fatalf("占位 tool 消息 = %s", got)
		}
	})

	t.Run("孤儿 tool 消息被丢弃", func(t *testing.T) {
		out := convertResponses(t, newConvSession(), `{"input":[{"type":"function_call_output","call_id":"c9","output":"x"}]}`, RequestOptions{})
		if messages := messageList(t, out); len(messages) != 0 {
			t.Fatalf("孤儿 tool 消息应被丢弃，got %s", jsonText(t, messages))
		}
	})

	t.Run("重复结果只保留第一条", func(t *testing.T) {
		out := convertResponses(t, newConvSession(), `{"input":[
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"c1","output":"first"},
			{"type":"function_call_output","call_id":"c1","output":"second"}
		]}`, RequestOptions{})
		messages := messageList(t, out)
		if len(messages) != 2 {
			t.Fatalf("messages = %s", jsonText(t, messages))
		}
		if got := jsonText(t, at(t, messages, 1)); got != `{"content":"first","role":"tool","tool_call_id":"c1"}` {
			t.Fatalf("tool = %s", got)
		}
	})

	t.Run("清理器直接作用于出站体", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "tool", "tool_call_id": "orphan", "content": "x"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
			}},
		}}
		sanitizeRawChatToolMessages(body)
		messages, _ := asArray(body["messages"])
		if len(messages) != 2 {
			t.Fatalf("sanitized = %s", jsonText(t, messages))
		}
		if got := jsonText(t, messages[1]); got != `{"content":"`+unavailableToolResultContent+`","role":"tool","tool_call_id":"c1"}` {
			t.Fatalf("占位 = %s", got)
		}
	})
}

// ==================== 工具结果预算（ocgo truncate）====================

func TestConvertRequestToolResultTruncation(t *testing.T) {
	long := strings.Repeat("a", maxAnthropicToolResultContentChars+10000)
	body := `{"model":"m","max_tokens":8,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + long + `"}]}
	]}`
	out := convertMessages(t, body, RequestOptions{})
	tool := at(t, messageList(t, out), 1)
	content, _ := asString(tool["content"])
	if !strings.HasPrefix(content, strings.Repeat("a", maxAnthropicToolResultContentChars)) {
		t.Fatalf("截断后的内容前缀不对（len=%d）", len(content))
	}
	if !strings.HasSuffix(content, "[ocgo truncated tool_result content: omitted 10000 characters]") {
		t.Fatalf("缺少截断提示：%q", content[len(content)-80:])
	}
	if runes := []rune(content); len(runes) != maxAnthropicToolResultContentChars+len([]rune("\n\n[ocgo truncated tool_result content: omitted 10000 characters]")) {
		t.Fatalf("截断后长度 = %d", len(runes))
	}
}

// ==================== 流式 usage 与 stream_options（规则 2）====================

func TestStreamingUsageAndStreamOptions(t *testing.T) {
	t.Run("Messages 方向：opts.Stream 打开 include_usage 并保留既有兄弟键", func(t *testing.T) {
		body := `{"model":"m","max_tokens":8,"stream_options":{"chunk_size":10},"messages":[{"role":"user","content":"hi"}]}`
		out := convertMessages(t, body, RequestOptions{Stream: true})
		if streaming, _ := asBool(out["stream"]); !streaming {
			t.Fatalf("stream 未打开：%s", jsonText(t, out))
		}
		if got := jsonText(t, out["stream_options"]); got != `{"chunk_size":10,"include_usage":true}` {
			t.Fatalf("stream_options = %s", got)
		}
	})

	t.Run("Responses 方向：opts.Stream 打开 include_usage", func(t *testing.T) {
		out := convertResponses(t, newConvSession(), `{"input":"hi"}`, RequestOptions{Stream: true})
		if streaming, _ := asBool(out["stream"]); !streaming {
			t.Fatalf("stream 未打开：%s", jsonText(t, out))
		}
		if got := jsonText(t, out["stream_options"]); got != `{"include_usage":true}` {
			t.Fatalf("stream_options = %s", got)
		}
	})

	t.Run("非流式移除入口的 stream 字段", func(t *testing.T) {
		out := convertMessages(t, `{"model":"m","max_tokens":8,"stream":true,"messages":[]}`, RequestOptions{})
		if _, ok := out["stream"]; ok {
			t.Fatalf("stream 应被移除：%s", jsonText(t, out))
		}
	})

	t.Run("requestStreamingUsage 的返回值与幂等", func(t *testing.T) {
		req := map[string]any{"stream": true}
		if !requestStreamingUsage(req) {
			t.Fatalf("首次注入应返回 true")
		}
		if requestStreamingUsage(req) {
			t.Fatalf("已开启时应返回 false")
		}
		if got := jsonText(t, req["stream_options"]); got != `{"include_usage":true}` {
			t.Fatalf("stream_options = %s", got)
		}
		if requestStreamingUsage(map[string]any{"stream": false}) {
			t.Fatalf("非流式应返回 false")
		}
		if requestStreamingUsage(map[string]any{}) {
			t.Fatalf("无 stream 字段应返回 false")
		}
	})

	t.Run("门面 EnsureChatStreamUsage 复用同一实现", func(t *testing.T) {
		raw, err := EnsureChatStreamUsage([]byte(`{"stream":true,"stream_options":{"foo":1}}`))
		if err != nil {
			t.Fatalf("EnsureChatStreamUsage: %v", err)
		}
		if got := string(raw); got != `{"stream":true,"stream_options":{"foo":1,"include_usage":true}}` {
			t.Fatalf("EnsureChatStreamUsage = %s", got)
		}
		plain, err := EnsureChatStreamUsage([]byte(`{"stream":false}`))
		if err != nil {
			t.Fatalf("EnsureChatStreamUsage: %v", err)
		}
		if string(plain) != `{"stream":false}` {
			t.Fatalf("非流式不应改动：%s", plain)
		}
	})
}

// ==================== 图片闸门 ====================

func TestImageGate(t *testing.T) {
	chatBody := func() map[string]any {
		return map[string]any{
			"model": "no-vision",
			"messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "http://img", "detail": "high"},
						"detail":    "low",
					},
				}},
			},
		}
	}

	if !rawChatBodyHasImages(chatBody()) {
		t.Fatalf("rawChatBodyHasImages 应为 true")
	}
	if rawChatBodyHasImages(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "text"}}}) {
		t.Fatalf("纯文本不应判为图片请求")
	}

	t.Run("SupportsImages=false 时报错", func(t *testing.T) {
		_, err := finalizeChatRequest(chatBody(), RequestOptions{SupportsImages: false})
		if err == nil {
			t.Fatalf("应返回错误")
		}
		if !strings.Contains(err.Error(), "model no-vision does not support image inputs") {
			t.Fatalf("错误文案 = %q", err.Error())
		}
	})

	t.Run("SupportsImages=true 时去掉 detail", func(t *testing.T) {
		out, err := finalizeChatRequest(chatBody(), RequestOptions{SupportsImages: true})
		if err != nil {
			t.Fatalf("不应报错：%v", err)
		}
		if strings.Contains(string(out), "detail") {
			t.Fatalf("detail 未被剥离：%s", out)
		}
	})

	t.Run("stripRawChatImageDetails 就地剥离", func(t *testing.T) {
		body := chatBody()
		stripRawChatImageDetails(body)
		if strings.Contains(jsonText(t, body), "detail") {
			t.Fatalf("detail 未被剥离：%s", jsonText(t, body))
		}
	})

	t.Run("模型名为空时用 unknown", func(t *testing.T) {
		if got := unsupportedImageModelError("").Error(); got != "model unknown does not support image inputs" {
			t.Fatalf("错误文案 = %q", got)
		}
	})

	t.Run("端到端：带图片的 Messages 请求被拦下", func(t *testing.T) {
		body := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"http://img"}}]}]}`)
		session := newConvSession()
		if _, err := session.buildUpstreamRequest(FormatAnthropic, FormatOpenAIChat, body, RequestOptions{SupportsImages: false}); err == nil {
			t.Fatalf("应返回不支持图片的错误")
		}
		if _, err := session.buildUpstreamRequest(FormatAnthropic, FormatOpenAIChat, body, RequestOptions{SupportsImages: true}); err != nil {
			t.Fatalf("支持图片时不应报错：%v", err)
		}
	})
}

// ==================== Responses 跨格式校验（规则 7）====================

func TestValidateResponsesCrossFormat(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "previous_response_id 拒绝",
			body:    `{"previous_response_id":"resp_1","input":"next"}`,
			wantErr: "Responses field 'previous_response_id' cannot be continued across protocol conversion",
		},
		{
			name:    "conversation 拒绝",
			body:    `{"conversation":{"id":"conv_1"},"input":"next"}`,
			wantErr: "Responses field 'conversation' cannot be continued across protocol conversion",
		},
		{
			name:    "encrypted reasoning 无 summary 拒绝",
			body:    `{"input":[{"type":"reasoning","encrypted_content":"secret","summary":[]}]}`,
			wantErr: "Responses encrypted reasoning has no portable summary for protocol conversion",
		},
		{
			name:    "encrypted reasoning 缺 summary 字段同样拒绝",
			body:    `{"input":[{"type":"reasoning","encrypted_content":"secret"}]}`,
			wantErr: "Responses encrypted reasoning has no portable summary for protocol conversion",
		},
		{
			name: "include 开关不拒绝",
			body: `{"include":["reasoning.encrypted_content"],"input":"next"}`,
		},
		{
			name: "有 summary 的 reasoning 可搬运",
			body: `{"input":[{"type":"reasoning","encrypted_content":"secret","summary":[{"type":"summary_text","text":"t"}]}]}`,
		},
		{
			name: "可搬运的普通请求",
			body: `{"model":"gpt-5-codex","input":"hi","tools":[{"type":"function","name":"f"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResponsesCrossFormat(mustDecode(t, tt.body))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("不应报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错 %q", tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("错误文案 = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}

	if err := validateResponsesCrossFormat(nil); err != nil {
		t.Fatalf("空请求不应报错：%v", err)
	}

	// 字节级门面：非法 JSON / 顶层非对象按 Rust 的 Value::get 语义放行。
	if err := ValidateResponsesCrossFormat([]byte(`{"previous_response_id":"x"}`)); err == nil {
		t.Fatalf("门面应返回错误")
	}
	if err := ValidateResponsesCrossFormat([]byte(`[]`)); err != nil {
		t.Fatalf("非对象请求体不应报错：%v", err)
	}
}

// ==================== Anthropic prompt caching 断点注入 ====================

func TestInjectAnthropicCacheBreakpoints(t *testing.T) {
	body := `{
		"model": "claude-3",
		"system": "sys",
		"tools": [{"name": "a", "input_schema": {"type": "object"}}, {"name": "b", "input_schema": {"type": "object"}}],
		"messages": [
			{"role": "user", "content": "one"},
			{"role": "assistant", "content": "two"},
			{"role": "user", "content": "three"}
		]
	}`
	req := mustDecode(t, body)
	if !injectAnthropicCacheBreakpoints(req) {
		t.Fatalf("首次注入应返回 true")
	}
	if count := strings.Count(jsonText(t, req), `"cache_control"`); count != 4 {
		t.Fatalf("断点数 = %d, want 4：%s", count, jsonText(t, req))
	}
	// system 字符串 → 单元素 text 数组。
	system, ok := asArray(req["system"])
	if !ok || len(system) != 1 {
		t.Fatalf("system = %s", jsonText(t, req["system"]))
	}
	// 幂等：已有任何 cache_control 就不再改动。
	before := jsonText(t, req)
	if injectAnthropicCacheBreakpoints(req) {
		t.Fatalf("第二次注入应返回 false")
	}
	if after := jsonText(t, req); after != before {
		t.Fatalf("幂等性被破坏：\n%s\n%s", before, after)
	}

	t.Run("已有 cache_control 时原样返回", func(t *testing.T) {
		other := mustDecode(t, `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[]}`)
		if injectAnthropicCacheBreakpoints(other) {
			t.Fatalf("应返回 false")
		}
	})

	t.Run("只有 messages 时给最后一条末块打点", func(t *testing.T) {
		single := mustDecode(t, `{"messages":[{"role":"user","content":"hi"}]}`)
		if !injectAnthropicCacheBreakpoints(single) {
			t.Fatalf("应返回 true")
		}
		messages, _ := asArray(single["messages"])
		msg, _ := asMap(messages[0])
		content, ok := asArray(msg["content"])
		if !ok || len(content) != 1 {
			t.Fatalf("content = %s", jsonText(t, msg["content"]))
		}
	})

	t.Run("门面保持字节形态", func(t *testing.T) {
		raw, err := InjectAnthropicCacheBreakpoints([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("InjectAnthropicCacheBreakpoints: %v", err)
		}
		if !strings.Contains(string(raw), "cache_control") {
			t.Fatalf("未注入断点：%s", raw)
		}
		again, err := InjectAnthropicCacheBreakpoints(raw)
		if err != nil {
			t.Fatalf("InjectAnthropicCacheBreakpoints: %v", err)
		}
		if string(again) != string(raw) {
			t.Fatalf("幂等性被破坏")
		}
	})
}

// ==================== 不入参就地修改 ====================

func TestConvertRequestDoesNotMutateInput(t *testing.T) {
	body := `{"model":"m","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}]}`
	req := mustDecode(t, body)
	before := jsonText(t, req)
	if _, err := convertRequest(req, RequestOptions{}); err != nil {
		t.Fatalf("convertRequest: %v", err)
	}
	if after := jsonText(t, req); after != before {
		t.Fatalf("入参被就地修改：\n%s\n%s", before, after)
	}
}
