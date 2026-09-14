// Package pipeline 是"处理一个审批实例"的核心逻辑：
// 下载三张图 → PDF转PNG → 识别 → 本地入库。
//
// 它被两个入口复用：
//
//	cmd/extract  手动按实例号跑
//	cmd/serve    常驻服务，收到审批事件后自动跑
//
// 这是整条管线里第一个"产生真实数据"的命令。它做四件事：
//  1. 拉取实例详情（**必须现取**：附件 URL 只有 24 小时有效期）
//  2. 下载三个附件槽的每个文件，算 sha256，记录文件名与 Content-Type
//  3. PDF → PNG（Qwen3-VL 不支持 PDF 输入）；JPEG 原样留用
//  4. 若配了 ocr.api_key，则调 SiliconFlow 抽取字段；否则标记 SKIPPED
//
// 产物（data/extract/）：
//
//	manifest.jsonl     每个实例一行，含全部文件元数据与抽取结果
//	files/<实例号>/...  预处理后的图（PDF 转出的 PNG）
//
// 用法：
//
//	go run ./cmd/extract -limit 2                 # 用 recon 已抓到的前 2 个实例
//	go run ./cmd/extract -instance <instance_code> # 指定实例
package pipeline

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/exact"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/match"
	"github.com/coffee/finance-router/internal/ocr"
	"github.com/coffee/finance-router/internal/store"
)

// 从审批定义里按**显示名**识别三个槽位，比硬编码 widget id 更稳。
var slotRules = []struct {
	Slot    string
	Kind    string
	Keyword []string
}{
	{"发票文件", "invoice", []string{"发票"}},
	{"订单截图", "order", []string{"订单"}},
	{"付款截图", "payment", []string{"付款", "转账", "支付"}},
}

type FileMeta struct {
	Slot      string   `json:"slot"`
	Kind      string   `json:"kind"`
	Index     int      `json:"index"`
	Filename  string   `json:"filename"`
	MediaType string   `json:"media_type"`
	Bytes     int64    `json:"bytes"`
	SHA256    string   `json:"sha256"`
	Pages     int      `json:"pages"`
	PNGs      []string `json:"pngs,omitempty"`
	EstTokens int      `json:"est_tokens"`
	Status    string   `json:"status"` // OK | ERROR | SKIPPED
	Note      string   `json:"note,omitempty"`

	// ── 抽取结果与两道校验 ──
	OCR      *ocr.Result `json:"ocr,omitempty"`
	UpperChk string      `json:"upper_check,omitempty"` // 小写 vs 大写（★ 主校验）
	TaxChk   string      `json:"tax_check,omitempty"`   // 税额自洽（辅助）

	// DupOwner 非空表示这张图已入库（重复报销），值是首次提交它的实例号。
	// 此时 OCR 不会执行，OCR 字段为空。
	DupOwner string `json:"dup_owner,omitempty"`

	// ProvErr 记录 provenance 旁路表写入失败（不阻断抽取，但会让 reindex 退化）。
	ProvErr error `json:"-"`

	// Exact 是"精确通道"的结果（PDF 文字层 + 票面二维码）。
	// 非 nil 且 Sufficient() 时**不调模型** —— 这两条通道是确定性的，
	// 既能省下 ≈5340 tok/张，又消除了识别误差。
	// 两条通道都有时会交叉校验，冲突记在 ExactConflicts。
	Exact          *exact.Fields    `json:"exact,omitempty"`
	ExactConflicts []exact.Conflict `json:"exact_conflicts,omitempty"`
	ExactUsed      bool             `json:"exact_used,omitempty"` // 是否真的没调模型
}

// InstanceMeta 是审批实例的业务元信息（来自审批表单，非图片识别）。
// 这些是"人看得懂"的字段，最终写进多维表格给人工复核。
type InstanceMeta struct {
	ApprovalName    string   `json:"approval_name"`
	Status          string   `json:"status"`      // 申请状态
	StartTime       string   `json:"start_time"`  // 毫秒时间戳
	Applicant       string   `json:"applicant"`   // 发起人姓名（取自表单"购买人"）
	Departments     []string `json:"departments"` // 物资所属部门（多选）
	MaterialType    string   `json:"material_type"`
	MaterialName    string   `json:"material_name"`
	Buyer           string   `json:"buyer"`
	ApplicantDept   string   `json:"applicant_dept"`    // 发起人部门名称
	ApplicantDeptID string   `json:"applicant_dept_id"` // 发起人部门 open_department_id（od-…）
	FundSource      string   `json:"fund_source"`
	Dachuang        string   `json:"dachuang"`  // 是否走大创资金报销
	IsAlipay        string   `json:"is_alipay"` // 是否为支付宝付款（新表单）
	Remark          string   `json:"remark"`
	// Applink 指向审批原单；instanceId 可直接用 instance_code（官方文档确认），
	// 因此这条链接长期有效，适合放进多维表格给人点。
	Applink string `json:"applink"`
}

type Manifest struct {
	InstanceCode string        `json:"instance_code"`
	Status       string        `json:"status"`
	Files        []FileMeta    `json:"files"`
	Meta         *InstanceMeta `json:"meta,omitempty"`
	At           string        `json:"at"`
	Match        *match.Report `json:"match,omitempty"` // 三单互核结果
}

// Options 是一次处理运行的参数。
type Options struct {
	CfgPath   string
	FormsPath string // recon 产出的 forms.jsonl（仅在未指定 Instance 时用）
	Instance  string // 只处理这一个实例
	Limit     int
	OutDir    string
	DPI       int // 0 = 用 config.pdf.dpi
	KeepPDF   bool
	Quiet     bool // 常驻服务用：不打印进度
}

// Run 按 Options 执行一次抽取（导出给 cmd/extract 与 cmd/serve 用）。
func Run(opts Options) error {
	if opts.FormsPath == "" {
		opts.FormsPath = "data/recon/forms.jsonl"
	}
	if opts.OutDir == "" {
		opts.OutDir = "data/extract"
	}
	return run(opts)
}

