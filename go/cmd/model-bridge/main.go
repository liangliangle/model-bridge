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
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modelbridge/internal/api"
	"modelbridge/internal/app"
	"modelbridge/internal/audit"
	"modelbridge/internal/channel"
	"modelbridge/internal/config"
	"modelbridge/internal/mcp"
	"modelbridge/internal/proxy"
	"modelbridge/internal/web"
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

	// 暴露面自检：绑定非回环地址却没有鉴权时告警（严格模式直接拒绝启动）。
	// 放在建库之前，让告警在启动日志的最前面就出现。
	if err := checkExposure(cfg); err != nil {
		log.Fatalf("%v", err)
	}

	db, err := audit.Open(config.AuditDBPath())
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer func() { _ = db.Close() }()

	// 大字段拆分迁移（幂等：已迁移则跳过）。老库在迁移前大字段还在主表里，
	// 不迁移则详情/列表查询拿不到内容。
	if migrated, err := db.MigrateSplitBodies(); err != nil {
		log.Printf("audit_db migration failed (old schema preserved): %v", err)
	} else if migrated {
		log.Printf("audit_db migration: body-split completed, DB compacted")
	}

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

	// 装配：代理入口、管理 API、MCP 中继、静态资源。
	//
	// 装配点在这里（而不是某个库包里），各包只导出自己的路由注册函数，
	// proxy 不再 import api。mcpState 只构造一份：OAuth 的 pending 表必须由
	// /api/config/mcp/oauth/start 写入、由 /oauth/callback 读回。
	mcpState := mcp.NewState(state, state.HTTP, state.Audit)
	mux := http.NewServeMux()
	proxy.Register(mux, state)             // /v1/*
	api.Register(mux, state, mcpState)     // /api/*
	mcp.Register(mux, mcpState)            // /mcp/* 与 /oauth/callback
	mux.HandleFunc("/", web.StaticHandler) // 内嵌前端 + SPA fallback
	handler := proxy.Middleware(mux)       // CORS + panic 恢复

	// 后台刷新即将过期的 MCP OAuth token（请求时的惰性刷新仍是兜底）。
	stopTokenRefresher := mcpState.StartTokenRefresher(ctx)
	defer stopTokenRefresher()

	addr := listenAddr(cfg)
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

// listenAddr 拼出监听地址（与 http.Server 使用的形式一致）。
func listenAddr(cfg *config.AppConfig) string {
	return cfg.ListenHost + ":" + strconv.Itoa(int(cfg.ListenPort))
}

// checkExposure 检查监听地址的暴露面：对外可达却缺少鉴权时打印告警。
//
// 为什么需要它：默认配置监听 127.0.0.1，一旦改成 0.0.0.0（例如从 Docker 或别的机器访问），
// 管理 API、代理入口就会对同网段完全开放；而这两种「未配置 token」的状态在运行期完全
// 静默——请求全部成功，看不出任何异常。因此把风险提前说到启动日志里。
//
// 设置 MODEL_BRIDGE_REQUIRE_AUTH=1（任何非空、非 "0" 的值）可把告警升级为拒绝启动，
// 用于公网部署时避免误开。
func checkExposure(cfg *config.AppConfig) error {
	warnings := config.ExposureWarnings(cfg)
	if len(warnings) == 0 {
		return nil
	}
	if v := strings.TrimSpace(os.Getenv("MODEL_BRIDGE_REQUIRE_AUTH")); v != "" && v != "0" {
		return fmt.Errorf("refusing to start: %s is reachable from other hosts but has no auth (MODEL_BRIDGE_REQUIRE_AUTH=%s): %s",
			listenAddr(cfg), v, strings.Join(warnings, "; "))
	}
	log.Printf("WARNING: listening on %s, which is reachable from other hosts:", listenAddr(cfg))
	for _, w := range warnings {
		log.Printf("WARNING:   - %s", w)
	}
	log.Printf("WARNING: 仅本机使用请把 listen_host 改回 127.0.0.1；对外提供服务请配置 auth.admin_token 与 auth.proxy_tokens")
	return nil
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
