// purchase 是"采购审批通过后"的两件事（docs/30-review/33 §5）：
//
//	① 写流水：把该采购的每条【费用明细】写成「27 - 收支表」的一行【支出】；
//	② 代建发票单：创建一条提交人=采购提交人的「27发票收集」审批，预填能映射的字段，
//	   然后立刻把它退回到 START —— 这样发起人才能在飞书里补齐附件并重新提交。
//
// 三条硬约束：
//   - 「27 - 流动资金采购审批」镜像表**只读**（我们只 GET + 按 SourceID 对账）；
//   - 写表**只填空单元格**（新行全空 → 全写；已存在 → 只补空）；
//   - 幂等键 = 采购实例 code（重复事件不重复写流水 / 不重复建单）。
package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/store"
)

// PurchaseOptions 配置一次采购处理。
type PurchaseOptions struct {
	CfgPath  string
	Instance string // 采购审批实例 code
	DryRun   bool
	Force    bool // 忽略本地已完成标记，重跑
}

// purchaseInfo 是从采购审批实例里抽出来的、要用的字段。
type purchaseInfo struct {
	InstanceCode  string
	ApprovalCode  string
	ApprovalName  string // 审批定义名（如「采购审批 - 27Test」）
	Status        string // APPROVED / …
	ApplicantID   string // 提交人 user_id
	ApplicantOID  string // 提交人 open_id（写"人员"字段用）
	ApplicantDept string // 发起人部门名（尽力而为，可能为空）
	ProjectGroup  string
	Category      string
	ProjectName   string
	Items         []purchaseItem
	Images        []string // 「商品图片」的临时直链（24h 有效，用来转存成附件）
	StartTimeMS   int64
	FinishTimeMS  int64
}

type purchaseItem struct {
	Name   string
	Spec   string
	Amount float64
	Qty    float64
}

