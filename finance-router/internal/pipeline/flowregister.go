// flowregister 实现「27-流水登记」这一步（用户 2026-09-21 定的新流程，
// 见 docs/30-review/33 §12.15）。
//
// 流程：
//
//	采购审批通过
//	  → 写「27 - 收支表」（每条费用明细一行，暂不开发票单）
//	  → **每条明细**给财务登记人开一张预填好的「27-流水登记」，退回到发起 + 私信通知
//	  → 登记人核对/补齐后提交（该表单没有审批人 → 提交即通过）
//	  → 用登记单的数据**覆盖**那一行流水（登记数据为准），并写「流水审批ID」
//	  → 该采购**所有明细**都登记完成后，才给采购提交人开「27发票收集」（带付款截图）
//
// 两条硬约束：
//   - 覆盖写是**显式覆盖**（不走"只填空"策略）——用户明确"以登记数据为主"；
//   - 幂等锚点是流水行的**「流水审批ID」列**（= 登记实例 code）：
//     事件重放时先按它找行，找到就更新、找不到才新建，所以重复事件不会插出重复行。
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/store"
)

// FlowRegisterOptions 配置一次「27-流水登记」结算。
type FlowRegisterOptions struct {
	CfgPath  string
	Instance string // 登记单实例 code
	DryRun   bool
	Force    bool // 忽略本地已完成标记，重跑覆盖
}

// flowForm 是「27-流水登记」解析出来的值。
type flowForm struct {
	Kind         string   // 转入 / 支出
	Transfer     float64  // 转账金额
	Expense      float64  // 支出金额
	DateMS       int64    // 转账日期
	Note         string   // 备注
	Source       string   // 金额来源
	Destination  string   // 金额去向
	Screenshots  []string // 转账截图的临时直链（24h）
	ApplicantID  string   // 登记人 user_id
	ApplicantOID string   // 登记人 open_id
	InstanceCode string
}

// Amount 返回这张登记单该写进「🔗 金额」的数：
// 转入 → 转账金额；支出 → 支出金额（缺失时用另一个兜底，不要把金额抹成 0）。
func (f *flowForm) Amount() float64 {
	if f.Kind == "转入" {
		if f.Transfer != 0 {
			return f.Transfer
		}
		return f.Expense
	}
	if f.Expense != 0 {
		return f.Expense
	}
	return f.Transfer
}

// Direction 把登记单的「类型」映射成收支表「收支方向」的选项值。
//
// ★ 登记表单写的是「转入」，而收支表的选项是「收入」——两者是同一个意思，
// 这里做映射（用户 2026-09-21 问的"余额公式是否同时支持转入和支出"，见
// docs/30-review/33 §12.15：公式里收入是 `+收入和`，本来就支持）。
func (f *flowForm) Direction() string {
	if f.Kind == "转入" {
		return "收入"
	}
	return "支出"
}

// parseFlowForm 从登记审批的表单里抽出要用的值。
//
// 控件先按 **id** 匹配（config 字典 controls.ledger_register），id 对不上时按控件名兜底，
// 这样用户改个控件 id 也不会静默丢值。
func parseFlowForm(cfg *config.Config, ws []feishu.FormWidget) *flowForm {
	f := &flowForm{}
	get := func(key, name string) (feishu.FormWidget, bool) {
		id := cfg.Control(config.RoleLedgerRegister, key)
		for _, w := range ws {
			if id != "" && w.ID == id {
				return w, true
			}
		}
		for _, w := range ws {
			if name != "" && strings.Contains(w.Name, name) {
				return w, true
			}
		}
		return feishu.FormWidget{}, false
	}

	if w, ok := get("date", "转账日期"); ok {
		f.DateMS = dateValueMS(w.Value)
	}
	if w, ok := get("kind", "类型"); ok {
		f.Kind = optionText(cfg, "类型", w.Value)
	}
	if w, ok := get("transfer_amount", "转账金额"); ok {
		f.Transfer = floatVal(w.Value)
	}
	if w, ok := get("expense_amount", "支出金额"); ok {
		f.Expense = floatVal(w.Value)
	}
	if w, ok := get("source", "金额来源"); ok {
		f.Source = optionText(cfg, "金额来源", w.Value)
	}
	if w, ok := get("destination", "金额去向"); ok {
		f.Destination = optionText(cfg, "金额去向", w.Value)
	}
	if w, ok := get("screenshot", "转账截图"); ok {
		f.Screenshots = w.AttachmentURLs()
	}
	if w, ok := get("note", "备注"); ok {
		f.Note = strVal(w.Value)
	}
	return f
}

