use parking_lot::Mutex;
use rusqlite::{params, Connection};
use std::sync::Arc;

use super::models::AuditEntry;

/// 详情副表（audit_log_bodies）最多保留的最近记录条数。
/// 列表/看板数据在主表 audit_log，不受影响；超出的详情大字段会被裁剪。
pub const BODIES_RETAIN_LATEST: u64 = 1000;

/// 审计日志数据库，基于 SQLite 实现，线程安全
pub struct AuditDb {
    conn: Mutex<Connection>, // 互斥锁保护的数据库连接
}

impl AuditDb {
    /// 打开数据库并初始化表结构，设置 WAL 模式以提升并发性能
    pub fn new(db_path: &str) -> Result<Arc<Self>, String> {
        let conn = Connection::open(db_path)
            .map_err(|e| format!("Failed to open database: {}", e))?;

        conn.execute_batch(
            "PRAGMA journal_mode=WAL;
             PRAGMA synchronous=NORMAL;
             PRAGMA busy_timeout=5000;
             PRAGMA wal_autocheckpoint=1000;
             PRAGMA journal_size_limit=32768;"
        ).map_err(|e| format!("Failed to set pragmas: {}", e))?;

        // 开启增量式自动清空：删除数据释放的页面可通过 incremental_vacuum 归还给操作系统，
        // 否则文件只会增长不会收缩。仅在全新数据库（无任何表）时立即生效；已存在表的旧库
        // 需要一次性 VACUUM 才能切换模式，这里设置好意图，由运维在磁盘空间充足时执行一次 VACUUM。
        conn.execute_batch("PRAGMA auto_vacuum = INCREMENTAL;")
            .map_err(|e| format!("Failed to set auto_vacuum: {}", e))?;

        // Recover space left by a previous crash or an interrupted checkpoint.
        // TRUNCATE is safe here because initialization owns the only connection.
        conn.execute_batch("PRAGMA wal_checkpoint(TRUNCATE);")
            .map_err(|e| format!("Failed to checkpoint WAL: {}", e))?;

        let db = Arc::new(Self {
            conn: Mutex::new(conn),
        });

        db.initialize_tables()?;
        Ok(db)
    }

    /// 初始化审计日志表、索引及 FTS5 全文搜索表
    /// 获取当前 UTC 毫秒时间戳
    fn now_ms() -> i64 {
        chrono::Utc::now().timestamp_millis()
    }

    /// 初始化审计日志表、索引及 FTS5 全文搜索表
    fn initialize_tables(&self) -> Result<(), String> {
        let conn = self.conn.lock();
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS audit_log (
                id                      INTEGER PRIMARY KEY AUTOINCREMENT,
                timestamp               INTEGER NOT NULL DEFAULT 0,
                method                  TEXT NOT NULL,
                path                    TEXT NOT NULL,
                request_headers         TEXT,
                forwarded_request_headers TEXT,
                request_body            TEXT,
                forwarded_request_body  TEXT,
                alias_model             TEXT NOT NULL,
                actual_channel          TEXT,
                actual_model            TEXT,
                mapping_source          TEXT,
                upstream_response_headers TEXT,
                response_headers        TEXT,
                upstream_response_body  TEXT,
                response_body           TEXT,
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

            -- 复合索引：get_channel_stats 热路径（每5秒×每个渠道）
            CREATE INDEX IF NOT EXISTS idx_audit_channel_ts ON audit_log(actual_channel, timestamp);
            -- 复合索引：query_list 按时间范围 + 状态筛选
            CREATE INDEX IF NOT EXISTS idx_audit_ts_status ON audit_log(timestamp, status_code);"
        ).map_err(|e| format!("Failed to create tables: {}", e))?;

        // 检测主表是否仍含大字段列（未迁移状态）
        let has_body_cols: bool = conn.prepare("PRAGMA table_info(audit_log)")
            .and_then(|mut stmt| {
                let rows = stmt.query_map([], |row| {
                    Ok(row.get::<_, String>(1)?)
                })?;
                let mut found = false;
                for name in rows {
                    if name.as_deref() == Ok("request_body") {
                        found = true;
                        break;
                    }
                }
                Ok(found)
            })
            .unwrap_or(true); // 出错保守为未迁移

        // 兼容旧版本：仅在未迁移时尝试添加新增的列，已存在则忽略
        if has_body_cols {
            for col in &[
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
            ] {
                conn.execute_batch(&format!("ALTER TABLE audit_log ADD COLUMN {};", col)).ok();
            }
        } else {
            // 已迁移：仅需补齐数值列（如果从很旧版本迁移）
            for col in &[
                "cache_read_tokens INTEGER",
                "cache_creation_tokens INTEGER",
                "first_byte_ms INTEGER",
                "cost_usd REAL",
            ] {
                conn.execute_batch(&format!("ALTER TABLE audit_log ADD COLUMN {};", col)).ok();
            }
        }

        // 迁移旧版文本时间戳为毫秒整数
        // 检测方式：timestamp 列中存在非数字内容（如 "2024-" 开头）
        let has_text_ts: bool = conn.query_row(
            "SELECT EXISTS(SELECT 1 FROM audit_log WHERE typeof(timestamp) = 'text' LIMIT 1)",
            [], |row| row.get(0),
        ).unwrap_or(false);
        if has_text_ts {
            // strftime('%s', ...) 返回秒级时间戳，* 1000 转毫秒
            let migrated = conn.execute(
                "UPDATE audit_log SET timestamp = CAST(strftime('%s', timestamp) AS INTEGER) * 1000
                 WHERE typeof(timestamp) = 'text'",
                [],
            ).unwrap_or(0);
            if migrated > 0 {
                tracing::info!("Migrated {} audit records from text timestamp to epoch ms", migrated);
            }
        }

        // 创建大字段副表（幂等）
        conn.execute_batch(
            "CREATE TABLE IF NOT EXISTS audit_log_bodies (
                id INTEGER PRIMARY KEY REFERENCES audit_log(id),
                request_headers TEXT,
                forwarded_request_headers TEXT,
                request_body TEXT,
                forwarded_request_body TEXT,
                upstream_response_headers TEXT,
                response_headers TEXT,
                upstream_response_body TEXT,
                response_body TEXT
            );"
        ).map_err(|e| format!("Failed to create audit_log_bodies: {}", e))?;

        // 历史版本曾创建过 FTS5 全文索引，但没有任何功能查询它（无 UI 搜索入口）。
        // 它对 request_body/response_body 全量建索引，body 里常含 base64 图片数据，
        // 索引体积可能比原始数据还大，是纯浪费。这里幂等地清理掉遗留的 FTS 表和触发器。
        conn.execute_batch(
            "DROP TRIGGER IF EXISTS audit_log_ai;
             DROP TRIGGER IF EXISTS audit_log_bodies_ai;
             DROP TABLE IF EXISTS audit_log_fts;"
        ).map_err(|e| format!("Failed to drop legacy FTS tables: {}", e))?;

