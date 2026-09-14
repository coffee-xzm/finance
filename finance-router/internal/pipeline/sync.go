// sync 把本地库里的抽取结果写进飞书多维表格「报销核对」表。
//
// 三条硬约束（都是实测/需求逼出来的，改之前先读这里）：
//
//  1. **一行 = 一张发票**（不是一条审批实例）。需求原话："一张发票一行"。
//     所以一条审批实例可以有 0..N 行；行与行的幂等键是「审批实例号 + 发票号码」。
//     旧的幂等键只有「审批实例号」，一单多票时第二张票会覆盖第一张。
//
//  2. 数据来源是**本地 SQLite**，不是 manifest.jsonl。
//     后者每次 extract 都被 os.Create **截断**，常驻服务一次只处理一个实例，
//     那个文件里永远只剩最后一个实例 —— 拿它当同步来源会漏掉其余全部。
//
//  3. 图片走**附件字段**（长期有效、可内联预览）。
//     审批附件的下载 URL 只签 24 小时，存成超链接第二天就点不开。
//
// 用法：
//
//	go run ./cmd/sync                 # 写全部
//	go run ./cmd/sync -dry-run        # 只打印
//	go run ./cmd/sync -update         # 已存在的行就地更新（保留人改的字段）
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/store"
)

// 人改的字段：更新时**绝不覆盖**。
var humanFields = []string{"人工审核", "审核备注", "已归档"}

// rowSep 是幂等键的分隔符。用不可见字符，避免发票号码里出现分隔符造成歧义。
const rowSep = "\x1f"

// rowKey 是"一行"的幂等键：审批实例号 + 发票号码。
//
// 发票号码为空时退化成只用实例号 —— 那种行是"没读到发票"的兜底行，
// 一条实例只该有一条。
func rowKey(instanceCode, invoiceNo string) string {
	return strings.TrimSpace(instanceCode) + rowSep + strings.TrimSpace(invoiceNo)
}