func RunPurchase(ctx context.Context, opts PurchaseOptions) error {
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
	appr, ok := cfg.ApprovalByRole(config.RolePurchase)
	if !ok || appr.Code == "" {
		return fmt.Errorf("配置里没有 role=purchase 的审批（feishu.approvals）")
	}
	if opts.Instance == "" {
		return fmt.Errorf("必须指定采购实例 code")
	}

	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()

	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	detail, _, err := client.GetInstanceDetail(ctx, opts.Instance)
	if err != nil {
		return fmt.Errorf("读采购实例 %s: %w", opts.Instance, err)
	}
	if !opts.Force {
		if prev, ok, err := db.GetPurchase(ctx, opts.Instance); err == nil && ok &&
			prev.InvoiceInstanceCode != "" && prev.DraftState == "awaiting_applicant" {
			fmt.Printf("  = 采购 %s 已处理过（发票单 %s，%s），跳过\n",
				short(opts.Instance), short(prev.InvoiceInstanceCode), prev.DraftState)
			return nil
		}
	}

	info, err := parsePurchase(detail)
	if err != nil {
		return err
	}
	status := strings.ToUpper(strings.TrimSpace(detail.Status))
	if status != "APPROVED" {
		return fmt.Errorf("采购实例 %s 状态是 %s（只处理 APPROVED）", short(opts.Instance), detail.Status)
	}
	fmt.Printf("▶ 采购 %s 通过：项目组=%s 明细=%d 条 提交人=%s\n",
		short(info.InstanceCode), info.ProjectGroup, len(info.Items), info.ApplicantID)

	// 部门对照：config 字典 + 本地学到的 + 通讯录（有 contact:department.base:readonly 时）。
	// 顺手把通讯录结果 learn 进本地，下次不依赖网络。
	deptByName, deptByID := loadDepts(ctx, client, db, cfg)
	if info.ApplicantDept == "" && detail.DepartmentID != "" {
		info.ApplicantDept = deptByID[detail.DepartmentID]
		if info.ApplicantDept == "" {
			if n, err := client.GetDepartmentName(ctx, detail.DepartmentID); err == nil {
				info.ApplicantDept = n
			}
		}
	}

	flowApp := cfg.Table(config.BaseFlow, "ledger")
	purchaseTbl := cfg.Table(config.BaseFlow, "purchase")
	flowBase, _ := cfg.Base(config.BaseFlow)
	if flowApp == "" || flowBase.AppToken == "" {
		return fmt.Errorf("配置缺少 flow base（feishu.bitable.bases.flow）")
	}

	// ── ① 镜像行（只读对账）──
	mirrorID, finishMS := "", info.FinishTimeMS
	if purchaseTbl != "" {
		if rec, err := findMirrorRecord(ctx, cfg, client, flowBase.AppToken, purchaseTbl, info.InstanceCode); err != nil {
			fmt.Printf("  ⚠ 读采购镜像表失败（继续，仅少了回链）: %v\n", err)
		} else if rec != nil {
			mirrorID = rec.RecordID
			if finishMS == 0 {
				if v := intOfField(rec.Fields[cfg.Field("purchase", "finish_time")]); v > 0 {
					finishMS = v
				}
			}
			fmt.Printf("  ✓ 匹配到镜像行 %s\n", mirrorID)
		} else {
			fmt.Printf("  ⚠ 镜像表里还没同步到该采购（连接器有延迟），本次不回链\n")
		}
	}

	// ── ② 写流水：每条费用明细一行 ──
	// ★ 幂等：本地已经记过流水行就不再写第二遍。
	//   （否则"流水写完、代建发票单失败"重跑时会重复记账。）
	applink := buildApprovalApplink(cfg, info.InstanceCode)
	ledgerIDs := []string{}
	if prev, ok, _ := db.GetPurchase(ctx, info.InstanceCode); ok && len(prev.LedgerRecordIDs) > 0 && !opts.Force {
		ledgerIDs = append(ledgerIDs, prev.LedgerRecordIDs...)
		if prev.MirrorRecordID != "" && mirrorID == "" {
			mirrorID = prev.MirrorRecordID
		}
		fmt.Printf("  = 流水已写过 %d 行，跳过重复写入\n", len(ledgerIDs))
	} else {
		for i, it := range info.Items {
			fields := buildLedgerFields(cfg, info, it, applink, mirrorID, finishMS)
			if opts.DryRun {
				b, _ := json.Marshal(fields)
				fmt.Printf("  [dry-run] 流水行 %d: %s\n", i+1, trunc(string(b), 300))
				continue
			}
			recID, err := client.CreateBitableRecord(ctx, flowBase.AppToken, flowApp, fields)
			if err != nil {
				return fmt.Errorf("写流水第 %d 行失败: %w%s", i+1, err,
					writeDeniedHint(ctx, client, cfg, flowBase.AppToken, err))
			}
			ledgerIDs = append(ledgerIDs, recID)
			fmt.Printf("  ✓ 流水行 %d record=%s 金额=%.2f（单价 %g × %s）科目=%s\n",
				i+1, recID, ledgerAmount(it), it.Amount,
				strconv.FormatFloat(it.Qty, 'f', -1, 64), cfg.SubjectForGroup(info.ProjectGroup))
		}
	}

	// ── ③ 写「27采购申请表」（wiki；字段对齐参考表 tbllUFPS…）──
	// 一条费用明细一行（参考表的明细列是单数，SourceID 也带 -N 序号）。
	requestIDs := []string{}
	if prBase, ok := cfg.Base(config.BasePurchaseRequest); ok {
		prTable := cfg.Table(config.BasePurchaseRequest, "request")
		if prTable == "" {
			fmt.Printf("  ⚠ 配了 purchase_request base 但没配 tables.request，跳过写采购申请表\n")
		} else {
			prevReq := []string{}
			if prev, ok, _ := db.GetPurchase(ctx, info.InstanceCode); ok {
				prevReq = prev.RequestRecordIDs
			}
			switch {
			case len(prevReq) > 0 && !opts.Force:
				requestIDs = append(requestIDs, prevReq...)
				fmt.Printf("  = 采购申请表已写过 %d 行，跳过\n", len(requestIDs))
			default:
				for i, it := range info.Items {
					fields := buildRequestFields(cfg, info, it, applink)
					if opts.DryRun {
						b, _ := json.Marshal(fields)
						fmt.Printf("  [dry-run] 采购申请表行 %d: %s\n", i+1, trunc(string(b), 300))
						continue
					}
					recID, err := client.CreateBitableRecord(ctx, prBase.AppToken, prTable, fields)
					if err != nil {
						// ★ 流水已经写了：必须把已完成的产物落本地，否则重跑会重复记账。
						_ = db.UpsertPurchase(ctx, store.PurchaseSync{
							PurchaseInstanceCode: info.InstanceCode, ApprovalCode: info.ApprovalCode,
							ApplicantUserID: info.ApplicantID, PurchaseStatus: status,
							ProjectGroup: info.ProjectGroup, MirrorRecordID: mirrorID,
							LedgerRecordIDs: ledgerIDs, RequestRecordIDs: requestIDs,
							DraftState: "failed", LastError: err.Error(),
						})
						return fmt.Errorf("写「27采购申请表」第 %d 行失败（流水已写 %d 行，已落本地）：%w%s",
							i+1, len(ledgerIDs), err,
							writeDeniedHint(ctx, client, cfg, prBase.AppToken, err))
					}
					requestIDs = append(requestIDs, recID)
					fmt.Printf("  ✓ 采购申请表行 %d record=%s %s\n", i+1, recID, it.Name)
					// 商品图片是"整条审批"的，挂到第一行即可（按明细拆行时不重复挂）
					if i == 0 && len(info.Images) > 0 {
						n, err := attachRequestImages(ctx, client, cfg, prBase.AppToken, prTable,
							recID, info.Images)
						if err != nil {
							fmt.Printf("  ⚠ 商品图片转存失败（不影响其它字段）: %v\n", err)
						} else {
							fmt.Printf("  ✓ 商品图片已转存 %d 张为附件\n", n)
						}
					}
				}
			}
		}
	}

	// ── ④ 代建 27发票收集 + 退回到发起人 ──
	invoiceCode := ""
	if inv, ok := cfg.ApprovalByRole(config.RoleInvoiceCollect); ok && inv.Code != "" {
		prevInv := ""
		if prev, ok, _ := db.GetPurchase(ctx, info.InstanceCode); ok {
			prevInv = prev.InvoiceInstanceCode
		}
		switch {
		case prevInv != "" && !opts.Force:
			invoiceCode = prevInv
			fmt.Printf("  = 发票单已代建过（%s），跳过\n", short(invoiceCode))
		case opts.DryRun:
			fmt.Printf("  [dry-run] 将代建发票单（提交人=%s，名称=%s，预填购买人/物资所属部门）\n",
				info.ApplicantID, invoiceName(cfg, info))
		default:
			invoiceCode, err = createInvoiceDraft(ctx, cfg, client, db, deptByName, inv.Code, info)
			if err != nil {
				// 发票单失败不掩盖流水已写：记状态，返回错误让人看见
				_ = db.UpsertPurchase(ctx, store.PurchaseSync{
					PurchaseInstanceCode: info.InstanceCode, ApprovalCode: info.ApprovalCode,
					ApplicantUserID: info.ApplicantID, PurchaseStatus: status,
					ProjectGroup: info.ProjectGroup, MirrorRecordID: mirrorID,
					LedgerRecordIDs: ledgerIDs, RequestRecordIDs: requestIDs,
					InvoiceInstanceCode: invoiceCode,
					DraftState:          "failed", LastError: err.Error(),
				})
				return fmt.Errorf("代建发票单失败（流水已写 %d 行）: %w", len(ledgerIDs), err)
			}
			fmt.Printf("  ✓ 已代建发票单 %s 并退回发起人\n", short(invoiceCode))
			_ = notifyApplicant(ctx, cfg, client, info, invoiceCode)
		}
	}

	// ── ④ 本地留痕（幂等锚点）──
	if !opts.DryRun {
		if err := db.UpsertPurchase(ctx, store.PurchaseSync{
			PurchaseInstanceCode: info.InstanceCode, ApprovalCode: info.ApprovalCode,
			ApplicantUserID: info.ApplicantID, PurchaseStatus: status,
			ProjectGroup: info.ProjectGroup, MirrorRecordID: mirrorID,
			LedgerRecordIDs: ledgerIDs, RequestRecordIDs: requestIDs,
			InvoiceInstanceCode: invoiceCode,
			DraftState:          "awaiting_applicant",
		}); err != nil {
			return fmt.Errorf("写本地采购记录失败: %w", err)
		}
	}
	fmt.Printf("✓ 采购 %s 处理完成（流水 %d 行，发票单 %s）\n",
		short(info.InstanceCode), len(ledgerIDs), short(invoiceCode))
	return nil
}

