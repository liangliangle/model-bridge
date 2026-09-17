package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"modelbridge/internal/audit"
	"modelbridge/internal/config"

	_ "modernc.org/sqlite"
)

// 渠道优先级与故障转移：高优先级上游持续报错时，最终由低优先级渠道服务，
// 且 mock 侧的调用记录能证明「先试高优先级、失败后才切换」。
func TestFailoverPrefersPriorityThenFallsBack(t *testing.T) {
	h := newHarness(t,
		channelSpec{"chat-p1", config.ProviderOpenAI, "PLACEHOLDER", 1},
		channelSpec{"chat-p2", config.ProviderOpenAI, "PLACEHOLDER", 2},
	)
	if err := h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat-fail")
		cfg.Channels[1].Endpoint.URL = h.mock.URL("/chat")
		return nil
	}); err != nil {
		t.Fatalf("更新渠道地址失败: %v", err)
	}

	resp, raw := h.post("/v1/chat/completions", body(chatRequestBody, false))
	if resp.StatusCode != 200 {
		t.Fatalf("期望由低优先级渠道成功服务（200），实际 %d: %s", resp.StatusCode, raw)
	}
	down := decode(t, raw)
	choices := asList(t, down["choices"], "choices")
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != mockContent {
		t.Fatalf("最终响应内容应来自可用渠道，实际 %v", msg["content"])
	}

	paths := h.mock.RequestPaths()
	if len(paths) != 2 {
		t.Fatalf("应恰好尝试两个渠道，实际调用 %v", paths)
	}
	if paths[0] != "/chat-fail" || paths[1] != "/chat" {
		t.Fatalf("必须先试高优先级渠道再切换，实际调用顺序 %v", paths)
	}

	var log strings.Builder
	fmt.Fprintf(&log, "渠道配置（按优先级）：\n")
	fmt.Fprintf(&log, "  priority=1 id=chat-p1 endpoint=%s\n", h.mock.URL("/chat-fail"))
	fmt.Fprintf(&log, "  priority=2 id=chat-p2 endpoint=%s\n", h.mock.URL("/chat"))
	fmt.Fprintf(&log, "\nmock 上游收到的调用顺序（含请求体）：\n")
	for i, req := range h.mock.Requests() {
		fmt.Fprintf(&log, "\n[%d] %s %s\n", i+1, req.Method, req.Path)
		fmt.Fprintf(&log, "    body: %s\n", strings.TrimSpace(string(req.Body)))
	}
	fmt.Fprintf(&log, "\n下游状态码: %d\n下游响应: %s\n", resp.StatusCode, raw)
	writeEvidence(t, "failover.log", []byte(log.String()))
}

// 审计与成本落库：请求完成后直接打开落盘的 SQLite 断言表结构与数值。
func TestAuditAndCostPersisted(t *testing.T) {
	h := newHarness(t, channelSpec{"chat-ch", config.ProviderOpenAI, "PLACEHOLDER", 1})
	if err := h.state.Update(func(cfg *config.AppConfig) error {
		cfg.Channels[0].Endpoint.URL = h.mock.URL("/chat")
		cfg.ModelPrices = []config.ModelPrice{{
			Model:            "up-model",
			InputPerMtok:     2.5,
			OutputPerMtok:    10.0,
			CacheReadPerMtok: ptrFloat(1.25),
			Enabled:          true,
		}}
		return nil
	}); err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}

	resp, raw := h.post("/v1/chat/completions", body(chatRequestBody, false))
	if resp.StatusCode != 200 {
		t.Fatalf("期望 200，实际 %d: %s", resp.StatusCode, raw)
	}

	// 走应用内 API 的读取路径（与 /api/audit 一致）。
	items, err := h.state.Audit.QueryList(audit.QueryListParams{Limit: 10})
	if err != nil {
		t.Fatalf("QueryList 失败: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("审计库中没有记录")
	}
	entry, err := h.state.Audit.GetByID(items[0].ID)
	if err != nil || entry == nil {
		t.Fatalf("GetByID 失败: %v", err)
	}

	// 再直接查落盘的库文件，证明数据真的持久化了（而不是只在内存里）。
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "audit.db"))
	if err != nil {
		t.Fatalf("打开 SQLite 失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	var log strings.Builder
	fmt.Fprintf(&log, "=== PRAGMA table_info(audit_log) ===\n")
	cols, err := db.Query("PRAGMA table_info(audit_log)")
	if err != nil {
		t.Fatalf("读取表结构失败: %v", err)
	}
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("扫描表结构失败: %v", err)
		}
		fmt.Fprintf(&log, "  %-28s %s\n", name, ctype)
	}
	cols.Close()

	var total int
	if err := db.QueryRow("SELECT count(*) FROM audit_log").Scan(&total); err != nil {
		t.Fatalf("统计记录数失败: %v", err)
	}
	fmt.Fprintf(&log, "\n=== 记录数 ===\n  audit_log: %d\n", total)
	if total < 1 {
		t.Fatal("audit_log 记录数应随请求增长")
	}

	var (
		statusCode   sql.NullInt64
		inputTokens  sql.NullInt64
		outputTokens sql.NullInt64
		costUSD      sql.NullFloat64
		channelID    sql.NullString
		actualModel  sql.NullString
	)
	err = db.QueryRow(`SELECT status_code, input_tokens, output_tokens, cost_usd, actual_channel, actual_model
	                   FROM audit_log ORDER BY id DESC LIMIT 1`).
		Scan(&statusCode, &inputTokens, &outputTokens, &costUSD, &channelID, &actualModel)
	if err != nil {
		t.Fatalf("读取最新记录失败: %v", err)
	}
	fmt.Fprintf(&log, "\n=== 最新记录 ===\n")
	fmt.Fprintf(&log, "  status_code   = %v\n", statusCode.Int64)
	fmt.Fprintf(&log, "  channel_id    = %v\n", channelID.String)
	fmt.Fprintf(&log, "  actual_model  = %v\n", actualModel.String)
	fmt.Fprintf(&log, "  input_tokens  = %v\n", inputTokens.Int64)
	fmt.Fprintf(&log, "  output_tokens = %v\n", outputTokens.Int64)
	fmt.Fprintf(&log, "  cost_usd      = %v\n", costUSD.Float64)

	if !inputTokens.Valid || inputTokens.Int64 != mockPromptTokens {
		t.Fatalf("input_tokens 应落库为 %d，实际 %v", mockPromptTokens, inputTokens)
	}
	if !outputTokens.Valid || outputTokens.Int64 != mockCompletionTokens {
		t.Fatalf("output_tokens 应落库为 %d，实际 %v", mockCompletionTokens, outputTokens)
	}
	if !costUSD.Valid || costUSD.Float64 <= 0 {
		t.Fatalf("cost_usd 应落库为正数，实际 %v", costUSD)
	}
	// 2.5/1M*11 + 10/1M*5 = 0.0000275 + 0.00005
	wantCost := (2.5*float64(mockPromptTokens) + 10.0*float64(mockCompletionTokens)) / 1_000_000.0
	if diff := costUSD.Float64 - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost_usd 应为 %.10f，实际 %.10f", wantCost, costUSD.Float64)
	}

	pretty, _ := json.MarshalIndent(entry, "", "  ")
	fmt.Fprintf(&log, "\n=== GET /api/audit/detail 形状（Entry）===\n%s\n", pretty)
	writeEvidence(t, "audit-db.txt", []byte(log.String()))
}

func ptrFloat(v float64) *float64 { return &v }
