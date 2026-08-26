//! Model Bridge 应用入口
//!
//! 启动流程：
//! 1. 初始化日志系统
//! 2. 加载或创建配置文件 (~/.model-bridge/config.yaml)
//! 3. 初始化 SQLite 审计数据库
//! 4. 初始化渠道健康检查
//! 5. 启动 HTTP 服务（代理 API + 管理界面）

use model_bridge_lib::audit::db::AuditDb;
use model_bridge_lib::channel::config::{default_config_yaml, AppConfig};
use model_bridge_lib::channel::health::{new_health_map, spawn_health_checker};
use model_bridge_lib::proxy::server::AppState;
use reqwest::Client;
use std::path::PathBuf;
use std::sync::Arc;

/// 获取用户主目录
fn get_home_dir() -> PathBuf {
    std::env::var("HOME")
        .or_else(|_| std::env::var("USERPROFILE"))
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from("."))
}

/// 获取配置目录路径：~/.model-bridge/
fn get_config_dir() -> PathBuf {
    get_home_dir().join(".model-bridge")
}

#[tokio::main]
async fn main() {
    // 初始化日志，支持 RUST_LOG 环境变量覆盖日志级别
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    // 确保配置目录存在
    let config_dir = get_config_dir();
    std::fs::create_dir_all(&config_dir).expect("Failed to create config directory");

    // 加载配置文件，不存在则生成默认配置
    let config_path = config_dir.join("config.yaml");
    let config = if config_path.exists() {
        match AppConfig::load_from_file(&config_path) {
            Ok(cfg) => {
                tracing::info!("Loaded config from {:?}", config_path);
                cfg
            }
            Err(e) => {
                tracing::error!("Failed to load config: {}, using defaults", e);
                AppConfig::default_config()
            }
        }
    } else {
        tracing::info!("Creating default config at {:?}", config_path);
        std::fs::write(&config_path, default_config_yaml())
            .expect("Failed to write default config");
        AppConfig::default_config()
    };

    // 命令行参数覆盖：--port / -p 端口，--debug 开启调试
    let mut config = config;
    {
        let args: Vec<String> = std::env::args().collect();
        let mut i = 1;
        while i < args.len() {
            match args[i].as_str() {
                "--port" | "-p" if i + 1 < args.len() => {
                    if let Ok(p) = args[i + 1].parse::<u16>() {
                        config.listen_port = p;
                    }
                    i += 2;
                }
                "--debug" => {
                    config.debug = true;
                    i += 1;
                }
                _ => { i += 1; }
            }
        }
    }

    // 初始化 SQLite 审计数据库
    let db_path = config_dir.join("audit.db");
    let audit_db = AuditDb::new(db_path.to_str().unwrap())
        .expect("Failed to initialize database");

    // 执行大字段拆分迁移（幂等：已迁移则跳过）
    match audit_db.migrate_split_bodies() {
        Ok(true) => tracing::info!("audit_db migration: body-split completed, DB compacted"),
        Ok(false) => tracing::info!("audit_db migration: already migrated, skipping"),
        Err(e) => tracing::warn!("audit_db migration failed (old schema preserved): {}", e),
    }

    // 为每个渠道初始化健康状态
    let channel_ids: Vec<String> = config.channels.iter().map(|c| c.id.clone()).collect();
    let health_map = new_health_map(&channel_ids);

    // 启动后台健康检查定时任务（周期性将 unhealthy 渠道转为 recovering）
    let health_map_clone = health_map.clone();
    let cb_config = config.failover.circuit_breaker.clone();
    spawn_health_checker(health_map_clone, cb_config);

    // 启动时清理过期的审计日志（audit_retention_days：0/未配置 = 永久留存）
    let retention_days = config.audit_retention_days.unwrap_or(0);
    if retention_days > 0 {
        if let Ok(deleted) = audit_db.cleanup(retention_days) {
            if deleted > 0 {
                tracing::info!("Startup cleanup: removed {} old audit records", deleted);
            }
        }
    }
    // 详情大字段（请求/响应体）只保留最近 1000 条，列表/统计数据不受影响
    if let Ok(pruned) = audit_db.prune_bodies(model_bridge_lib::audit::db::BODIES_RETAIN_LATEST) {
        if pruned > 0 {
            tracing::info!("Startup cleanup: pruned {} old audit detail bodies", pruned);
        }
    }
    // 将上面清理释放的页面归还给操作系统（仅对开启了 auto_vacuum=INCREMENTAL 的库生效，
    // 对历史库是空操作）。限制单次归还量，避免拖慢启动。
    if let Err(e) = audit_db.incremental_vacuum(20_000) {
        tracing::warn!("Startup incremental_vacuum failed: {}", e);
    }

    // 创建全局共享状态
    let app_state = Arc::new(AppState {
        config: Arc::new(parking_lot::RwLock::new(config.clone())),
        health_map,
        audit_db,
        http_client: Client::new(),
        oauth: model_bridge_lib::mcp::oauth::new_store_from_config(),
    });

    // 后台每天定时清理审计日志
    let cleanup_db = app_state.audit_db.clone();
    let cleanup_config = app_state.config.clone();
    tokio::spawn(async move {
        loop {
            tokio::time::sleep(std::time::Duration::from_secs(86400)).await;
            // 读取当前配置的保留天数（0 = 永久留存，跳过主表清理）
            let retention_days = cleanup_config.read().audit_retention_days.unwrap_or(0);
            if retention_days > 0 {
                if let Ok(deleted) = cleanup_db.cleanup(retention_days) {
                    if deleted > 0 {
                        tracing::info!("Daily cleanup: removed {} old audit records", deleted);
                    }
                }
            }
            if let Ok(pruned) = cleanup_db.prune_bodies(model_bridge_lib::audit::db::BODIES_RETAIN_LATEST) {
                if pruned > 0 {
                    tracing::info!("Daily cleanup: pruned {} old audit detail bodies", pruned);
                }
            }
            if let Err(e) = cleanup_db.incremental_vacuum(0) {
                tracing::warn!("Daily incremental_vacuum failed: {}", e);
            }
        }
    });

    // Refresh low-traffic MCP OAuth tokens before they expire. Request-time
    // refresh remains as a fallback for tokens that expire between polls.
    let oauth_state = app_state.clone();
    tokio::spawn(async move {
        let interval = std::time::Duration::from_secs(
            model_bridge_lib::mcp::oauth::TOKEN_REFRESH_INTERVAL_SECS,
        );
        loop {
            tokio::time::sleep(interval).await;
            model_bridge_lib::mcp::oauth::refresh_expiring_tokens(&oauth_state).await;
        }
    });

    let port = config.listen_port;
    tracing::info!("Starting Model Bridge on http://localhost:{}", port);
    tracing::info!("  Proxy API:  http://localhost:{}/v1/chat/completions", port);
    tracing::info!("  Admin UI:   http://localhost:{}/", port);

    // 创建优雅关闭通道
    let (shutdown_tx, shutdown_rx) = tokio::sync::oneshot::channel();

    // 监听关闭信号（Ctrl+C 或 SIGTERM）
    tokio::spawn(async move {
        #[cfg(unix)]
        {
            let mut sigterm = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                .expect("failed to register SIGTERM handler");
            tokio::select! {
                _ = tokio::signal::ctrl_c() => {},
                _ = sigterm.recv() => {},
            }
        }
        #[cfg(not(unix))]
        {
            tokio::signal::ctrl_c().await.ok();
        }
        tracing::info!("Shutting down...");
        let _ = shutdown_tx.send(());
    });

    // 启动 HTTP 服务（阻塞直到关闭）
    if let Err(e) = model_bridge_lib::proxy::server::start_server(app_state, shutdown_rx).await {
        tracing::error!("Server error: {}", e);
        std::process::exit(1);
    }
}
