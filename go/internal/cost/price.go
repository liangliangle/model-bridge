// Package cost 计算单次请求的成本。
//
// 对应 Rust 侧 src-tauri/src/cost/mod.rs。价格单位统一为「美元 / 百万 token」，
// 结果单位同样为美元（USD），与 audit_log.cost_usd 列一致（audit/db.rs:459）。
//
// 字段名与 config.ModelPrice 完全对齐（config.go:180-187）：
// input_per_mtok / output_per_mtok / cache_read_per_mtok / cache_write_per_mtok。
package cost

import "modelbridge/internal/config"

// Usage 一次请求的各类 token 数；nil 视为 0
// （cost/mod.rs:12-17 的四个 `Option<u64>` 参数）。
//
// 与 audit.TokenUsage 同形，可直接互转。
type Usage struct {
	Input         *int64 // 输入 token（prompt_tokens / input_tokens）
	Output        *int64 // 输出 token（completion_tokens / output_tokens）
	CacheRead     *int64 // 缓存读取 token
	CacheCreation *int64 // 缓存创建（写入）token
}

// Breakdown 成本明细：既含各项 token 数与分项费用，也含审计库落库的总价。
//
// 审计表 audit_log 存的列是 input_tokens / output_tokens / cache_read_tokens /
// cache_creation_tokens / cost_usd（audit/db.rs:453-464），本结构一一对应：
// 前四项直接落库，CostUSD 即 cost_usd。
type Breakdown struct {
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	InputCostUSD        float64 `json:"input_cost_usd"`
	OutputCostUSD       float64 `json:"output_cost_usd"`
	CacheReadCostUSD    float64 `json:"cache_read_cost_usd"`
	CacheWriteCostUSD   float64 `json:"cache_write_cost_usd"`
	// CostUSD 总成本，落库到 audit_log.cost_usd。
	CostUSD float64 `json:"cost_usd"`
}

// ComputeCost 计算单次请求成本（cost/mod.rs:12-34 的 compute_cost）。
//
//   - input / output / cacheRead / cacheCreation 为各类 token 数（nil 视为 0）
//   - cache_read_per_mtok / cache_write_per_mtok 未配置时回退到 input_per_mtok
//
// 注意：input 是否已包含缓存读取 token 在不同厂商间存在差异；这里按各自字段独立
// 计价（与 Rust 一致），避免重复扣减导致数据失真。
func ComputeCost(price *config.ModelPrice, input, output, cacheRead, cacheCreation *int64) float64 {
	return Compute(price, Usage{
		Input:         input,
		Output:        output,
		CacheRead:     cacheRead,
		CacheCreation: cacheCreation,
	}).CostUSD
}

// Compute 计算成本并返回明细。
func Compute(price *config.ModelPrice, u Usage) Breakdown {
	b := Breakdown{
		InputTokens:         deref(u.Input),
		OutputTokens:        deref(u.Output),
		CacheReadTokens:     deref(u.CacheRead),
		CacheCreationTokens: deref(u.CacheCreation),
	}
	if price == nil {
		return b
	}

	cacheReadRate := price.InputPerMtok
	if price.CacheReadPerMtok != nil {
		cacheReadRate = *price.CacheReadPerMtok
	}
	cacheWriteRate := price.InputPerMtok
	if price.CacheWritePerMtok != nil {
		cacheWriteRate = *price.CacheWritePerMtok
	}

	b.InputCostUSD = float64(b.InputTokens) * price.InputPerMtok / 1_000_000.0
	b.OutputCostUSD = float64(b.OutputTokens) * price.OutputPerMtok / 1_000_000.0
	b.CacheReadCostUSD = float64(b.CacheReadTokens) * cacheReadRate / 1_000_000.0
	b.CacheWriteCostUSD = float64(b.CacheCreationTokens) * cacheWriteRate / 1_000_000.0
	b.CostUSD = b.InputCostUSD + b.OutputCostUSD + b.CacheReadCostUSD + b.CacheWriteCostUSD
	return b
}

// ForModel 按实际模型名从配置中查找定价并计算成本，返回 (明细, 是否配置了定价)。
// 未配置定价时 ok 为 false，调用方应落库 nil（对应 executor.rs:658-662 的
// `compute_cost_from_config_ref` 返回 None）。
func ForModel(cfg *config.AppConfig, actualModel string, u Usage) (Breakdown, bool) {
	if cfg == nil {
		return Breakdown{}, false
	}
	price := cfg.FindModelPrice(actualModel)
	if price == nil {
		return Breakdown{}, false
	}
	return Compute(price, u), true
}

// Total 返回落库用的总成本指针（未配置定价时返回 nil，Rust 侧即为 None）。
func ForModelTotal(cfg *config.AppConfig, actualModel string, u Usage) *float64 {
	b, ok := ForModel(cfg, actualModel, u)
	if !ok {
		return nil
	}
	total := b.CostUSD
	return &total
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
