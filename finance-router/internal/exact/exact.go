// Package exact 实现发票的"精确通道"：PDF 文字层与票面二维码。
//
// 为什么需要：这两条通道都是**确定性**的 —— 要么读出、要么读不出，
// 不存在"识别错了"。而模型识别（VLM/OCR）是概率性的，还可能凭空编造。
// 实测（docs/30-review/28）：本项目收到的电子发票 PDF **都带文字层**，
// 且票面二维码可稳定解出，两者给出的发票号码/价税合计/开票日期完全一致。
//
// 用法：能走精确通道就不调模型；两条通道都有时**交叉校验**，不一致就报冲突。
package exact

import (
	"fmt"
	"strings"
)

// Fields 是精确通道读出的字段。所有字段都可能为空（读到什么算什么）。
type Fields struct {
	Source string `json:"source"` // pdftext | qr | pdftext+qr

	InvoiceNo   string `json:"invoice_no"`   // 发票号码（数电票 20 位）
	InvoiceCode string `json:"invoice_code"` // 发票代码（数电票为空）
	Date        string `json:"date"`         // 开票日期 YYYY-MM-DD

	AmountCent  *int64 `json:"amount_incl_tax_cent"` // 价税合计（分）
	TaxCent     *int64 `json:"tax_cent"`
	ExclCent    *int64 `json:"amount_excl_tax_cent"`
	AmountUpper string `json:"amount_upper"` // 票面大写（如"肆拾捌圆玖角整"）

	BuyerName   string `json:"buyer_name"`
	BuyerTaxID  string `json:"buyer_tax_id"`
	SellerName  string `json:"seller_name"`
	SellerTaxID string `json:"seller_tax_id"`

	// OrderNos 是从票面（主要是备注栏）里捞出来的候选订单号。
	// 电商发票常把订单号写在备注，这是"发票↔订单"最强的确定性键。
	OrderNos []string `json:"order_nos,omitempty"`

	RawQR   string `json:"raw_qr,omitempty"`
	RawText string `json:"raw_text,omitempty"`
}

// Conflict 表示两条通道对同一字段给出了不同答案。
//
// ⚠️ 这**不应该被静默忽略**：精确通道之间不一致，说明至少有一条读错了
// （或这张票被人改过）。这种单据必须交人工，不能自动通过。
type Conflict struct {
	Field string
	A, B  string
}

func (c Conflict) String() string { return fmt.Sprintf("%s: %q ≠ %q", c.Field, c.A, c.B) }

// Merge 合并两条通道的结果，并返回冲突列表。
// a 优先（通常是文字层，字段更全）；b 只补 a 没有的字段。
func Merge(a, b *Fields) (*Fields, []Conflict) {
	switch {
	case a == nil:
		return b, nil
	case b == nil:
		return a, nil
	}
	out := *a
	var conflicts []Conflict

	check := func(name, av, bv string) {
		if av != "" && bv != "" && av != bv {
			conflicts = append(conflicts, Conflict{Field: name, A: av, B: bv})
		}
	}
	fill := func(dst *string, bv string) {
		if *dst == "" {
			*dst = bv
		}
	}

	check("发票号码", a.InvoiceNo, b.InvoiceNo)
	fill(&out.InvoiceNo, b.InvoiceNo)
	check("开票日期", a.Date, b.Date)
	fill(&out.Date, b.Date)
	check("发票代码", a.InvoiceCode, b.InvoiceCode)
	fill(&out.InvoiceCode, b.InvoiceCode)
	// 金额：两条通道都有值时比对（这是"看图读错"最容易发生的地方）
	if a.AmountCent != nil && b.AmountCent != nil && *a.AmountCent != *b.AmountCent {
		conflicts = append(conflicts, Conflict{
			Field: "价税合计",
			A:     Yuan(*a.AmountCent), B: Yuan(*b.AmountCent),
		})
	}
	if out.AmountCent == nil {
		out.AmountCent = b.AmountCent
	}
	if out.RawQR == "" {
		out.RawQR = b.RawQR
	}
	out.Source = "pdftext+qr"
	return &out, conflicts
}

// Yuan 把"分"格式化成两位小数的元，用于显示与比对。
func Yuan(cent int64) string {
	neg := ""
	if cent < 0 {
		neg, cent = "-", -cent
	}
	return fmt.Sprintf("%s%d.%02d", neg, cent/100, cent%100)
}

// ParseAmount 把 "48.90" / "1,234.56" 解析成"分"。解析失败返回 nil。
//
// 为什么不复用 match.NormalizeAmount：那个函数面向模型输出的宽松文本，
// 这里面向票面/二维码里的严格数字串，语义不同，分开更清楚。
func ParseAmount(s string) *int64 {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, "¥", "")
	s = strings.ReplaceAll(s, "￥", "")
	if s == "" {
		return nil
	}
	var yuan, frac int64
	var fracDigits int
	dot := strings.IndexByte(s, '.')
	intPart := s
	if dot >= 0 {
		intPart, frac = s[:dot], 0
		fracStr := s[dot+1:]
		if len(fracStr) > 2 {
			fracStr = fracStr[:2]
		}
		fracDigits = len(fracStr)
		for _, c := range fracStr {
			if c < '0' || c > '9' {
				return nil
			}
			frac = frac*10 + int64(c-'0')
		}
	}
	if intPart == "" {
		intPart = "0"
	}
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return nil
		}
		yuan = yuan*10 + int64(c-'0')
	}
	for fracDigits < 2 {
		frac *= 10
		fracDigits++
	}
	cent := yuan*100 + frac
	return &cent
}
