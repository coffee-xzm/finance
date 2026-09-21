// Package export 实现「按清单导出多维表格附件」。
//
// 口径（docs/30-review/32 §3.1，用户 2026-09-19 拍板）：
//
//	人在飞书里勾选行 → 导出成一份清单（一行一个 审批实例号 或 record_id）
//	→ 本包解析清单 → 校验 → 取附件临时链接（一次 ≤5 个）→ 下载
//	→ 按内部命名规则重命名/分组 → zip + manifest.csv → 落盘 + 写审计。
//
// 三条纪律：
//  1. **只存 file_token，不存临时链接**（链接 24 小时失效，风险 R1）；
//  2. **失败不静默**：每个文件一条结果，空附件行单独统计（学 `处理说明` 的教训）；
//  3. 命名与分组**复用 internal/naming**（与插件侧共用同一份冻结向量）。
package export

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/bitable"
	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/naming"
	"github.com/coffee/finance-router/internal/store"
)

// Options 是一次导出的全部入参（与 cmd/export 的命令行参数一一对应）。
type Options struct {
	CfgPath string

	Table       string   // 表：integrated|submission|tblXXX|报销整合|报销核对
	RecordsFile string   // 清单文件（一行一个标识）——主路径
	Records     []string // 清单直接写在命令行（与 RecordsFile 合并）
	ViewID      string   // 视图口径（与清单二选一）

	Slots    []string // 附件槽位，默认 发票/订单截图/付款记录
	Naming   string   // 命名模板，默认取配置
	GroupBy  string   // department | month | none
	OutDir   string   // 输出目录
	NASRoot  string   // NAS 根，空=不复制
	DryRun   bool     // 只规划不下载
	Force    bool     // 覆盖已存在文件（默认跳过）
	Extra    string   // 高级权限表的 extra
	Operator string   // 操作人（审计）

	// AllowMissing: 清单里有对不上的标识时是否继续。
	// 默认 false —— 清单是人工挑的，对不上就该让人看见并修正（计划 §3.3）。
	AllowMissing bool
	// MaxFiles 单批上限，0 = 取配置。
	MaxFiles int
}

// summary 是一次导出的结果统计。
type summary struct {
	Records      int
	Planned      int
	Success      int
	Failed       int
	Skipped      int
	EmptyRows    int
	EmptyBySlot  map[string]int
	Warnings     []naming.Warning
	MissingIDs   []string
	NotFoundIDs  []string
	ManifestPath string
	ZipPath      string
}

