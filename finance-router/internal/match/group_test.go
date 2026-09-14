package match

import (
	"testing"

	"github.com/coffee/finance-router/internal/ocr"
)

// 实测数据的形态（合成值）：
//
//	订单页 订单号 = 4…001；支付宝账单 商家订单号 = T200P + 4…001
//	两张图靠这个 19 位数字段配上
const (
	gOrderNo   = "4000000000000000001"
	gOrderNo2  = "5000000000000000002"
	gMerchant  = "T200P" + gOrderNo
	gMerchant2 = "T200P" + gOrderNo2
)

func ip(v int64) *int64 { return &v }

func inv(slot string, cent int64, orderNoInRemark string) Doc {
	d := Doc{Slot: slot, Kind: ocr.KindInvoice, Amount: ip(cent), InvoiceNo: "10000000000000000001"}
	if orderNoInRemark != "" {
		// 发票备注里的订单号 → 进 Keys（真实实现里由 exact 解析填 OrderNo + Keys）
		d.Keys = KeysOf("10000000000000000001", orderNoInRemark)
		d.OrderNo = orderNoInRemark
	} else {
		d.Keys = KeysOf("10000000000000000001")
	}
	return d
}

func order(slot string, cent int64, orderNo string) Doc {
	return Doc{Slot: slot, Kind: ocr.KindOrder, Amount: ip(cent), OrderNo: orderNo,
		Keys: KeysOf(orderNo)}
}

func pay(slot string, cent int64, merchantNo string) Doc {
	return Doc{Slot: slot, Kind: ocr.KindPayment, Amount: ip(cent), Keys: KeysOf(merchantNo)}
}

// 一发票一订单（订单↔付款靠共有数字段配上）
func TestGroupOneToOne(t *testing.T) {
	docs := []Doc{
		inv("发票", 4890, gOrderNo),
		order("订单", 4890, gOrderNo),
		pay("付款", 4890, gMerchant),
	}
	g := Build(docs, FormAlipay, 1)
	if g.FormProblem != "" {
		t.Fatalf("形态应合规: %s", g.FormProblem)
	}
	if len(g.Groups) != 1 {
		t.Fatalf("应 1 组，实际 %d", len(g.Groups))
	}
	grp := g.Groups[0]
	if len(grp.Orders) != 1 || len(grp.Payments) != 1 {
		t.Fatalf("组内应是 1 订单 1 付款，实际 %d/%d", len(grp.Orders), len(grp.Payments))
	}
	if !grp.Matched(1) {
		t.Fatalf("金额应配上：发票 %d vs 支撑 %d", grp.InvoiceTotal, grp.SupportTotal)
	}
	t.Logf("✓ %v", grp.Reasons)
}

// ★ 订单↔付款必须靠**共有数字段**，不受模型填错字段影响。
// 实测：支付宝账单里模型把交易号填进了 order_no，把商家订单号填进了 alipay_txn_id。
func TestGroupPairSurvivesFieldSwap(t *testing.T) {
	o := order("订单", 4890, gOrderNo)
	// 模拟模型填反：OrderNo 里放交易号，Keys 仍从所有字段汇总（含商家订单号）
	p := Doc{Slot: "付款", Kind: ocr.KindPayment, Amount: ip(4890),
		OrderNo: "2026010123001116114116735029",
		Keys:    KeysOf("T200P"+gOrderNo, "2026010123001116114116735029")}
	g := Build([]Doc{inv("发票", 4890, gOrderNo), o, p}, FormAlipay, 1)
	if len(g.Groups) != 1 || len(g.Groups[0].Payments) != 1 {
		t.Fatalf("字段填反也应能配上，实际 %+v", g.Groups)
	}
}