// logf 按 Quiet 决定是否输出。
func (o Options) logf(format string, a ...any) {
	if !o.Quiet {
		fmt.Printf(format, a...)
	}
}

func run(opts Options) error {
	cfgPath, formsPath, one := opts.CfgPath, opts.FormsPath, opts.Instance
	limit, outDir, dpi, keepPDF := opts.Limit, opts.OutDir, opts.DPI, opts.KeepPDF
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
	if cfg.Feishu.AppID == "" || cfg.Feishu.AppSecret == "" {
		return fmt.Errorf("配置缺少 feishu.app_id / app_secret")
	}
	// dpi=0 表示用配置值（★ 必须走配置：实测 150dpi 会漏读小字）
	if dpi == 0 {
		dpi = cfg.PDF.DPI
	}
	if dpi == 0 {
		dpi = 300
	}
	fmt.Printf("PDF 光栅化 DPI: %d（config.pdf.dpi=%d）\n", dpi, cfg.PDF.DPI)

	codes, err := pickInstances(formsPath, one, limit)
	if err != nil {
		return err
	}
	fmt.Printf("待处理实例 %d 个\n", len(codes))

	hasKey := cfg.OCR.APIKey != ""
	var provider ocr.Provider
	if hasKey {
		sf := ocr.NewSiliconFlow(cfg.OCR.BaseURL, cfg.OCR.APIKey, cfg.OCR.Model)
		sf.Temperature = cfg.OCR.Temperature
		sf.MaxTokens = cfg.OCR.MaxTokens
		sf.ResponseFormat = cfg.OCR.ResponseFormat
		sf.EnableThinking = cfg.OCR.EnableThinking
		provider = sf
		fmt.Printf("抽取: %s @ %s\n", cfg.OCR.Model, cfg.OCR.BaseURL)
	} else {
		fmt.Println("⚠ ocr.api_key 为空 → 只做下载+预处理，抽取标记为 SKIPPED")
		fmt.Println("  取值：https://cloud.siliconflow.cn/ → API 密钥 → 新建，填进 config.yml 的 ocr.api_key")
	}
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	// 本地库：承担"唯一性 / 审计 / 可回放"三件飞书做不到的事。
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()
	fmt.Printf("本地库: %s\n", cfg.Paths.DB)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	outBase = outDir // rel/readLocal 依赖它解析相对路径（-out 改到别处时必须同步）
	mfFile, err := os.Create(filepath.Join(outDir, "manifest.jsonl"))
	if err != nil {
		return err
	}
	defer mfFile.Close()

	// deptCache: 部门 open_id（od-…）→ 名称。
	// 先装载库里累积的（跨次运行），再用本次表单里的补齐。
	// 来源是各单据「物资所属部门」控件值里的 {"name":…,"open_id":"od-…"}，
	// 用于通讯录接口缺 name 字段权限时精确兜底（按 ID 匹配，不靠猜）。
	deptCache := map[string]string{}
	loadDeptCache(ctx, db, deptCache)

	var totalFiles, okFiles, errFiles, saved, duplicates, dupFiles, skippedOther int
	for i, code := range codes {
		fmt.Printf("[%d/%d] %s\n", i+1, len(codes), code)

		// 本单是否出现"已入库的图"。一旦为 true，整单拦截、不跑 OCR。
		// dupFiles 是**跨单累计**的（用于结尾汇总），所以这里不能重置。
		dupSeen := false

		// 现取详情：附件 URL 24 小时过期
		detail, _, err := client.GetInstanceDetail(ctx, code)
		if err != nil {
			fmt.Printf("      ✗ 取详情失败: %v\n", err)
			continue
		}

		// ★ 只处理本项目绑定的那张审批定义。
		//
		// 租户里有多个审批表单（实测 4 个，名称见 config 的 approval_name_expect 与
		// docs/30-review/25）。forms.jsonl 可能来自旧数据、或有人手工传了
		// -instance，若不校验就会把别的表单当成报销单抽取、入库、写进多维表格。
		// 这里以**实例详情的 approval_code 为准**做硬校验（比信任入参可靠）。
		if want := cfg.Feishu.ApprovalCode; want != "" &&
			detail.ApprovalCode != "" && detail.ApprovalCode != want {
			fmt.Printf("      ⛔ 跳过：该实例属于「%s」，不是本项目绑定的审批定义\n",
				detail.ApprovalName)
			skippedOther++
			continue
		}
		widgets, err := feishu.ParseForm(detail.Form)
		if err != nil {
			fmt.Printf("      ✗ 解析表单失败: %v\n", err)
			continue
		}
		m := Manifest{
			InstanceCode: code, Status: "OK",
			At:   time.Now().Format(time.RFC3339),
			Meta: buildMeta(cfg, code, detail, widgets),
		}
		// 每次解析表单都往对照表里累积「部门 open_id → 名称」，
		// 供通讯录拿不到 name 时精确兜底。同时落库，跨次运行也能用。
		for id, name := range learnDepartments(widgets, deptCache) {
			_ = db.LearnDept(ctx, id, name)
		}

		// 发起人部门：先查通讯录；name 缺失时用对照表按 open_department_id 精确匹配。
		//
		// ⚠️ 不要用"本单表单里的物资所属部门"当申请人的部门 —— 那是**物资的**部门，
		//    两者可以不同（如机械组的人替电控组买东西）。用错会静默填错数据。
		if detail.DepartmentID != "" {
			info, derr := client.GetDepartment(ctx, detail.DepartmentID)
			switch {
			case derr == nil && info.Name != "":
				m.Meta.ApplicantDept = info.Name
				m.Meta.ApplicantDeptID = info.OpenDeptID
			case derr == nil && info.OpenDeptID != "":
				m.Meta.ApplicantDeptID = info.OpenDeptID
				if name, ok := deptCache[info.OpenDeptID]; ok {
					m.Meta.ApplicantDept = name
					fmt.Printf("      · 部门名取自对照表: %s\n", name)
				} else {
					fmt.Printf("      ⚠ 部门 %s（%s）名称未知：缺字段权限 "+
						"contact:department.base:readonly，且对照表里没有\n",
						detail.DepartmentID, info.OpenDeptID)
				}
			default:
				fmt.Printf("      ⚠ 发起人部门查询失败（%s）: %v\n", detail.DepartmentID, derr)
			}
		}
		dir := filepath.Join(outDir, "files", code)

		// ★ 资金来源 = 老师垫付 时，**忽略付款记录**。
		//
		// 钱不是从报销人账户走的，付款截图既无意义也不一定拿得到。
		// 注意表单里的选项文本是「老师垫付」（不是"代付"），用包含匹配避免
		// 以后文案微调（如加标点）就静默失效。
		skipPayment := strings.Contains(m.Meta.FundSource, "老师垫付")

		for _, rule := range slotRules {
			if rule.Kind == "payment" && skipPayment {
				fmt.Printf("      · 资金来源=%s，跳过 %s\n", m.Meta.FundSource, rule.Slot)
				continue
			}
			w, ok := findAttachmentWidget(widgets, rule.Keyword)
			if !ok {
				continue
			}
			urls := w.AttachmentURLs()
			if len(urls) == 0 {
				continue
			}
			for idx, u := range urls {
				totalFiles++
				fm := downloadOne(ctx, u, rule.Slot, rule.Kind, idx+1, dir, dpi, keepPDF)
				fm.EstTokens = actualTokens(fm, cfg)

				// ★★ 查重必须在 OCR **之前**。
				//    sha256 是唯一性的权威判定；一张已经入库的图不可能带来新信息，
				//    对它跑 OCR 是纯浪费（发票单张 ≈5000 tok）。下载阶段已经算好 sha，
				//    这里直接问库，命中就整单拦下。
				if fm.Status == "OK" && fm.SHA256 != "" {
					owner, found, derr := db.DuplicateOf(ctx, fm.SHA256)
					// ★ 命中自己的图不算重复：那是本实例重跑（或本实例内多张同图），
					//   唯一索引防的是**别的实例**复用同一张凭证。
					if derr == nil && found && owner != code {
						fm.Status = "DUPLICATE"
						fm.DupOwner = owner
						dupSeen = true
						dupFiles++
						fmt.Printf("      ⛔ %-6s#%d 重复图（首次来自 %s）跳过 OCR，省 ≈%d tok\n",
							rule.Slot, idx+1, shortID(owner), fm.EstTokens)
					}
				}

				if provider != nil && fm.Status == "OK" && len(fm.PNGs) > 0 {
					extractInto(ctx, provider, &fm, cfg)
				} else if provider == nil {
					fm.Status = "SKIPPED"
				}
				if fm.Status == "ERROR" {
					errFiles++
					fmt.Printf("      ✗ %s#%d %v\n", rule.Slot, idx+1, fm.Note)
				} else if fm.Status == "DUPLICATE" {
					// 已在上面打印过，不重复刷屏
				} else {
					okFiles++
					extra := ""
					if fm.OCR != nil {
						extra = fmt.Sprintf(" | %s 上校验=%s 税校验=%s",
							amountBrief(fm.OCR), fm.UpperChk, fm.TaxChk)
					}
					fmt.Printf("      ✓ %-6s#%d %-28s %-16s %6.1fKB sha=%s… tok≈%d%s\n",
						rule.Slot, idx+1, trunc(fm.Filename, 28), fm.MediaType,
						float64(fm.Bytes)/1024, fm.SHA256[:12], fm.EstTokens, extra)
				}
				m.Files = append(m.Files, fm)
			}
		}
		// ★ 整单查重拦截：任意一张图已入库 → 本单即重复报销，立刻停止。
		//   不跑 OCR（上面已跳过）、不做匹配、不入库。manifest 照写以便追溯。
		if dupSeen {
			m.Status = "DUPLICATE"
			n := 0
			for _, f := range m.Files {
				if f.Status == "DUPLICATE" {
					n++
				}
			}
			fmt.Printf("      ⛔ 重复报销拦截：本单含 %d 张已入库的图，整单跳过\n", n)
			duplicates++
			b, _ := json.Marshal(m)
			mfFile.Write(b)
			mfFile.Write([]byte("\n"))
			fmt.Println()
			continue
		}

		if len(m.Files) == 0 {
			m.Status = "NO_ATTACHMENT"
			fmt.Println("      ⚠ 没找到任何附件（槽位名对不上？看 recon 报告 §3）")
		} else {
			resolveDates(m.Files) // ★ 先用有年份的单据补全缺年份的（如电商订单页）
			m.Match = runMatch(m.Files, cfg.Matching.AmountToleranceCent)
			printMatch(m.Match)
		}

		// ★ 入库：命中 sha256 唯一索引 → 判定为"重复报销"并拦截
		if err := saveToDB(ctx, db, &m); err != nil {
			var dup *store.DupError
			if errors.As(err, &dup) {
				fmt.Printf("      ⛔ 重复报销拦截：%v\n", dup)
				duplicates++
			} else {
				fmt.Printf("      ⚠ 入库失败: %v\n", err)
			}
		} else {
			saved++
		}
		b, _ := json.Marshal(m)
		mfFile.Write(b)
		mfFile.Write([]byte("\n"))
		fmt.Println()
	}

	fmt.Printf("完成：文件 %d 个（成功 %d，失败 %d，重复跳过 %d）\n", totalFiles, okFiles, errFiles, dupFiles)
	fmt.Printf("      入库 %d 单，重复拦截 %d 单\n", saved, duplicates)
	if skippedOther > 0 {
		fmt.Printf("      ⛔ 跳过 %d 单（不属于本项目绑定的审批定义）\n", skippedOther)
	}
	fmt.Printf("产物: %s/manifest.jsonl 与 %s/files/\n", outDir, outDir)
	return nil
}