// Run 执行一次导出。
func Run(ctx context.Context, opts Options) error {
	started := time.Now()
	cfg, err := loadConfig(opts.CfgPath)
	if err != nil {
		return err
	}
	applyDefaults(&opts, cfg)

	if opts.Table == "" {
		opts.Table = "integrated"
	}
	tableID, tableName, err := resolveTable(cfg, opts.Table)
	if err != nil {
		return err
	}
	if opts.OutDir == "" {
		return fmt.Errorf("必须给 -out 指定输出目录")
	}
	if opts.ViewID == "" && len(opts.Records) == 0 && opts.RecordsFile == "" {
		return fmt.Errorf("请给 -records-file / -records（清单）或 -view（视图）之一")
	}
	if opts.ViewID != "" && (len(opts.Records) > 0 || opts.RecordsFile != "") {
		return fmt.Errorf("-view 与清单口径不能同时用：视图导出是整批，清单是挑选，混在一起口径不清")
	}

	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	fmt.Println("═══ 附件选择性导出 ═══")
	fmt.Printf("  表          %s (%s)\n", tableName, tableID)
	fmt.Printf("  槽位        %s\n", strings.Join(opts.Slots, " / "))
	fmt.Printf("  命名模板    %s\n", orDefault(opts.Naming, naming.DefaultTemplate))
	fmt.Printf("  分组        %s\n", opts.GroupBy)
	fmt.Printf("  输出        %s\n", opts.OutDir)
	if opts.DryRun {
		fmt.Println("  模式        ★ DRY-RUN（只规划，不下载）")
	}

	// ── 1. 取记录集合 ──
	scope := "view"
	listSHA := ""
	listLines := 0
	var records []feishu.BitableRecord

	if opts.ViewID != "" {
		fmt.Printf("  视图        %s\n", opts.ViewID)
		records, err = client.SearchBitableRecordsAll(ctx,
			cfg.Feishu.Bitable.AppToken, tableID,
			feishu.SearchOptions{ViewID: opts.ViewID, FieldNames: neededFields(opts.Slots)})
		if err != nil {
			return fmt.Errorf("按视图拉记录: %w", err)
		}
		fmt.Printf("  ✓ 视图内 %d 行\n", len(records))
	} else {
		scope = "list"
		ids, sha, lines, err := LoadList(opts.RecordsFile, opts.Records)
		if err != nil {
			return err
		}
		listSHA, listLines = sha, lines
		fmt.Printf("  清单        %d 行有效标识（文件 %d 行，sha256=%s…）\n",
			len(ids), lines, short(listSHA))

		res, err := fetchByList(ctx, client, cfg, tableID, ids, opts.AllowMissing)
		if err != nil {
			return err
		}
		records = res.Records
		if len(res.Missing) > 0 {
			fmt.Printf("  ✗ 有 %d 个标识对不上：\n", len(res.Missing))
			for _, m := range res.Missing {
				fmt.Printf("      第 %d 行 %q —— %s\n", m.Line, m.Value, m.Reason)
			}
			if !opts.AllowMissing {
				return fmt.Errorf("清单里有对不上的标识（见上）；确认无误可加 -allow-missing 跳过")
			}
		}
		if len(res.Expanded) > 0 {
			fmt.Printf("  ℹ 有 %d 个审批实例号对应多行（一张发票一行），已展开为 %d 行\n",
				len(res.Expanded), len(records))
		}
	}

	if len(records) == 0 {
		return fmt.Errorf("记录集合为空，没有东西可导")
	}
	if opts.MaxFiles > 0 {
		est := estimateFiles(records, opts.Slots)
		if est > opts.MaxFiles {
			return fmt.Errorf("预计 %d 个文件，超过单批上限 %d —— 请分批导出（或调 export.max_files_per_batch）",
				est, opts.MaxFiles)
		}
	}

	// ── 2. 规划文件名 ──
	rows := make([]naming.Row, 0, len(records))
	for _, r := range records {
		rows = append(rows, toNamingRow(r, opts.Slots))
	}
	plan := naming.PlanRows(rows, naming.Options{
		Template: opts.Naming,
		GroupBy:  opts.GroupBy,
	})
	if len(plan.Files) == 0 {
		return fmt.Errorf("按当前槽位设置没有任何可导出的附件（%d 行都没有附件）", len(records))
	}

	s := &summary{Records: len(records), Planned: len(plan.Files), EmptyBySlot: map[string]int{}}
	for _, w := range plan.Warnings {
		s.Warnings = append(s.Warnings, w)
		if w.Code == "NO_ATTACHMENTS" {
			s.EmptyRows++
		}
	}
	countEmptySlots(records, opts.Slots, s)

	fmt.Printf("\n── 规划结果 ──\n")
	fmt.Printf("  行 %d → 文件 %d（发票 %d / 订单 %d / 付款 %d，合计 %.1f MB）\n",
		len(records), len(plan.Files),
		plan.Counts[naming.SlotInvoice], plan.Counts[naming.SlotOrder], plan.Counts[naming.SlotPayment],
		float64(plan.TotalBytes)/1024/1024)
	printWarnings(plan.Warnings)
	if s.EmptyRows > 0 {
		fmt.Printf("  ⚠ 有 %d 行的三个附件列都是空的（不会产出文件）\n", s.EmptyRows)
	}
	for _, slot := range opts.Slots {
		if n := s.EmptyBySlot[slot]; n > 0 {
			fmt.Printf("  ⚠ 有 %d 行的「%s」为空\n", n, slot)
		}
	}
	printPlan(plan.Files, opts.DryRun)

	if opts.DryRun {
		fmt.Println("\n★ DRY-RUN 结束：未下载任何文件。")
		return nil
	}

	// ── 3. 下载 + 命名落盘 ──
	fmt.Printf("\n── 下载（现取现下，一次 %d 个 token）──\n", feishu.MaxTmpDownloadTokens)
	items, err := download(ctx, client, opts, plan.Files, s)
	if err != nil {
		return err
	}

	// ── 4. manifest + zip ──
	s.ManifestPath = filepath.Join(opts.OutDir, "manifest.csv")
	if err := writeManifest(s.ManifestPath, items, opts, tableID, tableName, started); err != nil {
		return err
	}
	fmt.Printf("  ✓ manifest: %s\n", s.ManifestPath)

	if cfg.Export.ZipEnabled() {
		zipPath := opts.OutDir + ".zip"
		if err := zipDir(opts.OutDir, zipPath); err != nil {
			return fmt.Errorf("打 zip: %w", err)
		}
		s.ZipPath = zipPath
		fmt.Printf("  ✓ zip: %s\n", zipPath)
		if opts.NASRoot != "" {
			dest, err := copyToNAS(zipPath, opts.NASRoot)
			if err != nil {
				fmt.Printf("  ⚠ 复制到 NAS 失败（zip 已在本机，不影响交付）: %v\n", err)
			} else {
				fmt.Printf("  ✓ NAS: %s\n", dest)
			}
		}
	}

	// ── 5. 审计 ──
	batchID := "export-" + started.Format("20060102-150405")
	if err := writeAudit(cfg, batchID, scope, tableName, tableID, opts, listSHA, listLines, s, items, started); err != nil {
		// 审计失败不推翻已经下载好的文件，但必须显著提示（不要静默）
		fmt.Printf("  ⚠ 写审计失败（文件已产出）: %v\n", err)
	} else {
		fmt.Printf("  ✓ 审计已入库：%s\n", batchID)
	}

	printSummary(s, opts, time.Since(started))
	if s.Failed > 0 {
		return fmt.Errorf("%d 个文件下载失败（见 manifest.csv 与上面的明细）", s.Failed)
	}
	return nil
}