        Ok(())
    }

    /// 将 7 个大 TEXT 字段从 audit_log 主表搬迁到 audit_log_bodies 副表。
    /// 返回 true 表示执行了迁移，false 表示已迁移过（幂等）。
    /// 迁移失败时回滚，保留老 schema 可用。
    pub fn migrate_split_bodies(&self) -> Result<bool, String> {
        let conn = self.conn.lock();

        // 检测主表是否仍有 request_body 列
        let has_body: bool = conn.prepare("PRAGMA table_info(audit_log)")
            .and_then(|mut stmt| {
                let rows = stmt.query_map([], |row| row.get::<_, String>(1))?;
                let mut found = false;
                for name in rows {
                    if name.as_deref() == Ok("request_body") {
                        found = true;
                        break;
                    }
                }
                Ok(found)
            })
            .unwrap_or(false);

        if !has_body {
            tracing::info!("audit_log already migrated (no request_body column)");
            return Ok(false);
        }

        tracing::info!("Starting audit_log body-split migration...");

        // 禁用外键约束（重建表期间避免干扰）
        conn.execute_batch("PRAGMA foreign_keys = OFF;")
            .map_err(|e| format!("Failed to disable FK: {}", e))?;

        conn.execute_batch("SAVEPOINT migrate;")
            .map_err(|e| format!("Failed to create savepoint: {}", e))?;

        let result = (|| -> Result<(), String> {
            // 确保副表存在
            conn.execute_batch(
                "CREATE TABLE IF NOT EXISTS audit_log_bodies (
                    id INTEGER PRIMARY KEY REFERENCES audit_log(id),
                    request_headers TEXT,
                    forwarded_request_headers TEXT,
                    request_body TEXT,
                    forwarded_request_body TEXT,
                    upstream_response_headers TEXT,
                    response_headers TEXT,
                    upstream_response_body TEXT,
                    response_body TEXT
                );"
            ).map_err(|e| format!("Create bodies table: {}", e))?;

            // 搬迁数据到副表（已存在的行不覆盖）
            let copied = conn.execute(
                "INSERT OR IGNORE INTO audit_log_bodies
                 (id, request_headers, forwarded_request_headers, request_body,
                  forwarded_request_body, upstream_response_headers, response_headers,
                  upstream_response_body, response_body)
                 SELECT id, request_headers, forwarded_request_headers, request_body,
                  forwarded_request_body, upstream_response_headers, response_headers,
                  upstream_response_body, response_body
                 FROM audit_log",
                [],
            ).map_err(|e| format!("Copy bodies: {}", e))?;
            tracing::info!("Copied {} rows to audit_log_bodies", copied);

            // 将副表改名为备份（避免 RENAME TABLE 把它的 FK 引用更新为 audit_log_old）
            conn.execute_batch("ALTER TABLE audit_log_bodies RENAME TO audit_log_bodies_backup;")
                .map_err(|e| format!("Rename bodies to backup: {}", e))?;

            // 删除旧 FTS（content=audit_log）及其触发器，迁移后重建
            conn.execute_batch("DROP TRIGGER IF EXISTS audit_log_ai;")
                .map_err(|e| format!("Drop old trigger: {}", e))?;
            conn.execute_batch("DROP TRIGGER IF EXISTS audit_log_bodies_ai;")
                .map_err(|e| format!("Drop bodies trigger: {}", e))?;
            conn.execute_batch("DROP TABLE IF EXISTS audit_log_fts;")
                .map_err(|e| format!("Drop old FTS: {}", e))?;

            // 重建主表：去掉 7 个大字段
            conn.execute_batch("ALTER TABLE audit_log RENAME TO audit_log_old;")
                .map_err(|e| format!("Rename old table: {}", e))?;

            conn.execute_batch(
                "CREATE TABLE audit_log (
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
                );"
            ).map_err(|e| format!("Create new main table: {}", e))?;

            // 搬迁主表数据（仅保留小字段）
            conn.execute(
                "INSERT INTO audit_log
                 (id, timestamp, method, path, alias_model, actual_channel, actual_model,
                  mapping_source, status_code, first_byte_ms, latency_ms,
                  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
                  cost_usd, retry_count, failover_chain, error_message, user_agent)
                 SELECT id, timestamp, method, path, alias_model, actual_channel, actual_model,
                  mapping_source, status_code, first_byte_ms, latency_ms,
                  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
                  cost_usd, retry_count, failover_chain, error_message, user_agent
                 FROM audit_log_old",
                [],
            ).map_err(|e| format!("Copy main table data: {}", e))?;

            conn.execute_batch("DROP TABLE audit_log_old;")
                .map_err(|e| format!("Drop old table: {}", e))?;

            // 重建副表（正确 FK 指向新 audit_log）
            conn.execute_batch(
                "CREATE TABLE audit_log_bodies (
                    id INTEGER PRIMARY KEY REFERENCES audit_log(id),
                    request_headers TEXT,
                    forwarded_request_headers TEXT,
                    request_body TEXT,
                    forwarded_request_body TEXT,
                    upstream_response_headers TEXT,
                    response_headers TEXT,
                    upstream_response_body TEXT,
                    response_body TEXT
                );"
            ).map_err(|e| format!("Recreate bodies table: {}", e))?;
            // 重新填充副表数据（从临时备份）
            conn.execute(
                "INSERT INTO audit_log_bodies
                 (id, request_headers, forwarded_request_headers, request_body,
                  forwarded_request_body, upstream_response_headers, response_headers,
                  upstream_response_body, response_body)
                 SELECT id, request_headers, forwarded_request_headers, request_body,
                  forwarded_request_body, upstream_response_headers, response_headers,
                  upstream_response_body, response_body
                 FROM audit_log_bodies_backup",
                [],
            ).map_err(|e| format!("Restore bodies: {}", e))?;
            conn.execute_batch("DROP TABLE audit_log_bodies_backup;")
                .map_err(|e| format!("Drop bodies backup: {}", e))?;

            // 重建所有索引
            conn.execute_batch(
                "CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log(timestamp);
                 CREATE INDEX IF NOT EXISTS idx_audit_model ON audit_log(alias_model);
                 CREATE INDEX IF NOT EXISTS idx_audit_channel ON audit_log(actual_channel);
                 CREATE INDEX IF NOT EXISTS idx_audit_status ON audit_log(status_code);
                 CREATE INDEX IF NOT EXISTS idx_audit_channel_ts ON audit_log(actual_channel, timestamp);
                 CREATE INDEX IF NOT EXISTS idx_audit_ts_status ON audit_log(timestamp, status_code);"
            ).map_err(|e| format!("Recreate indexes: {}", e))?;

            Ok(())
        })();

        match result {
            Ok(()) => {
                conn.execute_batch("RELEASE SAVEPOINT migrate;")
                    .map_err(|e| format!("Release savepoint: {}", e))?;
                conn.execute_batch("PRAGMA foreign_keys = ON;").ok();
                // VACUUM 必须在事务外执行，回收 freelist 碎片
                tracing::info!("Running VACUUM to reclaim freelist space...");
                conn.execute_batch("VACUUM;")
                    .map_err(|e| format!("VACUUM failed: {}", e))?;
                conn.execute_batch("PRAGMA optimize;").ok();
                tracing::info!("audit_log body-split migration completed successfully");
                Ok(true)
            }
            Err(e) => {
                conn.execute_batch("ROLLBACK TO SAVEPOINT migrate;").ok();
                conn.execute_batch("RELEASE SAVEPOINT migrate;").ok();
                conn.execute_batch("PRAGMA foreign_keys = ON;").ok();
                tracing::error!("audit_log migration failed, old schema preserved: {}", e);
                Err(format!("Migration failed (old schema preserved): {}", e))
            }
        }
    }

    // ========== 分阶段写入 API ==========
    // 请求生命周期：create → update_route → update_forwarded_request → update_response → update_complete

    /// 阶段1：请求进入，创建审计记录，返回 audit_id
    pub fn create(&self, method: &str, path: &str, alias_model: &str,
                  request_headers: Option<&str>, request_body: Option<&str>,
                  user_agent: Option<&str>) -> Result<i64, String> {
        let conn = self.conn.lock();
        conn.execute(
            "INSERT INTO audit_log (timestamp, method, path, alias_model, user_agent)
             VALUES (?1, ?2, ?3, ?4, ?5)",
            params![Self::now_ms(), method, path, alias_model, user_agent],
        ).map_err(|e| format!("Failed to create audit log: {}", e))?;
        let id = conn.last_insert_rowid();
        // 大字段写入副表
        conn.execute(
            "INSERT INTO audit_log_bodies (id, request_headers, request_body)
             VALUES (?1, ?2, ?3)",
            params![id, request_headers, request_body],
        ).map_err(|e| format!("Failed to create audit log bodies: {}", e))?;
        Ok(id)
    }

    /// 阶段2：路由决策完成，更新渠道和模型映射信息
    pub fn update_route(&self, id: i64, channel: &str, model: &str, mapping_source: &str) -> Result<(), String> {
        let conn = self.conn.lock();
        conn.execute(
            "UPDATE audit_log SET actual_channel=?2, actual_model=?3, mapping_source=?4 WHERE id=?1",
            params![id, channel, model, mapping_source],
        ).map_err(|e| format!("update_route failed: {}", e))?;
        Ok(())
    }

    /// 阶段3：转发请求构建完成，记录转发的请求头和请求体（写入副表）
    pub fn update_forwarded_request(&self, id: i64, headers: Option<&str>, body: Option<&str>) -> Result<(), String> {
        let conn = self.conn.lock();
        conn.execute(
            "UPDATE audit_log_bodies SET forwarded_request_headers=?2, forwarded_request_body=?3 WHERE id=?1",
            params![id, headers, body],
        ).map_err(|e| format!("update_forwarded_request failed: {}", e))?;
        Ok(())
    }

    /// 阶段3c：流式响应开始，记录首字节耗时（主表）和响应头（副表）
    pub fn update_first_byte(&self, id: i64, first_byte_ms: u64,
                             upstream_headers: Option<&str>,
                             response_headers: Option<&str>) -> Result<(), String> {
        let conn = self.conn.lock();
        // 主表：首字节耗时
        conn.execute(
            "UPDATE audit_log SET first_byte_ms=?2 WHERE id=?1",
            params![id, first_byte_ms as i64],
        ).map_err(|e| format!("update_first_byte failed: {}", e))?;
        // 副表：响应头（COALESCE 避免覆盖已有值）
        if let Some(h) = upstream_headers {
            conn.execute(
                "UPDATE audit_log_bodies SET upstream_response_headers=COALESCE(?2, upstream_response_headers) WHERE id=?1",
                params![id, h],
            ).map_err(|e| format!("update_first_byte upstream_headers failed: {}", e))?;
        }
        if let Some(h) = response_headers {
            conn.execute(
                "UPDATE audit_log_bodies SET response_headers=COALESCE(?2, response_headers) WHERE id=?1",
                params![id, h],
            ).map_err(|e| format!("update_first_byte response_headers failed: {}", e))?;
        }
        Ok(())
    }

    /// 阶段4：收到渠道响应，记录上游响应头/体、转发响应头/体、token 统计
    pub fn update_response(&self, id: i64, status_code: u16, latency_ms: u64,
                           upstream_headers: Option<&str>, upstream_body: Option<&str>,
                           response_headers: Option<&str>, response_body: Option<&str>,
                           tokens: &TokenUsage, cost_usd: Option<f64>) -> Result<(), String> {
        let conn = self.conn.lock();
        // 主表：数值 + 状态
        conn.execute(
            "UPDATE audit_log SET status_code=?2, first_byte_ms=?3, latency_ms=?3,
             input_tokens=?4, output_tokens=?5,
             cache_read_tokens=?6, cache_creation_tokens=?7, cost_usd=?8 WHERE id=?1",
            params![id, status_code as i64, latency_ms as i64,
                    tokens.input.map(|t| t as i64), tokens.output.map(|t| t as i64),
                    tokens.cache_read.map(|t| t as i64), tokens.cache_creation.map(|t| t as i64),
                    cost_usd],
        ).map_err(|e| format!("update_response failed: {}", e))?;
        // 副表：响应头 + 响应体
        conn.execute(
            "UPDATE audit_log_bodies SET upstream_response_headers=?2, upstream_response_body=?3,
             response_headers=?4, response_body=?5 WHERE id=?1",
            params![id, upstream_headers, upstream_body, response_headers, response_body],
        ).map_err(|e| format!("update_response bodies failed: {}", e))?;
        Ok(())
    }

    /// 阶段4b：流式响应结束，更新完整响应体、token 统计和最终耗时。
    /// raw_stream 为上游原始流；forwarded_stream 为转换后下发给客户端的流（透传场景传 None）。
    /// upstream_status 为上游真实 HTTP 状态码（>= 400 时优先采信，避免内容检测漏判）。
    /// upstream_response_body 记录上游原始（组装）；response_body 记录客户端实际收到的内容。
    pub fn update_streaming_response(&self, id: i64, raw_stream: &str, forwarded_stream: Option<&str>, upstream_status: Option<u16>, total_latency_ms: u64, cost_usd: Option<f64>) -> Result<(), String> {
        let upstream_assembled = assemble_streaming_response(raw_stream);
        // 转发响应也组装为最终结果对象（Responses/Chat/Anthropic 均支持），
        // 展示拼合后的完整响应而非逐条 SSE 事件；透传场景沿用上游组装结果。
        let response_body = match forwarded_stream {
            Some(fwd) => assemble_streaming_response(fwd),
            None => upstream_assembled.clone(),
        };
        // token/status/error 从上游原始流提取（含完整 usage，最准确）
        let (detected_status, error_msg) = detect_stream_error(raw_stream);
        let tokens = extract_tokens_from_stream(raw_stream);
        // status 优先级：上游真实 HTTP 错误（>=400）> SSE 内容里检测到的错误码 > 已有值 > 200
        let upstream_err = upstream_status.filter(|s| *s >= 400);
        let final_status: Option<i64> = upstream_err
            .map(|s| s as i64)
            .or(detected_status.map(|s| s as i64));
        let conn = self.conn.lock();
        // 主表：状态 + token + 延迟
        conn.execute(
            "UPDATE audit_log SET
             status_code=COALESCE(?2, status_code, 200), error_message=COALESCE(?3, error_message),
             input_tokens=COALESCE(?4, input_tokens), output_tokens=COALESCE(?5, output_tokens),
             cache_read_tokens=COALESCE(?6, cache_read_tokens), cache_creation_tokens=COALESCE(?7, cache_creation_tokens),
             cost_usd=COALESCE(?8, cost_usd),
             latency_ms=?9
             WHERE id=?1",
            params![id, final_status, error_msg,
                    tokens.input.map(|t| t as i64), tokens.output.map(|t| t as i64),
                    tokens.cache_read.map(|t| t as i64), tokens.cache_creation.map(|t| t as i64),
                    cost_usd,
                    total_latency_ms as i64],
        ).map_err(|e| format!("update_streaming_response failed: {}", e))?;
        // 副表：响应体
        conn.execute(
            "UPDATE audit_log_bodies SET response_body=?2, upstream_response_body=?3 WHERE id=?1",
            params![id, response_body, upstream_assembled],
        ).map_err(|e| format!("update_streaming_response bodies failed: {}", e))?;
        Ok(())
    }

    /// 阶段5：请求失败，记录错误信息
    pub fn update_error(&self, id: i64, status_code: u16, latency_ms: u64, error: &str) -> Result<(), String> {
        let conn = self.conn.lock();
        conn.execute(
            "UPDATE audit_log SET status_code=?2, first_byte_ms=?3, latency_ms=?3, error_message=?4 WHERE id=?1",
            params![id, status_code as i64, latency_ms as i64, error],
        ).map_err(|e| format!("update_error failed: {}", e))?;
        Ok(())
    }

    /// 记录本次请求经历的重试次数与故障转移链路（渠道 id 序列）
    pub fn update_retry_info(&self, id: i64, retry_count: u32, failover_chain: &[String]) -> Result<(), String> {
        let chain = if failover_chain.is_empty() {
            None
        } else {
            Some(failover_chain.join(" -> "))
        };
        let conn = self.conn.lock();
        conn.execute(
            "UPDATE audit_log SET retry_count=?2, failover_chain=?3 WHERE id=?1",
            params![id, retry_count as i64, chain],
        ).map_err(|e| format!("update_retry_info failed: {}", e))?;
        Ok(())
    }

    /// 轻量列表查询，支持多条件筛选：时间范围、渠道、模型、状态
    pub fn query_list(
        &self,
        model_filter: Option<&str>,
        channel_filter: Option<&str>,
        status_filter: Option<&str>,  // "success" | "error" | "pending"
        actual_model_filter: Option<&str>,
        path_filter: Option<&str>,
        time_from: Option<&str>,      // ISO 8601 时间
        time_to: Option<&str>,
        limit: u32,
    ) -> Result<Vec<AuditListItem>, String> {
        let conn = self.conn.lock();

        let select = "SELECT id, timestamp, method, path, alias_model, actual_channel, actual_model,
             mapping_source, status_code, first_byte_ms, latency_ms, input_tokens, output_tokens,
             cache_read_tokens, cache_creation_tokens, cost_usd, retry_count, failover_chain, error_message FROM audit_log";

        // 动态构建 WHERE 条件
        let mut conditions: Vec<String> = Vec::new();
        let mut bound: Vec<Box<dyn rusqlite::ToSql>> = Vec::new();

        if let Some(m) = model_filter {
            if !m.is_empty() {
                conditions.push(format!("alias_model = ?{}", bound.len() + 1));
                bound.push(Box::new(m.to_string()));
            }
        }
        if let Some(c) = channel_filter {
            if !c.is_empty() {
                conditions.push(format!("actual_channel = ?{}", bound.len() + 1));
                bound.push(Box::new(c.to_string()));
            }
        }
        if let Some(s) = status_filter {
            match s {
                "success" => conditions.push("status_code >= 200 AND status_code < 300".to_string()),
                "error" => conditions.push("(status_code >= 400 OR error_message IS NOT NULL)".to_string()),
                "pending" => conditions.push("status_code IS NULL".to_string()),
                _ => {}
            }
        }
        if let Some(am) = actual_model_filter {
            if !am.is_empty() {
                conditions.push(format!("actual_model = ?{}", bound.len() + 1));
                bound.push(Box::new(am.to_string()));
            }
        }
        if let Some(p) = path_filter {
            if !p.is_empty() {
                conditions.push(format!("path = ?{}", bound.len() + 1));
                bound.push(Box::new(p.to_string()));
            }
        }
        if let Some(from) = time_from {
            if !from.is_empty() {
                conditions.push(format!("timestamp >= ?{}", bound.len() + 1));
                bound.push(Box::new(from.parse::<i64>().unwrap_or(0)));
            }
        }
        if let Some(to) = time_to {
            if !to.is_empty() {
                conditions.push(format!("timestamp <= ?{}", bound.len() + 1));
                bound.push(Box::new(to.parse::<i64>().unwrap_or(i64::MAX)));
            }
        }

        let where_clause = if conditions.is_empty() {
            String::new()
        } else {
            format!(" WHERE {}", conditions.join(" AND "))
        };

        bound.push(Box::new(limit as i64));
        let limit_clause = format!(" ORDER BY id DESC LIMIT ?{}", bound.len());

        let sql = format!("{}{}{}", select, where_clause, limit_clause);
        let mut stmt = conn.prepare(&sql)
            .map_err(|e| format!("Failed to prepare query: {}", e))?;

        let params_refs: Vec<&dyn rusqlite::ToSql> = bound.iter().map(|p| p.as_ref()).collect();

        let entries = stmt.query_map(params_refs.as_slice(), |row| {
            Ok(AuditListItem {
                id: row.get(0)?,
                timestamp: row.get(1)?,
                method: row.get(2)?,
                path: row.get(3)?,
                alias_model: row.get(4)?,
                actual_channel: row.get(5)?,
                actual_model: row.get(6)?,
                mapping_source: row.get(7)?,
                status_code: row.get::<_, Option<i64>>(8)?.map(|v| v as u16),
                first_byte_ms: row.get::<_, Option<i64>>(9)?.map(|v| v as u64),
                latency_ms: row.get::<_, Option<i64>>(10)?.map(|v| v as u64),
                input_tokens: row.get::<_, Option<i64>>(11)?.map(|v| v as u64),
                output_tokens: row.get::<_, Option<i64>>(12)?.map(|v| v as u64),
                cache_read_tokens: row.get::<_, Option<i64>>(13)?.map(|v| v as u64),
                cache_creation_tokens: row.get::<_, Option<i64>>(14)?.map(|v| v as u64),
                cost_usd: row.get(15)?,
                retry_count: row.get::<_, i64>(16)? as u32,
                failover_chain: row.get(17)?,
                error_message: row.get(18)?,
            })
        })
        .map_err(|e| format!("Query failed: {}", e))?
        .filter_map(|r| r.ok())
        .collect();

        Ok(entries)
    }

    /// 按 ID 获取单条审计记录的完整详情（JOIN 主表 + 副表）
    pub fn get_by_id(&self, id: i64) -> Result<Option<AuditEntry>, String> {
        let conn = self.conn.lock();

        let sql = "SELECT
            a.id, a.timestamp, a.method, a.path, a.alias_model, a.actual_channel, a.actual_model,
            a.mapping_source, a.status_code, a.first_byte_ms, a.latency_ms,
            a.input_tokens, a.output_tokens, a.cache_read_tokens, a.cache_creation_tokens,
            a.cost_usd, a.retry_count, a.failover_chain, a.error_message, a.user_agent,
            b.request_headers, b.forwarded_request_headers, b.request_body, b.forwarded_request_body,
            b.upstream_response_headers, b.response_headers, b.upstream_response_body, b.response_body
            FROM audit_log a
            LEFT JOIN audit_log_bodies b ON a.id = b.id
            WHERE a.id = ?1";

        let mut stmt = conn.prepare(sql)
            .map_err(|e| format!("Failed to prepare query: {}", e))?;

        let result = stmt.query_row(params![id], |row| {
            Ok(AuditEntry {
                id: row.get(0)?,
                timestamp: row.get(1)?,
                method: row.get(2)?,
                path: row.get(3)?,
                request_headers: row.get(20)?,
                forwarded_request_headers: row.get(21)?,
                request_body: row.get(22)?,
                forwarded_request_body: row.get(23)?,
                alias_model: row.get(4)?,
                actual_channel: row.get(5)?,
                actual_model: row.get(6)?,
                mapping_source: row.get(7)?,
                upstream_response_headers: row.get(24)?,
                response_headers: row.get(25)?,
                upstream_response_body: row.get(26)?,
                response_body: row.get(27)?,
                status_code: row.get::<_, Option<i64>>(8)?.map(|v| v as u16),
                first_byte_ms: row.get::<_, Option<i64>>(9)?.map(|v| v as u64),
                latency_ms: row.get::<_, Option<i64>>(10)?.map(|v| v as u64),
                input_tokens: row.get::<_, Option<i64>>(11)?.map(|v| v as u64),
                output_tokens: row.get::<_, Option<i64>>(12)?.map(|v| v as u64),
                cache_read_tokens: row.get::<_, Option<i64>>(13)?.map(|v| v as u64),
                cache_creation_tokens: row.get::<_, Option<i64>>(14)?.map(|v| v as u64),
                cost_usd: row.get(15)?,
                retry_count: row.get::<_, i64>(16)? as u32,
                failover_chain: row.get(17)?,
                error_message: row.get(18)?,
                user_agent: row.get(19)?,
            })
        });

        match result {
            Ok(entry) => Ok(Some(entry)),
            Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
            Err(e) => Err(format!("Query failed: {}", e)),
        }
    }

    /// 获取统计概要，支持按时间范围和渠道筛选
    pub fn get_stats(&self, period: &str, channel: Option<&str>) -> Result<AuditStats, String> {
        let conn = self.conn.lock();

        // 按本地时区的自然日起点计算 cutoff（而非简单回退 24h/7d/30d）
        let cutoff = {
            let now = chrono::Local::now();
            let days_back = match period {
                "7d" => 7,
                "30d" => 30,
                _ => 0, // today = 今天 00:00
            };
            let target_date = now.date_naive() - chrono::Duration::days(days_back);
            let midnight = target_date.and_hms_opt(0, 0, 0).unwrap();
            let local_midnight = midnight.and_local_timezone(chrono::Local).unwrap();
            local_midnight.timestamp_millis()
        };

        let mut conditions = vec!["timestamp >= ?1".to_string()];
        let mut params: Vec<Box<dyn rusqlite::ToSql>> = vec![Box::new(cutoff)];
        if let Some(ch) = channel {
            if !ch.is_empty() {
                params.push(Box::new(ch.to_string()));
                conditions.push(format!("actual_channel = ?{}", params.len()));
            }
        }

        let sql = format!(
            "SELECT
                COUNT(*),
                COALESCE(SUM(COALESCE(input_tokens, 0)), 0),
                COALESCE(SUM(COALESCE(output_tokens, 0)), 0),
                COALESCE(SUM(COALESCE(cache_read_tokens, 0)), 0),
                COALESCE(SUM(COALESCE(cache_creation_tokens, 0)), 0),
                COALESCE(SUM(CASE WHEN status_code IS NOT NULL AND (status_code >= 400 OR error_message IS NOT NULL) THEN 1 ELSE 0 END), 0),
                COALESCE(SUM(CASE WHEN status_code IS NOT NULL AND status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0),
                COALESCE(SUM(COALESCE(cost_usd, 0)), 0)
             FROM audit_log WHERE {}", conditions.join(" AND ")
        );

        let mut stmt = conn.prepare(&sql)
            .map_err(|e| format!("Stats query failed: {}", e))?;

        let params_refs: Vec<&dyn rusqlite::ToSql> = params.iter().map(|p| p.as_ref()).collect();

        let stats = stmt.query_row(params_refs.as_slice(), |row| {
            Ok(AuditStats {
                total_requests: row.get::<_, i64>(0)? as u64,
                input_tokens: row.get::<_, i64>(1)? as u64,
                output_tokens: row.get::<_, i64>(2)? as u64,
                cache_read_tokens: row.get::<_, i64>(3)? as u64,
                cache_creation_tokens: row.get::<_, i64>(4)? as u64,
                error_count: row.get::<_, i64>(5)? as u64,
                success_count: row.get::<_, i64>(6)? as u64,
                cost_usd: row.get::<_, f64>(7)?,
            })
        }).map_err(|e| format!("Stats parse failed: {}", e))?;

        Ok(stats)
    }

    /// 按天聚合 token 用量，返回最近 N 天的热力图数据
    pub fn get_token_heatmap(&self, days: u32) -> Result<Vec<TokenDailyHeat>, String> {
        let conn = self.conn.lock();
        let now = chrono::Local::now();
        let target_date = now.date_naive() - chrono::Duration::days(days as i64);
        let midnight = target_date.and_hms_opt(0, 0, 0).unwrap();
        let local_midnight = midnight.and_local_timezone(chrono::Local).unwrap();
        let cutoff = local_midnight.timestamp_millis();

        let sql = "SELECT date(timestamp/1000, 'unixepoch', 'localtime') as day,
                          COALESCE(SUM(COALESCE(input_tokens,0) + COALESCE(output_tokens,0) + COALESCE(cache_read_tokens,0) + COALESCE(cache_creation_tokens,0)), 0) as total_tokens
                   FROM audit_log WHERE timestamp >= ?1
                   GROUP BY day ORDER BY day";

        let mut stmt = conn.prepare(sql).map_err(|e| format!("Heatmap query failed: {}", e))?;
        let rows = stmt.query_map(params![cutoff], |row| {
            Ok(TokenDailyHeat {
                date: row.get::<_, String>(0)?,
                tokens: row.get::<_, i64>(1)? as u64,
            })
        }).map_err(|e| format!("Heatmap parse failed: {}", e))?;

        let mut data = Vec::new();
        for row in rows {
            data.push(row.map_err(|e| format!("Heatmap row error: {}", e))?);
        }
        Ok(data)
    }

    /// 清理超过指定天数的审计记录，返回删除的条数
    pub fn cleanup(&self, retention_days: u32) -> Result<u64, String> {
        let conn = self.conn.lock();
        let cutoff = Self::now_ms() - (retention_days as i64) * 24 * 3600 * 1000;
        // 临时禁用 FK（副表有 FK 引用主表，先删主表需要绕过约束）
        conn.execute_batch("PRAGMA foreign_keys = OFF;").ok();
        let deleted = conn.execute(
            "DELETE FROM audit_log WHERE timestamp < ?1",
            params![cutoff],
        ).map_err(|e| format!("Cleanup failed: {}", e))? as u64;

        if deleted > 0 {
            // 显式清理副表孤立行
            conn.execute(
                "DELETE FROM audit_log_bodies WHERE id NOT IN (SELECT id FROM audit_log)",
                [],
            ).ok();
            tracing::info!("Cleaned up {} audit records older than {} days", deleted, retention_days);
        }
        conn.execute_batch("PRAGMA foreign_keys = ON;").ok();

        Ok(deleted)
    }

    /// 裁剪详情大字段：只保留最近 `keep` 条记录的 request/response body 等大字段，
    /// 更早记录的行从 audit_log_bodies 中删除（主表 audit_log 的列表/统计数据不受影响，
    /// 详情页对应记录会显示为空）。返回被裁剪掉的行数。
    pub fn prune_bodies(&self, keep: u64) -> Result<u64, String> {
        let conn = self.conn.lock();
        conn.execute_batch("PRAGMA foreign_keys = OFF;").ok();
        let pruned = conn.execute(
            "DELETE FROM audit_log_bodies WHERE id NOT IN (
                 SELECT id FROM audit_log_bodies ORDER BY id DESC LIMIT ?1
             )",
            params![keep as i64],
        ).map_err(|e| format!("Prune bodies failed: {}", e))? as u64;
        conn.execute_batch("PRAGMA foreign_keys = ON;").ok();

        if pruned > 0 {
            tracing::info!("Pruned {} old audit detail bodies (keeping latest {})", pruned, keep);
        }
        Ok(pruned)
    }

    /// 增量归还已删除数据占用的磁盘页面给操作系统（需要 auto_vacuum=INCREMENTAL 生效）。
    /// `max_pages` 限制单次调用处理的页数，避免大库上一次性阻塞太久；传 0 表示不限制（全部归还）。
    /// 对未开启增量自动清空的旧库（历史数据库文件）本操作是空操作，不会报错。
    pub fn incremental_vacuum(&self, max_pages: u32) -> Result<(), String> {
        let conn = self.conn.lock();
        if max_pages == 0 {
            conn.execute_batch("PRAGMA incremental_vacuum;")
        } else {
            conn.execute_batch(&format!("PRAGMA incremental_vacuum({});", max_pages))
        }.map_err(|e| format!("incremental_vacuum failed: {}", e))
    }

    /// 强制清理（供设置页手动触发）：
    /// 1. 若 `retention_days > 0`，删除超期的主表记录（0 表示永久留存，跳过此步）；
    /// 2. 详情副表只保留最近 `keep` 条；
    /// 3. VACUUM 重建数据库文件，立即把释放的空间归还给操作系统
    ///    （对历史库还会顺带启用 auto_vacuum=INCREMENTAL，之后增量清理即可收缩文件）。
    ///
    /// 注意：VACUUM 期间数据库被独占，并发写入会短暂失败（busy_timeout 内重试），
    /// 大库上可能持续数分钟，因此调用方应放在阻塞线程中执行。
    pub fn force_cleanup(&self, retention_days: u32, keep: u64) -> Result<ForceCleanupResult, String> {
        let size_before = self.file_size_bytes()?;
        let deleted_old = if retention_days > 0 { self.cleanup(retention_days)? } else { 0 };
        let pruned = self.prune_bodies(keep)?;

        {
            let conn = self.conn.lock();
            // 先把 WAL 内容合并进主文件，再整体重建
            conn.execute_batch("PRAGMA wal_checkpoint(TRUNCATE);").ok();
            tracing::info!("force_cleanup: running VACUUM (database will be locked during rebuild)...");
            conn.execute_batch("VACUUM;")
                .map_err(|e| format!("VACUUM failed: {}", e))?;
        }

        let size_after = self.file_size_bytes()?;
        tracing::info!(
            "force_cleanup done: deleted_old={} pruned_bodies={} size {} -> {} bytes",
            deleted_old, pruned, size_before, size_after
        );
        Ok(ForceCleanupResult { deleted_old, pruned, size_before, size_after })
    }

    /// 数据库主文件当前占用的字节数（page_size × page_count，不含 WAL/SHM 附属文件）
    pub fn file_size_bytes(&self) -> Result<u64, String> {
        let conn = self.conn.lock();
        let page_size: i64 = conn.query_row("PRAGMA page_size", [], |r| r.get(0))
            .map_err(|e| format!("page_size query failed: {}", e))?;
        let page_count: i64 = conn.query_row("PRAGMA page_count", [], |r| r.get(0))
            .map_err(|e| format!("page_count query failed: {}", e))?;
        Ok((page_size * page_count) as u64)
    }

    /// 数据库概况（供设置页展示）：文件大小、主表记录数、详情副表记录数
    pub fn status(&self) -> Result<DbStatus, String> {
        let size_bytes = self.file_size_bytes()?;
        let conn = self.conn.lock();
        let total_records: i64 = conn.query_row("SELECT COUNT(*) FROM audit_log", [], |r| r.get(0))
            .map_err(|e| format!("count audit_log failed: {}", e))?;
        let detail_records: i64 = conn.query_row("SELECT COUNT(*) FROM audit_log_bodies", [], |r| r.get(0))
            .map_err(|e| format!("count audit_log_bodies failed: {}", e))?;
        Ok(DbStatus { size_bytes, total_records, detail_records })
    }

    /// 获取指定渠道按时间段（本地时区自然日）的聚合统计
    pub fn get_channel_stats_by_period(&self, channel_id: &str, period: &str) -> Result<ChannelDbStats, String> {
        let conn = self.conn.lock();
        let cutoff = {
            let now = chrono::Local::now();
            let days_back = match period {
                "7d" => 7,
                "30d" => 30,
                _ => 0,
            };
            let target_date = now.date_naive() - chrono::Duration::days(days_back);
            let midnight = target_date.and_hms_opt(0, 0, 0).unwrap();
            let local_midnight = midnight.and_local_timezone(chrono::Local).unwrap();
            local_midnight.timestamp_millis()
        };

        let mut stmt = conn.prepare(
            "SELECT
                COUNT(*) as total,
                AVG(CASE WHEN first_byte_ms > 0 THEN first_byte_ms END) as avg_first_byte,
                AVG(CASE WHEN latency_ms > 0 THEN latency_ms END) as avg_latency,
                COALESCE(SUM(CASE WHEN status_code IS NOT NULL AND status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END), 0) as success,
                COALESCE(SUM(CASE WHEN status_code IS NOT NULL AND (status_code >= 400 OR error_message IS NOT NULL) THEN 1 ELSE 0 END), 0) as errors,
                COALESCE(SUM(COALESCE(input_tokens, 0)), 0),
                COALESCE(SUM(COALESCE(output_tokens, 0)), 0),
                COALESCE(SUM(COALESCE(cache_read_tokens, 0)), 0),
                COALESCE(SUM(COALESCE(cache_creation_tokens, 0)), 0),
                COUNT(CASE WHEN status_code IS NOT NULL THEN 1 END) as finished,
                COALESCE(SUM(COALESCE(cost_usd, 0)), 0)
             FROM audit_log
             WHERE actual_channel = ?1
               AND timestamp >= ?2"
        ).map_err(|e| format!("Channel stats query failed: {}", e))?;

        let stats = stmt.query_row(
            rusqlite::params![channel_id, cutoff],
            |row| {
                let total: i64 = row.get(0)?;
                let avg_first_byte: Option<f64> = row.get(1)?;
                let avg_latency: Option<f64> = row.get(2)?;
                let success: i64 = row.get(3)?;
                let errors: i64 = row.get(4)?;
                let finished: i64 = row.get(9)?;
                Ok(ChannelDbStats {
                    total_requests: total as u64,
                    avg_first_byte_ms: avg_first_byte.map(|v| v as u64),
                    avg_latency_ms: avg_latency.map(|v| v as u64),
                    success_count: success as u64,
                    error_count: errors as u64,
                    success_rate: if finished > 0 { Some((success as f64 / finished as f64 * 100.0) as u64) } else { None },
                    input_tokens: row.get::<_, i64>(5)? as u64,
                    output_tokens: row.get::<_, i64>(6)? as u64,
                    cache_read_tokens: row.get::<_, i64>(7)? as u64,
                    cache_creation_tokens: row.get::<_, i64>(8)? as u64,
                    cost_usd: row.get::<_, f64>(10)?,
                })
            },
        ).map_err(|e| format!("Channel stats parse failed: {}", e))?;

        Ok(stats)
    }
}

