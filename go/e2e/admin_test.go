package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modelbridge/internal/config"
)

// 管理 API 契约：逐个请求代表性端点，断言响应含前端所需字段；
// 再做写回读：POST 保存后 GET 断言生效，并检查磁盘配置文件真的变了。

func TestAdminAPIContract(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	if err := h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
		return nil
	}); err != nil {
		t.Fatalf("更新渠道地址失败: %v", err)
	}
	// 造一条审计记录，供 /api/audit 与 /api/audit/detail 使用。
	if _, raw := h.post("/v1/chat/completions", body(chatRequestBody, false)); !strings.Contains(raw, mockContent) {
		t.Fatalf("准备审计数据失败: %s", raw)
	}

	// ---------- 读端点 ----------
	getCases := []struct {
		name     string
		path     string
		assert   func(t *testing.T, obj map[string]any)
		wantList bool
	}{
		{name: "auth-status", path: "/api/auth/status", assert: func(t *testing.T, obj map[string]any) {
			if _, ok := obj["required"].(bool); !ok {
				t.Fatalf("缺少 required 字段: %#v", obj)
			}
			if _, ok := obj["valid"].(bool); !ok {
				t.Fatalf("缺少 valid 字段: %#v", obj)
			}
		}},
		{name: "config", path: "/api/config", assert: func(t *testing.T, obj map[string]any) {
			for _, key := range []string{"listen_port", "listen_host", "models", "channels", "failover", "auth", "mcp_servers", "audit_retention_days"} {
				if _, ok := obj[key]; !ok {
					t.Fatalf("ConfigViewData 缺少字段 %s: %#v", key, obj)
				}
			}
			channels := asList(t, obj["channels"], "channels")
			if len(channels) != 1 {
				t.Fatalf("期望 1 个渠道，实际 %d", len(channels))
			}
			ch := channels[0].(map[string]any)
			if ch["api_key"] != "test-key" {
				t.Fatalf("渠道视图必须回传 api_key（前端需要），实际 %v", ch["api_key"])
			}
			for _, key := range []string{"id", "provider", "url", "priority", "enabled", "model_mapping"} {
				if _, ok := ch[key]; !ok {
					t.Fatalf("渠道视图缺少字段 %s: %#v", key, ch)
				}
			}
			failover := obj["failover"].(map[string]any)
			if _, ok := failover["max_failover_channels"]; !ok {
				t.Fatalf("failover 缺少 max_failover_channels: %#v", failover)
			}
		}},
		{name: "channels", path: "/api/channels", wantList: true},
		{name: "channels-health", path: "/api/channels/health", wantList: true},
		{name: "audit", path: "/api/audit", wantList: true},
		{name: "audit-db-status", path: "/api/audit/db/status", assert: func(t *testing.T, obj map[string]any) {
			for _, key := range []string{"size_bytes", "total_records", "detail_records"} {
				if _, ok := obj[key]; !ok {
					t.Fatalf("审计库状态缺少字段 %s: %#v", key, obj)
				}
			}
		}},
		{name: "stats", path: "/api/stats"},
		{name: "stats-heatmap", path: "/api/stats/heatmap", wantList: true},
		{name: "model-prices", path: "/api/model-prices", wantList: true},
	}

	var auditID float64
	for _, tc := range getCases {
		resp, raw := h.get(tc.path, "")
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s 期望 200，实际 %d: %s", tc.path, resp.StatusCode, raw)
		}
		writeEvidence(t, "admin-"+tc.name+".json", []byte(raw))

		if tc.wantList {
			var list []map[string]any
			if err := json.Unmarshal([]byte(raw), &list); err != nil {
				t.Fatalf("GET %s 应为 JSON 数组: %v\n%s", tc.path, err, raw)
			}
			if tc.name == "audit" {
				if len(list) == 0 {
					t.Fatalf("审计列表不应为空")
				}
				auditID = num(t, list[0]["id"], "audit id")
			}
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			t.Fatalf("GET %s 应为 JSON 对象: %v\n%s", tc.path, err, raw)
		}
		if tc.assert != nil {
			tc.assert(t, obj)
		}
	}

	// 详情报文（AuditEntry）
	if auditID == 0 {
		t.Fatal("没有取到审计 id")
	}
	detailPath := "/api/audit/detail?id=" + formatID(auditID)
	resp, raw := h.get(detailPath, "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s 期望 200，实际 %d", detailPath, resp.StatusCode)
	}
	detail := decode(t, raw)
	for _, key := range []string{"id", "timestamp", "path", "request_body", "upstream_response_body", "response_body"} {
		if _, ok := detail[key]; !ok {
			t.Fatalf("AuditEntry 缺少字段 %s: %#v", key, detail)
		}
	}
	writeEvidence(t, "admin-audit-detail.json", []byte(raw))

	// ---------- 写回读：failover ----------
	configPath := filepath.Join(h.dir, "config.yaml")
	failoverBody := `{"max_failover_channels":7,"retry_timeout_ms":1234,"failure_threshold":5,"recovery_interval_sec":60,"probe_requests":3}`
	resp, raw = h.postAdmin("/api/config/failover", failoverBody, "")
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/config/failover 期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	writeEvidence(t, "admin-save-failover.json", []byte(raw))

	_, raw = h.get("/api/config", "")
	cfgView := decode(t, raw)
	failover := cfgView["failover"].(map[string]any)
	if got := num(t, failover["max_failover_channels"], "max_failover_channels"); got != 7 {
		t.Fatalf("failover 未生效，期望 7，实际 %v", got)
	}
	onDisk := readFileT(t, configPath)
	if !strings.Contains(onDisk, "max_failover_channels: 7") {
		t.Fatalf("配置文件未落盘 max_failover_channels: 7\n%s", onDisk)
	}
	writeEvidence(t, "admin-config-after-failover.json", []byte(raw))

	// ---------- 写回读：channel 新增 → 更新 → 删除 ----------
	newChannel := `{"id":"added-ch","name":"Added","provider":"openai","url":"` + h.mock.URL("/chat") + `","api_key":"k2","priority":9,"enabled":true,"fallback_model":"up-model","model_mapping":{"gpt-test":"up-model"},"timeout_ms":5000}`
	resp, raw = h.postAdmin("/api/config/channel", newChannel, "")
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/config/channel 期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	_, raw = h.get("/api/channels", "")
	if !strings.Contains(raw, "added-ch") {
		t.Fatalf("新增渠道未生效: %s", raw)
	}
	if !strings.Contains(readFileT(t, configPath), "added-ch") {
		t.Fatal("新增渠道未落盘")
	}
	writeEvidence(t, "admin-save-channel.json", []byte(raw))

	// 注意字段名：Rust 的 DeleteChannelReq 带 rename_all="camelCase"，前端提交的是 `channelId`。
	resp, raw = h.postAdmin("/api/config/channel/delete", `{"channelId":"added-ch"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/config/channel/delete 期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	_, raw = h.get("/api/channels", "")
	if strings.Contains(raw, "added-ch") {
		t.Fatalf("删除渠道未生效: %s", raw)
	}
	if strings.Contains(readFileT(t, configPath), "added-ch") {
		t.Fatal("删除渠道未落盘")
	}

	// ---------- 模型定价写回读 ----------
	priceBody := `{"model":"up-model","input_per_mtok":2.5,"output_per_mtok":10.0,"cache_read_per_mtok":1.25,"enabled":true}`
	resp, raw = h.postAdmin("/api/model-prices", priceBody, "")
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/model-prices 期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
	_, raw = h.get("/api/model-prices", "")
	if !strings.Contains(raw, "up-model") {
		t.Fatalf("定价未生效: %s", raw)
	}
	writeEvidence(t, "admin-model-prices.json", []byte(raw))

	resp, raw = h.postAdmin("/api/model-prices/delete", `{"model":"up-model"}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/model-prices/delete 期望 200，实际 %d: %s", resp.StatusCode, raw)
	}
}

