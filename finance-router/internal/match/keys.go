package match

import (
	"regexp"
	"sort"
	"strings"
)

// ★ 关于"用哪个键配对订单与付款"—— 这是实测后**改过**的设计。
//
// 原计划按需求用「支付宝交易号」配对。实测发现它**读不稳**：
// 同一笔交易，订单页读成 27 位、支付宝账单读成 28 位、人工看图是 28 位，
// 三个值互不相同。28 位纯数字对模型识别来说太长了。
//
// 而同一份数据里有更稳的键：**商家订单号**（19 位），它同时出现在
//   - 淘宝订单页的「订单号」
//   - 支付宝账单的「商家订单号」（带 T200P 之类前缀）
// 且两次都读对了。
//
// 所以：**配对用"长数字段"求交集**，而不是认定某个具体字段名。
// 这样做同时对两件事免疫：
//   ① 模型把值填进了错误的字段（实测支付宝账单上就把两个字段填反了）
//   ② 值上带前缀（T200P…）或有分隔符
//
// 支付宝交易号仍保留在数据里，作为**辅助**证据。

// 长数字：15 位以上。订单号通常 19 位，支付宝交易号 25~30 位。
var reDigitRun = regexp.MustCompile(`[0-9]{15,32}`)

// DigitRuns 提取字符串里所有长度 >= 15 的连续数字段，去重并排序。
func DigitRuns(s string) []string {
	if s == "" {
		return nil
	}
	// 先把常见的分隔符去掉，避免 "1234 5678" 这类被切断
	clean := strings.NewReplacer(" ", "", "-", "", "_", "", "\t", "").Replace(s)
	found := reDigitRun.FindAllString(clean, -1)
	if len(found) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range found {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// KeySet 是某份单据可用来配对的长数字集合。
// 值来自抽取到的各个字段 —— 不假设哪个字段名才是"对的那个"。
type KeySet struct {
	Runs []string
}

// KeysOf 把若干字段值汇总成一个键集。
func KeysOf(values ...string) KeySet {
	seen := map[string]bool{}
	var runs []string
	for _, v := range values {
		for _, d := range DigitRuns(v) {
			if !seen[d] {
				seen[d] = true
				runs = append(runs, d)
			}
		}
	}
	sort.Strings(runs)
	return KeySet{Runs: runs}
}

// SharedKey 返回两个键集共有的最长数字段；没有共同段时返回 ""。
//
// 取"最长"是为了避免短数字偶然相同（例如两笔交易金额位数巧合），
// 长数字段碰撞概率极低。
func (a KeySet) SharedKey(b KeySet) string {
	set := map[string]bool{}
	for _, r := range b.Runs {
		set[r] = true
	}
	best := ""
	for _, r := range a.Runs {
		if set[r] && len(r) > len(best) {
			best = r
		}
	}
	return best
}

// Contains 判断键集里是否有某个数字段（含子串关系）。
func (a KeySet) Contains(needle string) bool {
	if needle == "" {
		return false
	}
	for _, r := range a.Runs {
		if r == needle || strings.Contains(r, needle) || strings.Contains(needle, r) {
			return true
		}
	}
	return false
}
