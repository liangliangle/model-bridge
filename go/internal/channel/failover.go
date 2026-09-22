package channel

import (
	"fmt"

	"modelbridge/internal/config"
	"modelbridge/internal/converter"
)

// 路由选择：按协议矩阵硬过滤，再按优先级排序。
// 对应 Rust `channel/failover.rs::select_routes`。

// RouteDecision 是为某个模型别名选中的渠道路由。
type RouteDecision struct {
	Channel       *config.ChannelConfig
	ActualModel   string
	MappingSource config.MappingSource
}

// RouteErrorKind 区分「没有可用渠道」与「渠道协议都不匹配」。
type RouteErrorKind int

const (
	// RouteNoAvailableChannel 表示没有任何启用且健康的渠道。
	RouteNoAvailableChannel RouteErrorKind = iota
	// RouteUnsupportedProtocol 表示有健康渠道，但协议矩阵不允许服务该入口协议。
	RouteUnsupportedProtocol
)

// RouteError 是路由失败原因。
type RouteError struct {
	Kind  RouteErrorKind
	Input converter.ApiFormat
}

func (e *RouteError) Error() string {
	if e.Kind == RouteUnsupportedProtocol {
		return fmt.Sprintf("no channel can serve the %q input protocol", e.Input.String())
	}
	return "no available channels"
}

// FormatForProvider 把渠道供应商类型映射为上游协议格式
// （对应 Rust `ApiFormat::from_provider`）。
func FormatForProvider(p config.ProviderType) converter.ApiFormat {
	switch p {
	case config.ProviderAnthropic:
		return converter.FormatAnthropic
	case config.ProviderOpenAIResponses:
		return converter.FormatResponses
	default:
		return converter.FormatOpenAIChat
	}
}

// IsSupported 判定「入口协议 → 渠道协议」组合是否受协议矩阵支持。
// 与 converter.ApiFormat.CanRouteTo 共用同一事实来源（对应 Rust `FormatConverter::is_supported`）。
func IsSupported(in converter.ApiFormat, p config.ProviderType) bool {
	return in.CanRouteTo(FormatForProvider(p))
}

// SelectRoutes 为指定模型别名选择可用的渠道路由列表。
//
// 两层规则，顺序不能颠倒：
//  1. 硬过滤：渠道必须能服务该入口协议——同协议（透传）或 Chat（转换）。
//     不可服务的渠道直接排除，绝不允许退化成错误的协议转换。
//  2. 优先级排序：在剩下的候选里，顺序完全由 priority 决定（数值越小越靠前），
//     协议格式不参与排序；优先级相同时保持配置文件顺序。
//
// 候选数量上限由 failover.max_failover_channels 控制，0 表示不限制。
// 该上限只约束「尝试几个渠道」，与单渠道内重试次数（channel.retry_count）无关。
func SelectRoutes(
	cfg *config.AppConfig,
	health *HealthMap,
	modelAlias string,
	in converter.ApiFormat,
) ([]RouteDecision, error) {
	limit, hasLimit := cfg.CandidateLimit()

	// 健康且启用的渠道总数（不论协议），用于区分「没渠道」与「渠道协议不匹配」。
	healthyTotal := 0
	routes := make([]RouteDecision, 0, len(cfg.Channels))

	// SortedChannels 已按 priority 升序稳定排序（且过滤 enabled），过滤后仍保持该顺序。
	for _, ch := range cfg.SortedChannels() {
		if !health.IsAvailable(ch.ID) {
			continue
		}
		healthyTotal++
		if !IsSupported(in, ch.Provider) {
			continue
		}
		actualModel, source := ch.ResolveModel(modelAlias)
		routes = append(routes, RouteDecision{
			Channel:       ch,
			ActualModel:   actualModel,
			MappingSource: source,
		})
	}

	// 0 = 不限制：绝不能退化成截断为空，否则所有请求都会 503。
	if hasLimit && len(routes) > limit {
		routes = routes[:limit]
	}

	// 终极兜底渠道同样要满足协议矩阵，否则会退化成已下线的转换方向。
	if len(routes) == 0 && cfg.UltimateFallback != nil {
		if ch := cfg.FindChannel(cfg.UltimateFallback.Channel); ch != nil && IsSupported(in, ch.Provider) {
			actualModel, source := ch.ResolveModel(modelAlias)
			routes = append(routes, RouteDecision{
				Channel:       ch,
				ActualModel:   actualModel,
				MappingSource: source,
			})
		}
	}

	if len(routes) > 0 {
		return routes, nil
	}
	// 有健康渠道却一个都用不上 → 是协议不匹配，而不是「没有渠道」。
	if healthyTotal > 0 {
		return nil, &RouteError{Kind: RouteUnsupportedProtocol, Input: in}
	}
	return nil, &RouteError{Kind: RouteNoAvailableChannel}
}
