// Command download-probe 是「选择性下载」计划的 **P0 门禁探测**。
//
// 目的：在写 cmd/export 之前，用一条**真实记录**把下载链路上每个不确定点
// 都打出来（见 docs/30-review/32 §5.1）：
//
//	① 这条记录的三个附件列里到底有什么（token/name/size/type 是否齐全）
//	② batch_get_tmp_download_url 的真实返回结构（是 GET、一次最多 5 个）
//	③ 本应用在这张表上有没有读素材的权限（bitable:app / drive:drive）
//	④ 开了高级权限的表要不要 extra（bitablePerm），不带给什么错误码（403 还是 400）
//	⑤ 链接能不能真的下载下来，字节数与表里声明的 size 是否一致
//
// 用法：
//
//	go run ./cmd/download-probe -table integrated -record recXXXXXXXX
//	go run ./cmd/download-probe -table integrated -instance 7DB9ADCF... -out data/probe
//	go run ./cmd/download-probe -token <file_token>            # 只探链接与下载
//	go run ./cmd/download-probe -table integrated -record recX -extra '{"bitablePerm":{...}}'
//
// 退出码：0 = 全链路通过；1 = 有任一环节失败（脚本可直接拿它当门禁）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/bitable"
	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

type probe struct {
	cfg      *config.Config
	client   *feishu.Client
	tableID  string
	extra    string
	outDir   string
	failures int
}