// optionText 把单选控件的值翻译成**选项文字**。
//
// 审批实例里 radioV2 的值是选项的内部 value（形如 `m3n27dc0-euksyzgrrz-0`），
// 用 config 字典反向查出文字；查不到就原样返回（有的表单直接给文字）。
func optionText(cfg *config.Config, field string, raw json.RawMessage) string {
	s := strVal(raw)
	if s == "" {
		// 兜底 1：值可能是对象（{text|name|value:...}）
		var obj map[string]any
		if json.Unmarshal(raw, &obj) == nil {
			for _, k := range []string{"text", "name", "value"} {
				if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
					s = strings.TrimSpace(v)
					break
				}
			}
		}
	}
	if s == "" {
		// 兜底 2：值可能是对象数组
		for _, seg := range jsonRawToMaps(raw) {
			for _, k := range []string{"text", "name", "value"} {
				if v, ok := seg[k].(string); ok && strings.TrimSpace(v) != "" {
					s = strings.TrimSpace(v)
					break
				}
			}
			if s != "" {
				break
			}
		}
	}
	if s == "" {
		return ""
	}
	if m, ok := cfg.Dict.Options[config.RoleLedgerRegister]; ok {
		if opts, ok := m[field]; ok {
			for text, val := range opts {
				if val == s {
					return text
				}
			}
		}
	}
	return s
}

// dateValueMS 把日期控件的值读成 Unix 毫秒。
// 审批实例里可能是 RFC3339 字符串（新的 date 控件）或毫秒数（老控件）。
func dateValueMS(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
		return 0
	}
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	return 0
}

// buildFlowRegisterForm 组装代建「27-流水登记」的预填表单（纯函数）。
//
// 预填的是**确定**的部分：类型=支出、支出金额=采购金额、转账日期=采购完成时间、
// 备注=项目名+品名/规格/数量。「金额来源 / 金额去向 / 转账截图」是登记人的判断，
// 不预填（预填错了比留空更容易误导）。
func buildFlowRegisterForm(cfg *config.Config, info *purchaseInfo, it purchaseItem,
	finishMS int64) (form []map[string]any, notes []string) {

	form = []map[string]any{}
	ctrl := func(k string) string { return cfg.Control(config.RoleLedgerRegister, k) }

	if id := ctrl("kind"); id != "" {
		if v := cfg.OptionValue(config.RoleLedgerRegister, "类型", "支出"); v != "" {
			form = append(form, map[string]any{"id": id, "type": "radioV2", "value": v})
			notes = append(notes, "类型 = 支出")
		}
	}
	if id := ctrl("expense_amount"); id != "" {
		if amt := ledgerAmount(it); amt != 0 {
			form = append(form, map[string]any{"id": id, "type": "amount", "value": amt, "currency": "CNY"})
			notes = append(notes, fmt.Sprintf("支出金额 = %.2f", amt))
		}
	}
	// ★ 转账日期只在**当天**才预填：该表单的日期控件带范围校验（dateCheckType），
	//   实测把昨天的日期塞进去会被拒：`1390001 date: 2026-09-21 out of range`
	//   （2026-09-22 建单时踩到）。而且"转账日期"本来是登记人知道、我们不知道的信息，
	//   留空让他填比猜一个错的好。
	if id := ctrl("date"); id != "" && finishMS > 0 {
		t := time.UnixMilli(finishMS)
		if sameDay(t, time.Now()) {
			form = append(form, map[string]any{"id": id, "type": "date", "value": t.Format(time.RFC3339)})
			notes = append(notes, "转账日期 = "+t.Format("2006-01-02"))
		} else {
			notes = append(notes, fmt.Sprintf("转账日期留空（采购完成于 %s，不是今天；表单对过去的日期报 out of range）",
				t.Format("2006-01-02")))
		}
	}
	if id := ctrl("note"); id != "" {
		if n := ledgerNote(info, it); n != "" {
			form = append(form, map[string]any{"id": id, "type": "textarea", "value": n})
			notes = append(notes, "备注 = "+n)
		}
	}
	return form, notes
}

