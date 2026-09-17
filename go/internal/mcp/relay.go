// MCP 中继转发核心（对应 src-tauri/src/mcp/relay.rs）：
// 把客户端请求转发到上游 MCP server，按响应类型分流（JSON 缓冲 / SSE 流式透传），
// 并在 tools/list 时过滤黑名单工具。
//
// 与 Rust 的差别（不影响对外语义）：
//   - axum 的 `Response` 由本包的 [Response] + [Response.WriteTo] 承担：缓冲响应直接写出，
//     SSE 响应边读边写并在结束时补写审计。Rust 里 SSE 的审计写在一个 spawn 出来的任务里，
//     这里改为在 WriteTo 的同一 goroutine 内完成（无后台任务、无 goroutine 泄漏）。
//   - SSE 空闲超时由 [SSEIdleTimeout] 承担（Rust 是 const，这里留成 var 以便测试缩短）。
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"modelbridge/internal/audit"
	"modelbridge/internal/config"
)

// SSEIdleTimeout SSE 流空闲超时（连续无数据则断开）（relay.rs:21）。
var SSEIdleTimeout = 300 * time.Second

// streamStopWait 取消上游请求后等待读 goroutine 退出的上限（正常情况下立即返回）。
const streamStopWait = 5 * time.Second

// ConfigStore 提供对运行期配置的读写与落盘能力。
//
// Rust 侧直接持有 `Arc<RwLock<AppConfig>>` 并调用 commands::save_config_to_disk；
// Go 侧用接口隔离，便于 api 层注入自己的实现与测试替身。
type ConfigStore interface {
	// Snapshot 返回当前配置的只读快照（返回的指针不得被修改）。
	Snapshot() *config.AppConfig
	// Update 在写锁内修改配置并落盘（fn 返回错误时不落盘）。
	Update(fn func(cfg *config.AppConfig) error) error
}

// AuditSink 中继需要的审计写入能力，由 audit.DB 实现。
type AuditSink interface {
	Create(method, path, aliasModel string, headers, body, userAgent *string) (int64, error)
	UpdateError(id int64, statusCode int, latencyMs uint64, message string) error
	UpdateResponse(id int64, statusCode int, latencyMs uint64, upstreamHeaders, upstreamBody, responseHeaders, responseBody *string, tokens audit.TokenUsage, cost *float64) error
	UpdateStreamingResponse(id int64, upstreamBody string, forwardedBody *string, statusCode int, latencyMs uint64, cost *float64) error
	SetUpstreamResponseHeaders(id int64, headers *string) error
	SetResponseHeaders(id int64, headers *string) error
}

// State 中继所需的运行期依赖（对应 Rust 的 AppState 子集）。
type State struct {
	Config ConfigStore
	Client *http.Client
	Audit  AuditSink
	OAuth  *OAuthStore

	// oauthMu 保护 OAuth 的惰性初始化（见 oauthStore）。
	oauthMu sync.Mutex
}

// snapshot 返回配置快照；Config 未注入时返回 nil（调用方需判空）。
func (s *State) snapshot() *config.AppConfig {
	if s == nil || s.Config == nil {
		return nil
	}
	return s.Config.Snapshot()
}

// HTTPClient 返回可用的 HTTP 客户端。
func (s *State) HTTPClient() *http.Client {
	if s == nil || s.Client == nil {
		return http.DefaultClient
	}
	return s.Client
}

// Response 中继返回的响应：缓冲型（Body）或流式型（Stream）。
//
// 调用方拿到后直接 `resp.WriteTo(ctx, w)` 即可，无需关心审计补写与流关闭。
type Response struct {
	StatusCode int
	Header     http.Header
	// Body 缓冲型响应体（Stream 为 nil 时使用）。
	Body []byte

	// stream 非 nil 时表示 SSE 透传流。
	stream io.ReadCloser
	// streamCancel 取消上游请求（空闲超时/客户端断开时调用，可解除阻塞中的 Read）。
	streamCancel context.CancelFunc
	// onStreamDone 流结束时写审计（在 WriteTo 的调用 goroutine 内执行）。
	onStreamDone func(rawStream string)
}

