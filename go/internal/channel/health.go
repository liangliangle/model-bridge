// Package channel 提供渠道健康状态机（熔断器）与故障转移路由选择。
//
// 对应 Rust 侧 src-tauri/src/channel/health.rs 与 src-tauri/src/channel/failover.rs。
// 状态迁移阈值、计数器语义、可用性判定必须与 Rust 完全一致。
package channel

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"modelbridge/internal/config"
)

// HealthState 渠道健康状态（health.rs:12-18 的 HealthState）。
//
// 序列化名与 Rust serde(rename_all = "snake_case") 一致：
// healthy / degraded / unhealthy / recovering。
type HealthState int

const (
	// Healthy 健康，可正常使用。
	Healthy HealthState = iota
	// Degraded 降级，出现部分失败但未触发熔断。
	Degraded
	// Unhealthy 不健康，已被熔断。
	Unhealthy
	// Recovering 恢复中，正在进行探测请求。
	Recovering
)

// String 返回 snake_case 状态名，与 Rust 侧 `format!("{:?}", state).to_lowercase()`
// （commands.rs:311 用于 /api/channels/health）保持一致。
func (s HealthState) String() string {
	switch s {
	case Healthy:
		return "healthy"
	case Degraded:
		return "degraded"
	case Unhealthy:
		return "unhealthy"
	case Recovering:
		return "recovering"
	default:
		return "healthy"
	}
}

// MarshalJSON 以 snake_case 字符串序列化（与 Rust serde 行为一致）。
func (s HealthState) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON 接受 snake_case 字符串。
func (s *HealthState) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	switch name {
	case "degraded":
		*s = Degraded
	case "unhealthy":
		*s = Unhealthy
	case "recovering":
		*s = Recovering
	default:
		*s = Healthy
	}
	return nil
}

// latencySampleLimit 延迟采样保留的最近样本数（health.rs:129 `> 100`）。
const latencySampleLimit = 100

// ChannelHealth 单个渠道的健康状态信息，跟踪请求成功率和延迟
// （health.rs:23-32 的 ChannelHealth）。
type ChannelHealth struct {
	State                HealthState `json:"state"`
	ConsecutiveFailures  uint32      `json:"consecutive_failures"`
	ConsecutiveSuccesses uint32      `json:"consecutive_successes"`
	TotalRequests        uint64      `json:"total_requests"`
	TotalFailures        uint64      `json:"total_failures"`
	AvgLatencyMs         uint64      `json:"avg_latency_ms"`
	LastFailureTime      *time.Time  `json:"last_failure_time"`
	LastSuccessTime      *time.Time  `json:"last_success_time"`

	// latencySamples 延迟采样（保留最近 100 条）；Rust 侧为私有字段，不参与序列化。
	latencySamples []uint64
}

// NewChannelHealth 创建初始状态为健康的渠道健康实例（health.rs:38-48）。
func NewChannelHealth() *ChannelHealth {
	return &ChannelHealth{
		State:          Healthy,
		latencySamples: nil,
	}
}

// LatencySamples 返回延迟采样的副本（顺序为从旧到新）。
func (h *ChannelHealth) LatencySamples() []uint64 {
	out := make([]uint64, len(h.latencySamples))
	copy(out, h.latencySamples)
	return out
}

// RecordSuccess 记录一次成功请求，更新延迟和状态转换（health.rs:51-70）。
func (h *ChannelHealth) RecordSuccess(latencyMs uint64, cfg config.CircuitBreakerConfig) {
	h.TotalRequests++
	h.ConsecutiveFailures = 0
	h.ConsecutiveSuccesses++
	now := time.Now().UTC()
	h.LastSuccessTime = &now
	h.updateLatency(latencyMs)
	switch h.State {
	case Recovering:
		// 恢复中：连续成功次数达到探测阈值后转为健康
		if h.ConsecutiveSuccesses >= cfg.ProbeRequests {
			h.State = Healthy
		}
	case Degraded:
		// 降级：一次成功即恢复为健康
		h.State = Healthy
	}
}

