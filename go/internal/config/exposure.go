package config

import (
	"net"
	"strings"
)

// 暴露面自检：绑定非回环地址（0.0.0.0 / 具体外网 IP）时，服务对同网段或公网可达。
// 此时如果入口没有配置鉴权 token，任何人都能用你的渠道密钥与上游额度发请求。
// 这是配置事实的直接推论，因此放在 config 包（纯函数、可单测），由启动流程决定
// 是「告警」还是「拒绝启动」。

// IsLoopbackHost 判断监听地址是否只对本机可见。
//
// 空串、"0.0.0.0"、"::"（以及其它未指定的通配地址）都表示监听全部网卡，因此不算回环；
// "localhost" 与 127.0.0.0/8、::1 等回环地址算。
func IsLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		// net.JoinHostPort 语义：空 host 表示监听所有网卡。
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	// 去掉 IPv6 字面量的方括号（[::1]）。
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	ip := net.ParseIP(h)
	if ip == nil {
		// 不是 IP 字面量（例如主机名）：无法证明只对本机可见，按对外可达处理。
		return false
	}
	return ip.IsLoopback()
}

// ExposureWarnings 返回「当前监听地址下缺少鉴权」的告警项；只对本机可见时返回 nil。
//
// 每一项都是一句可直接打印的中文说明（启动日志用）。
func ExposureWarnings(cfg *AppConfig) []string {
	if cfg == nil || IsLoopbackHost(cfg.ListenHost) {
		return nil
	}

	var out []string
	if cfg.Auth.AdminToken == nil || strings.TrimSpace(*cfg.Auth.AdminToken) == "" {
		out = append(out, "管理 API (/api/*) 未配置 auth.admin_token：任何人都能读写配置与审计数据")
	}
	if len(cfg.Auth.ProxyTokens) == 0 {
		out = append(out, "代理入口 (/v1/*) 未配置 auth.proxy_tokens：任何人都能消耗你的上游额度")
	}
	// /mcp/* 与 /oauth/callback 在 Rust 与 Go 两侧都不做 token 校验（见 mcp/register.go），
	// 配了 token 也不会保护它，因此这里始终提示。
	out = append(out, "MCP 中继 (/mcp/*) 不校验 token（与既有行为一致）：会以上游凭证转发到已配置的 MCP server")
	return out
}