// createFlowRegisterDraft 代建一张「27-流水登记」并退回到发起人。
//
// uuid 由调用方给（正常是 `<采购实例code>-<明细序号>`）：同一明细重复处理会撞
// 60012（UUID 冲突），所以"重跑到同一明细"必须换一个 uuid（见 RunPurchase 里的重建分支）。
func createFlowRegisterDraft(ctx context.Context, cfg *config.Config, client *feishu.Client,
	registerCode string, info *purchaseInfo, it purchaseItem, idx int, finishMS int64,
	uuid string) (string, error) {

	userID := cfg.RegisterUserID()
	if userID == "" {
		return "", fmt.Errorf("config 缺 flow_register.user_id（流水登记人）")
	}
	form, notes := buildFlowRegisterForm(cfg, info, it, finishMS)
	for _, n := range notes {
		fmt.Printf("  预填 %s\n", n)
	}
	req := feishu.CreateInstanceRequest{
		ApprovalCode:  registerCode,
		UserID:        userID,
		Form:          form,
		UUID:          uuid,
		AllowResubmit: true,
	}
	newCode, err := client.CreateInstance(ctx, req)
	if err != nil && isDateRangeErr(err) {
		// 自愈：日期控件被表单的范围校验拒了 → 去掉该项再试（登记人自己填日期）
		req.Form = dropFormField(req.Form, cfg.Control(config.RoleLedgerRegister, "date"))
		fmt.Println("  ↻ 转账日期被表单拒了（out of range），去掉该预填项重试")
		newCode, err = client.CreateInstance(ctx, req)
	}
	if err != nil && strings.Contains(err.Error(), "60012") {
		// UUID 冲突 = 这个幂等键**已经被用过**（例如上一张单建出来了但没退回去，
		// 本地换了 uuid 重建却撞上了历史键）。换一个带时间戳的 uuid 再试一次：
		// 宁可多一张作废单，也不要让这笔采购永远补不回来。
		req.UUID = fmt.Sprintf("%s-r%d", uuid, time.Now().Unix())
		fmt.Printf("  ↻ uuid %s 已被占用（60012），改用 %s 重试\n", uuid, req.UUID)
		newCode, err = client.CreateInstance(ctx, req)
	}
	if err != nil {
		return "", err
	}
	det, _, err := client.GetInstanceDetail(ctx, newCode)
	if err != nil {
		return newCode, fmt.Errorf("建单成功但读详情失败（需人工退回到发起）: %w", err)
	}
	var task *feishu.TaskItem
	for i := range det.TaskList {
		if strings.EqualFold(det.TaskList[i].Status, "PENDING") {
			task = &det.TaskList[i]
			break
		}
	}
	if task == nil {
		// ★ 实测（2026-09-21）：「27-流水登记」的审批节点若配成**自动通过**，
		//   建单后实例立刻 APPROVED（timeline 只有 START + AUTO_PASS），
		//   既没有待办可退回（回退报 10112 no permission over task），
		//   登记人也永远拿不到可编辑的表单 —— "先填再撤回"就落不了地。
		//   正解：把该节点改成**真实审批人**（例如登记人本人），见 docs/33 §12.15。
		return newCode, fmt.Errorf("建单成功但审批定义的这个节点是「自动通过」，"+
			"没有可退回的待办任务（实例已 %s）→ 请把「27-流水登记」的审批节点改成真实审批人",
			det.Status)
	}
	if err := client.SpecifiedRollback(ctx, task.UserID, task.ID, []string{"START"},
		"请核对/补齐流水信息（转账日期、金额、截图、金额来源/去向）后提交"); err != nil {
		return newCode, fmt.Errorf("建单成功但退回失败（需人工退回）: %w", err)
	}
	return newCode, nil
}

// notifyRegistrant 私信登记人：有 N 张流水登记单要填。
func notifyRegistrant(ctx context.Context, cfg *config.Config, client *feishu.Client,
	info *purchaseInfo, codes []string, itemNames []string) error {

	userID := cfg.RegisterUserID()
	if userID == "" {
		return fmt.Errorf("config 缺 flow_register.user_id，无法通知登记人")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "【流水登记】采购审批「%s」已通过，已为你代建 %d 张「27-流水登记」：\n",
		info.ProjectName, len(codes))
	for i, c := range codes {
		name := ""
		if i < len(itemNames) {
			name = itemNames[i]
		}
		fmt.Fprintf(&b, "\n%d. %s\n%s", i+1, name, buildApprovalApplink(cfg, c))
	}
	b.WriteString("\n\n请核对/补齐（转账日期、金额、截图、金额来源/去向）后提交；" +
		"提交即通过，之后系统会给采购提交人开「27发票收集」。")
	return sendWithAdminFallback(ctx, cfg, client, userID, b.String(),
		fmt.Sprintf("采购「%s」的代建流水登记单已创建", info.ProjectName))
}

// notifyPendingRegister 是 notify 模式的核心动作：**不代建审批**，只把待登记内容
// 私信给登记人，由他自己在飞书里发一张「27-流水登记」。
//
// 为什么这么设计（用户 2026-09-22 选定）：「27-流水登记」的审批节点是「自动通过」，
// 用 API 代建的话实例会立刻 APPROVED（timeline 只有 START→AUTO_PASS），既没有待办
// 可退回（10112 no permission over task），登记人也拿不到可编辑的表单 ——
// "先填再撤回"落不了地。改成私信后：备注照抄即可，系统按**备注/金额**把登记数据
// 匹配回对应的流水行（见 matchLedgerRowForRegister）。
func notifyPendingRegister(ctx context.Context, cfg *config.Config, client *feishu.Client,
	info *purchaseInfo, ledgerIDs []string) error {

	userID := cfg.RegisterUserID()
	if userID == "" {
		return fmt.Errorf("config 缺 flow_register.user_id，无法通知登记人")
	}
	text := buildRegisterNotice(cfg, info)
	return sendWithAdminFallback(ctx, cfg, client, userID, text,
		fmt.Sprintf("采购「%s」通过后需要做 %d 笔流水登记（请提醒登记人）",
			info.ProjectName, len(info.Items)))
}