// ── 记录拉取 ───────────────────────────────────────────────

type listResult struct {
	Records  []feishu.BitableRecord
	Missing  []missingID
	Expanded []string
}

type missingID struct {
	Line   int
	Value  string
	Reason string
}

// fetchByList 按清单里的标识逐条取记录。
//
// 语义（计划 §3.3）：表里是「一张发票一行」，所以：
//   - 清单给 record_id → 就是那一行；
//   - 清单给审批实例号 → 展开为该实例的**所有行**（并明确提示），再按 record_id 去重。
func fetchByList(ctx context.Context, client *feishu.Client, cfg *config.Config,
	tableID string, ids []identifier, allowMissing bool) (*listResult, error) {

	res := &listResult{}
	seenRecord := map[string]bool{}
	seenInstance := map[string]string{} // 实例号 → 首次出处（用于提示重复行）

	for _, id := range ids {
		switch id.Kind {
		case kindRecord:
			rec, err := client.GetBitableRecord(ctx, cfg.Feishu.Bitable.AppToken, tableID, id.Value)
			if err != nil {
				if isNotFound(err) {
					res.Missing = append(res.Missing, missingID{id.Line, id.Value, "表里没有这个 record_id"})
					continue
				}
				return nil, fmt.Errorf("第 %d 行 %s: %w", id.Line, id.Value, err)
			}
			if !seenRecord[rec.RecordID] {
				seenRecord[rec.RecordID] = true
				res.Records = append(res.Records, *rec)
			}
		case kindInstance:
			if seenInstance[id.Value] != "" {
				continue // 同一实例号在清单里重复出现：跳过（重复行最终也会被 record_id 去重）
			}
			seenInstance[id.Value] = id.Value
			recs, err := client.SearchBitableRecordsAll(ctx, cfg.Feishu.Bitable.AppToken, tableID,
				feishu.SearchOptions{
					Filter: map[string]any{
						"conjunction": "and",
						"conditions": []map[string]any{
							{"field_name": "审批实例号", "operator": "is", "value": []string{id.Value}},
						},
					},
					FieldNames: neededFields(nil),
				})
			if err != nil {
				return nil, fmt.Errorf("第 %d 行 按实例号 %s 搜记录: %w", id.Line, id.Value, err)
			}
			if len(recs) == 0 {
				res.Missing = append(res.Missing, missingID{id.Line, id.Value, "表里没有这个审批实例号"})
				continue
			}
			if len(recs) > 1 {
				res.Expanded = append(res.Expanded, id.Value)
			}
			for _, rec := range recs {
				if !seenRecord[rec.RecordID] {
					seenRecord[rec.RecordID] = true
					res.Records = append(res.Records, rec)
				}
			}
		}
	}
	return res, nil
}

