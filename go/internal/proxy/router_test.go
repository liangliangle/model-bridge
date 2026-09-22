package proxy

import (
	"net/http"
	"strings"
	"testing"

	"modelbridge/internal/channel"
	"modelbridge/internal/converter"
)

// 选路错误 → HTTP 响应 的映射测试。
//
// 矩阵全开之后 RouteUnsupportedProtocol 分支**不可达**（任意协议组合都能服务），
// 但分支与文案保留着，作为未来若重新收窄矩阵时的落点——因此这里仍然把它钉住，
// 避免它悄悄腐坏成一句过时的话。

func TestRouteErrorResponse(t *testing.T) {
	ctx := NewRequestContext(converter.FormatAnthropic, "/v1/messages", http.Header{}, nil)

	t.Run("协议不受支持（当前不可达，保留文案）", func(t *testing.T) {
		status, errType, msg := routeErrorResponse(ctx, &channel.RouteError{
			Kind:  channel.RouteUnsupportedProtocol,
			Input: converter.FormatAnthropic,
		})
		if status != http.StatusBadRequest || errType != "invalid_request_error" {
			t.Fatalf("status/type = %d/%s", status, errType)
		}
		// 文案要能定位：点名入口协议，并说明三种协议之间都能互转。
		if !strings.Contains(msg, "messages") {
			t.Fatalf("文案应点名入口协议：%q", msg)
		}
		if !strings.Contains(msg, "chat / messages / responses") {
			t.Fatalf("文案应说明矩阵已全开：%q", msg)
		}
	})

	t.Run("没有可用渠道", func(t *testing.T) {
		status, errType, msg := routeErrorResponse(ctx, &channel.RouteError{Kind: channel.RouteNoAvailableChannel})
		if status != http.StatusServiceUnavailable || errType != "server_error" {
			t.Fatalf("status/type = %d/%s", status, errType)
		}
		if msg != "No available channels" {
			t.Fatalf("文案 = %q", msg)
		}
	})
}