// pickInstances 取要处理的实例号：优先 -instance，其次 forms.jsonl。
func pickInstances(formsPath, one string, limit int) ([]string, error) {
	if one != "" {
		return []string{one}, nil
	}
	f, err := os.Open(formsPath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败（先跑 recon）: %w", formsPath, err)
	}
	defer f.Close()
	var codes []string
	dec := json.NewDecoder(f)
	for dec.More() {
		var row struct {
			InstanceCode string `json:"instance_code"`
		}
		if err := dec.Decode(&row); err != nil {
			break
		}
		if row.InstanceCode != "" {
			codes = append(codes, row.InstanceCode)
		}
		if limit > 0 && len(codes) >= limit {
			break
		}
	}
	if len(codes) == 0 {
		return nil, fmt.Errorf("%s 里没有实例", formsPath)
	}
	return codes, nil
}

// findAttachmentWidget 找附件控件。
//
// ⚠️ 必须**同时按控件类型过滤**，不能只按名字匹配 —— 这是个真实踩过的坑：
// 新表单的控件「是否为支付宝付款（上传多张发票：需满足一张发票对应一张订单，
// 支付宝支付  |||  上传多张订单、付款记录：需满足支付宝支付）」名字里把
// "发票"、"订单"、"付款" **全都包含**了，而且它排在真正的附件控件**前面**。
// 只按名字匹配会让三个槽位全部命中这个 radio 控件，结果一张附件都取不到，
// 而报错信息只说"槽位名对不上"，把人往完全错误的方向引。
func findAttachmentWidget(ws []feishu.FormWidget, keywords []string) (feishu.FormWidget, bool) {
	for _, w := range ws {
		switch w.Type {
		case "attachment", "attachmentV2", "image", "imageV2":
		default:
			continue
		}
		for _, k := range keywords {
			if strings.Contains(w.Name, k) {
				return w, true
			}
		}
	}
	return feishu.FormWidget{}, false
}

