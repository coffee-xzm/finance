// Command recon 是只读侦察工具。
//
// 它只调用飞书的 GET 接口，把【现有审批】的定义与历史实例读到本地，
// 输出给人看的字段报告与给程序用的 JSONL。
//
// 安全保证：不写飞书任何数据、不创建实例、不改状态、不订阅任何定义、不写任何多维表格。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

func main() {
	var (
		cfgPath   = flag.String("config", "", "config.yml 路径（默认自动向上查找）")
		outDir    = flag.String("out", "data/recon", "输出目录")
		maxDetail = flag.Int("max-detail", 50, "最多拉取多少条实例详情（0 = 不限）")
		fromStr   = flag.String("from", "", "起始日期 YYYY-MM-DD（默认 90 天前）")
		toStr     = flag.String("to", "", "结束日期 YYYY-MM-DD（默认今天）")
		maxWindow = flag.Int("max-windows", 240, "时间窗口上限（每窗口 10 小时）")
		dryRun    = flag.Bool("dry-run", false,
			"只验证凭证与权限：取 token + 拉审批定义，打印摘要后退出（不拉实例）")
	)
	flag.Parse()

	if err := run(*cfgPath, *outDir, *maxDetail, *fromStr, *toStr, *maxWindow, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath, outDir string, maxDetail int, fromStr, toStr string, maxWindow int, dryRun bool) error {
	// ── 配置 ────────────────────────────────────────────────
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
	if err := cfg.ValidateRecon(); err != nil {
		return fmt.Errorf("%w\n  配置文件: %s", err, cfgPath)
	}
	fmt.Printf("配置: %s\n", cfgPath)
	switch {
	case cfg.Feishu.ApprovalCode != "":
		fmt.Printf("路径 A · 按审批定义枚举: approval_code=%s\n", cfg.Feishu.ApprovalCode)
	case cfg.Feishu.UserID != "":
		fmt.Printf("路径 B · 按用户待办枚举: user_id=%s（不需要 approval_code）\n", cfg.Feishu.UserID)
	}
	fmt.Println()

	if cfg.Feishu.Subscribe.Approval {
		fmt.Println("⚠  config.yml 里 feishu.subscribe.approval = true。")
		fmt.Println("   并行只读期建议置为 false —— 本工具不依赖事件订阅，保持物理隔离。")
		fmt.Println()
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录 %s: %w", outDir, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	// ── dry-run：只验证凭证与权限 ────────────────────────────
	if dryRun {
		fmt.Println("[dry-run] 验证凭证与权限…")
		tok, err := client.TenantAccessToken(ctx)
		if err != nil {
			return fmt.Errorf("tenant_access_token 获取失败（检查 app_id/app_secret）: %w", err)
		}
		fmt.Printf("      ✓ tenant_access_token 获取成功（%d 字符）\n", len(tok))

		if cfg.Feishu.ApprovalCode != "" {
			def, err := client.GetApprovalDefinition(ctx, cfg.Feishu.ApprovalCode)
			if err != nil {
				return fmt.Errorf("审批定义读取失败（检查 approval_code 与 approval scope）: %w", err)
			}
			ws, _ := feishu.ParseForm(def.Form)
			fmt.Printf("      ✓ 审批定义读取成功\n")
			fmt.Printf("        名称: %s\n", def.ApprovalName)
			fmt.Printf("        状态: %s\n", def.Status)
			fmt.Printf("        控件数: %d，流程节点数: %d\n", len(ws), len(def.NodeList))
			fmt.Printf("\n✓ 凭证与只读权限均正常，可以正式跑：go run ./cmd/recon\n")
		} else {
			fmt.Printf("\n✓ 凭证正常。未配置 approval_code，路径 B 需再验证 tasks/search 权限。\n")
		}
		return nil
	}

	// ── 1. 审批定义（仅路径 A 需要；路径 B 没有定义可查）──────────
	var (
		def        *feishu.ApprovalDefinition
		defWidgets []feishu.FormWidget
	)
	if cfg.Feishu.ApprovalCode != "" {
		fmt.Println("[1/4] 拉取审批定义…")
		def, err = client.GetApprovalDefinition(ctx, cfg.Feishu.ApprovalCode)
		if err != nil {
			return fmt.Errorf("拉取审批定义失败（检查 approval_code 与已有权限）: %w", err)
		}
		defRaw, _ := json.MarshalIndent(def, "", "  ")
		if err := os.WriteFile(filepath.Join(outDir, "definition.json"), defRaw, 0o644); err != nil {
			return err
		}
		defWidgets, err = feishu.ParseForm(def.Form)
		if err != nil {
			return fmt.Errorf("解析定义表单: %w", err)
		}
		fmt.Printf("      名称: %s\n", def.ApprovalName)
		fmt.Printf("      状态: %s\n", def.Status)
		fmt.Printf("      定义控件数: %d，流程节点数: %d\n\n", len(defWidgets), len(def.NodeList))
	} else {
		fmt.Println("[1/4] 跳过审批定义（路径 B 未配置 approval_code）")
		fmt.Println("      ⚠ 控件类型改由实例详情聚合得出；表单字段仍会完整输出。")
		fmt.Println()
	}

	// ── 2. 发现实例 ─────────────────────────────────────────
	var codes []string
	switch {
	case cfg.Feishu.ApprovalCode != "":
		to, from, err := parseRange(toStr, fromStr)
		if err != nil {
			return err
		}
		fmt.Printf("[2/4] 拉取实例列表（按定义，时间窗 ≤10h，%s ~ %s）…\n",
			from.Format("2006-01-02"), to.Format("2006-01-02"))
		var rawPages []json.RawMessage
		codes, rawPages, err = client.ListInstancesInRange(ctx, cfg.Feishu.ApprovalCode, from, to, maxWindow)
		if err != nil {
			return fmt.Errorf("拉取实例列表失败: %w", err)
		}
		if err := writeJSONL(filepath.Join(outDir, "instances_list.jsonl"), rawPages); err != nil {
			return err
		}

	case cfg.Feishu.UserID != "":
		fmt.Printf("[2/4] 搜索用户任务（tasks/search，不需要 approval_code）…\n")
		items, rawPages, err := client.SearchTasks(ctx,
			feishu.TaskSearchRequest{UserID: cfg.Feishu.UserID},
			cfg.Feishu.UserIDType, 50)
		if err != nil {
			return fmt.Errorf("搜索用户任务失败: %w", err)
		}
		if err := writeJSONL(filepath.Join(outDir, "tasks_search.jsonl"), rawPages); err != nil {
			return err
		}
		seen := map[string]bool{}
		skipped := 0
		for _, it := range items {
			// ★ 必须按 approval_code 过滤：tasks/search 返回的是"该用户的全部任务"，
			//   不区分审批定义。不过滤就会把「<OTHER_APPROVAL_1>」等别的表单的实例
			//   一起写进 forms.jsonl，后续 extract/sync 会当成报销单处理。
			//   租户里实测有 4 个审批表单，这个口子必须堵死。
			if cfg.Feishu.ApprovalCode != "" && it.ApprovalCode != cfg.Feishu.ApprovalCode {
				skipped++
				continue
			}
			if it.InstanceCode != "" && !seen[it.InstanceCode] {
				seen[it.InstanceCode] = true
				codes = append(codes, it.InstanceCode)
			}
		}
		fmt.Printf("      任务 %d 条 → 去重后 %d 个实例", len(items), len(codes))
		if skipped > 0 {
			fmt.Printf("（已按 approval_code 过滤掉 %d 条其它审批定义的任务）", skipped)
		}
		fmt.Println()
		fmt.Println("      ⚠ 该路径只覆盖此用户的任务，不等于该审批定义的全量实例。")
		fmt.Println()
	}

	fmt.Printf("      共 %d 条实例\n\n", len(codes))

	if len(codes) == 0 {
		fmt.Println("      没有取到实例 —— 先发一条测试审批再跑一次。")
	}

	if maxDetail > 0 && len(codes) > maxDetail {
		codes = codes[:maxDetail]
		fmt.Printf("      （按 -max-detail=%d 截断）\n\n", maxDetail)
	}

	// ── 3. 实例详情 ─────────────────────────────────────────
	fmt.Printf("[3/4] 拉取 %d 条实例详情（限速 5 req/s）…\n", len(codes))
	detailFile, err := os.Create(filepath.Join(outDir, "instances_detail.jsonl"))
	if err != nil {
		return err
	}
	defer detailFile.Close()
	formFile, err := os.Create(filepath.Join(outDir, "forms.jsonl"))
	if err != nil {
		return err
	}
	defer formFile.Close()
	dw, fw := bufio.NewWriter(detailFile), bufio.NewWriter(formFile)
	defer dw.Flush()
	defer fw.Flush()

	agg := newAggregate()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var okCount, failCount int
	for i, code := range codes {
		<-ticker.C
		detail, raw, err := client.GetInstanceDetail(ctx, code)
		if err != nil {
			failCount++
			fmt.Printf("      [%d/%d] %s ✗ %v\n", i+1, len(codes), code, err)
			continue
		}
		okCount++
		dw.Write(raw)
		dw.WriteByte('\n')

		ws, perr := feishu.ParseForm(detail.Form)
		if perr != nil {
			fmt.Printf("      [%d/%d] %s ⚠ form 解析失败: %v\n", i+1, len(codes), code, perr)
		}
		row := map[string]any{
			"instance_code": detail.InstanceCode,
			"status":        detail.Status,
			"start_time":    detail.StartTime,
			"end_time":      detail.EndTime,
			"widgets":       flatten(ws),
		}
		if b, mErr := json.Marshal(row); mErr == nil {
			fw.Write(b)
			fw.WriteByte('\n')
		}
		agg.add(detail, ws)
		if (i+1)%10 == 0 {
			fmt.Printf("      [%d/%d] …\n", i+1, len(codes))
		}
	}

	fmt.Printf("\n      成功 %d 条，失败 %d 条\n\n", okCount, failCount)

	// ── 报告 ────────────────────────────────────────────────
	report := agg.render(def, defWidgets, len(codes), okCount, failCount)
	rp := filepath.Join(outDir, "report.md")
	if err := os.WriteFile(rp, []byte(report), 0o644); err != nil {
		return err
	}
	fmt.Printf("✓ 报告: %s\n", rp)
	fmt.Printf("  原始: %s/{definition.json,instances_list.jsonl,instances_detail.jsonl,forms.jsonl}\n", outDir)
	return nil
}

func writeJSONL(path string, items []json.RawMessage) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	for _, it := range items {
		w.Write(it)
		w.WriteByte('\n')
	}
	return nil
}

// flatten 把控件列表转成 {key: value} 便于阅读。
func flatten(ws []feishu.FormWidget) map[string]any {
	out := make(map[string]any, len(ws))
	for _, w := range ws {
		var v any
		if len(w.Value) > 0 {
			_ = json.Unmarshal(w.Value, &v)
		}
		out[w.Key()] = v
	}
	return out
}

// ── 聚合 ────────────────────────────────────────────────────

type aggregate struct {
	statusCount map[string]int
	typeCount   map[string]int
	keyCount    map[string]int
	keyNames    map[string]string
	keyTypes    map[string]string
	keySamples  map[string]string
	attachKeys  map[string]int
	attachName  map[string]string
	attachTotal int
	order       []string
	formFailed  int
}

func newAggregate() *aggregate {
	return &aggregate{
		statusCount: map[string]int{},
		typeCount:   map[string]int{},
		keyCount:    map[string]int{},
		keyNames:    map[string]string{},
		keyTypes:    map[string]string{},
		keySamples:  map[string]string{},
		attachKeys:  map[string]int{},
		attachName:  map[string]string{},
	}
}

func (a *aggregate) add(d *feishu.InstanceDetail, ws []feishu.FormWidget) {
	a.statusCount[d.Status]++
	if d.Form == "" {
		a.formFailed++
	}
	for _, w := range ws {
		k := w.Key()
		if _, seen := a.keyNames[k]; !seen {
			a.order = append(a.order, k)
			a.keyNames[k] = w.Name
		}
		if a.keyTypes[k] == "" {
			a.keyTypes[k] = w.Type
		}
		a.typeCount[w.Type]++
		a.keyCount[k]++

		if len(w.Value) > 0 && string(w.Value) != "null" {
			s := string(w.Value)
			if len([]rune(s)) > 160 {
				s = string([]rune(s)[:160]) + "…"
			}
			if a.keySamples[k] == "" {
				a.keySamples[k] = s
			}
		}
		if urls := w.AttachmentURLs(); len(urls) > 0 {
			a.attachKeys[k] += len(urls)
			a.attachTotal += len(urls)
			a.attachName[k] = w.Name
		}
	}
}

func (a *aggregate) render(def *feishu.ApprovalDefinition, defWidgets []feishu.FormWidget, total, ok, fail int) string {
	var b strings.Builder
	now := time.Now().Format("2006-01-02 15:04:05")

	fmt.Fprintf(&b, "# 现有审批侦察报告\n\n")
	fmt.Fprintf(&b, "> 生成时间：%s ｜ 工具：`cmd/recon`（**只读**，不写飞书任何数据）\n\n", now)

	if def == nil {
		fmt.Fprintf(&b, "## 1. 审批定义\n\n")
		fmt.Fprintf(&b, "_未拉取（未配置 `feishu.approval_code`，走了按用户任务枚举的路径）。_\n")
		fmt.Fprintf(&b, "控件类型改由实例详情聚合得出，见 §3。\n\n")
	} else {
		fmt.Fprintf(&b, "## 1. 审批定义\n\n")
		fmt.Fprintf(&b, "| 项 | 值 |\n|---|---|\n")
		fmt.Fprintf(&b, "| 名称 | %s |\n", def.ApprovalName)
		fmt.Fprintf(&b, "| 状态 | %s |\n", def.Status)
		fmt.Fprintf(&b, "| 定义控件数 | %d |\n", len(defWidgets))
		fmt.Fprintf(&b, "| 流程节点数 | %d |\n\n", len(def.NodeList))

		if len(def.NodeList) > 0 {
			fmt.Fprintf(&b, "### 1.1 流程节点\n\n| # | 节点名 | node_type | 需自选审批人 | 可多选 |\n|---|---|---|---|---|\n")
			for i, n := range def.NodeList {
				fmt.Fprintf(&b, "| %d | %s | %s | %v | %v |\n", i+1, n.Name, n.NodeType, n.NeedApprover, n.ApproverChosenMulti)
			}
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "## 2. 实例概况\n\n")
	fmt.Fprintf(&b, "| 项 | 值 |\n|---|---|\n")
	fmt.Fprintf(&b, "| 列表返回 | %d 条 |\n", total)
	fmt.Fprintf(&b, "| 详情成功 | %d 条 |\n", ok)
	fmt.Fprintf(&b, "| 详情失败 | %d 条 |\n", fail)
	fmt.Fprintf(&b, "| form 为空 | %d 条 |\n\n", a.formFailed)

	fmt.Fprintf(&b, "### 2.1 状态分布\n\n| status | 条数 |\n|---|---|\n")
	for _, kv := range sortedCounts(a.statusCount) {
		fmt.Fprintf(&b, "| %s | %d |\n", kv.k, kv.v)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "## 3. 表单字段清单（★ 最关键）\n\n")
	fmt.Fprintf(&b, "按出现次数排序。`类型` 为飞书控件 type 原文。\n\n")
	fmt.Fprintf(&b, "| # | key (custom_id/id) | 显示名 | 类型 | 出现次数 | 附件 token 数 | 样例值 |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")

	type row struct {
		key, name, typ string
		count          int
	}
	var rows []row
	for _, k := range a.order {
		rows = append(rows, row{key: k, name: a.keyNames[k], typ: typeOfKey(k, a, defWidgets), count: a.keyCount[k]})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].count > rows[j].count })
	for i, r := range rows {
		attach := ""
		if n := a.attachKeys[r.key]; n > 0 {
			attach = fmt.Sprintf("**%d**", n)
		}
		sample := a.keySamples[r.key]
		sample = strings.ReplaceAll(sample, "|", "\\|")
		if sample == "" {
			sample = "_(空)_"
		}
		fmt.Fprintf(&b, "| %d | `%s` | %s | %s | %d | %s | %s |\n",
			i+1, r.key, r.name, r.typ, r.count, attach, sample)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "### 3.1 控件类型分布\n\n| type | 出现次数 |\n|---|---|\n")
	for _, kv := range sortedCounts(a.typeCount) {
		fmt.Fprintf(&b, "| %s | %d |\n", kv.k, kv.v)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "## 4. 结论与判断\n\n")
	fmt.Fprintf(&b, "### 4.1 附件（图片）承载能力 —— 决定三单匹配能否成立\n\n")
	if a.attachTotal == 0 {
		fmt.Fprintf(&b, "**未发现任何附件值。**\n\n")
		fmt.Fprintf(&b, "三种可能，需人工确认：\n\n")
		fmt.Fprintf(&b, "1. 该审批表单**没有附件控件** → 图片不在审批里，需另找来源（多维表格/群聊/其他表）；\n")
		fmt.Fprintf(&b, "2. 有附件控件但**历史实例都没传图** → 换个填了图的实例再验；\n")
		fmt.Fprintf(&b, "3. 附件 value 的 JSON 结构与预期不同 → 请查看 `forms.jsonl` 里对应控件的原始值。\n")
	} else {
		fmt.Fprintf(&b, "**发现 %d 个附件，分布在 %d 个控件上。**\n\n", a.attachTotal, len(a.attachKeys))
		fmt.Fprintf(&b, "| 控件 key | 显示名 | 附件数 |\n|---|---|---|\n")
		for _, kv := range sortedCounts(a.attachKeys) {
			fmt.Fprintf(&b, "| `%s` | %s | %d |\n", kv.k, a.attachName[kv.k], kv.v)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "**附件 value 的实际结构（实测）**：字符串数组，元素是**带 authcode 的下载直链**，\n")
		fmt.Fprintf(&b, "形如 `https://internal-api-drive-stream.feishu.cn/space/api/box/stream/download/authcode/?code=<base64>`。\n\n")
		fmt.Fprintf(&b, "| 结论 | 依据 |\n|---|---|\n")
		fmt.Fprintf(&b, "| **没有 `file_token`** | 所以走不通 `drive/v1/medias/:file_token/download` |\n")
		fmt.Fprintf(&b, "| **直链可直接 GET** | 实测 HTTP 200，无需额外鉴权头（authcode 自身即凭证） |\n")
		fmt.Fprintf(&b, "| **有效期 24 小时** | authcode 解码含 `_ID:<id>_<起>_<止>_V3`，起止相差 86400 秒 |\n\n")
		fmt.Fprintf(&b, "→ 因此：**必须现读现用，不能缓存 URL**；缓存了也要重取实例详情换新链接。\n")
	}

	fmt.Fprintf(&b, "\n### 4.2 关联键\n\n")
	fmt.Fprintf(&b, "本地关联键**推荐直接用 `instance_code`**（天然唯一、必然存在、无需队员多填）。\n")
	fmt.Fprintf(&b, "若表单里已有单号/工号类字段，可作为辅助核对：\n\n")
	found := false
	for _, r := range rows {
		lk := strings.ToLower(r.key + " " + r.name)
		if strings.Contains(lk, "单号") || strings.Contains(lk, "编号") ||
			strings.Contains(lk, "工号") || strings.Contains(lk, "订单") ||
			strings.Contains(lk, "no") || strings.Contains(lk, "id") {
			fmt.Fprintf(&b, "- `%s`（%s）\n", r.key, r.name)
			found = true
		}
	}
	if !found {
		fmt.Fprintf(&b, "_(未发现明显的单号类字段)_\n")
	}

	fmt.Fprintf(&b, "\n### 4.3 后续动作\n\n")
	fmt.Fprintf(&b, "- [ ] 人工核对本报告 §3，确认字段是否够做三单匹配（金额/日期/对方/发票号）\n")
	fmt.Fprintf(&b, "- [ ] 若附件链路成立 → 实现审批附件下载（含高级权限 `extra` 鉴权）\n")
	fmt.Fprintf(&b, "- [ ] 只有在需要**实时**感知新提交时才订阅该定义；在此之前保持不订阅\n")
	fmt.Fprintf(&b, "- [ ] 该定义**不要**被审批通过（会触发旧自动化）；测试期只读不批\n")

	return b.String()
}