func main() {
	var (
		cfgPath  = flag.String("config", "", "config.yml 路径（默认自动向上查找）")
		tableKey = flag.String("table", "integrated", "表：integrated|submission，或直接给 tblXXXX")
		record   = flag.String("record", "", "record_id（rec…）")
		instance = flag.String("instance", "", "审批实例号（与 -record 二选一）")
		tokens   = flag.String("token", "", "直接给 file_token（逗号分隔，最多 5 个）；跳过记录查找")
		extra    = flag.String("extra", "", "高级权限表用的 extra JSON（不传则先不带 extra 试一次）")
		outDir   = flag.String("out", "", "把下载到的文件落到这个目录（默认只算 sha256，不写盘）")
		timeout  = flag.Duration("timeout", 3*time.Minute, "整体超时")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fatal(err)
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	p := &probe{cfg: cfg, client: client, extra: *extra, outDir: *outDir}

	fmt.Println("═══ P0 · 附件下载链路探测 ═══")
	fmt.Printf("  应用        %s\n", cfg.Feishu.AppID)
	fmt.Printf("  多维表格    %s\n", cfg.Feishu.Bitable.AppToken)
	fmt.Printf("  主表        %s\n", *tableKey)

	// ── ① 找到记录，列出附件 ──
	var atts []slotAttachment
	if *tokens != "" {
		for _, t := range splitTokens(*tokens) {
			atts = append(atts, slotAttachment{Slot: "(直接指定)", Att: feishu.AttachmentValue{FileToken: t}})
		}
		fmt.Printf("\n① 跳过记录查找，直接用 %d 个 file_token\n", len(atts))
	} else {
		tableID, err := resolveTable(cfg, *tableKey)
		if err != nil {
			fatal(err)
		}
		p.tableID = tableID
		fmt.Printf("  table_id    %s\n", tableID)
		atts, err = p.findAttachments(ctx, *record, *instance)
		if err != nil {
			fatal(err)
		}
	}

	if len(atts) == 0 {
		fmt.Println("\n✗ 这条记录一个附件都没有 —— 没有可下载的对象，无法完成 P0 验证")
		os.Exit(1)
	}

	// ── ② 取临时链接 ──
	fmt.Println("\n② batch_get_tmp_download_url（GET，单次 ≤ 5 个 token）")
	toks := make([]string, 0, len(atts))
	for _, a := range atts {
		toks = append(toks, a.Att.FileToken)
	}
	urls, err := p.getTmpURLs(ctx, toks)
	if err != nil {
		p.fail("取临时链接失败: %v", err)
		if p.extra == "" {
			fmt.Println("    → 这很可能就是「开了高级权限」：请用 -extra 传 bitablePerm 再试一次")
		}
		os.Exit(1)
	}
	urlByToken := map[string]string{}
	for _, u := range urls {
		urlByToken[u.FileToken] = u.TmpDownloadURL
		fmt.Printf("   ✓ %s → %s\n", u.FileToken, trimURL(u.TmpDownloadURL))
	}

	// ── ③ 下载并校验字节数 ──
	fmt.Println("\n③ 下载并核对（字节数应与表里声明的 size 一致）")
	for _, a := range atts {
		tmp := urlByToken[a.Att.FileToken]
		if tmp == "" {
			p.fail("%s 没有拿到临时链接", a.Att.FileToken)
			continue
		}
		p.downloadOne(ctx, a, tmp)
	}

	// ── ④ 结论 ──
	fmt.Println("\n═══ 结论 ═══")
	if p.failures == 0 {
		fmt.Println("✅ P0 通过：临时链接可取、附件可下载、字节数与表里一致。")
		fmt.Println("   → cmd/export（P1）可以按「只存 file_token、现取现下」开始写。")
	} else {
		fmt.Printf("❌ P0 未通过：%d 个环节失败（见上）。\n", p.failures)
		os.Exit(1)
	}
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

func resolveTable(cfg *config.Config, key string) (string, error) {
	if strings.HasPrefix(key, "tbl") {
		return key, nil
	}
	id := cfg.Feishu.Bitable.Tables[key]
	if id == "" {
		return "", fmt.Errorf("配置里没有表 %q（可用：%v）", key, tableKeys(cfg))
	}
	return id, nil
}

func tableKeys(cfg *config.Config) []string {
	var out []string
	for k := range cfg.Feishu.Bitable.Tables {
		out = append(out, k)
	}
	return out
}

type slotAttachment struct {
	Slot string
	Att  feishu.AttachmentValue
}

// findAttachments 找到记录并枚举三个附件列。
func (p *probe) findAttachments(ctx context.Context, recordID, instance string) ([]slotAttachment, error) {
	if recordID == "" && instance == "" {
		return nil, fmt.Errorf("-record 或 -instance 至少给一个（或直接用 -token）")
	}

	var rec *feishu.BitableRecord
	if recordID != "" {
		r, err := p.client.GetBitableRecord(ctx, p.cfg.Feishu.Bitable.AppToken, p.tableID, recordID)
		if err != nil {
			return nil, fmt.Errorf("按 record_id 取记录: %w", err)
		}
		rec = r
	} else {
		// 按「审批实例号」搜：表里一行一张发票，同一实例可能多行
		recs, err := p.client.SearchBitableRecordsAll(ctx, p.cfg.Feishu.Bitable.AppToken, p.tableID,
			feishu.SearchOptions{
				Filter: map[string]any{
					"conjunction": "and",
					"conditions": []map[string]any{
						{"field_name": "审批实例号", "operator": "is", "value": []string{instance}},
					},
				},
				PageSize: 100,
			})
		if err != nil {
			return nil, fmt.Errorf("按实例号搜记录: %w", err)
		}
		if len(recs) == 0 {
			return nil, fmt.Errorf("表 %s 里找不到审批实例号 %s 的行", p.tableID, instance)
		}
		fmt.Printf("  ✓ 找到 %d 行（一行=一张发票），用第 1 行做探测\n", len(recs))
		rec = &recs[0]
	}

	fmt.Printf("\n① 记录 %s 的附件列\n", rec.RecordID)
	for _, name := range []string{"审批实例号", "发票号码", "购买人", "物资所属部门",
		"图读金额(元)", "图读日期", "销方名称"} {
		if raw, ok := rec.Fields[name]; ok {
			fmt.Printf("   %-14s %s\n", name, bitable.FieldValue(raw))
		}
	}

	var out []slotAttachment
	for _, slot := range []string{"发票", "订单截图", "付款记录"} {
		raw, ok := rec.Fields[slot]
		if !ok {
			fmt.Printf("   %-14s （字段不存在）\n", slot)
			continue
		}
		list, err := feishu.ParseAttachmentCell(raw)
		if err != nil {
			fmt.Printf("   %-14s ✗ %v\n", slot, err)
			continue
		}
		if len(list) == 0 {
			fmt.Printf("   %-14s （空）\n", slot)
			continue
		}
		for i, a := range list {
			fmt.Printf("   %-14s [%d] token=%s name=%q size=%d type=%s\n",
				slot, i+1, a.FileToken, a.Name, a.Size, a.Type)
			if a.TmpURL != "" || a.URL != "" {
				fmt.Printf("        （单元格里自带 url/tmp_url，但有效期短，导出时不要用它们）\n")
			}
			out = append(out, slotAttachment{Slot: slot, Att: a})
		}
	}
	return out, nil
}

// getTmpURLs 按 5 个一批取临时链接；有 extra 时带上。
func (p *probe) getTmpURLs(ctx context.Context, tokens []string) ([]feishu.TmpDownloadURL, error) {
	var all []feishu.TmpDownloadURL
	for i := 0; i < len(tokens); i += feishu.MaxTmpDownloadTokens {
		end := i + feishu.MaxTmpDownloadTokens
		if end > len(tokens) {
			end = len(tokens)
		}
		batch := tokens[i:end]
		extraNote := ""
		if p.extra != "" {
			extraNote = "（带 extra）"
		}
		fmt.Printf("   批次 %d: %d 个 token%s\n", i/feishu.MaxTmpDownloadTokens+1, len(batch), extraNote)
		out, err := p.client.BatchGetTmpDownloadURL(ctx, batch, p.extra)
		if err != nil {
			return all, err
		}
		all = append(all, out...)
	}
	return all, nil
}

// downloadOne 下载单个附件并核对字节数。
func (p *probe) downloadOne(ctx context.Context, a slotAttachment, tmpURL string) {
	var w io.Writer = io.Discard
	var f *os.File
	if p.outDir != "" {
		if err := os.MkdirAll(p.outDir, 0o755); err != nil {
			p.fail("%v", err)
			return
		}
		name := a.Att.Name
		if name == "" {
			name = a.Att.FileToken
		}
		path := filepath.Join(p.outDir, filepath.Base(name))
		var err error
		f, err = os.Create(path)
		if err != nil {
			p.fail("创建 %s: %v", path, err)
			return
		}
		defer f.Close()
		w = f
		fmt.Printf("   → %s\n", path)
	}

	hash := sha256.New()
	res, err := p.client.DownloadTmpURL(ctx, tmpURL, io.MultiWriter(w, hash))
	if err != nil {
		p.fail("下载 %s: %v", a.Att.FileToken, err)
		return
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	fmt.Printf("   ✓ %s/%s: %d 字节 sha256=%s content-type=%s\n",
		a.Slot, a.Att.FileToken, res.Bytes, sum[:16]+"…", res.ContentType)
	if a.Att.Size > 0 && res.Bytes != a.Att.Size {
		p.fail("%s 字节数不符：表里 %d，实际 %d", a.Att.FileToken, a.Att.Size, res.Bytes)
	}
}

func (p *probe) fail(format string, args ...any) {
	p.failures++
	fmt.Printf("   ✗ "+format+"\n", args...)
}

func splitTokens(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// trimURL 只打印链接的开头，避免把带鉴权参数的完整链接抄进日志。
func trimURL(u string) string {
	if i := strings.IndexByte(u, '?'); i > 0 {
		return u[:i] + "?…（含鉴权参数，已截断）"
	}
	return u
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
	os.Exit(1)
}