// RecordFailure 记录一次失败请求，根据连续失败次数触发状态降级或熔断（health.rs:72-100）。
func (h *ChannelHealth) RecordFailure(cfg config.CircuitBreakerConfig) {
	h.TotalRequests++
	h.TotalFailures++
	h.ConsecutiveFailures++
	h.ConsecutiveSuccesses = 0
	now := time.Now().UTC()
	h.LastFailureTime = &now
	switch h.State {
	case Healthy:
		// 健康：达到阈值一半时降级，达到阈值时熔断
		half := cfg.FailureThreshold / 2
		if half < 1 {
			half = 1 // Rust: `(config.failure_threshold / 2).max(1)`
		}
		if h.ConsecutiveFailures >= cfg.FailureThreshold {
			h.State = Unhealthy
		} else if h.ConsecutiveFailures >= half {
			h.State = Degraded
		}
	case Degraded:
		// 降级：继续失败达到阈值则熔断
		if h.ConsecutiveFailures >= cfg.FailureThreshold {
			h.State = Unhealthy
		}
	case Recovering:
		// 恢复中失败：立即回到熔断状态
		h.State = Unhealthy
	case Unhealthy:
		// 保持熔断
	}
}

// ShouldAttemptRecovery 检查不健康的渠道是否应该尝试恢复（health.rs:102-114）。
// 仅当状态为 Unhealthy 且距上次失败已超过恢复间隔时返回 true；无失败记录时返回 true。
func (h *ChannelHealth) ShouldAttemptRecovery(cfg config.CircuitBreakerConfig) bool {
	if h.State != Unhealthy {
		return false
	}
	if h.LastFailureTime != nil {
		elapsed := time.Since(*h.LastFailureTime)
		return elapsed >= time.Duration(cfg.RecoveryIntervalSec)*time.Second
	}
	return true
}

// StartRecovery 将渠道状态切换为恢复中，重置连续成功计数（health.rs:116-120）。
func (h *ChannelHealth) StartRecovery() {
	h.State = Recovering
	h.ConsecutiveSuccesses = 0
}

// IsAvailable 判断渠道是否可用（健康、降级、恢复中均可用，仅熔断不可用）（health.rs:122-124）。
func (h *ChannelHealth) IsAvailable() bool {
	return h.State == Healthy || h.State == Degraded || h.State == Recovering
}

// updateLatency 更新延迟统计，保留最近 100 个采样点计算平均值（health.rs:126-138）。
func (h *ChannelHealth) updateLatency(latencyMs uint64) {
	h.latencySamples = append(h.latencySamples, latencyMs)
	// 保留最近 100 个采样
	if len(h.latencySamples) > latencySampleLimit {
		h.latencySamples = h.latencySamples[1:]
	}
	if len(h.latencySamples) == 0 {
		h.AvgLatencyMs = 0
		return
	}
	var sum uint64
	for _, s := range h.latencySamples {
		sum += s
	}
	h.AvgLatencyMs = sum / uint64(len(h.latencySamples))
}

// HealthMap 渠道健康状态的线程安全映射表，键为渠道 ID
// （health.rs:141 `Arc<RwLock<HashMap<String, ChannelHealth>>>`）。
//
// 与 Rust 的差别：Rust 通过 guard 暴露 `&mut ChannelHealth`，Go 无法在锁外安全地返回
// 内部指针，因此读路径返回副本（Get），写路径通过带锁的方法（RecordSuccess/RecordFailure/Set）。
type HealthMap struct {
	mu sync.RWMutex
	m  map[string]*ChannelHealth
}

// NewHealthMap 根据渠道 ID 列表创建初始健康状态映射表（health.rs:143-150）。
func NewHealthMap(channelIDs []string) *HealthMap {
	hm := &HealthMap{m: make(map[string]*ChannelHealth, len(channelIDs))}
	for _, id := range channelIDs {
		hm.m[id] = NewChannelHealth()
	}
	return hm
}

// Get 返回渠道健康状态的副本；不存在时 ok 为 false。
func (hm *HealthMap) Get(id string) (ChannelHealth, bool) {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	h, ok := hm.m[id]
	if !ok || h == nil {
		return ChannelHealth{}, false
	}
	return *h, true
}

