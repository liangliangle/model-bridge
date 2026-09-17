package channel

import (
	"context"
	"runtime"
	"testing"
	"time"

	"modelbridge/internal/config"
)

func cbConfig(threshold uint32, recoverySec uint64, probes uint32) config.CircuitBreakerConfig {
	return config.CircuitBreakerConfig{
		FailureThreshold:    threshold,
		RecoveryIntervalSec: recoverySec,
		ProbeRequests:       probes,
	}
}

func TestNewChannelHealthIsHealthy(t *testing.T) {
	h := NewChannelHealth()
	if h.State != Healthy {
		t.Fatalf("initial state = %v, want Healthy", h.State)
	}
	if !h.IsAvailable() {
		t.Fatal("new channel should be available")
	}
	if h.State.String() != "healthy" {
		t.Fatalf("state string = %q, want %q", h.State.String(), "healthy")
	}
}

// health.rs:80-93：Healthy 分支在 consecutive_failures >= threshold/2 (至少 1) 时降级，
// 达到 threshold 时熔断。
func TestRecordFailureThresholds(t *testing.T) {
	cfg := cbConfig(4, 30, 2)
	h := NewChannelHealth()

	h.RecordFailure(cfg)
	if h.State != Healthy {
		t.Fatalf("after 1/4 failures state = %v, want Healthy", h.State)
	}
	if h.ConsecutiveFailures != 1 || h.TotalFailures != 1 || h.TotalRequests != 1 {
		t.Fatalf("counters = (%d,%d,%d), want (1,1,1)", h.ConsecutiveFailures, h.TotalFailures, h.TotalRequests)
	}

	h.RecordFailure(cfg)
	if h.State != Degraded {
		t.Fatalf("after 2/4 failures state = %v, want Degraded", h.State)
	}
	if !h.IsAvailable() {
		t.Fatal("Degraded must still be available")
	}

	h.RecordFailure(cfg)
	if h.State != Degraded {
		t.Fatalf("after 3/4 failures state = %v, want Degraded", h.State)
	}

	h.RecordFailure(cfg)
	if h.State != Unhealthy {
		t.Fatalf("after 4/4 failures state = %v, want Unhealthy", h.State)
	}
	if h.IsAvailable() {
		t.Fatal("Unhealthy must not be available")
	}
	if h.LastFailureTime == nil {
		t.Fatal("last failure time should be recorded")
	}
}

// threshold=3 → threshold/2 = 1 → 第一次失败即降级（Rust `.max(1)`）。
func TestFailHalfThresholdUsesMaxOne(t *testing.T) {
	cfg := cbConfig(3, 30, 2)
	h := NewChannelHealth()
	h.RecordFailure(cfg)
	if h.State != Degraded {
		t.Fatalf("state = %v, want Degraded (threshold/2 max 1)", h.State)
	}
	h.RecordFailure(cfg)
	h.RecordFailure(cfg)
	if h.State != Unhealthy {
		t.Fatalf("state = %v, want Unhealthy", h.State)
	}
}

// health.rs:64-67：Degraded 下一次成功即恢复健康，并清零连续失败。
func TestDegradedSuccessRestoresHealthy(t *testing.T) {
	cfg := cbConfig(4, 30, 2)
	h := NewChannelHealth()
	h.RecordFailure(cfg)
	h.RecordFailure(cfg)
	if h.State != Degraded {
		t.Fatalf("precondition state = %v, want Degraded", h.State)
	}
	h.RecordSuccess(120, cfg)
	if h.State != Healthy {
		t.Fatalf("state = %v, want Healthy", h.State)
	}
	if h.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d, want 0", h.ConsecutiveFailures)
	}
	if h.LastSuccessTime == nil {
		t.Fatal("last success time should be recorded")
	}
}

