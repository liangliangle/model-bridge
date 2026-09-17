// Command model-bridge 是 model-bridge 后端的 Go 实现入口。
//
// 启动流程（对应 Rust `src-tauri/src/main.rs`）：
//  1. 准备配置目录 ~/.model-bridge/，无配置文件时写入默认模板
//  2. 加载配置（支持 --port/-p 与 --debug 覆盖）
//  3. 初始化 SQLite 审计库，做启动清理
//  4. 初始化渠道健康表并启动后台健康检查
//  5. 监听 HTTP，处理 SIGINT/SIGTERM 优雅关闭
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"modelbridge/internal/app"
	"modelbridge/internal/audit"
	"modelbridge/internal/channel"
	"modelbridge/internal/config"
	"modelbridge/internal/proxy"
)

func main() {
	log.SetFlags(log.LstdFlags)

	configPath := config.ConfigPath()
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		log.Fatalf("Failed to create config directory %s: %v", configDir, err)
	}

	cfg, err := loadOrCreateConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	applyCLIOverrides(cfg)

	db, err := audit.Open(config.AuditDBPath())
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer func() { _ = db.Close() }()

	state := app.New(cfg, configPath, db)

	// 启动清理：过期审计记录 + 只保留最近 N 条详情大字段。
	if cfg.AuditRetentionDays != nil && *cfg.AuditRetentionDays > 0 {
		if res, err := db.ForceCleanup(*cfg.AuditRetentionDays, audit.BODIESRetainLatest); err != nil {
			log.Printf("Startup cleanup failed: %v", err)
		} else if res.DeletedOld > 0 || res.Pruned > 0 {
			log.Printf("Startup cleanup: removed %d old records, pruned %d detail bodies",
				res.DeletedOld, res.Pruned)
		}
	} else if pruned, err := db.PruneBodies(audit.BODIESRetainLatest); err != nil {
		log.Printf("Startup body prune failed: %v", err)
	} else if pruned > 0 {
		log.Printf("Startup cleanup: pruned %d old audit detail bodies", pruned)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 后台健康检查：周期性把达到恢复间隔的 unhealthy 渠道转为 recovering。
	go channel.RunHealthChecker(ctx, state.Health, cfg.Failover.CircuitBreaker)
	// 后台每日清理。
	go dailyCleanup(ctx, state)

	handler, mcpState := proxy.NewServer(state)
	// 后台刷新即将过期的 MCP OAuth token（请求时的惰性刷新仍是兜底）。
	stopTokenRefresher := mcpState.StartTokenRefresher(ctx)
	defer stopTokenRefresher()

	addr := cfg.ListenHost + ":" + strconv.Itoa(int(cfg.ListenPort))
	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// 不设置 WriteTimeout：流式响应可以持续很久，超时由请求 context 控制。
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		log.Printf("Starting Model Bridge on http://%s", addr)
		log.Printf("  Proxy API:  http://%s/v1/chat/completions", addr)
		log.Printf("  Admin UI:   http://%s/", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Failed to bind to %s: %v", addr, err)
		}
	}()

	<-ctx.Done()
	log.Printf("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Graceful shutdown failed: %v", err)
	}
	state.Close()
}

// loadOrCreateConfig 加载配置；文件不存在时写入默认模板并返回默认配置。
//
// 注意：与 Rust 行为一致——首次启动写入的是带示例渠道的模板文件，
// 但进程内使用的是 `DefaultConfig()`（渠道为空），需要重启后才会加载文件里的渠道。
func loadOrCreateConfig(path string) (*config.AppConfig, error) {
	if _, err := os.Stat(path); err == nil {
		cfg, err := config.LoadFromFile(path)
		if err != nil {
			log.Printf("Failed to load config from %s: %v, using defaults", path, err)
			return config.DefaultConfig(), nil
		}
		log.Printf("Loaded config from %s", path)
		return cfg, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	log.Printf("Creating default config at %s", path)
	if err := os.WriteFile(path, []byte(config.DefaultConfigYAML()), 0o600); err != nil {
		return nil, err
	}
	return config.DefaultConfig(), nil
}

// applyCLIOverrides 处理 --port/-p 与 --debug。
func applyCLIOverrides(cfg *config.AppConfig) {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			if i+1 < len(args) {
				if p, err := strconv.ParseUint(args[i+1], 10, 16); err == nil {
					cfg.ListenPort = uint16(p)
				}
				i++
			}
		case "--debug":
			cfg.Debug = true
		}
	}
}

// dailyCleanup 每天清理一次过期审计记录与详情大字段。
func dailyCleanup(ctx context.Context, state *app.State) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg := state.Config()
			if cfg.AuditRetentionDays != nil && *cfg.AuditRetentionDays > 0 {
				if res, err := state.Audit.ForceCleanup(*cfg.AuditRetentionDays, audit.BODIESRetainLatest); err != nil {
					log.Printf("Daily cleanup failed: %v", err)
				} else if res.DeletedOld > 0 {
					log.Printf("Daily cleanup: removed %d old audit records", res.DeletedOld)
				}
			}
			if pruned, err := state.Audit.PruneBodies(audit.BODIESRetainLatest); err != nil {
				log.Printf("Daily body prune failed: %v", err)
			} else if pruned > 0 {
				log.Printf("Daily cleanup: pruned %d old audit detail bodies", pruned)
			}
		}
	}
}
