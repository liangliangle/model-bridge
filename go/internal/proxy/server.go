package proxy

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"

	"modelbridge/internal/app"
	"modelbridge/internal/auth"
	"modelbridge/internal/converter"
)

// HTTP 服务：注册代理入口路由；入口 handler 只负责鉴权、读取请求体、创建 RequestContext。
// 对应 Rust `proxy/server.rs`。
//
// 管理 API、MCP 中继与静态资源的路由由装配点（cmd/model-bridge）分别调用
// api.Register / mcp.Register / web.StaticHandler 注册：本包不 import 它们。
// 此前 proxy → api 的反向依赖（由 api 复刻鉴权、并维护按 app.State 索引的全局
// mcp.State 表来绕开）由此消失，鉴权统一走 internal/auth。

// maxBodyBytes 与 Rust `DefaultBodyLimit::max(64MB)` 一致。
const maxBodyBytes = 64 * 1024 * 1024

// Register 注册代理入口路由（/v1/*）。
func Register(mux *http.ServeMux, state *app.State) {
	mux.HandleFunc("/v1/chat/completions", proxyEntry(state, converter.FormatOpenAIChat, "/v1/chat/completions"))
	mux.HandleFunc("/v1/completions", proxyEntry(state, converter.FormatOpenAIChat, "/v1/completions"))
	mux.HandleFunc("/v1/responses", proxyEntry(state, converter.FormatResponses, "/v1/responses"))
	mux.HandleFunc("/v1/messages", proxyEntry(state, converter.FormatAnthropic, "/v1/messages"))
	mux.HandleFunc("/v1/models", handleListModels(state))
}

// Middleware 是整棵路由的最外层包装：CORS + panic 恢复。
func Middleware(next http.Handler) http.Handler {
	return withRecovery(withCORS(next))
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
				auth.WriteError(w, http.StatusInternalServerError, "server_error", "Internal server error (panic)")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// proxyEntry 构造一个代理入口 handler。
func proxyEntry(state *app.State, format converter.ApiFormat, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.RequireProxy(w, r, state) {
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
		if !auth.RequireProxy(w, r, state) {
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

// ---------- 通用响应工具 ----------

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
			auth.WriteError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large")
			return nil, false
		}
		auth.WriteError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
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