/// 单个渠道基于审计日志的统计数据
#[derive(Debug, Clone, Default, serde::Serialize)]
pub struct ChannelDbStats {
    pub total_requests: u64,
    /// 首字节延迟均值（毫秒）；窗口内无样本时为 None
    pub avg_first_byte_ms: Option<u64>,
    /// 总延迟均值（毫秒）；窗口内无样本时为 None
    pub avg_latency_ms: Option<u64>,
    pub success_count: u64,
    pub error_count: u64,
    /// 成功率（百分比）；窗口内无请求时为 None
    pub success_rate: Option<u64>,
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cache_read_tokens: u64,
    pub cache_creation_tokens: u64,
    pub cost_usd: f64,
}

/// 审计列表项（轻量，不含报文内容）
#[derive(Debug, Clone, serde::Serialize)]
pub struct AuditListItem {
    pub id: i64,
    pub timestamp: i64, // UTC 毫秒时间戳
    pub method: String,
    pub path: String,
    pub alias_model: String,
    pub actual_channel: Option<String>,
    pub actual_model: Option<String>,
    pub mapping_source: Option<String>,
    pub status_code: Option<u16>,
    pub first_byte_ms: Option<u64>,
    pub latency_ms: Option<u64>,
    pub input_tokens: Option<u64>,
    pub output_tokens: Option<u64>,
    pub cache_read_tokens: Option<u64>,
    pub cache_creation_tokens: Option<u64>,
    pub retry_count: u32,
    pub failover_chain: Option<String>,
    pub error_message: Option<String>,
    pub cost_usd: Option<f64>,
}