func neededFields(slots []string) []string {
	base := []string{"审批实例号", "发票号码", "购买人", "物资所属部门",
		"图读金额(元)", "图读日期", "销方名称"}
	if slots == nil {
		slots = bitable.Slots()
	}
	return append(base, slots...)
}

// toNamingRow 把一条多维表格记录转成命名输入。
func toNamingRow(r feishu.BitableRecord, slots []string) naming.Row {
	amount := bitable.NumberOf(r.Fields["图读金额(元)"])
	row := naming.Row{
		RecordID:   r.RecordID,
		InstanceNo: bitable.TextOf(r.Fields["审批实例号"]),
		Fields: naming.Fields{
			Department: bitable.FirstString(r.Fields["物资所属部门"]),
			Buyer:      bitable.TextOf(r.Fields["购买人"]),
			Date:       bitable.DateOf(r.Fields["图读日期"]),
			Seller:     bitable.TextOf(r.Fields["销方名称"]),
			Amount:     amount,
			InvoiceNo:  bitable.TextOf(r.Fields["发票号码"]),
		},
		Attachments: map[naming.Slot][]naming.Attachment{},
	}
	for _, slotName := range slots {
		slot, ok := slotOf(slotName)
		if !ok {
			continue
		}
		list, err := feishu.ParseAttachmentCell(r.Fields[slotName])
		if err != nil {
			continue // 解析失败在这里不阻断；下载阶段会体现为 0 文件
		}
		for i, a := range list {
			mime := mimeOf(a)
			row.Attachments[slot] = append(row.Attachments[slot], naming.Attachment{
				Index:        i + 1,
				Token:        a.FileToken,
				OriginalName: a.Name,
				Mime:         mime,
				Size:         a.Size,
			})
		}
	}
	return row
}

func slotOf(fieldName string) (naming.Slot, bool) {
	for slot, label := range naming.SlotField {
		if label == fieldName {
			return slot, true
		}
	}
	return "", false
}

// mimeOf 从附件的 type/name 推 MIME（命名要用它定扩展名）。
func mimeOf(a feishu.AttachmentValue) string {
	t := strings.ToLower(a.Type)
	if strings.Contains(t, "/") {
		return t
	}
	return ""
}

func countEmptySlots(records []feishu.BitableRecord, slots []string, s *summary) {
	for _, r := range records {
		for _, slotName := range slots {
			list, err := feishu.ParseAttachmentCell(r.Fields[slotName])
			if err != nil || len(list) == 0 {
				s.EmptyBySlot[slotName]++
			}
		}
	}
}

func estimateFiles(records []feishu.BitableRecord, slots []string) int {
	n := 0
	for _, r := range records {
		for _, slotName := range slots {
			list, err := feishu.ParseAttachmentCell(r.Fields[slotName])
			if err == nil {
				n += len(list)
			}
		}
	}
	return n
}

// ── 下载 ──────────────────────────────────────────────────

// manifestRow 是 manifest.csv 的一行。
type manifestRow struct {
	RecordID     string
	InstanceNo   string
	Slot         string
	OriginalName string
	OutPath      string
	Token        string
	SHA256       string
	SizeBytes    int64
	Mime         string
	Status       string
	Error        string
}

