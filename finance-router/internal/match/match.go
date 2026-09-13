// Package match 实现三单互核与各种"归一化 + 交叉校验"。
//
// 设计前提（来自实测）：
//   - 模型会读错数字：实测把 ¥4.60 的 460 读成 4600（10 倍），
//     把发票号 <INVOICE_NO_2> 读成 <INVOICE_NO_1>（多插一个 0）。
//   - 算术自检挡不住自洽的错误（450+10=460 与 455+5=460 都自洽）。
//   - 因此必须靠**独立信息源**交叉验证：票面大写金额、文件名里的票号、三张单据互核。
package match

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/ocr"
)

// ── 发票号码归一化 ────────────────────────────────────────────

// 全电发票号码为 20 位；老式发票代码 10~12 位 + 号码 8 位。
var (
	// 文件名里的长数字（实测：`_<INVOICE_NO_2>_00_00_76373.pdf`）
	reLongDigits = regexp.MustCompile(`\d{15,25}`)
	reNonDigit   = regexp.MustCompile(`\D`)
)

// InvoiceNoResult 是票号的归一化结果。
type InvoiceNoResult struct {
	No       string // 最终采用的票号（已去非数字）
	Source   string // ocr | filename | none
	Conflict bool   // OCR 与文件名不一致（★ 说明至少一个错了）
	OCRValue string // OCR 原值，便于审计
	FileVal  string // 文件名推断值
}

// NormalizeInvoiceNo 归一化发票号码。
//
// 优先级：**文件名 > OCR**。
// 理由：全电发票 PDF 的文件名是开票系统生成的（`<票号>_00_00_xxx.pdf`），
// 属于"元数据"，不是图像识别结果，不会出现数字插入/丢失。
// 实测同一张票：文件名给出正确值，OCR 多插了一个 0。
func NormalizeInvoiceNo(ocrNo, filename string) InvoiceNoResult {
	r := InvoiceNoResult{
		OCRValue: strings.TrimSpace(ocrNo),
	}
	if m := reLongDigits.FindString(filename); m != "" {
		r.FileVal = m
	}
	ocrDigits := reNonDigit.ReplaceAllString(r.OCRValue, "")
	if r.FileVal != "" {
		r.No, r.Source = r.FileVal, "filename"
		if ocrDigits != "" && ocrDigits != r.FileVal {
			r.Conflict = true
		}
		return r
	}
	if ocrDigits != "" {
		r.No, r.Source = ocrDigits, "ocr"
		return r
	}
	r.Source = "none"
	return r
}

// ValidInvoiceNoLen 判断票号长度是否合理（20 位全电票号，或 8 位老式号码）。
func ValidInvoiceNoLen(no string) bool {
	switch len(no) {
	case 8, 20:
		return true
	}
	return false
}

// ── 金额归一化 ────────────────────────────────────────────────

// NormalizeAmount 把金额归一化为"支出为正"的分值。
//
// 理由（实测）：付款/转账截图常把支出显示为负数（-326.00），
// 而发票、订单是正数。比较前必须统一符号，否则会把同一笔钱判成不一致。
// 注意：**只对非发票类做取绝对值**；发票金额为负是异常（红冲票），保留原号并标记。
func NormalizeAmount(cent *int64, kind ocr.Kind) *int64 {
	if cent == nil {
		return nil
	}
	v := *cent
	if v < 0 && (kind == ocr.KindPayment || kind == ocr.KindOrder) {
		v = -v
	}
	return &v
}

// ── 三单互核 ──────────────────────────────────────────────────

// Item 是参与互核的一份证据。
type Item struct {
	Slot   string
	Kind   ocr.Kind
	Amount *int64 // 已归一化
	Date   string
	Party  string
}

// Finding 是一条核对结论。
type Finding struct {
	Level  string // OK | WARN | FAIL
	Code   string
	Detail string
}

// Report 是三单互核结果。
type Report struct {
	Findings []Finding
	Amounts  map[string]*int64 // slot -> 归一化金额
	Dates    *DateSpan         // 日期互核结论
	Verdict  string            // MATCHED | SUSPECT | DEFECTIVE
}

// DateSpan 是日期互核的结论。
type DateSpan struct {
	Earliest string
	Latest   string
	Days     int
	Checked  bool // 是否够两个以上日期可比
}

