package config_test

import (
	"strings"
	"testing"

	"modelbridge/internal/config"
)

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"":             false, // 空 host = 监听全部网卡
		"0.0.0.0":      false,
		"::":           false,
		"[::]":         false,
		"127.0.0.1":    true,
		"127.0.0.53":   true,
		"::1":          true,
		"[::1]":        true,
		"localhost":    true,
		"LOCALHOST":    true,
		" 127.0.0.1 ":  true,
		"192.168.1.5":  false,
		"10.0.0.7":     false,
		"example.com":  false, // 主机名无法证明只对本机可见
		"::ffff:0:0.1": false,
	}
	for host, want := range cases {
		if got := config.IsLoopbackHost(host); got != want {
			t.Fatalf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestExposureWarnings(t *testing.T) {
	token := "ll.941107"

	base := func() *config.AppConfig {
		cfg := config.DefaultConfig()
		cfg.Auth.AdminToken = &token
		cfg.Auth.ProxyTokens = []string{"sk-test"}
		return cfg
	}

	t.Run("回环地址不告警", func(t *testing.T) {
		cfg := base()
		cfg.ListenHost = "127.0.0.1"
		cfg.Auth.AdminToken = nil
		cfg.Auth.ProxyTokens = nil
		if got := config.ExposureWarnings(cfg); got != nil {
			t.Fatalf("回环地址不应告警: %v", got)
		}
	})

	t.Run("对外可达且两个入口都无鉴权", func(t *testing.T) {
		cfg := base()
		cfg.ListenHost = "0.0.0.0"
		cfg.Auth.AdminToken = nil
		cfg.Auth.ProxyTokens = nil
		got := config.ExposureWarnings(cfg)
		if len(got) != 3 {
			t.Fatalf("告警项 = %d, want 3: %v", len(got), got)
		}
		if !strings.Contains(got[0], "admin_token") || !strings.Contains(got[1], "proxy_tokens") {
			t.Fatalf("告警内容不符合预期: %v", got)
		}
	})

	t.Run("空串 admin_token 视为未配置", func(t *testing.T) {
		cfg := base()
		cfg.ListenHost = "192.168.1.5"
		empty := "   "
		cfg.Auth.AdminToken = &empty
		got := config.ExposureWarnings(cfg)
		if len(got) != 2 {
			t.Fatalf("告警项 = %d, want 2（管理 API + MCP）: %v", len(got), got)
		}
	})

	t.Run("两个入口都已鉴权时只提示 MCP", func(t *testing.T) {
		cfg := base()
		cfg.ListenHost = ""
		got := config.ExposureWarnings(cfg)
		if len(got) != 1 || !strings.Contains(got[0], "/mcp/*") {
			t.Fatalf("告警项 = %v, want 仅 MCP 一条", got)
		}
	})
}
