package pipeline

import (
	"encoding/json"
	"testing"

	"github.com/coffee/finance-router/internal/bitable"
	"github.com/coffee/finance-router/internal/feishu"
)

// 新表单的真实控件名。注意「是否为支付宝付款」这一项的括号说明里
// 把"发票""订单""付款"全都包含了。
const alipayName = "是否为支付宝付款（上传多张发票：需满足一张发票对应一张订单，支付宝支付  |||  上传多张订单、付款记录：需满足支付宝支付）"

func w(name, typ, val string) feishu.FormWidget {
	return feishu.FormWidget{Name: name, Type: typ, Value: json.RawMessage(val)}
}

// ★ 回归：附件槽位必须按【控件类型】过滤，不能只按名字。
//
// 真实 bug：新表单里「是否为支付宝付款（…上传多张发票…上传多张订单、付款记录…）」
// 这个 radio 控件名字里包含全部三个关键词，且排在附件控件前面。
// 只按名字匹配会让三个槽位全部命中它 → 一张附件都取不到，
// 而错误信息只说"槽位名对不上"，把人引向完全错误的方向。
func TestAttachmentWidgetIgnoresNonAttachment(t *testing.T) {
	widgets := []feishu.FormWidget{
		w("归属组（技术组只能由技术组长选取，正式队员选择兵种组）", "department", `[{"name":"视觉组"}]`),
		w("资金来源", "radioV2", `"个人"`),
		w(alipayName, "radioV2", `"是"`), // ← 陷阱：名字含"发票/订单/付款"
		w("发票", "attachmentV2", `["https://x/inv.pdf"]`),
		w("订单截图", "attachmentV2", `["https://x/order.jpg"]`),
		w("付款记录", "attachmentV2", `["https://x/pay.jpg"]`),
	}

	cases := []struct{ slot, wantURL string }{
		{"发票", "https://x/inv.pdf"},
		{"订单截图", "https://x/order.jpg"},
		{"付款记录", "https://x/pay.jpg"},
	}
	for _, rule := range slotRules {
		var want string
		for _, c := range cases {
			if c.slot == rule.Slot {
				want = c.wantURL
			}
		}
		got, ok := findAttachmentWidget(widgets, rule.Keyword)
		if !ok {
			t.Fatalf("%s：没找到附件控件（很可能命中了同名的非附件控件）", rule.Slot)
		}
		urls := got.AttachmentURLs()
		if len(urls) != 1 || urls[0] != want {
			t.Fatalf("%s：拿到 %v，期望 [%s]", rule.Slot, urls, want)
		}
		if got.Name == alipayName {
			t.Fatalf("%s：命中「是否为支付宝付款」控件 —— 类型过滤失效", rule.Slot)
		}
	}
	t.Log("✓ 三个附件槽位都没被同名 radio 控件抢走")
}

// 旧表单仍要能读：关键字是「发票文件/订单截图/付款截图」。
func TestAttachmentWidgetStillMatchesOldForm(t *testing.T) {
	widgets := []feishu.FormWidget{
		w("物资所属部门", "department", `[{"name":"机械组"}]`),
		w("发票文件", "attachmentV2", `["https://x/a.pdf"]`),
		w("订单截图", "attachmentV2", `["https://x/b.jpg"]`),
		w("付款截图", "attachmentV2", `["https://x/c.jpg"]`),
	}
	for _, rule := range slotRules {
		got, ok := findAttachmentWidget(widgets, rule.Keyword)
		if !ok || len(got.AttachmentURLs()) == 0 {
			t.Fatalf("%s：旧表单也应当能匹配到", rule.Slot)
		}
	}
}

// buildMeta 必须用"名称包含"匹配：新表单控件名带长括号说明。
func TestBuildMetaMatchesNewFormByContains(t *testing.T) {
	widgets := []feishu.FormWidget{
		w("归属组（技术组只能由技术组长选取，正式队员选择兵种组）", "department",
			`[{"name":"视觉组","open_id":"od-x"}]`),
		w("资金来源", "radioV2", `"个人"`),
		w(alipayName, "radioV2", `"是"`),
	}
	m := buildMeta(nil, "INST", &feishu.InstanceDetail{ApprovalName: "测试用审批"}, widgets)
	if len(m.Departments) != 1 || m.Departments[0] != "视觉组" {
		t.Fatalf("归属组没读到: %v", m.Departments)
	}
	if m.FundSource != "个人" {
		t.Fatalf("资金来源没读到: %q", m.FundSource)
	}
	if m.IsAlipay != "是" {
		t.Fatalf("是否为支付宝付款没读到: %q（精确匹配会静默落空）", m.IsAlipay)
	}
	if m.ApplicantDept != "" {
		t.Logf("注意：ApplicantDept 仍由调用方从 detail.DepartmentID 解析（%q）", m.ApplicantDept)
	}
}

