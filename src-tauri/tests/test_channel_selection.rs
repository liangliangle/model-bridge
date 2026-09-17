//! 回归测试：渠道候选选择（`select_routes`）。
//!
//! 两层规则，顺序不可颠倒：
//! 1. **硬过滤**：渠道必须能服务该入口协议。协议矩阵的规则是
//!    「渠道侧与请求方同协议（透传），或渠道侧是 Chat（转换）」，即
//!    `请求方 == 渠道方 || 渠道方 == Chat`。不可服务的渠道直接排除，
//!    一个都不剩时返回 `RouteError::UnsupportedProtocol`（而非静默降级）。
//! 2. **优先级排序**：在剩余候选里顺序完全由 `priority` 决定，协议不参与排序。
//!
//! 另覆盖候选数量上限：由 `failover.max_failover_channels` 控制，`0` = 不限制。
//! 该字段原名 `max_retries`，旧实现下取值 `0` 会清空全部候选导致全量 503。

use model_bridge_lib::channel::config::{AppConfig, ChannelConfig, EndpointConfig, ProviderType};
use model_bridge_lib::channel::failover::{select_routes, RouteError};
use model_bridge_lib::channel::health::{new_health_map, ChannelHealth, HealthState};
use model_bridge_lib::proxy::context::InputFormat;
use std::collections::HashMap;

fn ch(id: &str, provider: ProviderType, priority: u32) -> ChannelConfig {
    ChannelConfig {
        id: id.to_string(),
        name: id.to_string(),
        provider,
        endpoint: EndpointConfig { url: "http://example.invalid/v1/x".into() },
        api_key: String::new(),
        priority,
        enabled: true,
        fallback_model: "fallback-model".into(),
        model_mapping: HashMap::new(),
        timeout_ms: 1000,
        rate_limit: None,
        custom_headers: HashMap::new(),
        strip_thinking: false,
        retry_count: 0,
        retry_delay_ms: 500,
        force_effort: None,
        auto_cache: true,
    }
}

fn app_config(channels: Vec<ChannelConfig>, limit: u32) -> AppConfig {
    let mut c = AppConfig::default_config();
    c.channels = channels;
    c.failover.max_failover_channels = limit;
    c
}

fn selected(config: &AppConfig, input: InputFormat) -> Vec<String> {
    let health = new_health_map(&[]);
    select_routes(config, &health, "gpt-4o", input)
        .expect("该组合应可路由")
        .iter()
        .map(|r| r.channel.id.clone())
        .collect()
}

fn route_error(config: &AppConfig, input: InputFormat) -> RouteError {
    let health = new_health_map(&[]);
    select_routes(config, &health, "gpt-4o", input).expect_err("该组合不应可路由")
}

// ==================== 协议矩阵：硬过滤 ====================

/// Chat 请求方只能走 Chat 渠道：Anthropic / Responses 渠道一律排除。
#[test]
fn chat_input_only_routes_to_chat_channel() {
    let cfg = app_config(
        vec![
            ch("A-anthropic", ProviderType::Anthropic, 1),
            ch("B-responses", ProviderType::OpenaiResponses, 2),
            ch("C-chat", ProviderType::Openai, 3),
        ],
        10,
    );
    assert_eq!(selected(&cfg, InputFormat::OpenAI), vec!["C-chat"]);
}

/// Messages 请求方可以走 Messages（透传）或 Chat（转换），不能走 Responses。
#[test]
fn messages_input_routes_to_messages_or_chat() {
    let cfg = app_config(
        vec![
            ch("A-anthropic", ProviderType::Anthropic, 1),
            ch("B-responses", ProviderType::OpenaiResponses, 2),
            ch("C-chat", ProviderType::Openai, 3),
        ],
        10,
    );
    assert_eq!(
        selected(&cfg, InputFormat::Anthropic),
        vec!["A-anthropic", "C-chat"]
    );
}

