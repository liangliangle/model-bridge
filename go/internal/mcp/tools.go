// 主动握手拉取上游 MCP server 的工具列表（对应 src-tauri/src/mcp/tools.rs）。
// 流程：initialize → notifications/initialized → tools/list（分页）。
// 复用 OAuth 鉴权、头注入、JSON-RPC 响应解析。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"modelbridge/internal/config"
)

// mcpProtocolVersion 默认协议版本（tools.rs:22）。
const mcpProtocolVersion = "2025-06-18"

// maxToolPages 分页防御上限（tools.rs:24）。
const maxToolPages = 100

// errTextLimit 错误信息截断长度（字符数）（tools.rs:29）。
const errTextLimit = 500

// FetchToolsErrorKind 区分「需授权」与「普通失败」，便于前端分别提示（tools.rs:17-20）。
type FetchToolsErrorKind int

const (
	// FetchToolsNeedsAuth 需要重新授权。
	FetchToolsNeedsAuth FetchToolsErrorKind = iota
	// FetchToolsFailed 普通失败。
	FetchToolsFailed
)

// FetchToolsError 拉取工具的错误。
type FetchToolsError struct {
	Kind    FetchToolsErrorKind
	Message string
}

func (e *FetchToolsError) Error() string { return e.Message }

// FetchTools 主动握手拉取工具列表（tools.rs:52-162）。
func FetchTools(ctx context.Context, st *State, server *config.McpServerConfig) ([]config.ToolInfo, error) {
	// 步骤 0：鉴权
	token, err := st.EnsureValidToken(ctx, server)
	if err != nil {
		return nil, &FetchToolsError{Kind: FetchToolsNeedsAuth, Message: err.Error()}
	}

	// 步骤 1：initialize
	initBody := mustJSON(map[string]any{
		"jsonrpc": jsonrpcVersion,
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "model-bridge", "version": "0.1.0"},
		},
	})
	resp, err := doPost(ctx, st, server, token, initBody)
	if err != nil {
		return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: "initialize 网络错误: " + err.Error()}
	}
	// 从响应头抓 session id（部分上游 stateless 不下发，为空时后续不加该头）
	sessionID := resp.Header.Get("Mcp-Session-Id")
	text, readErr := readAllString(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: "读取 initialize 响应失败: " + readErr.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: fmt.Sprintf("initialize 返回 %d: %s", resp.StatusCode, truncate(text))}
	}
	initRPC, ok := ExtractJSONRPCResponse(text, json.RawMessage("1"))
	if !ok {
		return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: "initialize 响应无法解析: " + truncate(text)}
	}
	if errObj, has := initRPC["error"]; has {
		return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: fmt.Sprintf("initialize 返回错误: %v", errObj)}
	}
	// 协议版本：跟随上游响应，缺省用默认
	proto := mcpProtocolVersion
	if result, ok := initRPC["result"].(map[string]any); ok {
		if v, ok := result["protocolVersion"].(string); ok && v != "" {
			proto = v
		}
	}

	// 步骤 2：notifications/initialized（失败容错，不致命）
	notifBody := mustJSON(map[string]any{"jsonrpc": jsonrpcVersion, "method": "notifications/initialized"})
	if req, err := newPostRequest(ctx, st, server, token, notifBody); err == nil {
		addSessionHeaders(req, sessionID, proto)
		if resp, err := st.HTTPClient().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}

	// 步骤 3：tools/list 分页循环
	tools := []config.ToolInfo{}
	cursor := ""
	var nextID int64 = 2
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		reqBody := mustJSON(map[string]any{
			"jsonrpc": jsonrpcVersion,
			"id":      nextID,
			"method":  "tools/list",
			"params":  params,
		})
		req, err := newPostRequest(ctx, st, server, token, reqBody)
		if err != nil {
			return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: "tools/list 网络错误: " + err.Error()}
		}
		addSessionHeaders(req, sessionID, proto)
		resp, err := st.HTTPClient().Do(req)
		if err != nil {
			return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: "tools/list 网络错误: " + err.Error()}
		}
		text, readErr := readAllString(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: "读取 tools/list 响应失败: " + readErr.Error()}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: fmt.Sprintf("tools/list 返回 %d: %s", resp.StatusCode, truncate(text))}
		}
		idRaw, _ := json.Marshal(nextID)
		rpc, ok := ExtractJSONRPCResponse(text, idRaw)
		if !ok {
			return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: "tools/list 响应无法解析: " + truncate(text)}
		}
		if errObj, has := rpc["error"]; has {
			return nil, &FetchToolsError{Kind: FetchToolsFailed, Message: fmt.Sprintf("上游 tools/list 错误: %v", errObj)}
		}
		if result, ok := rpc["result"].(map[string]any); ok {
			if arr, ok := result["tools"].([]any); ok {
				for _, t := range arr {
					obj, ok := t.(map[string]any)
					if !ok {
						continue
					}
					name, _ := obj["name"].(string)
					if name == "" {
						continue
					}
					info := config.ToolInfo{Name: name}
					if desc, ok := obj["description"].(string); ok {
						d := desc
						info.Description = &d
					}
					if schema, ok := obj["inputSchema"].(map[string]any); ok {
						info.InputSchema = schema
					}
					tools = append(tools, info)
				}
			}
			if c, ok := result["nextCursor"].(string); ok {
				cursor = c
			} else {
				cursor = ""
			}
		} else {
			cursor = ""
		}
		nextID++
		if cursor == "" || nextID > maxToolPages {
			break
		}
	}
	return tools, nil
}

// doPost 发送一次带鉴权的 POST 并返回响应（tools.rs:40-49 post_builder 的等价物）。
func doPost(ctx context.Context, st *State, server *config.McpServerConfig, token *string, body string) (*http.Response, error) {
	req, err := newPostRequest(ctx, st, server, token, body)
	if err != nil {
		return nil, err
	}
	return st.HTTPClient().Do(req)
}

// newPostRequest 构造一个带认证 + Accept 的 POST 请求（tools.rs:39-49）。
// 注意：这里的 Accept 是 MCP 握手专用的 "application/json, text/event-stream"。
func newPostRequest(ctx context.Context, st *State, server *config.McpServerConfig, token *string, body string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.Endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 空 client headers：握手不携带客户端原始头（tools.rs:41-49）
	req.Header = BuildForwardHeaders(http.Header{}, server, token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return req, nil
}

// addSessionHeaders 在 build_mcp_forward_headers 之后追加 MCP 会话头
// （它本身不管 session/protocol-version）（tools.rs:31-37）。
func addSessionHeaders(req *http.Request, sessionID, proto string) {
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	req.Header.Set("MCP-Protocol-Version", proto)
}

// truncate 截断过长文本（错误信息/日志用），按字符边界（tools.rs:27-29）。
func truncate(s string) string {
	runes := []rune(s)
	if len(runes) <= errTextLimit {
		return s
	}
	return string(runes[:errTextLimit])
}

// mustJSON 序列化 JSON（结构固定，不会失败）。
func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