/// 审计统计概要
#[derive(Debug, Clone, serde::Serialize)]
pub struct AuditStats {
    pub total_requests: u64,
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cache_read_tokens: u64,
    pub cache_creation_tokens: u64,
    pub error_count: u64,
    pub success_count: u64,
    pub cost_usd: f64,
}

/// 强制清理的结果（供设置页展示）
#[derive(Debug, Clone, serde::Serialize)]
pub struct ForceCleanupResult {
    pub deleted_old: u64,   // 删除的超期主表记录数
    pub pruned: u64,        // 裁剪掉的详情副表行数
    pub size_before: u64,   // 清理前文件大小（字节）
    pub size_after: u64,    // 清理后文件大小（字节）
}

/// 数据库概况（供设置页展示）
#[derive(Debug, Clone, serde::Serialize)]
pub struct DbStatus {
    pub size_bytes: u64,
    pub total_records: i64,   // 主表记录数（列表/看板数据）
    pub detail_records: i64,  // 详情副表记录数
}

/// 按天聚合的 Token 用量（用于活动热力图）
#[derive(Debug, Clone, serde::Serialize)]
pub struct TokenDailyHeat {
    pub date: String,  // "2026-08-22" 格式
    pub tokens: u64,   // 当天 total tokens (input + output + cache)
}

