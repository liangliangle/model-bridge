package proxy

import (
	"encoding/json"
	"strings"

	"modelbridge/internal/sse"
)

// 流式错误检测：在把首个 chunk 转发给客户端之前，判断上游是否已经报错。
// 这是 Rust `proxy/executor.rs` 中 `detect_sse_error_in_prefix` 与
// `sse_has_content_output` 的逐一对照移植。

// DetectSSEErrorInPrefix 检测 SSE 字节前缀中的错误事件。
// 返回 (错误信息, true) 表示检测到错误。
//
// 支持五种形态（前三种对应 Rust 原实现，后两种在矩阵放开后才会从富协议渠道出现）：
//   - SSE `event:error` 行（DashScope/Qwen 等）
//   - OpenAI Chat `{"error":{...}}` data 事件
//   - Anthropic `{"type":"error","error":{"message":...}}` data 事件
//   - OpenAI Responses `{"type":"error","message":...}`（信息在**顶层**，不在 error 下）
//   - OpenAI Responses `{"type":"response.failed","response":{"error":{...}}}`
func DetectSSEErrorInPrefix(raw []byte) (string, bool) {
	text := string(raw)
	hasEventError := false

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.EqualFold(trimmed, "event:error") || strings.EqualFold(trimmed, "event: error") {
			hasEventError = true
			continue
		}

		data, ok := sse.Data(trimmed)
		if !ok || data == "" || data == "[DONE]" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}

		// Chat：错误信息在顶层 error 对象里。
		if errObj, ok := payload["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok {
				return msg, true
			}
			return "Unknown stream error", true
		}

		switch t, _ := payload["type"].(string); t {
		case "error":
			// Anthropic 把信息放在 error.message；Responses 放在顶层 message。
			if msg, ok := nestedString(payload, "error", "message"); ok {
				return msg, true
			}
			if msg, ok := payload["message"].(string); ok {
				return msg, true
			}
			return "Unknown stream error", true
		case "response.failed":
			// Responses 的失败终局事件：response.error.message（或顶层 message）。
			if msg, ok := nestedString(payload, "response", "error", "message"); ok {
				return msg, true
			}
			if msg, ok := nestedString(payload, "response", "error", "code"); ok {
				return "stream failed: " + msg, true
			}
			return "Unknown stream error", true
		}
	}

	// 出现了 event:error 行但 data 不是标准 JSON：仍视为错误，用原始文本当信息。
	if hasEventError {
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			data, ok := sse.Data(trimmed)
			if !ok || data == "" || data == "[DONE]" {
				continue
			}
			return "SSE error event: " + data, true
		}
		return "SSE error event detected", true
	}

	return "", false
}

// nestedString 依次下钻 keys 取字符串；任一层缺失或类型不符时返回 false。
func nestedString(obj map[string]any, keys ...string) (string, bool) {
	var cur any = obj
	for _, key := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[key]
		if !ok {
			return "", false
		}
	}
	s, ok := cur.(string)
	return s, ok && s != ""
}

// SSEHasContentOutput 判断 SSE 字节中是否已包含**已转发给客户端的实质内容**。
//
// 它只服务于一个决策：首个 chunk 里出现错误事件时，还能不能换渠道重试。
// 只要客户端已经收到过可以看见/会写进上下文的内容（正文、推理、工具调用参数），
// 就不能再换渠道（否则客户端会看到两段拼接的输出）；反过来，若上游只发了
// 生命周期事件（message_start / response.created / content_block_start），
// 换渠道是安全的。
//
// 三种协议的判定（与 converter 的三种流式方向一一对应）：
//   - Chat：choices[].delta 的 content / reasoning_content / tool_calls
//   - Anthropic：content_block_delta（text_delta / thinking_delta / input_json_delta）
//   - Responses：response.output_text.delta / response.function_call_arguments.delta /
//     response.reasoning_summary_text.delta / response.reasoning_text.delta
func SSEHasContentOutput(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		data, ok := sse.Data(trimmed)
		if !ok || data == "" || data == "[DONE]" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}

		if chatChunkHasContent(payload) {
			return true
		}
		if t, _ := payload["type"].(string); responsesDeltaHasContent(t, payload) {
			return true
		}
	}

	return false
}

// chatChunkHasContent 判断一个 Chat chunk 是否携带实质内容增量。
func chatChunkHasContent(payload map[string]any) bool {
	choices, ok := payload["choices"].([]any)
	if !ok {
		return false
	}
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if s, ok := delta["content"].(string); ok && s != "" {
			return true
		}
		// 推理内容与工具调用同样是已下发的内容，不能再看成「还没输出」。
		if s, ok := delta["reasoning_content"].(string); ok && s != "" {
			return true
		}
		if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
			return true
		}
	}
	return false
}

// responsesDeltaHasContent 判断一个 Responses 事件是否为携带内容的增量事件。
func responsesDeltaHasContent(eventType string, payload map[string]any) bool {
	switch eventType {
	// Anthropic 的三种 delta 都算内容（text / thinking / input_json）。
	case "content_block_delta":
		return true
	case "response.output_text.delta",
		"response.function_call_arguments.delta",
		"response.reasoning_summary_text.delta",
		"response.reasoning_text.delta":
		if s, ok := payload["delta"].(string); ok && s != "" {
			return true
		}
		return false
	}
	return false
}
