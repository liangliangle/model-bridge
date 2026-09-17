// Package auth 收敛两处 HTTP 鉴权规则：管理端（admin_token）与代理入口（proxy_tokens）。
//
// 收敛的原因：同一条「admin_token 为空即免鉴权、否则要求 Authorization: Bearer 完全相等」
// 的规则此前在 proxy 与 api 两个包里各写了一份（api 是因为不能 import proxy 而被迫复刻），
// 两处一旦漂移，同一个 token 在 /v1/* 与 /api/* 上会有不同结果。规则与 401 响应体现在
// 只此一份，proxy / api 都从这里调用。
package auth

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"modelbridge/internal/app"
)

// errorBody 是错误响应体的形状，与 Rust 侧逐字一致：
// `{"error": {"message": "...", "type": "auth_error"}}`。
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// WriteError 写出标准错误响应（proxy / api / mcp 共用同一形态）。
func WriteError(w http.ResponseWriter, status int, errType, message string) {
	var body errorBody
	body.Error.Message = message
	body.Error.Type = errType
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("failed to write JSON response: %v", err)
	}
}

// BearerToken 从 Authorization 头提取 Bearer token。
func BearerToken(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token, ok
}

// AdminOK 判断请求是否通过管理端鉴权。
//
// 对应 Rust `proxy::server::check_admin_auth`（server.rs:120）：admin_token 为
// None 或空字符串时免鉴权，否则要求 `Authorization: Bearer <token>` 完全相等。
func AdminOK(state *app.State, r *http.Request) bool {
	expected := state.Config().Auth.AdminToken
	if expected == nil || *expected == "" {
		return true
	}
	token, ok := BearerToken(r)
	return ok && token == *expected
}

// ProxyOK 判断请求是否通过代理入口鉴权：proxy_tokens 为空时免鉴权，
// 否则 Bearer token 命中任一配置项即通过。
func ProxyOK(state *app.State, r *http.Request) bool {
	tokens := state.Config().Auth.ProxyTokens
	if len(tokens) == 0 {
		return true
	}
	token, ok := BearerToken(r)
	if !ok {
		return false
	}
	for _, t := range tokens {
		if t == token {
			return true
		}
	}
	return false
}

// RequireAdmin 在鉴权失败时写出 401 响应并返回 false。
// 响应体与 Rust `auth_error_response`（server.rs:132）逐字一致。
func RequireAdmin(w http.ResponseWriter, r *http.Request, state *app.State) bool {
	if AdminOK(state, r) {
		return true
	}
	WriteError(w, http.StatusUnauthorized, "auth_error", "Invalid or missing admin token")
	return false
}

// RequireProxy 在鉴权失败时写出 401 响应并返回 false。
func RequireProxy(w http.ResponseWriter, r *http.Request, state *app.State) bool {
	if ProxyOK(state, r) {
		return true
	}
	WriteError(w, http.StatusUnauthorized, "auth_error", "Invalid or missing API key")
	return false
}