func RunSync(opts SyncOptions) error {
	cfgPath, limit, dryRun, update := opts.CfgPath, opts.Limit, opts.DryRun, opts.Update
	if opts.Recheck {
		return runRecheck(cfgPath, dryRun)
	}
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
	appToken := cfg.Feishu.Bitable.AppToken
	tableID := cfg.Feishu.Bitable.Tables["submission"]
	if appToken == "" || tableID == "" {
		return fmt.Errorf("配置缺少 bitable.app_token / tables.submission")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// ── 数据来源：本地库 ──
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()
	insts, err := db.SyncInstances(ctx, limit)
	if opts.Only != "" {
		insts, err = db.SyncInstanceOne(ctx, opts.Only)
	}
	if err != nil {
		return fmt.Errorf("读本地库失败: %w", err)
	}
	if len(insts) == 0 {
		return fmt.Errorf("本地库里没有实例（先跑 extract / 或服务还没收到审批）")
	}

	// 预演一遍分组，好把"要写多少行"提前告诉人。
	plans := make([][]plannedRow, len(insts))
	totalRows := 0
	for i, inst := range insts {
		plans[i] = planRows(inst)
		totalRows += len(plans[i])
	}
	fmt.Printf("本地库 %d 个实例 → %d 行（一张发票一行）→ %s\n", len(insts), totalRows, tableID)
	if dryRun {
		fmt.Println("（dry-run：不写入）")
	}
	fmt.Println()

	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	existing, err := existingRows(ctx, client, appToken, tableID)
	if err != nil {
		return fmt.Errorf("读取已有记录失败: %w", err)
	}
	byKey := map[string]existingRow{}
	for _, r := range existing {
		byKey[rowKey(r.Code, r.InvoiceNo)] = r
	}
	fmt.Printf("表中已有 %d 条\n\n", len(existing))

	var created, updated, skipped, failed int
	for i, inst := range insts {
		code := inst.Sub.InstanceCode
		rows := plans[i]

		// 旧表里"只有实例号、没有发票号码"的行：接管给本实例的**第一行**，
		// 免得升级后凭空多出一行重复记录。
		legacy, hasLegacy := byKey[rowKey(code, "")]

		for n, row := range rows {
			recID := ""
			var old existingRow
			if r, ok := byKey[rowKey(code, row.InvoiceNo)]; ok {
				recID, old = r.RecordID, r
			} else if n == 0 && hasLegacy && !legacy.Taken {
				recID, old = legacy.RecordID, legacy
				legacy.Taken = true
				byKey[rowKey(code, "")] = legacy
			}

			if recID != "" && !update {
				skipped++
				continue
			}
			fields, err := buildRowFields(ctx, client, appToken, row, dryRun)
			if err != nil {
				failed++
				fmt.Printf("  ✗ %s #%d 组装失败: %v\n", short(code), row.GroupIdx, err)
				continue
			}
			if recID != "" {
				// 保留人已做出的判断（通过/驳回），绝不覆盖。
				for _, k := range humanFields {
					delete(fields, k)
				}
				// ★ 差异说明里可能带着 notify 打的「已通知」标记 —— 不能抹掉。
				//   抹掉的后果是每同步一次就重新私信打扰一次。
				if strings.Contains(old.Note, notifiedMark) {
					if s, _ := fields["差异说明"].(string); !strings.Contains(s, notifiedMark) {
						fields["差异说明"] = strings.TrimSpace(s + " " + notifiedMark)
					}
				}
				// 「人还没表态 且 该行机器核对一致」→ 自动通过。
				// 这就是 review.auto_pass_clean 的语义：没察觉到错误就不打扰人。
				if cfg.Review.AutoPass() && old.Review == "待审" && row.Verdict == "一致" && row.Problem == "" {
					fields["人工审核"] = "通过"
				}
				if dryRun {
					fmt.Printf("  ~ %s #%d %s\n", short(code), row.GroupIdx, row.title())
					updated++
					continue
				}
				if err := client.UpdateBitableRecord(ctx, appToken, tableID, recID, fields); err != nil {
					failed++
					fmt.Printf("  ✗ %s #%d 更新失败: %v\n", short(code), row.GroupIdx, err)
					continue
				}
				updated++
				fmt.Printf("  ~ %s #%d 已更新  %s\n", short(code), row.GroupIdx, row.title())
				continue
			}

			if dryRun {
				b, _ := json.Marshal(fields)
				fmt.Printf("  + %s #%d %s\n    %s\n\n", short(code), row.GroupIdx, row.title(), trunc(string(b), 260))
				created++
				continue
			}
			id, err := client.CreateBitableRecord(ctx, appToken, tableID, fields)
			if err != nil {
				failed++
				fmt.Printf("  ✗ %s #%d 写入失败: %v\n", short(code), row.GroupIdx, err)
				continue
			}
			created++
			fmt.Printf("  ✓ %s #%d record=%s  %s\n", short(code), row.GroupIdx, id, row.title())
		}
	}

	fmt.Printf("\n完成：新增 %d，更新 %d，跳过 %d，失败 %d\n", created, updated, skipped, failed)
	return nil
}

// ────────────────────────── 行规划 ──────────────────────────

// plannedRow 是**即将写进表的一行**：一张发票 + 它的订单/付款。
type plannedRow struct {
	Sub      store.Submission
	Group    *store.DocGroup // nil = 实例兜底行（没有分成任何组时）
	GroupIdx int             // 展示序号，从 1 开始；兜底行为 0
	// InvoiceNo 是幂等键的一半。优先取分组的发票号码，缺失时由发票证据兜底。
	InvoiceNo string

	InvoiceEvs []store.Evidence
	OrderEvs   []store.Evidence
	PaymentEvs []store.Evidence
	AllEvs     []store.Evidence // 兜底行用：整实例的全部图

	Verdict string // 一致 / 存疑 / 缺件
	Problem string // 非空 = 不能自动通过，必须人看
	Explain string // 给人看的差异说明（不含哈希/模型等技术细节）
}

func (r plannedRow) title() string {
	amt := "?"
	if r.Group != nil && r.Group.InvoiceTotalCent != nil {
		amt = yuan(*r.Group.InvoiceTotalCent)
	}
	return fmt.Sprintf("发票=%s %s元  %s", shortTail(r.InvoiceNo, 6), amt, r.Verdict)
}

// planRows 把一个实例拆成"一张发票一行"。
func planRows(inst store.SyncInstance) []plannedRow {
	idx := indexEvidence(inst.Evidence)

	// 实例级的毛病：形态不合规、或有单据没配上。
	// 需求：「在审的如果有任何不符合要求，或无法匹配的情况，进入报销核对表单」。
	instProblem := ""
	if inst.Sub.FormProblem != "" {
		instProblem = "提交形态不符合规定：" + inst.Sub.FormProblem
	}
	if n := len(inst.Sub.Leftover); n > 0 {
		instProblem = joinProblem(instProblem,
			fmt.Sprintf("有 %d 份单据没能配到任何发票（%s）", n, strings.Join(inst.Sub.Leftover, "、")))
	}

	if len(inst.Groups) == 0 {
		// 兜底：一张发票都没分出来（没识别到、或完全配不上）。
		r := plannedRow{Sub: inst.Sub, AllEvs: inst.Evidence, Problem: instProblem}
		if !hasKind(inst.Evidence, "invoice") {
			r.Verdict = "缺件"
			r.Problem = joinProblem(r.Problem, "未识别到发票")
		} else {
			r.Verdict = "存疑"
			r.Problem = joinProblem(r.Problem, "识别到发票，但没能与订单/付款配成一组")
		}
		if inv := firstOfKind(inst.Evidence, "invoice"); inv != nil {
			r.InvoiceNo = inv.InvoiceNo
			r.InvoiceEvs = []store.Evidence{*inv}
		}
		r.Explain = explainRow(r)
		return []plannedRow{r}
	}

	out := make([]plannedRow, 0, len(inst.Groups))
	for i := range inst.Groups {
		g := &inst.Groups[i]
		r := plannedRow{
			Sub: inst.Sub, Group: g, GroupIdx: g.GroupIndex + 1,
			InvoiceNo:  g.InvoiceNo,
			InvoiceEvs: resolveOne(idx, g.InvoiceEv, g.InvoiceSlot),
			OrderEvs:   resolveMany(idx, g.OrderEvs, g.OrderSlots),
			PaymentEvs: resolveMany(idx, g.PaymentEvs, g.PaymentSlots),
		}
		// 分组的发票号码缺失时，用发票证据里的号码兜底。
		if r.InvoiceNo == "" && len(r.InvoiceEvs) > 0 {
			r.InvoiceNo = r.InvoiceEvs[0].InvoiceNo
		}
		switch {
		case !g.Matched:
			r.Verdict = "存疑"
			r.Problem = joinProblem(r.Problem, amountProblem(g))
		case instProblem != "":
			r.Verdict = "存疑"
			r.Problem = instProblem
		default:
			r.Verdict = "一致"
		}
		r.Explain = explainRow(r)
		out = append(out, r)
	}
	return out
}

// amountProblem 用人话描述"发票与支撑单据对不上"。
func amountProblem(g *store.DocGroup) string {
	if g.InvoiceTotalCent == nil || g.SupportTotalCent == nil {
		return "发票金额或支撑单据金额缺失，无法核对"
	}
	return fmt.Sprintf("发票 %s 元，订单/付款合计 %s 元，对不上",
		yuan(*g.InvoiceTotalCent), yuan(*g.SupportTotalCent))
}

// explainRow 生成给人看的差异说明；技术细节（哈希/模型/置信度）不进表。
func explainRow(r plannedRow) string {
	var parts []string
	if r.Group != nil {
		if r.Group.InvoiceTotalCent != nil {
			parts = append(parts, "发票 "+yuan(*r.Group.InvoiceTotalCent)+" 元")
		}
		if n := len(r.OrderEvs); n > 0 {
			parts = append(parts, fmt.Sprintf("订单 %d 张", n))
		} else if r.Group.SupportTotalCent != nil && len(r.PaymentEvs) == 0 {
			parts = append(parts, "无订单截图")
		}
		if n := len(r.PaymentEvs); n > 0 {
			parts = append(parts, fmt.Sprintf("付款 %d 张", n))
		}
	}
	s := strings.Join(parts, " / ")

	if r.Group != nil {
		for _, reason := range r.Group.Reasons {
			s = appendSentence(s, reason)
		}
	}
	// 大写校验是抓"数字读错"的主手段，但它属于技术细节 —— 只转成人话提示。
	for _, e := range append(append([]store.Evidence{}, r.InvoiceEvs...), r.OrderEvs...) {
		if e.UpperCheck == "UPPER_MISMATCH" {
			s = appendSentence(s, "发票小写与大写金额对不上（可能读错）")
			break
		}
	}
	for _, e := range r.InvoiceEvs {
		if e.TaxCheck != "" && e.TaxCheck != "OK" {
			s = appendSentence(s, "发票税额自检异常（可能读错）")
			break
		}
	}
	if r.Problem != "" {
		s = appendSentence(s, r.Problem)
	}
	if s == "" {
		s = "无差异"
	}
	return s
}

// ────────────────────────── 字段组装 ──────────────────────────

// buildRowFields 把一行组装成「报销核对」表的字段。
func buildRowFields(ctx context.Context, c *feishu.Client, appToken string, r plannedRow, dryRun bool) (map[string]any, error) {
	// 人工审核初值由核对结果决定（见 config.review.auto_pass_clean）：
	//   一致 → 直接「通过」（只有出错才需要人）
	//   其余 → 「待审」，并由 notify 发私信
	// 注意：更新已有行时调用方会删掉该字段，绝不覆盖人已做出的判断。
	initialReview := "待审"
	if r.Verdict == "一致" && r.Problem == "" {
		initialReview = "通过"
	}

	f := map[string]any{
		"审批实例号": r.Sub.InstanceCode,
		"人工审核":  initialReview,
		"已归档":   false,
	}
	setStr := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			f[k] = strings.TrimSpace(v)
		}
	}

	// ── 审批与表单字段 ──
	// ★ 超链接字段(type 15)必须写 {"link","text"} 对象，不能写纯字符串
	//   —— 否则报 1254068 URLFieldConvFail（实测踩过）。
	if r.Sub.Applink != "" {
		f["申请编号"] = map[string]string{"link": r.Sub.Applink, "text": "查看审批单"}
	}
	setStr("申请状态", statusWord(r.Sub.Status))
	if r.Sub.StartTimeMS != nil {
		f["发起时间"] = *r.Sub.StartTimeMS
	}
	setStr("发起人", r.Sub.Applicant)
	setStr("发起人部门", r.Sub.ApplicantDept)
	if len(r.Sub.Departments) > 0 {
		f["归属组"] = r.Sub.Departments
	}
	setStr("物资种类", r.Sub.MaterialType)
	setStr("物资名称", r.Sub.MaterialName)
	setStr("购买人", r.Sub.Buyer)
	setStr("资金来源", r.Sub.FundSource)
	setStr("是否走大创资金报销", r.Sub.Dachuang)
	setStr("是否为支付宝付款", r.Sub.IsAlipay)
	setStr("备注", r.Sub.Remark)
	setStr("审批实例号", r.Sub.InstanceCode)

	// ── 本行（一张发票）──
	setStr("发票号码", r.InvoiceNo)
	if len(r.InvoiceEvs) > 0 {
		setStr("发票号码来源", r.InvoiceEvs[0].InvoiceNoSrc)
	}
	if r.GroupIdx > 0 {
		f["分组序号"] = r.GroupIdx
	}
	setStr("订单号", joinDistinct(r.OrderEvs, func(e store.Evidence) string { return e.OrderNo }))
	setStr("支付宝交易号", joinDistinct(r.OrderEvs, func(e store.Evidence) string { return e.AlipayTxnID }))
	setStr("配对依据", strings.Join(groupReasons(r.Group), "；"))

	// ── 图读结果（以本行发票为准）──
	inv := firstEvidence(r.InvoiceEvs)
	if inv == nil {
		inv = firstOfKind(r.AllEvs, "invoice")
	}
	if inv != nil {
		if inv.AmountInclTaxCent != nil {
			f["图读金额(元)"] = centsToYuan(*inv.AmountInclTaxCent)
		}
		if inv.TaxCent != nil {
			f["图读税额(元)"] = centsToYuan(*inv.TaxCent)
		}
		if inv.Date != "" {
			if t, err := time.ParseInLocation("2006-01-02", inv.Date, time.Local); err == nil {
				f["图读日期"] = t.UnixMilli()
			}
		}
		setStr("销方名称", inv.Counterparty)
	}

	// ── 图片 → 附件字段（上传换 file_token）──
	if err := attach(ctx, c, appToken, f, "发票文件", r.InvoiceEvs, dryRun); err != nil {
		return nil, err
	}
	if err := attach(ctx, c, appToken, f, "订单截图", r.OrderEvs, dryRun); err != nil {
		return nil, err
	}
	if err := attach(ctx, c, appToken, f, "付款截图", r.PaymentEvs, dryRun); err != nil {
		return nil, err
	}
	// 兜底行：把整实例的图都挂上（否则人工看不到任何图）。
	if r.Group == nil {
		if err := attach(ctx, c, appToken, f, "发票文件", inKind(r.AllEvs, "invoice"), dryRun); err != nil {
			return nil, err
		}
		if err := attach(ctx, c, appToken, f, "订单截图", inKind(r.AllEvs, "order"), dryRun); err != nil {
			return nil, err
		}
		if err := attach(ctx, c, appToken, f, "付款截图", inKind(r.AllEvs, "payment"), dryRun); err != nil {
			return nil, err
		}
	}

	// ── 核对结论 ──
	f["核对结果"] = r.Verdict
	setStr("差异说明", r.Explain)
	return f, nil
}