func findWidgetByName(ws []feishu.FormWidget, keywords []string) (feishu.FormWidget, bool) {
	for _, w := range ws {
		for _, k := range keywords {
			if strings.Contains(w.Name, k) {
				return w, true
			}
		}
	}
	return feishu.FormWidget{}, false
}

var mimeExt = map[string]string{
	"application/pdf": ".pdf",
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
}

func downloadOne(ctx context.Context, url, slot, kind string, idx int, dir string, dpi int, keepPDF bool) FileMeta {
	fm := FileMeta{Slot: slot, Kind: kind, Index: idx, Status: "OK"}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fm.Status, fm.Note = "ERROR", err.Error()
		return fm
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		fm.Status, fm.Note = "ERROR", err.Error()
		return fm
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fm.Status, fm.Note = "ERROR", fmt.Sprintf("HTTP %d（URL 可能已过期，重新取详情）", resp.StatusCode)
		return fm
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fm.Status, fm.Note = "ERROR", err.Error()
		return fm
	}

	sum := sha256.Sum256(body)
	fm.SHA256 = hex.EncodeToString(sum[:])
	fm.Bytes = int64(len(body))
	fm.MediaType = strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]
	fm.Filename = parseFilename(resp.Header.Get("Content-Disposition"))
	if fm.Filename == "" {
		fm.Filename = fmt.Sprintf("%s-%d%s", kind, idx, mimeExt[fm.MediaType])
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		fm.Status, fm.Note = "ERROR", err.Error()
		return fm
	}
	base := filepath.Join(dir, fmt.Sprintf("%s-%d", kind, idx))

	if fm.MediaType == "application/pdf" {
		pdfPath := base + ".pdf"
		if err := os.WriteFile(pdfPath, body, 0o644); err != nil {
			fm.Status, fm.Note = "ERROR", err.Error()
			return fm
		}
		pngs, err := rasterize(ctx, pdfPath, base, dpi)
		if err != nil {
			fm.Status, fm.Note = "ERROR", "PDF 转 PNG 失败: "+err.Error()
			return fm
		}
		for _, p := range pngs {
			fm.PNGs = append(fm.PNGs, rel(p))
		}
		fm.Pages = len(pngs)
		for _, p := range pngs {
			fm.EstTokens += estTokensFromPNG(p)
		}
		// ★ 精确通道①：PDF 文字层。
		//   必须在删掉 PDF **之前**读 —— 默认 keepPDF=false，删了就没了。
		if f, ferr := exact.FromPDF(pdfPath); ferr == nil && f != nil && f.InvoiceNo != "" {
			fm.Exact = f
		}
		if !keepPDF {
			os.Remove(pdfPath)
		}
	} else {
		ext := mimeExt[fm.MediaType]
		if ext == "" {
			ext = ".bin"
		}
		p := base + ext
		if err := os.WriteFile(p, body, 0o644); err != nil {
			fm.Status, fm.Note = "ERROR", err.Error()
			return fm
		}
		fm.PNGs = []string{rel(p)}
		fm.Pages = 1
		fm.EstTokens = estTokensFromPNG(p)
	}

	// ★ 记 provenance：把"落盘的每个文件 → 原始下载字节的 sha256"写进旁路表。
	//
	// 为什么必须有：evidence.sha256 用的是**原始字节**的哈希（PDF 就是 PDF 的哈希），
	// 而磁盘上留的是光栅化后的 PNG（或原样 JPG）。默认不保留 PDF，原始字节随即消失。
	// 于是"从磁盘重建唯一性"（reindex）根本算不出真实的 sha —— 对 PDF 类附件永远是错的，
	// 重建出来的保护形同虚设：同一份 PDF 再提交一次，sha 对不上，重复就漏过去了。
	fm.ProvErr = recordProvenance(dir, fm.PNGs, fm.SHA256, fm.MediaType, fm.Bytes)
	return fm
}