/// Token 用量统计，兼容 OpenAI 和 Anthropic 格式
#[derive(Debug, Clone, Default)]
pub struct TokenUsage {
    pub input: Option<u64>,           // 输入 token（prompt_tokens / input_tokens）
    pub output: Option<u64>,          // 输出 token（completion_tokens / output_tokens）
    pub cache_read: Option<u64>,      // 缓存读取 token
    pub cache_creation: Option<u64>,  // 缓存创建 token
}

/// 从 JSON 响应体中提取 token 用量（兼容 OpenAI Chat / Responses / Anthropic 格式）
pub fn extract_tokens_from_json(body: &serde_json::Value) -> TokenUsage {
    // 优先查找顶层 usage，其次查找 response.usage（Responses API 的 response.completed 事件格式）
    let usage = body.get("usage")
        .or_else(|| body.get("response").and_then(|r| r.get("usage")));
    let usage = match usage {
        Some(u) => u,
        None => return TokenUsage::default(),
    };

    let input = usage.get("prompt_tokens")
        .or_else(|| usage.get("input_tokens"))
        .and_then(|v| v.as_u64());

    let output = usage.get("completion_tokens")
        .or_else(|| usage.get("output_tokens"))
        .and_then(|v| v.as_u64());

    // cache_read:
    //   Anthropic: cache_read_input_tokens
    //   OpenAI Chat: prompt_tokens_details.cached_tokens
    //   OpenAI Responses: input_tokens_details.cached_tokens
    let cache_read = usage.get("cache_read_input_tokens")
        .and_then(|v| v.as_u64())
        .or_else(|| {
            usage.get("prompt_tokens_details")
                .and_then(|d| d.get("cached_tokens"))
                .and_then(|v| v.as_u64())
        })
        .or_else(|| {
            usage.get("input_tokens_details")
                .and_then(|d| d.get("cached_tokens"))
                .and_then(|v| v.as_u64())
        });

    // cache_creation:
    //   Anthropic: cache_creation_input_tokens
    let cache_creation = usage.get("cache_creation_input_tokens")
        .and_then(|v| v.as_u64());

    TokenUsage { input, output, cache_read, cache_creation }
}