// attach 把若干证据的本地图上传进某个附件字段。
//
// 同一槽位多张图（一发票多订单）→ 追加到同一个附件单元格。
func attach(ctx context.Context, c *feishu.Client, appToken string,
	f map[string]any, slot string, evs []store.Evidence, dryRun bool) error {

	var toks []map[string]string
	for _, e := range evs {
		pngs := e.LocalPNGs
		if len(pngs) == 0 && e.LocalPNG != "" {
			pngs = []string{e.LocalPNG}
		}
		if len(pngs) == 0 {
			continue
		}
		for _, p := range pngs {
			// ★ 上传名要跟**实际内容**一致：PDF 已被转成 PNG，
			//   若仍用原 PDF 文件名，人会点开一个叫 .pdf 的图片，产生困惑。
			upName := uploadName(e.Filename, p)
			if dryRun {
				toks = append(toks, map[string]string{"file_token": "(dry-run)"})
				continue
			}
			b, err := readLocal(p)
			if err != nil {
				return fmt.Errorf("读 %s 失败: %w", p, err)
			}
			tok, err := c.UploadMedia(ctx, appToken, "bitable_image", upName, b)
			if err != nil {
				return fmt.Errorf("上传 %s 失败: %w", upName, err)
			}
			toks = append(toks, map[string]string{"file_token": tok})
		}
	}
	if len(toks) == 0 {
		return nil
	}
	if prev, ok := f[slot].([]map[string]string); ok {
		f[slot] = append(prev, toks...)
		return nil
	}
	f[slot] = toks
	return nil
}

