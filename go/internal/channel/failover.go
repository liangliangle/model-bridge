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
	//
	// 矩阵全开（任意协议互转）之后这一分支**不可达**：任意入口协议都能落到任意渠道协议上，
	// 因此「有健康渠道却一个都用不上」不再可能。保留它是为了未来若重新收窄矩阵时仍有落点，
	// 对应文案与单测也一并保留。
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

// SelectRoutes 为指定模型别名选择可用的渠道路由列表。
//
// 规则只有一层：**按 priority 排序**（数值越小越靠前），优先级相同时保持配置文件顺序。
//
// 协议不参与渠道决策：任意入口协议都能落到任意渠道协议上（转换路径由
// converter.NewPlan 规划，最多经 Chat 两跳），因此这里不再按协议过滤候选。
// 这也意味着候选池比「单跳矩阵」时期更大——优先级更高的渠道即使协议不同也会被选中，
// 并为此付出一次（或两次）协议转换。
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

	healthyTotal := 0
	routes := make([]RouteDecision, 0, len(cfg.Channels))

	// SortedChannels 已按 priority 升序稳定排序（且过滤 enabled）。
	for _, ch := range cfg.SortedChannels() {
		if !health.IsAvailable(ch.ID) {
			continue
		}
		healthyTotal++
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

	// 终极兜底渠道与普通候选一样：只看是否存在，不看协议。
	if len(routes) == 0 && cfg.UltimateFallback != nil {
		if ch := cfg.FindChannel(cfg.UltimateFallback.Channel); ch != nil {
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
	// 走到这里说明一个候选都没有：矩阵全开之后，剩下的唯一可能是「没有健康的可用渠道」。
	// RouteUnsupportedProtocol 分支保留但**当前不可达**（见该常量的注释），
	// 仅在未来重新收窄矩阵时才可能重新触发。
	if healthyTotal > 0 {
		return nil, &RouteError{Kind: RouteUnsupportedProtocol, Input: in}
	}
	return nil, &RouteError{Kind: RouteNoAvailableChannel}
}