// 管理端鉴权：admin_token 非空时缺 token 必须 401，带对 token 必须 200。
func TestAdminAuthRequired(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	if err := h.state.Update(func(cfg *config.AppConfig) error {
		token := "secret-admin"
		cfg.Auth.AdminToken = &token
		return nil
	}); err != nil {
		t.Fatalf("设置 admin_token 失败: %v", err)
	}

	resp, raw := h.get("/api/channels", "")
	if resp.StatusCode != 401 {
		t.Fatalf("缺少 admin token 应 401，实际 %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, "auth_error") {
		t.Fatalf("401 响应体应含 auth_error: %s", raw)
	}

	resp, raw = h.get("/api/channels", "secret-admin")
	if resp.StatusCode != 200 {
		t.Fatalf("带正确 admin token 应 200，实际 %d: %s", resp.StatusCode, raw)
	}

	// /api/auth/status 报告鉴权已启用且当前请求有效。
	resp, raw = h.get("/api/auth/status", "secret-admin")
	if resp.StatusCode != 200 {
		t.Fatalf("auth/status 期望 200，实际 %d", resp.StatusCode)
	}
	status := decode(t, raw)
	if status["required"] != true || status["valid"] != true {
		t.Fatalf("auth/status 应为 {required:true, valid:true}，实际 %#v", status)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(raw)
}

// formatID 把 JSON 解析出来的数字 id 转成不带小数点的十进制字符串。
func formatID(v float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(formatFloat(v), ".0"), ".")
}

func formatFloat(v float64) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
