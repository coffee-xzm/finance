package match

import (
	"testing"
	"time"
)

func TestInferYear(t *testing.T) {
	cases := []struct{ md, ref, want string }{
		{"06-26", "2026-07-03", "2026-06-26"},
		{"6月26日", "2026-07-03", ""},           // 非 MM-DD 形态不解析
		{"12-28", "2026-01-03", "2025-12-28"}, // 跨年：应落到上一年
		{"01-03", "2026-12-28", "2026-01-03"}, // 票据不可能未来 → 取 2026 而非 2027
		{"04-18", "2026-04-20", "2026-04-18"},
	}
	for _, c := range cases {
		got, ok := InferYear(c.md, c.ref)
		if c.want == "" {
			if ok {
				t.Errorf("InferYear(%q,%q) = %q, 期望解析失败", c.md, c.ref, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("InferYear(%q,%q) = (%q,%v), 期望 %q", c.md, c.ref, got, ok, c.want)
		} else {
			t.Logf("✓ %s + 参考%s → %s", c.md, c.ref, got)
		}
	}
}

func TestInferYearFallbackToNow(t *testing.T) {
	// 没有可参照的单据日期时，应回落到"系统时间"
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.Local)
	cases := []struct{ md, want string }{
		{"06-26", "2026-06-26"},
		{"12-28", "2025-12-28"}, // 2026-12-28 在 9/13 之后（未来），故取上一年
		{"01-03", "2026-01-03"}, // ★ 不能推到 2027（票据不可能来自未来）
		{"10-01", "2025-10-01"}, // 同理：10-01 在 9/13 之后
	}
	for _, c := range cases {
		got, ok := InferYearWithNow(c.md, "", now)
		if !ok || got != c.want {
			t.Errorf("InferYearWithNow(%q, 空, 2026-09-13) = (%q,%v), 期望 %q", c.md, got, ok, c.want)
		} else {
			t.Logf("✓ %s → %s（系统时间兜底）", c.md, got)
		}
	}
	// 有单据日期时，单据日期优先于系统时间
	got, ok := InferYearWithNow("12-28", "2025-01-05", now)
	if !ok || got != "2024-12-28" {
		t.Errorf("应优先用单据日期：得到 (%q,%v)，期望 2024-12-28", got, ok)
	} else {
		t.Logf("✓ 12-28 + 单据2025-01-05 → %s（单据优先于系统时间）", got)
	}
}
