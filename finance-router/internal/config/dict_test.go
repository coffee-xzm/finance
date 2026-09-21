package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleConfigParses 守住 config.example.yml（它是配置项的唯一文档来源）：
// 一旦 YAML 结构写错（缩进/inline 冲突），这里立刻失败。
func TestExampleConfigParses(t *testing.T) {
	p := filepath.Join("..", "..", "config.example.yml")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("config.example.yml 解析失败: %v", err)
	}
	if len(c.Feishu.Approvals) != 2 {
		t.Errorf("样例应有两个角色化审批，实际 %d", len(c.Feishu.Approvals))
	}
	if c.Field(BaseReview, "human_review") != "人工审核" {
		t.Error("字段字典应回落到内置默认")
	}
	if _, ok := c.Feishu.Bitable.Bases[BaseFlow]; !ok {
		t.Error("样例应包含 flow base 的键（值可为空）")
	}
}

func TestDictDefaults(t *testing.T) {
	c := &Config{}
	c.Dict = DefaultDict()

	if got := c.Field(BaseReview, "human_review"); got != "人工审核" {
		t.Errorf("review.human_review = %q", got)
	}
	if got := c.Field("ledger", "subject"); got != "🔗 科目 / 去向" {
		t.Errorf("ledger.subject = %q（emoji 表头必须逐字一致）", got)
	}
	if c.Control(RoleInvoiceCollect, "buyer") == "" {
		t.Error("invoice_collect.buyer 控件 id 缺失")
	}
	// 「名称（选填，采购提示）」是 2026-09-19 表单新增的控件，
	// 代建发票单要往里写 <项目名称>-采购审批。
	if c.Control(RoleInvoiceCollect, "name") == "" {
		t.Error("invoice_collect.name 控件 id 缺失（「名称（选填，采购提示）」）")
	}
	if got := c.Dict.PurchaseToInvoice.NameSuffix; got != "-采购审批" {
		t.Errorf("purchase_to_invoice.name_suffix 默认 = %q，应为 -采购审批", got)
	}
	if got := c.OptionValue(RoleInvoiceCollect, "资金来源", "老师垫付"); got == "" {
		t.Error("资金来源/老师垫付 的选项 value 缺失")
	}
}

func TestSubjectForGroup(t *testing.T) {
	c := &Config{Dict: DefaultDict()}
	cases := map[string]string{
		"硬件组": "技术组物资",
		"机械组": "技术组物资",
		"电控组": "技术组物资",
		"视觉组": "技术组物资",
		"重装组": "项目组物资",
		"步兵组": "项目组物资",
	}
	for group, want := range cases {
		if got := c.SubjectForGroup(group); got != want {
			t.Errorf("SubjectForGroup(%s) = %s, want %s", group, got, want)
		}
	}
}

func TestDeptAliasAndResolve(t *testing.T) {
	c := &Config{Dict: DefaultDict()}
	// 真实部门 id 不再写进源码（租户数据），测试自己给一个合成值。
	c.Dict.DeptMap = map[string]string{"视觉组": "od-00000000000000000000000000000000"}
	if got := c.DeptAlias("工程组"); got != "重装组" {
		t.Errorf("DeptAlias(工程组) = %s", got)
	}
	if got := c.DeptAlias("英雄组"); got != "重装组" {
		t.Errorf("DeptAlias(英雄组) = %s", got)
	}
	if got := c.DeptAlias("视觉组"); got != "视觉组" {
		t.Errorf("无别名时不应改动: %s", got)
	}
	if od, ok := c.ResolveDept("视觉组"); !ok || od == "" {
		t.Error("视觉组的 open_department_id 应能从 dept_map 解析")
	}
	if _, ok := c.ResolveDept("重装组"); ok {
		t.Error("重装组当前不在 dept_map 里（待补 scope 或手工填）——如已补请更新该断言")
	}
}

func TestApprovalByRoleFallback(t *testing.T) {
	// 旧配置：只有单个 approval_code 时，视为发票收集角色。
	c := &Config{}
	c.Feishu.ApprovalCode = "CODE-1"
	c.Feishu.ApprovalNameExpect = "27发票收集"
	a, ok := c.ApprovalByRole(RoleInvoiceCollect)
	if !ok || a.Code != "CODE-1" {
		t.Fatalf("旧配置回退失败: %+v ok=%v", a, ok)
	}
	if _, ok := c.ApprovalByRole(RolePurchase); ok {
		t.Error("旧配置不应凭空造出采购角色")
	}

	// 新配置：角色列表优先。
	c2 := &Config{}
	c2.Feishu.Approvals = []ApprovalRole{
		{Role: RoleInvoiceCollect, Code: "A"},
		{Role: RolePurchase, Code: "B"},
	}
	if a, _ := c2.ApprovalByRole(RolePurchase); a.Code != "B" {
		t.Errorf("采购角色 = %+v", a)
	}
}

func TestServiceOwned(t *testing.T) {
	c := &Config{Dict: DefaultDict()}
	for _, h := range []string{"人工审核", "已归档", "审核时间", "发票收集进度（待配置）"} {
		if !c.IsServiceOwned(h) {
			t.Errorf("%s 应为服务自有列（允许覆盖）", h)
		}
	}
	if c.IsServiceOwned("物资种类") {
		t.Error("业务列不应是服务自有列")
	}
	if !c.OnlyFillBlank() {
		t.Error("默认应只填空单元格")
	}
}

// TestLoadKeepsPurchaseToInvoiceDefaults 老配置文件（没有 purchase_to_invoice 段、
// 或只写了 draft_mode）必须**保留**新增的 name_suffix 默认值 —— 否则升级后
// 线上会静默地不填「名称（选填，采购提示）」，而且没有任何报错。
func TestLoadKeepsPurchaseToInvoiceDefaults(t *testing.T) {
	dir := t.TempDir()

	p := filepath.Join(dir, "old.yml")
	if err := os.WriteFile(p, []byte("feishu:\n  app_id: cli_x\n  app_secret: s\n"+
		"purchase_to_invoice:\n  draft_mode: rollback_to_start\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load(old.yml): %v", err)
	}
	if got := c.Dict.PurchaseToInvoice.NameSuffix; got != "-采购审批" {
		t.Errorf("name_suffix 默认值被老配置抹掉了: %q", got)
	}
	if got := c.Dict.PurchaseToInvoice.DraftMode; got != "rollback_to_start" {
		t.Errorf("显式写的 draft_mode 丢失: %q", got)
	}

	// 显式置空串 = 关掉该功能（这是唯一的关闭方式，必须有覆盖默认值的能力）
	p2 := filepath.Join(dir, "off.yml")
	if err := os.WriteFile(p2, []byte("purchase_to_invoice:\n  name_suffix: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p2)
	if err != nil {
		t.Fatalf("Load(off.yml): %v", err)
	}
	if got := c2.Dict.PurchaseToInvoice.NameSuffix; got != "" {
		t.Errorf("显式置空应覆盖默认值，实际 %q", got)
	}
}