// parsePurchase 抽取采购审批里我们需要的字段。
func parsePurchase(detail *feishu.InstanceDetail) (*purchaseInfo, error) {
	ws, err := feishu.ParseForm(detail.Form)
	if err != nil {
		return nil, fmt.Errorf("解析采购 form: %w", err)
	}
	info := &purchaseInfo{
		InstanceCode: detail.InstanceCode,
		ApprovalCode: detail.ApprovalCode,
		ApprovalName: detail.ApprovalName,
		Status:       detail.Status,
		ApplicantID:  detail.UserID,
		ApplicantOID: detail.OpenID,
	}
	if ms, err := strconv.ParseInt(strings.TrimSpace(detail.StartTime), 10, 64); err == nil {
		info.StartTimeMS = ms
	}
	if ms, err := strconv.ParseInt(strings.TrimSpace(detail.EndTime), 10, 64); err == nil {
		info.FinishTimeMS = ms
	}
	for _, w := range ws {
		switch {
		case strings.Contains(w.Name, "项目组"):
			info.ProjectGroup = strVal(w.Value)
		case strings.Contains(w.Name, "采购类别"):
			info.Category = strVal(w.Value)
		case strings.Contains(w.Name, "项目名称"):
			info.ProjectName = strVal(w.Value)
		case w.Type == "fieldList":
			info.Items = parseFieldList(w.Value)
		case w.Type == "attachmentV2" && strings.Contains(w.Name, "商品图片"):
			info.Images = w.AttachmentURLs()
		}
	}
	if info.ProjectGroup == "" {
		return nil, fmt.Errorf("采购 %s 没有读到「项目组」，无法定科目/部门", short(detail.InstanceCode))
	}
	return info, nil
}

