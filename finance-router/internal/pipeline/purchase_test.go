package pipeline

import (
	"testing"

	"github.com/coffee/finance-router/internal/config"
)

func TestInstanceCodeFromSourceID(t *testing.T) {
	// 实测值：base64 解码后第二段是 <实例code>-<序号>。
	// 原文：7685702934683798758:1D251D53-F15F-4154-A262-FE9B7441A81B-1:fb20…:1
	const sid = "NzY4NTcwMjkzNDY4Mzc5ODc1ODoxRDI1MUQ1My1GMTVGLTQxNTQtQTI2Mi1GRTlCNzQ0MUE4MUItMTpmYjIwZWJhZDJhNjhmNDI1YTZiNjNlZjQyNDMwOTBiMTox"
	got := instanceCodeFromSourceID(sid)
	want := "1D251D53-F15F-4154-A262-FE9B7441A81B"
	if got != want {
		t.Errorf("instanceCodeFromSourceID = %q, want %q", got, want)
	}
	if instanceCodeFromSourceID("not-base64!!") != "" {
		t.Error("非法 base64 应返回空")
	}
	if instanceCodeFromSourceID("") != "" {
		t.Error("空串应返回空")
	}
}

func TestParseFieldList(t *testing.T) {
	raw := []byte(`[[
	  {"id":"a","name":"名称","type":"input","value":"测试"},
	  {"id":"b","name":"金额","type":"amount","value":234},
	  {"id":"c","name":"数量","type":"number","value":23},
	  {"id":"d","name":"规格","type":"input","value":"3mm"}
	]]`)
	items := parseFieldList(raw)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	it := items[0]
	if it.Name != "测试" || it.Amount != 234 || it.Qty != 23 || it.Spec != "3mm" {
		t.Errorf("解析结果不对: %+v", it)
	}
}

// TestParseFieldListMulti 「多条费用明细 → 多行流水」的解析基础：
// 每条明细都要独立解析出来（RunPurchase 会为每条各写一行）。
func TestParseFieldListMulti(t *testing.T) {
	raw := []byte(`[
	  [
	    {"name":"名称","type":"input","value":"摩擦轮"},
	    {"name":"金额","type":"amount","value":100},
	    {"name":"数量","type":"number","value":1}
	  ],
	  [
	    {"name":"名称","type":"input","value":"轴承"},
	    {"name":"规格","type":"input","value":"608"},
	    {"name":"金额","type":"amount","value":40.5}
	  ]
	]`)
	items := parseFieldList(raw)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Name != "摩擦轮" || items[0].Amount != 100 {
		t.Errorf("第一条明细解析错误: %+v", items[0])
	}
	if items[1].Name != "轴承" || items[1].Amount != 40.5 || items[1].Spec != "608" {
		t.Errorf("第二条明细解析错误: %+v", items[1])
	}
}

func TestStrAndFloatVal(t *testing.T) {
	if strVal([]byte(`"重装组"`)) != "重装组" {
		t.Error("字符串取值失败")
	}
	if strVal([]byte(`42`)) != "42" {
		t.Error("数值应转成字符串")
	}
	if floatVal([]byte(`234`)) != 234 {
		t.Error("数值取值失败")
	}
	if floatVal([]byte(`"234.5"`)) != 234.5 {
		t.Error("字符串数值应可解析")
	}
}

func TestLedgerNote(t *testing.T) {
	info := &purchaseInfo{ProjectName: "弧轮云台"}
	it := purchaseItem{Name: "摩擦轮", Spec: "M3", Qty: 2}
	if got := ledgerNote(info, it); got != "弧轮云台 ｜ 摩擦轮(M3) ×2" {
		t.Errorf("ledgerNote = %q", got)
	}
}

