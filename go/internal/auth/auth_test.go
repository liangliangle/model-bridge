package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"modelbridge/internal/app"
	"modelbridge/internal/auth"
	"modelbridge/internal/config"
)

// newState 构造只带鉴权配置的 state（auth 只读 Config，不需要审计库）。
func newState(adminToken *string, proxyTokens []string) *app.State {
	cfg := config.DefaultConfig()
	cfg.Auth.AdminToken = adminToken
	cfg.Auth.ProxyTokens = proxyTokens
	return app.New(cfg, "", nil)
}

func ptr(s string) *string { return &s }

func request(authorization string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

func TestAdminOK(t *testing.T) {
	cases := []struct {
		name       string
		adminToken *string
		header     string
		want       bool
	}{
		{"未配置 admin_token：免鉴权", nil, "", true},
		{"admin_token 为空串：免鉴权", ptr(""), "", true},
		{"携带正确 token", ptr("ll.941107"), "Bearer ll.941107", true},
		{"token 不匹配", ptr("ll.941107"), "Bearer nope", false},
		{"缺少 Authorization", ptr("ll.941107"), "", false},
		{"缺少 Bearer 前缀", ptr("ll.941107"), "ll.941107", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newState(tc.adminToken, nil)
			if got := auth.AdminOK(state, request(tc.header)); got != tc.want {
				t.Fatalf("AdminOK = %v, want %v", got, tc.want)
			}
			// RequireAdmin 必须给出与 AdminOK 一致的判定。
			rec := httptest.NewRecorder()
			if got := auth.RequireAdmin(rec, request(tc.header), state); got != tc.want {
				t.Fatalf("RequireAdmin = %v, want %v", got, tc.want)
			}
			if tc.want && rec.Body.Len() != 0 {
				t.Fatalf("通过鉴权时不应写响应体: %q", rec.Body.String())
			}
		})
	}
}

// TestRequireAdminResponse 固定 401 响应体，与 Rust auth_error_response 逐字一致。
func TestRequireAdminResponse(t *testing.T) {
	state := newState(ptr("secret"), nil)
	rec := httptest.NewRecorder()
	if auth.RequireAdmin(rec, request("Bearer wrong"), state) {
		t.Fatalf("错误 token 不应通过")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	want := `{"error":{"message":"Invalid or missing admin token","type":"auth_error"}}` + "\n"
	if rec.Body.String() != want {
		t.Fatalf("响应体 = %q, want %q", rec.Body.String(), want)
	}
}

func TestProxyOK(t *testing.T) {
	cases := []struct {
		name        string
		proxyTokens []string
		header      string
		want        bool
	}{
		{"未配置 proxy_tokens：免鉴权", nil, "", true},
		{"空列表：免鉴权", []string{}, "", true},
		{"命中其中一个 token", []string{"sk-a", "sk-b"}, "Bearer sk-b", true},
		{"token 不匹配", []string{"sk-a"}, "Bearer sk-c", false},
		{"缺少 Authorization", []string{"sk-a"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newState(nil, tc.proxyTokens)
			if got := auth.ProxyOK(state, request(tc.header)); got != tc.want {
				t.Fatalf("ProxyOK = %v, want %v", got, tc.want)
			}
			rec := httptest.NewRecorder()
			if got := auth.RequireProxy(rec, request(tc.header), state); got != tc.want {
				t.Fatalf("RequireProxy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequireProxyResponse(t *testing.T) {
	state := newState(nil, []string{"sk-a"})
	rec := httptest.NewRecorder()
	if auth.RequireProxy(rec, request("Bearer sk-c"), state) {
		t.Fatalf("错误 token 不应通过")
	}
	want := `{"error":{"message":"Invalid or missing API key","type":"auth_error"}}` + "\n"
	if rec.Code != http.StatusUnauthorized || rec.Body.String() != want {
		t.Fatalf("响应 = %d %q, want 401 %q", rec.Code, rec.Body.String(), want)
	}
}
