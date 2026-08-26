use chrono::{DateTime, Utc};
use parking_lot::RwLock;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use super::config::CircuitBreakerConfig;

/// 渠道健康状态枚举
#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HealthState {
    Healthy,    // 健康，可正常使用
    Degraded,   // 降级，出现部分失败但未触发熔断
    Unhealthy,  // 不健康，已被熔断
    Recovering, // 恢复中，正在进行探测请求
}

/// 单个渠道的健康状态信息，跟踪请求成功率和延迟
#[derive(Debug, Clone, Serialize)]
pub struct ChannelHealth {
    pub state: HealthState,                       // 当前健康状态
    pub consecutive_failures: u32,                // 连续失败次数
    pub consecutive_successes: u32,               // 连续成功次数
    pub total_requests: u64,                      // 总请求数
    pub total_failures: u64,                      // 总失败数
    pub avg_latency_ms: u64,                      // 平均延迟（毫秒）
    pub last_failure_time: Option<DateTime<Utc>>, // 最后一次失败时间
    pub last_success_time: Option<DateTime<Utc>>, // 最后一次成功时间
    latency_samples: Vec<u64>,                    // 延迟采样数据（保留最近 100 条）
}

impl ChannelHealth {
    /// 创建初始状态为健康的渠道健康实例
    pub fn new() -> Self {
        Self {
            state: HealthState::Healthy,
            consecutive_failures: 0,
            consecutive_successes: 0,
            total_requests: 0,
            total_failures: 0,
            avg_latency_ms: 0,
            last_failure_time: None,
            last_success_time: None,
            latency_samples: Vec::new(),
        }
    }

    /// 记录一次成功请求，更新延迟和状态转换
    pub fn record_success(&mut self, latency_ms: u64, config: &CircuitBreakerConfig) {
        self.total_requests += 1;
        self.consecutive_failures = 0;
        self.consecutive_successes += 1;
        self.last_success_time = Some(Utc::now());
        self.update_latency(latency_ms);
        match self.state {
            // 恢复中：连续成功次数达到探测阈值后转为健康
            HealthState::Recovering => {
                if self.consecutive_successes >= config.probe_requests {
                    self.state = HealthState::Healthy;
                }
            }
            // 降级：一次成功即恢复为健康
            HealthState::Degraded => {
                self.state = HealthState::Healthy;
            }
            _ => {}
        }
    }

    /// 记录一次失败请求，根据连续失败次数触发状态降级或熔断
    pub fn record_failure(&mut self, config: &CircuitBreakerConfig) {
        self.total_requests += 1;
        self.total_failures += 1;
        self.consecutive_failures += 1;
        self.consecutive_successes = 0;
        self.last_failure_time = Some(Utc::now());
        match self.state {
            // 健康：达到阈值一半时降级，达到阈值时熔断
            HealthState::Healthy => {
                if self.consecutive_failures >= config.failure_threshold {
                    self.state = HealthState::Unhealthy;
                } else if self.consecutive_failures >= (config.failure_threshold / 2).max(1) {
                    self.state = HealthState::Degraded;
                }
            }
            // 降级：继续失败达到阈值则熔断
            HealthState::Degraded => {
                if self.consecutive_failures >= config.failure_threshold {
                    self.state = HealthState::Unhealthy;
                }
            }
            // 恢复中失败：立即回到熔断状态
            HealthState::Recovering => {
                self.state = HealthState::Unhealthy;
            }
            HealthState::Unhealthy => {}
        }
    }

    /// 检查不健康的渠道是否应该尝试恢复（距上次失败已超过恢复间隔）
    pub fn should_attempt_recovery(&self, config: &CircuitBreakerConfig) -> bool {
        if self.state != HealthState::Unhealthy {
            return false;
        }
        if let Some(last_fail) = self.last_failure_time {
            let elapsed = Utc::now() - last_fail;
            elapsed >= chrono::Duration::seconds(config.recovery_interval_sec as i64)
        } else {
            true
        }
    }

    /// 将渠道状态切换为恢复中，重置连续成功计数
    pub fn start_recovery(&mut self) {
        self.state = HealthState::Recovering;
        self.consecutive_successes = 0;
    }

    /// 判断渠道是否可用（健康、降级、恢复中均可用，仅熔断不可用）
    pub fn is_available(&self) -> bool {
        matches!(self.state, HealthState::Healthy | HealthState::Degraded | HealthState::Recovering)
    }

    /// 更新延迟统计，保留最近 100 个采样点计算平均值
    fn update_latency(&mut self, latency_ms: u64) {
        self.latency_samples.push(latency_ms);
        // 保留最近 100 个采样
        if self.latency_samples.len() > 100 {
            self.latency_samples.remove(0);
        }
        self.avg_latency_ms = if self.latency_samples.is_empty() {
            0
        } else {
            self.latency_samples.iter().sum::<u64>() / self.latency_samples.len() as u64
        };
    }
}

/// 渠道健康状态的线程安全映射表，键为渠道 ID
pub type HealthMap = Arc<RwLock<HashMap<String, ChannelHealth>>>;
/// 根据渠道 ID 列表创建初始健康状态映射表
pub fn new_health_map(channel_ids: &[String]) -> HealthMap {
    let mut map = HashMap::new();
    for id in channel_ids {
        map.insert(id.clone(), ChannelHealth::new());
    }
    Arc::new(RwLock::new(map))
}

/// 定期健康检查：将达到恢复间隔的不健康渠道转为恢复中状态
pub fn check_recovery(health_map: &HealthMap, config: &CircuitBreakerConfig) {
    let mut map = health_map.write();
    for health in map.values_mut() {
        if health.should_attempt_recovery(config) {
            health.start_recovery();
        }
    }
}

/// 启动后台定时任务，周期性执行渠道健康恢复检查
pub fn spawn_health_checker(
    health_map: HealthMap,
    config: CircuitBreakerConfig,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        let interval = Duration::from_secs(config.recovery_interval_sec / 2);
        loop {
            tokio::time::sleep(interval).await;
            check_recovery(&health_map, &config);
        }
    })
}
