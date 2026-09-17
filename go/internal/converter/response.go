package converter

// 本文件实现**非流式响应方向**的两条单跳转换：
//
//	chat → messages ：openAIToAnthropicResponse
//	chat → responses：(*Session).toResponsesWithNS
//
// 纯映射机制逐行对照 ocgo cmd/ocgo/main.go（writeAnthropicResponse line 2607、
// writeResponsesResponse line 3048），协议契约以 Rust converter/response.rs 为准：
//
//  1. 非流式 Anthropic 形态必须保留工具调用（`tool_use` 内容块 + `stop_reason: tool_use`），
//     不沿用 ocgo 丢弃 tool_calls 的写法（见 converter.go 顶部「两者冲突时以 Rust 契约为准」第 2 条）；
//  2. Responses 形态必须是完整的 response 对象（item 生命周期 + usage）；
//  3. custom / namespace 工具用 Session.nsReverse 还原（custom_tool_call 输出项 + 参数解壳）。

// firstChatChoice 取 Chat 响应的 choices[0]；缺失、非数组或元素非对象时返回 nil。
// 对应 Rust response.rs 里 `resp.get("choices").and_then(as_array).and_then(first)` 的取值链。
func firstChatChoice(resp map[string]any) map[string]any {
	choices, ok := asArray(resp["choices"])
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, _ := asMap(choices[0])
	return choice
}

// anthropicToolInput 把 Chat 的 function.arguments 字符串解析成 Anthropic tool_use 的 input 对象。
// 对应 Rust response.rs::openai_to_anthropic_response 里的
// `serde_json::from_str(args).unwrap_or_else(|_| json!({}))`：非法 JSON 退化为空对象。
func anthropicToolInput(arguments string) any {
	if v, ok := decodeAny(arguments); ok {
		return v
	}
	return map[string]any{}
}

// openAIToAnthropicResponse 把 Chat 非流式响应转换为 Anthropic Messages 响应。
//
// 对应 Rust response.rs::openai_to_anthropic_response（line 231），
// 同时对照 ocgo main.go::writeAnthropicResponse（line 2607）。
// 与 ocgo 的差异（契约要求）：ocgo 只搬运 text 且 stop_reason 写死 end_turn；
// 这里保留 reasoning_content→thinking、content→text、tool_calls→tool_use 三类内容块，
// 并由 finish_reason 推导 stop_reason（finishToAnthropicStop）。
func openAIToAnthropicResponse(resp map[string]any, model string) map[string]any {
	id := genID("msg_")
	if s, ok := asString(resp["id"]); ok {
		id = s
	}

	choice := firstChatChoice(resp)
	var message map[string]any
	finishReason := ""
	if choice != nil {
		message, _ = asMap(choice["message"])
		if fr, ok := asString(choice["finish_reason"]); ok {
			finishReason = fr
		}
	}

	blocks := []any{}
	if message != nil {
		// reasoning_content → thinking（放最前）
		if reasoning, ok := asString(message["reasoning_content"]); ok && reasoning != "" {
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reasoning})
		}
		// content → text
		if text := extractText(message["content"]); text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		// tool_calls → tool_use（ocgo 在此处丢数据，契约要求保留）
		if toolCalls, ok := asArray(message["tool_calls"]); ok {
			for _, raw := range toolCalls {
				call, ok := asMap(raw)
				if !ok {
					continue
				}
				callID, _ := asString(call["id"])
				if callID == "" {
					// 与 ocgo main.go::parseAnthropicResponse 的 `call_%d` 兜底等价：
					// 缺 id 时补一个生成 id（Rust 会把空 id 原样写出，客户端无法回填）。
					callID = genID("call_")
				}
				name := ""
				arguments := "{}"
				if fn, ok := asMap(call["function"]); ok {
					name, _ = asString(fn["name"])
					if args, ok := asString(fn["arguments"]); ok {
						arguments = args
					}
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    callID,
					"name":  name,
					"input": anthropicToolInput(arguments),
				})
			}
		}
	}

	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   finishToAnthropicStop(finishReason),
		"stop_sequence": nil,
	}
	// usage：Rust 仅在上游带 usage 键时写入；此处保持一致（键存在但为 null 视为零值）。
	if raw, present := resp["usage"]; present {
		usage, _ := asMap(raw)
		out["usage"] = chatUsageToAnthropic(usage)
	}
	return out
}

