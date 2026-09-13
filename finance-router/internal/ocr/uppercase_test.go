package ocr

import "testing"

func TestParseChineseAmount(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"肆圆陆角整", 460, true},
		{"叁圆贰角陆分", 326, true},
		{"贰佰贰拾捌圆叁角壹分", 22831, true},
		{"壹仟贰佰叁拾肆圆伍角陆分", 123456, true},
		{"肆圆整", 400, true},
		{"壹拾圆整", 1000, true},
		{"拾圆整", 1000, true},
		{"壹佰万圆整", 100000000, true},
		{"", 0, false},
		{"随便写的", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseChineseAmount(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseChineseAmount(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		} else {
			t.Logf("✓ %q → %d 分", c.in, got)
		}
	}
}
