package converter

import (
	"strings"
	"time"
)

// response_reverse.go —— **非流式响应方向的逆向映射：富协议 → Chat**。
//
//	anthropicResponseToChat  ：messages → chat
//	responsesResponseToChat  ：responses → chat
//
// 它们是 response.go 里 chat→messages / chat→responses 的逆，服务于
// 「Chat 客户端 ← 富协议渠道」，以及经 Chat 的两跳（如 messages 客户端 ← responses 渠道）。
//
// 产物必须是**合法的 Chat 响应对象**（chat.completion + choices[0].message），
// 这样它既能直接回给客户端，也能交给 replayChat 重放成 Chat SSE —— 上游对流式请求
// 返回 JSON 时的降级路径正需要这一点（见 Hop.ReplayResponseAsSSE）。

// chatCreatedFrom 取 Chat 响应需要的 created 时间戳。
// Responses 有 created_at 可搬；Anthropic 没有对应字段，退回当前时间。
func chatCreatedFrom(resp map[string]any, fallback int64) int64 {
	if v, ok := asInt(resp["created_at"]); ok && v > 0 {
		return v
	}
	return fallback
}

// chatContentValue 决定 message.content 的取值形态。
// Chat 的约定：只有工具调用、没有正文时 content 为 null（而不是空串）。
func chatContentValue(text string, hasToolCalls bool) any {
	if text == "" && hasToolCalls {
		return nil
	}
	return text
}

// anthropicResponseToChat 把 Anthropic Messages 响应转换为 Chat Completions 响应。
//
// 映射（与 openAIToAnthropicResponse 互为逆）：
//
//	text 块      → content
//	thinking 块  → reasoning_content（签名无处安放，属已知有损，见文档「有损清单」）
//	tool_use 块  → tool_calls（input 序列化回 arguments 字符串）
//	stop_reason  → finish_reason（anthropicStopToFinish）
//	usage        → anthropicUsageToChat（把缓存命中并回 prompt_tokens）
func (s *convSession) anthropicResponseToChat(resp map[string]any, model string) map[string]any {
	id := genID("chatcmpl-")
	if v, ok := asString(resp["id"]); ok && v != "" {
		id = v
	}
	if model == "" {
		model, _ = asString(resp["model"])
	}

	var text, reasoning strings.Builder
	toolCalls := []any{}
	if blocks, ok := asArray(resp["content"]); ok {
		for _, raw := range blocks {
			block, ok := asMap(raw)
			if !ok {
				continue
			}
			switch t, _ := asString(block["type"]); t {
			case "text":
				if v, ok := asString(block["text"]); ok {
					text.WriteString(v)
				}
			case "thinking":
				if v, ok := asString(block["thinking"]); ok {
					reasoning.WriteString(v)
				}
			case "redacted_thinking":
				s.noteEdge(FormatAnthropic, FormatOpenAIChat,
					"丢弃 redacted_thinking 块：Chat 没有承载加密推理的位置")
			case "tool_use":
				callID, _ := asString(block["id"])
				if callID == "" {
					callID = genID("call_")
				}
				name, _ := asString(block["name"])
				toolCalls = append(toolCalls, map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": jsonString(block["input"]),
					},
				})
			default:
				s.noteEdge(FormatAnthropic, FormatOpenAIChat, "丢弃未识别的内容块类型 %q", t)
			}
		}
	}

	stopReason, _ := asString(resp["stop_reason"])
	finishReason := anthropicStopToFinish(stopReason)
	// 有工具调用却给出 stop 时按 Chat 的约定修正：Chat 客户端据此判断要不要执行工具。
	if len(toolCalls) > 0 && finishReason == "stop" {
		finishReason = "tool_calls"
	}

	message := map[string]any{
		"role":    "assistant",
		"content": chatContentValue(text.String(), len(toolCalls) > 0),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": chatCreatedFrom(resp, time.Now().Unix()),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if raw, present := resp["usage"]; present {
		usage, _ := asMap(raw)
		out["usage"] = anthropicUsageToChat(usage)
	}
	return out
}

