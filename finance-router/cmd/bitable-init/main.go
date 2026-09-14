// Command bitable-init 在指定多维表格里建好「源表 + 整合表」两张表。
//
// 它做的事：
//  1. 把目标 Bitable 里的默认表改名为「识别结果_待审」，并把默认字段改名为「审批实例号」
//  2. 建齐源表的其余字段（含单选选项）
//  3. 新建「报销整合」表并建齐其字段
//
// 用法：
//
//	go run ./cmd/bitable-init -app-token <app_token> -table <默认table_id> -dry-run
//	go run ./cmd/bitable-init -app-token <app_token> -table <默认table_id>
//
// ⚠️ 这是**写操作**。默认 -dry-run，必须显式去掉才真写。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/bitable"
	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

func main() {
	var (
		cfgPath  = flag.String("config", "", "config.yml 路径")
		appToken = flag.String("app-token", "", "多维表格 app_token（默认取 config）")
		tableID  = flag.String("table", "", "默认表的 table_id（源表落在这里）")
		integID  = flag.String("integrated-table", "", "若已存在整合表，填其 table_id（否则新建）")
		dryRun   = flag.Bool("dry-run", true, "只打印计划，不写入。要真写请显式 -dry-run=false")
		newTable = flag.Bool("new", false, "新建一张表（而不是复用现有表）")
		renames  = flag.String("rename", "",
			"重命名字段，多个用逗号分隔，形如 旧名=新名,旧名2=新名2")
		align = flag.Bool("align", false,
			"把已存在字段的类型/选项对齐到 schema（选项变了、类型改了时用）")
		prune = flag.Bool("prune", false,
			"删除 schema 里没有的字段（表单删掉的项、历史遗留列）")
	)
	flag.Parse()
	if err := run(*cfgPath, *appToken, *tableID, *integID, *dryRun, *newTable, *renames, *align, *prune); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath, appToken, tableID, integID string, dryRun, newTable bool, renames string,
	align, prune bool) error {
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
	if appToken == "" {
		appToken = cfg.Feishu.Bitable.AppToken
	}
	if appToken == "" {
		return fmt.Errorf("未指定 app_token")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	src := bitable.ReviewTable()
	integ := bitable.IntegratedTable()
	_ = newTable // 见下方建表分支

	// ── 前置检查：先读一次，确认读权限与目标位置 ──────────────
	tables, err := client.ListBitableTables(ctx, appToken)
	if err != nil {
		return fmt.Errorf("读取数据表失败: %w", err)
	}
	fmt.Printf("app_token: %s\n", appToken)
	fmt.Printf("现有数据表 %d 张：\n", len(tables))
	for _, t := range tables {
		fmt.Printf("  %-20s %s\n", t.TableID, t.Name)
	}
	fmt.Println()

	if tableID == "" {
		if len(tables) == 0 {
			return fmt.Errorf("该文档没有任何数据表，无法确定源表位置；请先在 UI 里建一张")
		}
		tableID = tables[0].TableID
		fmt.Printf("未指定 -table，自动选用第一张: %s (%s)\n\n", tableID, tables[0].Name)
	}
	// 若已有名字像"报销整合"的表，复用它
	for _, t := range tables {
		if integID == "" && strings.Contains(t.Name, "报销整合") {
			integID = t.TableID
			fmt.Printf("发现已存在的整合表: %s (%s)\n\n", integID, t.Name)
		}
	}

	// -new：先建一张空表，再往里加字段
	if newTable {
		if dryRun {
			fmt.Printf("将新建数据表: %s\n", src.Name)
			return nil
		}
		id, err := client.CreateBitableTable(ctx, appToken, src.Name)
		if err != nil {
			return fmt.Errorf("新建源表失败: %w", err)
		}
		tableID = id
		fmt.Printf("✓ 已新建源表 %s（%s）\n", src.Name, id)
	}

	// ── 按需重命名字段（保留数据，比删了重建安全）──────
	if renames != "" {
		if tableID == "" {
			return fmt.Errorf("-rename 需要同时指定 -table")
		}
		fields, err := client.ListBitableFields(ctx, appToken, tableID)
		if err != nil {
			return err
		}
		byName := map[string]feishu.BitableField{}
		for _, f := range fields {
			byName[f.FieldName] = f
		}
		for _, pair := range strings.Split(renames, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("-rename 格式应为 旧名=新名，实际 %q", pair)
			}
			oldN, newN := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			f, ok := byName[oldN]
			if !ok {
				if _, exists := byName[newN]; exists {
					fmt.Printf("  − %s 已是 %s，跳过\n", oldN, newN)
					continue
				}
				fmt.Printf("  ? 找不到字段 %q，跳过\n", oldN)
				continue
			}
			if dryRun {
				fmt.Printf("  将重命名 %s → %s\n", oldN, newN)
				continue
			}
			if err := client.UpdateBitableField(ctx, appToken, tableID, f.FieldID,
				feishu.FieldSpec{FieldName: newN, Type: f.Type}); err != nil {
				return fmt.Errorf("重命名 %s → %s 失败: %w", oldN, newN, err)
			}
			fmt.Printf("  ✓ %s → %s\n", oldN, newN)
		}
		if dryRun {
			return nil
		}
	}

	// ── 打印计划 ────────────────────────────────────────────
	fmt.Println("════ 计划 ════")
	fmt.Printf("源表  %s\n", src.Name)
	fmt.Printf("  目标 table_id: %s\n", tableID)
	fields, err := client.ListBitableFields(ctx, appToken, tableID)
	if err != nil {
		return fmt.Errorf("读取源表字段失败: %w", err)
	}
	var primary *feishu.BitableField
	existing := map[string]bool{}
	for i := range fields {
		if fields[i].IsPrimary {
			primary = &fields[i]
		}
		existing[fields[i].FieldName] = true
	}
	if primary != nil {
		fmt.Printf("  主字段: %q (id=%s) → 将改名为 %q\n", primary.FieldName, primary.FieldID, src.Fields[0].Name)
	}
	plan := planFields(src, existing)
	fixes := planFixes(src, fields)
	extras := planPrune(src, fields)
	fmt.Printf("  需新增 %d 个字段（跳过已存在的）\n", len(plan))
	printFixPlan(fixes, extras, align, prune)

	fmt.Printf("\n整合表 %s\n", integ.Name)
	if integID == "" {
		fmt.Printf("  将新建数据表\n")
	} else {
		fmt.Printf("  复用 table_id: %s\n", integID)
	}
	fmt.Printf("  字段 %d 个\n", len(integ.Fields))

	if dryRun {
		fmt.Println("\n（dry-run：未做任何写入。去掉 -dry-run=false 才真写）")
		return nil
	}

	// ── 执行 ────────────────────────────────────────────────
	fmt.Println("\n════ 执行 ════")

	if primary != nil && primary.FieldName != src.Fields[0].Name {
		err := client.UpdateBitableField(ctx, appToken, tableID, primary.FieldID, feishu.FieldSpec{
			FieldName: src.Fields[0].Name, Type: int(primary.Type),
		})
		if err != nil {
			return fmt.Errorf("重命名主字段失败（写权限不足？）: %w", err)
		}
		fmt.Printf("  ✓ 主字段改名为 %q\n", src.Fields[0].Name)
	}

	created := 0
	for _, f := range plan {
		spec := toSpec(f)
		if _, err := client.CreateBitableField(ctx, appToken, tableID, spec); err != nil {
			return fmt.Errorf("新增字段 %q 失败: %w", f.Name, err)
		}
		created++
		fmt.Printf("  ✓ %s (%s)\n", f.Name, typeName(f.Type))
	}
	fmt.Printf("源表：新增 %d 个字段\n", created)
	if align {
		if err := applyFixes(ctx, client, appToken, tableID, fixes, dryRun); err != nil {
			return err
		}
	}
	if prune {
		if err := applyPrune(ctx, client, appToken, tableID, extras, dryRun); err != nil {
			return err
		}
	}

	if integID == "" {
		newID, err := client.CreateBitableTable(ctx, appToken, integ.Name)
		if err != nil {
			return fmt.Errorf("新建整合表失败: %w", err)
		}
		integID = newID
		fmt.Printf("  ✓ 新建整合表 %s\n", integID)
	}
	// 整合表也先建字段（含一个主字段）
	integFields, _ := client.ListBitableFields(ctx, appToken, integID)
	if len(integFields) > 0 && integFields[0].IsPrimary {
		_ = client.UpdateBitableField(ctx, appToken, integID, integFields[0].FieldID, feishu.FieldSpec{
			FieldName: integ.Fields[0].Name, Type: int(integFields[0].Type),
		})
	}
	existingInteg := map[string]bool{}
	for _, f := range integFields {
		existingInteg[f.FieldName] = true
	}
	createdInteg := 0
	for _, f := range planFields(integ, existingInteg) {
		if _, err := client.CreateBitableField(ctx, appToken, integID, toSpec(f)); err != nil {
			return fmt.Errorf("整合表新增字段 %q 失败: %w", f.Name, err)
		}
		createdInteg++
		fmt.Printf("  ✓ [整合] %s\n", f.Name)
	}
	fmt.Printf("整合表：新增 %d 个字段\n", createdInteg)
	integAll, _ := client.ListBitableFields(ctx, appToken, integID)
	if align {
		if err := applyFixes(ctx, client, appToken, integID, planFixes(integ, integAll), dryRun); err != nil {
			return err
		}
	}
	if prune {
		if err := applyPrune(ctx, client, appToken, integID, planPrune(integ, integAll), dryRun); err != nil {
			return err
		}
	}

	fmt.Printf("\n✓ 完成。请把下面两行填进 %s 的 feishu.bitable 段：\n", filepath.Base(cfgPath))
	fmt.Printf("  app_token: %q\n", appToken)
	fmt.Printf("  tables:\n    submission: %q\n    integrated: %q\n", tableID, integID)
	return nil
}