// parseFieldList 把「费用明细」解析成一行行明细。
func parseFieldList(raw json.RawMessage) []purchaseItem {
	var rows [][]struct {
		Name  string          `json:"name"`
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	var out []purchaseItem
	for _, row := range rows {
		var it purchaseItem
		for _, sub := range row {
			switch sub.Name {
			case "名称":
				it.Name = strVal(sub.Value)
			case "规格":
				it.Spec = strVal(sub.Value)
			case "金额":
				it.Amount = floatVal(sub.Value)
			case "数量":
				it.Qty = floatVal(sub.Value)
			}
		}
		if it.Name == "" && it.Amount == 0 {
			continue
		}
		out = append(out, it)
	}
	return out
}

// buildLedgerFields 把一条明细组装成「27 - 收支表」的一行。
//
// ★ 金额口径（用户 2026-09-21 指出）：采购表单「费用明细」里的 **金额是单价**，
// 流水行的金额必须是 **单价 × 数量**。见 ledgerAmount()。
func buildLedgerFields(cfg *config.Config, info *purchaseInfo, it purchaseItem,
	applink, mirrorID string, finishMS int64) map[string]any {

	f := map[string]any{
		cfg.Field("ledger", "direction"):        "支出",
		cfg.Field("ledger", "group"):            info.ProjectGroup,
		cfg.Field("ledger", "subject"):          cfg.SubjectForGroup(info.ProjectGroup),
		cfg.Field("ledger", "amount"):           ledgerAmount(it),
		cfg.Field("ledger", "invoice_progress"): cfg.Dict.Rules.InvoiceProgressTodo,
	}
	if n := ledgerNote(info, it); n != "" {
		f[cfg.Field("ledger", "note")] = n
	}
	if applink != "" {
		f[cfg.Field("ledger", "apply_link")] = map[string]string{"link": applink, "text": "查看采购审批"}
	}
	if mirrorID != "" {
		f[cfg.Field("ledger", "purchase_link")] = []string{mirrorID}
	}
	if finishMS > 0 {
		f[cfg.Field("ledger", "occurred_at")] = finishMS
	}
	return f
}

// ledgerAmount 算流水行该写的金额 = **单价 × 数量**。
//
// 采购表单「费用明细」里的字段名叫「金额」，但填的是**单价**（用户 2026-09-21 明确）；
// 数量缺失/为 0 时按 1 件处理（宁可按单价记，也不要乘出个 0 把金额抹掉）。
func ledgerAmount(it purchaseItem) float64 {
	if it.Qty > 0 {
		return it.Amount * it.Qty
	}
	return it.Amount
}

// buildRequestFields 把一条费用明细组装成「27采购申请表」的一行。
//
// 字段名沿用 fields.purchase 字典（wiki 表与参考表表头逐字一致）。
// 本轮留空的列：期望交付时间 / 采购事由（新采购表单没有这两个字段）。
// 2026-09-19 用户从表里删掉了 4 列（当前处理人 / 审批节点 / 费用明细_规格 /
// 费用明细_金额-币种）→ 字典与 schema 清单同步删除，绝不能往里写（写不存在的
// 字段名会让**整行**创建失败）。
func buildRequestFields(cfg *config.Config, info *purchaseInfo, it purchaseItem, applink string) map[string]any {
	F := func(k string) string { return cfg.Field("purchase", k) }
	f := map[string]any{}
	set := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			f[F(k)] = strings.TrimSpace(v)
		}
	}
	set("project_name", info.ProjectName)
	set("category", info.Category)
	set("source_id", info.InstanceCode) // 对账/幂等键：采购实例 code
	set("apply_status", statusWord(info.Status))
	set("flow", info.ApprovalName)
	set("applicant_dept", info.ApplicantDept)
	if applink != "" {
		f[F("apply_link")] = map[string]string{"link": applink, "text": "查看采购审批"}
	}
	if info.StartTimeMS > 0 {
		f[F("start_time")] = info.StartTimeMS
	}
	if info.FinishTimeMS > 0 {
		f[F("finish_time")] = info.FinishTimeMS
	}
	if info.ApplicantOID != "" {
		// 人员字段：默认 user_id_type=open_id
		f[F("applicant")] = []map[string]any{{"id": info.ApplicantOID}}
	}
	set("detail_name", it.Name)
	if it.Amount != 0 {
		f[F("detail_amount")] = it.Amount
	}
	if it.Qty != 0 {
		f[F("detail_qty")] = it.Qty
	}
	return f
}