// responsesResponseToChat 把 OpenAI Responses 响应转换为 Chat Completions 响应。
//
// 映射（与 toResponsesWithNS 互为逆）：
//
//	message 项（output_text part） → content
//	  其中 annotations 带 {"type":"reasoning"} 的 part → reasoning_content
//	  （这正是 toResponsesWithNS 标记推理内容的约定，两个方向必须一致）
//	reasoning 项（summary/content）→ reasoning_content
//	function_call 项               → tool_calls
//	custom_tool_call 项            → tool_calls（参数包回 {"content": ...} 外壳）
//	status                         → finish_reason（有工具调用时固定 tool_calls）
//	usage                          → responsesUsageToChat
func (s *convSession) responsesResponseToChat(resp map[string]any, model string) map[string]any {
	id := genID("chatcmpl-")
	if v, ok := asString(resp["id"]); ok && v != "" {
		id = v
	}
	if model == "" {
		model, _ = asString(resp["model"])
	}

	var text, reasoning strings.Builder
	toolCalls := []any{}

	if items, ok := asArray(resp["output"]); ok {
		for _, raw := range items {
			item, ok := asMap(raw)
			if !ok {
				continue
			}
			switch t, _ := asString(item["type"]); t {
			case "message":
				parts, _ := asArray(item["content"])
				for _, praw := range parts {
					part, ok := asMap(praw)
					if !ok {
						continue
					}
					ptype, _ := asString(part["type"])
					partText, _ := asString(part["text"])
					switch ptype {
					case "output_text", "input_text", "text":
						if partHasReasoningAnnotation(part) {
							reasoning.WriteString(partText)
						} else {
							text.WriteString(partText)
						}
					case "refusal":
						s.noteEdge(FormatResponses, FormatOpenAIChat,
							"丢弃 message 的 refusal part：Chat 无对应字段")
					default:
						s.noteEdge(FormatResponses, FormatOpenAIChat,
							"丢弃 message 的非文本 part %q", ptype)
					}
				}
			case "reasoning":
				if v := reasoningItemText(item); v != "" {
					reasoning.WriteString(v)
				}
			case "function_call":
				callID, _ := asString(item["call_id"])
				if callID == "" {
					callID, _ = asString(item["id"])
				}
				if callID == "" {
					callID = genID("call_")
				}
				name, _ := asString(item["name"])
				args, _ := asString(item["arguments"])
				if args == "" {
					args = "{}"
				}
				toolCalls = append(toolCalls, newChatToolCall(callID, name, args))
			case "custom_tool_call":
				callID, _ := asString(item["call_id"])
				if callID == "" {
					callID, _ = asString(item["id"])
				}
				if callID == "" {
					callID = genID("call_")
				}
				name, _ := asString(item["name"])
				toolCalls = append(toolCalls, newChatToolCall(callID, name, wrapCustomToolArguments(item["input"])))
				s.noteEdge(FormatResponses, FormatOpenAIChat,
					"custom_tool_call %q 按 function 工具承载（Chat 无 custom 工具形态）", name)
			case "function_call_output", "custom_tool_call_output":
				s.noteEdge(FormatResponses, FormatOpenAIChat,
					"忽略响应里的 %q 项：Chat 响应体不承载工具结果", t)
			default:
				s.noteEdge(FormatResponses, FormatOpenAIChat, "丢弃未识别的输出项类型 %q", t)
			}
		}
	}

	status, _ := asString(resp["status"])
	finishReason := statusToFinishReason(status)
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	message := map[string]any{
		"role":    "assistant",
		"content": chatContentValue(text.String(), len(toolCalls) > 0),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": chatCreatedFrom(resp, time.Now().Unix()),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if raw, present := resp["usage"]; present {
		usage, _ := asMap(raw)
		out["usage"] = responsesUsageToChat(usage)
	}
	return out
}

// newChatToolCall 组装一个 Chat 形态的工具调用。
func newChatToolCall(callID, name, arguments string) map[string]any {
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
}

// partHasReasoningAnnotation 判断 Responses 的文本 part 是否被标记为推理内容。
// 标记形式与 toResponsesWithNS 写出的完全一致：annotations 里含 {"type":"reasoning"}。
func partHasReasoningAnnotation(part map[string]any) bool {
	annotations, ok := asArray(part["annotations"])
	if !ok {
		return false
	}
	for _, raw := range annotations {
		annotation, ok := asMap(raw)
		if !ok {
			continue
		}
		if t, _ := asString(annotation["type"]); t == "reasoning" {
			return true
		}
	}
	return false
}

// wrapCustomToolArguments 是 unwrapCustomToolArguments 的逆：把 custom 工具的载荷
// 包回 Chat 侧的 {"content": ...} 函数外壳。载荷为空时给出空对象，保证 arguments 是合法 JSON。
func wrapCustomToolArguments(input any) string {
	if input == nil {
		return "{}"
	}
	if s, ok := asString(input); ok {
		if s == "" {
			return "{}"
		}
		return jsonString(map[string]any{"content": s})
	}
	return jsonString(map[string]any{"content": jsonString(input)})
}
