package pipeline

import (
	"encoding/json"
	"testing"

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
		{"发票文件", "https://x/inv.pdf"},
		{"订单截图", "https://x/order.jpg"},
		{"付款截图", "https://x/pay.jpg"},
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
