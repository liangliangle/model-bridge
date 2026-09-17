package cost

import (
	"math"
	"testing"

	"modelbridge/internal/config"
)

// 与 cost/mod.rs:39-50 的 test price 一致。
func testPrice() *config.ModelPrice {
	read := 1.25
	return &config.ModelPrice{
		Model:            "gpt-4o",
		InputPerMtok:     2.5,
		OutputPerMtok:    10.0,
		CacheReadPerMtok: &read,
		Enabled:          true,
	}
}

func i64(v int64) *int64 { return &v }

// cost/mod.rs:52-58
func TestComputeCostFull(t *testing.T) {
	// 1M input + 1M output + 1M cache_read + 1M cache_write(cache_write 未配置→回退 input)
	// = 2.5 + 10 + 1.25 + 2.5 = 16.25 USD
	got := ComputeCost(testPrice(), i64(1_000_000), i64(1_000_000), i64(1_000_000), i64(1_000_000))
	if math.Abs(got-16.25) > 1e-9 {
		t.Fatalf("cost = %v, want 16.25", got)
	}
}

// cost/mod.rs:60-65
func TestComputeCostCacheFallback(t *testing.T) {
	got := ComputeCost(testPrice(), i64(0), i64(0), i64(0), i64(1_000_000))
	if math.Abs(got-2.5) > 1e-9 {
		t.Fatalf("cost = %v, want 2.5", got)
	}
}

// cost/mod.rs:67-71
func TestComputeCostZeroTokens(t *testing.T) {
	if got := ComputeCost(testPrice(), nil, nil, nil, nil); got != 0.0 {
		t.Fatalf("cost = %v, want 0", got)
	}
}

// cost/mod.rs:73-77
func TestComputeCostPartial(t *testing.T) {
	got := ComputeCost(testPrice(), i64(500_000), i64(250_000), nil, nil)
	if math.Abs(got-3.75) > 1e-9 {
		t.Fatalf("cost = %v, want 3.75", got)
	}
}

// 审计库落库字段：各 token 数与总价一一对应。
func TestBreakdownMatchesAuditColumns(t *testing.T) {
	b := Compute(testPrice(), Usage{Input: i64(1_000_000), Output: i64(0), CacheRead: i64(0), CacheCreation: i64(0)})
	if b.InputTokens != 1_000_000 || b.OutputTokens != 0 {
		t.Fatalf("tokens = (%d,%d), want (1000000,0)", b.InputTokens, b.OutputTokens)
	}
	if math.Abs(b.CostUSD-2.5) > 1e-9 || math.Abs(b.InputCostUSD-2.5) > 1e-9 {
		t.Fatalf("cost = %v / input = %v, want 2.5", b.CostUSD, b.InputCostUSD)
	}
}

// executor.rs:658-662：未配置定价 → 返回 nil（落库 NULL）。
func TestForModelTotalWithoutPrice(t *testing.T) {
	cfg := config.DefaultConfig()
	if got := ForModelTotal(cfg, "unknown-model", Usage{Input: i64(10)}); got != nil {
		t.Fatalf("total = %v, want nil", *got)
	}
	cfg.ModelPrices = []config.ModelPrice{*testPrice()}
	got := ForModelTotal(cfg, "gpt-4o", Usage{Input: i64(1_000_000)})
	if got == nil || math.Abs(*got-2.5) > 1e-9 {
		t.Fatalf("total = %v, want 2.5", got)
	}
}

// 未启用（enabled=false）的定价不参与匹配（config.rs:401-403 / config.go FindModelPrice）。
func TestDisabledPriceIsIgnored(t *testing.T) {
	p := testPrice()
	p.Enabled = false
	cfg := config.DefaultConfig()
	cfg.ModelPrices = []config.ModelPrice{*p}
	if got := ForModelTotal(cfg, "gpt-4o", Usage{Input: i64(1_000_000)}); got != nil {
		t.Fatalf("total = %v, want nil for disabled price", *got)
	}
}