// toResponsesWithNS 把 Chat 非流式响应转换为 Responses 响应，并用 s.nsReverse 还原
// namespace / custom 工具名。
//
// 对应 Rust response.rs::to_responses_with_ns（line 26）与 chat_resp_to_responses_with_ns（line 44），
// 同时对照 ocgo main.go::writeResponsesResponse（line 3048）的工具项构造。
// item 生命周期（message / function_call / custom_tool_call）以 Rust 为准。
func (s *Session) toResponsesWithNS(resp map[string]any, model string) map[string]any {
	choice := firstChatChoice(resp)
	var message map[string]any
	finishReason := ""
	if choice != nil {
		message, _ = asMap(choice["message"])
		if fr, ok := asString(choice["finish_reason"]); ok {
			finishReason = fr
		}
	}

	id := genID("resp_")
	if v, ok := asString(resp["id"]); ok {
		id = v
	}
	msgID := genID("msg_")
	status := finishReasonToStatus(finishReason)

	// message item：reasoning_content 与正文各是一个 output_text part，
	// reasoning 用 annotations:[{type:"reasoning"}] 标记（与 Rust 一致，不造独立 reasoning 项）。
	parts := []any{}
	if message != nil {
		if reasoning, ok := asString(message["reasoning_content"]); ok && reasoning != "" {
			parts = append(parts, map[string]any{
				"type":        "output_text",
				"text":        reasoning,
				"annotations": []any{map[string]any{"type": "reasoning"}},
			})
		}
		if text := extractText(message["content"]); text != "" {
			parts = append(parts, map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			})
		}
	}

	output := []any{}
	if len(parts) > 0 {
		output = append(output, map[string]any{
			"type":    "message",
			"id":      msgID,
			"status":  status,
			"role":    "assistant",
			"content": parts,
		})
	}

	if message != nil {
		if toolCalls, ok := asArray(message["tool_calls"]); ok {
			callIDs := make([]string, 0, len(toolCalls))
			for _, raw := range toolCalls {
				call, ok := asMap(raw)
				if !ok {
					continue
				}
				callID, _ := asString(call["id"])
				name := ""
				arguments := "{}"
				if fn, ok := asMap(call["function"]); ok {
					name, _ = asString(fn["name"])
					if args, ok := asString(fn["arguments"]); ok {
						arguments = args
					}
				}
				item := map[string]any{
					"type":      "function_call",
					"id":        genID("fc_"),
					"call_id":   callID,
					"name":      name,
					"arguments": arguments,
					"status":    "completed",
				}
				if entry, known := s.nsReverse[name]; known {
					if entry.namespace == CUSTOM_TOOL_NAMESPACE_MARKER {
						// custom 工具：还原成 custom_tool_call，参数解壳并去掉 arguments 键。
						item["type"] = "custom_tool_call"
						item["id"] = callID
						item["input"] = unwrapCustomToolArguments(arguments)
						delete(item, "arguments")
					} else {
						// namespace 子工具：名字还原为 subtool，namespace 单独列出。
						item["name"] = entry.subtool
						item["namespace"] = entry.namespace
					}
				}
				output = append(output, item)
				if callID != "" {
					callIDs = append(callIDs, callID)
				}
			}
			// 对照 ocgo main.go::writeResponsesResponse：把 assistant 的 reasoning_content
			// 按 tool_call id 记入包级缓存，供后续 anthropic→chat 请求回显。
			if reasoning, ok := asString(message["reasoning_content"]); ok && reasoning != "" {
				cacheReasoningContent(callIDs, reasoning)
			}
		}
	}

	out := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": 0,
		"model":      model,
		"status":     status,
		"output":     output,
	}
	// usage：契约要求 Responses 形态**恒带** usage（规范里的必填字段），
	// 上游缺 usage 时给零值对象（与流式 response.completed 的处理一致）。
	usage, _ := asMap(resp["usage"])
	converted := chatUsageToResponses(usage)
	if converted == nil {
		converted = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	out["usage"] = converted
	return out
}