// attachRequestImages 把审批「商品图片」的临时直链（24h 失效）下载并转存成
// 目标表的**附件**。多维表格附件存 file_token，长期有效。
func attachRequestImages(ctx context.Context, c *feishu.Client, cfg *config.Config,
	appToken, tableID, recordID string, urls []string) (int, error) {
	if len(urls) == 0 {
		return 0, nil
	}
	var toks []map[string]string
	for i, u := range urls {
		var buf bytes.Buffer
		res, err := c.DownloadTmpURL(ctx, u, &buf)
		if err != nil {
			return len(toks), err
		}
		name := fmt.Sprintf("商品图片-%d%s", i+1, extByContentType(res.ContentType))
		tok, err := c.UploadMedia(ctx, appToken, "bitable_image", name, buf.Bytes())
		if err != nil {
			return len(toks), err
		}
		toks = append(toks, map[string]string{"file_token": tok})
	}
	if len(toks) == 0 {
		return 0, nil
	}
	if err := c.UpdateBitableRecord(ctx, appToken, tableID, recordID,
		map[string]any{cfg.Field("purchase", "image"): toks}); err != nil {
		return len(toks), err
	}
	return len(toks), nil
}

func extByContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch ct {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "application/pdf":
		return ".pdf"
	}
	return ".bin"
}

// BackfillPurchaseImages 给"已经写过采购申请表行"的采购补传商品图片（运维/测试用）：
// 用于字段类型从 URL 改成附件之后，把历史行的图补上。
func BackfillPurchaseImages(ctx context.Context, cfg *config.Config, instanceCode string) error {
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return err
	}
	defer db.Close()
	rec, ok, err := db.GetPurchase(ctx, instanceCode)
	if err != nil {
		return err
	}
	if !ok || len(rec.RequestRecordIDs) == 0 {
		return fmt.Errorf("本地没有采购 %s 的采购申请表行记录（先跑 cmd/purchase）", short(instanceCode))
	}
	prBase, ok := cfg.Base(config.BasePurchaseRequest)
	prTable := cfg.Table(config.BasePurchaseRequest, "request")
	if !ok || prTable == "" {
		return fmt.Errorf("配置缺少 bases.purchase_request")
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	detail, _, err := client.GetInstanceDetail(ctx, instanceCode)
	if err != nil {
		return err
	}
	info, err := parsePurchase(detail)
	if err != nil {
		return err
	}
	if len(info.Images) == 0 {
		fmt.Println("该采购审批里没有「商品图片」，无需补传")
		return nil
	}
	n, err := attachRequestImages(ctx, client, cfg, prBase.AppToken, prTable,
		rec.RequestRecordIDs[0], info.Images)
	if err != nil {
		return err
	}
	fmt.Printf("✓ 已补传 %d 张商品图片 → %s\n", n, rec.RequestRecordIDs[0])
	return nil
}

// ResyncPurchaseRequest 按"只填空单元格"策略补正已写过的采购申请表行
// （例如：部门解析权限后补，旧行缺的「发起人部门」）。
func ResyncPurchaseRequest(ctx context.Context, cfg *config.Config, instanceCode string) error {
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return err
	}
	defer db.Close()
	rec, ok, err := db.GetPurchase(ctx, instanceCode)
	if err != nil {
		return err
	}
	if !ok || len(rec.RequestRecordIDs) == 0 {
		return fmt.Errorf("本地没有采购 %s 的采购申请表行记录", short(instanceCode))
	}
	prBase, ok := cfg.Base(config.BasePurchaseRequest)
	prTable := cfg.Table(config.BasePurchaseRequest, "request")
	if !ok || prTable == "" {
		return fmt.Errorf("配置缺少 bases.purchase_request")
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	detail, _, err := client.GetInstanceDetail(ctx, instanceCode)
	if err != nil {
		return err
	}
	info, err := parsePurchase(detail)
	if err != nil {
		return err
	}
	_, deptByID := loadDepts(ctx, client, db, cfg)
	if info.ApplicantDept == "" && detail.DepartmentID != "" {
		info.ApplicantDept = deptByID[detail.DepartmentID]
	}
	applink := buildApprovalApplink(cfg, instanceCode)

	existing, err := client.SearchBitableRecords(ctx, prBase.AppToken, prTable, nil, 500)
	if err != nil {
		return err
	}
	byID := map[string]feishu.BitableRecord{}
	for _, r := range existing {
		byID[r.RecordID] = r
	}
	updated := 0
	for i, recID := range rec.RequestRecordIDs {
		item := purchaseItem{}
		if i < len(info.Items) {
			item = info.Items[i]
		}
		desired := buildRequestFields(cfg, info, item, applink)
		old, ok := byID[recID]
		if !ok {
			fmt.Printf("  ⚠ 找不到采购申请表行 %s，跳过\n", recID)
			continue
		}
		// 只填空：不动任何已有值的单元格
		fill := blankOnly(cfg, old.Fields, desired, nil)
		if len(fill) == 0 {
			fmt.Printf("  = 行 %s 没有可补的空单元格\n", recID)
			continue
		}
		if err := client.UpdateBitableRecord(ctx, prBase.AppToken, prTable, recID, fill); err != nil {
			return fmt.Errorf("补正行 %s 失败: %w", recID, err)
		}
		updated++
		fmt.Printf("  ✓ 补正行 %s：%d 个空单元格\n", recID, len(fill))
	}
	fmt.Printf("✓ 完成，补正 %d 行\n", updated)
	return nil
}

