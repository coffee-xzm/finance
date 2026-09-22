package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

func flowCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Dict = config.DefaultDict()
	return cfg
}

// widget 造一个"审批实例里的控件"（值与线上实例同形态）。
func widget(id, name, typ string, value any) feishu.FormWidget {
	raw, _ := json.Marshal(value)
	return feishu.FormWidget{ID: id, Name: name, Type: typ, Value: raw}
}

// TestParseFlowForm 锁住「27-流水登记」的表单解析口径：
// 单选给的是**选项内部 value**（要翻译成文字）、日期是 RFC3339、截图是临时直链数组。
func TestParseFlowForm(t *testing.T) {
	cfg := flowCfg()
	C := func(k string) string { return cfg.Control(config.RoleLedgerRegister, k) }

	// 转入：单选值 = 选项 value；金额在「转账金额」里
	ws := []feishu.FormWidget{
		widget(C("date"), "转账日期", "date", "2026-09-21T15:04:05+08:00"),
		widget(C("kind"), "类型", "radioV2", "m3n27dc0-euksyzgrrz-0"),
		widget(C("transfer_amount"), "转账金额", "amount", 1234.5),
		widget(C("expense_amount"), "支出金额", "amount", nil),
		widget(C("source"), "金额来源", "radioV2", "m2xek5tw-pi5yii5a0s7-1"),
		widget(C("destination"), "金额去向", "radioV2", "m3n294p6-19178gxt9m4-0"),
		widget(C("screenshot"), "转账截图", "attachmentV2", []string{"https://x/authcode/?code=1"}),
		widget(C("note"), "备注", "textarea", "八月的耗材"),
	}
	f := parseFlowForm(cfg, ws)
	if f.Kind != "转入" {
		t.Errorf("类型 = %q，选项 value 应被翻译成「转入」", f.Kind)
	}
	if f.Transfer != 1234.5 || f.Expense != 0 {
		t.Errorf("金额解析错误: 转账=%v 支出=%v", f.Transfer, f.Expense)
	}
	if f.Source != "竞赛经费" {
		t.Errorf("金额来源 = %q，应为竞赛经费", f.Source)
	}
	if f.Destination != "差旅垫付" {
		t.Errorf("金额去向 = %q，应为差旅垫付", f.Destination)
	}
	if len(f.Screenshots) != 1 {
		t.Errorf("转账截图 = %v", f.Screenshots)
	}
	if f.Note != "八月的耗材" {
		t.Errorf("备注 = %q", f.Note)
	}
	wantMS, _ := time.Parse(time.RFC3339, "2026-09-21T15:04:05+08:00")
	if f.DateMS != wantMS.UnixMilli() {
		t.Errorf("转账日期 = %d，应为 %d（RFC3339 → 毫秒）", f.DateMS, wantMS.UnixMilli())
	}
	// 转入 → 金额取转账金额；方向词映射成收支表的「收入」
	if f.Amount() != 1234.5 {
		t.Errorf("Amount() = %v，转入应取转账金额", f.Amount())
	}
	if f.Direction() != "收入" {
		t.Errorf("Direction() = %q，转入应映射成「收入」", f.Direction())
	}

	// 支出：金额取「支出金额」，方向词 = 支出
	f2 := parseFlowForm(cfg, []feishu.FormWidget{
		widget(C("kind"), "类型", "radioV2", "m3n27dc0-gpddu5bqtma-0"),
		widget(C("transfer_amount"), "转账金额", "amount", 999),
		widget(C("expense_amount"), "支出金额", "amount", 48.4),
	})
	if f2.Kind != "支出" || f2.Amount() != 48.4 || f2.Direction() != "支出" {
		t.Errorf("支出解析错误: kind=%q amount=%v dir=%q", f2.Kind, f2.Amount(), f2.Direction())
	}
}

