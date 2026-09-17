// 管理 API 的端到端测试：用真实配置/真实 SQLite 审计库驱动真实注册的 mux，
// 断言序列化后的响应字节与磁盘文件内容（而不是复述 handler 实现）。
package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"modelbridge/internal/api"
	"modelbridge/internal/app"
	"modelbridge/internal/audit"
	"modelbridge/internal/config"
	"modelbridge/internal/mcp"
)

// testConfigYAML 测试用配置：两个渠道 + failover + 一条模型价格 + 一个 MCP server。
// 注意 URL 指向保留端口 127.0.0.1:1，正常路径不会真的发请求。
const testConfigYAML = `
listen_port: 18080
listen_host: 127.0.0.1
debug: false
models:
  - gpt-4o
channels:
  - id: ch-alpha
    name: Alpha
    provider: openai
    endpoint:
      url: http://127.0.0.1:1/v1/chat/completions
    api_key: sk-alpha-key
    priority: 1
    enabled: true
    fallback_model: gpt-4o-mini
    model_mapping:
      gpt-4o: gpt-4o-2024-08-06
    timeout_ms: 30000
    custom_headers:
      X-Trace: alpha
    strip_thinking: true
    retry_count: 2
    retry_delay_ms: 250
    force_effort: high
    auto_cache: false
  - id: ch-beta
    name: Beta
    provider: anthropic
    endpoint:
      url: http://127.0.0.1:1/v1/messages
    api_key: sk-beta-key
    priority: 2
    enabled: true
    fallback_model: claude-3-5-sonnet
    model_mapping: {}
    timeout_ms: 60000
    custom_headers: {}
    strip_thinking: false
    retry_count: 0
    retry_delay_ms: 500
    auto_cache: true
failover:
  max_failover_channels: 2
  retry_timeout_ms: 3000
  circuit_breaker:
    failure_threshold: 2
    recovery_interval_sec: 15
    probe_requests: 1
auth:
  proxy_tokens:
    - proxy-tok
  admin_token: null
model_prices:
  - model: gpt-4o
    input_per_mtok: 2.5
    output_per_mtok: 10
    cache_read_per_mtok: 1.25
    cache_write_per_mtok: 3.75
    enabled: true
mcp_servers:
  - id: mcp-local
    name: Local MCP
    endpoint: http://127.0.0.1:9/mcp
    auth_token: mcp-tok
    custom_headers:
      X-MCP: "1"
    blocked_tools:
      - danger
    enabled: true
    oauth_enabled: false
`

// ---------- 测试环境 ----------

type testEnv struct {
	t          *testing.T
	dir        string
	configPath string
	state      *app.State
	mux        *http.ServeMux
	db         *audit.DB
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(testConfigYAML), 0o644); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	cfg, err := config.LoadFromFile(configPath)
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("测试配置应有两个渠道，实际 %d", len(cfg.Channels))
	}
	db, err := audit.Open(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatalf("打开审计库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	state := app.New(cfg, configPath, db)
	mux := http.NewServeMux()
	api.Register(mux, state, mcp.NewState(state, state.HTTP, state.Audit))
	return &testEnv{t: t, dir: dir, configPath: configPath, state: state, mux: mux, db: db}
}

// do 发起一次真实 HTTP 请求（走真实注册的 mux）。
// body 为 nil 表示无请求体；string 原样发送；其它类型 JSON 编码。
func (e *testEnv) do(method, target string, body any, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			reader = strings.NewReader(b)
		case []byte:
			reader = bytes.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				e.t.Fatalf("请求体编码失败: %v", err)
			}
			reader = bytes.NewReader(raw)
		}
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// getJSON 发 GET 并断言 200 + JSON，返回解码后的对象。
func (e *testEnv) getJSON(target string, headers map[string]string) map[string]any {
	e.t.Helper()
	rec := e.do(http.MethodGet, target, nil, headers)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET %s 期望 200，实际 %d，body=%s", target, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		e.t.Fatalf("GET %s Content-Type=%q，期望 application/json", target, ct)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("GET %s 响应不是 JSON 对象: %v（body=%s）", target, err, rec.Body.String())
	}
	return out
}

// getJSONArray 发 GET 并断言 200 + JSON 数组。
func (e *testEnv) getJSONArray(target string, headers map[string]string) []map[string]any {
	e.t.Helper()
	rec := e.do(http.MethodGet, target, nil, headers)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET %s 期望 200，实际 %d，body=%s", target, rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("GET %s 响应不是 JSON 数组: %v（body=%s）", target, err, rec.Body.String())
	}
	return out
}

// postJSON 发 POST 并断言 200 + JSON 对象。
func (e *testEnv) postJSON(target string, body any, headers map[string]string) map[string]any {
	e.t.Helper()
	rec := e.do(http.MethodPost, target, body, headers)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("POST %s 期望 200，实际 %d，body=%s", target, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("POST %s 响应不是 JSON 对象: %v（body=%s）", target, err, rec.Body.String())
	}
	return out
}

// diskYAML 读回磁盘上的 config.yaml。
func (e *testEnv) diskYAML() map[string]any {
	e.t.Helper()
	raw, err := os.ReadFile(e.configPath)
	if err != nil {
		e.t.Fatalf("读取磁盘配置失败: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("磁盘配置不是合法 YAML: %v", err)
	}
	return out
}

// diskText 读取磁盘配置原文（用于断言字段名/字段存在性）。
func (e *testEnv) diskText() string {
	e.t.Helper()
	raw, err := os.ReadFile(e.configPath)
	if err != nil {
		e.t.Fatalf("读取磁盘配置失败: %v", err)
	}
	return string(raw)
}

// keysOf 返回对象的键集合（排序后），用于断言 JSON 字段名逐字一致。
func keysOf(t *testing.T, obj map[string]any) []string {
	t.Helper()
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, obj map[string]any, want ...string) {
	t.Helper()
	got := keysOf(t, obj)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("字段集合不匹配\n实际: %v\n期望: %v", got, want)
	}
}

func findChannel(t *testing.T, cfg map[string]any, id string) map[string]any {
	t.Helper()
	channels, _ := cfg["channels"].([]any)
	for _, c := range channels {
		m, ok := c.(map[string]any)
		if ok && m["id"] == id {
			return m
		}
	}
	return nil
}

func strPtr(s string) *string   { return &s }
func i64Ptr(v int64) *int64     { return &v }
func f64Ptr(v float64) *float64 { return &v }