// ledgerNote 拼一条人可读的备注：项目名称 + 品名/规格/数量。
func ledgerNote(info *purchaseInfo, it purchaseItem) string {
	var parts []string
	if info.ProjectName != "" {
		parts = append(parts, info.ProjectName)
	}
	s := it.Name
	if it.Spec != "" {
		s += "(" + it.Spec + ")"
	}
	if it.Qty > 0 {
		// 用 'f' 避免 %g 把大数量写成科学计数法（1234513245 → 1.234513245e+09）
		s += " ×" + strconv.FormatFloat(it.Qty, 'f', -1, 64)
	}
	if strings.TrimSpace(s) != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, " ｜ ")
}

// createInvoiceDraft 代建「27发票收集」并退回到发起人；返回新实例 code。
func createInvoiceDraft(ctx context.Context, cfg *config.Config, client *feishu.Client,
	db *store.DB, deptByName map[string]string, invoiceCode string, info *purchaseInfo) (string, error) {

	form, notes := buildInvoiceForm(cfg, info, deptByName)
	for _, n := range notes {
		fmt.Printf("  预填 %s\n", n)
	}

	newCode, err := client.CreateInstance(ctx, feishu.CreateInstanceRequest{
		ApprovalCode:  invoiceCode,
		UserID:        info.ApplicantID,
		Form:          form,
		UUID:          info.InstanceCode, // ★ 幂等：同一采购只建一次（冲突 60012）
		AllowResubmit: true,
	})
	if err != nil {
		return "", err
	}

	// 退回到 START：让发起人能在同一实例里补齐附件并重新提交
	det, _, err := client.GetInstanceDetail(ctx, newCode)
	if err != nil {
		return newCode, fmt.Errorf("建单成功但读详情失败（需人工退回）: %w", err)
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
		"请补充发票 / 订单截图 / 付款记录后提交"); err != nil {
		return newCode, fmt.Errorf("建单成功但退回失败（需人工退回）: %w", err)
	}
	return newCode, nil
}

// buildInvoiceForm 组装代建发票单要预填的表单，并返回给人看的说明。
//
// 预填三类（对齐「27发票收集」的表单控件 id，见 config 字典 controls.invoice_collect）：
//
//	购买人         = 采购审批的提交人
//	物资所属部门   = 项目组经 rules.dept_alias 映射后的部门
//	名称           = <采购审批的「项目名称」> + purchase_to_invoice.name_suffix
//	                 （线上字段名是「名称（选填，采购提示）」，后缀用来标明
//	                   "这张发票单是哪笔采购带出来的"，如「测试-采购审批」）
//
// 取不到值的控件**不写**（留空让发起人自己填），不写空串占位。
// 纯函数：不碰网络，便于单测覆盖。
func buildInvoiceForm(cfg *config.Config, info *purchaseInfo,
	deptByName map[string]string) (form []map[string]any, notes []string) {

	form = []map[string]any{}
	if id := cfg.Control(config.RoleInvoiceCollect, "buyer"); id != "" && info.ApplicantID != "" {
		form = append(form, map[string]any{"id": id, "type": "contact", "value": []string{info.ApplicantID}})
		notes = append(notes, fmt.Sprintf("购买人 = %s", info.ApplicantID))
	}
	if id := cfg.Control(config.RoleInvoiceCollect, "departments"); id != "" {
		deptName := cfg.DeptAlias(info.ProjectGroup)
		if od := deptByName[deptName]; od != "" {
			form = append(form, map[string]any{"id": id, "type": "department",
				"value": []map[string]any{{"open_id": od}}})
			notes = append(notes, fmt.Sprintf("物资所属部门 = %s（%s）", deptName, od))
		} else {
			notes = append(notes, fmt.Sprintf("⚠ 项目组 %s（别名 %s）解析不到部门 open_department_id，物资所属部门留空",
				info.ProjectGroup, deptName))
		}
	}
	if id := cfg.Control(config.RoleInvoiceCollect, "name"); id != "" {
		if v := invoiceName(cfg, info); v != "" {
			form = append(form, map[string]any{"id": id, "type": "input", "value": v})
			notes = append(notes, fmt.Sprintf("名称 = %s", v))
		} else {
			notes = append(notes, "⚠ 采购审批的「费用明细」没有名称，发票单名称留空")
		}
	}
	return form, notes
}