// download 按 5 个一批取链接、随即下载，保证「现取现下」。
func download(ctx context.Context, client *feishu.Client, opts Options,
	files []naming.PlannedFile, s *summary) ([]manifestRow, error) {

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录: %w", err)
	}

	var items []manifestRow
	for i := 0; i < len(files); i += feishu.MaxTmpDownloadTokens {
		end := i + feishu.MaxTmpDownloadTokens
		if end > len(files) {
			end = len(files)
		}
		batch := files[i:end]
		tokens := make([]string, 0, len(batch))
		for _, f := range batch {
			tokens = append(tokens, f.Token)
		}
		urls, err := client.BatchGetTmpDownloadURL(ctx, tokens, opts.Extra)
		if err != nil {
			// 整批取链接失败：把这几个文件全部记成失败，**不静默跳过**
			for _, f := range batch {
				s.Failed++
				items = append(items, manifestRow{
					RecordID: f.RecordID, InstanceNo: f.InstanceNo, Slot: string(f.Slot),
					OriginalName: f.OriginalName, OutPath: f.Path, Token: f.Token,
					Mime: f.Mime, Status: "failed", Error: "取临时链接失败: " + err.Error(),
				})
			}
			fmt.Printf("  ✗ 批次 %d 取链接失败: %v\n", i/feishu.MaxTmpDownloadTokens+1, err)
			continue
		}
		urlByToken := map[string]string{}
		for _, u := range urls {
			urlByToken[u.FileToken] = u.TmpDownloadURL
		}
		for _, f := range batch {
			row := downloadOne(ctx, client, opts, f, urlByToken[f.Token])
			switch row.Status {
			case "ok":
				s.Success++
			case "skipped":
				s.Skipped++
			default:
				s.Failed++
			}
			items = append(items, row)
			fmt.Printf("  %s %s\n", statusMark(row.Status), row.OutPath)
			if row.Error != "" {
				fmt.Printf("      ↳ %s\n", row.Error)
			}
		}
	}
	return items, nil
}

func downloadOne(ctx context.Context, client *feishu.Client, opts Options,
	f naming.PlannedFile, tmpURL string) manifestRow {

	row := manifestRow{
		RecordID: f.RecordID, InstanceNo: f.InstanceNo, Slot: string(f.Slot),
		OriginalName: f.OriginalName, OutPath: f.Path, Token: f.Token,
		Mime: f.Mime, Status: "ok",
	}
	if tmpURL == "" {
		row.Status, row.Error = "failed", "接口没有返回这个 token 的临时链接"
		return row
	}
	dest := filepath.Join(opts.OutDir, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		row.Status, row.Error = "failed", err.Error()
		return row
	}
	if _, err := os.Stat(dest); err == nil && !opts.Force {
		row.Status, row.Error = "skipped", "文件已存在（-force 可覆盖）"
		if sum, err := fileSHA256(dest); err == nil {
			row.SHA256 = sum
		}
		if st, err := os.Stat(dest); err == nil {
			row.SizeBytes = st.Size()
		}
		return row
	}

	// 先写临时文件：中途失败不会留下半截「看起来正常」的文件
	tmpFile := dest + ".part"
	fh, err := os.Create(tmpFile)
	if err != nil {
		row.Status, row.Error = "failed", err.Error()
		return row
	}
	hash := sha256.New()
	res, derr := client.DownloadTmpURL(ctx, tmpURL, io.MultiWriter(fh, hash))
	cerr := fh.Close()
	if derr != nil {
		os.Remove(tmpFile)
		row.Status, row.Error = "failed", derr.Error()
		return row
	}
	if cerr != nil {
		os.Remove(tmpFile)
		row.Status, row.Error = "failed", cerr.Error()
		return row
	}
	if err := os.Rename(tmpFile, dest); err != nil {
		os.Remove(tmpFile)
		row.Status, row.Error = "failed", err.Error()
		return row
	}
	row.SHA256 = hex.EncodeToString(hash.Sum(nil))
	row.SizeBytes = res.Bytes
	if f.DeclaredSize > 0 && res.Bytes != f.DeclaredSize {
		row.Status = "failed"
		row.Error = fmt.Sprintf("字节数不符：表里声明 %d，实际 %d", f.DeclaredSize, res.Bytes)
	}
	return row
}

func statusMark(status string) string {
	switch status {
	case "ok":
		return "✓"
	case "skipped":
		return "↷"
	default:
		return "✗"
	}
}

