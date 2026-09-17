// Package api 实现管理中台的全部 /api/* 端点。
//
// 契约来源是 Rust 侧 src-tauri/src/commands.rs（同名函数）：路径、方法、查询参数名、
// 请求体字段名、响应 JSON 字段名都逐字保持一致，包括「可选字段序列化成 null」与
// 「缺省省略」的区别，以便前端（src/lib/tauri.ts、src/components/*.tsx）与 macOS
// 客户端无需任何改动。
//
// 两个与 axum 一致的响应约定：
//   - 处理函数内部的业务错误返回 HTTP 200 + {"error": "..."}（Rust 里是
//     `Json(json!({"error": e})).into_response()`，状态码默认 200）；
//   - 鉴权失败返回 401 + {"error": {"message": "...", "type": "auth_error"}}。
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"modelbridge/internal/app"
	"modelbridge/internal/mcp"
)

// maxBodyBytes 请求体上限，与 Rust `DefaultBodyLimit::max(64MB)` 一致。
const maxBodyBytes = 64 << 20

// Register 注册全部 /api/* 管理端点。
//
// mcpState 由调用方（cmd/model-bridge）构造并与 mcp 中继共用同一实例：OAuth 的
// pending 表必须由 oauth/start 写入、由 /oauth/callback 读回，两份 store 会让授权
// 回调永远无法命中。
//
// 对应 Rust `proxy/server.rs` 里的路由表：GET 11 条 + POST 13 条，其中
// /api/model-prices 同时注册 GET（get_model_prices）与 POST（save_model_price）。
func Register(mux *http.ServeMux, state *app.State, mcpState *mcp.State) {
	// ===== GET =====
	mux.HandleFunc("GET /api/auth/status", handleAuthStatus(state))
	mux.HandleFunc("GET /api/stats", handleStats(state))
	mux.HandleFunc("GET /api/stats/heatmap", handleTokenHeatmap(state))
	mux.HandleFunc("GET /api/audit", handleAuditLogs(state))
	mux.HandleFunc("GET /api/audit/detail", handleAuditDetail(state))
	mux.HandleFunc("GET /api/audit/db/status", handleAuditDBStatus(state))
	mux.HandleFunc("GET /api/channels", handleChannels(state))
	mux.HandleFunc("GET /api/channels/health", handleChannelHealth(state))
	mux.HandleFunc("GET /api/config", handleFullConfig(state))
	mux.HandleFunc("GET /api/config/mcp/oauth/status", handleMCPOAuthStatus(state, mcpState))
	mux.HandleFunc("GET /api/model-prices", handleModelPrices(state))

	// ===== POST =====
	mux.HandleFunc("POST /api/audit/cleanup", handleForceCleanupAudit(state))
	mux.HandleFunc("POST /api/config/channel", handleSaveChannel(state))
	mux.HandleFunc("POST /api/config/channel/delete", handleDeleteChannel(state))
	mux.HandleFunc("POST /api/config/failover", handleSaveFailover(state))
	mux.HandleFunc("POST /api/config/settings", handleSaveSettings(state))
	mux.HandleFunc("POST /api/config/channel/test", handleTestChannel(state))
	mux.HandleFunc("POST /api/config/mcp", handleSaveMCPServer(state))
	mux.HandleFunc("POST /api/config/mcp/delete", handleDeleteMCPServer(state))
	mux.HandleFunc("POST /api/config/mcp/oauth/start", handleStartMCPOAuth(state, mcpState))
	mux.HandleFunc("POST /api/config/mcp/tools/fetch", handleFetchMCPTools(state, mcpState))
	mux.HandleFunc("POST /api/config/mcp/tools/toggle", handleToggleMCPTool(state))
	mux.HandleFunc("POST /api/model-prices", handleSaveModelPrice(state))
	mux.HandleFunc("POST /api/model-prices/delete", handleDeleteModelPrice(state))
}

// ---------- 响应工具 ----------

// writeJSON 写出 JSON 响应（状态码默认 200），编码失败只记日志（此时已写出状态码）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		// 编码失败属于意料之外的错误：记录日志，并返回 JSON（而不是 HTML/纯文本）错误体。
		log.Printf("api: 序列化响应失败: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal encode error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Printf("api: 写响应失败: %v", err)
	}
}

// writeOK 写出 200 + JSON。
func writeOK(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, v) }

// writeAPIError 写出 Rust 风格的错误体 `{"error": "..."}`（状态码默认 200）。
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeErr 写出业务错误：HTTP 200 + {"error": msg}，与 Rust 的 Json 响应一致。
func writeErr(w http.ResponseWriter, err error) {
	writeAPIError(w, http.StatusOK, err.Error())
}

// successBody 是 `{"success": true}`，被多个写接口复用。
type successBody struct {
	Success bool `json:"success"`
}

// writeSuccess 写出 `{"success": true}`。
func writeSuccess(w http.ResponseWriter) { writeOK(w, successBody{Success: true}) }

// ---------- 请求体工具 ----------

// readBody 读取请求体（上限 64MB）。
// 超限时写出 413 并返回 nil,false；其它读取错误写出 400 并返回 nil,false。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			// Rust 侧由 DefaultBodyLimit 产生 413（纯文本 "length limit exceeded"），
			// 这里改用 JSON 错误体，状态码保持一致。
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return nil, false
		}
		writeAPIError(w, http.StatusBadRequest, "Bad request: "+err.Error())
		return nil, false
	}
	return raw, true
}

// decodeBody 读取请求体并解析 JSON 到 dst。
// 失败时已写出响应（400 + 错误体），返回 false。
//
// Rust 的 `Json<T>` 提取器在失败时返回 422 纯文本；这里统一成 400 + JSON 错误体，
// 状态码之外的差异不影响前端（前端只在响应体里读 `error`）。
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	raw, ok := readBody(w, r)
	if !ok {
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		writeAPIError(w, http.StatusBadRequest, "Bad request: empty body")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(dst); err != nil {
		writeAPIError(w, http.StatusBadRequest, "Bad request: "+err.Error())
		return false
	}
	return true
}

// ---------- 辅助 ----------

// nonNilMap 保证 map 序列化成 `{}` 而不是 `null`（Rust 侧 HashMap 恒为 `{}`）。
func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// nonNilSlice 保证切片序列化成 `[]` 而不是 `null`（Rust 侧 Vec 恒为 `[]`）。
func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// filterEmpty 复刻 Rust 的 `.filter(|s| !s.is_empty())`：nil 或空串都归为 nil。
func filterEmpty(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// derefStr 取字符串指针的值，nil 时返回空串。
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