// invoiceName 拼发票单「名称（选填，采购提示）」：<费用明细里的名称> + 配置后缀
// （默认「-采购审批」）。
//
// 用户 2026-09-19 定：取**费用明细的名称**（不是采购审批的「项目名称」），
// 因为收票人要对的是"买了什么"。多条明细用逗号连接；一个名称都没有 → 返回空串 = 不填。
func invoiceName(cfg *config.Config, info *purchaseInfo) string {
	var names []string
	seen := map[string]bool{}
	for _, it := range info.Items {
		n := strings.TrimSpace(it.Name)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ",") + cfg.Dict.PurchaseToInvoice.NameSuffix
}

// notifyApplicant 告诉发起人：已代建、请补齐并提交。
//
// ★ 失败不能静默（2026-09-21 实测教训）：硬件组那单采购通过后提交人**没收到提醒** ——
// 原因是机器人（应用）的**可用范围**不含该提交人，飞书返回
//
//	HTTP 400 code=230013 msg=Bot has NO availability to this user.
//
// 这是飞书开发者后台的设置（不是代码能改的），但至少要做到：① 日志里说清怎么根治；
// ② 退而通知管理员（管理员必然在可用范围内），别让"该提醒谁"这件事凭空消失。
func notifyApplicant(ctx context.Context, cfg *config.Config, client *feishu.Client,
	info *purchaseInfo, invoiceCode string) error {
	if info.ApplicantID == "" {
		return fmt.Errorf("采购实例没有提交人 user_id，无法通知")
	}
	link := buildApprovalApplink(cfg, invoiceCode)
	text := fmt.Sprintf("【发票收集】采购审批「%s」已通过，已为你创建「27发票收集」审批。\n"+
		"请补充发票 / 订单截图 / 付款记录后提交：\n%s", info.ProjectName, link)
	err := client.SendTextMessage(ctx, "user_id", info.ApplicantID, text)
	if err == nil {
		return nil
	}
	fmt.Printf("  ⚠ 通知发起人失败（不影响流程）: %v\n", err)

	if strings.Contains(err.Error(), "230013") {
		fmt.Println("     ↳ 机器人可用范围不含该提交人：飞书开发者后台 → 该应用 → 机器人/可用范围，" +
			"把提交人本人（或其部门）加进去")
	}
	admin := cfg.Feishu.AdminUserID
	if admin == "" || admin == info.ApplicantID {
		return err
	}
	warn := fmt.Sprintf("⚠ 无法私信提醒提交人（user_id=%s）。\n采购「%s」已通过并已代建发票单，"+
		"请手动提醒他补齐发票/订单截图/付款记录后提交：\n%s\n\n飞书返回：%v",
		info.ApplicantID, info.ProjectName, link, err)
	if e2 := client.SendTextMessage(ctx, "user_id", admin, warn); e2 != nil {
		fmt.Printf("  ⚠ 改通知管理员也失败: %v\n", e2)
	} else {
		fmt.Printf("  ✓ 已改通知管理员（user_id=%s）：请手动提醒提交人\n", admin)
	}
	return err
}

// RemindInvoiceDraft 运维入口：把「请补齐并提交」的提醒**重发**给发票单的提交人。
//
// 用途：① 之前因机器人可用范围（230013）发失败，修好权限后补发；② 提交人说没收到时重发。
// 若该发票单不是采购派生的（本地 purchase_sync 没有映射），返回错误。
func RemindInvoiceDraft(ctx context.Context, cfg *config.Config, invoiceInstanceCode string) error {
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()
	p, ok, err := db.PurchaseByInvoice(ctx, invoiceInstanceCode)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("发票单 %s 不是采购派生的（本地 purchase_sync 里没有映射）", short(invoiceInstanceCode))
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	det, _, err := client.GetInstanceDetail(ctx, p.PurchaseInstanceCode)
	if err != nil {
		return fmt.Errorf("读采购实例 %s 失败: %w", short(p.PurchaseInstanceCode), err)
	}
	info, err := parsePurchase(det)
	if err != nil {
		return err
	}
	fmt.Printf("▶ 重发提醒：采购 %s（%s）→ 提交人 %s\n",
		short(p.PurchaseInstanceCode), info.ProjectName, info.ApplicantID)
	return notifyApplicant(ctx, cfg, client, info, invoiceInstanceCode)
}

