package audit

import (
	"fmt"
	"log"
	"strings"
)

// 结构迁移。
//
// 对应 Rust `AuditDb::initialize_tables`（audit/db.rs:55）里的兼容分支与
// `AuditDb::migrate_split_bodies`（audit/db.rs:194）。
//
// 为什么必须有：`CREATE TABLE IF NOT EXISTS` 对已存在的库是空操作，老版本的库列更少，
// 不补列的话后续查询会直接报 `no such column`——例如旧库缺 cost_usd 时
// `/api/audit` 与 `/api/stats` 会整体报错，前端拿到错误对象后页面就白了。

// legacyBodyColumns 是「尚未拆分大字段」的旧主表需要补齐的列。
// 与 Rust db.rs:120 的列表逐项一致。
var legacyBodyColumns = []string{
	"request_headers TEXT",
	"response_headers TEXT",
	"forwarded_request_body TEXT",
	"forwarded_request_headers TEXT",
	"upstream_response_headers TEXT",
	"upstream_response_body TEXT",
	"cache_read_tokens INTEGER",
	"cache_creation_tokens INTEGER",
	"first_byte_ms INTEGER",
	"cost_usd REAL",
}

// numericBackfillColumns 是已拆分库仍需补齐的后加数值列。
// 与 Rust db.rs:136 的列表逐项一致。
var numericBackfillColumns = []string{
	"cache_read_tokens INTEGER",
	"cache_creation_tokens INTEGER",
	"first_byte_ms INTEGER",
	"cost_usd REAL",
}

// bodyColumns 是副表承载的 9 个大字段，搬迁与重建都按这个顺序。
var bodyColumns = []string{
	"request_headers",
	"forwarded_request_headers",
	"request_body",
	"forwarded_request_body",
	"upstream_response_headers",
	"response_headers",
	"upstream_response_body",
	"response_body",
}

// mainColumnsAfterSplit 是拆分后主表保留的列，用于搬迁数据。
// 顺序与 Rust db.rs:300 的 INSERT 列表一致。
var mainColumnsAfterSplit = []string{
	"id", "timestamp", "method", "path", "alias_model", "actual_channel", "actual_model",
	"mapping_source", "status_code", "first_byte_ms", "latency_ms",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens",
	"cost_usd", "retry_count", "failover_chain", "error_message", "user_agent",
}

// tableColumns 返回某张表的列名集合。table 只接受包内常量，不做转义处理。
func (d *DB) tableColumns(table string) (map[string]bool, error) {
	rows, err := d.conn.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// migrateSchema 补齐缺失列、迁移文本时间戳、清理历史 FTS 表。幂等。
func (d *DB) migrateSchema() error {
	cols, err := d.tableColumns("audit_log")
	if err != nil {
		return fmt.Errorf("read audit_log schema: %w", err)
	}

	// 检测主表是否仍含大字段列（未拆分状态），决定补哪一组列。
	unmigrated := cols["request_body"]
	backfill := numericBackfillColumns
	if unmigrated {
		backfill = legacyBodyColumns
	}
	for _, col := range backfill {
		name := strings.Fields(col)[0]
		if cols[name] {
			continue
		}
		// 逐条补列并忽略失败：与 Rust 的 `.ok()` 一致——老库不该因为补列失败就打不开。
		if _, err := d.conn.Exec("ALTER TABLE audit_log ADD COLUMN " + col); err != nil {
			log.Printf("audit: add column %s failed (ignored): %v", name, err)
		}
	}

	// 历史版本把 timestamp 存成文本，统一转成毫秒整数。
	var hasTextTS int
	if err := d.conn.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM audit_log WHERE typeof(timestamp) = 'text' LIMIT 1)",
	).Scan(&hasTextTS); err == nil && hasTextTS == 1 {
		res, err := d.conn.Exec(
			"UPDATE audit_log SET timestamp = CAST(strftime('%s', timestamp) AS INTEGER) * 1000 WHERE typeof(timestamp) = 'text'",
		)
		if err != nil {
			log.Printf("audit: text timestamp migration failed: %v", err)
		} else if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("audit: migrated %d records from text timestamp to epoch ms", n)
		}
	}

	// 历史版本建过 FTS5 全文索引但没有任何查询使用它，且 body 里的 base64 会让索引
	// 比原数据还大；这里幂等清理。
	if _, err := d.conn.Exec(
		"DROP TRIGGER IF EXISTS audit_log_ai;" +
			"DROP TRIGGER IF EXISTS audit_log_bodies_ai;" +
			"DROP TABLE IF EXISTS audit_log_fts;",
	); err != nil {
		return fmt.Errorf("drop legacy fts: %w", err)
	}
	return nil
}

