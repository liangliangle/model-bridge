import Foundation

// MARK: - 格式化工具方法 (与 Web 端保持完全一致)

public enum FormatUtils {
    
    /// 格式化耗时：超过 1 秒显示 X.Xs，否则显示 Xms
    public static func formatLatency(_ ms: Int?) -> String {
        guard let ms = ms else { return "-" }
        if ms >= 1000 {
            return String(format: "%.1fs", Double(ms) / 1000.0)
        }
        return "\(ms)ms"
    }

    /// 格式化 Token 数量：
    /// - < 10,000 → 原样千分位（如 9,999）
    /// - >= 10,000 且 < 10,000,000 → K（如 12.50K、542.10K）
    /// - >= 10,000,000 且 < 10,000,000,000 → M（如 1.28M、12.50M）
    /// - >= 10,000,000,000 → B（如 1.05B）
    public static func formatTokens(_ n: Int?, compact: Bool = false) -> String {
        guard let n = n, n > 0 else { return "0" }
        
        let d = Double(n)
        if compact {
            // 紧凑模式（适用于菜单栏标题），如 1.28M, 542K
            if n >= 1_000_000_000 {
                return String(format: "%.2fB", d / 1_000_000_000.0)
            } else if n >= 1_000_000 {
                return String(format: "%.2fM", d / 1_000_000.0)
            } else if n >= 10_000 {
                return String(format: "%.1fK", d / 1_000.0)
            } else {
                return "\(n)"
            }
        }
        
        // 标准模式
        if n < 10_000 {
            let formatter = NumberFormatter()
            formatter.numberStyle = .decimal
            return formatter.string(from: NSNumber(value: n)) ?? "\(n)"
        } else if n < 10_000_000 {
            let val = d / 1_000.0
            return String(format: "%.1fK", val)
        } else if n < 10_000_000_000 {
            let val = d / 1_000_000.0
            return String(format: "%.2fM", val)
        } else {
            let val = d / 1_000_000_000.0
            return String(format: "%.2fB", val)
        }
    }

    /// 数字千分位格式化（如 1,284,520）
    public static func formatNumberWithComma(_ n: Int?) -> String {
        guard let n = n else { return "0" }
        let formatter = NumberFormatter()
        formatter.numberStyle = .decimal
        return formatter.string(from: NSNumber(value: n)) ?? "\(n)"
    }
}