// buildRegisterNotice 是 notify 模式发给登记人的正文（纯函数，便于单测）：
// 每条明细给出金额与**要照抄的备注**（备注就是后面匹配流水行的键）。
func buildRegisterNotice(cfg *config.Config, info *purchaseInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【流水登记】采购审批「%s」已通过，请为下面 %d 笔流水各发一张「27-流水登记」：\n",
		info.ProjectName, len(info.Items))
	for i, it := range info.Items {
		fmt.Fprintf(&b, "\n%d. %s　金额 %.2f", i+1, it.Name, ledgerAmount(it))
		if it.Qty > 0 {
			fmt.Fprintf(&b, "（%s × %s）", trimNum(it.Amount), trimNum(it.Qty))
		}
		fmt.Fprintf(&b, "\n   备注请照抄：%s", ledgerNote(info, it))
	}
	fmt.Fprintf(&b, "\n\n采购审批：%s\n", buildApprovalApplink(cfg, info.InstanceCode))
	b.WriteString("\n填法：类型选「支出」，金额照上面填；转账日期填实际转账日；上传转账截图；" +
		"金额来源/金额去向按实际选。\n" +
		"提交即通过 —— 系统靠「备注」把这笔登记对到「27 - 收支表」对应行并覆盖，" +
		"该采购全部明细登记完成后会给采购提交人开「27发票收集」。")
	return b.String()
}

// PreviewRegisterNotice 只读预览：notify 模式下会发给登记人的那段文字
// （运维入口 cmd/purchase -notice <采购实例code>，用来核对格式/重发前看一眼）。
func PreviewRegisterNotice(ctx context.Context, cfg *config.Config, instanceCode string) (string, error) {
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	det, _, err := client.GetInstanceDetail(ctx, instanceCode)
	if err != nil {
		return "", fmt.Errorf("读采购实例 %s: %w", short(instanceCode), err)
	}
	info, err := parsePurchase(det)
	if err != nil {
		return "", err
	}
	return buildRegisterNotice(cfg, info), nil
}