// TestBuildLedgerFields 锁住"采购明细 → 收支表一行"的字段契约：
// 科目按项目组规则、日期取审批完成时间、回链是数组、进度初始为待办、
// 关联人 = 采购审批的提交人（人员字段，open_id）。
func TestBuildLedgerFields(t *testing.T) {
	cfg := &config.Config{}
	cfg.Dict = config.DefaultDict()
	info := &purchaseInfo{
		InstanceCode: "P-1",
		ProjectGroup: "重装组", // 不在 tech_groups → 项目组物资
		ProjectName:  "弧轮云台",
		ApplicantOID: "ou_x", // 提交人 open_id → 关联人
	}
	it := purchaseItem{Name: "摩擦轮", Spec: "M3", Qty: 2, Amount: 234}
	f := buildLedgerFields(cfg, info, it, "https://applink/x", "recMirror", 1789800000000)

	if cfg.Field("ledger", "related_user") != "关联人" {
		t.Errorf("字典里「关联人」列名应为 关联人，实际 %q", cfg.Field("ledger", "related_user"))
	}
	if arr, ok := f[cfg.Field("ledger", "related_user")].([]map[string]any); !ok ||
		len(arr) != 1 || arr[0]["id"] != "ou_x" {
		t.Errorf("关联人（人员字段）应是提交人 open_id，实际 %#v", f[cfg.Field("ledger", "related_user")])
	}

	if f[cfg.Field("ledger", "direction")] != "支出" {
		t.Error("收支方向应为 支出")
	}
	if f[cfg.Field("ledger", "subject")] != "项目组物资" {
		t.Errorf("科目 = %v，重装组应为项目组物资", f[cfg.Field("ledger", "subject")])
	}
	// 金额 = 单价 × 数量（用户 2026-09-21 明确「金额」是单价）：234 × 2 = 468
	if f[cfg.Field("ledger", "amount")] != 468.0 {
		t.Errorf("金额 = %v，应为 单价234 × 数量2 = 468", f[cfg.Field("ledger", "amount")])
	}
	if f[cfg.Field("ledger", "invoice_progress")] != "待办" {
		t.Errorf("进度初值 = %v", f[cfg.Field("ledger", "invoice_progress")])
	}
	if got, ok := f[cfg.Field("ledger", "purchase_link")].([]string); !ok || len(got) != 1 || got[0] != "recMirror" {
		t.Errorf("单向关联应写 record_id 数组，实际 %#v", f[cfg.Field("ledger", "purchase_link")])
	}
	if f[cfg.Field("ledger", "occurred_at")] != int64(1789800000000) {
		t.Errorf("发生日期应取完成时间(ms)，实际 %v", f[cfg.Field("ledger", "occurred_at")])
	}
	if link, ok := f[cfg.Field("ledger", "apply_link")].(map[string]string); !ok || link["link"] == "" {
		t.Errorf("关联申请单ID 应是 {link,text}，实际 %#v", f[cfg.Field("ledger", "apply_link")])
	}

	// 技术组四组 → 技术组物资
	info2 := &purchaseInfo{InstanceCode: "P-2", ProjectGroup: "视觉组"}
	f2 := buildLedgerFields(cfg, info2, it, "", "", 0)
	if f2[cfg.Field("ledger", "subject")] != "技术组物资" {
		t.Errorf("视觉组科目 = %v，应为技术组物资", f2[cfg.Field("ledger", "subject")])
	}
	if _, ok := f2[cfg.Field("ledger", "occurred_at")]; ok {
		t.Error("没有完成时间时不应写发生日期")
	}
	// 拿不到提交人 open_id 时不写「关联人」（写空值会得到一行空白人员，不如留空给人填）
	if _, ok := f2[cfg.Field("ledger", "related_user")]; ok {
		t.Error("没有提交人 open_id 时不应写关联人")
	}
}

