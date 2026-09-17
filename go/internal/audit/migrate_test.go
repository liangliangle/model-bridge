package audit

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// 结构迁移测试：老版本的库不能因为少列/未拆分就打不开或查询报错。

// legacySchema 是「尚未拆分大字段」的旧结构：大字段在主表里，
// 且缺少 cache_read_tokens / cache_creation_tokens / cost_usd，timestamp 还是文本。
const legacySchema = `
CREATE TABLE audit_log (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp                 TEXT,
    method                    TEXT NOT NULL,
    path                      TEXT NOT NULL,
    request_headers           TEXT,
    forwarded_request_headers TEXT,
    request_body              TEXT,
    forwarded_request_body    TEXT,
    alias_model               TEXT NOT NULL,
    actual_channel            TEXT,
    actual_model              TEXT,
    mapping_source            TEXT,
    upstream_response_headers TEXT,
    response_headers          TEXT,
    upstream_response_body    TEXT,
    response_body             TEXT,
    status_code               INTEGER,
    first_byte_ms             INTEGER,
    latency_ms                INTEGER,
    input_tokens              INTEGER,
    output_tokens             INTEGER,
    retry_count               INTEGER DEFAULT 0,
    failover_chain            TEXT,
    error_message             TEXT,
    user_agent                TEXT
);`

// splitSchemaWithoutCost 是「已拆分但缺 cost_usd」的结构——线上旧库最常见的形态，
// 也是 /api/audit 与 /api/stats 报 no such column 的直接原因。
const splitSchemaWithoutCost = `
CREATE TABLE audit_log (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp               INTEGER NOT NULL DEFAULT 0,
    method                  TEXT NOT NULL,
    path                    TEXT NOT NULL,
    alias_model             TEXT NOT NULL,
    actual_channel          TEXT,
    actual_model            TEXT,
    mapping_source          TEXT,
    status_code             INTEGER,
    first_byte_ms           INTEGER,
    latency_ms              INTEGER,
    input_tokens            INTEGER,
    output_tokens           INTEGER,
    cache_read_tokens       INTEGER,
    cache_creation_tokens   INTEGER,
    retry_count             INTEGER DEFAULT 0,
    failover_chain          TEXT,
    error_message           TEXT,
    user_agent              TEXT
);
CREATE TABLE audit_log_bodies (
    id INTEGER PRIMARY KEY REFERENCES audit_log(id),
    request_headers TEXT,
    forwarded_request_headers TEXT,
    request_body TEXT,
    forwarded_request_body TEXT,
    upstream_response_headers TEXT,
    response_headers TEXT,
    upstream_response_body TEXT,
    response_body TEXT
);`

func columnSet(t *testing.T, path, table string) map[string]bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	cols, err := (&DB{conn: db}).tableColumns(table)
	if err != nil {
		t.Fatalf("读取 %s 列失败: %v", table, err)
	}
	return cols
}