// seedAudit 往审计库写入 3 条真实记录，覆盖成功/失败/未完成三种形态。
//   - id1：ch-alpha 成功 200，latency=first_byte=120，token 1000/500/200/100，cost 0.0125
//   - id2：ch-beta 失败 502（UpdatetimeError），latency=3000
//   - id3：ch-alpha 未完成（status_code 为 NULL）
func seedAudit(t *testing.T, e *testEnv) (id1, id2, id3 int64) {
	t.Helper()
	reqHeaders := `{"content-type":"application/json"}`
	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

	var err error
	id1, err = e.db.Create("POST", "/v1/chat/completions", "gpt-4o", &reqHeaders, &reqBody, strPtr("test-agent"))
	if err != nil {
		t.Fatalf("插入审计记录 1 失败: %v", err)
	}
	if err := e.db.UpdateRoute(id1, "ch-alpha", "gpt-4o-2024-08-06", "explicit"); err != nil {
		t.Fatalf("写入路由信息失败: %v", err)
	}
	if err := e.db.UpdateResponse(id1, 200, 120, strPtr(`{"server":"up"}`), strPtr(`{"ok":true}`),
		strPtr(`{"server":"bridge"}`), strPtr(`{"ok":true}`),
		audit.TokenUsage{Input: i64Ptr(1000), Output: i64Ptr(500), CacheRead: i64Ptr(200), CacheCreation: i64Ptr(100)},
		f64Ptr(0.0125)); err != nil {
		t.Fatalf("写入响应信息失败: %v", err)
	}

	id2, err = e.db.Create("POST", "/v1/messages", "gpt-4o", &reqHeaders, &reqBody, strPtr("test-agent"))
	if err != nil {
		t.Fatalf("插入审计记录 2 失败: %v", err)
	}
	if err := e.db.UpdateRoute(id2, "ch-beta", "claude-3-5-sonnet", "fallback"); err != nil {
		t.Fatalf("写入路由信息失败: %v", err)
	}
	if err := e.db.UpdateError(id2, 502, 3000, "upstream 502 bad gateway"); err != nil {
		t.Fatalf("写入错误信息失败: %v", err)
	}

	id3, err = e.db.Create("POST", "/v1/responses", "gpt-4o-mini", &reqHeaders, &reqBody, strPtr("test-agent"))
	if err != nil {
		t.Fatalf("插入审计记录 3 失败: %v", err)
	}
	if err := e.db.UpdateRoute(id3, "ch-alpha", "gpt-4o-mini", "explicit"); err != nil {
		t.Fatalf("写入路由信息失败: %v", err)
	}
	return id1, id2, id3
}

// ---------- GET /api/config ----------