// recordProvenance 把 raw sha 追加写入 dir/_provenance.tsv（append-only，不覆盖历史）。
// 行格式：<落盘文件名>\t<原始sha256>\t<media_type>\t<原始字节数>
// 返回错误但不阻断主流程 —— 丢了 provenance 只影响 reindex，不影响本次抽取。
func recordProvenance(dir string, written []string, rawSHA, mediaType string, n int64) error {
	if dir == "" || rawSHA == "" || len(written) == 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, provenanceFile),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, w := range written {
		if _, err := fmt.Fprintf(f, "%s\t%s\t%s\t%d\n",
			filepath.Base(w), rawSHA, mediaType, n); err != nil {
			return err
		}
	}
	return nil
}

// provenanceFile 是每个实例目录下的旁路表：落盘文件 → 原始下载字节的 sha256。
const provenanceFile = "_provenance.tsv"

// rasterize 用 pdftoppm 把 PDF 转成 PNG，返回生成的文件路径。
func rasterize(ctx context.Context, pdfPath, base string, dpi int) ([]string, error) {
	bin, err := exec.LookPath("pdftoppm")
	if err != nil {
		return nil, fmt.Errorf("未找到 pdftoppm（apt install poppler-utils）")
	}
	cmd := exec.CommandContext(ctx, bin, "-png", "-r", fmt.Sprint(dpi), pdfPath, base)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pdftoppm: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	matches, _ := filepath.Glob(base + "-*.png")
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, fmt.Errorf("未生成 PNG（PDF 可能加密或为空）")
	}
	return matches, nil
}

// parseFilename 从 Content-Disposition 里取文件名（优先 filename* 的 UTF-8 形式）。
func parseFilename(cd string) string {
	if cd == "" {
		return ""
	}
	re := regexp.MustCompile(`filename\*=UTF-8''([^;]+)`)
	if m := re.FindStringSubmatch(cd); len(m) == 2 {
		if s, err := urlQueryUnescape(m[1]); err == nil {
			return filepath.Base(s)
		}
	}
	re2 := regexp.MustCompile(`filename="([^"]+)"`)
	if m := re2.FindStringSubmatch(cd); len(m) == 2 {
		return filepath.Base(m[1])
	}
	return ""
}

func urlQueryUnescape(s string) (string, error) {
	// 只解 %XX，不把 + 当空格（文件名里可能有 +）
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var b byte
			if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &b); err == nil {
				out = append(out, b)
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out), nil
}

// estTokensFromPNG 估算视觉 token（Qwen: ceil(h/28)*ceil(w/28)）。
// 支持 PNG 与 JPEG；读不到尺寸返回 0。
func estTokensFromPNG(path string) int {
	w, h := imageSize(path)
	if w == 0 || h == 0 {
		return 0
	}
	return ((w + 27) / 28) * ((h + 27) / 28)
}

// imageSize 只读文件头，取 PNG 或 JPEG 的像素尺寸（无第三方依赖）。
func imageSize(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return 0, 0
	}
	// PNG: 89 50 4E 47 ... IHDR 在偏移 16
	if head[0] == 0x89 && head[1] == 'P' && head[2] == 'N' && head[3] == 'G' {
		hdr := make([]byte, 24)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, 0
		}
		if _, err := io.ReadFull(f, hdr); err != nil {
			return 0, 0
		}
		if string(hdr[12:16]) != "IHDR" {
			return 0, 0
		}
		be := func(b []byte) int { return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3]) }
		return be(hdr[16:20]), be(hdr[20:24])
	}
	// JPEG: FF D8，然后逐段扫描找 SOF0..SOF15（排除 C4/C8/CC）
	if head[0] == 0xFF && head[1] == 0xD8 {
		if _, err := f.Seek(2, io.SeekStart); err != nil {
			return 0, 0
		}
		buf := make([]byte, 2)
		for {
			// 找段起始标记 0xFF
			b, err := readByte(f)
			if err != nil {
				return 0, 0
			}
			if b != 0xFF {
				continue
			}
			for {
				b, err = readByte(f)
				if err != nil {
					return 0, 0
				}
				if b != 0xFF {
					break
				}
			}
			marker := b
			if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
				continue // 无长度字段
			}
			if _, err := io.ReadFull(f, buf); err != nil {
				return 0, 0
			}
			segLen := int(buf[0])<<8 | int(buf[1])
			isSOF := marker >= 0xC0 && marker <= 0xCF &&
				marker != 0xC4 && marker != 0xC8 && marker != 0xCC
			if isSOF {
				sof := make([]byte, 5)
				if _, err := io.ReadFull(f, sof); err != nil {
					return 0, 0
				}
				h := int(sof[1])<<8 | int(sof[2])
				w := int(sof[3])<<8 | int(sof[4])
				return w, h
			}
			if segLen < 2 {
				return 0, 0
			}
			if _, err := f.Seek(int64(segLen-2), io.SeekCurrent); err != nil {
				return 0, 0
			}
		}
	}
	return 0, 0
}