// CompareDates 做日期互核：三张单据的日期应落在同一时间段内。
//
// 为什么要单独做：实测发现**日期读错不会被金额校验拦住**。
// 同一实例里发票是 2026-04-20、付款是 2026-04-18，而订单被读成 **2024-04-18**
// （差 2 年），但金额三单一致、大写校验通过 —— 整个链路「全绿」，错误却存在。
//
// 阈值刻意放宽：跨期报销、先买后付都是合理的，所以只抓**量级上的荒谬**
// （如年份读错），不做严格相等。
func CompareDates(items []Item, maxSpanDays int) DateSpan {
	var ds []time.Time
	var raws []string
	for _, it := range items {
		if it.Date == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", strings.TrimSpace(it.Date))
		if err != nil {
			continue
		}
		ds = append(ds, t)
		raws = append(raws, it.Date)
	}
	if len(ds) < 2 {
		return DateSpan{Checked: false}
	}
	minT, maxT := ds[0], ds[0]
	minS, maxS := raws[0], raws[0]
	for i, t := range ds {
		if t.Before(minT) {
			minT, minS = t, raws[i]
		}
		if t.After(maxT) {
			maxT, maxS = t, raws[i]
		}
	}
	return DateSpan{
		Earliest: minS, Latest: maxS,
		Days: int(maxT.Sub(minT).Hours() / 24), Checked: true,
	}
}

// Compare 做三单互核：金额为主，日期/对方为辅。
//
// 设计取舍（用户 2026-09-13 决定）：
//   - "订单与发票真不一致"暂不处理 → 只**记录**，不判定失败（MismatchPolicy = RecordOnly）；
//   - 但"发票读错"必须能发现 → 因此**发票与订单/付款不一致时一律 WARN 并进人工队列**，
//     因为无法区分"读错"与"真不一致"。
func Compare(items []Item, toleranceCent int64) *Report {
	r := &Report{Amounts: map[string]*int64{}, Verdict: "MATCHED"}

	var inv, oth []Item
	for _, it := range items {
		if it.Amount != nil {
			r.Amounts[it.Slot] = it.Amount
		}
		switch it.Kind {
		case ocr.KindInvoice:
			inv = append(inv, it)
		case ocr.KindOrder, ocr.KindPayment:
			if it.Amount != nil {
				oth = append(oth, it)
			}
		}
	}

	// ① 发票缺失 → 无法完成三单核对
	if len(inv) == 0 {
		r.Findings = append(r.Findings, Finding{"FAIL", "NO_INVOICE", "没有识别到发票"})
	}
	// ② 发票金额缺失
	for _, it := range inv {
		if it.Amount == nil {
			r.Findings = append(r.Findings, Finding{"FAIL", "INVOICE_AMOUNT_MISSING",
				it.Slot + " 未读出金额"})
		}
	}
	// ③ 订单/付款金额缺失
	for _, it := range items {
		if (it.Kind == ocr.KindOrder || it.Kind == ocr.KindPayment) && it.Amount == nil {
			r.Findings = append(r.Findings, Finding{"WARN", "SUPPORT_AMOUNT_MISSING",
				it.Slot + " 未读出金额"})
		}
	}

	// ④ 核心：发票 vs 订单/付款
	if len(inv) > 0 && inv[0].Amount != nil {
		base := *inv[0].Amount
		for _, it := range oth {
			diff := base - *it.Amount
			if diff < 0 {
				diff = -diff
			}
			if diff <= toleranceCent {
				r.Findings = append(r.Findings, Finding{"OK", "AMOUNT_AGREE",
					it.Slot + " 与发票一致"})
				continue
			}
			// 不一致：区分"像读错"还是"像真差"
			code, detail := classifyMismatch(base, *it.Amount, toleranceCent)
			r.Findings = append(r.Findings, Finding{"WARN", code, it.Slot + "：" + detail})
		}
	}

	// ⑤ 日期互核：抓"金额都对但日期荒谬"的情况
	if maxSpan := 30; true {
		ds := CompareDates(items, maxSpan)
		r.Dates = &ds
		if ds.Checked && ds.Days > maxSpan {
			r.Findings = append(r.Findings, Finding{"WARN", "DATE_SPAN",
				fmt.Sprintf("单据日期跨度过大（%s ~ %s，%d 天）——可能是年份读错",
					ds.Earliest, ds.Latest, ds.Days)})
		}
	}

	// ⑥ 汇总判定
	for _, f := range r.Findings {
		if f.Level == "FAIL" {
			r.Verdict = "DEFECTIVE"
			return r
		}
	}
	for _, f := range r.Findings {
		if f.Level == "WARN" {
			r.Verdict = "SUSPECT"
			return r
		}
	}
	return r
}