// ────────────────────────── 证据索引 ──────────────────────────

// evIndex 是某实例证据的两套索引：按 "槽位:序号" 精确查，按槽位名模糊查。
type evIndex struct {
	byRef  map[string]store.Evidence
	bySlot map[string][]store.Evidence
	all    []store.Evidence
}

func indexEvidence(evs []store.Evidence) evIndex {
	ix := evIndex{byRef: map[string]store.Evidence{}, bySlot: map[string][]store.Evidence{}, all: evs}
	for _, e := range evs {
		ix.byRef[store.EvRef(e.Slot, e.IndexNo)] = e
		ix.bySlot[e.Slot] = append(ix.bySlot[e.Slot], e)
	}
	return ix
}

// resolveOne 解析单个"槽位:序号"引用。
//
// 向后兼容：0005 之前的分组只存槽位名（不含 ":"），那种旧值退化成
// "该槽位的全部图" —— 旧数据本来也分不清是哪一张。
func resolveOne(ix evIndex, ref, slot string) []store.Evidence {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	if strings.Contains(ref, ":") {
		if e, ok := ix.byRef[ref]; ok {
			return []store.Evidence{e}
		}
		return nil
	}
	return ix.bySlot[ref]
}

// resolveMany 解析一串引用；refs 为空时退回槽位名列表，再退回空。
func resolveMany(ix evIndex, refs, slots []string) []store.Evidence {
	var out []store.Evidence
	for _, r := range refs {
		for _, e := range resolveOne(ix, r, "") {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		for _, s := range slots {
			out = append(out, ix.bySlot[s]...)
		}
	}
	return dedupEvidence(out)
}

func dedupEvidence(in []store.Evidence) []store.Evidence {
	seen := map[string]bool{}
	var out []store.Evidence
	for _, e := range in {
		k := store.EvRef(e.Slot, e.IndexNo)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}

func inKind(evs []store.Evidence, kind string) []store.Evidence {
	var out []store.Evidence
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func hasKind(evs []store.Evidence, kind string) bool { return firstOfKind(evs, kind) != nil }

func firstOfKind(evs []store.Evidence, kind string) *store.Evidence {
	for i := range evs {
		if evs[i].Kind == kind {
			return &evs[i]
		}
	}
	return nil
}

func firstEvidence(evs []store.Evidence) *store.Evidence {
	if len(evs) == 0 {
		return nil
	}
	return &evs[0]
}

func groupReasons(g *store.DocGroup) []string {
	if g == nil {
		return nil
	}
	return g.Reasons
}

func joinDistinct(evs []store.Evidence, get func(store.Evidence) string) string {
	var out []string
	seen := map[string]bool{}
	for _, e := range evs {
		v := strings.TrimSpace(get(e))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return strings.Join(out, " / ")
}

// ────────────────────────── 小工具 ──────────────────────────

// statusWord 把审批 API 的状态枚举映射成表里的中文选项。
//
// 不做映射会怎样：单选字段写入一个不存在的选项时，飞书会**自动新建选项**
// （或报错），表里就会出现中英混杂的状态。所以必须映射到现有选项。
var statusMap = map[string]string{
	"PENDING":   "审批中",
	"APPROVED":  "已通过",
	"REJECTED":  "已拒绝",
	"CANCELED":  "已取消",
	"WITHDRAWN": "已撤回",
	"DELETED":   "已删除",
}

func statusWord(s string) string {
	if w, ok := statusMap[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return w
	}
	return s
}

func yuan(cent int64) string {
	neg := ""
	if cent < 0 {
		neg, cent = "-", -cent
	}
	return fmt.Sprintf("%s%d.%02d", neg, cent/100, cent%100)
}

func centsToYuan(c int64) float64 { return float64(c) / 100 }

func joinProblem(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "；" + b
}

// appendSentence 把一句补充说明接在已有文本后面，自动处理分隔符。
func appendSentence(s, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return s
	}
	if strings.TrimSpace(s) == "" {
		return extra
	}
	return s + "；" + extra
}

func shortTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(无号)"
	}
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// readLocal 读取预处理后的图（库里存的是相对 data/extract 的路径）。
func readLocal(p string) ([]byte, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join("data/extract", p)
	}
	return os.ReadFile(p)
}

// uploadName 让上传文件名跟实际内容的后缀一致。
// 多页 PDF 转出多张 PNG 时，用 -第N页 区分，避免同名覆盖。
func uploadName(origName, localPath string) string {
	base := strings.TrimSuffix(origName, filepath.Ext(origName))
	if base == "" {
		base = filepath.Base(localPath)
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	ext := strings.ToLower(filepath.Ext(localPath))
	if ext == "" {
		ext = ".png"
	}
	// 多页：invoice-1-1.png / invoice-1-2.png → 追加页码
	if m := regexp.MustCompile(`-(\d+)$`).FindStringSubmatch(strings.TrimSuffix(filepath.Base(localPath), ext)); len(m) == 2 {
		if page := m[1]; page != "1" {
			base = base + "-第" + page + "页"
		}
	}
	return base + ext
}

// ────────────────────────── 已有行 ──────────────────────────

// existingRow 是源表里已有的一行。
type existingRow struct {
	RecordID  string
	Code      string // 审批实例号
	InvoiceNo string // 发票号码（旧行可能为空）
	Review    string // 人工审核：待审/通过/驳回
	Verdict   string // 核对结果
	Note      string // 差异说明（可能带 notify 打的「已通知」标记）

	// Taken 只在 RunSync 内部用于"旧行已被本实例的第一行接管"的标记。
	Taken bool
}

// existingRows 返回源表里的全部行。
func existingRows(ctx context.Context, c *feishu.Client, appToken, tableID string) ([]existingRow, error) {
	recs, err := c.SearchBitableRecords(ctx, appToken, tableID, nil, 500)
	if err != nil {
		return nil, err
	}
	var out []existingRow
	for _, r := range recs {
		code := textOfField(r.Fields["审批实例号"])
		if code == "" {
			continue
		}
		out = append(out, existingRow{
			RecordID:  r.RecordID,
			Code:      code,
			InvoiceNo: textOfField(r.Fields["发票号码"]),
			Review:    textOfField(r.Fields["人工审核"]),
			Verdict:   textOfField(r.Fields["核对结果"]),
			Note:      textOfField(r.Fields["差异说明"]),
		})
	}
	return out, nil
}

// PurgeInstance 从「报销核对」表删掉某审批实例的**全部行**。
//
// 用途：审批被退回/撤回时，该单不该继续留在核对表里（需求原话：
// "被退回的就剔除掉"）。本地库用 store.DeleteInstance 清，表里用本函数清。
func PurgeInstance(ctx context.Context, cfg *config.Config, instanceCode string) (int, error) {
	appToken := cfg.Feishu.Bitable.AppToken
	tableID := cfg.Feishu.Bitable.Tables["submission"]
	if appToken == "" || tableID == "" {
		return 0, fmt.Errorf("配置缺少 bitable.app_token / tables.submission")
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	rows, err := existingRows(ctx, client, appToken, tableID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.Code != instanceCode {
			continue
		}
		if err := client.DeleteBitableRecord(ctx, appToken, tableID, r.RecordID); err != nil {
			return n, fmt.Errorf("删除 record=%s 失败: %w", r.RecordID, err)
		}
		n++
	}
	return n, nil
}

// ────────────────────────── 补正 ──────────────────────────

// SyncOptions 配置一次"落表"运行。
type SyncOptions struct {
	CfgPath string
	In      string // 已废弃：数据来源改为本地库，保留仅为兼容旧的命令行参数
	Limit   int
	DryRun  bool
	Update  bool
	// Only 非空时只处理这一个审批实例（常驻服务用：避免每次重传全部实例的附件）。
	Only string
	// Recheck: 不读本地库，直接按 review 策略修正源表里已有的行
	// （把「待审 且 核对一致」的自动改为「通过」）。
	// 用于**策略变更后补正历史数据**，不需要重新下载与识别。
	Recheck bool
}

// runRecheck 按 review 策略修正源表里已有的行：把「待审 且 核对一致」改为「通过」。
//
// 为什么需要：auto_pass_clean 是策略，策略可能后加。
// 加进来之前的行仍是「待审」，但它们其实没问题 —— 本函数补正这些历史行。
// **绝不改动人已表态的行**（通过/驳回保持原样）。
func runRecheck(cfgPath string, dryRun bool) error {
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
	if !cfg.Review.AutoPass() {
		fmt.Println("review.auto_pass_clean = false，无需补正")
		return nil
	}
	appToken := cfg.Feishu.Bitable.AppToken
	tableID := cfg.Feishu.Bitable.Tables["submission"]
	if appToken == "" || tableID == "" {
		return fmt.Errorf("配置缺少 bitable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	rows, err := existingRows(ctx, client, appToken, tableID)
	if err != nil {
		return err
	}
	fmt.Printf("扫描源表 %d 行，策略：核对一致 → 自动通过\n", len(rows))
	fixed := 0
	for _, row := range rows {
		if row.Review != "待审" || row.Verdict != "一致" {
			continue
		}
		if dryRun {
			fmt.Printf("  将自动通过 %s %s\n", short(row.Code), shortTail(row.InvoiceNo, 6))
			fixed++
			continue
		}
		if err := client.UpdateBitableRecord(ctx, appToken, tableID, row.RecordID,
			map[string]any{"人工审核": "通过", "审核备注": "自动通过（核对一致，无需人工）"}); err != nil {
			fmt.Printf("  ✗ %s 更新失败: %v\n", short(row.Code), err)
			continue
		}
		fixed++
		fmt.Printf("  ✓ %s %s 已自动通过\n", short(row.Code), shortTail(row.InvoiceNo, 6))
	}
	fmt.Printf("\n完成：补正 %d 行\n", fixed)
	return nil
}
