package audit

// 本文件移植 Rust src-tauri/src/audit/db.rs 中把流式 SSE 响应组装成完整响应对象的逻辑。
// 审计界面展示的应是拼合后的完整响应（而非逐条 SSE 事件），前端与 macOS 客户端按这里的
// JSON 结构读取，因此字段名与形状必须与 Rust 侧保持一致。

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"modelbridge/internal/sse"
)

// ---------- serde_json::Value 兼容的小工具 ----------

// parseJSONObject 解析一行 data 为 JSON 对象；使用 UseNumber 保留整数的精确表示
// （对应 serde_json 的 u64/i64 语义）。非对象或解析失败返回 ok=false。
func parseJSONObject(data string) (map[string]any, bool) {
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	return obj, ok
}

// asU64 等价于 serde_json::Value::as_u64：仅接受非负整数，浮点返回 false。
func asU64(v any) (uint64, bool) {
	switch n := v.(type) {
	case json.Number:
		if u, err := strconv.ParseUint(n.String(), 10, 64); err == nil {
			return u, true
		}
		return 0, false
	case uint64:
		return n, true
	case int64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case int:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case float64:
		if n < 0 || n != float64(uint64(n)) {
			return 0, false
		}
		return uint64(n), true
	default:
		return 0, false
	}
}

// usageU64 按 keys 顺序取第一个「存在」的键，对应
// `usage.get(k).or_else(...).and_then(as_u64).unwrap_or(old)`：键存在但取不到无符号整数时
// 也视为失败，调用方应保留旧值。
func usageU64(usage map[string]any, keys ...string) (uint64, bool) {
	for _, k := range keys {
		if v, ok := usage[k]; ok {
			return asU64(v)
		}
	}
	return 0, false
}

// prettyJSON 等价于 serde_json::to_string_pretty：2 空格缩进、不转义 HTML、无结尾换行。
func prettyJSON(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// indentRaw 把原始 JSON 片段格式化为 serde_json::to_string_pretty 风格。
// 直接对原始字节缩进可保留键顺序（serde_json 开启了 preserve_order）。
func indentRaw(raw []byte) (string, bool) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return "", false
	}
	return buf.String(), true
}

// ---------- 组装后的响应对象形状（字段顺序与 Rust json! 一致） ----------

type anthropicAssembledContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicAssembledUsage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

type anthropicAssembledResponse struct {
	ID         string                      `json:"id"`
	Type       string                      `json:"type"`
	Role       string                      `json:"role"`
	Model      string                      `json:"model"`
	Content    []anthropicAssembledContent `json:"content"`
	StopReason string                      `json:"stop_reason"`
	Usage      anthropicAssembledUsage     `json:"usage"`
}

type chatAssembledMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatAssembledChoice struct {
	Index        int                  `json:"index"`
	Message      chatAssembledMessage `json:"message"`
	FinishReason string               `json:"finish_reason"`
}

type chatAssembledUsage struct {
	PromptTokens     uint64 `json:"prompt_tokens"`
	CompletionTokens uint64 `json:"completion_tokens"`
	TotalTokens      uint64 `json:"total_tokens"`
}

type chatAssembledResponse struct {
	ID      string                `json:"id"`
	Object  string                `json:"object"`
	Model   string                `json:"model"`
	Choices []chatAssembledChoice `json:"choices"`
	Usage   chatAssembledUsage    `json:"usage"`
}

type responsesAssembledContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type responsesAssembledOutput struct {
	Type    string                      `json:"type"`
	Role    string                      `json:"role"`
	Content []responsesAssembledContent `json:"content"`
}

type responsesAssembledResponse struct {
	ID     string                     `json:"id"`
	Object string                     `json:"object"`
	Model  string                     `json:"model"`
	Status string                     `json:"status"`
	Output []responsesAssembledOutput `json:"output"`
}

// ---------- 主体逻辑 ----------

