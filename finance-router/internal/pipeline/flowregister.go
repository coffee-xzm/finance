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
	if id := ctrl("date"); id != "" && finishMS > 0 {
		form = append(form, map[string]any{"id": id, "type": "date",
			"value": time.UnixMilli(finishMS).Format(time.RFC3339)})
		notes = append(notes, "转账日期 = "+time.UnixMilli(finishMS).Format("2006-01-02"))
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
// uuid 用 `<采购实例code>-<明细序号>`：同一明细重复处理会撞 60012（UUID 冲突），
// 由调用方按"本地已记过就跳过"来避免。
func createFlowRegisterDraft(ctx context.Context, cfg *config.Config, client *feishu.Client,
	registerCode string, info *purchaseInfo, it purchaseItem, idx int, finishMS int64) (string, error) {

	userID := cfg.RegisterUserID()
	if userID == "" {
		return "", fmt.Errorf("config 缺 flow_register.user_id（流水登记人）")
	}
	form, notes := buildFlowRegisterForm(cfg, info, it, finishMS)
	for _, n := range notes {
		fmt.Printf("  预填 %s\n", n)
	}
	newCode, err := client.CreateInstance(ctx, feishu.CreateInstanceRequest{
		ApprovalCode:  registerCode,
		UserID:        userID,
		Form:          form,
		UUID:          fmt.Sprintf("%s-%d", info.InstanceCode, idx),
		AllowResubmit: true,
	})
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
		return newCode, fmt.Errorf("建单成功但没有 PENDING 任务可退回（需人工处理）")
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

	// 目标流水行：本地映射优先，其次按「流水审批ID」反查（幂等锚点）
	ledgerRecID, purchaseCode := "", ""
	if r, ok, err := db.GetFlowRegister(ctx, opts.Instance); err == nil && ok {
		ledgerRecID, purchaseCode = r.LedgerRecordID, r.PurchaseInstanceCode
		if r.State == store.FlowRegisterApplied && !opts.Force {
			fmt.Printf("  = 登记单 %s 已经覆盖过流水行（%s），跳过\n", short(opts.Instance), ledgerRecID)
			return nil
		}
	}
	if ledgerRecID == "" {
		if id, err := findLedgerRowByFlowID(ctx, cfg, client, flowBase.AppToken, ledgerTbl, opts.Instance); err == nil {
			ledgerRecID = id
		} else {
			fmt.Printf("  ⚠ 按「流水审批ID」反查流水行失败（继续，按自建登记处理）: %v\n", err)
		}
	}
	fmt.Printf("▶ 流水登记 %s：类型=%s 金额=%.2f 来源=%s 去向=%s → 流水行 %s\n",
		short(opts.Instance), orDash(ff.Kind), ff.Amount(), orDash(ff.Source), orDash(ff.Destination),
		orDash(short(ledgerRecID)))

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

// findLedgerRowByFlowID 在「27 - 收支表」里按「流水审批ID」列反查登记单对应的行。
func findLedgerRowByFlowID(ctx context.Context, cfg *config.Config, c *feishu.Client,
	appToken, tableID, instanceCode string) (string, error) {

	recs, err := c.SearchBitableRecords(ctx, appToken, tableID, nil, 500)
	if err != nil {
		return "", err
	}
	field := cfg.Field("ledger", "flow_id")
	for i := range recs {
		raw, ok := recs[i].Fields[field]
		if !ok || len(raw) == 0 {
			continue
		}
		if strings.Contains(string(raw), instanceCode) {
			return recs[i].RecordID, nil
		}
	}
	return "", nil
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

// maybeCreateInvoiceForPurchase 在**该采购的全部流水登记单都已 applied** 之后
// 给采购提交人开「27发票收集」（预填 + 各登记单的付款截图），并私信通知。
//
// 为什么要等全部：发票单是按**整笔采购**开的（「名称」列是所有明细名），
// 少一张登记单就开票，金额/截图都会缺。
func maybeCreateInvoiceForPurchase(ctx context.Context, cfg *config.Config, client *feishu.Client,
	db *store.DB, purchaseCode string) error {

	prev, ok, err := db.GetPurchase(ctx, purchaseCode)
	if err != nil {
		return err
	}
	if ok && prev.InvoiceInstanceCode != "" {
		fmt.Printf("  = 发票单已代建过（%s），跳过\n", short(prev.InvoiceInstanceCode))
		return nil
	}
	regs, err := db.FlowRegistersOfPurchase(ctx, purchaseCode)
	if err != nil {
		return err
	}
	if len(regs) == 0 {
		fmt.Println("  ⚠ 本地没有该采购的登记单留痕，无法判断是否登记完成 → 暂不开票")
		return nil
	}
	pending := 0
	for _, r := range regs {
		if r.State != store.FlowRegisterApplied {
			pending++
		}
	}
	if pending > 0 {
		fmt.Printf("  ⏳ 还有 %d/%d 张流水登记单未完成 → 暂不开票\n", pending, len(regs))
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

	// 各登记单的转账截图 → 上传到审批系统换 file code，作为发票单的「付款记录」
	var payTokens []string
	for _, r := range regs {
		rdet, _, err := client.GetInstanceDetail(ctx, r.InstanceCode)
		if err != nil {
			fmt.Printf("  ⚠ 读登记单 %s 失败（少一张付款截图）: %v\n", short(r.InstanceCode), err)
			continue
		}
		rws, err := feishu.ParseForm(rdet.Form)
		if err != nil {
			continue
		}
		rff := parseFlowForm(cfg, rws)
		for i, u := range rff.Screenshots {
			tok, err := downloadAndUploadApprovalFile(ctx, client, u,
				fmt.Sprintf("付款截图-%s-%d", short(r.InstanceCode), i+1))
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
		fmt.Println("  ⚠ 各登记单里没有转账截图，发票单的「付款记录」留空由提交人补")
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
	fmt.Printf("  ✓ 已代建发票单 %s 并退回发起人（全部 %d 张登记单已完成）\n",
		short(invoiceCode), len(regs))
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
