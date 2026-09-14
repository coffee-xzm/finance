package pipeline

import (
	"strings"
	"testing"

	"github.com/coffee/finance-router/internal/store"
)

// mkEv 造一条证据。index 是"该槽位内的序号"，与 FileMeta.Index 同义。
func mkEv(slot, kind string, index int, invoiceNo string) store.Evidence {
	e := store.Evidence{Slot: slot, Kind: kind, IndexNo: index, LocalPNG: "files/x.png"}
	if invoiceNo != "" {
		e.InvoiceNo = invoiceNo
		e.InvoiceNoSrc = "exact"
	}
	return e
}

func cent(v int64) *int64 { return &v }

func mkInst(evs []store.Evidence, groups []store.DocGroup, depts []string) store.SyncInstance {
	return store.SyncInstance{
		Sub: store.Submission{
			InstanceCode: "INST-1", Status: "PENDING", Departments: depts,
			IsAlipay: "是",
		},
		Evidence: evs,
		Groups:   groups,
	}
}

// ★ 核心回归：一单多票时，每张票的订单/付款必须精确归属到**自己的那一行**。
//
// 曾经的 bug：分组只记槽位名，落表时每一行都挂上该实例的全部订单截图，
// 图片张冠李戴。这里用"两张票、各一张订单截图"把它钉住。
func TestPlanRowsAttributesEvidencePerGroup(t *testing.T) {
	evs := []store.Evidence{
		mkEv("发票文件", "invoice", 1, "INV-A"),
		mkEv("订单截图", "order", 1, ""),
		mkEv("发票文件", "invoice", 2, "INV-B"),
		mkEv("订单截图", "order", 2, ""),
	}
	groups := []store.DocGroup{
		{GroupIndex: 0, InvoiceNo: "INV-A", InvoiceEv: "发票文件:1",
			OrderEvs: []string{"订单截图:1"}, Matched: true,
			InvoiceTotalCent: cent(4890), SupportTotalCent: cent(4890)},
		{GroupIndex: 1, InvoiceNo: "INV-B", InvoiceEv: "发票文件:2",
			OrderEvs: []string{"订单截图:2"}, Matched: true,
			InvoiceTotalCent: cent(1000), SupportTotalCent: cent(1000)},
	}

	rows := planRows(mkInst(evs, groups, nil))
	if len(rows) != 2 {
		t.Fatalf("想要 2 行（一张发票一行），得到 %d", len(rows))
	}
	if rows[0].InvoiceNo != "INV-A" || rows[1].InvoiceNo != "INV-B" {
		t.Fatalf("行的发票号码不对: %q / %q", rows[0].InvoiceNo, rows[1].InvoiceNo)
	}
	if got := rows[0].OrderEvs[0].IndexNo; got != 1 {
		t.Errorf("行 0 的订单截图应是 #1，实际 #%d —— 图片会张冠李戴", got)
	}
	if got := rows[1].OrderEvs[0].IndexNo; got != 2 {
		t.Errorf("行 1 的订单截图应是 #2，实际 #%d", got)
	}
	if rows[0].Verdict != "一致" || rows[0].Problem != "" {
		t.Errorf("金额对上的组应判「一致」且无问题，得到 %q/%q", rows[0].Verdict, rows[0].Problem)
	}
	if rows[0].GroupIdx != 1 || rows[1].GroupIdx != 2 {
		t.Errorf("分组序号应从 1 起：%d / %d", rows[0].GroupIdx, rows[1].GroupIdx)
	}
}

// 幂等键必须区分同一实例的不同发票 —— 否则第二张票会覆盖第一张。
func TestRowKeySeparatesInvoicesOfOneInstance(t *testing.T) {
	a := rowKey("INST-1", "INV-A")
	b := rowKey("INST-1", "INV-B")
	if a == b {
		t.Fatal("同一实例的两张发票算出了同一个幂等键 —— 后一张会覆盖前一张")
	}
	// 发票号码缺失时退化成只用实例号（那种行是兜底行，一实例一行）
	if rowKey("INST-1", "") != rowKey("INST-1", "  ") {
		t.Error("空号码应归一化成同一个键")
	}
	if rowKey("INST-1", "") == b {
		t.Error("兜底行的键与有号码的行撞了")
	}
}

// 有单据没配上 → 整单进人工，不能自动通过。
func TestPlanRowsLeftoverForcesManual(t *testing.T) {
	evs := []store.Evidence{
		mkEv("发票文件", "invoice", 1, "INV-A"),
		mkEv("订单截图", "order", 1, ""),
		mkEv("订单截图", "order", 2, ""), // 这张没配上
	}
	groups := []store.DocGroup{
		{GroupIndex: 0, InvoiceNo: "INV-A", InvoiceEv: "发票文件:1",
			OrderEvs: []string{"订单截图:1"}, Matched: true,
			InvoiceTotalCent: cent(4890), SupportTotalCent: cent(4890)},
	}
	inst := mkInst(evs, groups, nil)
	inst.Sub.Leftover = []string{"订单截图:2"}

	rows := planRows(inst)
	if len(rows) != 1 {
		t.Fatalf("想要 1 行，得到 %d", len(rows))
	}
	if rows[0].Verdict != "存疑" {
		t.Errorf("有没配上的单据时应判「存疑」，得到 %q", rows[0].Verdict)
	}
	if rows[0].Problem == "" {
		t.Error("必须记下问题原因，否则自动通过会把人该看的单吞掉")
	}
	if !strings.Contains(rows[0].Explain, "没能配到任何发票") {
		t.Errorf("差异说明里要说清原因，得到 %q", rows[0].Explain)
	}
}

