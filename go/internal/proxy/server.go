package proxy

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"modelbridge/internal/api"
	"modelbridge/internal/app"
	"modelbridge/internal/converter"
	"modelbridge/internal/mcp"
	"modelbridge/internal/web"
)

// HTTP 服务：注册路由；入口 handler 只负责鉴权、读取请求体、创建 RequestContext。
// 对应 Rust `proxy/server.rs`。

// maxBodyBytes 与 Rust `DefaultBodyLimit::max(64MB)` 一致。
const maxBodyBytes = 64 * 1024 * 1024

// NewServer 组装完整的 HTTP 处理链，并返回 MCP 中继状态
// （调用方用它启动后台 OAuth token 刷新）。
func NewServer(state *app.State) (http.Handler, *mcp.State) {
	mux := http.NewServeMux()

	// 代理入口
	mux.HandleFunc("/v1/chat/completions", proxyEntry(state, converter.FormatOpenAIChat, "/v1/chat/completions"))
	mux.HandleFunc("/v1/completions", proxyEntry(state, converter.FormatOpenAIChat, "/v1/completions"))
	mux.HandleFunc("/v1/responses", proxyEntry(state, converter.FormatResponses, "/v1/responses"))
	mux.HandleFunc("/v1/messages", proxyEntry(state, converter.FormatAnthropic, "/v1/messages"))
	mux.HandleFunc("/v1/models", handleListModels(state))

	// 管理 API 与 MCP 中继由各自包注册；OAuth 回调在 mcp 包内（不走 admin 鉴权）。
	api.Register(mux, state)
	// 与 api 包共用同一份 mcp.State：/oauth/callback 必须能读到 oauth/start 写入的
	// pending state，两份 store 会让授权回调永远无法命中。
	mcpState := api.MCPSession(state)
	mcp.Register(mux, mcpState)

	// 静态资源兜底（内嵌前端 + SPA fallback）
	mux.HandleFunc("/", web.StaticHandler)

	return withRecovery(withCORS(mux)), mcpState
}

// withCORS 等价于 Rust 的 `CorsLayer::permissive()`。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "*")
		h.Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withRecovery 捕获 panic，返回与 Rust `CatchPanicLayer` 一致的错误体。
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic while handling %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "server_error", "Internal server error (panic)")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// proxyEntry 构造一个代理入口 handler。
func proxyEntry(state *app.State, format converter.ApiFormat, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reject := checkProxyAuth(state, r); reject != nil {
			reject(w)
			return
		}
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		ctx := NewRequestContext(format, path, r.Header, body)
		ctx.ReqContext = r.Context()
		RouteRequest(state, ctx, w)
	}
}

// handleListModels 返回模型列表；配置为空时从启用渠道的 model_mapping 收集。
func handleListModels(state *app.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reject := checkProxyAuth(state, r); reject != nil {
			reject(w)
			return
		}
		cfg := state.Config()
		models := make([]string, 0, len(cfg.Models))
		if len(cfg.Models) > 0 {
			models = append(models, cfg.Models...)
		} else {
			seen := map[string]bool{}
			for i := range cfg.Channels {
				ch := &cfg.Channels[i]
				if !ch.Enabled {
					continue
				}
				for alias := range ch.ModelMapping {
					if !seen[alias] {
						seen[alias] = true
						models = append(models, alias)
					}
				}
			}
		}
		sort.Strings(models)

		type model struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		}
		out := struct {
			Object string  `json:"object"`
			Data   []model `json:"data"`
		}{Object: "list", Data: make([]model, 0, len(models))}
		for _, m := range models {
			out.Data = append(out.Data, model{ID: m, Object: "model", OwnedBy: "model-bridge"})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// ---------- 鉴权 ----------

// extractBearerToken 从 Authorization 头提取 Bearer token。
func extractBearerToken(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(v, "Bearer ")
	return token, ok
}

// checkProxyAuth 返回 nil 表示通过；否则返回一个写出 401 的闭包。
// 未配置 proxy_tokens 时免鉴权。
func checkProxyAuth(state *app.State, r *http.Request) func(http.ResponseWriter) {
	tokens := state.Config().Auth.ProxyTokens
	if len(tokens) == 0 {
		return nil
	}
	if token, ok := extractBearerToken(r); ok {
		for _, t := range tokens {
			if t == token {
				return nil
			}
		}
	}
	return func(w http.ResponseWriter) {
		writeError(w, http.StatusUnauthorized, "auth_error", "Invalid or missing API key")
	}
}

// CheckAdminAuth 供管理 API 使用：返回 nil 表示通过，否则返回带 401 写出的闭包。
// 未配置 admin_token 或为空字符串时免鉴权（与 Rust 一致）。
func CheckAdminAuth(state *app.State, r *http.Request) func(http.ResponseWriter) {
	adminToken := state.Config().Auth.AdminToken
	if adminToken == nil || *adminToken == "" {
		return nil
	}
	if token, ok := extractBearerToken(r); ok && token == *adminToken {
		return nil
	}
	return func(w http.ResponseWriter) {
		writeError(w, http.StatusUnauthorized, "auth_error", "Invalid or missing admin token")
	}
}

// ---------- 通用响应工具 ----------

// errorBody 是错误响应体的形状，与 Rust 侧逐字一致。
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// WriteError 写出标准错误响应，供 api / mcp 包复用。
func WriteError(w http.ResponseWriter, status int, errType, message string) {
	writeError(w, status, errType, message)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	var body errorBody
	body.Error.Message = message
	body.Error.Type = errType
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("failed to write JSON response: %v", err)
	}
}

// readBody 读取请求体，超过上限时按 Rust `DefaultBodyLimit` 的行为返回 413。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if ok := asMaxBytesError(err, &maxErr); ok {
			log.Printf("Request rejected (413 Payload Too Large): path=%s content-length=%d limit=%d bytes",
				r.URL.Path, r.ContentLength, maxBodyBytes)
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
		return nil, false
	}
	return body, true
}

func asMaxBytesError(err error, target **http.MaxBytesError) bool {
	for err != nil {
		if e, ok := err.(*http.MaxBytesError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