// trimNum 打印数量/单价时不带多余小数位。
func trimNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// RunFlowRegister 处理一张**已通过**的「27-流水登记」：
// 覆盖流水行 → （该采购所有明细都登记完时）开票 + 通知提交人。
func RunFlowRegister(ctx context.Context, opts FlowRegisterOptions) error {
	cfgPath := opts.CfgPath
	if cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			return err
		}
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if opts.Instance == "" {
		return fmt.Errorf("必须指定流水登记实例 code")
	}
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()
	flowBase, ok := cfg.Base(config.BaseFlow)
	ledgerTbl := cfg.Table(config.BaseFlow, "ledger")
	if !ok || ledgerTbl == "" || flowBase.AppToken == "" {
		return fmt.Errorf("配置缺少 flow base（feishu.bitable.bases.flow）")
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	det, _, err := client.GetInstanceDetail(ctx, opts.Instance)
	if err != nil {
		return fmt.Errorf("读登记实例 %s: %w", short(opts.Instance), err)
	}
	if !strings.EqualFold(det.Status, "APPROVED") {
		return fmt.Errorf("登记实例 %s 状态是 %s（只处理 APPROVED）", short(opts.Instance), det.Status)
	}
	ws, err := feishu.ParseForm(det.Form)
	if err != nil {
		return fmt.Errorf("解析登记表单: %w", err)
	}
	ff := parseFlowForm(cfg, ws)
	ff.ApplicantID = det.UserID
	ff.ApplicantOID = det.OpenID
	ff.InstanceCode = det.InstanceCode

	// 目标流水行：① 本地映射 ②「流水审批ID」反查 ③ 备注精确匹配 ④ 金额匹配
	ledgerRecID, purchaseCode := "", ""
	if r, ok, err := db.GetFlowRegister(ctx, opts.Instance); err == nil && ok {
		ledgerRecID, purchaseCode = r.LedgerRecordID, r.PurchaseInstanceCode
		if r.State == store.FlowRegisterApplied && !opts.Force {
			fmt.Printf("  = 登记单 %s 已经覆盖过流水行（%s），跳过\n", short(opts.Instance), ledgerRecID)
			return nil
		}
	}
	if ledgerRecID == "" && purchaseCode != "" {
		// 本地有映射但没记到行 → 按明细序号对采购的流水行
		if p, ok, _ := db.GetPurchase(ctx, purchaseCode); ok {
			if it, ok2, _ := db.GetFlowRegister(ctx, opts.Instance); ok2 {
				ledgerRecID = ledgerRecordIDAt(p.LedgerRecordIDs, it.ItemIndex-1)
			}
		}
	}
	how := ""
	if ledgerRecID == "" {
		// ★ notify 模式（登记人自己开单）没有本地映射，靠**备注/金额**认行：
		//   我们发私信时把要照抄的备注给到他，备注就是天然的对账键。
		id, h, err := matchLedgerRowForRegister(ctx, cfg, client, flowBase.AppToken, ledgerTbl, ff, opts.Instance)
		if err != nil {
			return err
		}
		ledgerRecID, how = id, h
	}
	if ledgerRecID != "" && purchaseCode == "" {
		if p, ok, _ := db.PurchaseByLedgerRecord(ctx, ledgerRecID); ok {
			purchaseCode = p.PurchaseInstanceCode
		}
	}
	fmt.Printf("▶ 流水登记 %s：类型=%s 金额=%.2f 来源=%s 去向=%s → 流水行 %s%s\n",
		short(opts.Instance), orDash(ff.Kind), ff.Amount(), orDash(ff.Source), orDash(ff.Destination),
		orDash(short(ledgerRecID)), func() string {
			if how == "" {
				return ""
			}
			return "（按" + how + "匹配）"
		}())

	// 转账截图 → 多维表格附件（临时直链 24h 失效，必须转存）
	var attach []map[string]string
	if len(ff.Screenshots) > 0 {
		attach, err = uploadScreenshotsToBitable(ctx, client, flowBase.AppToken, ff.Screenshots, opts.Instance)
		if err != nil {
			fmt.Printf("  ⚠ 转账截图转存失败（不影响其它字段）: %v\n", err)
		} else {
			fmt.Printf("  ✓ 转账截图转存 %d 张\n", len(attach))
		}
	}

	fields := ledgerOverwriteFields(cfg, ff, attach)
	if opts.DryRun {
		b, _ := json.Marshal(fields)
		fmt.Printf("  [dry-run] %s\n", trunc(string(b), 400))
		return nil
	}
	if ledgerRecID != "" {
		if err := client.UpdateBitableRecord(ctx, flowBase.AppToken, ledgerTbl, ledgerRecID, fields); err != nil {
			_ = db.SetFlowRegisterState(ctx, opts.Instance, store.FlowRegisterFailed, err.Error())
			return fmt.Errorf("覆盖流水行 %s 失败: %w%s", ledgerRecID, err,
				writeDeniedHint(ctx, client, cfg, flowBase.AppToken, err))
		}
		fmt.Printf("  ✓ 已按登记数据覆盖流水行 %s（%d 个字段）\n", ledgerRecID, len(fields))
	} else {
		// 不是采购派生的（登记人自己提的流水）→ 新建一行。
		// 这行没有采购提交人，所以「关联人」= 登记人自己（可归因）。
		if fid := cfg.Field("ledger", "related_user"); fid != "" && ff.ApplicantOID != "" {
			fields[fid] = []map[string]any{{"id": ff.ApplicantOID}}
		}
		recID, err := client.CreateBitableRecord(ctx, flowBase.AppToken, ledgerTbl, fields)
		if err != nil {
			_ = db.SetFlowRegisterState(ctx, opts.Instance, store.FlowRegisterFailed, err.Error())
			return fmt.Errorf("新建流水行失败: %w%s", err, writeDeniedHint(ctx, client, cfg, flowBase.AppToken, err))
		}
		ledgerRecID = recID
		fmt.Printf("  ✓ 已新建流水行 %s（登记人自建流水，无采购来源）\n", recID)
	}
	if err := db.UpsertFlowRegister(ctx, store.FlowRegisterSync{
		InstanceCode:         opts.Instance,
		PurchaseInstanceCode: purchaseCode,
		LedgerRecordID:       ledgerRecID,
		State:                store.FlowRegisterApplied,
	}); err != nil {
		fmt.Printf("  ⚠ 本地留痕失败（不影响流水）: %v\n", err)
	}

	// 该采购的全部明细都登记完了 → 才开票
	if purchaseCode != "" {
		if err := maybeCreateInvoiceForPurchase(ctx, cfg, client, db, purchaseCode); err != nil {
			return err
		}
	}
	fmt.Printf("✓ 流水登记 %s 处理完成（流水行 %s）\n", short(opts.Instance), ledgerRecID)
	return nil
}

// isDateRangeErr 判断建单失败是不是"日期超出表单允许范围"。
func isDateRangeErr(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "out of range") || (strings.Contains(m, "date") && strings.Contains(m, "1390001"))
}

// dropFormField 从表单里去掉某个控件（用于"某项被表单拒了就退一步"）。
func dropFormField(form []map[string]any, id string) []map[string]any {
	if id == "" {
		return form
	}
	out := make([]map[string]any, 0, len(form))
	for _, f := range form {
		if v, _ := f["id"].(string); v == id {
			continue
		}
		out = append(out, f)
	}
	return out
}