// WriteTo 把响应写到 http.ResponseWriter。
//
// 缓冲型：写入 Body；流式型：边读边写并 flush，空闲超过 [SSEIdleTimeout] 或客户端
// 断开（ctx 取消）时结束，随后写审计并关闭上游流（不遗留 goroutine）。
func (r *Response) WriteTo(ctx context.Context, w http.ResponseWriter) error {
	if ctx == nil {
		ctx = context.Background()
	}
	dst := w.Header()
	for k, vs := range r.Header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	status := r.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	if r.stream == nil {
		if len(r.Body) == 0 {
			return nil
		}
		_, err := w.Write(r.Body)
		return err
	}
	return r.copyStream(ctx, w)
}

// copyStream 字节级透传上游 SSE 流，并累积全部内容供审计（relay.rs:200-270）。
func (r *Response) copyStream(ctx context.Context, w http.ResponseWriter) error {
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	var collected bytes.Buffer
	stop := make(chan struct{})
	chunks := make(chan []byte, 1)
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := r.stream.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				select {
				case chunks <- cp:
				case <-stop:
					return
				}
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()

	timer := time.NewTimer(SSEIdleTimeout)
	defer timer.Stop()

loop:
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				break loop // 上游流正常结束或报错
			}
			collected.Write(chunk)
			if _, err := w.Write(chunk); err != nil {
				break loop
			}
			if flusher != nil {
				flusher.Flush()
			}
			resetTimer(timer, SSEIdleTimeout)
		case <-timer.C:
			// 空闲超时（relay.rs:244-249 的 tracing::warn 分支）
			break loop
		case <-ctx.Done():
			// 客户端断开
			break loop
		}
	}

	// 先取消上游请求（解除阻塞中的 Read），再让读 goroutine 退出。
	if r.streamCancel != nil {
		r.streamCancel()
	}
	close(stop)
	select {
	case <-readerDone:
	case <-time.After(streamStopWait):
	}
	_ = r.stream.Close()
	if r.onStreamDone != nil {
		r.onStreamDone(collected.String())
	}
	return nil
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// ForwardPost 转发 POST 请求到上游 MCP server（relay.rs:26-91）。
//   - method/reqID 已从 body 解析好（用于审计和 tools/list 过滤判断）
//   - method == "tools/list" 时走过滤路径
func ForwardPost(
	ctx context.Context,
	st *State,
	server *config.McpServerConfig,
	clientHeaders http.Header,
	body string,
	method string,
	reqID json.RawMessage,
	auditID int64,
) Response {
	start := time.Now()

	// OAuth：转发前确保有有效 token（过期则自动刷新）
	token, err := st.EnsureValidToken(ctx, server)
	if err != nil {
		st.auditUpdateError(auditID, 401, 0, err.Error())
		return mcpErrorResponse(reqID, -32001, "MCP OAuth: "+err.Error())
	}

	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, server.Endpoint, strings.NewReader(body))
	if err != nil {
		cancel()
		st.auditUpdateError(auditID, 502, 0, "network_error: "+err.Error())
		return mcpErrorResponse(reqID, -32000, "network_error: "+err.Error())
	}
	req.Header = BuildForwardHeaders(clientHeaders, server, token)
	req.Header.Set("Content-Type", "application/json")
	applyDefaultAccept(req)

	resp, err := st.HTTPClient().Do(req)
	if err != nil {
		cancel()
		msg := networkErrorMessage(err)
		st.auditUpdateError(auditID, 502, elapsedMs(start), msg)
		return mcpErrorResponse(reqID, -32000, msg)
	}

	status := resp.StatusCode
	upstreamHeaders := resp.Header.Clone()
	if status == 401 && server.OAuthEnabled {
		_ = st.ClearOAuthRegistration(server.ID)
		st.auditUpdateError(auditID, 401, elapsedMs(start), "MCP OAuth client rejected; registration cleared")
	}

	isSSE := strings.Contains(upstreamHeaders.Get("Content-Type"), "text/event-stream")

	// tools/list：缓冲 → 过滤 → 回 application/json
	if method == "tools/list" {
		defer cancel()
		defer resp.Body.Close()
		return filterToolsList(st, server, resp, status, upstreamHeaders, reqID, auditID, start)
	}

	if isSSE {
		return passthroughSSE(st, resp, cancel, status, upstreamHeaders, auditID, start)
	}
	defer cancel()
	defer resp.Body.Close()
	return passthroughJSON(st, resp, status, upstreamHeaders, auditID, start)
}

