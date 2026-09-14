// Package ocr 是票据抽取层：把一张图变成结构化字段。
//
// 设计要点（见 docs/30-review/02-ocr-provider-interface.md）：
//   - Provider 只负责"图 → 字段"，**不做任何通过/不通过判定**；
//   - 换引擎 = 加一个实现 + 改一行配置；
//   - **读不到就返回 nil，禁止猜测**（幻觉金额比空值危险得多）。
package ocr

import "context"

// Kind 由模型判定（调用方不传，避免传错）。
type Kind string

const (
	KindInvoice   Kind = "invoice"   // 增值税发票（强版式，有票号）
	KindOrder     Kind = "order"     // 订单详情页截图
	KindPayment   Kind = "payment"   // 转账/付款记录截图
	KindItinerary Kind = "itinerary" // 行程单（★ 实测存在："滴滴出行行程报销单"）
	KindUnknown   Kind = "unknown"
)

// Result 是一张图的抽取结果。金额一律以【分】存整数。
//
// ★ 金额三件套（专治"漏读税额"）：
//   - AmountInclTaxCent 是**主口径**（发票的"价税合计"、订单的"实付"、转账的"金额"）
//   - TaxCent 单独留存，用于核对与发现漏读
//   - AmountExclTaxCent 是不含税金额（发票的"金额"栏）
//
// 所有金额字段都是 *int64：nil 表示**没读到**，与 0 有本质区别。
type Result struct {
	Kind       Kind    `json:"kind"`
	Confidence float64 `json:"confidence"`

	AmountInclTaxCent *int64 `json:"amount_incl_tax_cent"`
	TaxCent           *int64 `json:"tax_cent"`
	AmountExclTaxCent *int64 `json:"amount_excl_tax_cent"`

	// ★ 价税合计的【大写】原文（如"肆圆陆角整"）。中文发票自带，
	// 是独立于小写的**第二套字形**，可作交叉校验。
	AmountInclTaxUpper string `json:"amount_incl_tax_upper"`

	Date         string `json:"date"`         // YYYY-MM-DD
	Counterparty string `json:"counterparty"` // 销方/店铺/收款方

	InvoiceCode string `json:"invoice_code"`
	InvoiceNo   string `json:"invoice_no"`
	CheckCode   string `json:"check_code"`
	SellerTaxID string `json:"seller_tax_id"`
	OrderNo     string `json:"order_no"`      // 商家/店铺订单号
	AlipayTxnID string `json:"alipay_txn_id"` // 支付宝交易号（配对"订单↔付款"的键）

	// 审计与可回放
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	TraceID     string `json:"trace_id"`
	LatencyMS   int    `json:"latency_ms"`
	RawResponse string `json:"raw_response,omitempty"`
	Error       string `json:"error,omitempty"`
}

// TaxCheck 是"漏读税额"的自检结果。
type TaxCheck string

const (
	TaxCheckOK          TaxCheck = "OK"           // 价税合计 − 不含税 ≈ 税额
	TaxCheckMismatch    TaxCheck = "TAX_MISMATCH" // 三者对不上
	TaxCheckNoTaxField  TaxCheck = "NO_TAX_FIELD" // 没有独立的税额/不含税字段（如订单、转账）
	TaxCheckUndecidable TaxCheck = "无法判定"
)

// CheckTax 自检：价税合计 − 不含税金额 是否等于税额。
//
// toleranceCent 是容差（分）。三者都读到才判定；缺任何一项都返回 NO_TAX_FIELD
// —— **宁可标"无法判定"，也不要静默采信一个可能漏税的金额**。
func (r *Result) CheckTax(toleranceCent int64) TaxCheck {
	if r.AmountInclTaxCent == nil || r.AmountExclTaxCent == nil || r.TaxCent == nil {
		return TaxCheckNoTaxField
	}
	diff := *r.AmountInclTaxCent - *r.AmountExclTaxCent - *r.TaxCent
	if diff < 0 {
		diff = -diff
	}
	if diff <= toleranceCent {
		return TaxCheckOK
	}
	return TaxCheckMismatch
}

// UpperCheck 是"小写 vs 大写"的交叉校验结果。
type UpperCheck string

const (
	UpperOK         UpperCheck = "OK"             // 小写与大写一致
	UpperMismatch   UpperCheck = "UPPER_MISMATCH" // 不一致 —— 至少一个读错了
	UpperNoField    UpperCheck = "NO_UPPER"       // 票面没有大写（订单/转账）或未读到
	UpperUnparsable UpperCheck = "UNPARSABLE"     // 读到大写但解析不了
)

// CheckUpper 用票面自带的大写金额校验小写金额。
//
// 这比 CheckTax 更可靠：大写是**另一套字形**，与小写独立出错；
// 而"价税合计−不含税≈税额"在模型编造自洽三元组时会失效（已实测）。
func (r *Result) CheckUpper(toleranceCent int64) UpperCheck {
	if r.AmountInclTaxUpper == "" {
		return UpperNoField
	}
	up, ok := ParseChineseAmount(r.AmountInclTaxUpper)
	if !ok {
		return UpperUnparsable
	}
	if r.AmountInclTaxCent == nil {
		return UpperUnparsable
	}
	diff := *r.AmountInclTaxCent - up
	if diff < 0 {
		diff = -diff
	}
	if diff <= toleranceCent {
		return UpperOK
	}
	return UpperMismatch
}

// ImageRef 是待抽取的图片。
type ImageRef struct {
	SHA256    string
	Bytes     []byte // 已预处理（PDF 已转 PNG）
	MediaType string // image/png | image/jpeg
	Detail    string // high | low
}

// Provider 是换引擎的唯一接缝。
type Provider interface {
	Name() string
	// Extract 必须：(1) 幂等可重试；(2) 失败返回 error 而非 panic；
	// (3) 成功时金额字段允许为 nil —— 绝不允许编造金额。
	Extract(ctx context.Context, img ImageRef) (*Result, error)
}
