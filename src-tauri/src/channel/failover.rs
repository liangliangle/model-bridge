use serde::{Deserialize, Serialize};

use super::config::{AppConfig, ChannelConfig, MappingSource, ProviderType};
use super::health::HealthMap;
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

/// 判断渠道的 ProviderType 是否与输入格式一致（用于同格式优先排序，不再做硬过滤）
fn format_matches(input: InputFormat, provider: &ProviderType) -> bool {
    matches!(
        (input, provider),
        (InputFormat::OpenAI, ProviderType::Openai)
        | (InputFormat::Anthropic, ProviderType::Anthropic)
        | (InputFormat::Responses, ProviderType::OpenaiResponses)
    )
}

/// 为指定模型别名选择可用的渠道路由列表。
/// 所有健康且可路由的渠道都进入候选（跨格式渠道由 converter 层做协议转换），
/// 同格式渠道优先排序（transform 成本更低），同格式/跨格式各自内部保持优先级顺序。
pub fn select_routes(
    config: &AppConfig,
    health_map: &HealthMap,
    model_alias: &str,
    input_format: InputFormat,
) -> Vec<RouteDecision> {
    let health = health_map.read();
    let max = config.failover.max_retries as usize;

    // 先按优先级收集所有健康渠道，再按「是否同格式」稳定分区，同格式在前
    let mut same_format: Vec<RouteDecision> = Vec::new();
    let mut cross_format: Vec<RouteDecision> = Vec::new();

    for channel in config.sorted_channels() {
        let channel_health = health.get(&channel.id);
        let is_available = channel_health.map(|h| h.is_available()).unwrap_or(true);
        if !is_available {
            continue;
        }

        let (actual_model, mapping_source) = channel.resolve_model(model_alias);
        let decision = RouteDecision {
            channel: channel.clone(),
            actual_model,
            mapping_source,
        };

        if format_matches(input_format, &channel.provider) {
            same_format.push(decision);
        } else {
            cross_format.push(decision);
        }
    }

    let mut routes: Vec<RouteDecision> = same_format;
    routes.extend(cross_format);
    routes.truncate(max);

    // 所有渠道不可用时，尝试终极兜底渠道（不再要求格式匹配，跨格式由 converter 处理）
    if routes.is_empty() {
        if let Some(ref fallback) = config.ultimate_fallback {
            if let Some(channel) = config.channels.iter().find(|c| c.id == fallback.channel) {
                let (actual_model, mapping_source) = channel.resolve_model(model_alias);
                routes.push(RouteDecision {
                    channel: channel.clone(),
                    actual_model,
                    mapping_source,
                });
            }
        }
    }

    routes
}