// sameDay 判断两个时间是不是同一个自然日（本地时区）。
func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// ledgerOverwriteFields 组"按登记数据覆盖流水行"的字段。
//
// ★ 覆盖是显式的（不受 write_policy.only_fill_blank 限制）：用户明确"以登记数据为主"。
// 但**空值不覆盖** —— 登记人没填的项保持原样，否则会把采购写的好数据抹掉。
func ledgerOverwriteFields(cfg *config.Config, ff *flowForm, attach []map[string]string) map[string]any {
	F := func(k string) string { return cfg.Field("ledger", k) }
	f := map[string]any{}
	if ff.Kind != "" {
		if v := F("direction"); v != "" {
			f[v] = ff.Direction()
		}
	}
	if amt := ff.Amount(); amt != 0 {
		if v := F("amount"); v != "" {
			f[v] = amt
		}
	}
	if ff.DateMS > 0 {
		if v := F("occurred_at"); v != "" {
			f[v] = ff.DateMS
		}
	}
	if n := strings.TrimSpace(ff.Note); n != "" {
		if v := F("note"); v != "" {
			f[v] = n
		}
	}
	if ff.Source != "" {
		if v := F("amount_source"); v != "" {
			f[v] = ff.Source
		}
	}
	if ff.Destination != "" {
		if v := F("amount_dest"); v != "" {
			f[v] = ff.Destination
		}
	}
	if len(attach) > 0 {
		if v := F("screenshots"); v != "" {
			f[v] = attach
		}
	}
	if v := F("flow_id"); v != "" {
		f[v] = map[string]string{
			"link": buildApprovalApplink(cfg, ff.InstanceCode),
			"text": "查看流水登记",
		}
	}
	return f
}

// ledgerRowView 是一条流水行里"认行"要用的几个值。
type ledgerRowView struct {
	RecordID  string
	FlowID    string  // 流水审批ID 原文（含登记单 code）
	Note      string  // 🔗 备注
	Amount    float64 // 🔗 金额
	Direction string  // 收支方向
}

// listLedgerRows 读全表（只读）。收支表是给人看的表，量级小（几十~几百行）。
func listLedgerRows(ctx context.Context, cfg *config.Config, c *feishu.Client,
	appToken, tableID string) ([]ledgerRowView, error) {

	recs, err := c.SearchBitableRecords(ctx, appToken, tableID, nil, 500)
	if err != nil {
		return nil, err
	}
	F := func(k string) string { return cfg.Field("ledger", k) }
	out := make([]ledgerRowView, 0, len(recs))
	for i := range recs {
		f := recs[i].Fields
		out = append(out, ledgerRowView{
			RecordID:  recs[i].RecordID,
			FlowID:    string(f[F("flow_id")]),
			Note:      textOfField(f[F("note")]),
			Amount:    floatVal(f[F("amount")]),
			Direction: textOfField(f[F("direction")]),
		})
	}
	return out, nil
}

// matchLedgerRowForRegister 给一张（登记人自己开的）登记单找它该覆盖的流水行。
//
// 认行顺序（notify 模式下没有本地映射，必须靠数据本身）：
//
//	①「流水审批ID」里已经有这张登记单的 code（事件重放 / 上次部分成功）
//	② 备注**逐字相同**（我们私信里让他照抄的备注就是为此设计的）：唯一命中即用；
//	   多条命中 → 再用金额收敛
//	③ 金额相同 + 收支方向相同，且该行还没被登记过（流水审批ID 为空）：唯一命中即用
//	   （防"他改了备注"）
//	④ 都不中 → 返回空（调用方按"登记人自建流水"新建一行）
//
// 命中多条时**返回错误而不是随便挑一行**：宁可让人看一眼，也不要把钱记到错的行上。
func matchLedgerRowForRegister(ctx context.Context, cfg *config.Config, c *feishu.Client,
	appToken, tableID string, ff *flowForm, instanceCode string) (string, string, error) {

	rows, err := listLedgerRows(ctx, cfg, c, appToken, tableID)
	if err != nil {
		return "", "", fmt.Errorf("读收支表失败: %w", err)
	}
	return pickLedgerRow(rows, ff, instanceCode)
}

// pickLedgerRow 是 matchLedgerRowForRegister 的纯函数内核（便于单测）。
func pickLedgerRow(rows []ledgerRowView, ff *flowForm, instanceCode string) (string, string, error) {
	for _, r := range rows {
		if instanceCode != "" && strings.Contains(r.FlowID, instanceCode) {
			return r.RecordID, "流水审批ID", nil
		}
	}
	note := strings.TrimSpace(ff.Note)
	if note != "" {
		var hits []ledgerRowView
		for _, r := range rows {
			if strings.TrimSpace(r.Note) == note {
				hits = append(hits, r)
			}
		}
		if len(hits) == 1 {
			return hits[0].RecordID, "备注", nil
		}
		if len(hits) > 1 {
			// 同备注多条（同一笔采购多条明细、他复制了同样的备注）→ 用金额收敛
			var narrowed []ledgerRowView
			for _, r := range hits {
				if sameMoney(r.Amount, ff.Amount()) {
					narrowed = append(narrowed, r)
				}
			}
			if len(narrowed) == 1 {
				return narrowed[0].RecordID, "备注+金额", nil
			}
			return "", "", fmt.Errorf("备注「%s」匹配到 %d 条流水行（金额收敛后 %d 条），"+
				"无法确定该覆盖哪一行 —— 请人工核对（登记单 %s）",
				note, len(hits), len(narrowed), short(instanceCode))
		}
	}
	// 备注没命中：用"金额 + 方向 + 还没登记过"兜底
	var cands []ledgerRowView
	for _, r := range rows {
		if r.FlowID != "" && r.FlowID != "null" {
			continue // 已经被别的登记单覆盖过
		}
		if !sameMoney(r.Amount, ff.Amount()) {
			continue
		}
		if ff.Kind != "" && r.Direction != "" && r.Direction != ff.Direction() {
			continue
		}
		cands = append(cands, r)
	}
	if len(cands) == 1 {
		return cands[0].RecordID, "金额", nil
	}
	return "", "", nil
}