// TestBuildRequestFields 锁住"采购 → 27采购申请表一行"的字段契约（对齐参考表 tbllUFPS…）。
func TestBuildRequestFields(t *testing.T) {
	cfg := &config.Config{}
	cfg.Dict = config.DefaultDict()
	info := &purchaseInfo{
		InstanceCode: "P-9", ApprovalName: "采购审批 - 27Test", Status: "APPROVED",
		ApplicantOID: "ou_x", ApplicantDept: "视觉组",
		ProjectName: "测试", Category: "其他",
		StartTimeMS: 111, FinishTimeMS: 222,
	}
	it := purchaseItem{Name: "213", Spec: "M3", Amount: 234, Qty: 5}
	f := buildRequestFields(cfg, info, it, "https://applink/x")

	checks := map[string]any{
		cfg.Field("purchase", "source_id"):      "P-9",
		cfg.Field("purchase", "apply_status"):   "已通过",
		cfg.Field("purchase", "flow"):           "采购审批 - 27Test",
		cfg.Field("purchase", "project_name"):   "测试",
		cfg.Field("purchase", "category"):       "其他",
		cfg.Field("purchase", "applicant_dept"): "视觉组",
		cfg.Field("purchase", "detail_name"):    "213",
		cfg.Field("purchase", "detail_amount"):  234.0,
		cfg.Field("purchase", "detail_qty"):     5.0,
		cfg.Field("purchase", "start_time"):     int64(111),
		cfg.Field("purchase", "finish_time"):    int64(222),
	}
	for k, want := range checks {
		if f[k] != want {
			t.Errorf("%s = %v, want %v", k, f[k], want)
		}
	}
	// 用户 2026-09-19 删掉的 4 列：写进去会让整行创建失败（字段不存在），必须不再出现。
	for _, k := range []string{"detail_spec", "detail_currency", "handler", "node"} {
		if _, ok := f[cfg.Field("purchase", k)]; ok {
			t.Errorf("不应再写已删除的列 %s", cfg.Field("purchase", k))
		}
	}
	if cfg.Field("purchase", "detail_spec") != "" || cfg.Field("purchase", "detail_currency") != "" {
		t.Error("字典里不该再保留已删除列的映射（detail_spec / detail_currency）")
	}
	if arr, ok := f[cfg.Field("purchase", "applicant")].([]map[string]any); !ok ||
		len(arr) != 1 || arr[0]["id"] != "ou_x" {
		t.Errorf("发起人（人员字段）格式不对: %#v", f[cfg.Field("purchase", "applicant")])
	}
	if _, ok := f[cfg.Field("purchase", "image")]; ok {
		t.Error("商品图片本轮应留空（审批附件直链 24h 失效）")
	}
}

// 按控件 id 取出表单里的一项，便于断言（表单是 []map[string]any）。
func formItem(t *testing.T, form []map[string]any, id string) map[string]any {
	t.Helper()
	for _, f := range form {
		if f["id"] == id {
			return f
		}
	}
	return nil
}

// TestBuildInvoiceForm 锁住"代建 27发票收集"的预填契约：
// 购买人 / 物资所属部门 / 名称（名称 = <费用明细的名称> + 后缀「-采购审批」）。
func TestBuildInvoiceForm(t *testing.T) {
	cfg := &config.Config{}
	cfg.Dict = config.DefaultDict()
	info := &purchaseInfo{
		InstanceCode: "P-9", ProjectGroup: "视觉组", ProjectName: "测试",
		ApplicantID: "u_probe01",
		Items:       []purchaseItem{{Name: "QWER", Amount: 23, Qty: 4}},
	}
	depts := map[string]string{"视觉组": "od-00000000000000000000000000000000"}

	form, notes := buildInvoiceForm(cfg, info, depts)
	if len(form) != 3 {
		t.Fatalf("表单应预填 3 项（购买人/部门/名称），实际 %d: %#v", len(form), form)
	}

	buyer := formItem(t, form, cfg.Control(config.RoleInvoiceCollect, "buyer"))
	if buyer == nil || buyer["type"] != "contact" {
		t.Fatalf("购买人控件缺失或类型不对: %#v", buyer)
	}
	if got := buyer["value"].([]string); len(got) != 1 || got[0] != "u_probe01" {
		t.Errorf("购买人 = %#v", buyer["value"])
	}

	dept := formItem(t, form, cfg.Control(config.RoleInvoiceCollect, "departments"))
	if dept == nil || dept["type"] != "department" {
		t.Fatalf("物资所属部门控件缺失或类型不对: %#v", dept)
	}

	name := formItem(t, form, cfg.Control(config.RoleInvoiceCollect, "name"))
	if name == nil {
		t.Fatal("「名称」控件未预填")
	}
	if name["type"] != "input" {
		t.Errorf("「名称」类型 = %v，应为 input", name["type"])
	}
	if name["value"] != "QWER-采购审批" {
		t.Errorf("「名称」= %v，应为 QWER-采购审批（取费用明细名称，不是项目名称）", name["value"])
	}
	if len(notes) != 3 {
		t.Errorf("说明条数 = %d，want 3: %#v", len(notes), notes)
	}
}