// filterToolsList tools/list 过滤：缓冲完整响应，提取 JSON-RPC response，
// 删除黑名单工具，回 JSON（relay.rs:93-155）。
func filterToolsList(
	st *State,
	server *config.McpServerConfig,
	resp *http.Response,
	status int,
	upstreamHeaders http.Header,
	reqID json.RawMessage,
	auditID int64,
	start time.Time,
) Response {
	raw, err := readAllString(resp.Body)
	if err != nil {
		msg := "read upstream failed: " + err.Error()
		st.auditUpdateError(auditID, 502, elapsedMs(start), msg)
		return mcpErrorResponse(reqID, -32000, msg)
	}

	// 提取 JSON-RPC response 并过滤；提取失败则原样透传上游文本
	body := raw
	contentType := "application/json"
	if rpc, ok := ExtractJSONRPCResponse(raw, reqID); ok {
		FilterToolsInResponse(rpc, server.BlockedTools)
		if encoded, err := json.Marshal(rpc); err == nil {
			body = string(encoded)
		}
	} else {
		// 解析不出来：保守透传，Content-Type 跟随上游（可能是 SSE）
		ct := upstreamHeaders.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		if strings.Contains(ct, "event-stream") {
			contentType = "text/event-stream"
		}
	}

	latency := elapsedMs(start)
	outHeaders := headersJSON(httpHeader("Content-Type", "application/json"))
	st.auditSetUpstreamResponseHeaders(auditID, headersJSON(upstreamHeaders))
	st.auditSetResponseHeaders(auditID, outHeaders)
	st.auditUpdateResponse(auditID, status, latency, latency, &raw, &body)

	out := Response{
		StatusCode: status,
		Header:     httpHeader("Content-Type", contentType),
		Body:       []byte(body),
	}
	CopyMCPResponseHeaders(out.Header, upstreamHeaders)
	return out
}

// passthroughJSON 非流式响应：缓冲全部字节，原 Content-Type 回传（relay.rs:157-198）。
func passthroughJSON(
	st *State,
	resp *http.Response,
	status int,
	upstreamHeaders http.Header,
	auditID int64,
	start time.Time,
) Response {
	contentType := upstreamHeaders.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}

	text, err := readAllString(resp.Body)
	if err != nil {
		msg := "read upstream failed: " + err.Error()
		st.auditUpdateError(auditID, 502, elapsedMs(start), msg)
		return Response{
			StatusCode: http.StatusBadGateway,
			Header:     httpHeader("Content-Type", "text/plain; charset=utf-8"),
			Body:       []byte(msg),
		}
	}

	latency := elapsedMs(start)
	st.auditSetUpstreamResponseHeaders(auditID, headersJSON(upstreamHeaders))
	st.auditUpdateResponse(auditID, status, latency, latency, &text, &text)

	out := Response{
		StatusCode: status,
		Header:     httpHeader("Content-Type", contentType),
		Body:       []byte(text),
	}
	CopyMCPResponseHeaders(out.Header, upstreamHeaders)
	return out
}

// passthroughSSE 流式响应：字节级透传上游 SSE 流给客户端，累积内容供审计（relay.rs:200-270）。
func passthroughSSE(
	st *State,
	resp *http.Response,
	cancel context.CancelFunc,
	status int,
	upstreamHeaders http.Header,
	auditID int64,
	start time.Time,
) Response {
	// Rust: update_first_byte(audit_id, first_byte, upstream_headers, upstream_headers)
	// —— 首字节与响应头都用上游头，这里沿用（audit 侧没有单独的 first_byte setter，
	// 延迟由流结束时的 update_streaming_response 落库）。
	headers := headersJSON(upstreamHeaders)
	st.auditSetUpstreamResponseHeaders(auditID, headers)
	st.auditSetResponseHeaders(auditID, headers)

	out := Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
			"Connection":    []string{"keep-alive"},
		},
		stream:       resp.Body,
		streamCancel: cancel,
		onStreamDone: func(rawStream string) {
			st.auditUpdateStreamingResponse(auditID, rawStream, status, elapsedMs(start))
		},
	}
	CopyMCPResponseHeaders(out.Header, upstreamHeaders)
	return out
}