// health.rs:55-63：Recovering 需要连续 probe_requests 次成功才转 Healthy。
func TestRecoveringRequiresProbeRequests(t *testing.T) {
	cfg := cbConfig(4, 30, 3)
	h := NewChannelHealth()
	h.State = Unhealthy
	h.StartRecovery()
	if h.State != Recovering {
		t.Fatalf("state = %v, want Recovering", h.State)
	}
	if !h.IsAvailable() {
		t.Fatal("Recovering must be available")
	}

	h.RecordSuccess(10, cfg)
	if h.State != Recovering {
		t.Fatalf("after 1/3 probes state = %v, want Recovering", h.State)
	}
	h.RecordSuccess(10, cfg)
	if h.State != Recovering {
		t.Fatalf("after 2/3 probes state = %v, want Recovering", h.State)
	}
	h.RecordSuccess(10, cfg)
	if h.State != Healthy {
		t.Fatalf("after 3/3 probes state = %v, want Healthy", h.State)
	}
}

// health.rs:94-96：Recovering 时失败立即回到 Unhealthy。
func TestRecoveringFailureReturnsToUnhealthy(t *testing.T) {
	cfg := cbConfig(4, 30, 2)
	h := NewChannelHealth()
	h.StartRecovery()
	h.RecordFailure(cfg)
	if h.State != Unhealthy {
		t.Fatalf("state = %v, want Unhealthy", h.State)
	}
}

// health.rs:102-114 + 116-120。
func TestShouldAttemptRecoveryAndStartRecovery(t *testing.T) {
	cfg := cbConfig(3, 30, 2)
	h := NewChannelHealth()
	// 非 Unhealthy 状态一律不尝试恢复
	if h.ShouldAttemptRecovery(cfg) {
		t.Fatal("Healthy channel should not attempt recovery")
	}

	// 无失败时间记录（人工构造）→ true
	h.State = Unhealthy
	if !h.ShouldAttemptRecovery(cfg) {
		t.Fatal("Unhealthy without last failure time should attempt recovery")
	}

	// 刚失败过 → 未到恢复间隔
	h.RecordFailure(cfg)
	if h.State != Unhealthy {
		t.Fatalf("state = %v, want Unhealthy", h.State)
	}
	if h.ShouldAttemptRecovery(cfg) {
		t.Fatal("should not attempt recovery before interval elapsed")
	}

	old := time.Now().UTC().Add(-time.Duration(cfg.RecoveryIntervalSec+1) * time.Second)
	h.LastFailureTime = &old
	if !h.ShouldAttemptRecovery(cfg) {
		t.Fatal("should attempt recovery after interval elapsed")
	}

	h.StartRecovery()
	if h.State != Recovering || h.ConsecutiveSuccesses != 0 {
		t.Fatalf("start recovery = (%v,%d), want (Recovering,0)", h.State, h.ConsecutiveSuccesses)
	}
}

// health.rs:126-138：延迟采样仅保留最近 100 条。
func TestUpdateLatencyKeepsLast100Samples(t *testing.T) {
	cfg := cbConfig(4, 30, 2)
	h := NewChannelHealth()
	for i := 1; i <= 150; i++ {
		h.RecordSuccess(uint64(i), cfg)
	}
	samples := h.LatencySamples()
	if len(samples) != 100 {
		t.Fatalf("sample count = %d, want 100", len(samples))
	}
	if samples[0] != 51 || samples[99] != 150 {
		t.Fatalf("samples = [%d..%d], want [51..150]", samples[0], samples[99])
	}
	// (51+150)*100/2/100 = 100
	if h.AvgLatencyMs != 100 {
		t.Fatalf("avg latency = %d, want 100", h.AvgLatencyMs)
	}
}

// failover.rs:62-63：映射表中不存在的渠道视为可用；存在但熔断则不可用。
func TestHealthMapAvailability(t *testing.T) {
	hm := NewHealthMap(nil)
	if !hm.IsAvailable("unknown") {
		t.Fatal("missing channel should be treated as available")
	}

	down := NewChannelHealth()
	down.State = Unhealthy
	hm.Set("down", *down)
	if hm.IsAvailable("down") {
		t.Fatal("unhealthy channel should not be available")
	}

	hm.Set("up", *NewChannelHealth())
	if !hm.IsAvailable("up") {
		t.Fatal("healthy channel should be available")
	}
	if hm.Len() != 2 {
		t.Fatalf("len = %d, want 2", hm.Len())
	}
	hm.Remove("down")
	if hm.Len() != 1 {
		t.Fatalf("len after remove = %d, want 1", hm.Len())
	}
}