// TestInvoiceNameMultiDetails 多条费用明细 → 名称拼成一个值（去重、去空）。
func TestInvoiceNameMultiDetails(t *testing.T) {
	cfg := &config.Config{Dict: config.DefaultDict()}
	got := invoiceName(cfg, &purchaseInfo{Items: []purchaseItem{
		{Name: "QWER"}, {Name: " 摩擦轮 "}, {Name: "QWER"}, {Name: ""},
	}})
	if got != "QWER,摩擦轮-采购审批" {
		t.Errorf("名称 = %q，应为 QWER,摩擦轮-采购审批", got)
	}
}

// TestBuildInvoiceFormNameOptional 「名称」是选填：拿不到费用明细名称、或后缀被配成空串时，
// 宁可不写该控件，也不写「-采购审批」这种半截值。
func TestBuildInvoiceFormNameOptional(t *testing.T) {
	cfg := &config.Config{}
	cfg.Dict = config.DefaultDict()
	id := cfg.Control(config.RoleInvoiceCollect, "name")

	// ① 费用明细没有名称 → 不写（注意：项目名称有值也不顶用）
	form, _ := buildInvoiceForm(cfg, &purchaseInfo{
		ProjectGroup: "视觉组", ProjectName: "测试", ApplicantID: "u1"}, nil)
	if formItem(t, form, id) != nil {
		t.Error("费用明细名称为空时不应写「名称」")
	}
	if got := invoiceName(cfg, &purchaseInfo{ProjectName: "测试", Items: []purchaseItem{{Name: "  "}}}); got != "" {
		t.Errorf("空白明细名称应返回空串，实际 %q", got)
	}

	// ② 后缀配成空串 → 只填明细名称，不加后缀
	cfg.Dict.PurchaseToInvoice.NameSuffix = ""
	form2, _ := buildInvoiceForm(cfg, &purchaseInfo{
		ProjectGroup: "视觉组", Items: []purchaseItem{{Name: "QWER"}}}, nil)
	name2 := formItem(t, form2, id)
	if name2 == nil || name2["value"] != "QWER" {
		t.Errorf("后缀为空时「名称」应为 QWER，实际 %#v", name2)
	}
	if got := invoiceName(cfg, &purchaseInfo{Items: []purchaseItem{{Name: "QWER"}}}); got != "QWER" {
		t.Errorf("后缀为空时名称 = %q，应为 QWER", got)
	}
}

// TestLedgerAmount 金额口径：采购表单里的「金额」是**单价**，流水行要写 单价 × 数量。
// 用户 2026-09-21 实测案例：焊锡 31.45 × 2 → 62.90（原来只写了 31.45）。
func TestLedgerAmount(t *testing.T) {
	cases := []struct {
		name string
		it   purchaseItem
		want float64
	}{
		{"单价×数量", purchaseItem{Amount: 31.45, Qty: 2}, 62.9},
		{"数量为 1", purchaseItem{Amount: 48.4, Qty: 1}, 48.4},
		{"数量缺失按 1 件", purchaseItem{Amount: 27.6}, 27.6},
		{"数量为 0 按 1 件", purchaseItem{Amount: 27.6, Qty: 0}, 27.6},
		{"数量为负按 1 件", purchaseItem{Amount: 27.6, Qty: -3}, 27.6},
		{"整数数量", purchaseItem{Amount: 23, Qty: 4}, 92},
	}
	for _, c := range cases {
		if got := ledgerAmount(c.it); got != c.want {
			t.Errorf("%s: ledgerAmount(%+v) = %v, want %v", c.name, c.it, got, c.want)
		}
	}

	// 流水行的「🔗 金额」必须落到乘积上
	cfg := &config.Config{}
	cfg.Dict = config.DefaultDict()
	info := &purchaseInfo{ProjectGroup: "硬件组", ProjectName: "耗材"}
	f := buildLedgerFields(cfg, info, purchaseItem{Name: "焊锡", Amount: 31.45, Qty: 2}, "", "", 0)
	if got := f[cfg.Field("ledger", "amount")]; got != 62.9 {
		t.Errorf("流水行金额 = %v, want 62.9（单价×数量）", got)
	}
}
