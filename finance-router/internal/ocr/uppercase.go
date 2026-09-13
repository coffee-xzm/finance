package ocr

import (
	"strings"
)

// 大写金额解析：中文发票的「价税合计(大写)」是**票面自带的校验码**。
//
// 例：肆圆陆角整 → 460 分；叁圆贰角陆分 → 326 分；壹仟贰佰叁拾肆圆伍角陆分 → 123456 分
//
// 为什么重要：小写数字可能被误读（实测同一张 ¥4.60 的发票，模型在 300dpi 下
// 把 460 读成了 4600，即 10 倍错误）。而**大写是另一套字形**，两者独立出错，
// 因此"小写 vs 大写"是一个便宜且有效的交叉校验。
//
// 解析失败返回 nil —— 不猜。

var cnDigits = map[rune]int{
	'零': 0, '〇': 0, '○': 0,
	'一': 1, '壹': 1, '二': 2, '贰': 2, '貳': 2, '两': 2, '兩': 2,
	'三': 3, '叁': 3, '參': 3, '参': 3,
	'四': 4, '肆': 4, '五': 5, '伍': 5,
	'六': 6, '陆': 6, '陸': 6, '七': 7, '柒': 7,
	'八': 8, '捌': 8, '九': 9, '玖': 9,
}

var cnUnits = map[rune]int{
	'十': 10, '拾': 10,
	'百': 100, '佰': 100,
	'千': 1000, '仟': 1000,
}

// 圆/元/角/分 的分隔字
func isYuan(r rune) bool { return r == '圆' || r == '元' || r == '園' || r == '圜' }
func isJiao(r rune) bool { return r == '角' }
func isFen(r rune) bool  { return r == '分' }
func isEnd(r rune) bool  { return r == '整' || r == '正' }

// ParseChineseAmount 把大写金额解析为「分」。无法解析返回 (0,false)。
func ParseChineseAmount(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	// 去掉常见前缀与空白
	for _, p := range []string{"价税合计", "金额", "人民币", "小写", "大写", "（", "）", "(", ")", "：", ":"} {
		s = strings.ReplaceAll(s, p, "")
	}
	s = strings.TrimSpace(s)
	// 允许"肆圆陆角整"这种没有"分"的写法
	var (
		yuanPart, jiaoPart, fenPart string
	)
	// 切分：先找 圆/元，再找 角，最后 分
	if i := indexAny(s, isYuan); i >= 0 {
		yuanPart = s[:i]
		rest := s[i+1:]
		if j := indexAny(rest, isJiao); j >= 0 {
			jiaoPart = rest[:j]
			rest2 := rest[j+1:]
			if k := indexAny(rest2, isFen); k >= 0 {
				fenPart = rest2[:k]
			}
		} else if k := indexAny(rest, isFen); k >= 0 {
			fenPart = rest[:k]
		}
	} else {
		// 没有"圆"，可能是纯数字写法，放弃
		return 0, false
	}

	yuan, ok1 := parseChineseInt(yuanPart)
	jiao, ok2 := parseChineseInt(jiaoPart)
	fen, ok3 := parseChineseInt(fenPart)
	if !ok1 {
		return 0, false
	}
	if jiaoPart == "" {
		jiao, ok2 = 0, true
	}
	if fenPart == "" {
		fen, ok3 = 0, true
	}
	if !ok2 || !ok3 {
		return 0, false
	}
	if jiao < 0 || jiao > 9 || fen < 0 || fen > 9 {
		return 0, false
	}
	return int64(yuan)*100 + int64(jiao)*10 + int64(fen), true
}

// parseChineseInt 解析"壹仟贰佰叁拾肆"这类整数（不含万/亿的复杂组合时也可用于节内）。
func parseChineseInt(s string) (int, bool) {
	if s == "" {
		return 0, true // 空段合法（如"肆圆整"没有角分）
	}
	total, section, num := 0, 0, -1
	seen := false
	for _, r := range s {
		if r == ' ' || r == '　' || isEnd(r) {
			continue
		}
		if d, ok := cnDigits[r]; ok {
			num = d
			seen = true
			continue
		}
		if u, ok := cnUnits[r]; ok {
			if num < 0 {
				num = 1 // "拾伍" = 15
			}
			section += num * u
			num = -1
			seen = true
			continue
		}
		if r == '万' || r == '萬' {
			if num >= 0 {
				section += num
				num = -1
			}
			if section == 0 {
				section = 1
			}
			total += section * 10000
			section = 0
			seen = true
			continue
		}
		if r == '亿' || r == '億' {
			if num >= 0 {
				section += num
				num = -1
			}
			total = (total + section) * 100000000
			section = 0
			seen = true
			continue
		}
		// 未知字符（如标点）忽略
	}
	if num >= 0 {
		section += num
	}
	return total + section, seen
}

func indexAny(s string, pred func(rune) bool) int {
	for i, r := range s {
		if pred(r) {
			return i
		}
	}
	return -1
}