// planFields 过滤掉已存在的字段（第一个字段视为主字段，单独处理）。
func planFields(t bitable.Table, existing map[string]bool) []bitable.Field {
	var out []bitable.Field
	for i, f := range t.Fields {
		if i == 0 {
			continue // 主字段由重命名处理
		}
		if existing[f.Name] {
			continue
		}
		out = append(out, f)
	}
	return out
}

func toSpec(f bitable.Field) feishu.FieldSpec {
	spec := feishu.FieldSpec{FieldName: f.Name, Type: int(f.Type)}
	var prop map[string]any
	if len(f.Options) > 0 {
		opts := make([]map[string]any, 0, len(f.Options))
		for _, o := range f.Options {
			opts = append(opts, map[string]any{"name": o})
		}
		prop = map[string]any{"options": opts}
	}
	if f.Formatter != "" || f.DateFmt != "" {
		if prop == nil {
			prop = map[string]any{}
		}
		if f.Formatter != "" {
			prop["formatter"] = f.Formatter
		}
		if f.DateFmt != "" {
			prop["date_formatter"] = f.DateFmt
		}
	}
	spec.Property = prop
	return spec
}

func typeName(t bitable.FieldType) string {
	switch t {
	case bitable.TypeText:
		return "文本"
	case bitable.TypeNumber:
		return "数字"
	case bitable.TypeSingleSelect:
		return "单选"
	case bitable.TypeMultiSelect:
		return "多选"
	case bitable.TypeDate:
		return "日期"
	case bitable.TypeCheckbox:
		return "复选框"
	case bitable.TypeURL:
		return "超链接"
	case bitable.TypeAttachment:
		return "附件"
	}
	return fmt.Sprintf("type=%d", int(t))
}

