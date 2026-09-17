package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"modelbridge/internal/app"
	"modelbridge/internal/audit"
	"modelbridge/internal/config"
	"modelbridge/internal/converter"
	"modelbridge/internal/cost"
)

// 渠道执行器：负责单个渠道上的请求处理。
// 协议矩阵已在路由层过滤，这里只做「透传或转换 → 改 body → 发请求 → 回写」。
//
// 对应 Rust `proxy/executor.rs::execute_on_channel`。

// Route 是一次渠道尝试所需的全部信息。
type Route struct {
	Channel       *config.ChannelConfig
	ActualModel   string
	MappingSource config.MappingSource
}

// 流式相关超时，与 Rust 侧一致。
const (
	firstChunkTimeout = 30 * time.Second
	streamIdleTimeout = 300 * time.Second
	streamTotal       = 30 * time.Minute
)

// FormatFromProvider 把渠道供应商类型映射为上游协议格式。
func FormatFromProvider(p config.ProviderType) converter.ApiFormat {
	switch p {
	case config.ProviderAnthropic:
		return converter.FormatAnthropic
	case config.ProviderOpenAIResponses:
		return converter.FormatResponses
	default:
		return converter.FormatOpenAIChat
	}
}

// ExecuteOnChannel 在指定渠道上执行请求。
//
// 返回 nil 表示响应（含流式）已经写给了客户端，路由层必须就此返回，
// 不得再尝试其它渠道；返回非 nil 表示尚未写入任何字节，可以安全换渠道重试。
func ExecuteOnChannel(state *app.State, ctx *RequestContext, rt Route, auditID int64, w http.ResponseWriter) error {
	start := time.Now()
	ch := rt.Channel
	debug := state.Config().Debug
	to := FormatFromProvider(ch.Provider)
	convert := ctx.Format != to

	// 兜底校验：路由层已按矩阵过滤，能走到这里说明矩阵被绕过。
	if convert && !ctx.Format.CanRouteTo(to) {
		return fmt.Errorf("invalid_request: cannot convert %s request to a %s channel", ctx.Format, to)
	}

	session := converter.NewSession()
	sendBody, err := buildUpstreamBody(ctx, rt, session, to, convert)
	if err != nil {
		return err
	}

	if debug {
		log.Printf("[DEBUG] audit_id=%d channel=%s %s → %s converted=%v bytes=%d",
			auditID, ch.ID, ctx.Format, to, convert, len(sendBody))
	}

	// 渠道级请求体改造（顺序与 Rust executor 一致）。
	if ch.StripThinking {
		sendBody = StripThinkingBlocks(sendBody)
	}
	if ch.Provider == config.ProviderAnthropic {
		sendBody = SanitizeAnthropicBody(sendBody)
		if ch.AutoCache {
			sendBody = InjectAnthropicCache(sendBody)
		}
	}
	if ch.ForceEffort != nil {
		sendBody = ForceEffortInJSON(sendBody, *ch.ForceEffort)
	}
	// 转换路径下 convert 会丢掉 stream 字段，按入口的流式意图补回。
	if convert {
		sendBody = SetStreamInJSON(sendBody, ctx.IsStream)
	}

	// 超时：流式用整体上限，非流式用渠道超时。
	timeout := time.Duration(ch.TimeoutMs) * time.Millisecond
	if ctx.IsStream {
		timeout = streamTotal
	}
	parent := ctx.ReqContext
	if parent == nil {
		parent = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(parent, timeout)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ch.Endpoint.URL, bytes.NewReader(sendBody))
	if err != nil {
		cancel()
		return fmt.Errorf("build request failed: %s", err)
	}
	for k, v := range buildUpstreamHeaders(ctx, ch) {
		req.Header.Set(k, v)
	}
	reqHeadersJSON := HeadersToJSON(req.Header)

	_ = state.Audit.SetForwardedRequest(auditID, &reqHeadersJSON, ptrStr(string(sendBody)))

	resp, err := state.HTTP.Do(req)
	if err != nil {
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("timeout")
		}
		return fmt.Errorf("network_error: %s", err)
	}
	defer func() { _ = resp.Body.Close() }()

	status := resp.StatusCode
	respHeadersJSON := HeadersToJSON(resp.Header)
	upstreamCT := strings.ToLower(resp.Header.Get("Content-Type"))

	if debug {
		log.Printf("[DEBUG] audit_id=%d upstream status=%d content-type=%q", auditID, status, upstreamCT)
	}

	if status == http.StatusTooManyRequests {
		cancel()
		return errors.New("rate_limited")
	}

	if ctx.IsStream {
		return streamFromUpstream(streamArgs{
			state: state, ctx: ctx, rt: rt, session: session, convert: convert,
			to: to, auditID: auditID, w: w, start: start, status: status,
			respHeadersJSON: respHeadersJSON, upstreamCT: upstreamCT,
			body: resp.Body, cancel: cancel, debug: debug,
		})
	}

	// 非流式在这里就结束，必须自己释放 context（流式路径由 streamFromUpstream 的 defer 负责）。
	err = nonStreamFromUpstream(state, ctx, rt, session, convert, to, auditID, w, start, status, respHeadersJSON, resp.Body)
	cancel()
	return err
}

