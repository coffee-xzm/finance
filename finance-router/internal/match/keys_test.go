package match

import "testing"

// 实测数据的形态：
//
//	淘宝订单页  订单号        4000000000000000001（19 位）
//	支付宝账单  商家订单号    T200P4000000000000000001（带前缀）
//	支付宝交易号 2xxxxxxxxxxxxxxx…（28 位，实测读不稳，只作辅助）
const (
	tOrderNo   = "4000000000000000001"
	tMerchant  = "T200P" + tOrderNo
	tAlipayTxn = "2026010123001116114116735029"
)

// ★ 核心：订单与付款必须靠**共有的长数字段**配上，而不是靠某个字段名。
//
// 实测背景：模型在支付宝账单上把「订单号」（其实是交易号）和「商家订单号」
// 填反了，而且没去掉 T200P 前缀。若按字段名配对，这一单必然配不上。
func TestOrderPaymentPairBySharedDigitRun(t *testing.T) {
	order := KeysOf(tOrderNo, "", "")
	pay := KeysOf(tMerchant, tAlipayTxn) // 模型把两个值填乱了也无所谓

	shared := order.SharedKey(pay)
	if shared != tOrderNo {
		t.Fatalf("应在订单号上配对成功，实际 shared=%q", shared)
	}
	t.Logf("✓ 靠共有数字段 %s 配上（前缀 T200P 不影响）", shared)
}

// 前缀/分隔符不能把数字段切断。
func TestDigitRunsTolerantToPrefixAndSeparators(t *testing.T) {
	cases := map[string]string{
		tMerchant:                 tOrderNo,
		"订单号 " + tOrderNo:         tOrderNo,
		"4000 0000 0000 0000 001": tOrderNo,
		"4000-0000-0000-0000-001": tOrderNo,
	}
	for in, want := range cases {
		runs := DigitRuns(in)
		found := false
		for _, r := range runs {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DigitRuns(%q) = %v，应含 %s", in, runs, want)
		}
	}
}

// 太短的数字段不参与配对（避免金额位数巧合造成误配）。
func TestDigitRunsIgnoresShortNumbers(t *testing.T) {
	if r := DigitRuns("金额 4890 元，日期 2026-05-09"); len(r) != 0 {
		t.Fatalf("短数字不该成为配对键，实际 %v", r)
	}
}

// 两份完全无关的单据不应配上。
func TestUnrelatedDocsDoNotPair(t *testing.T) {
	a := KeysOf("4000000000000000001")
	b := KeysOf("5000000000000000002")
	if s := a.SharedKey(b); s != "" {
		t.Fatalf("无关单据不该配上，实际 shared=%q", s)
	}
}

// 支付宝交易号只在两边**读得完全一致**时才能用作配对依据 ——
// 实测它读不稳（同一笔交易三个不同读数），所以不能单独依赖它。
func TestAlipayTxnOnlyPairsWhenIdentical(t *testing.T) {
	a := KeysOf(tAlipayTxn)
	b := KeysOf("202601012300111611416735029") // 少一位的误读
	if s := a.SharedKey(b); s != "" {
		t.Fatalf("读得不一样的交易号不该配上，实际 %q", s)
	}
	if s := a.SharedKey(KeysOf(tAlipayTxn)); s != tAlipayTxn {
		t.Fatalf("完全一致时应配上，实际 %q", s)
	}
}