// ForwardGet 转发 GET 请求（客户端打开 SSE 长连接接收服务端推送）（relay.rs:272-320）。
func ForwardGet(
	ctx context.Context,
	st *State,
	server *config.McpServerConfig,
	clientHeaders http.Header,
	auditID int64,
) Response {
	start := time.Now()
	token, err := st.EnsureValidToken(ctx, server)
	if err != nil {
		st.auditUpdateError(auditID, 401, 0, err.Error())
		return Response{
			StatusCode: http.StatusUnauthorized,
			Header:     httpHeader("Content-Type", "text/plain; charset=utf-8"),
			Body:       []byte("MCP OAuth: " + err.Error()),
		}
	}

	reqCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, server.Endpoint, nil)
	if err != nil {
		cancel()
		st.auditUpdateError(auditID, 502, 0, "network_error: "+err.Error())
		return Response{StatusCode: http.StatusBadGateway, Body: []byte(err.Error()), Header: http.Header{}}
	}
	req.Header = BuildForwardHeaders(clientHeaders, server, token)
	applyDefaultAccept(req)

	resp, err := st.HTTPClient().Do(req)
	if err != nil {
		cancel()
		msg := "network_error: " + err.Error()
		st.auditUpdateError(auditID, 502, elapsedMs(start), msg)
		return Response{StatusCode: http.StatusBadGateway, Body: []byte(msg), Header: http.Header{}}
	}

	status := resp.StatusCode
	upstreamHeaders := resp.Header.Clone()
	if status == 401 && server.OAuthEnabled {
		_ = st.ClearOAuthRegistration(server.ID)
		st.auditUpdateError(auditID, 401, elapsedMs(start), "MCP OAuth client rejected; registration cleared")
	}

	// 405 表示上游不提供 GET SSE 通道，原样回传
	if status == http.StatusMethodNotAllowed {
		latency := elapsedMs(start)
		st.auditSetUpstreamResponseHeaders(auditID, headersJSON(upstreamHeaders))
		st.auditUpdateResponse(auditID, status, latency, latency, nil, nil)
		_ = resp.Body.Close()
		cancel()
		return Response{StatusCode: http.StatusMethodNotAllowed, Header: http.Header{}}
	}

	return passthroughSSE(st, resp, cancel, status, upstreamHeaders, auditID, start)
}

// ForwardDelete 转发 DELETE 请求（终止会话），缓冲回传上游状态码与 body（relay.rs:322-370）。
func ForwardDelete(
	ctx context.Context,
	st *State,
	server *config.McpServerConfig,
	clientHeaders http.Header,
	auditID int64,
) Response {
	start := time.Now()
	token, err := st.EnsureValidToken(ctx, server)
	if err != nil {
		st.auditUpdateError(auditID, 401, 0, err.Error())
		return Response{
			StatusCode: http.StatusUnauthorized,
			Header:     httpHeader("Content-Type", "text/plain; charset=utf-8"),
			Body:       []byte("MCP OAuth: " + err.Error()),
		}
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodDelete, server.Endpoint, nil)
	if err != nil {
		st.auditUpdateError(auditID, 502, 0, "network_error: "+err.Error())
		return Response{StatusCode: http.StatusBadGateway, Body: []byte(err.Error()), Header: http.Header{}}
	}
	req.Header = BuildForwardHeaders(clientHeaders, server, token)
	applyDefaultAccept(req)

	resp, err := st.HTTPClient().Do(req)
	if err != nil {
		msg := "network_error: " + err.Error()
		st.auditUpdateError(auditID, 502, elapsedMs(start), msg)
		return Response{StatusCode: http.StatusBadGateway, Body: []byte(msg), Header: http.Header{}}
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	upstreamHeaders := resp.Header.Clone()
	if status == 401 && server.OAuthEnabled {
		_ = st.ClearOAuthRegistration(server.ID)
		st.auditUpdateError(auditID, 401, elapsedMs(start), "MCP OAuth client rejected; registration cleared")
	}
	text, _ := readAllString(resp.Body)

	latency := elapsedMs(start)
	st.auditSetUpstreamResponseHeaders(auditID, headersJSON(upstreamHeaders))
	st.auditUpdateResponse(auditID, status, latency, latency, &text, &text)

	out := Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       []byte(text),
	}
	CopyMCPResponseHeaders(out.Header, upstreamHeaders)
	return out
}