// buildUpstreamBody 计算发往上游的请求体：透传只替换 model，转换走协议映射。
func buildUpstreamBody(ctx *RequestContext, rt Route, session *converter.Session, to converter.ApiFormat, convert bool) ([]byte, error) {
	if !convert {
		return ReplaceModelInJSON(ctx.RawBody, rt.ActualModel), nil
	}
	if ctx.Format == converter.FormatResponses {
		if err := converter.ValidateResponsesCrossFormat(ctx.RawBody); err != nil {
			return nil, fmt.Errorf("invalid_request: %s", err)
		}
	}
	// 说明：Rust/model-bridge 没有「模型是否支持图片」的能力表，因此这里不做图像闸门，
	// 与重写前行为一致（ocgo 的图像校验依赖其自带的模型元数据，本仓库无此数据源）。
	out, err := session.BuildUpstreamRequest(ctx.Format, to, ctx.RawBody, converter.RequestOptions{
		Model:          rt.ActualModel,
		Stream:         ctx.IsStream,
		SupportsImages: true,
	})
	if err != nil {
		return nil, fmt.Errorf("invalid_request: %s", err)
	}
	return out, nil
}

// buildUpstreamHeaders 组装发往上游的请求头（透传 + custom_headers + 供应商认证）。
func buildUpstreamHeaders(ctx *RequestContext, ch *config.ChannelConfig) map[string]string {
	headers := map[string]string{"content-type": "application/json"}
	CollectForwardedHeaders(headers, ctx.Headers, ch)
	switch ch.Provider {
	case config.ProviderAnthropic:
		headers["x-api-key"] = ch.APIKey
		headers["anthropic-version"] = "2023-06-01"
	default:
		if ch.APIKey != "" {
			headers["authorization"] = "Bearer " + ch.APIKey
		}
	}
	return headers
}

// nonStreamFromUpstream 处理非流式响应：读全量 → 转回入口协议 → 落审计 → 回写。
func nonStreamFromUpstream(
	state *app.State, ctx *RequestContext, rt Route, session *converter.Session, convert bool,
	to converter.ApiFormat, auditID int64, w http.ResponseWriter, start time.Time,
	status int, respHeadersJSON string, body io.Reader,
) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("read response failed: %s", err)
	}
	rawText := string(raw)

	if status >= 500 {
		return fmt.Errorf("server_error_%d: %s", status, preview(rawText, 200))
	}
	if status >= 400 {
		return fmt.Errorf("HTTP %d — %s", status, upstreamErrorMessage(rawText))
	}

	var tokens audit.TokenUsage
	if obj := decodeObject(raw); obj != nil {
		tokens = audit.ExtractTokensFromJSON(obj)
	}
	cost := computeCost(state, rt.ActualModel, tokens)

	responseBody := rawText
	if convert {
		// 注意方向：转换器以「上游格式 → 下游格式」为参数顺序，
		// 上游永远是 Chat（本仓库矩阵里唯一的转换枢纽），下游才是入口协议。
		if converted, err := session.ConvertNonStreamResponse(to, ctx.Format, raw, rt.ActualModel); err == nil {
			responseBody = string(converted)
		}
	}

	latency := uint64(time.Since(start).Milliseconds())
	_ = state.Audit.SetResponseHeaders(auditID, &respHeadersJSON)
	_ = state.Audit.UpdateResponse(auditID, status, latency,
		&respHeadersJSON, &rawText, &respHeadersJSON, &responseBody, tokens, cost)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(responseBody))
	return nil
}

// streamArgs 是流式处理需要的上下文集合（避免超长参数列表）。
type streamArgs struct {
	state           *app.State
	ctx             *RequestContext
	rt              Route
	session         *converter.Session
	convert         bool
	to              converter.ApiFormat
	auditID         int64
	w               http.ResponseWriter
	start           time.Time
	status          int
	respHeadersJSON string
	upstreamCT      string
	body            io.ReadCloser
	cancel          context.CancelFunc
	debug           bool
}

