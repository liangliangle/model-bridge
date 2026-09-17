package mcp

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"modelbridge/internal/config"
)

// 路由注册与入口 handler，对应 Rust `mcp/mod.rs` 的 mcp_post / mcp_get / mcp_delete。
//
// 注意：与 Rust 现状一致，这三个入口**不做代理 token 鉴权**（Rust 只在
// /v1/* 入口调用 check_proxy_auth，MCP 路径没有挂任何鉴权中间件）。此处刻意保持
// 一致，避免未经要求的行为变更。

// maxBodyBytes 与 Rust `DefaultBodyLimit::max(64MB)` 一致。
const maxBodyBytes = 64 * 1024 * 1024

// Register 注册 MCP 中继与 OAuth 回调路由。
// mux 上的模式与 Rust 路由表一致：/mcp/{server_id} 支持 POST/GET/DELETE，
// /oauth/callback 为授权服务器重定向目标（靠 state 参数校验，不走 admin 鉴权）。
func Register(mux *http.ServeMux, st *State) {
	mux.HandleFunc("/mcp/", func(w http.ResponseWriter, r *http.Request) {
		serverID := strings.TrimPrefix(r.URL.Path, "/mcp/")
		if serverID == "" || strings.Contains(serverID, "/") {
			writeJSONRPCNotFound(w, strings.TrimPrefix(r.URL.Path, "/mcp/"))
			return
		}
		switch r.Method {
		case http.MethodPost:
			handlePost(st, serverID, w, r)
		case http.MethodGet:
			handleGet(st, serverID, w, r)
		case http.MethodDelete:
			handleDelete(st, serverID, w, r)
		default:
			w.Header().Set("Allow", "POST, GET, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		st.HandleOAuthCallback(w, r)
	})
}

// lookupServer 按 server_id 查找已启用的 MCP server；找不到时返回 404 + JSON-RPC error。
func lookupServer(st *State, serverID string) (*config.McpServerConfig, bool) {
	cfg := st.snapshot()
	if cfg == nil {
		return nil, false
	}
	server := cfg.FindMCPServer(serverID)
	return server, server != nil
}

func writeJSONRPCNotFound(w http.ResponseWriter, serverID string) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]any{
			"code":    -32600,
			"message": "MCP server not found: " + serverID,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(body)
}

// handlePost 处理 POST /mcp/{server_id}：客户端发送 JSON-RPC 消息。
func handlePost(st *State, serverID string, w http.ResponseWriter, r *http.Request) {
	server, ok := lookupServer(st, serverID)
	if !ok {
		writeJSONRPCNotFound(w, serverID)
		return
	}

	body, ok := readMCPBody(w, r)
	if !ok {
		return
	}
	parsed := ParseJSONRPCRequest(body)
	path := "/mcp/" + serverID

	// 拦截 A：tools/call 调用黑名单工具 → 直接拒绝，不转发上游。
	if parsed.Method == "tools/call" && IsToolBlocked(server.BlockedTools, parsed.ToolName) {
		auditID, _ := st.auditCreate("POST", path, "tools/call:"+parsed.ToolName, nil, &body, userAgent(r))
		if auditID > 0 {
			_ = st.Audit.UpdateError(auditID, http.StatusOK, 0, "blocked tool: "+parsed.ToolName)
		}
		resp := RejectToolCall(parsed.ReqID, parsed.ToolName)
		_ = resp.WriteTo(r.Context(), w)
		return
	}

	// 审计占位：alias_model 字段复用为 MCP method 名。
	alias := parsed.Method
	if alias == "" {
		alias = "mcp"
	}
	auditID, _ := st.auditCreate("POST", path, alias, nil, &body, userAgent(r))

	resp := ForwardPost(r.Context(), st, server, r.Header, body, parsed.Method, parsed.ReqID, auditID)
	if err := resp.WriteTo(r.Context(), w); err != nil {
		log.Printf("mcp POST /mcp/%s: write response failed: %v", serverID, err)
	}
}

// handleGet 处理 GET /mcp/{server_id}：客户端打开 SSE 流接收服务端推送。
func handleGet(st *State, serverID string, w http.ResponseWriter, r *http.Request) {
	server, ok := lookupServer(st, serverID)
	if !ok {
		writeJSONRPCNotFound(w, serverID)
		return
	}
	auditID, _ := st.auditCreate("GET", "/mcp/"+serverID, "mcp:sse", nil, nil, userAgent(r))
	resp := ForwardGet(r.Context(), st, server, r.Header, auditID)
	if err := resp.WriteTo(r.Context(), w); err != nil {
		log.Printf("mcp GET /mcp/%s: write response failed: %v", serverID, err)
	}
}

// handleDelete 处理 DELETE /mcp/{server_id}：终止会话。
func handleDelete(st *State, serverID string, w http.ResponseWriter, r *http.Request) {
	server, ok := lookupServer(st, serverID)
	if !ok {
		writeJSONRPCNotFound(w, serverID)
		return
	}
	auditID, _ := st.auditCreate("DELETE", "/mcp/"+serverID, "mcp:delete", nil, nil, userAgent(r))
	resp := ForwardDelete(r.Context(), st, server, r.Header, auditID)
	if err := resp.WriteTo(r.Context(), w); err != nil {
		log.Printf("mcp DELETE /mcp/%s: write response failed: %v", serverID, err)
	}
}

// auditCreate 建审计记录；未注入 AuditSink 时返回 -1。
func (s *State) auditCreate(method, path, alias string, headers, body *string, userAgent *string) (int64, error) {
	if s == nil || s.Audit == nil {
		return -1, nil
	}
	return s.Audit.Create(method, path, alias, headers, body, userAgent)
}

func userAgent(r *http.Request) *string {
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		return nil
	}
	return &ua
}

// readMCPBody 读取请求体，超限时返回 413。
func readMCPBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		if _, ok := err.(*http.MaxBytesError); ok {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return "", false
		}
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return "", false
	}
	return string(raw), true
}