// mcpErrorResponse 构造一个 JSON-RPC error 形式的 HTTP 响应（网络层错误等）（relay.rs:372-380）。
func mcpErrorResponse(reqID json.RawMessage, code int, message string) Response {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": jsonrpcVersion,
		"id":      decodeRawValue(reqID),
		"error":   map[string]any{"code": code, "message": message},
	})
	if err != nil {
		body = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"internal error"}}`)
	}
	return Response{
		StatusCode: http.StatusBadGateway,
		Header:     httpHeader("Content-Type", "application/json"),
		Body:       body,
	}
}

// headersJSON 序列化响应头为 JSON 字符串（审计用）（relay.rs:383-389）。
func headersJSON(headers http.Header) string {
	if headers == nil {
		return "{}"
	}
	out, err := json.Marshal(headers)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// applyDefaultAccept 复刻 reqwest 默认的 `accept: */*`（仅在客户端未提供时补上）。
func applyDefaultAccept(req *http.Request) {
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
}

// networkErrorMessage 与 relay.rs:56-61 一致：超时归类为 "timeout"。
func networkErrorMessage(err error) string {
	if os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "network_error: " + err.Error()
}

func elapsedMs(start time.Time) uint64 {
	d := time.Since(start).Milliseconds()
	if d < 0 {
		return 0
	}
	return uint64(d)
}

func readAllString(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ===== 审计写入的小封装：忽略错误（与 Rust `let _ = ...` 一致） =====

func (s *State) auditUpdateError(id int64, status int, latencyMs uint64, message string) {
	if s == nil || s.Audit == nil || id <= 0 {
		return
	}
	_ = s.Audit.UpdateError(id, status, latencyMs, message)
}

// auditUpdateResponse 写非流式审计结果。
//
// 说明：Rust 侧的 update_response 自带 headers 参数，本包改用
// SetUpstreamResponseHeaders / SetResponseHeaders 两个专用写入器（调用点都已各自调用），
// 效果等价；MCP 中继不是 LLM 调用，因此 token 恒为零值、成本恒为 nil。
// AuditDb::update_response 的 first_byte_ms 在 Rust 中就等于 latency_ms，故此处不再单独传。
func (s *State) auditUpdateResponse(id int64, status int, firstByteMs, latencyMs uint64, upstreamBody, responseBody *string) {
	if s == nil || s.Audit == nil || id <= 0 {
		return
	}
	_ = s.Audit.UpdateResponse(id, status, latencyMs, nil, upstreamBody, nil, responseBody, audit.TokenUsage{}, nil)
}

func (s *State) auditUpdateStreamingResponse(id int64, upstreamBody string, status int, latencyMs uint64) {
	if s == nil || s.Audit == nil || id <= 0 {
		return
	}
	_ = s.Audit.UpdateStreamingResponse(id, upstreamBody, nil, status, latencyMs, nil)
}

func (s *State) auditSetUpstreamResponseHeaders(id int64, headers string) {
	if s == nil || s.Audit == nil || id <= 0 {
		return
	}
	_ = s.Audit.SetUpstreamResponseHeaders(id, &headers)
}

func (s *State) auditSetResponseHeaders(id int64, headers string) {
	if s == nil || s.Audit == nil || id <= 0 {
		return
	}
	_ = s.Audit.SetResponseHeaders(id, &headers)
}