/// 从 SSE 流式响应中提取 token 用量
/// 扫描所有 data 行，找到包含 usage 的最后一条（通常在 message_delta 或最终 chunk 中）
/// 兼容三种格式：Anthropic SSE、OpenAI Chat SSE、OpenAI Responses SSE
pub fn extract_tokens_from_stream(raw_stream: &str) -> TokenUsage {
    let mut tokens = TokenUsage::default();

    for line in raw_stream.lines() {
        let data = match line.strip_prefix("data: ").or_else(|| line.strip_prefix("data:")) {
            Some(d) => d.trim(),
            None => continue,
        };
        if data == "[DONE]" || data.is_empty() { continue; }

        if let Ok(json) = serde_json::from_str::<serde_json::Value>(data) {
            // ---- Responses API 格式 ----
            // response.completed 事件: {"type":"response.completed","response":{"usage":{...}}}
            if json.get("type").and_then(|t| t.as_str()) == Some("response.completed") {
                if let Some(usage) = json.get("response").and_then(|r| r.get("usage")) {
                    if let Some(v) = usage.get("input_tokens").and_then(|v| v.as_u64()) {
                        if v > 0 { tokens.input = Some(v); }
                    }
                    if let Some(v) = usage.get("output_tokens").and_then(|v| v.as_u64()) {
                        if v > 0 { tokens.output = Some(v); }
                    }
                    // input_tokens_details.cached_tokens
                    if let Some(cached) = usage.get("input_tokens_details")
                        .and_then(|d| d.get("cached_tokens"))
                        .and_then(|v| v.as_u64()) {
                        if cached > 0 { tokens.cache_read = Some(cached); }
                    }
                }
                continue;
            }

            // ---- Anthropic 格式 ----
            // Anthropic message_delta 事件包含 usage
            if let Some(usage) = json.get("usage") {
                if let Some(v) = usage.get("input_tokens").and_then(|v| v.as_u64()) {
                    if v > 0 { tokens.input = Some(v); }
                }
                if let Some(v) = usage.get("output_tokens").and_then(|v| v.as_u64()) {
                    if v > 0 { tokens.output = Some(v); }
                }
                if let Some(v) = usage.get("cache_read_input_tokens").and_then(|v| v.as_u64()) {
                    if v > 0 { tokens.cache_read = Some(v); }
                }
                if let Some(v) = usage.get("cache_creation_input_tokens").and_then(|v| v.as_u64()) {
                    if v > 0 { tokens.cache_creation = Some(v); }
                }
            }

            // Anthropic message_start 事件也可能包含 usage
            if let Some(msg) = json.get("message") {
                if let Some(usage) = msg.get("usage") {
                    if let Some(v) = usage.get("input_tokens").and_then(|v| v.as_u64()) {
                        if v > 0 { tokens.input = Some(v); }
                    }
                    if let Some(v) = usage.get("cache_read_input_tokens").and_then(|v| v.as_u64()) {
                        if v > 0 { tokens.cache_read = Some(v); }
                    }
                    if let Some(v) = usage.get("cache_creation_input_tokens").and_then(|v| v.as_u64()) {
                        if v > 0 { tokens.cache_creation = Some(v); }
                    }
                }
            }

            // ---- OpenAI Chat 格式 ----
            // OpenAI 最终 chunk 可能包含 usage
            if let Some(usage) = json.get("usage") {
                if let Some(v) = usage.get("prompt_tokens").and_then(|v| v.as_u64()) {
                    if v > 0 { tokens.input = Some(v); }
                }
                if let Some(v) = usage.get("completion_tokens").and_then(|v| v.as_u64()) {
                    if v > 0 { tokens.output = Some(v); }
                }
                if let Some(cached) = usage.get("prompt_tokens_details")
                    .and_then(|d| d.get("cached_tokens"))
                    .and_then(|v| v.as_u64()) {
                    if cached > 0 { tokens.cache_read = Some(cached); }
                }
                // Responses API 的 input_tokens_details.cached_tokens（非 response.completed 事件中）
                if let Some(cached) = usage.get("input_tokens_details")
                    .and_then(|d| d.get("cached_tokens"))
                    .and_then(|v| v.as_u64()) {
                    if cached > 0 { tokens.cache_read = Some(cached); }
                }
            }
        }
    }

    tokens
}

/// 从 streaming 响应文本中检测是否包含错误信息
/// 返回 (错误状态码, 错误消息)，如果没有错误则返回 (None, None)
fn detect_stream_error(response_body: &str) -> (Option<u16>, Option<String>) {
    // 逐行解析 SSE 数据，查找 error 类型的事件
    for line in response_body.lines() {
        let data = line.strip_prefix("data: ").or_else(|| line.strip_prefix("data:"));
        let data = match data {
            Some(d) => d.trim(),
            None => continue,
        };

        // 跳过 [DONE] 标记
        if data == "[DONE]" {
            continue;
        }

        // 尝试解析为 JSON
        if let Ok(json) = serde_json::from_str::<serde_json::Value>(data) {
            // 检查 OpenAI 格式错误: {"error": {"message": "...", "type": "..."}}
            if let Some(error) = json.get("error") {
                let msg = error.get("message")
                    .and_then(|m| m.as_str())
                    .unwrap_or("Unknown stream error")
                    .to_string();
                let error_type = error.get("type")
                    .and_then(|t| t.as_str())
                    .unwrap_or("");

                // 根据错误类型推断状态码
                let status = match error_type {
                    "invalid_request_error" => 400,
                    "authentication_error" => 401,
                    "permission_error" => 403,
                    "not_found_error" => 404,
                    "rate_limit_error" => 429,
                    "overloaded_error" => 529,
                    _ => 500,
                };

                return (Some(status), Some(msg));
            }

            // 检查 Anthropic 格式错误: {"type": "error", "error": {"type": "...", "message": "..."}}
            if json.get("type").and_then(|t| t.as_str()) == Some("error") {
                if let Some(error) = json.get("error") {
                    let msg = error.get("message")
                        .and_then(|m| m.as_str())
                        .unwrap_or("Unknown stream error")
                        .to_string();
                    return (Some(400), Some(msg));
                }
            }
        }
    }

    (None, None)
}

