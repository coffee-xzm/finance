// sync 把抽取结果写进飞书多维表格「报销核对」表。
//
// 与旧版的根本区别：**一行 = 一条审批实例**（不是一张图）。
// 三个附件槽的图作为**附件字段**存进去（长期有效、可内联预览），
// 便于人工复核。技术性元信息（sha256/模型/置信度等）**只留本地**。
//
// 幂等：以「审批实例号」为准（同一实例重跑不会产生重复行）。
//
// 用法：
//
//	go run ./cmd/sync                 # 写全部
//	go run ./cmd/sync -dry-run        # 只打印
//	go run ./cmd/sync -update         # 已存在的行就地更新（保留人改的字段）
package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

// 人改的字段：更新时**绝不覆盖**。
var humanFields = []string{"人工审核", "审核备注", "已归档"}

func RunSync(opts SyncOptions) error {
	cfgPath, in, limit, dryRun, update := opts.CfgPath, opts.In, opts.Limit, opts.DryRun, opts.Update
	if in == "" {
		in = "data/extract/manifest.jsonl"
	}
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

	ms, err := readManifests(in, limit)
	if err != nil {
		return err
	}
	fmt.Printf("待同步 %d 个实例 → %s\n", len(ms), tableID)
	if dryRun {
		fmt.Println("（dry-run：不写入）")
	}
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	existing, err := existingInstances(ctx, client, appToken, tableID)
	if err != nil {
		return fmt.Errorf("读取已有记录失败: %w", err)
	}
	fmt.Printf("表中已有 %d 条\n\n", len(existing))

	var created, updated, skipped, failed int
	for _, m := range ms {
		if row, ok := existing[m.InstanceCode]; ok {
			recID := row.RecordID
			if !update {
				skipped++
				continue
			}
			autoPass := cfg.Review.AutoPass()
			fields, err := buildFields(ctx, client, appToken, m, dryRun, autoPass)
			if err != nil {
				failed++
				fmt.Printf("  ✗ %s 组装失败: %v\n", short(m.InstanceCode), err)
				continue
			}
			// 保留人已做出的判断（通过/驳回），绝不覆盖。
			for _, k := range humanFields {
				delete(fields, k)
			}
			// 但「人还没表态(待审) 且 机器核对一致」的行，允许自动通过 ——
			// 这正是 review.auto_pass_clean 的语义：没察觉到错误就不打扰人。
			if autoPass && row.Review == "待审" && verdictWord(m) == "一致" {
				fields["人工审核"] = "通过"
			}
			if dryRun {
				fmt.Printf("  ~ %s (update %s)\n", short(m.InstanceCode), recID)
				updated++
				continue
			}
			if err := client.UpdateBitableRecord(ctx, appToken, tableID, recID, fields); err != nil {
				failed++
				fmt.Printf("  ✗ %s 更新失败: %v\n", short(m.InstanceCode), err)
				continue
			}
			updated++
			fmt.Printf("  ~ %s 已更新\n", short(m.InstanceCode))
			continue
		}

		fields, err := buildFields(ctx, client, appToken, m, dryRun, cfg.Review.AutoPass())
		if err != nil {
			failed++
			fmt.Printf("  ✗ %s 组装失败: %v\n", short(m.InstanceCode), err)
			continue
		}
		if dryRun {
			b, _ := json.Marshal(fields)
			fmt.Printf("  + %s\n    %s\n\n", short(m.InstanceCode), trunc(string(b), 260))
			created++
			continue
		}
		id, err := client.CreateBitableRecord(ctx, appToken, tableID, fields)
		if err != nil {
			failed++
			fmt.Printf("  ✗ %s 写入失败: %v\n", short(m.InstanceCode), err)
			continue
		}
		created++
		fmt.Printf("  ✓ %s record=%s  %s\n", short(m.InstanceCode), id, brief(m))
	}

	fmt.Printf("\n完成：新增 %d，更新 %d，跳过 %d，失败 %d\n", created, updated, skipped, failed)
	return nil
}