// TestBuildFlowRegisterForm 代建登记单的预填：类型=支出、支出金额=单价×数量、
// 转账日期=采购完成时间（RFC3339）、备注=项目名+品名规格数量。
func TestBuildFlowRegisterForm(t *testing.T) {
	cfg := flowCfg()
	C := func(k string) string { return cfg.Control(config.RoleLedgerRegister, k) }
	info := &purchaseInfo{ProjectName: "耗材（接口焊锡洗板水）"}
	it := purchaseItem{Name: "焊锡", Qty: 2, Amount: 31.45}

	// 转账日期：只有"当天"才预填（表单对过去的日期报 out of range）——
	// 用 time.Now() 的毫秒值来测"当天"分支。
	form, notes := buildFlowRegisterForm(cfg, info, it, time.Now().UnixMilli())
	if len(form) != 4 || len(notes) != 4 {
		t.Fatalf("应预填 4 项（类型/支出金额/日期/备注），实际 %d: %#v", len(form), form)
	}
	kind := formItem(t, form, C("kind"))
	if kind == nil || kind["type"] != "radioV2" {
		t.Fatalf("类型控件缺失: %#v", kind)
	}
	if kind["value"] != cfg.OptionValue(config.RoleLedgerRegister, "类型", "支出") {
		t.Errorf("类型应预填「支出」的选项 value，实际 %v", kind["value"])
	}
	exp := formItem(t, form, C("expense_amount"))
	if exp == nil || exp["type"] != "amount" || exp["value"] != 62.9 || exp["currency"] != "CNY" {
		t.Errorf("支出金额应预填 31.45×2=62.9：%#v", exp)
	}
	d := formItem(t, form, C("date"))
	if d == nil || d["type"] != "date" {
		t.Fatalf("转账日期控件缺失: %#v", d)
	}
	ms := dateValueMS(mustJSON(d["value"]))
	if !sameDay(time.UnixMilli(ms), time.Now()) {
		t.Errorf("转账日期 = %d，应为采购完成时间（当天）", ms)
	}
	if n := formItem(t, form, C("note")); n == nil || n["value"] != "耗材（接口焊锡洗板水） ｜ 焊锡 ×2" {
		t.Errorf("备注 = %#v", n)
	}
	// 金额来源/去向/截图是登记人的判断，不预填
	if formItem(t, form, C("source")) != nil || formItem(t, form, C("destination")) != nil ||
		formItem(t, form, C("screenshot")) != nil {
		t.Error("金额来源/金额去向/转账截图不应预填")
	}

	// 不是当天（例如采购是昨天通过的）→ 不预填日期，避免 1390001 out of range
	old, _ := time.Parse(time.RFC3339, "2026-09-21T15:04:05+08:00")
	form2, _ := buildFlowRegisterForm(cfg, info, it, old.UnixMilli())
	if formItem(t, form2, C("date")) != nil {
		t.Error("采购完成时间不是当天时不应预填转账日期（表单会拒）")
	}
	if formItem(t, form2, C("expense_amount")) == nil || formItem(t, form2, C("note")) == nil {
		t.Error("不预填日期时其它项仍要预填")
	}
}

// TestDropFormField / TestIsDateRangeErr 自愈路径：日期被表单拒 → 去掉该项重试。
func TestDropFormField(t *testing.T) {
	form := []map[string]any{{"id": "a"}, {"id": "b"}, {"id": "c"}}
	got := dropFormField(form, "b")
	if len(got) != 2 || got[0]["id"] != "a" || got[1]["id"] != "c" {
		t.Errorf("dropFormField = %#v", got)
	}
	if len(dropFormField(form, "")) != 3 {
		t.Error("空 id 不应改动表单")
	}
	if !isDateRangeErr(fmt.Errorf("HTTP 400 code=1390001 msg=date: 2026-09-21 out of range")) {
		t.Error("应识别出日期超范围错误")
	}
	if isDateRangeErr(fmt.Errorf("HTTP 400 code=1390001 msg=其他错误")) {
		t.Error("不该把其它 1390001 当成日期错误")
	}
}