/// Responses 请求方可以走 Responses（透传）或 Chat（转换），不能走 Messages。
#[test]
fn responses_input_routes_to_responses_or_chat() {
    let cfg = app_config(
        vec![
            ch("A-anthropic", ProviderType::Anthropic, 1),
            ch("B-responses", ProviderType::OpenaiResponses, 2),
            ch("C-chat", ProviderType::Openai, 3),
        ],
        10,
    );
    assert_eq!(
        selected(&cfg, InputFormat::Responses),
        vec!["B-responses", "C-chat"]
    );
}

/// 有健康渠道、但没有一个能服务该协议的组合，必须报明确原因而不是「没有渠道」。
#[test]
fn unsupported_protocol_is_reported_distinctly() {
    // Chat 请求方，只配了 Anthropic + Responses 渠道
    let cfg = app_config(
        vec![
            ch("A-anthropic", ProviderType::Anthropic, 1),
            ch("B-responses", ProviderType::OpenaiResponses, 2),
        ],
        10,
    );
    assert_eq!(
        route_error(&cfg, InputFormat::OpenAI),
        RouteError::UnsupportedProtocol { input: InputFormat::OpenAI }
    );

    // Messages 请求方只配了 Responses 渠道
    let cfg2 = app_config(vec![ch("B-responses", ProviderType::OpenaiResponses, 1)], 10);
    assert_eq!(
        route_error(&cfg2, InputFormat::Anthropic),
        RouteError::UnsupportedProtocol { input: InputFormat::Anthropic }
    );
}

/// 没有任何启用/健康渠道 → 与「协议不匹配」区分开。
#[test]
fn no_available_channel_is_reported_distinctly() {
    let cfg = app_config(vec![ch("A-chat", ProviderType::Openai, 1)], 10);
    let health = new_health_map(&[]);
    {
        let mut h = health.write();
        let mut down = ChannelHealth::new();
        down.state = HealthState::Unhealthy;
        h.insert("A-chat".to_string(), down);
    }
    assert_eq!(
        select_routes(&cfg, &health, "gpt-4o", InputFormat::OpenAI).unwrap_err(),
        RouteError::NoAvailableChannel
    );
}

// ==================== 优先级排序（在可用候选内） ====================

/// 过滤之后，顺序仍严格由优先级决定。
#[test]
fn priority_orders_the_supported_candidates() {
    let cfg = app_config(
        vec![
            ch("chat-p3", ProviderType::Openai, 3),
            ch("chat-p1", ProviderType::Openai, 1),
            ch("chat-p2", ProviderType::Openai, 2),
        ],
        10,
    );
    assert_eq!(
        selected(&cfg, InputFormat::OpenAI),
        vec!["chat-p1", "chat-p2", "chat-p3"]
    );
}

/// Messages 请求方：优先级高的 Chat 渠道要排在优先级低的 Messages 渠道之前
/// （协议不参与排序，只参与准入）。
#[test]
fn chat_channel_wins_when_priority_is_higher() {
    let cfg = app_config(
        vec![
            ch("openai-p1", ProviderType::Openai, 1),
            ch("anthropic-p2", ProviderType::Anthropic, 2),
        ],
        10,
    );
    assert_eq!(
        selected(&cfg, InputFormat::Anthropic),
        vec!["openai-p1", "anthropic-p2"]
    );
}

/// 优先级相同时保持配置文件顺序。
#[test]
fn equal_priority_keeps_config_order() {
    let cfg = app_config(
        vec![
            ch("first-p5", ProviderType::Openai, 5),
            ch("second-p5", ProviderType::Openai, 5),
        ],
        10,
    );
    assert_eq!(selected(&cfg, InputFormat::OpenAI), vec!["first-p5", "second-p5"]);
}