// sameMoney 金额比较（分位容差）。
func sameMoney(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.011
}

// allLedgerRowsRegistered 判断"这笔采购的流水行是否都已经有登记单了"
// （判据：流水行的「流水审批ID」非空 —— 覆盖时会写上去）。
func allLedgerRowsRegistered(ctx context.Context, cfg *config.Config, c *feishu.Client,
	appToken, tableID string, ledgerIDs []string) (bool, int, int, error) {

	if len(ledgerIDs) == 0 {
		return false, 0, 0, nil
	}
	rows, err := listLedgerRows(ctx, cfg, c, appToken, tableID)
	if err != nil {
		return false, 0, 0, err
	}
	byID := map[string]ledgerRowView{}
	for _, r := range rows {
		byID[r.RecordID] = r
	}
	done, total := 0, 0
	for _, id := range ledgerIDs {
		r, ok := byID[id]
		if !ok {
			continue // 行被人删了 → 不计入
		}
		total++
		if strings.TrimSpace(r.FlowID) != "" && r.FlowID != "null" {
			done++
		}
	}
	return total > 0 && done == total, done, total, nil
}

// uploadScreenshotsToBitable 把登记单的转账截图（临时直链）转存成多维表格附件。
func uploadScreenshotsToBitable(ctx context.Context, c *feishu.Client, appToken string,
	urls []string, tag string) ([]map[string]string, error) {

	var out []map[string]string
	for i, u := range urls {
		var buf bytes.Buffer
		res, err := c.DownloadTmpURL(ctx, u, &buf)
		if err != nil {
			return out, err
		}
		name := fmt.Sprintf("转账截图-%s-%d%s", short(tag), i+1, extByContentType(res.ContentType))
		tok, err := c.UploadMedia(ctx, appToken, "bitable_image", name, buf.Bytes())
		if err != nil {
			return out, err
		}
		out = append(out, map[string]string{"file_token": tok})
	}
	return out, nil
}