/// 将 SSE 流式响应拼合为完整的响应 JSON
/// 支持 OpenAI Chat、Anthropic、OpenAI Responses 三种 SSE 格式
fn assemble_streaming_response(raw: &str) -> String {
    // 优先识别 OpenAI Responses 格式（事件 type 形如 response.*）
    if let Some(assembled) = assemble_responses_stream(raw) {
        return assembled;
    }

    let mut id = String::new();
    let mut model = String::new();
    let mut content = String::new();
    let mut input_tokens: u64 = 0;
    let mut output_tokens: u64 = 0;
    let mut finish_reason = String::new();
    let mut is_anthropic = false;

    for line in raw.lines() {
        // 解析 SSE data 行
        let data = match line.strip_prefix("data: ").or_else(|| line.strip_prefix("data:")) {
            Some(d) => d.trim(),
            None => continue,
        };

        if data == "[DONE]" || data.is_empty() {
            continue;
        }

        let json: serde_json::Value = match serde_json::from_str(data) {
            Ok(v) => v,
            Err(_) => continue,
        };

        // ---- Anthropic 格式 ----
        if let Some(event_type) = json.get("type").and_then(|t| t.as_str()) {
            match event_type {
                "message_start" => {
                    is_anthropic = true;
                    if let Some(msg) = json.get("message") {
                        id = msg.get("id").and_then(|v| v.as_str()).unwrap_or("").to_string();
                        model = msg.get("model").and_then(|v| v.as_str()).unwrap_or("").to_string();
                        if let Some(usage) = msg.get("usage") {
                            input_tokens = usage.get("input_tokens").and_then(|v| v.as_u64()).unwrap_or(0);
                        }
                    }
                }
                "content_block_delta" => {
                    if let Some(delta) = json.get("delta") {
                        if let Some(text) = delta.get("text").and_then(|t| t.as_str()) {
                            content.push_str(text);
                        }
                    }
                }
                "message_delta" => {
                    if let Some(delta) = json.get("delta") {
                        if let Some(sr) = delta.get("stop_reason").and_then(|s| s.as_str()) {
                            finish_reason = sr.to_string();
                        }
                    }
                    if let Some(usage) = json.get("usage") {
                        output_tokens = usage.get("output_tokens").and_then(|v| v.as_u64()).unwrap_or(output_tokens);
                    }
                }
                "error" => {
                    // 错误事件直接返回原始 JSON
                    return serde_json::to_string_pretty(&json).unwrap_or_else(|_| raw.to_string());
                }
                _ => {}
            }
            continue;
        }

        // ---- OpenAI 格式 ----
        if let Some(choices) = json.get("choices").and_then(|c| c.as_array()) {
            if let Some(choice) = choices.first() {
                if let Some(delta) = choice.get("delta") {
                    if let Some(text) = delta.get("content").and_then(|c| c.as_str()) {
                        content.push_str(text);
                    }
                }
                if let Some(fr) = choice.get("finish_reason").and_then(|f| f.as_str()) {
                    finish_reason = fr.to_string();
                }
            }
        }
        if id.is_empty() {
            if let Some(v) = json.get("id").and_then(|v| v.as_str()) {
                id = v.to_string();
            }
        }
        if model.is_empty() {
            if let Some(v) = json.get("model").and_then(|v| v.as_str()) {
                model = v.to_string();
            }
        }
        if let Some(usage) = json.get("usage") {
            input_tokens = usage.get("prompt_tokens")
                .or_else(|| usage.get("input_tokens"))
                .and_then(|v| v.as_u64()).unwrap_or(input_tokens);
            output_tokens = usage.get("completion_tokens")
                .or_else(|| usage.get("output_tokens"))
                .and_then(|v| v.as_u64()).unwrap_or(output_tokens);
        }
    }

    // 没有解析到任何内容，返回原始数据
    if content.is_empty() && id.is_empty() {
        return raw.to_string();
    }

    // 组装完整响应
    let assembled = if is_anthropic {
        serde_json::json!({
            "id": id,
            "type": "message",
            "role": "assistant",
            "model": model,
            "content": [{"type": "text", "text": content}],
            "stop_reason": if finish_reason.is_empty() { "end_turn" } else { &finish_reason },
            "usage": {
                "input_tokens": input_tokens,
                "output_tokens": output_tokens,
            }
        })
    } else {
        serde_json::json!({
            "id": id,
            "object": "chat.completion",
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": content},
                "finish_reason": if finish_reason.is_empty() { "stop" } else { &finish_reason },
            }],
            "usage": {
                "prompt_tokens": input_tokens,
                "completion_tokens": output_tokens,
                "total_tokens": input_tokens + output_tokens,
            }
        })
    };

    serde_json::to_string_pretty(&assembled).unwrap_or_else(|_| raw.to_string())
}

/// 拼合 OpenAI Responses 格式的 SSE 流为最终完整 response 对象。
/// 返回 None 表示流中不含 Responses 事件（非 Responses 格式，交回通用逻辑处理）。
fn assemble_responses_stream(raw: &str) -> Option<String> {
    let mut saw_responses_event = false;
    let mut final_response: Option<serde_json::Value> = None;
    let mut id = String::new();
    let mut model = String::new();
    let mut text = String::new();

    for line in raw.lines() {
        let data = match line.strip_prefix("data: ").or_else(|| line.strip_prefix("data:")) {
            Some(d) => d.trim(),
            None => continue,
        };
        if data == "[DONE]" || data.is_empty() {
            continue;
        }
        let json: serde_json::Value = match serde_json::from_str(data) {
            Ok(v) => v,
            Err(_) => continue,
        };
        let etype = match json.get("type").and_then(|t| t.as_str()) {
            Some(t) if t.starts_with("response.") => t,
            _ => continue,
        };
        saw_responses_event = true;

        match etype {
            // 终态事件内嵌完整 response 对象，直接采用（最完整）
            "response.completed" | "response.incomplete" | "response.failed" => {
                if let Some(resp) = json.get("response") {
                    final_response = Some(resp.clone());
                }
            }
            "response.output_text.delta" => {
                if let Some(d) = json.get("delta").and_then(|v| v.as_str()) {
                    text.push_str(d);
                }
            }
            _ => {
                // 从任意事件里尽力补齐 id/model
                if let Some(resp) = json.get("response") {
                    if id.is_empty() {
                        id = resp.get("id").and_then(|v| v.as_str()).unwrap_or("").to_string();
                    }
                    if model.is_empty() {
                        model = resp.get("model").and_then(|v| v.as_str()).unwrap_or("").to_string();
                    }
                }
            }
        }
    }

    if !saw_responses_event {
        return None;
    }

    // 优先返回终态事件里的完整 response 对象
    if let Some(resp) = final_response {
        return Some(serde_json::to_string_pretty(&resp).unwrap_or_else(|_| raw.to_string()));
    }

    // 无终态事件：用聚合的文本构造一个最小完整 response 对象
    let assembled = serde_json::json!({
        "id": id,
        "object": "response",
        "model": model,
        "status": "completed",
        "output": [{
            "type": "message",
            "role": "assistant",
            "content": [{"type": "output_text", "text": text, "annotations": []}]
        }]
    });
    Some(serde_json::to_string_pretty(&assembled).unwrap_or_else(|_| raw.to_string()))
}

#[cfg(test)]
mod assemble_tests {
    use super::assemble_streaming_response;

    #[test]
    fn test_assemble_responses_stream_uses_completed_object() {
        // 含 response.completed：应直接返回其内嵌的完整 response 对象
        let raw = concat!(
            "event: response.created\n",
            "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"m\",\"status\":\"in_progress\",\"output\":[]}}\n\n",
            "event: response.output_text.delta\n",
            "data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n",
            "event: response.completed\n",
            "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"你好世界\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n"
        );
        let out = assemble_streaming_response(raw);
        let v: serde_json::Value = serde_json::from_str(&out).expect("应为合法 JSON 对象，而非 SSE 事件流");
        assert_eq!(v["object"], "response");
        assert_eq!(v["status"], "completed");
        assert_eq!(v["output"][0]["content"][0]["text"], "你好世界");
        assert_eq!(v["usage"]["input_tokens"], 3);
        // 不应包含任何 SSE 事件前缀
        assert!(!out.contains("event: response."));
    }

    #[test]
    fn test_assemble_responses_stream_aggregates_deltas_without_completed() {
        // 无 completed：应聚合 output_text.delta 构造最终对象
        let raw = concat!(
            "event: response.created\n",
            "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_2\",\"model\":\"m2\"}}\n\n",
            "event: response.output_text.delta\n",
            "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello \"}\n\n",
            "event: response.output_text.delta\n",
            "data: {\"type\":\"response.output_text.delta\",\"delta\":\"World\"}\n\n"
        );
        let out = assemble_streaming_response(raw);
        let v: serde_json::Value = serde_json::from_str(&out).expect("应为合法 JSON 对象");
        assert_eq!(v["object"], "response");
        assert_eq!(v["output"][0]["content"][0]["text"], "Hello World");
        assert!(!out.contains("event: response."));
    }

    #[test]
    fn test_assemble_still_handles_openai_chat_stream() {
        // 非 Responses 格式仍走原有逻辑
        let raw = concat!(
            "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n",
            "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
            "data: [DONE]\n\n"
        );
        let out = assemble_streaming_response(raw);
        let v: serde_json::Value = serde_json::from_str(&out).expect("应为合法 JSON 对象");
        assert_eq!(v["object"], "chat.completion");
        assert_eq!(v["choices"][0]["message"]["content"], "hi");
    }
}

#[cfg(test)]
mod split_body_tests {
    use super::*;

    /// 创建内存数据库用于测试
    fn test_db() -> Arc<AuditDb> {
        AuditDb::new(":memory:").expect("Failed to create test db")
    }

    #[test]
    fn test_migrate_split_bodies_idempotent() {
        let db = test_db();
        // 新数据库：迁移应成功执行
        let result1 = db.migrate_split_bodies().expect("First migration should succeed");
        assert!(result1, "First migration should return true (executed)");

        // 再次迁移：应跳过（已迁移）
        let result2 = db.migrate_split_bodies().expect("Second migration should succeed");
        assert!(!result2, "Second migration should return false (already migrated)");
    }