// router.rs:107-124：只对已存在的渠道记录成功/失败。
func TestHealthMapRecordOnlyKnownChannels(t *testing.T) {
	cfg := cbConfig(3, 30, 2)
	hm := NewHealthMap([]string{"a"})
	hm.RecordFailure("b", cfg)
	if _, ok := hm.Get("b"); ok {
		t.Fatal("unknown channel should not be created by RecordFailure")
	}
	hm.RecordFailure("a", cfg)
	got, _ := hm.Get("a")
	if got.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", got.ConsecutiveFailures)
	}
	hm.RecordSuccess("a", 5, cfg)
	got, _ = hm.Get("a")
	if got.ConsecutiveSuccesses != 1 {
		t.Fatalf("consecutive successes = %d, want 1", got.ConsecutiveSuccesses)
	}
}

// health.rs:153-159：CheckRecovery 把到期的 Unhealthy 渠道转为 Recovering。
func TestCheckRecovery(t *testing.T) {
	cfg := cbConfig(3, 30, 2)
	hm := NewHealthMap([]string{"a", "b"})

	fresh := NewChannelHealth()
	fresh.State = Unhealthy
	old := time.Now().UTC().Add(-time.Minute)
	fresh.LastFailureTime = &old
	hm.Set("a", *fresh)

	recent := NewChannelHealth()
	recent.State = Unhealthy
	now := time.Now().UTC()
	recent.LastFailureTime = &now
	hm.Set("b", *recent)

	CheckRecovery(hm, cfg)

	a, _ := hm.Get("a")
	b, _ := hm.Get("b")
	if a.State != Recovering {
		t.Fatalf("a state = %v, want Recovering", a.State)
	}
	if b.State != Unhealthy {
		t.Fatalf("b state = %v, want Unhealthy", b.State)
	}
}

func TestHealthCheckInterval(t *testing.T) {
	if got := healthCheckInterval(cbConfig(3, 30, 2)); got != 15*time.Second {
		t.Fatalf("interval = %v, want 15s", got)
	}
	// Rust 下 0 会变成 sleep(0)；Go 退化为 1s
	if got := healthCheckInterval(cbConfig(3, 0, 2)); got != time.Second {
		t.Fatalf("interval = %v, want 1s fallback", got)
	}
}

// 后台 ticker 必须在 ctx 取消后停止，且不遗留 goroutine。
func TestHealthCheckerStopsOnContextCancel(t *testing.T) {
	before := runtime.NumGoroutine()
	hm := NewHealthMap([]string{"a"})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		RunHealthChecker(ctx, hm, cbConfig(3, 30, 2))
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunHealthChecker did not stop after context cancellation")
	}
	// 等待调度器回收
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Fatalf("goroutines leaked: before=%d after=%d", before, got)
	}
}

// StartHealthChecker 返回的 stop 函数幂等且立即返回（goroutine 已退出）。
func TestStartHealthCheckerStopIsClean(t *testing.T) {
	before := runtime.NumGoroutine()
	hm := NewHealthMap([]string{"a"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := StartHealthChecker(ctx, hm, cbConfig(3, 30, 2))
	stop()
	stop()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Fatalf("goroutines leaked: before=%d after=%d", before, got)
	}
}

// 后台 ticker 确实会执行恢复检查（recovery_interval_sec=2 → 间隔 1s）。
func TestHealthCheckerPerformsRecovery(t *testing.T) {
	hm := NewHealthMap([]string{"a"})
	down := NewChannelHealth()
	down.State = Unhealthy
	old := time.Now().UTC().Add(-time.Minute)
	down.LastFailureTime = &old
	hm.Set("a", *down)

	ctx, cancel := context.WithCancel(context.Background())
	stop := StartHealthChecker(ctx, hm, cbConfig(3, 2, 2))
	defer func() {
		cancel()
		stop()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := hm.Get("a"); got.State == Recovering {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background health checker never started recovery")
}
