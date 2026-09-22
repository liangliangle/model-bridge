package converter

import (
	"testing"
)

// 本文件覆盖非流式响应方向：chat → messages 与 chat → responses。
// 表驱动 + 标准库；输入一律用原始 JSON 字符串，走与生产路径相同的 decodeObject。

// mustObject 解析 JSON 对象字面量，失败即测试失败。
func mustObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	obj, err := decodeObject([]byte(raw))
	if err != nil {
		t.Fatalf("decodeObject(%s): %v", raw, err)
	}
	return obj
}

// blockTypes 取出 Anthropic content 数组里各块的类型。
func blockTypes(t *testing.T, content any) []string {
	t.Helper()
	blocks, ok := asArray(content)
	if !ok {
		t.Fatalf("content 不是数组: %#v", content)
	}
	types := make([]string, 0, len(blocks))
	for _, raw := range blocks {
		block, ok := asMap(raw)
		if !ok {
			t.Fatalf("content 元素不是对象: %#v", raw)
		}
		blockType, _ := asString(block["type"])
		types = append(types, blockType)
	}
	return types
}

// itemTypes 取出 Responses output 数组里各项的类型。
func itemTypes(t *testing.T, output any) []string {
	t.Helper()
	items, ok := asArray(output)
	if !ok {
		t.Fatalf("output 不是数组: %#v", output)
	}
	types := make([]string, 0, len(items))
	for _, raw := range items {
		item, ok := asMap(raw)
		if !ok {
			t.Fatalf("output 元素不是对象: %#v", raw)
		}
		itemType, _ := asString(item["type"])
		types = append(types, itemType)
	}
	return types
}

// findItem 在 output 数组里找第一个指定类型的项。
func findItem(t *testing.T, output any, itemType string) map[string]any {
	t.Helper()
	items, ok := asArray(output)
	if !ok {
		t.Fatalf("output 不是数组: %#v", output)
	}
	for _, raw := range items {
		item, ok := asMap(raw)
		if !ok {
			continue
		}
		if got, _ := asString(item["type"]); got == itemType {
			return item
		}
	}
	return nil
}

// ==================== chat → messages（非流式）====================

