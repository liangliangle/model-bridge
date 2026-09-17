use serde::{Deserialize, Serialize};

use super::config::{candidate_limit, AppConfig, ChannelConfig, MappingSource};
use super::health::HealthMap;

use crate::converter::FormatConverter;
use crate::proxy::context::InputFormat;

/// 路由决策结果，表示为某个模型别名选中的渠道和实际模型
#[derive(Debug, Clone)]
pub struct RouteDecision {
    pub channel: ChannelConfig,       // 选中的渠道配置
    pub actual_model: String,         // 解析后的实际模型名称
    pub mapping_source: MappingSource, // 模型名称的映射来源
}

/// 故障转移链中单次尝试的记录，用于追踪请求经过的渠道
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FailoverAttempt {
    pub channel: String,              // 尝试的渠道 ID
    pub mapped_model: String,         // 使用的模型名称
    pub mapping_source: MappingSource, // 模型映射来源
    pub status: String,               // 请求结果状态
    pub latency_ms: u64,              // 请求耗时（毫秒）
}

/// 路由失败原因。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RouteError {
    /// 没有任何启用且可用的渠道。
    NoAvailableChannel,
    /// 有可用渠道，但协议矩阵不允许服务该入口协议（渠道侧既非同协议也非 Chat）。
    UnsupportedProtocol { input: InputFormat },
}

/// 为指定模型别名选择可用的渠道路由列表。
///
/// 两层规则，顺序不能颠倒：
/// 1. **硬过滤**：渠道必须能服务该入口协议——同协议（透传）或 Chat（转换），
///    由 [`FormatConverter::is_supported`] 判定。不可服务的渠道直接排除，
///    绝不允许退化成错误的协议转换。
/// 2. **优先级排序**：在剩下的候选里，顺序**完全由 `priority` 决定**
///    （数值越小越靠前），协议格式不再参与排序；优先级相同时保持配置文件顺序。
///
/// 候选数量上限由 `failover.max_failover_channels` 控制，`0` 表示不限制。
/// 该上限只约束「尝试几个渠道」，与单渠道内重试次数（`channel.retry_count`）无关。
pub fn select_routes(
    config: &AppConfig,
    health_map: &HealthMap,
    model_alias: &str,
    input_format: InputFormat,
) -> Result<Vec<RouteDecision>, RouteError> {
    let health = health_map.read();
    let max_candidates = candidate_limit(config.failover.max_failover_channels);

    // sorted_channels() 已按 priority 升序稳定排序，过滤后仍保持该顺序
    let mut routes: Vec<RouteDecision> = Vec::new();
    // 健康且启用的渠道总数（不论协议），用于区分「没渠道」与「渠道协议不匹配」
    let mut healthy_total = 0usize;

    for channel in config.sorted_channels() {
        let channel_health = health.get(&channel.id);
        let is_available = channel_health.map(|h| h.is_available()).unwrap_or(true);
        if !is_available {
            continue;
        }
        healthy_total += 1;

        if !FormatConverter::is_supported(input_format, &channel.provider) {
            continue;
        }

        let (actual_model, mapping_source) = channel.resolve_model(model_alias);
        routes.push(RouteDecision {
            channel: channel.clone(),
            actual_model,
            mapping_source,
        });
    }

    // 0 = 不限制：绝不能退化成 truncate(0) 清空全部候选，否则所有请求都会 503
    if let Some(limit) = max_candidates {
        routes.truncate(limit);
    }

    // 终极兜底渠道同样要满足协议矩阵，否则会退化成已下线的转换方向
    if routes.is_empty() {
        if let Some(ref fallback) = config.ultimate_fallback {
            if let Some(channel) = config.channels.iter().find(|c| c.id == fallback.channel) {
                if FormatConverter::is_supported(input_format, &channel.provider) {
                    let (actual_model, mapping_source) = channel.resolve_model(model_alias);
                    routes.push(RouteDecision {
                        channel: channel.clone(),
                        actual_model,
                        mapping_source,
                    });
                }
            }
        }
    }

    if !routes.is_empty() {
        return Ok(routes);
    }
    // 有健康渠道却一个都用不上 → 是协议不匹配，而不是「没有渠道」
    if healthy_total > 0 {
        Err(RouteError::UnsupportedProtocol {
            input: input_format,
        })
    } else {
        Err(RouteError::NoAvailableChannel)
    }
}