    #[test]
    fn test_migrate_preserves_data() {
        let db = test_db();

        // 插入一条带大字段的记录（迁移前 schema）
        let id = db.create("POST", "/v1/chat", "gpt-4",
            Some("{\"auth\":\"bearer\"}"), Some("{\"messages\":[]}"),
            Some("test-agent")).unwrap();
        db.update_route(id, "ch1", "gpt-4-turbo", "auto").unwrap();
        db.update_forwarded_request(id, Some("{\"x-api-key\":\"...\"}"), Some("{\"forwarded\":true}")).unwrap();

        let tokens = TokenUsage { input: Some(100), output: Some(50), cache_read: None, cache_creation: None };
        db.update_response(id, 200, 150,
            Some("{\"upstream-h\":\"v\"}"), Some("{\"upstream\":\"body\"}"),
            Some("{\"resp-h\":\"v\"}"), Some("{\"resp\":\"body\"}"),
            &tokens, Some(0.0005)).unwrap();

        // 验证迁移前 get_by_id 能读到大字段
        let before = db.get_by_id(id).unwrap().unwrap();
        assert_eq!(before.request_headers.as_deref(), Some("{\"auth\":\"bearer\"}"));
        assert_eq!(before.response_body.as_deref(), Some("{\"resp\":\"body\"}"));

        // 执行迁移
        let migrated = db.migrate_split_bodies().unwrap();
        assert!(migrated);

        // 验证迁移后 get_by_id 返回一致数据
        let after = db.get_by_id(id).unwrap().unwrap();
        assert_eq!(after.id, id);
        assert_eq!(after.method, "POST");
        assert_eq!(after.path, "/v1/chat");
        assert_eq!(after.alias_model, "gpt-4");
        assert_eq!(after.actual_channel.as_deref(), Some("ch1"));
        assert_eq!(after.actual_model.as_deref(), Some("gpt-4-turbo"));
        assert_eq!(after.status_code, Some(200));
        assert_eq!(after.input_tokens, Some(100));
        assert_eq!(after.output_tokens, Some(50));
        // 大字段
        assert_eq!(after.request_headers.as_deref(), Some("{\"auth\":\"bearer\"}"));
        assert_eq!(after.request_body.as_deref(), Some("{\"messages\":[]}"));
        assert_eq!(after.forwarded_request_headers.as_deref(), Some("{\"x-api-key\":\"...\"}"));
        assert_eq!(after.forwarded_request_body.as_deref(), Some("{\"forwarded\":true}"));
        assert_eq!(after.upstream_response_headers.as_deref(), Some("{\"upstream-h\":\"v\"}"));
        assert_eq!(after.upstream_response_body.as_deref(), Some("{\"upstream\":\"body\"}"));
        assert_eq!(after.response_headers.as_deref(), Some("{\"resp-h\":\"v\"}"));
        assert_eq!(after.response_body.as_deref(), Some("{\"resp\":\"body\"}"));
        assert_eq!(after.user_agent.as_deref(), Some("test-agent"));
    }

    #[test]
    fn test_create_splits_tables() {
        let db = test_db();
        // 先迁移，确保新 schema
        db.migrate_split_bodies().unwrap();

        let id = db.create("POST", "/v1/chat", "claude-3",
            Some("{\"h\":\"v\"}"), Some("{\"body\":\"x\"}"),
            Some("ua")).unwrap();

        // 主表不应有 request_body 列
        let conn = db.conn.lock();
        let has_req_body: bool = conn.prepare("PRAGMA table_info(audit_log)")
            .and_then(|mut stmt| {
                let rows = stmt.query_map([], |row| row.get::<_, String>(1))?;
                let mut found = false;
                for name in rows {
                    if name.as_deref() == Ok("request_body") { found = true; break; }
                }
                Ok(found)
            }).unwrap();
        assert!(!has_req_body, "Main table should not have request_body after migration");

        // 副表应有该行
        let body_count: i64 = conn.query_row(
            "SELECT COUNT(*) FROM audit_log_bodies WHERE id = ?1",
            params![id], |row| row.get(0),
        ).unwrap();
        assert_eq!(body_count, 1);
        drop(conn);

        // get_by_id 应返回完整数据
        let entry = db.get_by_id(id).unwrap().unwrap();
        assert_eq!(entry.request_headers.as_deref(), Some("{\"h\":\"v\"}"));
        assert_eq!(entry.request_body.as_deref(), Some("{\"body\":\"x\"}"));
    }

    #[test]
    fn test_query_list_does_not_touch_bodies() {
        let db = test_db();
        db.migrate_split_bodies().unwrap();

        // 插入几条记录
        for i in 0..5 {
            let id = db.create("POST", &format!("/v1/{}", i), "gpt-4",
                Some("big-headers"), Some("big-body"), None).unwrap();
            let tokens = TokenUsage { input: Some(10), output: Some(5), cache_read: None, cache_creation: None };
            db.update_response(id, 200, 100, None, None, None, None, &tokens, Some(0.0001)).unwrap();
        }

        // query_list 应正常工作，只返回小字段
        let list = db.query_list(None, None, None, None, None, None, None, 10).unwrap();
        assert_eq!(list.len(), 5);
        assert_eq!(list[0].input_tokens, Some(10));

        // 用 EXPLAIN QUERY PLAN 验证 query_list 不碰 audit_log_bodies
        let conn = db.conn.lock();
        let plan: String = conn.prepare(
            "EXPLAIN QUERY PLAN SELECT id, timestamp, method, path, alias_model, actual_channel,
             actual_model, mapping_source, status_code, first_byte_ms, latency_ms,
             input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
             retry_count, error_message FROM audit_log ORDER BY id DESC LIMIT 10"
        ).and_then(|mut stmt| {
            let rows = stmt.query_map([], |row| row.get::<_, String>(0))?;
            let mut plan = String::new();
            for r in rows {
                if let Ok(s) = r { plan.push_str(&s); plan.push('\n'); }
            }
            Ok(plan)
        }).unwrap();
        assert!(!plan.contains("audit_log_bodies"),
            "query_list should not reference audit_log_bodies. Plan: {}", plan);
    }

    #[test]
    fn test_cleanup_removes_bodies() {
        let db = test_db();
        db.migrate_split_bodies().unwrap();

        // 插入记录
        let _id = db.create("POST", "/v1/test", "gpt-4",
            Some("h"), Some("b"), None).unwrap();

        // 手动把 timestamp 改为很久以前，确保 cleanup 能匹配
        let conn = db.conn.lock();
        conn.execute(
            "UPDATE audit_log SET timestamp = 1000000",
            [],
        ).unwrap();
        drop(conn);

        // 验证副表有数据
        let conn = db.conn.lock();
        let count: i64 = conn.query_row(
            "SELECT COUNT(*) FROM audit_log_bodies", [], |row| row.get(0),
        ).unwrap();
        assert_eq!(count, 1);
        drop(conn);

        // 清理（30天，记录 timestamp 远早于此）
        let deleted = db.cleanup(30).unwrap();
        assert_eq!(deleted, 1);

        // 验证副表也被清理
        let conn = db.conn.lock();
        let count: i64 = conn.query_row(
            "SELECT COUNT(*) FROM audit_log_bodies", [], |row| row.get(0),
        ).unwrap();
        assert_eq!(count, 0);
    }

    #[test]
    fn test_prune_bodies_keeps_latest_only() {
        let db = test_db();
        db.migrate_split_bodies().unwrap();

        // 插入 5 条记录
        let mut ids = Vec::new();
        for i in 0..5 {
            let id = db.create("POST", &format!("/v1/{}", i), "gpt-4",
                Some("h"), Some(&format!("body-{}", i)), None).unwrap();
            ids.push(id);
        }

        // 只保留最近 2 条详情
        let pruned = db.prune_bodies(2).unwrap();
        assert_eq!(pruned, 3, "should prune the 3 oldest detail rows");

        // 主表（列表/统计）5 条记录应完整保留，不受裁剪影响
        let list = db.query_list(None, None, None, None, None, None, None, 10).unwrap();
        assert_eq!(list.len(), 5, "list data in audit_log must be unaffected by body pruning");

        // 最旧的 3 条详情应为空（LEFT JOIN 命不中副表）
        for &old_id in &ids[0..3] {
            let entry = db.get_by_id(old_id).unwrap().unwrap();
            assert_eq!(entry.request_body, None, "pruned row should have empty detail body");
        }
        // 最新的 2 条详情应仍然完整
        for &recent_id in &ids[3..5] {
            let entry = db.get_by_id(recent_id).unwrap().unwrap();
            assert!(entry.request_body.is_some(), "recent row should keep its detail body");
        }

        // 再次裁剪应为幂等（无新增可裁剪的行）
        let pruned_again = db.prune_bodies(2).unwrap();
        assert_eq!(pruned_again, 0);
    }

    #[test]
    fn test_force_cleanup_keeps_latest_details_and_shrinks() {
        let db = test_db();
        db.migrate_split_bodies().unwrap();

        // 插入 5 条记录
        let mut ids = Vec::new();
        for i in 0..5 {
            let id = db.create("POST", &format!("/v1/{}", i), "gpt-4",
                Some("h"), Some(&format!("body-{}", i)), None).unwrap();
            ids.push(id);
        }

        // 强制清理：保留最近 2 条详情
        let result = db.force_cleanup(30, 2).unwrap();
        assert_eq!(result.pruned, 3, "should prune the 3 oldest detail rows");
        assert_eq!(result.deleted_old, 0, "no records are older than 30 days");

        // 主表记录全部保留（列表/看板不受影响）
        let list = db.query_list(None, None, None, None, None, None, None, 10).unwrap();
        assert_eq!(list.len(), 5);

        // 最旧 3 条详情为空，最新 2 条完整
        for &old_id in &ids[0..3] {
            let entry = db.get_by_id(old_id).unwrap().unwrap();
            assert_eq!(entry.request_body, None);
        }
        for &recent_id in &ids[3..5] {
            let entry = db.get_by_id(recent_id).unwrap().unwrap();
            assert!(entry.request_body.is_some());
        }

        // status 应反映清理后的状态
        let status = db.status().unwrap();
        assert_eq!(status.total_records, 5);
        assert_eq!(status.detail_records, 2);
        assert!(status.size_bytes > 0);
    }
}