func typeOfKey(key string, a *aggregate, defWidgets []feishu.FormWidget) string {
	for _, w := range defWidgets {
		if w.Key() == key {
			return w.Type
		}
	}
	// 路径 B（无审批定义）：类型由实例详情聚合得出
	if t := a.keyTypes[key]; t != "" {
		return t + " *"
	}
	return "?"
}

// parseRange 解析 -from/-to（YYYY-MM-DD），默认最近 90 天。
func parseRange(toStr, fromStr string) (to, from time.Time, err error) {
	now := time.Now()
	to = time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, now.Location())
	from = to.AddDate(0, 0, -90)
	if toStr != "" {
		to, err = time.ParseInLocation("2006-01-02", toStr, time.Local)
		if err != nil {
			return to, from, fmt.Errorf("-to 格式应为 YYYY-MM-DD: %w", err)
		}
		to = to.Add(24*time.Hour - time.Second)
	}
	if fromStr != "" {
		from, err = time.ParseInLocation("2006-01-02", fromStr, time.Local)
		if err != nil {
			return to, from, fmt.Errorf("-from 格式应为 YYYY-MM-DD: %w", err)
		}
	}
	if !to.After(from) {
		return to, from, fmt.Errorf("-to（%s）必须晚于 -from（%s）",
			to.Format("2006-01-02"), from.Format("2006-01-02"))
	}
	return to, from, nil
}

type kv struct {
	k string
	v int
}

func sortedCounts(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].v != out[j].v {
			return out[i].v > out[j].v
		}
		return out[i].k < out[j].k
	})
	return out
}
