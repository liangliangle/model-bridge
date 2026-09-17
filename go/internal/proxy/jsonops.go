package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"modelbridge/internal/config"
	"modelbridge/internal/converter"
)

// 本文件是 Rust `proxy/executor.rs` 中「请求体小改造」函数的逐一对照移植：
// 替换 model、补/去 stream、剥离 thinking 块、Anthropic 体净化、强制 effort。

// decodeObject 把 JSON 解析为对象；数字保留原样（json.Number），失败返回 nil。
func decodeObject(raw []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return obj
}

// encodeObject 序列化对象；失败时返回原字节。
func encodeObject(obj map[string]any, fallback []byte) []byte {
	out, err := json.Marshal(obj)
	if err != nil {
		return fallback
	}
	return out
}

// ReplaceModelInJSON 替换顶层 model 字段，保留其余字段。对应 Rust `replace_model_in_json_str`。
func ReplaceModelInJSON(raw []byte, newModel string) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return raw
	}
	encoded, err := json.Marshal(newModel)
	if err != nil {
		return raw
	}
	fields["model"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// SetStreamInJSON 设置或移除顶层 stream 字段（转换路径专用）。
// 对应 Rust `set_stream_in_json_str`：非流式时是「删除」而不是置 false。
func SetStreamInJSON(raw []byte, isStream bool) []byte {
	obj := decodeObject(raw)
	if obj == nil {
		return raw
	}
	if isStream {
		obj["stream"] = true
	} else {
		delete(obj, "stream")
	}
	return encodeObject(obj, raw)
}

// StripThinkingBlocks 从所有消息中移除 thinking / redacted_thinking 内容块。
// 对应 Rust `strip_thinking_blocks`。
func StripThinkingBlocks(raw []byte) []byte {
	obj := decodeObject(raw)
	if obj == nil {
		return raw
	}
	messages, ok := obj["messages"].([]any)
	if !ok {
		return raw
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		kept := blocks[:0]
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				kept = append(kept, b)
				continue
			}
			t, _ := block["type"].(string)
			if t == "thinking" || t == "redacted_thinking" {
				continue
			}
			kept = append(kept, b)
		}
		msg["content"] = kept
	}
	return encodeObject(obj, raw)
}

// SanitizeAnthropicBody 为 Anthropic 渠道移除 temperature 参数。
// 对应 Rust `sanitize_anthropic_body_str`。
func SanitizeAnthropicBody(raw []byte) []byte {
	obj := decodeObject(raw)
	if obj == nil {
		return raw
	}
	delete(obj, "temperature")
	return encodeObject(obj, raw)
}

// ForceEffortInJSON 强制覆盖 output_config.effort；output_config 不存在时创建。
// 对应 Rust `force_effort_in_json_str`。
func ForceEffortInJSON(raw []byte, effort string) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return raw
	}
	inner := map[string]json.RawMessage{}
	if existing, ok := fields["output_config"]; ok {
		_ = json.Unmarshal(existing, &inner)
		if inner == nil {
			inner = map[string]json.RawMessage{}
		}
	}
	encoded, err := json.Marshal(effort)
	if err != nil {
		return raw
	}
	inner["effort"] = encoded
	merged, err := json.Marshal(inner)
	if err != nil {
		return raw
	}
	fields["output_config"] = merged
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// InjectAnthropicCache 在请求体上注入 prompt caching 断点（仅在无任何 cache_control 时）。
// 对应 Rust `inject_anthropic_cache_str`，实现委托给 converter 包。
func InjectAnthropicCache(raw []byte) []byte {
	out, err := converter.InjectAnthropicCacheBreakpoints(raw)
	if err != nil {
		return raw
	}
	return out
}

// PrettyJSON 尝试格式化 JSON，失败则原样返回。对应 Rust `pretty_json`。
func PrettyJSON(raw string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return raw
	}
	return buf.String()
}

// HeadersToJSON 把请求头序列化为 JSON 对象字符串。对应 Rust `headers_to_json`。
func HeadersToJSON(h http.Header) string {
	m := map[string]string{}
	for k, vs := range h {
		if len(vs) == 0 {
			continue
		}
		m[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// hopByHopHeaders 是透传时需要跳过的头（含认证头，认证头由调用方单独设置）。
// 与 Rust `collect_forwarded_headers` 的 skip 列表一致。
var hopByHopHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
	"authorization":     true,
	"x-api-key":         true,
	"accept-encoding":   true,
}

// CollectForwardedHeaders 收集调用方 headers 与渠道 custom_headers。
// 覆盖语义：后写覆盖前写（custom_headers 优先）。对应 Rust `collect_forwarded_headers`。
func CollectForwardedHeaders(dst map[string]string, caller http.Header, ch *config.ChannelConfig) {
	for key, values := range caller {
		name := strings.ToLower(key)
		if hopByHopHeaders[name] || len(values) == 0 {
			continue
		}
		dst[name] = values[0]
	}
	for key, value := range ch.CustomHeaders {
		dst[strings.ToLower(key)] = value
	}
}

// SortedHeaderKeys 返回排序后的 header 名，用于稳定地构造请求与日志输出。
func SortedHeaderKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
