package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modelbridge/internal/api"
	"modelbridge/internal/app"
	"modelbridge/internal/audit"
	"modelbridge/internal/config"
	"modelbridge/internal/mcp"
	"modelbridge/internal/proxy"
	"modelbridge/internal/sse"
	"modelbridge/internal/web"
)

// 端到端验证：真实启动后端（proxy.NewServer），以 mock 上游为渠道端点，
// 走真实的 HTTP 入口，断言「发往上游的请求体」与「下发给客户端的响应体」。
//
// 证据写入 $SCRATCH（规划中的 {SCRATCH}）。

// writeEvidence 把证据落盘到 $SCRATCH；未设置时静默跳过（断言不受影响）。
func writeEvidence(t *testing.T, name string, content []byte) {
	t.Helper()
	dir := os.Getenv("SCRATCH")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
		t.Fatalf("写证据 %s 失败: %v", name, err)
	}
}

func writeEvidenceJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("序列化证据 %s 失败: %v", name, err)
	}
	writeEvidence(t, name, raw)
}

// harness 是被测后端 + mock 上游。
type harness struct {
	t      *testing.T
	mock   *MockUpstream
	state  *app.State
	server *httptest.Server
	dir    string
}

type channelSpec struct {
	id       string
	provider config.ProviderType
	url      string
	priority uint32
}

func newHarness(t *testing.T, specs ...channelSpec) *harness {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // 隔离 ~/.model-bridge

	mock := NewMockUpstream()
	t.Cleanup(mock.Close)

	cfg := config.DefaultConfig()
	cfg.Failover.MaxFailoverChannels = 10
	cfg.Channels = make([]config.ChannelConfig, 0, len(specs))
	for _, s := range specs {
		cfg.Channels = append(cfg.Channels, config.ChannelConfig{
			ID:            s.id,
			Name:          s.id,
			Provider:      s.provider,
			Endpoint:      config.EndpointConfig{URL: s.url},
			APIKey:        "test-key",
			Priority:      s.priority,
			Enabled:       true,
			FallbackModel: "up-model",
			ModelMapping:  map[string]string{"gpt-test": "up-model"},
			TimeoutMs:     5000,
		})
	}

	dir := t.TempDir()
	db, err := audit.Open(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("打开审计库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	state := app.New(cfg, filepath.Join(dir, "config.yaml"), db)
	handler, _ := newBackend(state)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &harness{t: t, mock: mock, state: state, server: srv, dir: dir}
}

// newBackend 按 cmd/model-bridge 的装配顺序组装完整 handler（代理入口 + 管理 API +
// MCP 中继 + 静态兜底，最后套 CORS/panic 中间件），并返回 MCP 中继状态。
func newBackend(state *app.State) (http.Handler, *mcp.State) {
	mcpState := mcp.NewState(state, state.HTTP, state.Audit)
	mux := http.NewServeMux()
	proxy.Register(mux, state)
	api.Register(mux, state, mcpState)
	mcp.Register(mux, mcpState)
	mux.HandleFunc("/", web.StaticHandler)
	return proxy.Middleware(mux), mcpState
}

func (h *harness) post(path, body string) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		h.t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("请求 %s 失败: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("读取响应失败: %v", err)
	}
	return resp, string(raw)
}

// get 发起 GET 请求；token 非空时带 Authorization 头。
func (h *harness) get(path, token string) (*http.Response, string) {
	h.t.Helper()
	return h.do(http.MethodGet, path, "", token)
}

// postAdmin 以管理令牌发起 POST（body 为空字符串表示不发送请求体）。
func (h *harness) postAdmin(path, bodyJSON, token string) (*http.Response, string) {
	h.t.Helper()
	return h.do(http.MethodPost, path, bodyJSON, token)
}

func (h *harness) do(method, path, bodyJSON, token string) (*http.Response, string) {
	h.t.Helper()
	var reader io.Reader
	if bodyJSON != "" {
		reader = bytes.NewReader([]byte(bodyJSON))
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("构造请求失败: %v", err)
	}
	if bodyJSON != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("请求 %s 失败: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("读取响应失败: %v", err)
	}
	return resp, string(raw)
}

// parseSSE 解析下游事件流，复用后端的 internal/sse（分帧规则只有一处实现）。
//
// sse.Scan 把 `[DONE]` 视为流结束标志且不交付该事件，而这里的断言要看流尾标记，
// 因此收到过 [DONE] 时补一个 data 为 "[DONE]" 的事件。
func parseSSE(t *testing.T, raw string) []sse.Event {
	t.Helper()
	var out []sse.Event
	if err := sse.Scan(strings.NewReader(raw), func(ev sse.Event) error {
		out = append(out, ev)
		return nil
	}); err != nil {
		t.Fatalf("解析下游 SSE 失败: %v", err)
	}
	if strings.Contains(raw, sse.Done) {
		out = append(out, sse.Event{Data: sse.Done})
	}
	return out
}

// sseTypes 返回 data 载荷里 type 字段（Anthropic/Responses 形态）或 "chat.chunk"。
func sseTypes(t *testing.T, events []sse.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		if ev.Data == "[DONE]" {
			out = append(out, "[DONE]")
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			t.Fatalf("SSE data 不是合法 JSON: %q", ev.Data)
		}
		if typ, ok := payload["type"].(string); ok {
			out = append(out, typ)
			continue
		}
		out = append(out, "chat.chunk")
	}
	return out
}

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, raw)
	}
	return out
}

func asList(t *testing.T, v any, what string) []any {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%s 不是数组: %#v", what, v)
	}
	return list
}

func num(t *testing.T, v any, what string) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s 不是数字: %#v", what, v)
	}
	return f
}

// ==================== 请求体样本 ====================

const messagesRequestBody = `{
  "model": "gpt-test",
  "max_tokens": 64,
  "system": "be brief",
  "messages": [{"role": "user", "content": "hi"}],
  "stream": %s
}`

const responsesRequestBody = `{
  "model": "gpt-test",
  "instructions": "be brief",
  "input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}],
  "stream": %s
}`

const chatRequestBody = `{
  "model": "gpt-test",
  "max_tokens": 64,
  "messages": [{"role": "user", "content": "hi"}],
  "stream": %s
}`

func body(tmpl string, stream bool) string {
	return fmt.Sprintf(tmpl, fmt.Sprintf("%t", stream))
}

func assertUpstreamModel(t *testing.T, req RecordedRequest) {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(req.Body, &parsed); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, req.Body)
	}
	if parsed["model"] != "up-model" {
		t.Fatalf("上游 model 应为 up-model，实际 %v", parsed["model"])
	}
}

// assertNonStreamJSON 断言下游返回的是 JSON，且文本内容等于 mock 内容。
func assertNonStreamJSON(t *testing.T, wantContentType string, resp *http.Response, raw string) map[string]any {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, wantContentType) {
		t.Fatalf("Content-Type 应为 %s，实际 %s", wantContentType, ct)
	}
	return decode(t, raw)
}
