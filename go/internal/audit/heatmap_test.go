package audit

import (
	"path/filepath"
	"testing"
	"time"
)

// 热力图窗口的回归测试。
//
// 背景（真实缺陷）：GetTokenHeatmap 曾用 periodCutoffMs(fmt.Sprintf("%dd", days)) 算窗口下界，
// 而 periodCutoffMs 只认 /api/stats 的 "7d"/"30d"，其它取值落进默认分支 → cutoff 变成
// **今天零点**。后果是 days=365（前端 Dashboard 唯一使用的值）返回空数组，默认值 91 同样是空的，
// 只有 7 和 30 碰巧正确——而它们在「库里只有一天数据」时也"看起来对"，所以极易漏掉。
//
// 这里用一个跨多天的 fixture 把窗口边界钉死：days 越大覆盖的日期必须单调不减，
// 且 365 必须能取到 100 天前的记录。

// insertAuditRow 直接插一行审计记录（时间戳可控，绕过 Create 的「当前时间」）。
func insertAuditRow(t *testing.T, db *DB, ts time.Time, inputTokens, outputTokens int) {
	t.Helper()
	_, err := db.conn.Exec(
		`INSERT INTO audit_log (timestamp, method, path, alias_model, input_tokens, output_tokens)
		 VALUES (?, 'POST', '/v1/chat/completions', 'gpt-test', ?, ?)`,
		ts.UnixMilli(), inputTokens, outputTokens)
	if err != nil {
		t.Fatalf("插入审计记录失败: %v", err)
	}
}

// dayAtNoon 返回本地时区「days 天前」的中午（避开零点边界，避免测试在零点附近抖动）。
func dayAtNoon(days int) time.Time {
	return localMidnightDaysAgo(days).Add(12 * time.Hour)
}

func newHeatmapDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("打开审计库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 今天两行（验证按天求和）、3 天前一行、100 天前一行、400 天前一行。
	insertAuditRow(t, db, dayAtNoon(0), 10, 1)
	insertAuditRow(t, db, dayAtNoon(0), 20, 2)
	insertAuditRow(t, db, dayAtNoon(3), 100, 0)
	insertAuditRow(t, db, dayAtNoon(100), 1000, 0)
	insertAuditRow(t, db, dayAtNoon(400), 10000, 0)
	return db
}

// heatByDate 把结果整理成 date → tokens。
func heatByDate(t *testing.T, db *DB, days uint32) map[string]uint64 {
	t.Helper()
	rows, err := db.GetTokenHeatmap(days)
	if err != nil {
		t.Fatalf("GetTokenHeatmap(%d) 失败: %v", days, err)
	}
	out := make(map[string]uint64, len(rows))
	for _, row := range rows {
		if _, dup := out[row.Date]; dup {
			t.Fatalf("GetTokenHeatmap(%d) 返回了重复日期 %s", days, row.Date)
		}
		out[row.Date] = row.Tokens
	}
	return out
}

func dateKey(days int) string { return dayAtNoon(days).Format("2006-01-02") }