// streamFromUpstream 处理流式响应：透传字节流，或经转换器产出入口协议的事件流。
func streamFromUpstream(a streamArgs) error {
	defer a.cancel()

	upstreamIsSSE := strings.Contains(a.upstreamCT, "text/event-stream")

	// 降级一：跨格式转换路径下，上游对流式请求返回了非 SSE 的完整 JSON
	// （常见于带工具调用的场景）。按 SSE 解析的转换器无法处理，
	// 这里读全量、转换后重放为入口协议的 SSE 事件流。
	if a.convert && !upstreamIsSSE {
		raw, err := io.ReadAll(a.body)
		if err != nil {
			return fmt.Errorf("read response failed: %s", err)
		}
		if a.status < 400 {
			// 方向同上：上游格式（chat）→ 下游格式（入口协议）。
			if sse, err := a.session.FullResponseToSSE(a.to, a.ctx.Format, raw, a.rt.ActualModel); err == nil && len(sse) > 0 {
				var tokens audit.TokenUsage
				var convertedText string
				if obj := decodeObject(raw); obj != nil {
					tokens = audit.ExtractTokensFromJSON(obj)
				}
				if converted, err := a.session.ConvertNonStreamResponse(a.to, a.ctx.Format, raw, a.rt.ActualModel); err == nil {
					convertedText = strings.TrimSpace(PrettyJSON(string(converted)))
				} else {
					convertedText = string(sse)
				}
				latency := uint64(time.Since(a.start).Milliseconds())
				cost := computeCost(a.state, a.rt.ActualModel, tokens)
				rawText := string(raw)
				_ = a.state.Audit.UpdateFirstByte(a.auditID, latency, &a.respHeadersJSON, &a.respHeadersJSON)
				_ = a.state.Audit.UpdateResponse(a.auditID, a.status, latency,
					&a.respHeadersJSON, &rawText, &a.respHeadersJSON, &convertedText, tokens, cost)
				if a.debug {
					log.Printf("[DEBUG] audit_id=%d non-SSE upstream on stream request replayed as %s SSE (%d bytes)",
						a.auditID, a.ctx.Format, len(sse))
				}
				writeSSEHeaders(a.w, http.StatusOK)
				_, _ = a.w.Write(sse)
				flushWriter(a.w)
				return nil
			}
		}
		// 解析失败或上游报错：把已读取的原始文本作为单块透传。
		if a.debug {
			log.Printf("[DEBUG] audit_id=%d upstream body is not SSE and not convertible; passing through raw", a.auditID)
		}
		writeSSEHeaders(a.w, a.status)
		_, _ = a.w.Write(raw)
		flushWriter(a.w)
		return nil
	}

	// 正常流式：把上游字节流切成 chunk 交给转换器（或直接透传）。
	_ = a.state.Audit.UpdateFirstByte(a.auditID, uint64(time.Since(a.start).Milliseconds()),
		&a.respHeadersJSON, &a.respHeadersJSON)

	chunks := readChunks(a.body)
	first, err := readChunkWithTimeout(chunks, firstChunkTimeout)
	if err != nil {
		return err
	}

	// 首个 chunk 里如果有错误事件且尚无正常内容，触发换渠道重试。
	if len(first) > 0 {
		if msg, isErr := DetectSSEErrorInPrefix(first); isErr && !SSEHasContentOutput(first) {
			_ = a.state.Audit.UpdateError(a.auditID, http.StatusBadGateway,
				uint64(time.Since(a.start).Milliseconds()), "stream_error: "+msg)
			return fmt.Errorf("stream_error: %s", msg)
		}
	}

	src := &firstThenChanReader{pending: first, chunks: chunks, idle: streamIdleTimeout}

	// 审计始终收集上游原始字节；forwarded 收集实际下发给客户端的字节。
	collected := &bytes.Buffer{}
	upstreamReader := io.TeeReader(src, collected)
	forwarded := &bytes.Buffer{}
	sink := io.MultiWriter(a.w, forwarded)

	writeSSEHeaders(a.w, http.StatusOK)
	flushWriter(a.w)

	if a.convert {
		// 方向：上游格式（chat）→ 下游格式（入口协议）。
		_, err = a.session.ConvertStreamResponse(a.to, a.ctx.Format, upstreamReader, sink, a.rt.ActualModel, func() {
			flushWriter(a.w)
		})
		if err != nil {
			log.Printf("audit_id=%d stream conversion error: %v", a.auditID, err)
		}
	} else {
		copyStream(upstreamReader, sink, func() { flushWriter(a.w) })
	}
	flushWriter(a.w)

	rawText := collected.String()
	var forwardedPtr *string
	if a.convert {
		forwardedText := forwarded.String()
		forwardedPtr = &forwardedText
	}
	var tokens audit.TokenUsage
	if rawText != "" {
		tokens = audit.ExtractTokensFromStream(rawText)
	}
	cost := computeCost(a.state, a.rt.ActualModel, tokens)
	_ = a.state.Audit.UpdateStreamingResponse(a.auditID, rawText, forwardedPtr,
		a.status, uint64(time.Since(a.start).Milliseconds()), cost)
	if a.debug {
		log.Printf("[DEBUG] audit_id=%d streaming finished: upstream=%d bytes forwarded=%d bytes",
			a.auditID, len(rawText), forwarded.Len())
	}
	return nil
}

