//! 成本计算模块：按实际模型匹配定价，累加各类 token 的费用。
//!
//! 价格单位统一为「美元 / 百万 token」。成本以美元（USD）为结果单位。

use crate::channel::config::ModelPrice;

/// 计算单次请求成本。
///
/// - `input` / `output` / `cache_read` / `cache_creation` 为各类 token 数（None 视为 0）
/// - `cache_read_per_mtok` / `cache_write_per_mtok` 未配置时回退到 `input_per_mtok`
/// - 无匹配定价时返回 None（表示该模型未配置价格）
pub fn compute_cost(
    price: &ModelPrice,
    input: Option<u64>,
    output: Option<u64>,
    cache_read: Option<u64>,
    cache_creation: Option<u64>,
) -> f64 {
    let input_tokens = input.unwrap_or(0) as f64;
    let output_tokens = output.unwrap_or(0) as f64;
    let cache_read_tokens = cache_read.unwrap_or(0) as f64;
    let cache_creation_tokens = cache_creation.unwrap_or(0) as f64;

    let cache_read_rate = price.cache_read_per_mtok.unwrap_or(price.input_per_mtok);
    let cache_write_rate = price.cache_write_per_mtok.unwrap_or(price.input_per_mtok);

    // 注意：input 已包含缓存读取 token 的语义在不同厂商间存在差异。
    // 保守起见这里按各自字段独立计价，避免重复扣减导致数据失真。
    let cost = input_tokens * price.input_per_mtok
        + output_tokens * price.output_per_mtok
        + cache_read_tokens * cache_read_rate
        + cache_creation_tokens * cache_write_rate;

    cost / 1_000_000.0
}

#[cfg(test)]
mod tests {
    use super::*;

    fn price() -> ModelPrice {
        ModelPrice {
            model: "gpt-4o".to_string(),
            input_per_mtok: 2.5,
            output_per_mtok: 10.0,
            cache_read_per_mtok: Some(1.25),
            cache_write_per_mtok: None,
            enabled: true,
        }
    }

    #[test]
    fn test_compute_cost_full() {
        // 1M input + 1M output + 1M cache_read + 1M cache_write
        // = 2.5 + 10 + 1.25 + 2.5 = 16.25 USD
        let cost = compute_cost(&price(), Some(1_000_000), Some(1_000_000), Some(1_000_000), Some(1_000_000));
        assert!((cost - 16.25).abs() < 1e-9, "cost = {}", cost);
    }

    #[test]
    fn test_compute_cost_cache_fallback() {
        // cache_write 未配置，回退到 input 价 2.5
        let cost = compute_cost(&price(), Some(0), Some(0), Some(0), Some(1_000_000));
        assert!((cost - 2.5).abs() < 1e-9, "cost = {}", cost);
    }

    #[test]
    fn test_compute_cost_zero_tokens() {
        let cost = compute_cost(&price(), None, None, None, None);
        assert_eq!(cost, 0.0);
    }

    #[test]
    fn test_compute_cost_partial() {
        // 500K input + 250K output = 0.5*2.5 + 0.25*10 = 1.25 + 2.5 = 3.75
        let cost = compute_cost(&price(), Some(500_000), Some(250_000), None, None);
        assert!((cost - 3.75).abs() < 1e-9, "cost = {}", cost);
    }
}