// buildFields 把一个实例的抽取结果组装成「报销核对」表的一行。
func buildFields(ctx context.Context, c *feishu.Client, appToken string, m Manifest, dryRun, autoPass bool) (map[string]any, error) {
	// 人工审核初值由「核对结果」决定（策略见 config.review.auto_pass_clean）：
	//   一致   → 直接「通过」（只有出错才需要人）
	//   存疑/缺件 → 「待审」，并由 notify 发私信
	// 注意：更新已有行时调用方会删掉该字段，绝不覆盖人已做出的判断。
	initialReview := "待审"
	if autoPass && verdictWord(m) == "一致" {
		initialReview = "通过"
	}
	f := map[string]any{
		"审批实例号": m.InstanceCode,
		"人工审核":  initialReview,
		"已归档":   false,
	}
	setStr := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			f[k] = strings.TrimSpace(v)
		}
	}

	// ── 审批与表单字段 ──
	if mt := m.Meta; mt != nil {
		// ★ 超链接字段(type 15)必须写 {"link","text"} 对象，不能写纯字符串
		//   —— 否则报 1254068 URLFieldConvFail（实测踩过）。
		if mt.Applink != "" {
			f["申请编号"] = map[string]string{"link": mt.Applink, "text": "查看审批单"}
		}
		setStr("申请状态", statusWord(mt.Status))
		if mt.StartTime != "" {
			if ms, err := strconv.ParseInt(mt.StartTime, 10, 64); err == nil {
				f["发起时间"] = ms
			}
		}
		setStr("发起人", mt.Applicant)
		setStr("发起人部门", mt.ApplicantDept)
		if len(mt.Departments) > 0 {
			f["物资所属部门"] = mt.Departments
		}
		setStr("物资种类", mt.MaterialType)
		setStr("物资名称", mt.MaterialName)
		setStr("购买人", mt.Buyer)
		setStr("资金来源", mt.FundSource)
		setStr("是否走大创资金报销", mt.Dachuang)
		setStr("备注", mt.Remark)
	}

	// ── 图读结果（以发票为准）──
	var inv *FileMeta
	bySlot := map[string]*FileMeta{}
	for i := range m.Files {
		e := &m.Files[i]
		bySlot[e.Slot] = e
		if e.Kind == "invoice" && inv == nil {
			inv = e
		}
	}
	if inv != nil && inv.OCR != nil {
		if inv.OCR.AmountInclTaxCent != nil {
			f["图读金额(元)"] = centsToYuan(*inv.OCR.AmountInclTaxCent)
		}
		if inv.OCR.TaxCent != nil {
			f["图读税额(元)"] = centsToYuan(*inv.OCR.TaxCent)
		}
		if inv.OCR.Date != "" {
			if t, err := time.ParseInLocation("2006-01-02", inv.OCR.Date, time.Local); err == nil {
				f["图读日期"] = t.UnixMilli()
			}
		}
		setStr("销方名称", inv.OCR.Counterparty)
	}

	// ── 图片 → 附件字段（上传换 file_token）──
	for _, e := range m.Files {
		for _, p := range e.PNGs {
			var toks []map[string]string
			// ★ 上传名要跟**实际内容**一致：PDF 已被转成 PNG，
			//   若仍用原 PDF 文件名，人会点开一个叫 .pdf 的图片，产生困惑。
			upName := uploadName(e.Filename, p)
			if !dryRun {
				b, err := readLocal(p)
				if err != nil {
					return nil, fmt.Errorf("读 %s 失败: %w", p, err)
				}
				tok, err := c.UploadMedia(ctx, appToken, "bitable_image", upName, b)
				if err != nil {
					return nil, fmt.Errorf("上传 %s 失败: %w", upName, err)
				}
				toks = append(toks, map[string]string{"file_token": tok})
			}
			// 同一槽位多张图 → 追加到同一附件字段
			if prev, ok := f[e.Slot].([]map[string]string); ok {
				f[e.Slot] = append(prev, toks...)
			} else if !dryRun {
				f[e.Slot] = toks
			} else {
				f[e.Slot] = []map[string]string{{"file_token": "(dry-run)"}}
			}
		}
	}

	// ── 核对结论（人话，不含技术细节）──
	f["核对结果"] = verdictWord(m)
	setStr("差异说明", explain(m))
	return f, nil
}

// statusWord 把审批 API 的状态枚举映射成表里的中文选项。
//
// 不做映射会怎样：单选字段写入一个不存在的选项时，飞书会**自动新建选项**
// （或报错），表里就会出现中英混杂的状态。所以必须映射到现有选项。
var statusMap = map[string]string{
	"PENDING":  "审批中",
	"APPROVED": "已通过",
	"REJECTED": "已拒绝",
	"CANCELED": "已撤回",
	"DELETED":  "已删除",
}

func statusWord(s string) string {
	if w, ok := statusMap[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return w
	}
	return s
}

func verdictWord(m Manifest) string {
	if m.Match == nil {
		return "存疑"
	}
	switch m.Match.Verdict {
	case "MATCHED":
		return "一致"
	case "DEFECTIVE":
		return "缺件"
	default:
		return "存疑"
	}
}