// 表单把「归属组」改回了「物资所属部门」—— 两个名字都要认。
func TestBuildMetaAcceptsBothDeptWidgetNames(t *testing.T) {
	for _, name := range []string{"物资所属部门", "物资所属部门（技术组只能由技术组长选取）", "归属组", "归属组（说明）"} {
		m := buildMeta(nil, "INST", &feishu.InstanceDetail{},
			[]feishu.FormWidget{w(name, "department", `[{"name":"电控组"}]`)})
		if len(m.Departments) != 1 || m.Departments[0] != "电控组" {
			t.Errorf("控件名 %q 没被识别: %v", name, m.Departments)
		}
	}
}

// ★ 购买人是 contact 控件：值可能只是用户 ID，绝不能当人名写进表。
func TestPersonValueShapes(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`"许芙蓉"`, "许芙蓉"},                        // 旧表单的 input
		{`[{"name":"许芙蓉"}]`, "许芙蓉"},             // 带 name 的对象数组
		{`[{"text":"许芙蓉"}]`, "许芙蓉"},             // 富文本片段
		{`"ou_abc"`, "ou_abc"},                  // 纯 open_id
		{`["ou_abc"]`, "ou_abc"},                // open_id 数组
		{`[{"open_id":"ou_abc"}]`, "ou_abc"},    // 带 open_id 的对象
		{`[{"id":"ou_abc","name":"李四"}]`, "李四"}, // 两者都有 → 优先 name
		{`null`, ""},
		{`[]`, ""},
	}
	for _, c := range cases {
		if got := personValue(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("personValue(%s) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

func TestLooksLikeUserID(t *testing.T) {
	for _, s := range []string{
		"ou_abc123", "on_abc", "8f3a9c2b1d4e5f60718293a4b5",
		// ★ 实测：contact 控件给的就是这种 8 位短 ID，没有 ou_ 前缀、也不长。
		//   靠"长度/前缀"猜的实现会把它当人名直接写进「购买人」列。
		"u7x2k9qz",
	} {
		if !looksLikeUserID(s) {
			t.Errorf("%q 应被判定为用户 ID", s)
		}
	}
	for _, s := range []string{"许芙蓉", "", "张三丰", "张三", "John", "Li Ming", "a@b.com"} {
		if looksLikeUserID(s) {
			t.Errorf("%q 不应被判定为用户 ID（这是人名/邮箱）", s)
		}
	}
}

// contact 控件的值形态决定了必须靠**控件类型**判断，不能靠"长得像不像 ID"猜。
func TestBuildMetaMarksContactBuyerAsID(t *testing.T) {
	m := buildMeta(nil, "INST", &feishu.InstanceDetail{},
		[]feishu.FormWidget{w("购买人", "contact", `["u7x2k9qz"]`)})
	if m.Buyer != "u7x2k9qz" {
		t.Fatalf("contact 值没解析出来: %q", m.Buyer)
	}
	if !m.buyerIsID {
		t.Fatal("contact 控件的值必须标记为「这是 ID，要换姓名」")
	}

	// 旧表单的 input 控件：值是真人名，不该被当成 ID
	m2 := buildMeta(nil, "INST", &feishu.InstanceDetail{},
		[]feishu.FormWidget{w("购买人", "input", `"许芙蓉"`)})
	if m2.buyerIsID {
		t.Fatal("input 控件的人名被误判成 ID —— 会被白白清空")
	}
}

// ★ 不变量：附件槽位名必须在三处一致 ——
// slotRules 的规范名、bitable.slots、以及表的附件列名。
// 表单改名（发票文件→发票）时最容易漏改其中一处，然后表现成"图不见了"。
func TestSlotNamesAreConsistent(t *testing.T) {
	if len(slotRules) != len(bitable.Slots()) {
		t.Fatalf("槽位数量不一致：slotRules=%d bitable=%d", len(slotRules), len(bitable.Slots()))
	}
	for i, rule := range slotRules {
		if rule.Slot != bitable.Slots()[i] {
			t.Errorf("第 %d 个槽位名不一致：slotRules=%q bitable=%q", i, rule.Slot, bitable.Slots()[i])
		}
	}
	// 表里必须真的有这几个附件列，否则写入会报字段不存在。
	cols := map[string]bool{}
	for _, f := range bitable.ReviewTable().Fields {
		cols[f.Name] = true
	}
	for _, s := range bitable.Slots() {
		if !cols[s] {
			t.Errorf("「报销核对」表里没有附件列 %q", s)
		}
	}
}