// TestTokenHeatmapWindowCoversRequestedDays 是这次缺陷的直接回归：
// days=365 必须覆盖到 100 天前的记录（修复前它连今天零点之前的都查不到）。
func TestTokenHeatmapWindowCoversRequestedDays(t *testing.T) {
	db := newHeatmapDB(t)

	t.Run("days=365 覆盖 100 天前的记录", func(t *testing.T) {
		got := heatByDate(t, db, 365)
		if _, ok := got[dateKey(100)]; !ok {
			t.Fatalf("days=365 应包含 100 天前的记录，实际 %v", got)
		}
		// 400 天前在窗口之外。
		if _, ok := got[dateKey(400)]; ok {
			t.Fatalf("days=365 不应包含 400 天前的记录，实际 %v", got)
		}
		if len(got) != 3 {
			t.Fatalf("days=365 应返回 3 天，实际 %d 天：%v", len(got), got)
		}
		if got[dateKey(100)] != 1000 {
			t.Fatalf("100 天前的 token 数 = %d, want 1000", got[dateKey(100)])
		}
	})

	t.Run("days=91 不覆盖 100 天前（边界）", func(t *testing.T) {
		got := heatByDate(t, db, 91)
		if _, ok := got[dateKey(100)]; ok {
			t.Fatalf("days=91 不应包含 100 天前的记录，实际 %v", got)
		}
		if len(got) != 2 {
			t.Fatalf("days=91 应返回 2 天（今天 + 3 天前），实际 %d 天：%v", len(got), got)
		}
	})

	t.Run("同一天的记录按键求和", func(t *testing.T) {
		got := heatByDate(t, db, 7)
		// 今天两行：10+1 与 20+2。
		if got[dateKey(0)] != 33 {
			t.Fatalf("今天的 token 数 = %d, want 33（两行之和）", got[dateKey(0)])
		}
		if got[dateKey(3)] != 100 {
			t.Fatalf("3 天前的 token 数 = %d, want 100", got[dateKey(3)])
		}
	})

	t.Run("days=0 只含今天", func(t *testing.T) {
		got := heatByDate(t, db, 0)
		if len(got) != 1 {
			t.Fatalf("days=0 应只返回今天，实际 %v", got)
		}
		if _, ok := got[dateKey(0)]; !ok {
			t.Fatalf("days=0 应包含今天，实际 %v", got)
		}
	})

	t.Run("覆盖天数随 days 单调不减", func(t *testing.T) {
		daysList := []uint32{0, 3, 7, 30, 91, 365, 400, 800}
		prev := -1
		for _, days := range daysList {
			n := len(heatByDate(t, db, days))
			if n < prev {
				t.Fatalf("days=%d 覆盖 %d 天，少于更小窗口的 %d 天（窗口被算错）", days, n, prev)
			}
			prev = n
		}
		if prev != 4 {
			t.Fatalf("800 天窗口应覆盖全部 4 天，实际 %d 天", prev)
		}
	})
}

// TestPeriodCutoffMsOnlyKnowsStatsPeriods 固定 periodCutoffMs 的契约：
// 它只服务 /api/stats 的三个周期，未知取值按 today 处理。
// 这条测试是「别再把天数当周期传进来」的护栏。
func TestPeriodCutoffMsOnlyKnowsStatsPeriods(t *testing.T) {
	today := localMidnightDaysAgo(0).UnixMilli()
	if got := periodCutoffMs("today"); got != today {
		t.Fatalf("periodCutoffMs(today) = %d, want 今天零点 %d", got, today)
	}
	if got := periodCutoffMs("7d"); got != localMidnightDaysAgo(7).UnixMilli() {
		t.Fatalf("periodCutoffMs(7d) 应为 7 天前零点")
	}
	if got := periodCutoffMs("30d"); got != localMidnightDaysAgo(30).UnixMilli() {
		t.Fatalf("periodCutoffMs(30d) 应为 30 天前零点")
	}
	// "365d" 这类「天数」不是它的取值：按 today 处理（因此绝不能拿它算任意天数窗口）。
	if got := periodCutoffMs("365d"); got != today {
		t.Fatalf("periodCutoffMs 对未知取值应按 today 处理，实际 %d", got)
	}
}

// TestStatsPeriodWindows 确认三个统计周期的窗口边界（GetStats 的原有语义不变）。
func TestStatsPeriodWindows(t *testing.T) {
	db := newHeatmapDB(t)

	todayRows := func(period string) int64 {
		t.Helper()
		stats, err := db.GetStats(period, nil)
		if err != nil {
			t.Fatalf("GetStats(%s) 失败: %v", period, err)
		}
		return stats.TotalRequests
	}

	if got := todayRows("today"); got != 2 {
		t.Fatalf("today 窗口应含今天 2 行，实际 %d", got)
	}
	if got := todayRows("7d"); got != 3 {
		t.Fatalf("7d 窗口应含今天 2 行 + 3 天前 1 行 = 3，实际 %d", got)
	}
	if got := todayRows("30d"); got != 3 {
		t.Fatalf("30d 窗口应含 3 行，实际 %d", got)
	}
}