func readByte(f *os.File) (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// outBase 是本次运行的输出目录，供 rel/readLocal 解析相对路径。
// （原先写死 "data/extract"，导致 -out 指向别处时读不到预处理图。）
var outBase = "data/extract"

func rel(p string) string {
	if r, err := filepath.Rel(outBase, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// actualTokens 按实际发送的 detail 档位估算视觉 token。
//
// detail=low 时提供方会把图统一压到 448x448 → 恒 256 token（已核实官方口径）；
// detail=high 时按 ceil(h/28)*ceil(w/28)。下载阶段算出的 EstTokens 是 high 档的值，
// 直接显示会高估成本（实测截图 high 3393 vs low 256，差 13 倍）。
func actualTokens(fm FileMeta, cfg *config.Config) int {
	detail := cfg.DetailByKind[fm.Kind]
	if detail == "low" {
		return 256
	}
	return fm.EstTokens
}

// extractInto 对已预处理的图片调用抽取，并跑两个校验。
func extractInto(ctx context.Context, p ocr.Provider, fm *FileMeta, cfg *config.Config) {
	path := fm.PNGs[0]
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(outBase, path)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		fm.Status, fm.Note = "ERROR", "读预处理图失败: "+err.Error()
		return
	}
	mt := "image/png"
	if strings.HasSuffix(strings.ToLower(abs), ".jpg") || strings.HasSuffix(strings.ToLower(abs), ".jpeg") {
		mt = "image/jpeg"
	}
	// ★ 精确通道②：二维码（从**已渲染的整页图**上解，不需要裁切定位）。
	//   实测 2481×1654 的 300dpi 整页 PNG，gozxing 0.19 秒解出。
	//
	//   只对**发票**做：订单/付款截图里也可能出现二维码（支付页、小程序码…），
	//   虽然 ParseQR 要求首段为 "01" 已经能挡掉大部分，但没必要冒这个险，
	//   而且对它们做解码纯属浪费。
	if fm.Kind == "invoice" {
		if qf, qerr := exact.FromImageFile(abs); qerr == nil && qf != nil {
			merged, conflicts := exact.Merge(fm.Exact, qf)
			fm.Exact, fm.ExactConflicts = merged, conflicts
			for _, c := range conflicts {
				fmt.Printf("      ⚠ 精确通道冲突（%s）—— 两条通道读数不一致，交人工\n", c)
			}
		}
	}

	// ★ 精确通道够用 → **不调模型**。发票至少要拿到 发票号码 + 价税合计。
	if fm.Exact != nil && fm.Exact.Sufficient() {
		res := exactToResult(fm.Exact, fm.Kind)
		fm.OCR = res
		fm.ExactUsed = true
		tol := cfg.Matching.AmountToleranceCent
		fm.UpperChk = string(res.CheckUpper(tol))
		fm.TaxChk = string(res.CheckTax(tol))
		fmt.Printf("      ◎ 精确通道命中（%s）—— 未调用模型，省 ≈%d tok\n",
			fm.Exact.Source, fm.EstTokens)
		return
	}

	// detail 按图类型分层：发票 high，订单/付款 low（省钱且够用）
	detail := cfg.DetailByKind[fm.Kind]
	if detail == "" {
		detail = "high"
	}
	res, err := p.Extract(ctx, ocr.ImageRef{Bytes: b, MediaType: mt, Detail: detail})
	if err != nil {
		fm.Status, fm.Note = "ERROR", "抽取失败: "+err.Error()
		return
	}
	fm.OCR = res
	if res.Error != "" {
		fm.Status, fm.Note = "ERROR", res.Error
		return
	}
	tol := cfg.Matching.AmountToleranceCent
	fm.UpperChk = string(res.CheckUpper(tol))
	fm.TaxChk = string(res.CheckTax(tol))
}

// exactToResult 把精确通道的结果转成与模型输出同构的 ocr.Result，
// 这样下游的校验（大写/税额）、匹配、入库全都不用改。
func exactToResult(f *exact.Fields, kind string) *ocr.Result {
	return &ocr.Result{
		Kind:               ocr.Kind(kind),
		Confidence:         1.0, // 确定性通道，不是"置信度"意义上的估计
		AmountInclTaxCent:  f.AmountCent,
		TaxCent:            f.TaxCent,
		AmountExclTaxCent:  f.ExclCent,
		AmountInclTaxUpper: f.AmountUpper,
		Date:               f.Date,
		Counterparty:       f.SellerName,
		InvoiceCode:        f.InvoiceCode,
		InvoiceNo:          f.InvoiceNo,
		SellerTaxID:        f.SellerTaxID,
		// 票面备注里的订单号 —— 这是"发票↔订单"最强的确定性键
		OrderNo:  strings.Join(f.OrderNos, ","),
		Provider: "exact:" + f.Source,
	}
}

// amountBrief 把金额三件套压成一行显示。
func amountBrief(r *ocr.Result) string {
	f := func(p *int64) string {
		if p == nil {
			return "—"
		}
		return fmt.Sprintf("%d", *p)
	}
	s := fmt.Sprintf("价税合计%s/税%s/不含%s", f(r.AmountInclTaxCent), f(r.TaxCent), f(r.AmountExclTaxCent))
	if r.InvoiceNo != "" {
		s += " 号" + trunc(r.InvoiceNo, 20)
	}
	return s
}

// resolveDates 用同一笔业务里【有年份】的日期，去补全【只有月日】的日期。
//
// 动机（实测）：电商订单页常写"6月26日"不写年份（淘宝的"凭据：6月26日下单"）。
// 若模型被迫输出 YYYY-MM-DD，它会**猜**一个年份（实测猜成 2024/2023），
// 于是三单日期跨度变成 700~1100 天的荒谬值。
//
// 策略：优先用发票日期当参考（发票日期最可靠），其次付款截图。
// 补全后在「处理说明」留痕，便于审计"这个年份是推断的，不是读出来的"。
func resolveDates(files []FileMeta) {
	ref := ""
	// 先找参考日期：发票优先
	for _, kind := range []string{"invoice", "payment", "order"} {
		for _, f := range files {
			if f.Kind != kind || f.OCR == nil {
				continue
			}
			if match.HasYear(f.OCR.Date) {
				ref = f.OCR.Date
				break
			}
		}
		if ref != "" {
			break
		}
	}
	if ref == "" {
		return
	}
	for i := range files {
		f := &files[i]
		if f.OCR == nil || f.OCR.Date == "" || match.HasYear(f.OCR.Date) {
			continue
		}
		if fixed, ok := match.InferYear(f.OCR.Date, ref); ok {
			raw := f.OCR.Date
			f.OCR.Date = fixed
			mark := fmt.Sprintf("年份由发票日期(%s)推断（票面只有 %s）", ref, raw)
			if f.Note != "" {
				f.Note += "；" + mark
			} else {
				f.Note = mark
			}
		}
	}
}

// runMatch 把抽取结果组装成互核输入并执行三单互核。
func runMatch(files []FileMeta, tol int64) *match.Report {
	var items []match.Item
	for _, f := range files {
		if f.OCR == nil {
			continue
		}
		kind := f.OCR.Kind
		if kind == ocr.KindUnknown || kind == "" {
			kind = ocr.Kind(f.Kind) // 回退到槽位推断
		}
		items = append(items, match.Item{
			Slot:   f.Slot,
			Kind:   kind,
			Amount: match.NormalizeAmount(f.OCR.AmountInclTaxCent, kind),
			Date:   f.OCR.Date,
			Party:  f.OCR.Counterparty,
		})
	}
	return match.Compare(items, tol)
}

// printMatch 打印互核结论。
func printMatch(r *match.Report) {
	icon := map[string]string{"OK": "✓", "WARN": "⚠", "FAIL": "✗"}[r.Verdict]
	_ = icon
	verdict := map[string]string{
		"MATCHED": "三单一致", "SUSPECT": "存疑，需人工看", "DEFECTIVE": "缺件，无法核对",
	}[r.Verdict]
	fmt.Printf("      ── 互核：%s\n", verdict)
	if r.Dates != nil && r.Dates.Checked {
		fmt.Printf("         日期跨度: %s ~ %s (%d 天)\n", r.Dates.Earliest, r.Dates.Latest, r.Dates.Days)
	}
	for _, f := range r.Findings {
		mark := map[string]string{"OK": "✓", "WARN": "⚠", "FAIL": "✗"}[f.Level]
		fmt.Printf("         %s [%s] %s\n", mark, f.Code, f.Detail)
	}
}

// approvalAppID 是**飞书审批小程序的固定平台常量**，用于拼接 applink：
//
//	https://applink.feishu.cn/client/approval/instance?appId=<approvalAppID>&instanceId=<code>
//
// 官方文档：飞书品牌下审批的应用 ID 为 cli_9cb844403dbb9108，
// Lark 品牌下为 cli_9c7cc8a9a9edd105。**它对所有租户都一样**，
// 是平台公开常量，不是本项目的 app_id（那个在 config.yml 里、repo 公开故不入库）。
// 可用 config 的 feishu.approval_app_id 覆盖（Lark 品牌需要）。
var approvalAppID = "cli_9cb844403dbb9108" // 飞书品牌；Lark 品牌见 config 的 approval_app_id

// resolveApprovalAppID 允许 config 覆盖上面的平台默认值。
func resolveApprovalAppID(cfg *config.Config) string {
	if cfg != nil && cfg.Feishu.ApprovalAppID != "" {
		return cfg.Feishu.ApprovalAppID
	}
	return approvalAppID
}

// buildMeta 从审批实例详情里抽出"人看得懂"的业务字段。
//
// applink 用官方文档给出的拼接方式；`instanceId` 可直接用 instance_code，
// 因此这条链接**长期有效**，适合放进多维表格给人点。
func buildMeta(cfg *config.Config, code string, detail *feishu.InstanceDetail, widgets []feishu.FormWidget) *InstanceMeta {
	m := &InstanceMeta{
		ApprovalName:  detail.ApprovalName,
		Status:        detail.Status,
		StartTime:     detail.StartTime,
		ApplicantDept: detail.DepartmentID, // 先存 ID，稍后由调用方解析成名称
		Applink: fmt.Sprintf(
			"https://applink.feishu.cn/client/mini_program/open?mode=appCenter&appId=%s"+
				"&width=1136&height=750&path=pc%%2Fpages%%2Fin-process%%2Findex%%3FinstanceId%%3D%s",
			resolveApprovalAppID(cfg), code),
	}
	// ★ 用"名称包含"匹配，不要用精确相等。
	//
	// 两张表单的控件名不一样，而且新表单的名字里带着长长的括号说明，例如
	//   "归属组（技术组只能由技术组长选取，正式队员选择兵种组）"
	//   "是否为支付宝付款（上传多张发票：需满足一张发票对应一张订单…）"
	// 精确匹配会**静默落空** —— 字段就是空的，看不出哪里错。
	for _, w := range widgets {
		switch {
		case hasAny(w.Name, "物资所属部门", "归属组"):
			m.Departments = append(m.Departments, deptNames(w.Value)...)
		case hasAny(w.Name, "物资种类"):
			m.MaterialType = jsonRawToString(w.Value)
		case hasAny(w.Name, "物资名称"):
			m.MaterialName = jsonRawToString(w.Value)
		case hasAny(w.Name, "购买人"):
			m.Buyer = jsonRawToString(w.Value)
			m.Applicant = m.Buyer
		case hasAny(w.Name, "资金来源"):
			m.FundSource = jsonRawToString(w.Value)
		case hasAny(w.Name, "是否走大创资金报销"):
			m.Dachuang = jsonRawToString(w.Value)
		case hasAny(w.Name, "是否为支付宝付款"):
			m.IsAlipay = jsonRawToString(w.Value)
		case hasAny(w.Name, "备注"):
			m.Remark = jsonRawToString(w.Value)
		}
	}
	return m
}

// hasAny 判断控件名是否包含任一关键字（新表单的控件名带括号说明）。
func hasAny(name string, keys ...string) bool {
	for _, k := range keys {
		if strings.Contains(name, k) {
			return true
		}
	}
	return false
}

// deptNames 取部门控件里的名称列表（值形如 [{"name":"…","open_id":"…"}]）。
func deptNames(raw json.RawMessage) []string {
	var out []string
	for _, seg := range jsonRawToMaps(raw) {
		if n, ok := seg["name"].(string); ok && n != "" {
			out = append(out, n)
		}
	}
	return out
}

func jsonRawToMaps(raw json.RawMessage) []map[string]any {
	var out []map[string]any
	if len(raw) == 0 {
		return nil
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// jsonRawToString 把控件值转成字符串（兼容纯字符串、数字、富文本片段数组）。
func jsonRawToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	var segs []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &segs) == nil && len(segs) > 0 {
		var sb strings.Builder
		for _, x := range segs {
			sb.WriteString(x.Text)
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// learnDepartments 从表单的 department 控件里累积「open_id → 名称」对照，
// 并返回本次新学到的条目（供落库）。
//
// 控件值形如：[{"name":"机械组","open_id":"od-xxxxxxxx"}]
// 整个团队只会出现十来个部门，跑几条单据就能把对照表补全。
func learnDepartments(widgets []feishu.FormWidget, cache map[string]string) map[string]string {
	learned := map[string]string{}
	for _, w := range widgets {
		if w.Type != "department" {
			continue
		}
		for _, seg := range jsonRawToMaps(w.Value) {
			name, _ := seg["name"].(string)
			id, _ := seg["open_id"].(string)
			if name != "" && id != "" {
				if cache[id] != name {
					learned[id] = name
				}
				cache[id] = name
			}
		}
	}
	return learned
}

// loadDeptCache 把库里累积的部门对照装进内存。
func loadDeptCache(ctx context.Context, db *store.DB, cache map[string]string) {
	m, err := db.AllDepts(ctx)
	if err != nil {
		return
	}
	for k, v := range m {
		cache[k] = v
	}
}

// ProcessOne 处理单个审批实例：下载 → 转PNG → 识别 → 入库。
//
// 这是常驻服务收到审批事件后调用的入口。
// 返回的 Manifest 已含元信息、互核结论与本地路径（供 sync 使用）。
func ProcessOne(ctx context.Context, cfg *config.Config, client *feishu.Client,
	db *store.DB, instanceCode string, quiet bool) (*Manifest, error) {
	outDir := "data/extract"
	if cfg.PDF.TmpDir != "" {
		// 中间产物目录按架构稿用 tmp_dir，但预处理图 sync 还要读，故仍放 extract
		_ = cfg.PDF.TmpDir
	}
	prev := outBase
	outBase = outDir
	defer func() { outBase = prev }()

	if err := run(Options{
		Instance: instanceCode,
		Limit:    1,
		OutDir:   outDir,
		Quiet:    quiet,
	}); err != nil {
		return nil, err
	}
	// run 会把该实例写进 manifest.jsonl；读回来给调用方（sync 需要它）
	return lastManifest(outDir, instanceCode)
}

// lastManifest 从 manifest.jsonl 里取指定实例那一行。
func lastManifest(outDir, instanceCode string) (*Manifest, error) {
	f, err := os.Open(filepath.Join(outDir, "manifest.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var found *Manifest
	for sc.Scan() {
		var m Manifest
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.InstanceCode == instanceCode {
			mm := m
			found = &mm
		}
	}
	if found == nil {
		return nil, fmt.Errorf("manifest 里没有实例 %s", instanceCode)
	}
	return found, nil
}

// saveToDB 把一个实例的抽取结果写入本地 SQLite。
//
// 关键：整单在一个事务里写；任一条证据命中 sha256 唯一约束 → 整单回滚，
// 并由调用方判定为"重复报销"。
func saveToDB(ctx context.Context, db *store.DB, m *Manifest) error {
	sub := store.Submission{
		InstanceCode: m.InstanceCode,
		Status:       "",
		Verdict:      verdictText(m),
	}
	if mt := m.Meta; mt != nil {
		sub.ApprovalName = mt.ApprovalName
		sub.Status = mt.Status
		sub.Applicant = mt.Applicant
		sub.ApplicantDept = mt.ApplicantDept
		sub.MaterialType = mt.MaterialType
		sub.MaterialName = mt.MaterialName
		sub.Buyer = mt.Buyer
		sub.FundSource = mt.FundSource
		sub.Seller = "" // 下面从发票证据里取
	}
	var evs []store.Evidence
	for _, f := range m.Files {
		e := store.Evidence{
			InstanceCode: m.InstanceCode,
			Slot:         f.Slot, Kind: f.Kind, IndexNo: f.Index,
			Filename: f.Filename, MediaType: f.MediaType,
			SHA256: f.SHA256, SizeBytes: f.Bytes,
			UpperCheck: f.UpperChk, TaxCheck: f.TaxChk,
		}
		if len(f.PNGs) > 0 {
			e.LocalPNG = f.PNGs[0] // 相对 data/extract
		}
		if f.OCR != nil {
			e.AmountInclTaxCent = f.OCR.AmountInclTaxCent
			e.TaxCent = f.OCR.TaxCent
			e.AmountExclTaxCent = f.OCR.AmountExclTaxCent
			e.AmountUpper = f.OCR.AmountInclTaxUpper
			e.Date = f.OCR.Date
			e.Counterparty = f.OCR.Counterparty
			e.Provider = f.OCR.Provider
			e.Model = f.OCR.Model
			e.TraceID = f.OCR.TraceID
			c := f.OCR.Confidence
			e.Confidence = &c
			// 发票作为主口径
			if f.Kind == "invoice" {
				sub.AmountCent = f.OCR.AmountInclTaxCent
				sub.TaxCent = f.OCR.TaxCent
				sub.InvoiceDate = f.OCR.Date
				sub.Seller = f.OCR.Counterparty
			}
		}
		evs = append(evs, e)
	}
	return db.SaveInstance(ctx, sub, evs)
}

// verdictText 把互核结论转成表里用的中文。
func verdictText(m *Manifest) string {
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

// shortID 缩短实例号/哈希用于日志（保留前 8 位）。
func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
