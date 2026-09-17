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
// 支持三种形态：
//   - SSE `event:error` 行（DashScope/Qwen 等）
//   - Anthropic `{"type":"error","error":{...}}` data 事件
//   - OpenAI `{"error":{...}}` data 事件
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

		if errObj, ok := payload["error"].(map[string]any); ok {
			if msg, ok := errObj["message"].(string); ok {
				return msg, true
			}
			return "Unknown stream error", true
		}

		if t, _ := payload["type"].(string); t == "error" {
			if errObj, ok := payload["error"].(map[string]any); ok {
				if msg, ok := errObj["message"].(string); ok {
					return msg, true
				}
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

// SSEHasContentOutput 判断 SSE 字节中是否已包含正常内容增量。
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

		// OpenAI：choices[].delta.content 非空
		if choices, ok := payload["choices"].([]any); ok {
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
			}
		}

		// Anthropic：content_block_delta
		if t, _ := payload["type"].(string); t == "content_block_delta" {
			return true
		}
	}

	return false
}