// ────────────────────────── 对齐与清理 ──────────────────────────

// fieldFix 是"已有字段与 schema 不一致"的一条修正。
type fieldFix struct {
	FieldID string
	Name    string
	Why     string
	Spec    feishu.FieldSpec
}

// planFixes 找出类型或选项与 schema 不一致的已有字段。
//
// 为什么需要：schema 改了（比如资金来源的选项从一串老师名简化成"个人/老师垫付"），
// 光靠"新增缺失字段"是改不动的 —— 字段已存在就被跳过了，表里会一直留着旧选项。
func planFixes(t bitable.Table, fields []feishu.BitableField) []fieldFix {
	byName := map[string]feishu.BitableField{}
	for _, e := range fields {
		byName[e.FieldName] = e
	}
	var out []fieldFix
	for _, f := range t.Fields {
		e, ok := byName[f.Name]
		if !ok || e.IsPrimary {
			continue
		}
		if int(f.Type) != e.Type {
			out = append(out, fieldFix{e.FieldID, f.Name,
				fmt.Sprintf("类型 %d → %d", e.Type, int(f.Type)), toSpec(f)})
			continue
		}
		if len(f.Options) == 0 {
			continue
		}
		if !sameOptions(f.Options, existingOptions(e.Property)) {
			out = append(out, fieldFix{e.FieldID, f.Name,
				"选项 " + strings.Join(existingOptions(e.Property), "/") +
					" → " + strings.Join(f.Options, "/"), toSpec(f)})
		}
	}
	return out
}