/// 熔断渠道被跳过，其余仍按优先级。
#[test]
fn unhealthy_channels_are_skipped() {
    let cfg = app_config(
        vec![
            ch("A-chat-p1", ProviderType::Openai, 1),
            ch("B-chat-p2", ProviderType::Openai, 2),
        ],
        10,
    );
    let health = new_health_map(&[]);
    {
        let mut h = health.write();
        let mut down = ChannelHealth::new();
        down.state = HealthState::Unhealthy;
        h.insert("A-chat-p1".to_string(), down);
    }
    let got: Vec<String> = select_routes(&cfg, &health, "gpt-4o", InputFormat::OpenAI)
        .unwrap()
        .iter()
        .map(|r| r.channel.id.clone())
        .collect();
    assert_eq!(got, vec!["B-chat-p2"]);
}

// ==================== 候选数量上限 ====================

#[test]
fn default_limit_is_three() {
    assert_eq!(AppConfig::default_config().failover.max_failover_channels, 3);
}

#[test]
fn legacy_max_retries_key_still_loads() {
    let cfg: AppConfig = serde_yaml::from_str("failover:\n  max_retries: 7\n").unwrap();
    assert_eq!(cfg.failover.max_failover_channels, 7, "旧字段名应作为别名继续生效");
}

#[test]
fn serialization_uses_new_key() {
    let yaml = serde_yaml::to_string(&AppConfig::default_config()).unwrap();
    assert!(yaml.contains("max_failover_channels: 3"));
    assert!(!yaml.contains("max_retries"), "序列化不应再写出旧字段名");
}

/// 核心 bug 回归：旧实现下 max_retries=0 会 truncate(0) 清空候选，导致全量 503。
#[test]
fn legacy_zero_limit_no_longer_empties_candidates() {
    let mut cfg: AppConfig = serde_yaml::from_str("failover:\n  max_retries: 0\n").unwrap();
    assert_eq!(cfg.failover.max_failover_channels, 0);
    cfg.channels = vec![
        ch("a", ProviderType::Openai, 1),
        ch("b", ProviderType::Openai, 2),
    ];
    assert_eq!(selected(&cfg, InputFormat::OpenAI), vec!["a", "b"]);
}

#[test]
fn positive_limit_caps_candidates() {
    let cfg = app_config(
        vec![
            ch("a", ProviderType::Openai, 1),
            ch("b", ProviderType::Openai, 2),
            ch("c", ProviderType::Openai, 3),
        ],
        2,
    );
    assert_eq!(selected(&cfg, InputFormat::OpenAI), vec!["a", "b"]);
}

/// 上限只约束「尝试几个渠道」，不影响单渠道内的重试次数。
#[test]
fn limit_is_independent_from_per_channel_retry_count() {
    let mut a = ch("a", ProviderType::Openai, 1);
    a.retry_count = 2;
    a.retry_delay_ms = 50;
    let cfg = app_config(vec![a, ch("b", ProviderType::Openai, 2)], 1);
    assert_eq!(selected(&cfg, InputFormat::OpenAI), vec!["a"]);
    assert_eq!(cfg.channels[0].retry_count, 2);
}

// ==================== 终极兜底也要守协议矩阵 ====================

#[test]
fn ultimate_fallback_respects_protocol_matrix() {
    // 兜底渠道是 Anthropic，但请求方是 Chat → 不能兜（否则会退化成已下线的方向）
    let mut cfg = app_config(
        vec![ch("A-anthropic", ProviderType::Anthropic, 1)],
        10,
    );
    cfg.ultimate_fallback = Some(model_bridge_lib::channel::config::UltimateFallback {
        channel: "A-anthropic".to_string(),
    });
    assert_eq!(
        route_error(&cfg, InputFormat::OpenAI),
        RouteError::UnsupportedProtocol { input: InputFormat::OpenAI }
    );

    // 兜底渠道协议匹配时仍可生效
    let mut cfg2 = app_config(
        vec![ch("A-anthropic", ProviderType::Anthropic, 1)],
        10,
    );
    cfg2.ultimate_fallback = Some(model_bridge_lib::channel::config::UltimateFallback {
        channel: "A-anthropic".to_string(),
    });
    assert_eq!(selected(&cfg2, InputFormat::Anthropic), vec!["A-anthropic"]);
}