// ── 输出 ──────────────────────────────────────────────────

func printPlan(files []naming.PlannedFile, dryRun bool) {
	if !dryRun {
		return
	}
	fmt.Println("\n── 将要落盘的文件 ──")
	for _, f := range files {
		fmt.Printf("  %s  [%s]\n", f.Path, f.Token)
	}
}

func printWarnings(ws []naming.Warning) {
	// 逐条打印会刷屏：同类只打印前 5 条 + 总数
	byCode := map[string][]naming.Warning{}
	for _, w := range ws {
		byCode[w.Code] = append(byCode[w.Code], w)
	}
	var codes []string
	for c := range byCode {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		list := byCode[c]
		for i, w := range list {
			if i >= 5 {
				fmt.Printf("  ⚠ %s …（同类共 %d 条）\n", c, len(list))
				break
			}
			fmt.Printf("  ⚠ %s %s: %s\n", c, w.RecordID, w.Message)
		}
	}
}

func printSummary(s *summary, opts Options, d time.Duration) {
	fmt.Println("\n═══ 汇总 ═══")
	fmt.Printf("  行 %d → 计划 %d 个文件\n", s.Records, s.Planned)
	fmt.Printf("  成功 %d / 跳过 %d / 失败 %d / 空附件行 %d\n",
		s.Success, s.Skipped, s.Failed, s.EmptyRows)
	if len(s.MissingIDs) > 0 || len(s.NotFoundIDs) > 0 {
		fmt.Printf("  清单问题 %d 条\n", len(s.MissingIDs)+len(s.NotFoundIDs))
	}
	if s.ZipPath != "" {
		fmt.Printf("  zip %s\n", s.ZipPath)
	}
	fmt.Printf("  manifest %s\n", s.ManifestPath)
	fmt.Printf("  用时 %s\n", d.Round(time.Millisecond))
}

// writeManifest 写 UTF-8 + BOM 的 manifest.csv（Excel 打开中文不乱码）。
func writeManifest(path string, items []manifestRow, opts Options,
	tableID, tableName string, started time.Time) error {

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("\ufeff"); err != nil {
		return err
	}
	w := csv.NewWriter(f)
	header := []string{"记录ID", "审批实例号", "槽位", "原始文件名", "zip内路径",
		"file_token", "sha256", "字节数", "MIME", "状态", "失败原因"}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, it := range items {
		rec := []string{it.RecordID, it.InstanceNo, it.Slot, it.OriginalName,
			it.OutPath, it.Token, it.SHA256, fmt.Sprint(it.SizeBytes), it.Mime,
			it.Status, it.Error}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return os.WriteFile(strings.TrimSuffix(path, ".csv")+"-params.json", paramsJSON(opts, tableID, tableName, started), 0o644)
}

func paramsJSON(opts Options, tableID, tableName string, started time.Time) []byte {
	b, _ := json.MarshalIndent(map[string]any{
		"exported_at":  started.Format(time.RFC3339),
		"table":        tableName,
		"table_id":     tableID,
		"view_id":      opts.ViewID,
		"slots":        opts.Slots,
		"naming":       orDefault(opts.Naming, naming.DefaultTemplate),
		"group_by":     opts.GroupBy,
		"operator":     opts.Operator,
		"note":         "一行=一张发票；file_token 是长期标识，临时下载链接不在此记录（24h 失效）",
	}, "", "  ")
	return b
}

// zipDir 把 dir 下的内容打成 zip（条目路径相对 dir）。
func zipDir(dir, zipPath string) error {
	tmp := zipPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	var paths []string
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, src); err != nil {
			src.Close()
			return err
		}
		src.Close()
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, zipPath)
}

func copyToNAS(zipPath, nasRoot string) (string, error) {
	if st, err := os.Stat(nasRoot); err != nil || !st.IsDir() {
		return "", fmt.Errorf("NAS 路径不可用: %s", nasRoot)
	}
	dest := filepath.Join(nasRoot, filepath.Base(zipPath))
	src, err := os.Open(zipPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, src); err != nil {
		return "", err
	}
	return dest, nil
}

