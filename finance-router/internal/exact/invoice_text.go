package exact

import (
	"regexp"
	"strings"
)

// 电子发票 PDF 文字层里的字段模式。
//
// 注意：这些正则面对的是 `pdftotext -layout` 的输出，**列是按空白对齐的**，
// 所以购买方/销售方会横向交错在同一行：
//
//	购 名称：<购买方>            销 名称：<销售方>
//	信 统一社会信用代码/纳税人识别号：<税号>   信 统一社会信用代码/纳税人识别号：<税号>
//
// 因此不能按行解析，要按**全文中的出现顺序**取第 1、2 个。
var (
	// 发票号码：20 位（数电票）。老式票 8 位，故放宽到 8~25。
	reInvoiceNo = regexp.MustCompile(`发票号码[:：]\s*([0-9]{8,25})`)
	// 开票日期：2026年06月02日
	reInvoiceDate = regexp.MustCompile(`开票日期[:：]\s*(\d{4})\s*年\s*(\d{1,2})\s*月\s*(\d{1,2})\s*日`)
	// 价税合计（大写）肆拾捌圆玖角整 （小写）¥48.90
	reTotalUpper = regexp.MustCompile(`价税合计\s*[（(]\s*大写\s*[）)]\s*(\S+?)\s*[（(]\s*小写\s*[）)]\s*[¥￥]?\s*([0-9,]+(?:\.[0-9]+)?)`)
	// 合计 ¥43.27 ¥5.63  → 金额、税额
	reSum = regexp.MustCompile(`合\s*计\s*[¥￥]\s*([0-9,]+(?:\.[0-9]+)?)\s+[¥￥]\s*([0-9,]+(?:\.[0-9]+)?)`)
	// 名称：xxx（按出现顺序：购买方、销售方）
	rePartyName = regexp.MustCompile(`名\s*称[:：]\s*([^\s，,、]+)`)
	// 统一社会信用代码/纳税人识别号：xxx
	reTaxID = regexp.MustCompile(`(?:统一社会信用代码\s*/\s*)?纳税人识别号[:：]\s*([0-9A-Za-z]{15,20})`)
	// 候选订单号：票面里的长数字串（备注栏里的电商订单号）
	reLongDigits = regexp.MustCompile(`[0-9]{15,25}`)
)

// ParseInvoiceText 从电子发票 PDF 的文字层解析字段。
//
// 只填能可靠识别的字段；识别不到就留空 —— **不猜**。
// 这是精确通道，宁可少读几个字段，也不能给出错的值。
func ParseInvoiceText(text string) *Fields {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	f := &Fields{Source: "pdftext", RawText: text}

	if m := reInvoiceNo.FindStringSubmatch(text); m != nil {
		f.InvoiceNo = m[1]
	}
	if m := reInvoiceDate.FindStringSubmatch(text); m != nil {
		f.Date = m[1] + "-" + pad2(m[2]) + "-" + pad2(m[3])
	}
	if m := reTotalUpper.FindStringSubmatch(text); m != nil {
		f.AmountUpper = m[1]
		f.AmountCent = ParseAmount(m[2])
	}
	if m := reSum.FindStringSubmatch(text); m != nil {
		f.ExclCent = ParseAmount(m[1])
		f.TaxCent = ParseAmount(m[2])
		// 有"金额+税额"就能自洽推出价税合计 —— 但只在没有直接读到小写时才用，
		// 避免覆盖更权威的票面小写值。
		if f.AmountCent == nil && f.ExclCent != nil && f.TaxCent != nil {
			sum := *f.ExclCent + *f.TaxCent
			f.AmountCent = &sum
		}
	}

	// 购买方/销售方：按出现顺序取前两个
	if ms := rePartyName.FindAllStringSubmatch(text, -1); len(ms) > 0 {
		f.BuyerName = ms[0][1]
		if len(ms) > 1 {
			f.SellerName = ms[1][1]
		}
	}
	if ms := reTaxID.FindAllStringSubmatch(text, -1); len(ms) > 0 {
		f.BuyerTaxID = ms[0][1]
		if len(ms) > 1 {
			f.SellerTaxID = ms[1][1]
		}
	}

	// 候选订单号：票面里的长数字，排除发票号码与双方税号本身。
	//
	// 电商发票常把订单号写在**备注**栏，这是"发票↔订单"最强的确定性键。
	// 但备注栏在 -layout 输出里的位置不固定（实测内容会出现在"备/注"标签**之前**），
	// 按位置取很脆；改成"全文里除票号/税号外的长数字串"，稳得多。
	// ⚠️ 必须按**子串**排除，不能按相等。
	// 统一社会信用代码/纳税人识别号形如「17 位数字 + 1 位字母」，
	// 其中那个 17 位数字前缀本身就是一个合法的"长数字"，会被 reLongDigits 扫到。
	// 按相等排除会漏掉它 —— 结果税号被当成订单号，拿它去匹配订单必然失败，
	// 而且看不出哪里错。
	claimed := []string{f.InvoiceNo, f.BuyerTaxID, f.SellerTaxID}
	for _, d := range reLongDigits.FindAllString(text, -1) {
		if d == "" {
			continue
		}
		skip := false
		for _, c := range claimed {
			if c != "" && strings.Contains(c, d) {
				skip = true
				break
			}
		}
		if !skip {
			f.OrderNos = append(f.OrderNos, d)
		}
	}
	return f
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}
