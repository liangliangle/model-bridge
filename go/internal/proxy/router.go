package proxy

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"modelbridge/internal/app"
	"modelbridge/internal/auth"
	"modelbridge/internal/channel"
	"modelbridge/internal/config"
	"modelbridge/internal/converter"
)

// 路由器：选择渠道、建审计记录、跑 failover 循环。
// 不做格式转换、不处理 header，只负责「选谁」和「记日志」。
// 对应 Rust `proxy/router.rs::route_request`。

// RouteRequest 是代理入口的统一出口。
func RouteRequest(state *app.State, ctx *RequestContext, w http.ResponseWriter) {
	start := time.Now()

	if ctx.ParseError != "" {
		msg := fmt.Sprintf("Invalid JSON request: %s", ctx.ParseError)
		if id, err := state.Audit.Create(http.MethodPost, ctx.Path, ctx.ModelAlias,
			&ctx.HeadersJSON, ptrStr(string(ctx.RawBody)), ptrStrOrNil(ctx.UserAgent)); err == nil {
			_ = state.Audit.UpdateError(id, http.StatusBadRequest, msSince(start), msg)
		}
		auth.WriteError(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}

	cfg := state.Config()
	routes, routeErr := selectRoutes(cfg, state.Health, ctx.ModelAlias, ctx.Format)
	if routeErr != nil {
		status, errType, message := routeErrorResponse(ctx, routeErr)
		if id, err := state.Audit.Create(http.MethodPost, ctx.Path, ctx.ModelAlias,
			&ctx.HeadersJSON, ptrStr(string(ctx.RawBody)), ptrStrOrNil(ctx.UserAgent)); err == nil {
			_ = state.Audit.UpdateError(id, status, msSince(start), message)
		}
		auth.WriteError(w, status, errType, message)
		return
	}

	var lastError string
	failoverChain := make([]string, 0, len(routes))
	var totalRetries uint32

	for _, route := range routes {
		ch := route.Channel
		maxAttempts := 1 + int(ch.RetryCount)
		failoverChain = append(failoverChain, ch.ID)

		for attempt := 0; attempt < maxAttempts; attempt++ {
			if attempt > 0 {
				totalRetries++
				time.Sleep(time.Duration(ch.RetryDelayMs) * time.Millisecond)
				log.Printf("channel=%s attempt=%d/%d retrying on same channel", ch.ID, attempt+1, maxAttempts)
			}
			attemptStart := time.Now()

			auditID, err := state.Audit.Create(http.MethodPost, ctx.Path, ctx.ModelAlias,
				&ctx.HeadersJSON, ptrStr(string(ctx.RawBody)), ptrStrOrNil(ctx.UserAgent))
			if err != nil {
				auditID = -1
			}
			_ = state.Audit.UpdateRoute(auditID, ch.ID, route.ActualModel, string(route.MappingSource))

			execErr := ExecuteOnChannel(state, ctx, route, auditID, w)
			latency := msSince(attemptStart)

			if execErr == nil {
				_ = state.Audit.UpdateRetryInfo(auditID, totalRetries, failoverChain)
				state.Health.RecordSuccess(ch.ID, latency, cfg.Failover.CircuitBreaker)
				return
			}

			msg := execErr.Error()
			errorStatus := http.StatusBadGateway
			if strings.HasPrefix(msg, "invalid_request:") {
				errorStatus = http.StatusBadRequest
			}
			_ = state.Audit.UpdateError(auditID, errorStatus, latency, msg)
			_ = state.Audit.UpdateRetryInfo(auditID, totalRetries, failoverChain)
			lastError = msg

			// 仅在该渠道所有重试耗尽后才更新熔断计数。
			if attempt+1 == maxAttempts {
				state.Health.RecordFailure(ch.ID, cfg.Failover.CircuitBreaker)
			}

			// 请求本身不合法时换渠道也没有意义，直接返回。
			if errorStatus == http.StatusBadRequest {
				auth.WriteError(w, http.StatusBadRequest, "invalid_request_error", msg)
				return
			}
		}
	}

	status := http.StatusBadGateway
	if strings.HasPrefix(lastError, "invalid_request:") {
		status = http.StatusBadRequest
	}
	auth.WriteError(w, status, "server_error", "All channels failed: "+lastError)
}

// routeErrorResponse 把选路错误映射为 HTTP 状态与文案。
func routeErrorResponse(ctx *RequestContext, err error) (int, string, string) {
	var re *channel.RouteError
	if errors.As(err, &re) && re.Kind == channel.RouteUnsupportedProtocol {
		// 请求一律在派发前拒绝：不做「先发出去再失败重试」，
		// 避免浪费一次上游调用，也让用户看到可定位的原因。
		return http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
			"No channel can serve the '%s' input protocol: this gateway requires %s. "+
				"Add a matching-format channel, or a chat channel to convert into.",
			ctx.ProtocolName(), AllowedProviderHint(ctx.Format))
	}
	return http.StatusServiceUnavailable, "server_error", "No available channels"
}

// selectRoutes 是 channel 包选路结果的唯一适配点：把选路决定转成代理侧的 Route。
// 其余代码只依赖 Route，便于与 channel 包的实际签名保持解耦。
func selectRoutes(cfg *config.AppConfig, health *channel.HealthMap, modelAlias string, in converter.ApiFormat) ([]Route, error) {
	decisions, err := channel.SelectRoutes(cfg, health, modelAlias, in)
	if err != nil {
		return nil, err
	}
	routes := make([]Route, 0, len(decisions))
	for _, d := range decisions {
		routes = append(routes, Route{
			Channel:       d.Channel,
			ActualModel:   d.ActualModel,
			MappingSource: d.MappingSource,
		})
	}
	return routes, nil
}

func msSince(t time.Time) uint64 { return uint64(time.Since(t).Milliseconds()) }

func ptrStrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