// classifyMismatch 猜测差异性质，给出可操作的提示。
func classifyMismatch(a, b, tol int64) (string, string) {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	small, big := a, b
	if small > big {
		small, big = big, small
	}
	// 10 倍/100 倍关系 → 极可能是位数读错（实测发生过 10 倍错）
	if small > 0 {
		for _, k := range []int64{10, 100} {
			if big/small == k && big%small <= tol {
				return "AMOUNT_SCALE_ERROR",
					fmt.Sprintf("疑似 %d 倍读错（%d vs %d）", k, a, b)
			}
		}
	}
	return "AMOUNT_MISMATCH",
		fmt.Sprintf("金额不一致（%d vs %d，差 %d 分）", a, b, diff)
}

// ── 年份推断 ──────────────────────────────────────────────────
//
// 背景（实测）：电商订单页常只写"6月26日"**不写年份**（如淘宝的"凭据：6月26日下单"）。
// 模型会因为要求输出 YYYY-MM-DD 而**猜**一个年份，实测猜成了 2024/2023，
// 导致三单日期跨度出现 700~1100 天的荒谬值。
//
// 正确做法：让模型**只输出月日**（如 "06-26"），年份由下游根据其它单据推断。

var reMonthDay = regexp.MustCompile(`^(\d{1,2})-(\d{1,2})$`)
var reFullDate = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)

// HasYear 判断日期串是否自带年份。
func HasYear(s string) bool { return reFullDate.MatchString(strings.TrimSpace(s)) }

// InferYear 把只有月日的日期补上年份，reference 无效时回落到系统时间。
//
// 参考基准的优先级（重要）：
//  1. **同一笔业务的其它单据日期**（发票/付款）—— 首选，因为同属一笔交易，
//     年份必然接近；且不受"报销滞后多久"影响；
//  2. **系统时间** —— 兜底。适用于一次只有一张缺年份的单据、没有可参照对象时。
//
// 为什么不是"一律用系统时间"：报销常滞后。1 月处理去年 12 月的单子时，
// 用系统时间会把年份补成新的一年，整整差 12 个月。而用发票日期不会错。
func InferYear(monthDay, reference string) (string, bool) {
	return InferYearWithNow(monthDay, reference, time.Now())
}

// InferYearWithNow 同上，但显式传入"现在"，便于测试。
func InferYearWithNow(monthDay, reference string, now time.Time) (string, bool) {
	m := reMonthDay.FindStringSubmatch(strings.TrimSpace(monthDay))
	if m == nil {
		return "", false
	}
	// 参考基准：优先用同笔业务的日期，取不到就用系统时间
	ref, err := time.Parse("2006-01-02", strings.TrimSpace(reference))
	if err != nil {
		ref = now
	}
	month, day := m[1], m[2]
	if len(month) == 1 {
		month = "0" + month
	}
	if len(day) == 1 {
		day = "0" + day
	}
	// 候选年份：参考年 ±1
	type cand struct {
		date   string
		t      time.Time
		future bool
	}
	var cands []cand
	// 宽限 1 天：容忍时区/时钟偏差，但仍把"明显未来"的排除掉
	cutoff := now.AddDate(0, 0, 1)
	for _, y := range []int{ref.Year() - 1, ref.Year(), ref.Year() + 1} {
		s := fmt.Sprintf("%04d-%s-%s", y, month, day)
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			continue // 如 02-30 这种不存在的日期
		}
		cands = append(cands, cand{s, t, t.After(cutoff)})
	}
	if len(cands) == 0 {
		return "", false
	}

	// ★ 票据不可能来自未来：优先在"不晚于今天"的候选里选**最近的那个**。
	best, bestGap := "", 1<<30
	for _, c := range cands {
		if c.future {
			continue
		}
		gap := c.t.Sub(ref)
		if gap < 0 {
			gap = -gap
		}
		if d := int(gap.Hours() / 24); d < bestGap {
			bestGap, best = d, c.date
		}
	}
	if best != "" {
		return best, true
	}
	// 全部落在未来（罕见：单据日期确实晚于今天）→ 退化为取最近的一个
	for _, c := range cands {
		gap := c.t.Sub(ref)
		if gap < 0 {
			gap = -gap
		}
		if d := int(gap.Hours() / 24); d < bestGap {
			bestGap, best = d, c.date
		}
	}
	return best, best != ""
}
