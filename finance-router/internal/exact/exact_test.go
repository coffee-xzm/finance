package exact

import "testing"

// 真实发票文字层的结构（公司名/税号已替换为等长占位，版式不变）。
const sampleInvoiceText = `                        电子发票（普通发票）                              发票号码：10000000000000000001
                                                                 开票日期：2026年06月02日
                                                                 下载次数：1
购 名称：某某大学                                 销 名称：某某电器有限公司
买                                           售
方                                           方
信 统一社会信用代码/纳税人识别号：20000000000000001A        信 统一社会信用代码/纳税人识别号：30000000000000001B
息                                           息
    项目名称     规格型号       单 位      数   量       单 价       金 额       税率/征收率              税    额
    *计算机配套产品*CM650 15636        个           1       43.27     43.27       13%                   5.63
合          计                                     ¥43.27                         ¥5.63
价税合计（大写）           肆拾捌圆玖角整                         （小写）¥48.90
4000000000000000001
备
注
开票人：某某人`

func TestParseInvoiceText(t *testing.T) {
	f := ParseInvoiceText(sampleInvoiceText)
	checks := []struct{ name, got, want string }{
		{"发票号码", f.InvoiceNo, "10000000000000000001"},
		{"开票日期", f.Date, "2026-06-02"},
		{"大写金额", f.AmountUpper, "肆拾捌圆玖角整"},
		{"购买方", f.BuyerName, "某某大学"},
		{"销售方", f.SellerName, "某某电器有限公司"},
		{"购买方税号", f.BuyerTaxID, "20000000000000001A"},
		{"销售方税号", f.SellerTaxID, "30000000000000001B"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q want %q", c.name, c.got, c.want)
		}
	}
	if f.AmountCent == nil || Yuan(*f.AmountCent) != "48.90" {
		t.Errorf("价税合计: got %v want 48.90", f.AmountCent)
	}
	if f.ExclCent == nil || Yuan(*f.ExclCent) != "43.27" {
		t.Errorf("金额: got %v want 43.27", f.ExclCent)
	}
	if f.TaxCent == nil || Yuan(*f.TaxCent) != "5.63" {
		t.Errorf("税额: got %v want 5.63", f.TaxCent)
	}
	// 金额 + 税额 必须自洽 —— 这是票面内部的第一次自检
	if *f.ExclCent+*f.TaxCent != *f.AmountCent {
		t.Errorf("金额+税额 ≠ 价税合计: %d+%d ≠ %d", *f.ExclCent, *f.TaxCent, *f.AmountCent)
	}
}

// ★ 回归：税号里的**数字子串**不能被当成订单号。
//
// 真实 bug：`20000000000000001A` 含 17 位数字子串 `20000000000000001`，
// 而"候选订单号"是按 [0-9]{15,25} 扫全文得来的。按**相等**排除税号会漏掉它，
// 结果税号被当成订单号 → 拿它去匹配订单必然失败，且看不出哪里错。
func TestOrderNoCandidatesExcludeTaxIDSubstrings(t *testing.T) {
	f := ParseInvoiceText(sampleInvoiceText)
	if len(f.OrderNos) != 1 {
		t.Fatalf("候选订单号应只有 1 个（真正的备注订单号），实际 %v", f.OrderNos)
	}
	if f.OrderNos[0] != "4000000000000000001" {
		t.Fatalf("候选订单号错误: %v", f.OrderNos)
	}
	for _, bad := range []string{"20000000000000001", "30000000000000001", f.InvoiceNo} {
		for _, got := range f.OrderNos {
			if got == bad {
				t.Fatalf("税号/票号的数字子串 %q 被当成了订单号", bad)
			}
		}
	}
}

func TestParseQR(t *testing.T) {
	// 数电票：发票代码为空
	q := ParseQR("01,32,,10000000000000000001,48.90,20260602,,7014")
	if q == nil {
		t.Fatal("数电票二维码应能解析")
	}
	if q.InvoiceNo != "10000000000000000001" || q.Date != "2026-06-02" {
		t.Errorf("号码/日期错: %q %q", q.InvoiceNo, q.Date)
	}
	if q.AmountCent == nil || Yuan(*q.AmountCent) != "48.90" {
		t.Errorf("金额错: %v", q.AmountCent)
	}
	if q.InvoiceCode != "" {
		t.Errorf("数电票的发票代码应为空，实际 %q", q.InvoiceCode)
	}

	// 老式增值税票：有发票代码、8 位号码、带校验码
	old := ParseQR("01,10,011001605111,80100798,64.9,20161018,85342965681116380258,BE2D")
	if old == nil || old.InvoiceCode != "011001605111" || old.InvoiceNo != "80100798" {
		t.Fatalf("老式票解析错: %+v", old)
	}

	// 不是发票二维码 → 不认（避免把支付码等误当发票）
	if ParseQR("https://example.com/pay?x=1") != nil {
		t.Error("非发票二维码不应被解析")
	}
	if ParseQR("02,32,,123,1,20260101") != nil {
		t.Error("首段不是 01 的不应被解析")
	}
	if ParseQR("01,32,,") != nil {
		t.Error("段数不足的不应被解析")
	}
}

func TestMergeDetectsConflict(t *testing.T) {
	text := ParseInvoiceText(sampleInvoiceText)
	// 造一个"二维码说金额是 49.90"的冲突
	bad := ParseQR("01,32,,10000000000000000001,49.90,20260602,,7014")
	_, conflicts := Merge(text, bad)
	if len(conflicts) != 1 {
		t.Fatalf("应报 1 个冲突，实际 %v", conflicts)
	}
	if conflicts[0].Field != "价税合计" {
		t.Fatalf("冲突字段应为价税合计，实际 %q", conflicts[0].Field)
	}

	// 一致时不应报冲突，且合并后两条通道的字段都在
	good := ParseQR("01,32,,10000000000000000001,48.90,20260602,,7014")
	m, conflicts := Merge(text, good)
	if len(conflicts) != 0 {
		t.Fatalf("一致时不应有冲突: %v", conflicts)
	}
	if m.Source != "pdftext+qr" || !m.Sufficient() || !m.HasUpper() {
		t.Fatalf("合并结果不完整: %+v", m)
	}
}

func TestSufficientRequiresNumberAndAmount(t *testing.T) {
	if (&Fields{InvoiceNo: "1"}).Sufficient() {
		t.Error("只有号码不算够用")
	}
	amt := int64(4890)
	if (&Fields{AmountCent: &amt}).Sufficient() {
		t.Error("只有金额不算够用")
	}
	if !(&Fields{InvoiceNo: "1", AmountCent: &amt}).Sufficient() {
		t.Error("号码+金额 应算够用")
	}
}

func TestParseAmount(t *testing.T) {
	cases := map[string]string{
		"48.90": "48.90", "48.9": "48.90", "1,234.56": "1234.56",
		"¥48.90": "48.90", "0.05": "0.05", "100": "100.00", ".5": "0.50",
	}
	for in, want := range cases {
		got := ParseAmount(in)
		if got == nil || Yuan(*got) != want {
			t.Errorf("ParseAmount(%q) = %v, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "4.5.6", "¥"} {
		if ParseAmount(bad) != nil {
			t.Errorf("ParseAmount(%q) 应返回 nil", bad)
		}
	}
}