// explain 生成给人看的差异说明；技术细节（哈希/模型/置信度）不进表。
func explain(m Manifest) string {
	var amt []string
	for _, e := range m.Files {
		if e.OCR == nil || e.OCR.AmountInclTaxCent == nil {
			continue
		}
		v := *e.OCR.AmountInclTaxCent
		if v < 0 {
			v = -v
		}
		amt = append(amt, fmt.Sprintf("%s %.2f", e.Slot, float64(v)/100))
	}
	s := strings.Join(amt, " / ")
	if m.Match != nil {
		for _, fd := range m.Match.Findings {
			switch fd.Code {
			case "DATE_SPAN":
				s += "；日期跨度异常，请核对单据日期"
			case "AMOUNT_SCALE_ERROR":
				s += "；金额疑似位数读错，请核对原图"
			case "AMOUNT_MISMATCH":
				s += "；三单金额不一致，请核对"
			case "NO_INVOICE":
				s += "；未识别到发票"
			}
		}
	}
	// 大写校验是抓"数字读错"的主手段，但它属于技术细节 —— 只转成人话提示
	for _, e := range m.Files {
		if e.UpperChk == "UPPER_MISMATCH" {
			s += "；发票小写与大写金额对不上（可能读错）"
			break
		}
	}
	return strings.TrimPrefix(s, " /")
}

func brief(m Manifest) string {
	if m.Meta == nil {
		return ""
	}
	amt := "?"
	if inv := findInvoice(m); inv != nil && inv.OCR != nil && inv.OCR.AmountInclTaxCent != nil {
		amt = fmt.Sprintf("%.2f", float64(*inv.OCR.AmountInclTaxCent)/100)
	}
	return fmt.Sprintf("%s %s %s元", m.Meta.Buyer, m.Meta.MaterialName, amt)
}

func findInvoice(m Manifest) *FileMeta {
	for i := range m.Files {
		if m.Files[i].Kind == "invoice" {
			return &m.Files[i]
		}
	}
	return nil
}

// readLocal 读取预处理后的图（manifest 里存的是相对 data/extract 的路径）。
func readLocal(p string) ([]byte, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join("data/extract", p)
	}
	return os.ReadFile(p)
}

func centsToYuan(c int64) float64 { return float64(c) / 100 }

// uploadName 让上传文件名跟实际内容的后缀一致。
// 多页 PDF 转出多张 PNG 时，用 -1/-2 区分，避免同名覆盖。
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

// existingRow 是源表里已有的一行（判断是否需要更新/自动通过用）。
type existingRow struct {
	RecordID string
	Review   string // 人工审核：待审/通过/驳回
	Verdict  string // 核对结果
}

// existingInstances 返回 审批实例号 → 已有行信息。
func existingInstances(ctx context.Context, c *feishu.Client, appToken, tableID string) (map[string]existingRow, error) {
	out := map[string]existingRow{}
	recs, err := c.SearchBitableRecords(ctx, appToken, tableID, nil, 500)
	if err != nil {
		return out, err
	}
	for _, r := range recs {
		raw, ok := r.Fields["审批实例号"]
		if !ok {
			continue
		}
		code := fieldText(raw)
		if code == "" {
			continue
		}
		out[code] = existingRow{
			RecordID: r.RecordID,
			Review:   fieldText(r.Fields["人工审核"]),
			Verdict:  fieldText(r.Fields["核对结果"]),
		}
	}
	return out, nil
}

// fieldText 兼容纯字符串与富文本片段数组两种返回形态。
func fieldText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var segs []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &segs) == nil && len(segs) > 0 {
		var sb strings.Builder
		for _, x := range segs {
			sb.WriteString(x.Text)
		}
		return sb.String()
	}
	return ""
}

func readManifests(path string, limit int) ([]Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败（先跑 extract）: %w", path, err)
	}
	defer f.Close()
	var out []Manifest
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var m Manifest
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		out = append(out, m)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 里没有可用记录", path)
	}
	return out, nil
}

// SyncOptions 配置一次"落表"运行。
type SyncOptions struct {
	CfgPath string
	In      string
	Limit   int
	DryRun  bool
	Update  bool
	// Recheck: 不读 manifest，直接按 review 策略修正源表里已有的行
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

	rows, err := existingInstances(ctx, client, appToken, tableID)
	if err != nil {
		return err
	}
	fmt.Printf("扫描源表 %d 行，策略：核对一致 → 自动通过\n", len(rows))
	fixed := 0
	for code, row := range rows {
		if row.Review != "待审" || row.Verdict != "一致" {
			continue
		}
		if dryRun {
			fmt.Printf("  将自动通过 %s\n", short(code))
			fixed++
			continue
		}
		if err := client.UpdateBitableRecord(ctx, appToken, tableID, row.RecordID,
			map[string]any{"人工审核": "通过", "审核备注": "自动通过（核对一致，无需人工）"}); err != nil {
			fmt.Printf("  ✗ %s 更新失败: %v\n", short(code), err)
			continue
		}
		fixed++
		fmt.Printf("  ✓ %s 已自动通过\n", short(code))
	}
	fmt.Printf("\n完成：补正 %d 行\n", fixed)
	return nil
}