func TestGetFullConfigReturnsChannelsWithKeys(t *testing.T) {
	e := newTestEnv(t)
	rec := e.do(http.MethodGet, "/api/config", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	raw := rec.Body.String()
	// 真实序列化字节：API Key 必须在响应里（UI 的渠道编辑弹框依赖它）。
	for _, want := range []string{`"api_key":"sk-alpha-key"`, `"api_key":"sk-beta-key"`, `ch-alpha`, `"mcp_servers"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("响应缺少 %s：%s", want, raw)
		}
	}

	cfg := e.getJSON("/api/config", nil)
	assertKeys(t, cfg, "listen_port", "listen_host", "public_url", "models", "channels",
		"failover", "auth", "ultimate_fallback_channel", "mcp_servers", "audit_retention_days")

	if cfg["listen_port"].(float64) != 18080 {
		t.Fatalf("listen_port 期望 18080，实际 %v", cfg["listen_port"])
	}
	if cfg["ultimate_fallback_channel"] != nil {
		t.Fatalf("ultimate_fallback_channel 期望 null，实际 %v", cfg["ultimate_fallback_channel"])
	}
	if cfg["audit_retention_days"].(float64) != 0 {
		t.Fatalf("audit_retention_days 期望 0，实际 %v", cfg["audit_retention_days"])
	}

	alpha := findChannel(t, cfg, "ch-alpha")
	if alpha == nil {
		t.Fatal("响应里没有 ch-alpha")
	}
	assertKeys(t, alpha, "id", "name", "provider", "url", "api_key", "priority", "enabled",
		"fallback_model", "model_mapping", "timeout_ms", "custom_headers", "strip_thinking",
		"retry_count", "retry_delay_ms", "force_effort", "auto_cache")
	if alpha["api_key"] != "sk-alpha-key" || alpha["url"] != "http://127.0.0.1:1/v1/chat/completions" {
		t.Fatalf("ch-alpha 字段不正确: %v", alpha)
	}
	if alpha["force_effort"] != "high" || alpha["auto_cache"] != false || alpha["retry_delay_ms"].(float64) != 250 {
		t.Fatalf("ch-alpha 可选字段不正确: %v", alpha)
	}
	if mm, ok := alpha["model_mapping"].(map[string]any); !ok || mm["gpt-4o"] != "gpt-4o-2024-08-06" {
		t.Fatalf("model_mapping 不正确: %v", alpha["model_mapping"])
	}

}

// ---------- GET /api/channels 与 /api/channels/health ----------

func TestGetChannelsAndHealth(t *testing.T) {
	e := newTestEnv(t)

	channels := e.getJSONArray("/api/channels", nil)
	if len(channels) != 2 {
		t.Fatalf("期望 2 个渠道，实际 %d", len(channels))
	}
	assertKeys(t, channels[0], "id", "name", "provider", "url", "priority", "enabled",
		"fallback_model", "model_mapping")
	if channels[0]["id"] != "ch-alpha" || channels[0]["provider"] != "openai" {
		t.Fatalf("渠道概要字段不正确: %v", channels[0])
	}
	if _, leaked := channels[0]["api_key"]; leaked {
		t.Fatalf("/api/channels 不应包含 api_key: %v", channels[0])
	}

	// 未写入任何审计数据时：统计为 0，均值/成功率为 null。
	health := e.getJSONArray("/api/channels/health", nil)
	if len(health) != 2 {
		t.Fatalf("期望 2 条健康信息，实际 %d", len(health))
	}
	assertKeys(t, health[0], "id", "name", "state", "avg_first_byte_ms", "avg_latency_ms",
		"success_rate", "total_requests", "recent_failures", "input_tokens", "output_tokens",
		"cache_read_tokens", "cache_creation_tokens")
	if health[0]["state"] != "healthy" || health[0]["total_requests"].(float64) != 0 {
		t.Fatalf("初始健康状态不正确: %v", health[0])
	}
	if health[0]["avg_first_byte_ms"] != nil || health[0]["success_rate"] != nil {
		t.Fatalf("无样本时均值应为 null: %v", health[0])
	}

	// 写入真实审计数据后重查：渠道维度统计必须来自 DB。
	seedAudit(t, e)
	health = e.getJSONArray("/api/channels/health", nil)
	byID := map[string]map[string]any{}
	for _, h := range health {
		byID[h["id"].(string)] = h
	}
	a := byID["ch-alpha"]
	if a == nil {
		t.Fatalf("健康列表缺少 ch-alpha: %v", health)
	}
	if a["total_requests"].(float64) != 2 || a["avg_latency_ms"].(float64) != 120 ||
		a["avg_first_byte_ms"].(float64) != 120 || a["success_rate"].(float64) != 100 ||
		a["input_tokens"].(float64) != 1000 || a["output_tokens"].(float64) != 500 ||
		a["cache_read_tokens"].(float64) != 200 || a["cache_creation_tokens"].(float64) != 100 ||
		a["recent_failures"].(float64) != 0 {
		t.Fatalf("ch-alpha 统计不正确: %v", a)
	}
	b := byID["ch-beta"]
	if b["total_requests"].(float64) != 1 || b["recent_failures"].(float64) != 1 ||
		b["success_rate"].(float64) != 0 || b["avg_latency_ms"].(float64) != 3000 {
		t.Fatalf("ch-beta 统计不正确: %v", b)
	}
	if b["avg_first_byte_ms"] != nil {
		t.Fatalf("ch-beta 无 first_byte 样本，应为 null: %v", b)
	}
}

// ---------- POST /api/config/failover ----------

func TestSaveFailoverPersistsToDisk(t *testing.T) {
	e := newTestEnv(t)

	// 用旧字段名 max_retries 提交，验证别名兼容。
	resp := e.postJSON("/api/config/failover", map[string]any{
		"max_retries":           7,
		"retry_timeout_ms":      9000,
		"failure_threshold":     5,
		"recovery_interval_sec": 60,
		"probe_requests":        3,
	}, nil)
	if resp["success"] != true {
		t.Fatalf("保存失败: %v", resp)
	}

	cfg := e.getJSON("/api/config", nil)
	fo := cfg["failover"].(map[string]any)
	assertKeys(t, fo, "max_failover_channels", "retry_timeout_ms", "failure_threshold",
		"recovery_interval_sec", "probe_requests")
	if fo["max_failover_channels"].(float64) != 7 || fo["retry_timeout_ms"].(float64) != 9000 ||
		fo["failure_threshold"].(float64) != 5 || fo["recovery_interval_sec"].(float64) != 60 ||
		fo["probe_requests"].(float64) != 3 {
		t.Fatalf("failover 未按提交值更新: %v", fo)
	}

	// 磁盘文件必须真的变了（并且写的是新字段名）。
	disk := e.diskYAML()
	dfo := disk["failover"].(map[string]any)
	if dfo["max_failover_channels"] != 7 {
		t.Fatalf("磁盘 failover.max_failover_channels 期望 7，实际 %v", dfo["max_failover_channels"])
	}
	cb := dfo["circuit_breaker"].(map[string]any)
	if cb["failure_threshold"] != 5 || cb["recovery_interval_sec"] != 60 || cb["probe_requests"] != 3 {
		t.Fatalf("磁盘熔断配置未更新: %v", cb)
	}
	if strings.Contains(e.diskText(), "max_retries:") {
		t.Fatalf("磁盘配置不应写入旧字段名 max_retries:\n%s", e.diskText())
	}

	// 缺字段时拒绝（Rust 的 serde 同样拒绝），且不污染已有配置。
	rec := e.do(http.MethodPost, "/api/config/failover", map[string]any{"max_failover_channels": 1}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺字段期望 400，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "retry_timeout_ms") {
		t.Fatalf("错误信息应指明缺失字段: %s", rec.Body.String())
	}
	again := e.getJSON("/api/config", nil)["failover"].(map[string]any)
	if again["max_failover_channels"].(float64) != 7 {
		t.Fatalf("失败请求不应改动配置: %v", again)
	}
}

// ---------- POST /api/config/channel（upsert）与 delete ----------

func TestChannelUpsertAndDelete(t *testing.T) {
	e := newTestEnv(t)

	newChannel := map[string]any{
		"id": "ch-new", "name": "New", "provider": "openai_responses",
		"url": "http://127.0.0.1:1/v1/responses", "api_key": "sk-new",
		"priority": 3, "enabled": true, "fallback_model": "gpt-4o-mini",
		"model_mapping": map[string]string{"gpt-4o": "gpt-4o-mini"},
		"timeout_ms":    45000, "custom_headers": map[string]string{"X-New": "1"},
		"strip_thinking": false, "retry_count": 1, "retry_delay_ms": 750,
		"force_effort": "low", "auto_cache": true,
	}
	if resp := e.postJSON("/api/config/channel", newChannel, nil); resp["success"] != true {
		t.Fatalf("新增渠道失败: %v", resp)
	}
	cfg := e.getJSON("/api/config", nil)
	if len(cfg["channels"].([]any)) != 3 {
		t.Fatalf("新增后应有 3 个渠道: %v", cfg["channels"])
	}
	created := findChannel(t, cfg, "ch-new")
	if created["api_key"] != "sk-new" || created["provider"] != "openai_responses" ||
		created["retry_delay_ms"].(float64) != 750 || created["force_effort"] != "low" {
		t.Fatalf("新增渠道字段不正确: %v", created)
	}
	if !strings.Contains(e.diskText(), "ch-new") {
		t.Fatalf("磁盘配置未包含新渠道:\n%s", e.diskText())
	}

	// 同 id 再存一次 = 更新（而不是追加）。
	updated := map[string]any{}
	for k, v := range newChannel {
		updated[k] = v
	}
	updated["name"] = "New Renamed"
	updated["api_key"] = "sk-new-2"
	updated["priority"] = 9
	if resp := e.postJSON("/api/config/channel", updated, nil); resp["success"] != true {
		t.Fatalf("更新渠道失败: %v", resp)
	}
	cfg = e.getJSON("/api/config", nil)
	if len(cfg["channels"].([]any)) != 3 {
		t.Fatalf("更新不应改变渠道数量: %v", cfg["channels"])
	}
	created = findChannel(t, cfg, "ch-new")
	if created["name"] != "New Renamed" || created["api_key"] != "sk-new-2" || created["priority"].(float64) != 9 {
		t.Fatalf("渠道未被更新: %v", created)
	}
	diskChannels := e.diskYAML()["channels"].([]any)
	var diskNew map[string]any
	for _, c := range diskChannels {
		m := c.(map[string]any)
		if m["id"] == "ch-new" {
			diskNew = m
		}
	}
	if diskNew == nil || diskNew["api_key"] != "sk-new-2" || diskNew["name"] != "New Renamed" {
		t.Fatalf("磁盘配置未反映更新: %v", diskNew)
	}

	// 健康表：新渠道应在 /api/channels/health 里出现（按优先级排序，priority 9 在最后）。
	health := e.getJSONArray("/api/channels/health", nil)
	if len(health) != 3 || health[2]["id"] != "ch-new" {
		t.Fatalf("健康表未包含新渠道: %v", health)
	}

	// 未知 provider：按 Rust parse_provider 的报错文案返回。
	bad := map[string]any{}
	for k, v := range newChannel {
		bad[k] = v
	}
	bad["provider"] = "bogus"
	rec := e.do(http.MethodPost, "/api/config/channel", bad, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Unknown provider: bogus") {
		t.Fatalf("未知 provider 期望 200 + Unknown provider: bogus，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 删除。
	if resp := e.postJSON("/api/config/channel/delete", map[string]any{"channelId": "ch-new"}, nil); resp["success"] != true {
		t.Fatalf("删除渠道失败: %v", resp)
	}
	cfg = e.getJSON("/api/config", nil)
	if len(cfg["channels"].([]any)) != 2 || findChannel(t, cfg, "ch-new") != nil {
		t.Fatalf("删除后仍有 ch-new: %v", cfg["channels"])
	}
	if strings.Contains(e.diskText(), "ch-new") {
		t.Fatalf("磁盘配置仍包含已删除渠道:\n%s", e.diskText())
	}
	health = e.getJSONArray("/api/channels/health", nil)
	if len(health) != 2 {
		t.Fatalf("删除渠道后健康表未同步: %v", health)
	}
	for _, h := range health {
		if h["id"] == "ch-new" {
			t.Fatalf("健康表仍保留已删除渠道: %v", h)
		}
	}
}

// ---------- 模型价格 CRUD ----------

func TestModelPricesCRUD(t *testing.T) {
	e := newTestEnv(t)

	prices := e.getJSONArray("/api/model-prices", nil)
	if len(prices) != 1 {
		t.Fatalf("期望 1 条价格，实际 %d", len(prices))
	}
	assertKeys(t, prices[0], "model", "input_per_mtok", "output_per_mtok",
		"cache_read_per_mtok", "cache_write_per_mtok", "enabled")
	if prices[0]["model"] != "gpt-4o" || prices[0]["cache_read_per_mtok"].(float64) != 1.25 {
		t.Fatalf("价格字段不正确: %v", prices[0])
	}

	// 新增：不传 cache_* 与 enabled，应落成 null/null/true。
	if resp := e.postJSON("/api/model-prices", map[string]any{
		"model": "gpt-4o-mini", "input_per_mtok": 0.15, "output_per_mtok": 0.6,
	}, nil); resp["success"] != true {
		t.Fatalf("新增价格失败: %v", resp)
	}
	raw := e.do(http.MethodGet, "/api/model-prices", nil, nil).Body.String()
	if !strings.Contains(raw, `"cache_read_per_mtok":null`) {
		t.Fatalf("未配置的缓存价格应序列化成 null: %s", raw)
	}
	prices = e.getJSONArray("/api/model-prices", nil)
	if len(prices) != 2 {
		t.Fatalf("新增后应有 2 条价格: %v", prices)
	}
	mini := prices[1]
	if mini["model"] != "gpt-4o-mini" || mini["enabled"] != true ||
		mini["cache_read_per_mtok"] != nil || mini["cache_write_per_mtok"] != nil ||
		mini["input_per_mtok"].(float64) != 0.15 {
		t.Fatalf("新增价格字段不正确: %v", mini)
	}

	// 同模型再存 = 更新。
	if resp := e.postJSON("/api/model-prices", map[string]any{
		"model": "gpt-4o-mini", "input_per_mtok": 0.2, "output_per_mtok": 0.8,
		"cache_read_per_mtok": 0.05, "enabled": false,
	}, nil); resp["success"] != true {
		t.Fatalf("更新价格失败: %v", resp)
	}
	prices = e.getJSONArray("/api/model-prices", nil)
	if len(prices) != 2 {
		t.Fatalf("更新不应新增条目: %v", prices)
	}
	mini = prices[1]
	if mini["input_per_mtok"].(float64) != 0.2 || mini["enabled"] != false ||
		mini["cache_read_per_mtok"].(float64) != 0.05 {
		t.Fatalf("价格未被更新: %v", mini)
	}
	diskPrices := e.diskYAML()["model_prices"].([]any)
	if len(diskPrices) != 2 {
		t.Fatalf("磁盘价格条目数不正确: %v", diskPrices)
	}

	// 空模型名：Rust 返回 {"error":"模型名不能为空"}。
	rec := e.do(http.MethodPost, "/api/model-prices", map[string]any{"model": "   "}, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "模型名不能为空") {
		t.Fatalf("空模型名期望 200 + 模型名不能为空，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 删除。
	if resp := e.postJSON("/api/model-prices/delete", map[string]any{"model": "gpt-4o-mini"}, nil); resp["success"] != true {
		t.Fatalf("删除价格失败: %v", resp)
	}
	prices = e.getJSONArray("/api/model-prices", nil)
	if len(prices) != 1 || prices[0]["model"] != "gpt-4o" {
		t.Fatalf("删除后价格列表不正确: %v", prices)
	}
	if len(e.diskYAML()["model_prices"].([]any)) != 1 {
		t.Fatalf("磁盘未反映删除: %s", e.diskText())
	}
}

// ---------- 审计：列表过滤 / 详情 / 库概况 / 清理 ----------

func TestAuditListFiltersAndDetail(t *testing.T) {
	e := newTestEnv(t)
	id1, id2, id3 := seedAudit(t, e)

	all := e.getJSONArray("/api/audit", nil)
	if len(all) != 3 {
		t.Fatalf("期望 3 条审计记录，实际 %d", len(all))
	}
	assertKeys(t, all[0], "id", "timestamp", "method", "path", "alias_model", "actual_channel",
		"actual_model", "mapping_source", "status_code", "first_byte_ms", "latency_ms",
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens",
		"cost_usd", "retry_count", "error_message")
	// 默认按 id DESC。
	if all[0]["id"].(float64) != float64(id3) || all[2]["id"].(float64) != float64(id1) {
		t.Fatalf("列表顺序不是 id DESC: %v", all)
	}
	if all[2]["cost_usd"].(float64) != 0.0125 || all[2]["input_tokens"].(float64) != 1000 {
		t.Fatalf("成功记录的 token/成本不正确: %v", all[2])
	}

	ids := func(rows []map[string]any) []float64 {
		out := make([]float64, 0, len(rows))
		for _, r := range rows {
			out = append(out, r["id"].(float64))
		}
		return out
	}

	cases := []struct {
		name  string
		query string
		want  []float64
	}{
		{"model_filter", "?model_filter=gpt-4o", []float64{float64(id2), float64(id1)}},
		{"channel_filter", "?channel_filter=ch-beta", []float64{float64(id2)}},
		{"channel_filter_alpha", "?channel_filter=ch-alpha", []float64{float64(id3), float64(id1)}},
		{"status_filter_success", "?status_filter=success", []float64{float64(id1)}},
		{"status_filter_error", "?status_filter=error", []float64{float64(id2)}},
		{"status_filter_code", "?status_filter=200", []float64{float64(id1)}},
		{"actual_model_filter", "?actual_model_filter=claude-3-5-sonnet", []float64{float64(id2)}},
		{"path_filter", "?path_filter=/v1/responses", []float64{float64(id3)}},
		{"limit", "?limit=1", []float64{float64(id3)}},
		{"time_from_epoch", fmt.Sprintf("?time_from=%d", time.Now().Add(-time.Hour).UnixMilli()), []float64{float64(id3), float64(id2), float64(id1)}},
		{"time_to_past", fmt.Sprintf("?time_to=%d", time.Now().Add(-time.Hour).UnixMilli()), nil},
		{"combined", "?model_filter=gpt-4o&channel_filter=ch-alpha&status_filter=200", []float64{float64(id1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := e.getJSONArray("/api/audit"+tc.query, nil)
			got := ids(rows)
			sort.Float64s(got)
			want := append([]float64(nil), tc.want...)
			sort.Float64s(want)
			if len(got) != len(want) {
				t.Fatalf("过滤 %s 期望 %v 条，实际 %v（rows=%v）", tc.query, want, got, rows)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("过滤 %s 结果不匹配：实际 %v 期望 %v", tc.query, got, want)
				}
			}
		})
	}

	// RFC3339 形式的时间边界（Rust 的 parse 同样接受）。
	rfc := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if rows := e.getJSONArray("/api/audit?time_from="+rfc, nil); len(rows) != 3 {
		t.Fatalf("RFC3339 time_from 过滤应返回 3 条，实际 %d", len(rows))
	}

	// 详情：存在 → 完整报文；不存在 → {"error":"Not found"}（HTTP 200）。
	detail := e.getJSON("/api/audit/detail?id="+fmt.Sprint(id1), nil)
	assertKeys(t, detail, "id", "timestamp", "method", "path", "alias_model", "actual_channel",
		"actual_model", "mapping_source", "status_code", "first_byte_ms", "latency_ms",
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens",
		"cost_usd", "retry_count", "error_message", "request_headers", "forwarded_request_headers",
		"request_body", "forwarded_request_body", "upstream_response_headers", "response_headers",
		"upstream_response_body", "response_body", "failover_chain", "user_agent")
	if detail["request_body"] != `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}` ||
		detail["user_agent"] != "test-agent" || detail["response_body"] != `{"ok":true}` {
		t.Fatalf("详情字段不正确: %v", detail)
	}

	missing := e.do(http.MethodGet, "/api/audit/detail?id=99999", nil, nil)
	if missing.Code != http.StatusOK || !strings.Contains(missing.Body.String(), `"error":"Not found"`) {
		t.Fatalf("不存在 id 期望 200 + Not found，实际 %d %s", missing.Code, missing.Body.String())
	}
	badID := e.do(http.MethodGet, "/api/audit/detail?id=abc", nil, nil)
	if badID.Code != http.StatusBadRequest {
		t.Fatalf("非法 id 期望 400，实际 %d %s", badID.Code, badID.Body.String())
	}
}

func TestAuditDBStatusStatsHeatmapAndCleanup(t *testing.T) {
	e := newTestEnv(t)
	id1, _, _ := seedAudit(t, e)

	status := e.getJSON("/api/audit/db/status", nil)
	assertKeys(t, status, "size_bytes", "total_records", "detail_records")
	if status["total_records"].(float64) != 3 || status["detail_records"].(float64) != 3 {
		t.Fatalf("库概况不正确: %v", status)
	}
	if status["size_bytes"].(float64) <= 0 {
		t.Fatalf("size_bytes 应为正数: %v", status)
	}

	stats := e.getJSON("/api/stats", nil)
	assertKeys(t, stats, "total_requests", "input_tokens", "output_tokens", "cache_read_tokens",
		"cache_creation_tokens", "error_count", "success_count", "cost_usd")
	if stats["total_requests"].(float64) != 3 || stats["success_count"].(float64) != 1 ||
		stats["error_count"].(float64) != 1 || stats["input_tokens"].(float64) != 1000 ||
		stats["output_tokens"].(float64) != 500 || stats["cache_read_tokens"].(float64) != 200 ||
		stats["cache_creation_tokens"].(float64) != 100 || stats["cost_usd"].(float64) != 0.0125 {
		t.Fatalf("统计不正确: %v", stats)
	}

	scoped := e.getJSON("/api/stats?period=7d&channel=ch-beta", nil)
	if scoped["total_requests"].(float64) != 1 || scoped["error_count"].(float64) != 1 {
		t.Fatalf("按渠道过滤的统计不正确: %v", scoped)
	}

	heat := e.getJSONArray("/api/stats/heatmap?days=7", nil)
	if len(heat) != 1 {
		t.Fatalf("热力图应有 1 天数据，实际 %v", heat)
	}
	assertKeys(t, heat[0], "date", "tokens")
	if heat[0]["date"] != time.Now().Format("2006-01-02") || heat[0]["tokens"].(float64) != 1800 {
		t.Fatalf("热力图数据不正确: %v", heat[0])
	}
	if rows := e.getJSONArray("/api/stats/heatmap", nil); len(rows) != 1 {
		t.Fatalf("热力图缺省 days=91 应返回 1 天，实际 %v", rows)
	}

	cleanup := e.postJSON("/api/audit/cleanup", nil, nil)
	assertKeys(t, cleanup, "deleted_old", "pruned", "size_before", "size_after")
	if cleanup["deleted_old"].(float64) != 0 || cleanup["pruned"].(float64) != 0 {
		t.Fatalf("retention=0 + 少于 1000 条时不应删除任何数据: %v", cleanup)
	}
	if cleanup["size_before"].(float64) <= 0 || cleanup["size_after"].(float64) <= 0 {
		t.Fatalf("清理结果应包含库大小: %v", cleanup)
	}
	// 清理后列表仍然可用，记录未被误删。
	if rows := e.getJSONArray("/api/audit", nil); len(rows) != 3 {
		t.Fatalf("清理后记录数变化: %v", rows)
	}
	detail := e.getJSON("/api/audit/detail?id="+fmt.Sprint(id1), nil)
	if detail["id"].(float64) != float64(id1) {
		t.Fatalf("清理后详情不可读: %v", detail)
	}

	// 设置保留天数后，清理会真的按保留期删除（这里记录都是刚才写入的，不应被删）。
	if resp := e.postJSON("/api/config/settings", map[string]any{
		"listen_port": 18080, "listen_host": "127.0.0.1", "public_url": nil,
		"models": []string{"gpt-4o"}, "audit_retention_days": 30,
	}, nil); resp["success"] != true {
		t.Fatalf("保存设置失败: %v", resp)
	}
	if cfg := e.getJSON("/api/config", nil); cfg["audit_retention_days"].(float64) != 30 {
		t.Fatalf("audit_retention_days 未持久化: %v", cfg["audit_retention_days"])
	}
	cleanup = e.postJSON("/api/audit/cleanup", nil, nil)
	if cleanup["deleted_old"].(float64) != 0 {
		t.Fatalf("30 天保留期不应删除今天的记录: %v", cleanup)
	}
}

// ---------- POST /api/config/channel/test ----------

func TestTestChannelHitsUpstream(t *testing.T) {
	e := newTestEnv(t)

	type seenReq struct {
		path    string
		headers http.Header
		body    map[string]any
	}
	var mu sync.Mutex
	var seen []seenReq
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		seen = append(seen, seenReq{path: r.URL.Path, headers: r.Header.Clone(), body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pong":true}`))
	}))
	defer upstream.Close()

	// 1) Anthropic 渠道：x-api-key + anthropic-version + messages 报文，自定义头可覆盖。
	if resp := e.postJSON("/api/config/channel", map[string]any{
		"id": "ch-anth", "name": "Anth", "provider": "anthropic",
		"url": upstream.URL + "/v1/messages", "api_key": "sk-anth",
		"priority": 4, "enabled": true, "fallback_model": "claude-x",
		"model_mapping": map[string]string{}, "timeout_ms": 5000,
		"custom_headers": map[string]string{"x-custom": "yes"}, "strip_thinking": false,
		"retry_count": 0, "retry_delay_ms": 500, "auto_cache": true,
	}, nil); resp["success"] != true {
		t.Fatalf("新增渠道失败: %v", resp)
	}
	res := e.postJSON("/api/config/channel/test", map[string]any{"channelId": "ch-anth"}, nil)
	assertKeys(t, res, "success", "status_code", "latency_ms", "response_body")
	if res["success"] != true || res["status_code"].(float64) != 200 || res["response_body"] != `{"pong":true}` {
		t.Fatalf("渠道测试结果不正确: %v", res)
	}
	if _, ok := res["latency_ms"].(float64); !ok {
		t.Fatalf("latency_ms 应为数字: %v", res)
	}
	mu.Lock()
	got := seen[len(seen)-1]
	mu.Unlock()
	if got.path != "/v1/messages" || got.headers.Get("x-api-key") != "sk-anth" ||
		got.headers.Get("anthropic-version") != "2023-06-01" ||
		got.headers.Get("x-custom") != "yes" ||
		got.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("上游收到的头不正确: %v", got.headers)
	}
	if got.body["model"] != "claude-x" || got.body["max_tokens"].(float64) != 16 {
		t.Fatalf("上游收到的报文不正确: %v", got.body)
	}
	msgs, _ := got.body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "ping" {
		t.Fatalf("Anthropic 测试报文缺少 ping 消息: %v", got.body)
	}

	// 2) OpenAI（chat）渠道：Bearer 认证。
	if resp := e.postJSON("/api/config/channel", map[string]any{
		"id": "ch-oai", "name": "OAI", "provider": "openai",
		"url": upstream.URL + "/v1/chat/completions", "api_key": "sk-oai",
		"priority": 5, "enabled": true, "fallback_model": "gpt-x",
		"model_mapping": map[string]string{}, "timeout_ms": 5000,
		"custom_headers": map[string]string{}, "strip_thinking": false,
		"retry_count": 0, "retry_delay_ms": 500, "auto_cache": true,
	}, nil); resp["success"] != true {
		t.Fatalf("新增渠道失败: %v", resp)
	}
	if res := e.postJSON("/api/config/channel/test", map[string]any{"channelId": "ch-oai"}, nil); res["success"] != true {
		t.Fatalf("chat 渠道测试失败: %v", res)
	}
	mu.Lock()
	got = seen[len(seen)-1]
	mu.Unlock()
	if got.headers.Get("Authorization") != "Bearer sk-oai" || got.headers.Get("x-api-key") != "" {
		t.Fatalf("chat 渠道认证头不正确: %v", got.headers)
	}

	// 3) Responses 渠道：input 字段。
	if resp := e.postJSON("/api/config/channel", map[string]any{
		"id": "ch-resp", "name": "Resp", "provider": "openai_responses",
		"url": upstream.URL + "/v1/responses", "api_key": "sk-resp",
		"priority": 6, "enabled": true, "fallback_model": "gpt-r",
		"model_mapping": map[string]string{}, "timeout_ms": 5000,
		"custom_headers": map[string]string{}, "strip_thinking": false,
		"retry_count": 0, "retry_delay_ms": 500, "auto_cache": true,
	}, nil); resp["success"] != true {
		t.Fatalf("新增渠道失败: %v", resp)
	}
	if res := e.postJSON("/api/config/channel/test", map[string]any{"channelId": "ch-resp"}, nil); res["success"] != true {
		t.Fatalf("responses 渠道测试失败: %v", res)
	}
	mu.Lock()
	got = seen[len(seen)-1]
	mu.Unlock()
	if got.body["input"] != "ping" || got.body["model"] != "gpt-r" {
		t.Fatalf("Responses 测试报文不正确: %v", got.body)
	}

	// 4) 不存在的渠道 id。
	nf := e.postJSON("/api/config/channel/test", map[string]any{"channelId": "nope"}, nil)
	assertKeys(t, nf, "success", "error")
	if nf["success"] != false || nf["error"] != "Channel not found" {
		t.Fatalf("缺失渠道响应不正确: %v", nf)
	}

	// 5) 请求体不是合法 JSON：Rust 的 body:String + from_str 分支。
	rec := e.do(http.MethodPost, "/api/config/channel/test", "not-json", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"success":false`) ||
		!strings.Contains(rec.Body.String(), "Bad request:") {
		t.Fatalf("非法请求体响应不正确: %d %s", rec.Code, rec.Body.String())
	}

	// 6) 上游不可达：success=false + error + latency_ms。
	if resp := e.postJSON("/api/config/channel", map[string]any{
		"id": "ch-dead", "name": "Dead", "provider": "openai",
		"url": "http://127.0.0.1:1/v1/chat/completions", "api_key": "",
		"priority": 7, "enabled": true, "fallback_model": "gpt-d",
		"model_mapping": map[string]string{}, "timeout_ms": 5000,
		"custom_headers": map[string]string{}, "strip_thinking": false,
		"retry_count": 0, "retry_delay_ms": 500, "auto_cache": true,
	}, nil); resp["success"] != true {
		t.Fatalf("新增渠道失败: %v", resp)
	}
	dead := e.postJSON("/api/config/channel/test", map[string]any{"channelId": "ch-dead"}, nil)
	if dead["success"] != false || dead["error"] == "" || dead["error"] == nil || dead["latency_ms"] == nil {
		t.Fatalf("不可达渠道响应不正确: %v", dead)
	}
}

// ---------- 鉴权 ----------

func TestAdminAuthEnforcement(t *testing.T) {
	e := newTestEnv(t)

	// 未配置 admin_token：required=false 且所有接口免鉴权。
	status := e.getJSON("/api/auth/status", nil)
	assertKeys(t, status, "required", "valid")
	if status["required"] != false || status["valid"] != true {
		t.Fatalf("初始鉴权状态不正确: %v", status)
	}
	if rec := e.do(http.MethodGet, "/api/channels", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("未启用鉴权时应 200，实际 %d", rec.Code)
	}

	// 通过 save_settings 打开鉴权（此时还不需要 token）。
	if resp := e.postJSON("/api/config/settings", map[string]any{
		"listen_port": 18080,
		"listen_host": "127.0.0.1",
		"public_url":  "https://bridge.example.com",
		"models":      []string{"gpt-4o", "gpt-4o-mini"},
		"auth": map[string]any{
			"proxy_tokens": []string{"proxy-tok", "proxy-tok-2"},
			"admin_token":  "s3cret",
		},
		"ultimate_fallback_channel": "ch-alpha",
		"audit_retention_days":      7,
	}, nil); resp["success"] != true {
		t.Fatalf("保存设置失败: %v", resp)
	}

	// 磁盘上确实写入了。
	disk := e.diskYAML()
	if disk["public_url"] != "https://bridge.example.com" {
		t.Fatalf("public_url 未落盘: %v", disk["public_url"])
	}
	if uf, ok := disk["ultimate_fallback"].(map[string]any); !ok || uf["channel"] != "ch-alpha" {
		t.Fatalf("ultimate_fallback 未落盘: %v", disk["ultimate_fallback"])
	}
	if rt, ok := disk["audit_retention_days"].(int); !ok || rt != 7 {
		t.Fatalf("audit_retention_days 未落盘: %v(%T)", disk["audit_retention_days"], disk["audit_retention_days"])
	}
	authBlock := disk["auth"].(map[string]any)
	tokens := authBlock["proxy_tokens"].([]any)
	if len(tokens) != 2 || tokens[1] != "proxy-tok-2" || authBlock["admin_token"] != "s3cret" {
		t.Fatalf("auth 未落盘: %v", authBlock)
	}

	// 无 header：401 + auth_error 包装体。
	rec := e.do(http.MethodGet, "/api/channels", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺 token 期望 401，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("401 响应不是 JSON: %v（body=%s）", err, rec.Body.String())
	}
	inner, ok := errBody["error"].(map[string]any)
	if !ok || inner["type"] != "auth_error" || inner["message"] != "Invalid or missing admin token" {
		t.Fatalf("401 错误体不正确: %s", rec.Body.String())
	}

	// 错误 token / 非 Bearer 形式同样 401。
	for _, h := range []map[string]string{
		{"Authorization": "Bearer wrong"},
		{"Authorization": "s3cret"},
		{"Authorization": "Bearer "},
	} {
		if rec := e.do(http.MethodGet, "/api/channels", nil, h); rec.Code != http.StatusUnauthorized {
			t.Fatalf("错误 token 期望 401，实际 %d（header=%v）", rec.Code, h)
		}
	}

	// 正确 token：GET 与写接口都放行。
	okHdr := map[string]string{"Authorization": "Bearer s3cret"}
	if rec := e.do(http.MethodGet, "/api/channels", nil, okHdr); rec.Code != http.StatusOK {
		t.Fatalf("正确 token 期望 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	status = e.getJSON("/api/auth/status", nil)
	if status["required"] != true || status["valid"] != false {
		t.Fatalf("auth/status（无 token）不正确: %v", status)
	}
	status = e.getJSON("/api/auth/status", okHdr)
	if status["required"] != true || status["valid"] != true {
		t.Fatalf("auth/status（有 token）不正确: %v", status)
	}
	write := e.postJSON("/api/config/failover", map[string]any{
		"max_failover_channels": 3, "retry_timeout_ms": 5000, "failure_threshold": 3,
		"recovery_interval_sec": 30, "probe_requests": 2,
	}, okHdr)
	if write["success"] != true {
		t.Fatalf("带 token 的写接口应成功: %v", write)
	}

	// 401 的接口在无 token 时不得落盘/改配置：用一个明确的写请求验证。
	rec = e.do(http.MethodPost, "/api/config/failover", map[string]any{
		"max_failover_channels": 99, "retry_timeout_ms": 1, "failure_threshold": 1,
		"recovery_interval_sec": 1, "probe_requests": 1,
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 的写请求期望 401，实际 %d", rec.Code)
	}
	fo := e.getJSON("/api/config", okHdr)["failover"].(map[string]any)
	if fo["max_failover_channels"].(float64) != 3 {
		t.Fatalf("被拒绝的写请求改动了配置: %v", fo)
	}
}

// ---------- 请求体上限 ----------

// zerosReader 生成 size 个 'a'（构造超限请求体时不占用同样大的内存）。
type zerosReader struct {
	remaining int64
}

func (z *zerosReader) Read(p []byte) (int, error) {
	if z.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > z.remaining {
		n = z.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'a'
	}
	z.remaining -= n
	return int(n), nil
}

func TestBodySizeLimitReturns413(t *testing.T) {
	e := newTestEnv(t)

	// 64MB + 1 字节：超过上限。
	req := httptest.NewRequest(http.MethodPost, "/api/config/channel",
		&zerosReader{remaining: (64 << 20) + 1})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求体期望 413，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("413 响应不是 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if _, ok := body["error"]; !ok {
		t.Fatalf("413 响应缺少 error 字段: %v", body)
	}
}

// ---------- MCP server 配置与工具开关 ----------

func TestMCPServerSaveToggleAndOAuthStatus(t *testing.T) {
	e := newTestEnv(t)

	cfg := e.getJSON("/api/config", nil)
	servers := cfg["mcp_servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("期望 1 个 MCP server，实际 %v", servers)
	}
	mcpOne := servers[0].(map[string]any)
	assertKeys(t, mcpOne, "id", "name", "endpoint", "auth_token", "custom_headers",
		"blocked_tools", "enabled", "oauth_enabled", "cached_tools")
	if mcpOne["auth_token"] != "mcp-tok" || mcpOne["id"] != "mcp-local" {
		t.Fatalf("MCP server 字段不正确: %v", mcpOne)
	}

	// 新增一个 server（auth_token 传空串 → 按 Rust 的 filter 落成 null）。
	if resp := e.postJSON("/api/config/mcp", map[string]any{
		"id": "mcp-2", "name": "Second", "endpoint": "http://127.0.0.1:9/mcp2",
		"auth_token": "", "custom_headers": map[string]string{}, "blocked_tools": []string{},
		"enabled": false, "oauth_enabled": true,
	}, nil); resp["success"] != true {
		t.Fatalf("保存 MCP server 失败: %v", resp)
	}
	cfg = e.getJSON("/api/config", nil)
	servers = cfg["mcp_servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("应有 2 个 MCP server，实际 %v", servers)
	}
	second := servers[1].(map[string]any)
	if second["auth_token"] != nil || second["enabled"] != false || second["oauth_enabled"] != true {
		t.Fatalf("第二个 MCP server 字段不正确: %v", second)
	}
	if len(second["cached_tools"].([]any)) != 0 {
		t.Fatalf("新 server 的 cached_tools 应为空数组: %v", second["cached_tools"])
	}

	// 更新同一个 id：不新增条目，且保留 oauth（由 config 里的 oauth 字段决定，这里只验证数量与字段）。
	if resp := e.postJSON("/api/config/mcp", map[string]any{
		"id": "mcp-2", "name": "Second Renamed", "endpoint": "http://127.0.0.1:9/mcp2",
		"auth_token": "tok-2", "custom_headers": map[string]string{"A": "b"},
		"blocked_tools": []string{"x"}, "enabled": true, "oauth_enabled": true,
	}, nil); resp["success"] != true {
		t.Fatalf("更新 MCP server 失败: %v", resp)
	}
	cfg = e.getJSON("/api/config", nil)
	servers = cfg["mcp_servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("更新不应增加条目: %v", servers)
	}
	second = servers[1].(map[string]any)
	if second["name"] != "Second Renamed" || second["auth_token"] != "tok-2" || second["enabled"] != true {
		t.Fatalf("MCP server 未被更新: %v", second)
	}
	if !strings.Contains(e.diskText(), "Second Renamed") {
		t.Fatalf("磁盘配置未反映 MCP 更新:\n%s", e.diskText())
	}

	// 工具开关：禁用 → blocked_tools 增加；启用 → 移除。
	if resp := e.postJSON("/api/config/mcp/tools/toggle",
		map[string]any{"serverId": "mcp-local", "tool": "danger2", "enabled": false}, nil); resp["success"] != true {
		t.Fatalf("禁用工具失败: %v", resp)
	}
	blocked := e.getJSON("/api/config", nil)["mcp_servers"].([]any)[0].(map[string]any)["blocked_tools"].([]any)
	if len(blocked) != 2 || blocked[1] != "danger2" {
		t.Fatalf("blocked_tools 未更新: %v", blocked)
	}
	if resp := e.postJSON("/api/config/mcp/tools/toggle",
		map[string]any{"serverId": "mcp-local", "tool": "danger", "enabled": true}, nil); resp["success"] != true {
		t.Fatalf("启用工具失败: %v", resp)
	}
	blocked = e.getJSON("/api/config", nil)["mcp_servers"].([]any)[0].(map[string]any)["blocked_tools"].([]any)
	if len(blocked) != 1 || blocked[0] != "danger2" {
		t.Fatalf("blocked_tools 未按启用移除: %v", blocked)
	}
	// 不存在的 server。
	nf := e.postJSON("/api/config/mcp/tools/toggle",
		map[string]any{"serverId": "nope", "tool": "t", "enabled": true}, nil)
	if nf["error"] != "MCP server 不存在" {
		t.Fatalf("缺失 server 的响应不正确: %v", nf)
	}
	nf = e.postJSON("/api/config/mcp/tools/fetch", map[string]any{"serverId": "nope"}, nil)
	if nf["error"] != "MCP server 不存在" {
		t.Fatalf("fetch 缺失 server 的响应不正确: %v", nf)
	}

	// OAuth 状态：未授权。
	oauth := e.getJSON("/api/config/mcp/oauth/status?serverId=mcp-local", nil)
	assertKeys(t, oauth, "authorized", "expiresAt", "needsReauth")
	if oauth["authorized"] != false || oauth["needsReauth"] != true || oauth["expiresAt"] != nil {
		t.Fatalf("OAuth 状态不正确: %v", oauth)
	}

	// 删除 server。
	if resp := e.postJSON("/api/config/mcp/delete", map[string]any{"serverId": "mcp-2"}, nil); resp["success"] != true {
		t.Fatalf("删除 MCP server 失败: %v", resp)
	}
	servers = e.getJSON("/api/config", nil)["mcp_servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("删除后应剩 1 个 MCP server: %v", servers)
	}
	if strings.Contains(e.diskText(), "mcp-2") {
		t.Fatalf("磁盘配置仍包含已删除 server:\n%s", e.diskText())
	}
}

// TestRegisterCoversAllFrozenPaths 冻结路径清单：注册后每个端点都不应落到 404/405。
func TestRegisterCoversAllFrozenPaths(t *testing.T) {
	e := newTestEnv(t)
	getPaths := []string{
		"/api/auth/status", "/api/stats", "/api/stats/heatmap", "/api/audit",
		"/api/audit/detail?id=1", "/api/audit/db/status", "/api/channels",
		"/api/channels/health", "/api/config", "/api/config/mcp/oauth/status?serverId=mcp-local",
		"/api/model-prices",
	}
	for _, p := range getPaths {
		if rec := e.do(http.MethodGet, p, nil, nil); rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("GET %s 未注册（%d）", p, rec.Code)
		}
	}
	postPaths := map[string]any{
		"/api/audit/cleanup":           nil,
		"/api/config/channel":          map[string]any{},
		"/api/config/channel/delete":   map[string]any{"channelId": "nope"},
		"/api/config/failover":         map[string]any{},
		"/api/config/settings":         map[string]any{},
		"/api/config/channel/test":     map[string]any{"channelId": "nope"},
		"/api/config/mcp":              map[string]any{},
		"/api/config/mcp/delete":       map[string]any{"serverId": "nope"},
		"/api/config/mcp/oauth/start":  map[string]any{"serverId": "nope"},
		"/api/config/mcp/tools/fetch":  map[string]any{"serverId": "nope"},
		"/api/config/mcp/tools/toggle": map[string]any{"serverId": "nope", "tool": "t"},
		"/api/model-prices":            map[string]any{"model": "x"},
		"/api/model-prices/delete":     map[string]any{"model": "nope"},
	}
	for p, body := range postPaths {
		if rec := e.do(http.MethodPost, p, body, nil); rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("POST %s 未注册（%d）", p, rec.Code)
		}
	}
	// /api/model-prices 必须同时支持 GET 与 POST。
	if rec := e.do(http.MethodGet, "/api/model-prices", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/model-prices 异常：%d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/api/model-prices", map[string]any{"model": "x"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("POST /api/model-prices 异常：%d", rec.Code)
	}
}
