/** 格式化耗时：超过 1 秒显示 X.Xs，否则显示 Xms */
export function formatLatency(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return "-";
  if (ms >= 1000) {
    return `${(ms / 1000).toFixed(1)}s`;
  }
  return `${ms}ms`;
}

/**
 * 格式化 token 数量：达到上一单位的 10 倍时升级单位
 * - < 10,000 → 原样显示（如 9999）
 * - >= 10,000 → K（如 10.0K、999.9K）
 * - >= 10,000,000 → M（如 10.0M、999.9M）
 * - >= 10,000,000,000 → B（如 10.0B）
 */
export function formatTokens(n: number | null | undefined): string {
  if (n === null || n === undefined) return "0";
  if (n < 10_000) return n.toLocaleString();
  if (n < 10_000_000) return `${(n / 1_000).toFixed(2)}K`;
  if (n < 10_000_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
  return `${(n / 1_000_000_000).toFixed(2)}B`;
}