// maybeCreateInvoiceForPurchase 在**该采购的流水行都登记过**之后，给采购提交人
// 开「27发票收集」（预填 + 各登记单的付款截图），并私信通知。
//
// 判据：流水行的「流水审批ID」都非空（覆盖时写上去的）——notify 模式下没有
// 本地登记单映射，只能以表格为准。为什么要等全部：发票单是按**整笔采购**开的
// （「名称」列是所有明细名），少一行就开票，金额/截图都会缺。
func maybeCreateInvoiceForPurchase(ctx context.Context, cfg *config.Config, client *feishu.Client,
	db *store.DB, purchaseCode string) error {

	prev, ok, err := db.GetPurchase(ctx, purchaseCode)
	if err != nil {
		return err
	}
	if !ok {
		return nil // 不是本服务写的采购（人工录的），不掺和
	}
	if prev.InvoiceInstanceCode != "" {
		fmt.Printf("  = 发票单已代建过（%s），跳过\n", short(prev.InvoiceInstanceCode))
		return nil
	}
	flowBase, hasFlow := cfg.Base(config.BaseFlow)
	ledgerTbl := cfg.Table(config.BaseFlow, "ledger")
	if !hasFlow || ledgerTbl == "" {
		return fmt.Errorf("配置缺少 flow base")
	}
	ready, done, total, err := allLedgerRowsRegistered(ctx, cfg, client,
		flowBase.AppToken, ledgerTbl, prev.LedgerRecordIDs)
	if err != nil {
		return err
	}
	if !ready {
		fmt.Printf("  ⏳ 还有 %d/%d 行流水没登记 → 暂不开票\n", total-done, total)
		return nil
	}

	inv, ok := cfg.ApprovalByRole(config.RoleInvoiceCollect)
	if !ok || inv.Code == "" {
		return fmt.Errorf("配置里没有 role=invoice_collect 的审批")
	}
	det, _, err := client.GetInstanceDetail(ctx, purchaseCode)
	if err != nil {
		return fmt.Errorf("读采购实例 %s: %w", short(purchaseCode), err)
	}
	info, err := parsePurchase(det)
	if err != nil {
		return err
	}
	deptByName, _ := loadDepts(ctx, client, db, cfg)

	// 各登记单的转账截图 → 上传到审批系统换 file code，作为发票单的「付款记录」。
	// 登记单 code 从流水行的「流水审批ID」里取。
	rows, err := listLedgerRows(ctx, cfg, client, flowBase.AppToken, ledgerTbl)
	if err != nil {
		return err
	}
	byID := map[string]ledgerRowView{}
	for _, r := range rows {
		byID[r.RecordID] = r
	}
	var payTokens []string
	for _, id := range prev.LedgerRecordIDs {
		r, ok := byID[id]
		if !ok {
			continue
		}
		code := registerCodeFromFlowID(r.FlowID)
		if code == "" {
			continue
		}
		rdet, _, err := client.GetInstanceDetail(ctx, code)
		if err != nil {
			fmt.Printf("  ⚠ 读登记单 %s 失败（少一张付款截图）: %v\n", short(code), err)
			continue
		}
		rws, err := feishu.ParseForm(rdet.Form)
		if err != nil {
			continue
		}
		rff := parseFlowForm(cfg, rws)
		for i, u := range rff.Screenshots {
			tok, err := downloadAndUploadApprovalFile(ctx, client, u,
				fmt.Sprintf("付款截图-%s-%d", short(code), i+1))
			if err != nil {
				fmt.Printf("  ⚠ 付款截图转审批文件失败: %v\n", err)
				continue
			}
			payTokens = append(payTokens, tok)
		}
	}
	if len(payTokens) > 0 {
		fmt.Printf("  ✓ 付款截图 %d 张将预填进发票单\n", len(payTokens))
	} else {
		fmt.Println("  ⚠ 登记单里没有转账截图，发票单的「付款记录」留空由提交人补")
	}

	invoiceCode, err := createInvoiceDraft(ctx, cfg, client, db, deptByName, inv.Code, info, payTokens)
	if err != nil {
		_ = db.UpsertPurchase(ctx, store.PurchaseSync{
			PurchaseInstanceCode: purchaseCode, ApprovalCode: info.ApprovalCode,
			ApplicantUserID: info.ApplicantID, PurchaseStatus: info.Status,
			ProjectGroup: info.ProjectGroup, MirrorRecordID: prev.MirrorRecordID,
			LedgerRecordIDs: prev.LedgerRecordIDs, RequestRecordIDs: prev.RequestRecordIDs,
			FlowRegisterCodes:   prev.FlowRegisterCodes,
			InvoiceInstanceCode: invoiceCode, DraftState: "failed", LastError: err.Error(),
		})
		return fmt.Errorf("代建发票单失败: %w", err)
	}
	fmt.Printf("  ✓ 已代建发票单 %s 并退回发起人（%d/%d 行流水已登记）\n",
		short(invoiceCode), done, total)
	_ = db.UpsertPurchase(ctx, store.PurchaseSync{
		PurchaseInstanceCode: purchaseCode, ApprovalCode: info.ApprovalCode,
		ApplicantUserID: info.ApplicantID, PurchaseStatus: info.Status,
		ProjectGroup: info.ProjectGroup, MirrorRecordID: prev.MirrorRecordID,
		LedgerRecordIDs: prev.LedgerRecordIDs, RequestRecordIDs: prev.RequestRecordIDs,
		FlowRegisterCodes:   prev.FlowRegisterCodes,
		InvoiceInstanceCode: invoiceCode, DraftState: "awaiting_applicant",
	})
	_ = notifyApplicant(ctx, cfg, client, info, invoiceCode)
	return nil
}

// registerCodeFromFlowID 从流水行「流水审批ID」的值里取出登记单实例 code。
// 值形态是 [{"link":"...instanceId=<code>","text":"查看流水登记"}]。
func registerCodeFromFlowID(raw string) string {
	if raw == "" {
		return ""
	}
	// 优先从 applink 的 instanceId= 参数取
	for _, sep := range []string{"instanceId%3D", "instanceId="} {
		if i := strings.Index(raw, sep); i >= 0 {
			rest := raw[i+len(sep):]
			if j := strings.IndexAny(rest, "&\"\\"); j >= 0 {
				rest = rest[:j]
			}
			if len(rest) >= 8 {
				return rest
			}
		}
	}
	// 兜底：值里直接是实例 code（36 位 UUID）
	var m []string
	for _, seg := range strings.Split(raw, "\"") {
		if len(seg) == 36 && strings.Count(seg, "-") == 4 {
			m = append(m, seg)
		}
	}
	if len(m) > 0 {
		return m[0]
	}
	return ""
}

// downloadAndUploadApprovalFile 下载临时直链 → 上传到审批系统 → 返回 file code。
func downloadAndUploadApprovalFile(ctx context.Context, c *feishu.Client, url, name string) (string, error) {
	var buf bytes.Buffer
	res, err := c.DownloadTmpURL(ctx, url, &buf)
	if err != nil {
		return "", err
	}
	full := name + extByContentType(res.ContentType)
	return c.UploadApprovalFile(ctx, full, "attachment", buf.Bytes())
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
