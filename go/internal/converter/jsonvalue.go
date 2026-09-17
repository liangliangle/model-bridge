package converter

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// 本文件是 JSON 取值/取值辅助函数。解码统一开启 UseNumber，保证数字原样搬运
// （与 Rust serde_json::Value 保留原始数值、ocgo 用 json.RawMessage 透传的意图一致）。

var (
	errEmptyBody = errors.New("converter: request body is empty")
	errNotObject = errors.New("converter: request body must be a JSON object")
)

// decodeObject 把字节解析为 JSON 对象。非法 JSON 或顶层非对象都返回错误。
func decodeObject(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errEmptyBody
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errNotObject
	}
	return obj, nil
}

// marshalJSON 序列化为 JSON 字节；map 输出键按字典序，与 Go 默认一致。
func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// decodeAny 解析任意 JSON 值；失败时返回 (nil, false)。
func decodeAny(raw string) (any, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, false
	}
	return v, true
}

// asMap 断言 JSON 对象。
func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// asArray 断言 JSON 数组。
func asArray(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

// asString 断言 JSON 字符串。
func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// asBool 断言 JSON 布尔。
func asBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// asInt 取整数（兼容 json.Number / float64 / int）。非数值或缺失返回 (0, false)。
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}

// asFloat 取浮点数（兼容 json.Number / int）。
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// stripNulls 删除 map 里的 null 字段（对应 Rust common.rs::strip_nulls）。
// 仅作用于传入的那一层，与 Rust 的调用方式一致（只清洗顶层出站对象）。
func stripNulls(obj map[string]any) {
	for k, v := range obj {
		if v == nil {
			delete(obj, k)
		}
	}
}