// writeDeniedHint 写被拒时补一句"本应用在该文档的角色"，省得反复猜。
// 实测最常见的坑：把应用加成了「可阅读(view)」而不是「可编辑(edit)」。
func writeDeniedHint(ctx context.Context, c *feishu.Client, cfg *config.Config,
	appToken string, err error) string {
	if err == nil || !(strings.Contains(err.Error(), "91403") || strings.Contains(err.Error(), "Forbidden")) {
		return ""
	}
	role := "未在协作者列表里"
	members, e := c.ListPermissionMembers(ctx, appToken, "bitable")
	if e != nil {
		role = "查询失败: " + e.Error()
	} else {
		found := false
		for _, m := range members {
			if m.MemberType == "appid" && m.MemberID == cfg.Feishu.AppID {
				role, found = m.Perm, true
			}
		}
		if !found {
			role = "未在协作者列表里"
		}
	}
	return fmt.Sprintf("\n  ⚠ 本应用（%s）在该 base 的协作者角色 = %s；写记录需要 edit（可编辑）。"+
		"\n     多维表格：在该文档「分享 / 协作者」里把应用改成【可编辑】。"+
		"\n     若这是知识库(wiki)里的文档：权限挂在知识库空间上 —— 知识库 → 设置 → 成员管理 → 添加应用为【可编辑】。",
		cfg.Feishu.AppID, role)
}

// loadDepts 汇总"部门名 ↔ open_department_id / department_id"对照：
// config 字典 + 本地学到的 + 通讯录（有 contact:department.base:readonly 时）。
// 通讯录结果会 learn 进本地，之后即使没网络也能解析。
func loadDepts(ctx context.Context, client *feishu.Client, db *store.DB,
	cfg *config.Config) (byName, byID map[string]string) {

	byName = map[string]string{}
	byID = map[string]string{}
	for n, od := range cfg.Dict.DeptMap {
		if n != "" && od != "" {
			byName[n] = od
		}
	}
	if db != nil {
		if m, err := db.AllDepts(ctx); err == nil {
			for od, n := range m {
				if n == "" {
					continue
				}
				byID[od] = n
				if _, ok := byName[n]; !ok {
					byName[n] = od
				}
			}
		}
	}
	items, err := client.ListDepartments(ctx, "0")
	if err != nil {
		return byName, byID
	}
	for _, d := range items {
		if d.Name == "" || d.OpenDepartmentID == "" {
			continue
		}
		byName[d.Name] = d.OpenDepartmentID
		byID[d.OpenDepartmentID] = d.Name
		if d.DepartmentID != "" {
			byID[d.DepartmentID] = d.Name
		}
		if db != nil {
			_ = db.LearnDept(ctx, d.OpenDepartmentID, d.Name)
		}
	}
	return byName, byID
}

// findMirrorRecord 在采购镜像表里按 SourceID 找到对应审批实例的那一行（只读）。
func findMirrorRecord(ctx context.Context, cfg *config.Config, c *feishu.Client, appToken, tableID, instanceCode string) (*feishu.BitableRecord, error) {
	recs, err := c.SearchBitableRecords(ctx, appToken, tableID, nil, 500)
	if err != nil {
		return nil, err
	}
	sourceField := cfg.Field("purchase", "source_id")
	if sourceField == "" {
		sourceField = "SourceID"
	}
	for i := range recs {
		sid := textOfField(recs[i].Fields[sourceField])
		if sid == "" {
			continue
		}
		if code := instanceCodeFromSourceID(sid); code != "" &&
			(strings.HasPrefix(code, instanceCode) || strings.HasPrefix(instanceCode, code)) {
			return &recs[i], nil
		}
	}
	return nil, nil
}

// instanceCodeFromSourceID 从镜像表 SourceID（base64）里解出审批实例 code。
//
// 实测：base64 解出来形如
//
//	7685…:1D251D53-…-1:fb20…:1（真实实例号与序号已截断）
//
// 第二段去掉尾部 "-<序号>" 就是实例 code。
func instanceCodeFromSourceID(sid string) string {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sid))
	if err != nil {
		return ""
	}
	segs := strings.Split(string(b), ":")
	if len(segs) < 2 {
		return ""
	}
	part := segs[1]
	if i := strings.LastIndex(part, "-"); i > 0 {
		if _, err := strconv.Atoi(part[i+1:]); err == nil {
			return part[:i]
		}
	}
	return part
}

// buildApprovalApplink 拼审批原单深链（与 extract.go 的 buildMeta 同一口径）。
func buildApprovalApplink(cfg *config.Config, instanceCode string) string {
	return fmt.Sprintf("https://applink.feishu.cn/client/mini_program/open?mode=appCenter&appId=%s"+
		"&width=1136&height=750&path=pc%%2Fpages%%2Fin-process%%2Findex%%3FinstanceId%%3D%s",
		resolveApprovalAppID(cfg), instanceCode)
}

// ── 取值小工具 ──

// strVal 兼容"纯字符串"和"数值"两种 JSON 值。
func strVal(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

func floatVal(raw json.RawMessage) float64 {
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return v
		}
	}
	return 0
}

// intOfField 把多维表格日期字段读成 Unix 毫秒。
func intOfField(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return v
		}
	}
	return 0
}