// 支付宝 + 一发票对多订单 → 允许（只 1 张发票）
func TestGroupAlipayOneInvoiceManyOrders(t *testing.T) {
	docs := []Doc{
		inv("发票", 10000, ""), // 两张订单 4000+6000=10000
		order("订单1", 4000, gOrderNo),
		order("订单2", 6000, gOrderNo2),
		pay("付款1", 4000, gMerchant),
		pay("付款2", 6000, gMerchant2),
	}
	g := Build(docs, FormAlipay, 1)
	if g.FormProblem != "" {
		t.Fatalf("1 发票多订单在支付宝下应合规: %s", g.FormProblem)
	}
	if len(g.Groups) != 1 || len(g.Groups[0].Orders) != 2 {
		t.Fatalf("应 1 组含 2 订单，实际 %+v", g.Groups)
	}
	if !g.Groups[0].Matched(1) {
		t.Fatalf("求和应配上：%d vs %d", g.Groups[0].InvoiceTotal, g.Groups[0].SupportTotal)
	}
	t.Logf("✓ %v", g.Groups[0].Reasons)
}

// ★ 支付宝 + 一发票多订单 + 多张发票 → **形态不合规**（需求明令）
func TestFormAlipayMultiInvoiceWithMultiOrderRejected(t *testing.T) {
	docs := []Doc{
		inv("发票1", 10000, ""),
		inv("发票2", 3000, ""),
		order("订单1", 4000, gOrderNo),
		order("订单2", 6000, gOrderNo2),
	}
	g := Build(docs, FormAlipay, 1)
	if g.FormProblem == "" {
		t.Fatal("一发票多订单同时有多张发票，应判形态不合规")
	}
	t.Logf("✓ %s", g.FormProblem)
}

// ★ 非支付宝：只允许 1:1:1
func TestFormNonAlipayRejectsMultiple(t *testing.T) {
	docs := []Doc{
		inv("发票1", 4890, gOrderNo),
		inv("发票2", 1000, ""),
		order("订单", 4890, gOrderNo),
		pay("付款", 4890, gMerchant),
	}
	g := Build(docs, FormNonAlipay, 1)
	if g.FormProblem == "" {
		t.Fatal("非支付宝多张发票应判形态不合规")
	}
	t.Logf("✓ %s", g.FormProblem)
}

// 非支付宝 1:1:1 应合规
func TestFormNonAlipayOneEachOK(t *testing.T) {
	docs := []Doc{
		inv("发票", 4890, ""),
		order("订单", 4890, ""),
		pay("付款", 4890, ""),
	}
	g := Build(docs, FormNonAlipay, 1)
	if g.FormProblem != "" {
		t.Fatalf("非支付宝 1:1:1 应合规: %s", g.FormProblem)
	}
}

// 配不上的单据进 Leftover，交给人工纠错
func TestUnmatchedGoesToLeftover(t *testing.T) {
	docs := []Doc{
		inv("发票", 4890, ""),
		order("订单", 9999, gOrderNo), // 金额对不上、备注也没命中
		pay("付款", 4890, "9999999999999999999"),
	}
	g := Build(docs, FormAlipay, 1)
	if len(g.Groups) != 1 {
		t.Fatalf("应仍有 1 组（发票本身），实际 %d", len(g.Groups))
	}
	if len(g.Groups[0].Orders) != 0 {
		t.Fatalf("金额对不上的订单不该进组，实际 %d", len(g.Groups[0].Orders))
	}
	if len(g.Leftover) != 2 {
		t.Fatalf("订单与付款都应进 Leftover，实际 %d", len(g.Leftover))
	}
}

// 一份发票配不上任何东西时，也该出现在结果里（不能静默丢单）
func TestInvoiceAlwaysProducesGroup(t *testing.T) {
	g := Build([]Doc{inv("发票", 4890, "")}, FormAlipay, 1)
	if len(g.Groups) != 1 || len(g.Leftover) != 0 {
		t.Fatalf("孤零零的发票也应成组: groups=%d leftover=%d", len(g.Groups), len(g.Leftover))
	}
}
