package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"modelbridge/internal/config"
)

// MCP 中继验证：转发 JSON-RPC、按黑名单裁剪 tools/list、拒绝 tools/call 黑名单工具。
//
// 交互式 OAuth 2.1 授权流程需要外部授权服务器，本沙箱无法端到端驱动（见规划 Risks），
// 因此这里只覆盖中继转发与工具裁剪。
func TestMCPRelayAndToolFiltering(t *testing.T) {
	var (
		mu       sync.Mutex
		received []map[string]any
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		received = append(received, req)
		mu.Unlock()

		method, _ := req["method"].(string)
		id := req["id"]
		var result any
		switch method {
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "keep_tool", "description": "kept"},
				map[string]any{"name": "blocked_tool", "description": "must be filtered"},
			}}
		default:
			result = map[string]any{"ok": true}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  result,
		})
	}))
	defer upstream.Close()

	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	if err := h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
		cfg.McpServers = []config.McpServerConfig{{
			ID:           "mock-mcp",
			Name:         "Mock MCP",
			Endpoint:     upstream.URL,
			Enabled:      true,
			BlockedTools: []string{"blocked_tool"},
		}}
		return nil
	}); err != nil {
		t.Fatalf("配置 MCP server 失败: %v", err)
	}

	// 1) tools/list：黑名单工具必须被删除。
	resp, raw := h.postAdmin("/mcp/mock-mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("tools/list 期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	if strings.Contains(raw, "blocked_tool") {
		t.Fatalf("黑名单工具未被裁剪: %s", raw)
	}
	if !strings.Contains(raw, "keep_tool") {
		t.Fatalf("非黑名单工具不应被删除: %s", raw)
	}
	writeEvidence(t, "mcp-tools-list.json", []byte(raw))

	// 2) tools/call 命中黑名单：本地直接拒绝，绝不转发上游。
	resp, raw = h.postAdmin("/mcp/mock-mcp",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"blocked_tool","arguments":{}}}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("被拦截的 tools/call 期望 200（JSON-RPC 错误体），实际 %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, "error") {
		t.Fatalf("被拦截的 tools/call 应返回 JSON-RPC error: %s", raw)
	}
	mu.Lock()
	for _, req := range received {
		if req["method"] == "tools/call" {
			mu.Unlock()
			t.Fatal("黑名单工具的 tools/call 不应被转发到上游")
		}
	}
	mu.Unlock()
	writeEvidence(t, "mcp-blocked-call.json", []byte(raw))

	// 3) tools/call 未命中黑名单：正常转发。
	resp, raw = h.postAdmin("/mcp/mock-mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"keep_tool","arguments":{}}}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("放行的 tools/call 期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	mu.Lock()
	forwarded := len(received)
	mu.Unlock()
	if forwarded != 2 {
		t.Fatalf("上游应只收到 tools/list 与放行的 tools/call 共 2 次，实际 %d", forwarded)
	}
	writeEvidence(t, "mcp-allowed-call.json", []byte(raw))

	// 4) 未知 server_id：404 + JSON-RPC error。
	resp, raw = h.postAdmin("/mcp/no-such-server", `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, "")
	if resp.StatusCode != 404 {
		t.Fatalf("未知 server_id 期望 404，实际 %d: %s", resp.StatusCode, raw)
	}
	writeEvidence(t, "mcp-not-found.json", []byte(raw))
}
