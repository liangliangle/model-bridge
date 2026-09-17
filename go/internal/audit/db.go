// Package audit 提供审计日志的 SQLite 落库与检索。
//
// 表结构与字段名与 Rust 侧 src-tauri/src/audit/db.rs 保持一致（新增了列而非改名），
// 大字段（请求/响应报文）放在副表 audit_log_bodies，并只保留最近 BODIES_RETAIN_LATEST 条。
package audit

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// BODIESRetainLatest 详情大字段只保留最近多少条。
const BODIESRetainLatest = 1000

const schema = `
CREATE TABLE IF NOT EXISTS audit_log (
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
    cost_usd                REAL,
    retry_count             INTEGER DEFAULT 0,
    failover_chain          TEXT,
    error_message           TEXT,
    user_agent              TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log(timestamp);
CREATE INDEX IF NOT EXISTS idx_audit_model ON audit_log(alias_model);
CREATE INDEX IF NOT EXISTS idx_audit_channel ON audit_log(actual_channel);
CREATE INDEX IF NOT EXISTS idx_audit_status ON audit_log(status_code);
CREATE INDEX IF NOT EXISTS idx_audit_channel_ts ON audit_log(actual_channel, timestamp);
CREATE INDEX IF NOT EXISTS idx_audit_ts_status ON audit_log(timestamp, status_code);

CREATE TABLE IF NOT EXISTS audit_log_bodies (
    id INTEGER PRIMARY KEY REFERENCES audit_log(id),
    request_headers TEXT,
    forwarded_request_headers TEXT,
    request_body TEXT,
    forwarded_request_body TEXT,
    upstream_response_headers TEXT,
    response_headers TEXT,
    upstream_response_body TEXT,
    response_body TEXT
);
`

// DB 审计数据库句柄。
type DB struct {
	mu   sync.Mutex
	conn *sql.DB
	path string
}

// Open 打开（或创建）审计数据库并建表。
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create audit dir: %w", err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open audit db: %w", err)
	}
	// SQLite 单写者；串行化避免 database is locked
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(schema); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}
	db := &DB{conn: conn, path: path}
	// 老版本的库列更少：CREATE TABLE IF NOT EXISTS 是空操作，必须显式补列，
	// 否则查询会报 no such column（见 migrate.go 顶部说明）。
	if err := db.migrateSchema(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return db, nil
}

// Close 关闭数据库。
func (d *DB) Close() error { return d.conn.Close() }

// Path 返回数据库文件路径。
func (d *DB) Path() string { return d.path }

// TokenUsage 一次请求的各类 token 数。
type TokenUsage struct {
	Input         *int64 `json:"input"`
	Output        *int64 `json:"output"`
	CacheRead     *int64 `json:"cache_read"`
	CacheCreation *int64 `json:"cache_creation"`
}

// nowMs 当前 UTC 毫秒时间戳。
func nowMs() int64 { return time.Now().UTC().UnixMilli() }

// Create 插入一条请求记录，返回自增 id。
func (d *DB) Create(method, path, aliasModel string, headers, body, userAgent *string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.conn.Exec(
		`INSERT INTO audit_log (timestamp, method, path, alias_model, user_agent)
		 VALUES (?,?,?,?,?)`,
		nowMs(), method, path, aliasModel, userAgent,
	)
	if err != nil {
		return -1, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return -1, err
	}
	if _, err := d.conn.Exec(
		`INSERT INTO audit_log_bodies (id, request_headers, request_body) VALUES (?,?,?)`,
		id, headers, body,
	); err != nil {
		return id, err
	}
	return id, nil
}

// UpdateRoute 记录选中的渠道与实际模型。
func (d *DB) UpdateRoute(id int64, channel, actualModel, mappingSource string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(
		`UPDATE audit_log SET actual_channel=?, actual_model=?, mapping_source=? WHERE id=?`,
		channel, actualModel, mappingSource, id,
	)
	return err
}

// UpdateRetryInfo 记录重试次数与故障转移链。
func (d *DB) UpdateRetryInfo(id int64, retryCount uint32, chain []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	joined := strings.Join(chain, ",")
	_, err := d.conn.Exec(
		`UPDATE audit_log SET retry_count=?, failover_chain=? WHERE id=?`,
		retryCount, joined, id,
	)
	return err
}