// 提交形态不合规 → 也要进人工。
func TestPlanRowsFormProblemForcesManual(t *testing.T) {
	evs := []store.Evidence{mkEv("发票文件", "invoice", 1, "INV-A")}
	groups := []store.DocGroup{{GroupIndex: 0, InvoiceNo: "INV-A", InvoiceEv: "发票文件:1",
		Matched: true, InvoiceTotalCent: cent(100), SupportTotalCent: cent(100)}}
	inst := mkInst(evs, groups, nil)
	inst.Sub.FormProblem = "FORM_NON_ALIPAY_ONE_EACH：非支付宝只允许 1 张发票，实际 2 张"

	rows := planRows(inst)
	if rows[0].Verdict != "存疑" || rows[0].Problem == "" {
		t.Fatalf("形态不合规必须进人工，得到 %q/%q", rows[0].Verdict, rows[0].Problem)
	}
}

// 金额对不上 → 存疑，且说明里写清两个数。
func TestPlanRowsAmountMismatch(t *testing.T) {
	evs := []store.Evidence{mkEv("发票文件", "invoice", 1, "INV-A")}
	groups := []store.DocGroup{{GroupIndex: 0, InvoiceNo: "INV-A", InvoiceEv: "发票文件:1",
		Matched: false, InvoiceTotalCent: cent(4890), SupportTotalCent: cent(3000)}}
	rows := planRows(mkInst(evs, groups, nil))
	if rows[0].Verdict != "存疑" {
		t.Fatalf("对不上应判「存疑」，得到 %q", rows[0].Verdict)
	}
	for _, want := range []string{"48.90", "30.00"} {
		if !strings.Contains(rows[0].Explain, want) {
			t.Errorf("差异说明应含 %s，实际 %q", want, rows[0].Explain)
		}
	}
}

// 一张发票都没分出来 → 兜底行，且明确说"缺件"。
func TestPlanRowsFallbackNoInvoice(t *testing.T) {
	evs := []store.Evidence{mkEv("订单截图", "order", 1, "")}
	rows := planRows(mkInst(evs, nil, nil))
	if len(rows) != 1 || rows[0].GroupIdx != 0 {
		t.Fatalf("应恰好 1 行兜底行，得到 %d", len(rows))
	}
	if rows[0].Verdict != "缺件" {
		t.Errorf("没有发票应判「缺件」，得到 %q", rows[0].Verdict)
	}
	if len(rows[0].AllEvs) != 1 {
		t.Error("兜底行要挂上整实例的图，否则人工看不到任何图")
	}
}

// 分组只有发票号码、没有任何证据引用（老数据）时，不应崩，也不应误挂别组的图。
func TestResolveBackwardCompatibleSlotOnly(t *testing.T) {
	evs := []store.Evidence{
		mkEv("订单截图", "order", 1, ""),
		mkEv("订单截图", "order", 2, ""),
	}
	ix := indexEvidence(evs)
	if got := resolveOne(ix, "订单截图:2", ""); len(got) != 1 || got[0].IndexNo != 2 {
		t.Fatalf("精确引用应只解析出一张图，得到 %d 张", len(got))
	}
	// 旧格式（纯槽位名）退化成整个槽位
	if got := resolveOne(ix, "订单截图", ""); len(got) != 2 {
		t.Errorf("旧格式应退化成整槽位 2 张，得到 %d", len(got))
	}
	// 引用了不存在的证据 → 空，而不是随便挑一张
	if got := resolveOne(ix, "订单截图:9", ""); len(got) != 0 {
		t.Errorf("引用不存在时应返回空，得到 %d 张", len(got))
	}
}

// 配对依据要落进表里，人工复核时才知道"凭什么配上的"。
func TestExplainCarriesReasons(t *testing.T) {
	evs := []store.Evidence{mkEv("发票文件", "invoice", 1, "INV-A")}
	groups := []store.DocGroup{{GroupIndex: 0, InvoiceNo: "INV-A", InvoiceEv: "发票文件:1",
		Matched: true, InvoiceTotalCent: cent(4890), SupportTotalCent: cent(4890),
		Reasons: []string{"发票备注里的订单号 123 命中 订单截图"}}}
	rows := planRows(mkInst(evs, groups, nil))
	if !strings.Contains(rows[0].Explain, "凭什么") && !strings.Contains(rows[0].Explain, "命中") {
		t.Errorf("差异说明里应保留配对依据，得到 %q", rows[0].Explain)
	}
}