// existingOptions 从字段属性里取出单/多选选项名。
func existingOptions(prop json.RawMessage) []string {
	if len(prop) == 0 {
		return nil
	}
	var p struct {
		Options []struct {
			Name string `json:"name"`
		} `json:"options"`
	}
	if json.Unmarshal(prop, &p) != nil {
		return nil
	}
	var out []string
	for _, o := range p.Options {
		out = append(out, o.Name)
	}
	return out
}

// sameOptions 比较两组选项（顺序无关 —— 顺序只影响下拉框里的排列）。
func sameOptions(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, x := range b {
		set[x] = true
	}
	for _, x := range a {
		if !set[x] {
			return false
		}
	}
	return true
}

// planPrune 找出表里有、但 schema 里没有的字段（跳过主字段）。
func planPrune(t bitable.Table, fields []feishu.BitableField) []feishu.BitableField {
	want := map[string]bool{}
	for _, f := range t.Fields {
		want[f.Name] = true
	}
	var out []feishu.BitableField
	for _, e := range fields {
		if e.IsPrimary || want[e.FieldName] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func printFixPlan(fixes []fieldFix, extras []feishu.BitableField, align, prune bool) {
	if align && len(fixes) > 0 {
		fmt.Printf("  需对齐 %d 个已有字段：\n", len(fixes))
		for _, f := range fixes {
			fmt.Printf("    ~ %s（%s）\n", f.Name, f.Why)
		}
	}
	if prune && len(extras) > 0 {
		fmt.Printf("  需删除 %d 个 schema 里没有的字段：\n", len(extras))
		for _, e := range extras {
			fmt.Printf("    − %s\n", e.FieldName)
		}
	}
	if !align && len(fixes) > 0 {
		fmt.Printf("  ⚠ %d 个已有字段与 schema 不一致（加 -align 才会改）\n", len(fixes))
	}
	if !prune && len(extras) > 0 {
		fmt.Printf("  ⚠ %d 个字段 schema 里没有（加 -prune 才会删）\n", len(extras))
	}
}

func applyFixes(ctx context.Context, c *feishu.Client, appToken, tableID string,
	fixes []fieldFix, dryRun bool) error {
	for _, f := range fixes {
		if dryRun {
			continue
		}
		if err := c.UpdateBitableField(ctx, appToken, tableID, f.FieldID, f.Spec); err != nil {
			return fmt.Errorf("对齐字段 %q 失败: %w", f.Name, err)
		}
		fmt.Printf("  ✓ [对齐] %s（%s）\n", f.Name, f.Why)
	}
	return nil
}

func applyPrune(ctx context.Context, c *feishu.Client, appToken, tableID string,
	extras []feishu.BitableField, dryRun bool) error {
	for _, e := range extras {
		if dryRun {
			continue
		}
		if err := c.DeleteBitableField(ctx, appToken, tableID, e.FieldID); err != nil {
			return fmt.Errorf("删除字段 %q 失败: %w", e.FieldName, err)
		}
		fmt.Printf("  ✓ [删除] %s\n", e.FieldName)
	}
	return nil
}