// UpdateError 记录失败结果。
func (d *DB) UpdateError(id int64, statusCode int, latencyMs uint64, message string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(
		`UPDATE audit_log SET status_code=?, latency_ms=?, error_message=? WHERE id=?`,
		statusCode, latencyMs, message, id,
	)
	return err
}

// UpdateFirstByte 记录流式响应的首字节耗时（主表）与响应头（副表）。
//
// 对应 Rust AuditDb::update_first_byte（db.rs:424）：主表总是写入 first_byte_ms；
// 副表的两个响应头列用 COALESCE(?2, col) 更新，已有值不会被覆盖。
func (d *DB) UpdateFirstByte(id int64, firstByteMs uint64, upstreamHeaders, responseHeaders *string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// 主表：首字节耗时
	if _, err := d.conn.Exec(
		`UPDATE audit_log SET first_byte_ms=? WHERE id=?`, firstByteMs, id,
	); err != nil {
		return err
	}
	// 副表：响应头（COALESCE 避免覆盖已有值）
	if upstreamHeaders != nil {
		if _, err := d.conn.Exec(
			`UPDATE audit_log_bodies SET upstream_response_headers=COALESCE(?, upstream_response_headers) WHERE id=?`,
			upstreamHeaders, id,
		); err != nil {
			return err
		}
	}
	if responseHeaders != nil {
		if _, err := d.conn.Exec(
			`UPDATE audit_log_bodies SET response_headers=COALESCE(?, response_headers) WHERE id=?`,
			responseHeaders, id,
		); err != nil {
			return err
		}
	}
	return nil
}