// MigrateSplitBodies 把 7 个大文本字段从 audit_log 主表搬到副表 audit_log_bodies。
//
// 返回 true 表示本次执行了迁移，false 表示库已是拆分结构（幂等）。
// 对应 Rust `migrate_split_bodies`（db.rs:194）：先复制到副表 → 备份副表 →
// 重建主表 → 回填 → 重建副表并还原。任何一步失败都会回滚到 savepoint，
// 保留旧结构可用，不会丢数据。
func (d *DB) MigrateSplitBodies() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	cols, err := d.tableColumns("audit_log")
	if err != nil {
		return false, fmt.Errorf("read audit_log schema: %w", err)
	}
	if !cols["request_body"] {
		return false, nil // 已迁移
	}
	log.Printf("audit: starting body-split migration...")

	if _, err := d.conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return false, fmt.Errorf("disable foreign keys: %w", err)
	}
	defer func() { _, _ = d.conn.Exec("PRAGMA foreign_keys = ON") }()

	if _, err := d.conn.Exec("SAVEPOINT migrate"); err != nil {
		return false, fmt.Errorf("create savepoint: %w", err)
	}

	if err := d.splitBodiesSteps(); err != nil {
		if _, rbErr := d.conn.Exec("ROLLBACK TO migrate"); rbErr != nil {
			log.Printf("audit: rollback after failed migration failed: %v", rbErr)
		}
		if _, relErr := d.conn.Exec("RELEASE migrate"); relErr != nil {
			log.Printf("audit: release savepoint failed: %v", relErr)
		}
		return false, err
	}
	if _, err := d.conn.Exec("RELEASE migrate"); err != nil {
		return false, fmt.Errorf("release savepoint: %w", err)
	}
	// VACUUM 必须在事务外执行，回收重建表留下的 freelist 碎片。
	if _, err := d.conn.Exec("VACUUM"); err != nil {
		log.Printf("audit: VACUUM after migration failed (ignored): %v", err)
	}
	if _, err := d.conn.Exec("PRAGMA optimize"); err != nil {
		log.Printf("audit: PRAGMA optimize failed (ignored): %v", err)
	}
	log.Printf("audit: body-split migration completed")
	return true, nil
}

// splitBodiesSteps 执行拆分迁移的全部步骤；调用方负责 savepoint 与回滚。
func (d *DB) splitBodiesSteps() error {
	bodyList := strings.Join(bodyColumns, ", ")

	// 1. 确保副表存在，并把主表里的大字段复制过去（已存在的行不覆盖）。
	if _, err := d.conn.Exec(`CREATE TABLE IF NOT EXISTS audit_log_bodies (
		id INTEGER PRIMARY KEY REFERENCES audit_log(id),
		request_headers TEXT,
		forwarded_request_headers TEXT,
		request_body TEXT,
		forwarded_request_body TEXT,
		upstream_response_headers TEXT,
		response_headers TEXT,
		upstream_response_body TEXT,
		response_body TEXT
	)`); err != nil {
		return fmt.Errorf("create bodies table: %w", err)
	}
	if _, err := d.conn.Exec(fmt.Sprintf(
		"INSERT OR IGNORE INTO audit_log_bodies (id, %s) SELECT id, %s FROM audit_log",
		bodyList, bodyList,
	)); err != nil {
		return fmt.Errorf("copy bodies: %w", err)
	}

	// 2. 副表先改名备份：否则下面重命名主表时 SQLite 会把它的外键引用一起改到旧表名。
	if _, err := d.conn.Exec("ALTER TABLE audit_log_bodies RENAME TO audit_log_bodies_backup"); err != nil {
		return fmt.Errorf("rename bodies to backup: %w", err)
	}

	// 3. 清理历史 FTS 与触发器（重建后再也不需要）。
	if _, err := d.conn.Exec(
		"DROP TRIGGER IF EXISTS audit_log_ai;" +
			"DROP TRIGGER IF EXISTS audit_log_bodies_ai;" +
			"DROP TABLE IF EXISTS audit_log_fts;",
	); err != nil {
		return fmt.Errorf("drop legacy fts: %w", err)
	}

	// 4. 重建主表：只保留小字段。
	if _, err := d.conn.Exec("ALTER TABLE audit_log RENAME TO audit_log_old"); err != nil {
		return fmt.Errorf("rename old main table: %w", err)
	}
	if _, err := d.conn.Exec(`CREATE TABLE audit_log (
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
	)`); err != nil {
		return fmt.Errorf("create new main table: %w", err)
	}
	mainList := strings.Join(mainColumnsAfterSplit, ", ")
	if _, err := d.conn.Exec(fmt.Sprintf(
		"INSERT INTO audit_log (%s) SELECT %s FROM audit_log_old", mainList, mainList,
	)); err != nil {
		return fmt.Errorf("copy main table data: %w", err)
	}
	if _, err := d.conn.Exec("DROP TABLE audit_log_old"); err != nil {
		return fmt.Errorf("drop old main table: %w", err)
	}

	// 5. 重建副表（外键正确指向新主表），再从备份还原数据。
	if _, err := d.conn.Exec(`CREATE TABLE audit_log_bodies (
		id INTEGER PRIMARY KEY REFERENCES audit_log(id),
		request_headers TEXT,
		forwarded_request_headers TEXT,
		request_body TEXT,
		forwarded_request_body TEXT,
		upstream_response_headers TEXT,
		response_headers TEXT,
		upstream_response_body TEXT,
		response_body TEXT
	)`); err != nil {
		return fmt.Errorf("recreate bodies table: %w", err)
	}
	if _, err := d.conn.Exec(fmt.Sprintf(
		"INSERT INTO audit_log_bodies (id, %s) SELECT id, %s FROM audit_log_bodies_backup",
		bodyList, bodyList,
	)); err != nil {
		return fmt.Errorf("restore bodies data: %w", err)
	}
	if _, err := d.conn.Exec("DROP TABLE audit_log_bodies_backup"); err != nil {
		return fmt.Errorf("drop bodies backup: %w", err)
	}

	// 6. 重建索引：主表是新建的，旧索引随 audit_log_old 一并被删掉了。
	if _, err := d.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log(timestamp);
		CREATE INDEX IF NOT EXISTS idx_audit_model ON audit_log(alias_model);
		CREATE INDEX IF NOT EXISTS idx_audit_channel ON audit_log(actual_channel);
		CREATE INDEX IF NOT EXISTS idx_audit_status ON audit_log(status_code);
		CREATE INDEX IF NOT EXISTS idx_audit_channel_ts ON audit_log(actual_channel, timestamp);
		CREATE INDEX IF NOT EXISTS idx_audit_ts_status ON audit_log(timestamp, status_code);`,
	); err != nil {
		return fmt.Errorf("recreate indexes: %w", err)
	}
	return nil
}
