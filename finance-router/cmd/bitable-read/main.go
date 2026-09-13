// Command bitable-read 只读读取一个多维表格的真实结构（数据表 + 字段）。
//
// 用途：把「测试表该建哪些字段」从"我拍脑袋设计"改成"照着真实表来"。
// 它只调用两个只读 GET 接口：
//
//	GET /bitable/v1/apps/:app_token/tables
//	GET /bitable/v1/apps/:app_token/tables/:table_id/fields
//
// 权限：base:table:read + base:field:read，或 bitable:app:readonly。
// **不写任何数据、不改任何字段。**
//
// 用法：
//
//	go run ./cmd/bitable-read                          # app_token 取 config.yml
//	go run ./cmd/bitable-read -app-token bascnXXXX     # 或直接指定
//	go run ./cmd/bitable-read -table tblXXXX           # 只看某张表
package main

import (
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
		cfgPath  = flag.String("config", "", "config.yml 路径（默认自动向上查找）")
		appToken = flag.String("app-token", "", "多维表格 app_token（默认取 config.feishu.bitable.app_token）")
		onlyTbl  = flag.String("table", "", "只看指定 table_id")
		outDir   = flag.String("out", "data/bitable", "原始 JSON 输出目录")
		records  = flag.Int("records", 0, "额外只读拉取 N 条记录，用于确认字段取值形态（0=不拉）")
	)
	flag.Parse()
	if err := run(*cfgPath, *appToken, *onlyTbl, *outDir, *records); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}

func run(cfgPath, appToken, onlyTbl, outDir string, records int) error {
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
		return fmt.Errorf("未指定 app_token。用 -app-token，或在 %s 里填 feishu.bitable.app_token", cfgPath)
	}
	if cfg.Feishu.AppID == "" || cfg.Feishu.AppSecret == "" {
		return fmt.Errorf("配置缺少 feishu.app_id / app_secret")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	fmt.Printf("配置: %s\napp_token: %s\n\n", cfgPath, appToken)
	fmt.Println("[1/2] 列出数据表…")
	tables, err := client.ListBitableTables(ctx, appToken)
	if err != nil {
		return fmt.Errorf("列出数据表失败（检查 app_token 与 bitable:app:readonly 权限、"+
			"以及应用是否已加为该文档协作者）: %w", err)
	}
	if len(tables) == 0 {
		fmt.Println("      该文档下没有数据表。")
		return nil
	}
	fmt.Printf("      共 %d 张表：\n", len(tables))
	for _, t := range tables {
		mark := "  "
		if t.TableID == onlyTbl {
			mark = "→ "
		}
		fmt.Printf("      %s%s  %s\n", mark, t.TableID, t.Name)
	}
	fmt.Println()

	var results []tableResult

	fmt.Println("[2/2] 列出各表字段…")
	for _, t := range tables {
		if onlyTbl != "" && t.TableID != onlyTbl {
			continue
		}
		fields, err := client.ListBitableFields(ctx, appToken, t.TableID)
		if err != nil {
			fmt.Printf("      %s (%s) ✗ %v\n", t.Name, t.TableID, err)
			continue
		}
		fmt.Printf("      %s (%s): %d 个字段\n", t.Name, t.TableID, len(fields))
		results = append(results, tableResult{Table: t, Fields: fields})
	}

	// 原始 JSON
	raw, _ := json.MarshalIndent(map[string]any{
		"app_token": appToken,
		"tables":    results,
	}, "", "  ")
	rawPath := filepath.Join(outDir, "schema.json")
	if err := os.WriteFile(rawPath, raw, 0o644); err != nil {
		return err
	}

	// Markdown 报告
	md := renderMarkdown(appToken, results)
	mdPath := filepath.Join(outDir, "schema.md")
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		return err
	}

	if records > 0 {
		fmt.Printf("\n[3/3] 只读抽取 %d 条记录（确认字段取值形态）…\n", records)
		for _, r := range results {
			recs, err := client.ListBitableRecords(ctx, appToken, r.Table.TableID, records)
			if err != nil {
				fmt.Printf("      %s ✗ %v\n", r.Table.Name, err)
				continue
			}
			fmt.Printf("      %s: %d 条\n", r.Table.Name, len(recs))
			for i, rec := range recs {
				fmt.Printf("        [%d] record_id=%s 字段数=%d\n", i+1, rec.RecordID, len(rec.Fields))
				for _, f := range r.Fields {
					raw, ok := rec.Fields[f.FieldName]
					if !ok || len(raw) == 0 || string(raw) == "null" {
						continue
					}
					// 只对 URL/附件/文本 类字段展示形态摘要（截断，避免刷屏）
					if f.Type == 15 || f.Type == 17 || f.Type == 1 || f.Type == 19 {
						s := string(raw)
						if len([]rune(s)) > 120 {
							s = string([]rune(s)[:120]) + "…"
						}
						fmt.Printf("            %-22s (type %2d): %s\n", f.FieldName, f.Type, s)
					}
				}
				fmt.Println()
			}
		}
	}

	fmt.Printf("\n✓ 报告: %s\n  原始: %s\n", mdPath, rawPath)
	fmt.Println()
	fmt.Println("提示：把这份报告里的字段名/类型，与 `go run ./cmd/bitable-schema` 的")
	fmt.Println("      建议结构对照，即可决定测试表要不要沿用现有命名。")
	return nil
}

