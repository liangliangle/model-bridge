// MCP 工具黑名单过滤逻辑（对应 src-tauri/src/mcp/filter.rs）：
//   - tools/list 响应：从 result.tools[] 删除黑名单工具
//   - tools/call 请求：调用黑名单工具时直接返回 JSON-RPC error
//
// 与 Rust 的差别（不影响语义）：
//  1. Go 的 map 序列化会按键名排序，剥离/重建 JSON 对象后键序与上游不同；
//     JSON 对象键序无语义，客户端不受影响。
//  2. 解析时使用 json.Decoder.UseNumber()，避免大整数 id / token 计数被 float64 破坏。
package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
)

// jsonrpcVersion 所有响应的 jsonrpc 字段固定值（filter.rs:14）。
const jsonrpcVersion = "2.0"

// RejectToolCall 构造拒绝调用黑名单工具的 JSON-RPC error 响应（HTTP 200 + error body）。
// id 原样回显（可能是数字/字符串/null）（filter.rs:12-22）。
func RejectToolCall(reqID json.RawMessage, toolName string) Response {
	body := map[string]any{
		"jsonrpc": jsonrpcVersion,
		"id":      decodeRawValue(reqID),
		"error": map[string]any{
			"code":    -32602,
			"message": "Unknown tool: " + toolName,
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		encoded = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32602,"message":"Unknown tool"}}`)
	}
	return Response{
		StatusCode: 200,
		Header:     httpHeader("Content-Type", "application/json"),
		Body:       encoded,
	}
}

// FilterToolsInResponse 从 tools/list 的 JSON-RPC response 中过滤掉黑名单工具。
// 原地修改 result.tools[]，保留 nextCursor 等其它字段（filter.rs:26-36）。
func FilterToolsInResponse(rpc map[string]any, blocked []string) {
	if len(blocked) == 0 {
		return
	}
	result, ok := rpc["result"].(map[string]any)
	if !ok {
		return
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(tools))
	for _, t := range tools {
		name := ""
		if obj, ok := t.(map[string]any); ok {
			if s, ok := obj["name"].(string); ok {
				name = s
			}
		}
		if IsToolBlocked(blocked, name) {
			continue
		}
		kept = append(kept, t)
	}
	result["tools"] = kept
}

// IsToolBlocked 黑名单按精确名字匹配（filter.rs:31-34 / mod.rs:76）。
func IsToolBlocked(blocked []string, name string) bool {
	for _, b := range blocked {
		if b == name {
			return true
		}
	}
	return false
}

// JSONRPCRequest 从客户端请求体解析出的最小信息（mcp/mod.rs:63-70 的子集）：
// method / id / params.name。解析失败时 OK 为 false，调用方应按透传处理（method 为空）。
type JSONRPCRequest struct {
	Method   string
	ReqID    json.RawMessage
	ToolName string
	OK       bool
}

// ParseJSONRPCRequest 解析客户端的 JSON-RPC 请求体（mcp/mod.rs:63-70）。
func ParseJSONRPCRequest(body string) JSONRPCRequest {
	out := JSONRPCRequest{ReqID: json.RawMessage("null")}
	parsed, ok := decodeJSONObject(body)
	if !ok {
		return out
	}
	out.OK = true
	if m, ok := parsed["method"].(string); ok {
		out.Method = m
	}
	if id, ok := parsed["id"]; ok {
		if raw, err := json.Marshal(id); err == nil {
			out.ReqID = raw
		}
	}
	if params, ok := parsed["params"].(map[string]any); ok {
		if name, ok := params["name"].(string); ok {
			out.ToolName = name
		}
	}
	return out
}

// ExtractJSONRPCResponse 从上游响应文本中提取目标 JSON-RPC response
// （filter.rs:38-63）。兼容两种上游响应形态：
//  1. application/json：整个 body 就是一个 JSON-RPC response
//  2. text/event-stream：SSE 帧，response 嵌在某个 event 的 data: 行里
//
// 优先返回 id 匹配 reqID 且含 "result" 或 "error" 的那条；
// 解析失败则返回 ok=false（调用方应回退到原样透传）。
func ExtractJSONRPCResponse(body string, reqID json.RawMessage) (map[string]any, bool) {
	want := decodeRawValue(reqID)

	// 先尝试整体作为单个 JSON 解析（application/json 情况）
	if v, ok := decodeJSONObject(strings.TrimSpace(body)); ok {
		if isTargetResponse(v, want) {
			return v, true
		}
	}

	// 回退：按 SSE 帧解析，逐个 event 的 data 负载尝试
	for _, payload := range iterSSEDataPayloads(body) {
		v, ok := decodeJSONObject(payload)
		if !ok {
			continue
		}
		if isTargetResponse(v, want) {
			return v, true
		}
	}
	return nil, false
}

// isTargetResponse 判断一个 JSON 值是否是我们要找的 JSON-RPC response：
// 含 result 或 error，且 id 与请求 id 相等（filter.rs:65-77）。
func isTargetResponse(v map[string]any, want any) bool {
	if _, ok := v["result"]; !ok {
		if _, ok := v["error"]; !ok {
			return false
		}
	}
	if id, ok := v["id"]; ok {
		return jsonValueEqual(id, want)
	}
	// 响应无 id 字段：仅当请求 id 缺失/null 时匹配
	return want == nil
}

// jsonValueEqual 比较两个已解析的 JSON 值是否相等（serde_json::Value 的 PartialEq）。
func jsonValueEqual(a, b any) bool {
	an, aIsNum := a.(json.Number)
	bn, bIsNum := b.(json.Number)
	if aIsNum && bIsNum {
		// serde_json 中 1 (u64) 与 1.0 (f64) 不相等；这里按字面量比较以保持一致。
		return an.String() == bn.String()
	}
	if aIsNum != bIsNum {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// iterSSEDataPayloads 从 SSE 文本中提取每个 event 的 data 负载（多行 data: 拼接为一个负载）
// （filter.rs:79-113）。SSE 规则：以空行分隔 event；event 内 "data:" 开头的行去前缀后按换行拼接。
func iterSSEDataPayloads(body string) []string {
	payloads := []string{}
	var current strings.Builder
	hasData := false

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			// event 边界
			if hasData {
				payloads = append(payloads, current.String())
			}
			current.Reset()
			hasData = false
			continue
		}
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			// 其它字段（event:/id:/retry:/注释行）忽略
			continue
		}
		// data: 后可能有一个可选空格
		rest = strings.TrimPrefix(rest, " ")
		if hasData {
			current.WriteByte('\n')
		}
		current.WriteString(rest)
		hasData = true
	}
	// 最后一个 event 没有以空行结尾的情况
	if hasData {
		payloads = append(payloads, current.String())
	}
	return payloads
}

// decodeJSONObject 解析 JSON 对象，数字保留为 json.Number。
func decodeJSONObject(s string) (map[string]any, bool) {
	if strings.TrimSpace(s) == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// decodeRawValue 把 json.RawMessage 解析为可比对的 Go 值；空/null 返回 nil。
func decodeRawValue(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
}