// UpdateResponse 记录非流式成功结果（上游/下发响应头与响应体、token 统计）。
//
// 对应 Rust AuditDb::update_response（db.rs:450）：first_byte_ms 与 latency_ms 绑定同一个值。
func (d *DB) UpdateResponse(
	id int64, statusCode int, latencyMs uint64,
	upstreamHeaders, upstreamBody, responseHeaders, responseBody *string,
	tokens TokenUsage, cost *float64,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE audit_log SET status_code=?, first_byte_ms=?, latency_ms=?,
		    input_tokens=?, output_tokens=?, cache_read_tokens=?, cache_creation_tokens=?, cost_usd=?
		 WHERE id=?`,
		statusCode, latencyMs, latencyMs,
		tokens.Input, tokens.Output, tokens.CacheRead, tokens.CacheCreation, cost, id,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE audit_log_bodies SET upstream_response_headers=?, upstream_response_body=?,
		    response_headers=?, response_body=? WHERE id=?`,
		upstreamHeaders, upstreamBody, responseHeaders, responseBody, id,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateStreamingResponse 记录流式结果（上游原始 + 转换后下发内容）。
//
// 对应 Rust AuditDb::update_streaming_response（db.rs:478）：
// 入库前先把逐条 SSE 事件组装成完整响应对象，透传场景沿用上游组装结果；
// token 仍从上游原始流提取（usage 最完整）。
func (d *DB) UpdateStreamingResponse(
	id int64, upstreamBody string, forwardedBody *string,
	statusCode int, latencyMs uint64, cost *float64,
) error {
	upstreamAssembled := assembleStreamingResponse(upstreamBody)
	// 转发响应也组装为最终结果对象；透传场景沿用上游组装结果。
	responseBody := upstreamAssembled
	if forwardedBody != nil {
		responseBody = assembleStreamingResponse(*forwardedBody)
	}
	tokens := ExtractTokensFromStream(upstreamBody)
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE audit_log SET status_code=?, latency_ms=?,
		    input_tokens=?, output_tokens=?, cache_read_tokens=?, cache_creation_tokens=?, cost_usd=?
		 WHERE id=?`,
		statusCode, latencyMs,
		tokens.Input, tokens.Output, tokens.CacheRead, tokens.CacheCreation, cost, id,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE audit_log_bodies SET upstream_response_body=?, response_body=? WHERE id=?`,
		upstreamAssembled, responseBody, id,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// SetForwardedRequest 记录转换后实际发往上游的请求体。
func (d *DB) SetForwardedRequest(id int64, headers, body *string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(
		`UPDATE audit_log_bodies SET forwarded_request_headers=?, forwarded_request_body=? WHERE id=?`,
		headers, body, id,
	)
	return err
}

// SetResponseHeaders 记录下游响应头。
func (d *DB) SetResponseHeaders(id int64, headers *string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(
		`UPDATE audit_log_bodies SET response_headers=? WHERE id=?`, headers, id,
	)
	return err
}

// SetUpstreamRequestHeaders 记录转发给上游的请求头。
func (d *DB) SetUpstreamRequestHeaders(id int64, headers *string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(
		`UPDATE audit_log_bodies SET forwarded_request_headers=? WHERE id=?`, headers, id,
	)
	return err
}

// SetUpstreamResponseHeaders 记录上游响应头。
func (d *DB) SetUpstreamResponseHeaders(id int64, headers *string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(
		`UPDATE audit_log_bodies SET upstream_response_headers=? WHERE id=?`, headers, id,
	)
	return err
}

// ListItem 审计列表的轻量条目（前端 AuditListItem）。
type ListItem struct {
	ID                  int64    `json:"id"`
	Timestamp           int64    `json:"timestamp"`
	Method              string   `json:"method"`
	Path                string   `json:"path"`
	AliasModel          string   `json:"alias_model"`
	ActualChannel       *string  `json:"actual_channel"`
	ActualModel         *string  `json:"actual_model"`
	MappingSource       *string  `json:"mapping_source"`
	StatusCode          *int64   `json:"status_code"`
	FirstByteMs         *int64   `json:"first_byte_ms"`
	LatencyMs           *int64   `json:"latency_ms"`
	InputTokens         *int64   `json:"input_tokens"`
	OutputTokens        *int64   `json:"output_tokens"`
	CacheReadTokens     *int64   `json:"cache_read_tokens"`
	CacheCreationTokens *int64   `json:"cache_creation_tokens"`
	CostUsd             *float64 `json:"cost_usd"`
	RetryCount          int64    `json:"retry_count"`
	ErrorMessage        *string  `json:"error_message"`
}

// Entry 审计详情（前端 AuditEntry）。
type Entry struct {
	ListItem
	RequestHeaders          *string `json:"request_headers"`
	ForwardedRequestHeaders *string `json:"forwarded_request_headers"`
	RequestBody             *string `json:"request_body"`
	ForwardedRequestBody    *string `json:"forwarded_request_body"`
	UpstreamResponseHeaders *string `json:"upstream_response_headers"`
	ResponseHeaders         *string `json:"response_headers"`
	UpstreamResponseBody    *string `json:"upstream_response_body"`
	ResponseBody            *string `json:"response_body"`
	FailoverChain           *string `json:"failover_chain"`
	UserAgent               *string `json:"user_agent"`
}

// QueryListParams 列表查询过滤条件。
type QueryListParams struct {
	ModelFilter       string
	ChannelFilter     string
	StatusFilter      string
	ActualModelFilter string
	PathFilter        string
	TimeFrom          string
	TimeTo            string
	Limit             uint32
}

// QueryList 查询审计列表（不含详情大字段）。
func (d *DB) QueryList(p QueryListParams) ([]ListItem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}

	if p.ModelFilter != "" {
		add("alias_model = ?%d", p.ModelFilter)
	}
	if p.ChannelFilter != "" {
		add("actual_channel = ?%d", p.ChannelFilter)
	}
	if p.ActualModelFilter != "" {
		add("actual_model = ?%d", p.ActualModelFilter)
	}
	if p.PathFilter != "" {
		add("path = ?%d", p.PathFilter)
	}
	if p.StatusFilter != "" {
		switch p.StatusFilter {
		case "success":
			conds = append(conds, "status_code >= 200 AND status_code < 300")
		case "error":
			conds = append(conds, "(status_code >= 400 OR error_message IS NOT NULL)")
		default:
			if v, err := parseInt(p.StatusFilter); err == nil {
				add("status_code = ?%d", v)
			}
		}
	}
	if p.TimeFrom != "" {
		if ms, err := parseTimeBound(p.TimeFrom); err == nil {
			add("timestamp >= ?%d", ms)
		}
	}
	if p.TimeTo != "" {
		if ms, err := parseTimeBound(p.TimeTo); err == nil {
			add("timestamp <= ?%d", ms)
		}
	}

	limit := p.Limit
	if limit == 0 {
		limit = 100
	}
	args = append(args, limit)
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	sqlText := fmt.Sprintf(
		`SELECT id, timestamp, method, path, alias_model, actual_channel, actual_model,
		        mapping_source, status_code, first_byte_ms, latency_ms,
		        input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		        cost_usd, retry_count, error_message
		 FROM audit_log %s ORDER BY id DESC LIMIT ?%d`, where, len(args))

	rows, err := d.conn.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ListItem{}
	for rows.Next() {
		var it ListItem
		if err := rows.Scan(
			&it.ID, &it.Timestamp, &it.Method, &it.Path, &it.AliasModel,
			&it.ActualChannel, &it.ActualModel, &it.MappingSource,
			&it.StatusCode, &it.FirstByteMs, &it.LatencyMs,
			&it.InputTokens, &it.OutputTokens, &it.CacheReadTokens, &it.CacheCreationTokens,
			&it.CostUsd, &it.RetryCount, &it.ErrorMessage,
		); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetByID 读取单条完整记录（含详情大字段）。
func (d *DB) GetByID(id int64) (*Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var e Entry
	err := d.conn.QueryRow(
		`SELECT a.id, a.timestamp, a.method, a.path, a.alias_model, a.actual_channel,
		        a.actual_model, a.mapping_source, a.status_code, a.first_byte_ms, a.latency_ms,
		        a.input_tokens, a.output_tokens, a.cache_read_tokens, a.cache_creation_tokens,
		        a.cost_usd, a.retry_count, a.error_message,
		        b.request_headers, b.forwarded_request_headers, b.request_body,
		        b.forwarded_request_body, b.upstream_response_headers, b.response_headers,
		        b.upstream_response_body, b.response_body,
		        a.failover_chain, a.user_agent
		 FROM audit_log a LEFT JOIN audit_log_bodies b ON a.id = b.id
		 WHERE a.id = ?`, id,
	).Scan(
		&e.ID, &e.Timestamp, &e.Method, &e.Path, &e.AliasModel, &e.ActualChannel,
		&e.ActualModel, &e.MappingSource, &e.StatusCode, &e.FirstByteMs, &e.LatencyMs,
		&e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheCreationTokens,
		&e.CostUsd, &e.RetryCount, &e.ErrorMessage,
		&e.RequestHeaders, &e.ForwardedRequestHeaders, &e.RequestBody,
		&e.ForwardedRequestBody, &e.UpstreamResponseHeaders, &e.ResponseHeaders,
		&e.UpstreamResponseBody, &e.ResponseBody,
		&e.FailoverChain, &e.UserAgent,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Status 审计数据库概况（设置页展示）。
type Status struct {
	SizeBytes     int64 `json:"size_bytes"`
	TotalRecords  int64 `json:"total_records"`
	DetailRecords int64 `json:"detail_records"`
}

// Status 返回数据库概况。
func (d *DB) Status() (Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var st Status
	if fi, err := os.Stat(d.path); err == nil {
		st.SizeBytes = fi.Size()
	}
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&st.TotalRecords); err != nil {
		return st, err
	}
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM audit_log_bodies`).Scan(&st.DetailRecords); err != nil {
		return st, err
	}
	return st, nil
}

// Stats 统计概览（仪表盘）。
type Stats struct {
	TotalRequests       int64   `json:"total_requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	ErrorCount          int64   `json:"error_count"`
	SuccessCount        int64   `json:"success_count"`
	CostUsd             float64 `json:"cost_usd"`
}

// periodCutoffMs 按本地时区的自然日起点计算 cutoff。
func periodCutoffMs(period string) int64 {
	now := time.Now()
	daysBack := 0
	switch period {
	case "7d":
		daysBack = 7
	case "30d":
		daysBack = 30
	}
	target := now.AddDate(0, 0, -daysBack)
	midnight := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, time.Local)
	return midnight.UnixMilli()
}

// GetStats 统计概览，按 period（today/7d/30d）与可选渠道过滤。
func (d *DB) GetStats(period string, channel *string) (Stats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	cutoff := periodCutoffMs(period)
	where := "timestamp >= ?1"
	var args []any
	args = append(args, cutoff)
	if channel != nil && *channel != "" {
		args = append(args, *channel)
		where += fmt.Sprintf(" AND actual_channel = ?%d", len(args))
	}

	var st Stats
	err := d.conn.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*),
		        COALESCE(SUM(COALESCE(input_tokens,0)),0),
		        COALESCE(SUM(COALESCE(output_tokens,0)),0),
		        COALESCE(SUM(COALESCE(cache_read_tokens,0)),0),
		        COALESCE(SUM(COALESCE(cache_creation_tokens,0)),0),
		        COALESCE(SUM(CASE WHEN status_code IS NOT NULL AND (status_code >= 400 OR error_message IS NOT NULL) THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN status_code IS NOT NULL AND status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(COALESCE(cost_usd,0)),0)
		 FROM audit_log WHERE %s`, where), args...,
	).Scan(
		&st.TotalRequests, &st.InputTokens, &st.OutputTokens,
		&st.CacheReadTokens, &st.CacheCreationTokens,
		&st.ErrorCount, &st.SuccessCount, &st.CostUsd,
	)
	return st, err
}

// DailyHeat 按天聚合的 token 用量。
type DailyHeat struct {
	Date   string `json:"date"`
	Tokens uint64 `json:"tokens"`
}

// GetTokenHeatmap 返回最近 days 天的按天 token 聚合。
func (d *DB) GetTokenHeatmap(days uint32) ([]DailyHeat, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	cutoff := periodCutoffMs(fmt.Sprintf("%dd", days))
	rows, err := d.conn.Query(
		`SELECT date(timestamp/1000, 'unixepoch', 'localtime') AS day,
		        COALESCE(SUM(COALESCE(input_tokens,0)+COALESCE(output_tokens,0)+
		                     COALESCE(cache_read_tokens,0)+COALESCE(cache_creation_tokens,0)),0)
		 FROM audit_log WHERE timestamp >= ?1 GROUP BY day ORDER BY day`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DailyHeat{}
	for rows.Next() {
		var h DailyHeat
		if err := rows.Scan(&h.Date, &h.Tokens); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// CleanupResult 清理结果。
type CleanupResult struct {
	DeletedOld int64 `json:"deleted_old"`
	Pruned     int64 `json:"pruned"`
	SizeBefore int64 `json:"size_before"`
	SizeAfter  int64 `json:"size_after"`
}

// ForceCleanup 删除超期记录 + 详情只保留最近 keepLatest 条 + VACUUM 回收空间。
// retentionDays 为 0 表示永久留存，跳过按天删除。
func (d *DB) ForceCleanup(retentionDays uint32, keepLatest int) (CleanupResult, error) {
	var res CleanupResult
	if fi, err := os.Stat(d.path); err == nil {
		res.SizeBefore = fi.Size()
	}

	if retentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -int(retentionDays)).UnixMilli()
		r, err := d.conn.Exec(`DELETE FROM audit_log WHERE timestamp < ?`, cutoff)
		if err != nil {
			return res, err
		}
		res.DeletedOld, _ = r.RowsAffected()
	}

	pruned, err := d.pruneBodies(keepLatest)
	if err != nil {
		return res, err
	}
	res.Pruned = pruned

	// VACUUM 需要无活动事务
	if _, err := d.conn.Exec(`VACUUM`); err != nil {
		return res, err
	}
	if fi, err := os.Stat(d.path); err == nil {
		res.SizeAfter = fi.Size()
	}
	return res, nil
}

// pruneBodies 详情只保留最近 keep 条。
func (d *DB) pruneBodies(keep int) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pruneBodiesLocked(keep)
}

func (d *DB) pruneBodiesLocked(keep int) (int64, error) {
	r, err := d.conn.Exec(
		`DELETE FROM audit_log_bodies WHERE id NOT IN (
		     SELECT id FROM audit_log ORDER BY id DESC LIMIT ?
		 )`, keep)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return n, nil
}

// PruneBodies 供外部调用的详情裁剪。
func (d *DB) PruneBodies(keep int) (int64, error) { return d.pruneBodies(keep) }

// ---------- token 提取 ----------

func u64Ptr(v int64) *int64 { return &v }

// ExtractTokensFromJSON 从非流式响应 JSON 提取 token 用量。
func ExtractTokensFromJSON(v map[string]any) TokenUsage {
	var t TokenUsage
	usage, _ := v["usage"].(map[string]any)
	if usage == nil {
		return t
	}
	// Anthropic 口径
	if x, ok := asInt(usage["input_tokens"]); ok {
		t.Input = u64Ptr(x)
	}
	if x, ok := asInt(usage["output_tokens"]); ok {
		t.Output = u64Ptr(x)
	}
	if x, ok := asInt(usage["cache_read_input_tokens"]); ok {
		t.CacheRead = u64Ptr(x)
	}
	if x, ok := asInt(usage["cache_creation_input_tokens"]); ok {
		t.CacheCreation = u64Ptr(x)
	}
	// Chat 口径（覆盖上面的零值）
	if x, ok := asInt(usage["prompt_tokens"]); ok {
		t.Input = u64Ptr(x)
	}
	if x, ok := asInt(usage["completion_tokens"]); ok {
		t.Output = u64Ptr(x)
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if x, ok := asInt(details["cached_tokens"]); ok {
			t.CacheRead = u64Ptr(x)
		}
		if x, ok := asInt(details["cache_creation_tokens"]); ok {
			t.CacheCreation = u64Ptr(x)
		}
	}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if x, ok := asInt(details["cached_tokens"]); ok {
			t.CacheRead = u64Ptr(x)
		}
	}
	return t
}

// ExtractTokensFromStream 从 SSR 原始字节流提取 token 用量。
//
// 三种上游格式都覆盖：Responses 的 response.completed、Anthropic 的
// message_start/message_delta、Chat 的末尾 usage chunk。
func ExtractTokensFromStream(raw string) TokenUsage {
	var t TokenUsage
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		data, ok := strings.CutPrefix(trimmed, "data: ")
		if !ok {
			data, ok = strings.CutPrefix(trimmed, "data:")
			if !ok {
				continue
			}
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			continue
		}
		if v["type"] == "response.completed" {
			if resp, ok := v["response"].(map[string]any); ok {
				if usage, ok := resp["usage"].(map[string]any); ok {
					mergeResponsesUsage(&t, usage)
				}
			}
			continue
		}
		if usage, ok := v["usage"].(map[string]any); ok {
			mergeUsageObject(&t, usage)
		}
		if msg, ok := v["message"].(map[string]any); ok {
			if usage, ok := msg["usage"].(map[string]any); ok {
				mergeUsageObject(&t, usage)
			}
		}
	}
	return t
}

func mergeUsageObject(t *TokenUsage, usage map[string]any) {
	if x, ok := asInt(usage["input_tokens"]); ok && x > 0 {
		t.Input = u64Ptr(x)
	}
	if x, ok := asInt(usage["output_tokens"]); ok && x > 0 {
		t.Output = u64Ptr(x)
	}
	if x, ok := asInt(usage["cache_read_input_tokens"]); ok && x > 0 {
		t.CacheRead = u64Ptr(x)
	}
	if x, ok := asInt(usage["cache_creation_input_tokens"]); ok && x > 0 {
		t.CacheCreation = u64Ptr(x)
	}
	if x, ok := asInt(usage["prompt_tokens"]); ok && x > 0 {
		t.Input = u64Ptr(x)
	}
	if x, ok := asInt(usage["completion_tokens"]); ok && x > 0 {
		t.Output = u64Ptr(x)
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if x, ok := asInt(details["cached_tokens"]); ok && x > 0 {
			t.CacheRead = u64Ptr(x)
		}
	}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if x, ok := asInt(details["cached_tokens"]); ok && x > 0 {
			t.CacheRead = u64Ptr(x)
		}
	}
}

func mergeResponsesUsage(t *TokenUsage, usage map[string]any) {
	if x, ok := asInt(usage["input_tokens"]); ok && x > 0 {
		t.Input = u64Ptr(x)
	}
	if x, ok := asInt(usage["output_tokens"]); ok && x > 0 {
		t.Output = u64Ptr(x)
	}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if x, ok := asInt(details["cached_tokens"]); ok && x > 0 {
			t.CacheRead = u64Ptr(x)
		}
	}
}

// asInt 把 JSON 数字转成 int64。
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

// parseInt 解析整数字符串。
func parseInt(s string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}

// parseTimeBound 解析时间边界：支持毫秒时间戳或 RFC3339 字符串。
func parseTimeBound(s string) (int64, error) {
	if v, err := parseInt(s); err == nil {
		return v, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}