func TestOpenAIToAnthropicResponse(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		model         string
		wantStop      string
		wantBlocks    []string
		wantText      string
		wantThinking  string
		wantToolName  string
		wantToolID    string
		wantToolInput map[string]any
		wantUsage     map[string]any // nil 表示不应出现 usage 字段
	}{
		{
			name: "文本与工具调用都保留",
			raw: `{
				"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-4o",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "let me check",
						"tool_calls": [{
							"id": "call_1", "type": "function",
							"function": {"name": "get_weather", "arguments": "{\"location\":\"SF\"}"}
						}]
					},
					"finish_reason": "tool_calls"
				}],
				"usage": {
					"prompt_tokens": 100, "completion_tokens": 5, "total_tokens": 105,
					"prompt_tokens_details": {"cached_tokens": 40}
				}
			}`,
			model:         "claude-x",
			wantStop:      "tool_use",
			wantBlocks:    []string{"text", "tool_use"},
			wantText:      "let me check",
			wantToolName:  "get_weather",
			wantToolID:    "call_1",
			wantToolInput: map[string]any{"location": "SF"},
			// Anthropic 口径：input_tokens 不含缓存命中，缓存单独列出。
			wantUsage: map[string]any{"input_tokens": 60, "output_tokens": 5, "cache_read_input_tokens": 40},
		},
		{
			name: "推理内容与文本，length 映成 max_tokens",
			raw: `{
				"id": "chatcmpl-2",
				"choices": [{
					"index": 0,
					"message": {"role": "assistant", "reasoning_content": "think", "content": "answer"},
					"finish_reason": "length"
				}]
			}`,
			model:        "claude-x",
			wantStop:     "max_tokens",
			wantBlocks:   []string{"thinking", "text"},
			wantThinking: "think",
			wantText:     "answer",
		},
		{
			name:       "空 choices 退化为空内容块",
			raw:        `{"id":"chatcmpl-3","choices":[]}`,
			model:      "claude-x",
			wantStop:   "end_turn",
			wantBlocks: []string{},
		},
		{
			name:       "上游没有 usage 键时不写 usage",
			raw:        `{"id":"chatcmpl-4","choices":[{"index":0,"message":{"content":"hi"},"finish_reason":"stop"}]}`,
			model:      "claude-x",
			wantStop:   "end_turn",
			wantBlocks: []string{"text"},
			wantText:   "hi",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustObject(t, tc.raw)
			out := openAIToAnthropicResponse(resp, tc.model)

			if got, _ := asString(out["type"]); got != "message" {
				t.Fatalf("type = %q, want message", got)
			}
			if got, _ := asString(out["role"]); got != "assistant" {
				t.Fatalf("role = %q, want assistant", got)
			}
			if got, _ := asString(out["model"]); got != tc.model {
				t.Fatalf("model = %q, want %q", got, tc.model)
			}
			if got, _ := asString(out["stop_reason"]); got != tc.wantStop {
				t.Fatalf("stop_reason = %q, want %q", got, tc.wantStop)
			}
			if _, present := out["stop_sequence"]; !present {
				t.Fatalf("stop_sequence 字段必须存在（null）")
			}
			types := blockTypes(t, out["content"])
			if len(types) != len(tc.wantBlocks) {
				t.Fatalf("content 块 = %v, want %v", types, tc.wantBlocks)
			}
			for i, want := range tc.wantBlocks {
				if types[i] != want {
					t.Fatalf("content 块 = %v, want %v", types, tc.wantBlocks)
				}
			}
			blocks, _ := asArray(out["content"])
			for _, raw := range blocks {
				block, _ := asMap(raw)
				switch block["type"] {
				case "text":
					if got, _ := asString(block["text"]); got != tc.wantText {
						t.Fatalf("text = %q, want %q", got, tc.wantText)
					}
				case "thinking":
					if got, _ := asString(block["thinking"]); got != tc.wantThinking {
						t.Fatalf("thinking = %q, want %q", got, tc.wantThinking)
					}
				case "tool_use":
					if got, _ := asString(block["id"]); got != tc.wantToolID {
						t.Fatalf("tool_use.id = %q, want %q", got, tc.wantToolID)
					}
					if got, _ := asString(block["name"]); got != tc.wantToolName {
						t.Fatalf("tool_use.name = %q, want %q", got, tc.wantToolName)
					}
					input, ok := asMap(block["input"])
					if !ok {
						t.Fatalf("tool_use.input 不是对象: %#v", block["input"])
					}
					for key, want := range tc.wantToolInput {
						if got, _ := asString(input[key]); got != want {
							t.Fatalf("tool_use.input[%s] = %v, want %v", key, input[key], want)
						}
					}
				}
			}

			usageRaw, present := out["usage"]
			if tc.wantUsage == nil {
				if present {
					t.Fatalf("不应出现 usage 字段: %#v", usageRaw)
				}
				return
			}
			if !present {
				t.Fatalf("缺少 usage 字段")
			}
			usage, ok := asMap(usageRaw)
			if !ok {
				t.Fatalf("usage 不是对象: %#v", usageRaw)
			}
			for key, want := range tc.wantUsage {
				n, _ := asInt(want)
				if got := intField(usage, key); got != int(n) {
					t.Fatalf("usage[%s] = %d, want %d", key, got, n)
				}
			}
		})
	}
}

// ==================== chat → responses（非流式）====================

func TestToResponsesWithNS(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantStatus string
		wantItems  []string
	}{
		{
			name: "文本 + 工具调用：message 与 function_call 两个 item",
			raw: `{
				"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-4o",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "done",
						"tool_calls": [{
							"id": "call_1", "type": "function",
							"function": {"name": "get_weather", "arguments": "{\"location\":\"SF\"}"}
						}]
					},
					"finish_reason": "tool_calls"
				}],
				"usage": {"prompt_tokens": 100, "completion_tokens": 5, "prompt_tokens_details": {"cached_tokens": 40}}
			}`,
			wantStatus: "completed",
			wantItems:  []string{"message", "function_call"},
		},
		{
			name:       "length → incomplete",
			raw:        `{"id":"chatcmpl-2","choices":[{"index":0,"message":{"content":"hi"},"finish_reason":"length"}]}`,
			wantStatus: "incomplete",
			wantItems:  []string{"message"},
		},
		{
			name:       "空响应：output 为空数组且仍带 usage",
			raw:        `{"id":"chatcmpl-3","choices":[{"index":0,"message":{"content":""},"finish_reason":"stop"}]}`,
			wantStatus: "completed",
			wantItems:  []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := newConvSession()
			resp := mustObject(t, tc.raw)
			out := session.toResponsesWithNS(resp, "gpt-4o")

			if got, _ := asString(out["object"]); got != "response" {
				t.Fatalf("object = %q, want response", got)
			}
			if got, _ := asString(out["status"]); got != tc.wantStatus {
				t.Fatalf("status = %q, want %q", got, tc.wantStatus)
			}
			if got, _ := asString(out["model"]); got != "gpt-4o" {
				t.Fatalf("model = %q", got)
			}
			types := itemTypes(t, out["output"])
			if len(types) != len(tc.wantItems) {
				t.Fatalf("output = %v, want %v", types, tc.wantItems)
			}
			for i, want := range tc.wantItems {
				if types[i] != want {
					t.Fatalf("output = %v, want %v", types, tc.wantItems)
				}
			}
			// usage 恒在（Responses 必填字段）。
			usage, ok := asMap(out["usage"])
			if !ok {
				t.Fatalf("usage 缺失或不是对象: %#v", out["usage"])
			}
			for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
				if _, present := usage[key]; !present {
					t.Fatalf("usage 缺少 %s: %#v", key, usage)
				}
			}
		})
	}
}