// TestLedgerOverwriteFields 覆盖写：登记数据优先，但**空值不覆盖**（不抹掉采购写的数据）。
func TestLedgerOverwriteFields(t *testing.T) {
	cfg := flowCfg()
	F := func(k string) string { return cfg.Field("ledger", k) }

	ff := &flowForm{
		Kind: "转入", Transfer: 500, DateMS: 1789800000000,
		Note: "九月的经费", Source: "大创经费", Destination: "物资购买",
		InstanceCode: "REG-1",
	}
	attach := []map[string]string{{"file_token": "tok1"}}
	f := ledgerOverwriteFields(cfg, ff, attach)

	if f[F("direction")] != "收入" {
		t.Errorf("收支方向 = %v，转入应写「收入」", f[F("direction")])
	}
	if f[F("amount")] != 500.0 {
		t.Errorf("金额 = %v", f[F("amount")])
	}
	if f[F("occurred_at")] != int64(1789800000000) {
		t.Errorf("发生日期 = %v", f[F("occurred_at")])
	}
	if f[F("note")] != "九月的经费" || f[F("amount_source")] != "大创经费" || f[F("amount_dest")] != "物资购买" {
		t.Errorf("备注/来源/去向 = %v / %v / %v", f[F("note")], f[F("amount_source")], f[F("amount_dest")])
	}
	if _, ok := f[F("screenshots")].([]map[string]string); !ok {
		t.Errorf("付款/收款截图应写附件数组，实际 %#v", f[F("screenshots")])
	}
	if link, ok := f[F("flow_id")].(map[string]string); !ok || link["link"] == "" {
		t.Errorf("流水审批ID 应写 {link,text}，实际 %#v", f[F("flow_id")])
	}

	// 登记人没填的项 → 不出现在覆盖字段里
	bare := ledgerOverwriteFields(cfg, &flowForm{Kind: "支出", Expense: 0, InstanceCode: "REG-2"}, nil)
	if _, ok := bare[F("note")]; ok {
		t.Error("备注为空时不应覆盖")
	}
	if _, ok := bare[F("amount")]; ok {
		t.Error("金额为 0 时不应覆盖（别把采购写的金额抹掉）")
	}
	if _, ok := bare[F("occurred_at")]; ok {
		t.Error("日期为空时不应覆盖")
	}
	if bare[F("direction")] != "支出" {
		t.Errorf("类型有值时应写方向，实际 %v", bare[F("direction")])
	}
}