// copyStream 透传：边读边写，保证首包尽快到达客户端。
func copyStream(src io.Reader, dst io.Writer, flush func()) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			return
		}
	}
}

// ---------- 上游字节流 → chunk 通道 ----------

type chunk struct {
	data []byte
	err  error
}

// readChunks 在后台把 reader 切成 chunk 送入通道。
// 调用方必须读到通道关闭为止，否则该 goroutine 会阻塞在发送上。
func readChunks(body io.Reader) <-chan chunk {
	out := make(chan chunk, 1)
	go func() {
		defer close(out)
		buf := make([]byte, 32*1024)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				out <- chunk{data: b}
			}
			if err != nil {
				out <- chunk{err: err}
				return
			}
		}
	}()
	return out
}

// readChunkWithTimeout 带超时地取一个 chunk。
// 上游正常结束（EOF）返回 (nil, nil)；超时返回错误。
func readChunkWithTimeout(chunks <-chan chunk, timeout time.Duration) ([]byte, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case c, ok := <-chunks:
		if !ok {
			return nil, nil
		}
		if c.err != nil {
			if errors.Is(c.err, io.EOF) {
				return c.data, nil
			}
			return c.data, fmt.Errorf("network_error: %s", c.err)
		}
		return c.data, nil
	case <-timer.C:
		return nil, errors.New("timeout")
	}
}

// firstThenChanReader 先吐出缓冲的首个 chunk，再继续读通道。
//
// 空闲超时按「等同于上游结束」处理：与 Rust 一致，超时后让转换器走 finalize()，
// 使下游仍能收到 message_stop / finish_reason 等终止事件（否则会丢结束事件）。
type firstThenChanReader struct {
	// pending 是已取到但尚未交付给调用方的字节。
	pending []byte
	chunks  <-chan chunk
	idle    time.Duration
	done    bool
}

func (r *firstThenChanReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// fill 取下一个 chunk 到 pending。上游结束或空闲超时都返回 io.EOF，
// 让转换器可以走 finalize() 补出终止事件。
func (r *firstThenChanReader) fill() error {
	timer := time.NewTimer(r.idle)
	defer timer.Stop()
	select {
	case c, ok := <-r.chunks:
		if !ok {
			r.done = true
			return io.EOF
		}
		if c.err != nil {
			r.done = true
			if errors.Is(c.err, io.EOF) {
				r.pending = c.data
				return nil
			}
			return c.err
		}
		r.pending = c.data
		return nil
	case <-timer.C:
		log.Printf("streaming idle timeout (%s); finalizing downstream stream", r.idle)
		r.done = true
		return io.EOF
	}
}

// ---------- 小工具 ----------

func writeSSEHeaders(w http.ResponseWriter, status int) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(status)
}

func flushWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// upstreamErrorMessage 从上游错误体里取 error.message，取不到则截断原文。
func upstreamErrorMessage(raw string) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err == nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	return preview(raw, 200)
}

func ptrStr(s string) *string { return &s }

// computeCost 按实际模型查定价并计费；未配置定价时返回 nil
// （对应 Rust `compute_cost_from_config_ref` 返回 None）。
func computeCost(state *app.State, actualModel string, tokens audit.TokenUsage) *float64 {
	return cost.ForModelTotal(state.Config(), actualModel, cost.Usage{
		Input:         tokens.Input,
		Output:        tokens.Output,
		CacheRead:     tokens.CacheRead,
		CacheCreation: tokens.CacheCreation,
	})
}