func TestToResponsesWithNSUsageConvention(t *testing.T) {
	session := newConvSession()
	resp := mustObject(t, `{
		"id": "chatcmpl-1",
		"choices": [{"index": 0, "message": {"content": "hi"}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 5, "total_tokens": 105,
		          "prompt_tokens_details": {"cached_tokens": 40}}
	}`)
	out := session.toResponsesWithNS(resp, "gpt-4o")
	usage, ok := asMap(out["usage"])
	if !ok {
		t.Fatalf("usage 不是对象: %#v", out["usage"])
	}
	// Responses 与 Chat 同为「含缓存」口径：input_tokens 必须回到原值。
	if got := intField(usage, "input_tokens"); got != 100 {
		t.Fatalf("input_tokens = %d, want 100", got)
	}
	if got := intField(usage, "output_tokens"); got != 5 {
		t.Fatalf("output_tokens = %d, want 5", got)
	}
	if got := intField(usage, "total_tokens"); got != 105 {
		t.Fatalf("total_tokens = %d, want 105", got)
	}
	details, ok := asMap(usage["input_tokens_details"])
	if !ok {
		t.Fatalf("input_tokens_details 缺失: %#v", usage)
	}
	if got := intField(details, "cached_tokens"); got != 40 {
		t.Fatalf("cached_tokens = %d, want 40", got)
	}
}