// TestOptionTextFallback 单选值不在字典里时原样返回（有的表单直接给文字，
// 也让"字典没跟上表单改选项"这件事不至于丢值）。
func TestOptionTextFallback(t *testing.T) {
	cfg := flowCfg()
	if got := optionText(cfg, "类型", mustJSON("转入")); got != "转入" {
		t.Errorf("未知选项值应原样返回，实际 %q", got)
	}
	if got := optionText(cfg, "类型", mustJSON(map[string]any{"text": "转入"})); got != "转入" {
		t.Errorf("对象形态应取 text，实际 %q", got)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// TestPickLedgerRow 认行口径（notify 模式的核心）：备注优先、金额兜底、
// 命中多条**报错而不是随便挑**。
func TestPickLedgerRow(t *testing.T) {
	rows := []ledgerRowView{
		{RecordID: "row1", Note: "耗材 ｜ 焊锡 ×2", Amount: 62.9, Direction: "支出"},
		{RecordID: "row2", Note: "耗材 ｜ 洗板水 ×1", Amount: 27.6, Direction: "支出"},
		{RecordID: "row3", Note: "已登记过的行", Amount: 5, Direction: "支出", FlowID: `[{"link":"x"}]`},
	}
	// ① 备注精确命中
	if id, how, err := pickLedgerRow(rows, &flowForm{Kind: "支出", Expense: 62.9, Note: "耗材 ｜ 焊锡 ×2"}, ""); err != nil || id != "row1" || how != "备注" {
		t.Errorf("备注命中失败: %s %s %v", id, how, err)
	}
	// ② 备注带空白也应命中（登记人手抖多打个空格）
	if id, _, err := pickLedgerRow(rows, &flowForm{Kind: "支出", Expense: 27.6, Note: "  耗材 ｜ 洗板水 ×1  "}, ""); err != nil || id != "row2" {
		t.Errorf("备注去空白后应命中 row2，实际 %s %v", id, err)
	}
	// ③ 备注改了（没命中）→ 用金额兜底
	if id, how, err := pickLedgerRow(rows, &flowForm{Kind: "支出", Expense: 27.6, Note: "我改过备注"}, ""); err != nil || id != "row2" || how != "金额" {
		t.Errorf("金额兜底失败: %s %s %v", id, how, err)
	}
	// ④ 已经登记过的行不能被金额兜底再匹配一次
	if id, _, err := pickLedgerRow(rows, &flowForm{Kind: "支出", Expense: 5, Note: ""}, ""); err != nil || id != "" {
		t.Errorf("已登记的行不该再被匹配: %s %v", id, err)
	}
	// ⑤ 同备注多条 + 金额也分不开 → 报错（宁可不写，也不要把钱记错行）
	dup := []ledgerRowView{
		{RecordID: "a", Note: "同一句备注", Amount: 10, Direction: "支出"},
		{RecordID: "b", Note: "同一句备注", Amount: 10, Direction: "支出"},
	}
	if _, _, err := pickLedgerRow(dup, &flowForm{Kind: "支出", Expense: 10, Note: "同一句备注"}, "REG-1"); err == nil {
		t.Error("同备注同金额多条时应报错而不是猜")
	}
	// ⑥ 同备注多条但金额不同 → 金额能收敛，选那一行
	dup2 := []ledgerRowView{
		{RecordID: "a", Note: "同一句备注", Amount: 10, Direction: "支出"},
		{RecordID: "b", Note: "同一句备注", Amount: 12, Direction: "支出"},
	}
	if id, how, err := pickLedgerRow(dup2, &flowForm{Kind: "支出", Expense: 12, Note: "同一句备注"}, ""); err != nil || id != "b" || how != "备注+金额" {
		t.Errorf("备注+金额收敛失败: %s %s %v", id, how, err)
	}
	// ⑦ 流水审批ID 里已经有这张登记单 → 直接命中（事件重放）
	rows2 := []ledgerRowView{{RecordID: "r", FlowID: `[{"link":"...instanceId%3DABC-123"}]`}}
	if id, how, _ := pickLedgerRow(rows2, &flowForm{}, "ABC-123"); id != "r" || how != "流水审批ID" {
		t.Errorf("按流水审批ID命中失败: %s %s", id, how)
	}
}

// TestRegisterCodeFromFlowID 从「流水审批ID」的值里取回登记单 code。
func TestRegisterCodeFromFlowID(t *testing.T) {
	link := `[{"link":"https://applink.feishu.cn/client/mini_program/open?mode=appCenter&path=pc%2Fpages%2Fin-process%2Findex%3FinstanceId%3D4A4EDC04-F04B-4056-8A2A-95C0D007BE50","text":"查看流水登记"}]`
	if got := registerCodeFromFlowID(link); got != "4A4EDC04-F04B-4056-8A2A-95C0D007BE50" {
		t.Errorf("registerCodeFromFlowID = %q", got)
	}
	if got := registerCodeFromFlowID(""); got != "" {
		t.Errorf("空值应为空串，实际 %q", got)
	}
}

// TestBuildRegisterNotice 私信正文必须带上"要照抄的备注"（那是后面认行的键）。
func TestBuildRegisterNotice(t *testing.T) {
	cfg := flowCfg()
	info := &purchaseInfo{
		InstanceCode: "P-1", ProjectName: "耗材（接口焊锡洗板水）",
		Items: []purchaseItem{{Name: "焊锡", Qty: 2, Amount: 31.45}, {Name: "洗板水", Qty: 1, Amount: 27.6}},
	}
	text := buildRegisterNotice(cfg, info)
	for _, want := range []string{
		"采购审批「耗材（接口焊锡洗板水）」已通过，请为下面 2 笔流水各发一张「27-流水登记」",
		"1. 焊锡　金额 62.90（31.45 × 2）",
		"备注请照抄：耗材（接口焊锡洗板水） ｜ 焊锡 ×2",
		"2. 洗板水　金额 27.60（27.6 × 1）",
		"备注请照抄：耗材（接口焊锡洗板水） ｜ 洗板水 ×1",
		"类型选「支出」",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("私信正文缺少 %q：\n%s", want, text)
		}
	}
}

// TestDraftStateFor draft_state 是补漏扫描的幂等锚点，口径不能含糊。
func TestDraftStateFor(t *testing.T) {
	if got := draftStateFor("notify", "", ""); got != "awaiting_register" {
		t.Errorf("notify 成功应为 awaiting_register，实际 %q", got)
	}
	if got := draftStateFor("notify", "", "230013"); got != "failed" {
		t.Errorf("私信失败应记 failed（好让补漏重试），实际 %q", got)
	}
	if got := draftStateFor("draft", "", ""); got != "awaiting_applicant" {
		t.Errorf("draft 模式应为 awaiting_applicant，实际 %q", got)
	}
	if got := draftStateFor("", "INV-1", ""); got != "awaiting_applicant" {
		t.Errorf("旧流程（直接开票）应为 awaiting_applicant，实际 %q", got)
	}
}
