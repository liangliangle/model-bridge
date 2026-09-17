// MCP 中继的请求头处理（对应 src-tauri/src/mcp/headers.rs）：
// 双向透传 Mcp-Session-Id / MCP-Protocol-Version 等，并按渠道配置注入或透传 Authorization。
package mcp

import (
	"net/http"
	"strings"

	"modelbridge/internal/config"
)

// skipHeaders 不应透传给上游的 hop-by-hop / 自动管理的请求头（headers.rs:10-17）。
var skipHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
	"accept-encoding":   true, // 避免上游返回 gzip，缓冲 tools/list 时还要解压
	"authorization":     true, // 单独处理（注入或透传）
}

// copyBackHeaders 需要从上游响应回传给客户端的 MCP 相关响应头（headers.rs:70）。
var copyBackHeaders = []string{"Mcp-Session-Id", "MCP-Protocol-Version"}

// BuildForwardHeaders 构建转发到上游 MCP server 的请求头（headers.rs:19-67）：
//  1. 透传客户端原始 headers（跳过 hop-by-hop 和 authorization）
//     —— Mcp-Session-Id / MCP-Protocol-Version / Accept 都会被原样带上
//  2. Authorization 优先级：OAuth token > 静态 auth_token > 透传客户端
//     （OAuth 启用时绝不回退透传客户端 token：MCP spec 禁止 token passthrough）
//  3. 叠加渠道自定义 headers（最后，可覆盖前面的）
func BuildForwardHeaders(clientHeaders http.Header, server *config.McpServerConfig, oauthToken *string) http.Header {
	out := http.Header{}

	// 步骤 1：透传客户端 headers
	for key, values := range clientHeaders {
		if skipHeaders[strings.ToLower(key)] {
			continue
		}
		for _, v := range values {
			out.Add(key, v)
		}
	}

	// 步骤 2：Authorization
	switch {
	case oauthToken != nil && *oauthToken != "":
		out.Set("Authorization", "Bearer "+*oauthToken)
	case server != nil && server.AuthToken != nil && *server.AuthToken != "":
		out.Set("Authorization", "Bearer "+*server.AuthToken)
	default:
		// 透传客户端原始 Authorization（若有）
		if auth := clientHeaders.Get("Authorization"); auth != "" {
			out.Set("Authorization", auth)
		}
	}

	// 步骤 3：叠加自定义 headers
	if server != nil {
		for k, v := range server.CustomHeaders {
			out.Set(k, v)
		}
	}
	return out
}

// CopyMCPResponseHeaders 将上游响应中的 MCP 会话相关头复制到返回给客户端的响应头。
// 尤其是 initialize 阶段下发的 Mcp-Session-Id，必须回传，否则客户端后续请求会失败
// （headers.rs:74-86）。
func CopyMCPResponseHeaders(dst http.Header, upstream http.Header) {
	if dst == nil || upstream == nil {
		return
	}
	for _, name := range copyBackHeaders {
		if val := upstream.Get(name); val != "" {
			dst.Set(name, val)
		}
	}
}

// httpHeader 构造单值 header（内部小工具）。
func httpHeader(key, value string) http.Header {
	h := http.Header{}
	h.Set(key, value)
	return h
}