// Snapshot 返回全部渠道健康状态的副本。
func (hm *HealthMap) Snapshot() map[string]ChannelHealth {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	out := make(map[string]ChannelHealth, len(hm.m))
	for id, h := range hm.m {
		if h != nil {
			out[id] = *h
		}
	}
	return out
}

// Len 返回映射表中的渠道数量。
func (hm *HealthMap) Len() int {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return len(hm.m)
}

// Set 写入/覆盖某渠道的健康状态（对应 Rust 的 `HashMap::insert`）。
func (hm *HealthMap) Set(id string, h ChannelHealth) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	cp := h
	cp.latencySamples = append([]uint64(nil), h.latencySamples...)
	if cp.latencySamples == nil {
		cp.latencySamples = []uint64{}
	}
	hm.m[id] = &cp
}

// Ensure 确保渠道存在健康记录并返回其状态副本（对应 Rust `entry(..).or_insert_with(new)`）。
func (hm *HealthMap) Ensure(id string) ChannelHealth {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	h, ok := hm.m[id]
	if !ok || h == nil {
		h = NewChannelHealth()
		hm.m[id] = h
	}
	return *h
}

// Remove 删除某渠道的健康记录（渠道被删除时调用）。
func (hm *HealthMap) Remove(id string) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	delete(hm.m, id)
}

// IsAvailable 判断渠道是否可用；映射表中不存在该渠道时视为可用
// （failover.rs:62-63 `unwrap_or(true)`）。
func (hm *HealthMap) IsAvailable(id string) bool {
	h, ok := hm.Get(id)
	if !ok {
		return true
	}
	return h.IsAvailable()
}

// RecordSuccess 记录一次成功；渠道不在映射表中时不做任何事
// （router.rs:107-110 `if let Some(h) = health.get_mut(..)`）。
func (hm *HealthMap) RecordSuccess(id string, latencyMs uint64, cfg config.CircuitBreakerConfig) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	if h, ok := hm.m[id]; ok && h != nil {
		h.RecordSuccess(latencyMs, cfg)
	}
}

// RecordFailure 记录一次失败；渠道不在映射表中时不做任何事（router.rs:121-124）。
func (hm *HealthMap) RecordFailure(id string, cfg config.CircuitBreakerConfig) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	if h, ok := hm.m[id]; ok && h != nil {
		h.RecordFailure(cfg)
	}
}

// CheckRecovery 定期健康检查：将达到恢复间隔的不健康渠道转为恢复中状态（health.rs:153-159）。
func CheckRecovery(hm *HealthMap, cfg config.CircuitBreakerConfig) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	for _, h := range hm.m {
		if h == nil {
			continue
		}
		if h.ShouldAttemptRecovery(cfg) {
			h.StartRecovery()
		}
	}
}

// healthCheckInterval 计算后台检查间隔：Rust 为 `recovery_interval_sec / 2`
// （health.rs:167-169）。Rust 下取 0 会退化成 `sleep(0)` 忙轮询；Go 的 time.Ticker
// 不接受非正数，这里退化为 1s，保持「可取消且不空转」。
func healthCheckInterval(cfg config.CircuitBreakerConfig) time.Duration {
	d := time.Duration(cfg.RecoveryIntervalSec/2) * time.Second
	if d <= 0 {
		d = time.Second
	}
	return d
}

// RunHealthChecker 阻塞运行健康检查循环，直到 ctx 被取消
// （对应 health.rs:163-174 的 spawn_health_checker，去掉 tokio::spawn）。
// 返回时保证不遗留 goroutine。
func RunHealthChecker(ctx context.Context, hm *HealthMap, cfg config.CircuitBreakerConfig) {
	ticker := time.NewTicker(healthCheckInterval(cfg))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			CheckRecovery(hm, cfg)
		}
	}
}

// StartHealthChecker 启动后台健康检查 goroutine；ctx 取消后 goroutine 退出。
// 返回的 stop 函数与 ctx 取消等价，调用方至少要用其中一个（通常在进程退出时 cancel ctx）。
func StartHealthChecker(ctx context.Context, hm *HealthMap, cfg config.CircuitBreakerConfig) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunHealthChecker(ctx, hm, cfg)
	}()
	return func() {
		cancel()
		<-done // 等待 goroutine 真正退出，避免调用方观察到泄漏
	}
}