// assembleStreamingResponse 把一段 SSE 原始流组装为单个完整响应对象。
//
// 对应 Rust assemble_streaming_response（db.rs:1258）：优先识别 OpenAI Responses
// （事件 type 形如 response.*），其次 Anthropic（message_start / content_block_delta /
// message_delta / error），最后按 OpenAI Chat 处理；无法识别时原样返回。
func assembleStreamingResponse(raw string) string {
	// 优先识别 OpenAI Responses 格式（事件 type 形如 response.*）
	if assembled, ok := assembleResponsesStream(raw); ok {
		return assembled
	}

	var id, model string
	var content strings.Builder
	var inputTokens, outputTokens uint64
	var finishReason string
	isAnthropic := false

	for _, line := range strings.Split(raw, "\n") {
		// 解析 SSE data 行
		data, ok := sse.Data(line)
		if !ok {
			continue
		}
		if data == "[DONE]" || data == "" {
			continue
		}
		v, ok := parseJSONObject(data)
		if !ok {
			continue
		}

		// ---- Anthropic 格式 ----
		if eventType, ok := v["type"].(string); ok {
			switch eventType {
			case "message_start":
				isAnthropic = true
				if msg, ok := v["message"].(map[string]any); ok {
					id, _ = msg["id"].(string)
					model, _ = msg["model"].(string)
					if usage, ok := msg["usage"].(map[string]any); ok {
						if n, ok := asU64(usage["input_tokens"]); ok {
							inputTokens = n
						} else {
							inputTokens = 0
						}
					}
				}
			case "content_block_delta":
				if delta, ok := v["delta"].(map[string]any); ok {
					if text, ok := delta["text"].(string); ok {
						content.WriteString(text)
					}
				}
			case "message_delta":
				if delta, ok := v["delta"].(map[string]any); ok {
					if sr, ok := delta["stop_reason"].(string); ok {
						finishReason = sr
					}
				}
				if usage, ok := v["usage"].(map[string]any); ok {
					if n, ok := asU64(usage["output_tokens"]); ok {
						outputTokens = n
					}
				}
			case "error":
				// 错误事件直接返回原始 JSON（pretty 打印）
				if s, ok := indentRaw([]byte(data)); ok {
					return s
				}
				return raw
			}
			continue
		}

		// ---- OpenAI 格式 ----
		if choices, ok := v["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if delta, ok := choice["delta"].(map[string]any); ok {
					if text, ok := delta["content"].(string); ok {
						content.WriteString(text)
					}
				}
				if fr, ok := choice["finish_reason"].(string); ok {
					finishReason = fr
				}
			}
		}
		if id == "" {
			if s, ok := v["id"].(string); ok {
				id = s
			}
		}
		if model == "" {
			if s, ok := v["model"].(string); ok {
				model = s
			}
		}
		if usage, ok := v["usage"].(map[string]any); ok {
			if n, ok := usageU64(usage, "prompt_tokens", "input_tokens"); ok {
				inputTokens = n
			}
			if n, ok := usageU64(usage, "completion_tokens", "output_tokens"); ok {
				outputTokens = n
			}
		}
	}

	// 没有解析到任何内容，返回原始数据
	if content.Len() == 0 && id == "" {
		return raw
	}

	// 组装完整响应
	if isAnthropic {
		stopReason := finishReason
		if stopReason == "" {
			stopReason = "end_turn"
		}
		assembled := anthropicAssembledResponse{
			ID:         id,
			Type:       "message",
			Role:       "assistant",
			Model:      model,
			Content:    []anthropicAssembledContent{{Type: "text", Text: content.String()}},
			StopReason: stopReason,
			Usage: anthropicAssembledUsage{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}
		if s, ok := prettyJSON(assembled); ok {
			return s
		}
		return raw
	}

	stopReason := finishReason
	if stopReason == "" {
		stopReason = "stop"
	}
	assembled := chatAssembledResponse{
		ID:     id,
		Object: "chat.completion",
		Model:  model,
		Choices: []chatAssembledChoice{{
			Index:        0,
			Message:      chatAssembledMessage{Role: "assistant", Content: content.String()},
			FinishReason: stopReason,
		}},
		Usage: chatAssembledUsage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
	}
	if s, ok := prettyJSON(assembled); ok {
		return s
	}
	return raw
}

// assembleResponsesStream 拼合 OpenAI Responses 格式的 SSE 流为最终完整 response 对象。
//
// 对应 Rust assemble_responses_stream（db.rs:1402）：返回 ok=false 表示流中不含
// Responses 事件（非 Responses 格式，交回通用逻辑处理）。
func assembleResponsesStream(raw string) (string, bool) {
	sawResponsesEvent := false
	var finalResponse json.RawMessage
	hasFinalResponse := false
	var id, model string
	var text strings.Builder

	for _, line := range strings.Split(raw, "\n") {
		data, ok := sse.Data(line)
		if !ok {
			continue
		}
		if data == "[DONE]" || data == "" {
			continue
		}
		// 用 RawMessage 保留内嵌 response 对象的原始键顺序
		var ev map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		typeRaw, ok := ev["type"]
		if !ok {
			continue
		}
		var eventType string
		if err := json.Unmarshal(typeRaw, &eventType); err != nil {
			continue
		}
		if !strings.HasPrefix(eventType, "response.") {
			continue
		}
		sawResponsesEvent = true

		switch eventType {
		// 终态事件内嵌完整 response 对象，直接采用（最完整）
		case "response.completed", "response.incomplete", "response.failed":
			if resp, ok := ev["response"]; ok {
				finalResponse = resp
				hasFinalResponse = true
			}
		case "response.output_text.delta":
			if deltaRaw, ok := ev["delta"]; ok {
				var delta string
				if err := json.Unmarshal(deltaRaw, &delta); err == nil {
					text.WriteString(delta)
				}
			}
		default:
			// 从任意事件里尽力补齐 id/model
			if resp, ok := ev["response"]; ok {
				var meta struct {
					ID    string `json:"id"`
					Model string `json:"model"`
				}
				_ = json.Unmarshal(resp, &meta)
				if id == "" {
					id = meta.ID
				}
				if model == "" {
					model = meta.Model
				}
			}
		}
	}

	if !sawResponsesEvent {
		return "", false
	}

	// 优先返回终态事件里的完整 response 对象
	if hasFinalResponse {
		if s, ok := indentRaw(finalResponse); ok {
			return s, true
		}
		return raw, true
	}

	// 无终态事件：用聚合的文本构造一个最小完整 response 对象
	assembled := responsesAssembledResponse{
		ID:     id,
		Object: "response",
		Model:  model,
		Status: "completed",
		Output: []responsesAssembledOutput{{
			Type: "message",
			Role: "assistant",
			Content: []responsesAssembledContent{{
				Type:        "output_text",
				Text:        text.String(),
				Annotations: []any{},
			}},
		}},
	}
	if s, ok := prettyJSON(assembled); ok {
		return s, true
	}
	return raw, true
}