func TestToResponsesWithNSCustomToolRoundTrip(t *testing.T) {
	session := newConvSession()
	// 请求方向会把 custom 工具登记为「namespace 位置 = MARKER，subtool = 原名」。
	registerCustomTool("exec", session.nsReverse)

	resp := mustObject(t, `{
		"id": "chatcmpl-1",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"id": "call_custom_1", "type": "function",
					"function": {"name": "exec", "arguments": "{\"content\":\"return 1\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)
	out := session.toResponsesWithNS(resp, "gpt-5")
	item := findItem(t, out["output"], "custom_tool_call")
	if item == nil {
		t.Fatalf("缺少 custom_tool_call 输出项: %#v", out["output"])
	}
	if got, _ := asString(item["input"]); got != "return 1" {
		t.Fatalf("input = %q, want 解壳后的 return 1", got)
	}
	if _, present := item["arguments"]; present {
		t.Fatalf("custom_tool_call 不应带 arguments: %#v", item)
	}
	if got, _ := asString(item["call_id"]); got != "call_custom_1" {
		t.Fatalf("call_id = %q", got)
	}
	if got, _ := asString(item["id"]); got != "call_custom_1" {
		t.Fatalf("id = %q, want call_id（Rust 契约）", got)
	}
	if got, _ := asString(item["name"]); got != "exec" {
		t.Fatalf("name = %q, want exec", got)
	}
}

func TestToResponsesWithNSNamespaceRestore(t *testing.T) {
	session := newConvSession()
	// 请求方向：namespace 子工具被展平成 `<ns>__<sub>` 并登记反向映射。
	flat, ok := flattenNamespaceSubtool("mcp__tools", map[string]any{
		"name":       "sub_a",
		"parameters": map[string]any{"type": "object"},
	}, session.nsReverse)
	if !ok {
		t.Fatalf("flattenNamespaceSubtool 失败")
	}
	fn, _ := asMap(flat["function"])
	flatName, _ := asString(fn["name"])
	if flatName != "mcp__tools__sub_a" {
		t.Fatalf("flat name = %q", flatName)
	}

	resp := mustObject(t, `{
		"id": "chatcmpl-1",
		"choices": [{
			"index": 0,
			"message": {
				"tool_calls": [{
					"id": "toolu_1", "type": "function",
					"function": {"name": "`+flatName+`", "arguments": "{\"a\":\"hello\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)
	out := session.toResponsesWithNS(resp, "gpt-5")
	item := findItem(t, out["output"], "function_call")
	if item == nil {
		t.Fatalf("缺少 function_call 输出项: %#v", out["output"])
	}
	if got, _ := asString(item["name"]); got != "sub_a" {
		t.Fatalf("name = %q, want sub_a（还原 subtool）", got)
	}
	if got, _ := asString(item["namespace"]); got != "mcp__tools" {
		t.Fatalf("namespace = %q, want mcp__tools", got)
	}
	if got, _ := asString(item["arguments"]); got != `{"a":"hello"}` {
		t.Fatalf("arguments = %q", got)
	}
	if got, _ := asString(item["status"]); got != "completed" {
		t.Fatalf("status = %q", got)
	}
}

func TestToResponsesWithNSReasoningPart(t *testing.T) {
	session := newConvSession()
	resp := mustObject(t, `{
		"id": "chatcmpl-1",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "reasoning_content": "先想", "content": "再答"},
			"finish_reason": "stop"
		}]
	}`)
	out := session.toResponsesWithNS(resp, "gpt-5")
	item := findItem(t, out["output"], "message")
	if item == nil {
		t.Fatalf("缺少 message 输出项: %#v", out["output"])
	}
	parts, ok := asArray(item["content"])
	if !ok || len(parts) != 2 {
		t.Fatalf("content parts = %#v, want 2 个", item["content"])
	}
	first, _ := asMap(parts[0])
	if got, _ := asString(first["text"]); got != "先想" {
		t.Fatalf("parts[0].text = %q", got)
	}
	if got, _ := asString(first["type"]); got != "output_text" {
		t.Fatalf("parts[0].type = %q", got)
	}
	annotations, ok := asArray(first["annotations"])
	if !ok || len(annotations) != 1 {
		t.Fatalf("reasoning part 需要 annotations:[{type:reasoning}]，得到 %#v", first["annotations"])
	}
	annotation, _ := asMap(annotations[0])
	if got, _ := asString(annotation["type"]); got != "reasoning" {
		t.Fatalf("annotation.type = %q", got)
	}
	second, _ := asMap(parts[1])
	if got, _ := asString(second["text"]); got != "再答" {
		t.Fatalf("parts[1].text = %q", got)
	}
	if got, _ := asString(item["status"]); got != "completed" {
		t.Fatalf("item.status = %q", got)
	}
}

// ==================== 门面（facade）端到端 ====================

func TestConvertNonStreamResponseFacade(t *testing.T) {
	session := newConvSession()
	body := []byte(`{
		"id": "chatcmpl-9",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "hi",
				"tool_calls": [{"id":"call_9","type":"function",
					"function":{"name":"f","arguments":"{}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 7, "completion_tokens": 3}
	}`)

	rawMessages, err := session.convertNonStreamResponse(FormatOpenAIChat, FormatAnthropic, body, "m")
	if err != nil {
		t.Fatalf("ConvertNonStreamResponse(->messages): %v", err)
	}
	messages := mustObject(t, string(rawMessages))
	if got, _ := asString(messages["stop_reason"]); got != "tool_use" {
		t.Fatalf("messages stop_reason = %q, want tool_use", got)
	}
	if types := blockTypes(t, messages["content"]); len(types) != 2 || types[1] != "tool_use" {
		t.Fatalf("messages content = %v", types)
	}

	rawResponses, err := session.convertNonStreamResponse(FormatOpenAIChat, FormatResponses, body, "m")
	if err != nil {
		t.Fatalf("ConvertNonStreamResponse(->responses): %v", err)
	}
	responses := mustObject(t, string(rawResponses))
	if got, _ := asString(responses["object"]); got != "response" {
		t.Fatalf("responses object = %q", got)
	}
	item := findItem(t, responses["output"], "function_call")
	if item == nil {
		t.Fatalf("responses 缺少 function_call: %#v", responses["output"])
	}
	if got, _ := asString(item["call_id"]); got != "call_9" {
		t.Fatalf("call_id = %q", got)
	}
	usage, _ := asMap(responses["usage"])
	if got := intField(usage, "input_tokens"); got != 7 {
		t.Fatalf("usage.input_tokens = %d", got)
	}

	// 矩阵外的组合必须报错。
	if _, err := session.convertNonStreamResponse(FormatAnthropic, FormatResponses, body, "m"); err == nil {
		t.Fatalf("messages → responses 必须返回 *UnsupportedConversion")
	} else if _, ok := err.(*UnsupportedConversion); !ok {
		t.Fatalf("错误类型 = %T, want *UnsupportedConversion", err)
	}
}