func TestOpenBackfillsMissingColumnsOnSplitDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")

	// 造一个「已拆分但缺 cost_usd」的库，并放一条记录。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	if _, err := raw.Exec(splitSchemaWithoutCost); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO audit_log (timestamp, method, path, alias_model, actual_channel, actual_model,
		    status_code, first_byte_ms, latency_ms, input_tokens, output_tokens)
		 VALUES (1700000000000, 'POST', '/v1/chat/completions', 'gpt-test', 'chat-ch', 'up-model', 200, 5, 20, 11, 5)`,
	); err != nil {
		t.Fatalf("插入记录失败: %v", err)
	}
	_ = raw.Close()

	// Open 应当补齐缺失列，而不是留下一个查询必炸的库。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	cols := columnSet(t, path, "audit_log")
	if !cols["cost_usd"] {
		t.Fatal("Open 应补齐 cost_usd 列")
	}

	// 这两条查询之前会直接报 no such column: cost_usd。
	if _, err := db.QueryList(QueryListParams{Limit: 10}); err != nil {
		t.Fatalf("QueryList 应可用: %v", err)
	}
	if _, err := db.GetStats("all", nil); err != nil {
		t.Fatalf("GetStats 应可用: %v", err)
	}

	// 补列之后，代理实际使用的整条写入链路都必须能在老库上跑通：
	// Create → UpdateRoute → UpdateResponse（写 cost_usd）→ 读回。
	headers, body, ua := `{"user-agent":"test"}`, `{"model":"gpt-test"}`, "test/1.0"
	id, err := db.Create("POST", "/v1/chat/completions", "gpt-test", &headers, &body, &ua)
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if err := db.UpdateRoute(id, "chat-ch", "up-model", "explicit"); err != nil {
		t.Fatalf("UpdateRoute 失败: %v", err)
	}
	cost := 0.25
	tokens := TokenUsage{Input: u64Ptr(11), Output: u64Ptr(5)}
	respBody, upBody := `{"choices":[]}`, `{"choices":[]}`
	if err := db.UpdateResponse(id, 200, 20, &headers, &upBody, &headers, &respBody, tokens, &cost); err != nil {
		t.Fatalf("UpdateResponse（写 cost_usd）失败: %v", err)
	}
	items, err := db.QueryList(QueryListParams{Limit: 10})
	if err != nil || len(items) != 2 {
		t.Fatalf("写入后应能列出 2 条，得到 %d 条 err=%v", len(items), err)
	}
	entry, err := db.GetByID(id)
	if err != nil || entry == nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if entry.CostUsd == nil || *entry.CostUsd != cost {
		t.Fatalf("cost_usd 应能写入并读回，实际 %v", entry.CostUsd)
	}

	// 已拆分结构的库不该被再次拆分。
	migrated, err := db.MigrateSplitBodies()
	if err != nil {
		t.Fatalf("MigrateSplitBodies 失败: %v", err)
	}
	if migrated {
		t.Fatal("已拆分的库不应重复迁移")
	}
}

func TestMigrateSplitBodiesOnLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO audit_log (timestamp, method, path, request_headers, request_body,
		    alias_model, actual_channel, actual_model, upstream_response_body, response_body,
		    status_code, input_tokens, output_tokens)
		 VALUES ('2024-01-02 03:04:05', 'POST', '/v1/messages', '{"a":"1"}', '{"model":"gpt-test"}',
		         'gpt-test', 'anthropic-main', 'claude-x', '{"ok":true}', '{"ok":true}',
		         200, 11, 5)`,
	); err != nil {
		t.Fatalf("插入旧记录失败: %v", err)
	}
	_ = raw.Close()

	// Open 会补列（未拆分分支）+ 把文本时间戳转成毫秒。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	cols := columnSet(t, path, "audit_log")
	for _, want := range []string{"cost_usd", "cache_read_tokens", "cache_creation_tokens"} {
		if !cols[want] {
			t.Fatalf("Open 应补齐列 %s", want)
		}
	}

	// 迁移前：大字段还在主表里。
	migrated, err := db.MigrateSplitBodies()
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !migrated {
		t.Fatal("未拆分的库应执行迁移")
	}

	after := columnSet(t, path, "audit_log")
	if after["request_body"] {
		t.Fatal("迁移后主表不应再有 request_body 列")
	}
	if !columnSet(t, path, "audit_log_bodies")["request_body"] {
		t.Fatal("副表应有 request_body 列")
	}

	// 数据必须完整搬过去，且主表小字段保留。
	raw2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("重开库失败: %v", err)
	}
	defer func() { _ = raw2.Close() }()

	var (
		path2, alias, channel, actualModel string
		ts                                 int64
		inTok, outTok                      int64
	)
	if err := raw2.QueryRow(`SELECT path, alias_model, actual_channel, actual_model, timestamp, input_tokens, output_tokens
	                         FROM audit_log`).Scan(&path2, &alias, &channel, &actualModel, &ts, &inTok, &outTok); err != nil {
		t.Fatalf("读取迁移后主表失败: %v", err)
	}
	if path2 != "/v1/messages" || alias != "gpt-test" || channel != "anthropic-main" || actualModel != "claude-x" {
		t.Fatalf("主表小字段在迁移中丢失: %q %q %q %q", path2, alias, channel, actualModel)
	}
	if inTok != 11 || outTok != 5 {
		t.Fatalf("token 数在迁移中丢失: %d %d", inTok, outTok)
	}
	// '2024-01-02 03:04:05' UTC = 1704164645000 ms
	if ts != 1704164645000 {
		t.Fatalf("文本时间戳应转为毫秒（1704164645000），实际 %d", ts)
	}

	var reqBody string
	if err := raw2.QueryRow(`SELECT request_body FROM audit_log_bodies`).Scan(&reqBody); err != nil {
		t.Fatalf("读取迁移后副表失败: %v", err)
	}
	if reqBody != `{"model":"gpt-test"}` {
		t.Fatalf("副表数据不正确: %s", reqBody)
	}

	// 索引必须重建，否则列表查询会退化成全表扫描。
	var idxCount int
	if err := raw2.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND tbl_name='audit_log' AND name LIKE 'idx_audit_%'`,
	).Scan(&idxCount); err != nil {
		t.Fatalf("读取索引失败: %v", err)
	}
	if idxCount < 6 {
		t.Fatalf("迁移后应重建 6 个索引，实际 %d", idxCount)
	}

	// 幂等：再跑一次应为 false。
	again, err := db.MigrateSplitBodies()
	if err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}
	if again {
		t.Fatal("迁移应幂等")
	}

	// 迁移后的库，详情检索可用且内容完整。
	items, err := db.QueryList(QueryListParams{Limit: 10})
	if err != nil || len(items) != 1 {
		t.Fatalf("迁移后列表查询应可用，得到 %d 条 err=%v", len(items), err)
	}
	entry, err := db.GetByID(items[0].ID)
	if err != nil || entry == nil {
		t.Fatalf("迁移后详情查询应可用: %v", err)
	}
	if entry.RequestBody == nil || *entry.RequestBody != `{"model":"gpt-test"}` {
		t.Fatalf("迁移后详情里应能读到请求体，实际 %v", entry.RequestBody)
	}
}