// writeAudit 把批次与逐文件结果写进本地库（审计底账）。
func writeAudit(cfg *config.Config, batchID, scope, tableName, tableID string,
	opts Options, listSHA string, listLines int, s *summary, items []manifestRow, started time.Time) error {

	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return err
	}
	defer db.Close()

	b := store.ExportBatch{
		BatchID: batchID, Scope: scope, TableName: tableName, TableID: tableID,
		ViewID: opts.ViewID, ListSHA256: listSHA, ListLines: listLines,
		Fields: strings.Join(opts.Slots, ","), Naming: orDefault(opts.Naming, naming.DefaultTemplate),
		GroupBy: opts.GroupBy, Operator: opts.Operator,
		StartedAt: started.Format(time.RFC3339), OutDir: opts.OutDir,
		Records: s.Records, Success: s.Success, Failed: s.Failed,
		Skipped: s.Skipped, EmptyRows: s.EmptyRows, ZipPath: s.ZipPath,
	}
	storeItems := make([]store.ExportItem, 0, len(items))
	for _, it := range items {
		storeItems = append(storeItems, store.ExportItem{
			RecordID: it.RecordID, InstanceNo: it.InstanceNo, Slot: it.Slot,
			FileToken: it.Token, OriginalName: it.OriginalName, OutPath: it.OutPath,
			SHA256: it.SHA256, SizeBytes: it.SizeBytes, Mime: it.Mime,
			Status: it.Status, Error: it.Error,
		})
	}
	return db.SaveExport(context.Background(), b, storeItems)
}

// ── 小工具 ────────────────────────────────────────────────

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "1254043") || strings.Contains(s, "NOTEXIST") ||
		strings.Contains(s, "not found") || strings.Contains(s, "RecordIdNotFound")
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			return nil, err
		}
		path = p
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if cfg.Feishu.Bitable.AppToken == "" {
		return nil, fmt.Errorf("config 里 feishu.bitable.app_token 为空")
	}
	return cfg, nil
}

func applyDefaults(o *Options, cfg *config.Config) {
	if len(o.Slots) == 0 {
		o.Slots = bitable.Slots()
	}
	if o.Naming == "" {
		o.Naming = cfg.Export.Naming
	}
	if o.GroupBy == "" {
		o.GroupBy = cfg.Export.GroupBy
	}
	if o.GroupBy == "" {
		o.GroupBy = "department"
	}
	if o.OutDir == "" && cfg.Export.Root != "" {
		o.OutDir = filepath.Join(cfg.Export.Root, time.Now().Format("2006-01"))
	}
	if o.NASRoot == "" {
		o.NASRoot = cfg.Export.NASRoot
	}
	if o.MaxFiles == 0 {
		o.MaxFiles = cfg.Export.MaxFilesPerBatch
	}
	if o.Operator == "" {
		o.Operator = "cli:" + currentUser()
	}
}

func resolveTable(cfg *config.Config, key string) (id, name string, err error) {
	if strings.HasPrefix(key, "tbl") {
		return key, key, nil
	}
	if id := cfg.Feishu.Bitable.Tables[key]; id != "" {
		return id, tableDisplayName(key), nil
	}
	switch key {
	case bitable.IntegratedTable().Name:
		if id := cfg.Feishu.Bitable.Tables["integrated"]; id != "" {
			return id, key, nil
		}
	case bitable.ReviewTable().Name:
		if id := cfg.Feishu.Bitable.Tables["submission"]; id != "" {
			return id, key, nil
		}
	}
	var keys []string
	for k := range cfg.Feishu.Bitable.Tables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "", "", fmt.Errorf("配置里没有表 %q（可用：%s；也可直接给 tblXXXX）",
		key, strings.Join(keys, "|"))
}

func tableDisplayName(key string) string {
	switch key {
	case "integrated":
		return bitable.IntegratedTable().Name
	case "submission":
		return bitable.ReviewTable().Name
	}
	return key
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ctxOf 是给内部调用用的背景上下文（命令整体由 main 的超时控制）。
func ctxOf() context.Context { return context.Background() }