type tableResult struct {
	Table  feishu.BitableTable
	Fields []feishu.BitableField
}

func renderMarkdown(appToken string, results []tableResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 多维表格真实结构（只读读取）\n\n")
	fmt.Fprintf(&b, "> 生成时间：%s ｜ 工具：`cmd/bitable-read`（**只读**，不写任何数据）\n\n",
		time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- `app_token`: `%s`\n", appToken)
	fmt.Fprintf(&b, "- 数据表数：%d\n\n", len(results))

	for i, r := range results {
		fmt.Fprintf(&b, "## %d. %s\n\n", i+1, r.Table.Name)
		fmt.Fprintf(&b, "- `table_id`: `%s`\n", r.Table.TableID)
		fmt.Fprintf(&b, "- 字段数：%d\n\n", len(r.Fields))
		fmt.Fprintf(&b, "| # | 字段名 | field_id | type | ui_type | 主字段 | 选项/属性 |\n")
		fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")
		for j, f := range r.Fields {
			opts := summarizeProperty(f.Property)
			primary := ""
			if f.IsPrimary {
				primary = "★"
			}
			fmt.Fprintf(&b, "| %d | **%s** | `%s` | `%d` | %s | %s | %s |\n",
				j+1, f.FieldName, f.FieldID, f.Type, f.UIType, primary, opts)
		}
		b.WriteString("\n")
	}

	b.WriteString("## 字段类型对照（官方枚举）\n\n")
	for _, kv := range []struct {
		t    int
		name string
	}{
		{1, "文本"}, {2, "数字"}, {3, "单选"}, {4, "多选"}, {5, "日期"}, {7, "复选框"},
		{11, "人员"}, {13, "电话"}, {15, "超链接"}, {17, "附件"}, {18, "单向关联"},
		{19, "查找引用(只读)"}, {20, "公式(只读)"}, {21, "双向关联"}, {22, "地理位置"},
		{23, "群组"}, {1001, "创建时间(只读)"}, {1002, "最后更新时间(只读)"},
		{1003, "创建人(只读)"}, {1004, "修改人(只读)"}, {1005, "自动编号(只读)"},
	} {
		fmt.Fprintf(&b, "- `%d` %s\n", kv.t, kv.name)
	}
	b.WriteString("\n> **只读字段绝不能作为写入目标**：19 / 20 / 1001-1005。\n")

	return b.String()
}

// summarizeProperty 把 property 压成一行摘要，重点是单选/多选的选项列表。
func summarizeProperty(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "—"
	}
	var p struct {
		Options []struct {
			Name string `json:"name"`
		} `json:"options"`
		Formatter     string `json:"formatter"`
		CurrencyCode  string `json:"currency_code"`
		DateFormatter string `json:"date_formatter"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "—"
	}
	var parts []string
	if len(p.Options) > 0 {
		names := make([]string, 0, len(p.Options))
		for _, o := range p.Options {
			names = append(names, o.Name)
		}
		sort.Strings(names)
		s := strings.Join(names, " / ")
		if len([]rune(s)) > 200 {
			s = string([]rune(s)[:200]) + "…"
		}
		parts = append(parts, "选项: "+s)
	}
	if p.Formatter != "" {
		parts = append(parts, "格式 "+p.Formatter)
	}
	if p.CurrencyCode != "" {
		parts = append(parts, "货币 "+p.CurrencyCode)
	}
	if p.DateFormatter != "" {
		parts = append(parts, "日期格式 "+p.DateFormatter)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "；")
}
