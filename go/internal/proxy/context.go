// Package proxy 实现代理请求的上下文、路由、渠道执行与 HTTP 服务。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"modelbridge/internal/converter"
)

// RequestContext 封装一次入口请求的全部信息，贯穿整个处理流水线。
//
// 关键设计（对应 Rust `proxy/context.rs`）：保留原始请求体的字节表示，
// 不做 JSON 往返序列化，避免原样透传时字段被重排或丢精度。
type RequestContext struct {
	// Format 是入口协议（chat / messages / responses）。
	Format converter.ApiFormat
	Path   string
	// ModelAlias 是下游请求的 model 字段；缺失或非字符串时为 "unknown"。
	ModelAlias string
	IsStream   bool
	// RawBody 是未经修改的原始请求体。
	RawBody []byte
	// Parsed 仅在解析成功且顶层为对象时非 nil；数字以 json.Number 保留原样。
	Parsed map[string]any
	// ParseError 非空表示请求体不是合法 JSON。
	ParseError string

	Headers     http.Header
	HeadersJSON string
	UserAgent   string

	// ReqContext 是入站请求的 context，客户端断开时上游请求随之取消。
	// 由 HTTP handler 注入；缺省时回退到 context.Background()。
	ReqContext context.Context
}

// NewRequestContext 从原始请求创建上下文（保留原始字节）。
func NewRequestContext(format converter.ApiFormat, path string, headers http.Header, rawBody []byte) *RequestContext {
	ctx := &RequestContext{
		Format:     format,
		Path:       path,
		RawBody:    rawBody,
		Headers:    headers,
		ModelAlias: "unknown",
	}
	if headers != nil {
		ctx.UserAgent = headers.Get("User-Agent")
		ctx.HeadersJSON = HeadersToJSON(headers)
	} else {
		ctx.HeadersJSON = "{}"
	}

	// 最小解析：解析成功才取 model / stream；顶层非对象时视为空对象（与 Rust 一致）。
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		ctx.ParseError = err.Error()
		ctx.Parsed = map[string]any{}
		return ctx
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		ctx.Parsed = map[string]any{}
		return ctx
	}
	ctx.Parsed = obj
	if m, ok := obj["model"].(string); ok && m != "" {
		ctx.ModelAlias = m
	}
	if s, ok := obj["stream"].(bool); ok {
		ctx.IsStream = s
	}
	return ctx
}

// ProtocolName 返回入口协议名，用于错误提示与日志。
func (c *RequestContext) ProtocolName() string { return c.Format.String() }
