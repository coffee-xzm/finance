// Command bitable-schema 输出测试用多维表格的表结构清单（Markdown / CSV）。
//
// 默认**只打印**，不调用任何飞书接口 —— 你可以照着它在 UI 里手建。
// 加 -create 且配置了 bitable.app_token 时，才会调用 API 建表建字段。
//
// 用法：
//
//	go run ./cmd/bitable-schema                  # 打印 Markdown 清单
//	go run ./cmd/bitable-schema -format csv      # 打印 CSV
//	go run ./cmd/bitable-schema -create          # 真的建表（需要 app_token + 写权限）
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/coffee/finance-router/internal/bitable"
)

func main() {
	var (
		format  = flag.String("format", "markdown", "输出格式：markdown | csv")
		create  = flag.Bool("create", false, "真的调用飞书 API 建表建字段（默认只打印）")
		minimal = flag.Bool("minimal", false, "只输出【验证用最小表】，而不是完整 4 张表")
	)
	flag.Parse()

	if *create {
		fmt.Fprintln(os.Stderr, "✗ -create 尚未实现 —— 请先用打印出来的清单在 UI 里手工建表。")
		fmt.Fprintln(os.Stderr, "  原因：建表需要 bitable:app 写权限，且要先确认 app_token 与文件夹协作者已配好；")
		fmt.Fprintln(os.Stderr, "  在链路跑通前不引入写操作，符合并行只读期的纪律。")
		os.Exit(1)
	}

	switch *format {
	case "markdown":
		if *minimal {
			printMinimal()
		} else {
			printMarkdown()
		}
	case "csv":
		printCSV()
	default:
		fmt.Fprintf(os.Stderr, "未知格式 %q（支持 markdown | csv）\n", *format)
		os.Exit(2)
	}
}

func printMarkdown() {
	fmt.Println("# 测试多维表格 · 表结构清单")
	fmt.Println()
	fmt.Println("> 目标：**个人文件夹**下新建一个多维表格，按下面的表建字段。")
	fmt.Println("> 建完把文档 URL 里的 `app_token` 与各表 `table_id` 填进 `config.yml`。")
	fmt.Println()

	for i, t := range []bitable.Table{bitable.ReviewTable(), bitable.IntegratedTable()} {
		fmt.Printf("## %d. `%s` —— %s\n\n", i+1, t.Key, t.Name)
		fmt.Printf("- 数据权属：**%s**\n", t.Authority)
		if t.Description != "" {
			fmt.Printf("- 说明：%s\n", t.Description)
		}
		fmt.Println()
		fmt.Println("| # | 字段名 | 类型 | 类型码 | 取值/格式 | 说明 |")
		fmt.Println("|---|---|---|---|---|---|")
		for j, f := range t.Fields {
			extra := fieldExtra(f)
			note := f.Note
			note = strings.ReplaceAll(note, "|", "\\|")
			fmt.Printf("| %d | **%s** | %s | `%d` | %s | %s |\n",
				j+1, f.Name, typeName(f.Type), int(f.Type), extra, note)
		}
		fmt.Println()
	}

	fmt.Println("---")
	fmt.Println()
	fmt.Println("## 填写 `config.yml`")
	fmt.Println()
	fmt.Println("```yaml")
	fmt.Println("feishu:")
	fmt.Println("  bitable:")
	fmt.Println("    app_token: \"<多维表格 URL 里 /base/ 后面那一段>\"")
	fmt.Println("    tables:")
	fmt.Println("      submission: \"tblXXXX\"        # 票据收集台账")
	fmt.Println("      evidence: \"tblXXXX\"          # 识别证据")
	fmt.Println("      invoice_dedup: \"tblXXXX\"     # 发票唯一性")
	fmt.Println("      approval_result: \"tblXXXX\"   # 审批结果")
	fmt.Println("```")
	fmt.Println()
	fmt.Println("`table_id` 的取法：打开表 → 看 URL 的 `?table=tblXXXX`；")
	fmt.Println("或调用 `GET /bitable/v1/apps/:app_token/tables` 列出全部表。")
	fmt.Println()
	fmt.Println("## 三条硬约束（违反会导致 API 报错或数据错乱）")
	fmt.Println()
	fmt.Println("1. **只读字段绝不能作为写入目标**：公式(20)、查找引用(19)、自动编号(1005)、")
	fmt.Println("   创建时间(1001)、最后更新时间(1002)、创建人(1003)、修改人(1004)。")
	fmt.Println("   → 所有计算结果由本地服务写进**普通字段**。")
	fmt.Println("2. **金额一律以「分」存整数**（`金额_分` 字段），禁用小数，避免浮点误差。")
	fmt.Println("3. **同一数据表禁止并发写**（`1254291`）→ 服务侧按 `table_id` 串行写入。")
	fmt.Println()
	fmt.Println("## 别忘了")
	fmt.Println()
	fmt.Println("- 把**应用加为该文档的协作者**，否则 API 写入返回 403。")
	fmt.Println("- **测试期先别开高级权限**，把权限这个变量隔离掉。")
	fmt.Println("- 仪表盘**无法用 API 创建** → 要看板就手工建一次，之后用 `copy` 克隆。")
	fmt.Println("- 单表行数免费版上限约 **2,000 行**；流水类高频数据按年/月分表。")
}

func fieldExtra(f bitable.Field) string {
	var parts []string
	if len(f.Options) > 0 {
		parts = append(parts, strings.Join(f.Options, " / "))
	}
	if f.Formatter != "" {
		parts = append(parts, "格式 "+f.Formatter)
	}
	if f.DateFmt != "" {
		parts = append(parts, "格式 "+f.DateFmt)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "；")
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
	case bitable.TypeUser:
		return "人员"
	case bitable.TypeAttachment:
		return "附件"
	default:
		return "?"
	}
}

// printMinimal 只输出验证用最小表。
func printMinimal() {
	printOneTable(bitable.ReviewTable(), false)
	fmt.Println()
	printOneTable(bitable.IntegratedTable(), true)
}

func printOneTable(t bitable.Table, compact bool) {
	if compact {
		fmt.Printf("## 表 2 · 整合表：`%s`\n\n", t.Name)
	} else {
		fmt.Printf("# 表 1 · 源表（机器写、人审）：`%s`\n\n", t.Name)
	}
	fmt.Printf("> %s\n\n", t.Description)
	fmt.Println("| # | 字段名 | 类型 | 类型码 | 取值 | 说明 |")
	fmt.Println("|---|---|---|---|---|---|")
	for i, f := range t.Fields {
		fmt.Printf("| %d | **%s** | %s | `%d` | %s | %s |\n",
			i+1, f.Name, typeName(f.Type), int(f.Type), fieldExtra(f), f.Note)
	}
	fmt.Printf("\n合计 **%d 个字段**。\n", len(t.Fields))
	if !compact {
		fmt.Println()
		fmt.Println("### 每一列验证什么")
		fmt.Println()
		fmt.Println("| 列 | 验证的事 |")
		fmt.Println("|---|---|")
		fmt.Println("| 审批实例号 / 槽位 | 三个附件槽是否都能取到，且能对上审批 |")
		fmt.Println("| 媒体类型 / 文件名 | PDF 与 JPEG 分流是否正确 |")
		fmt.Println("| **价税合计_分 / 税额_分 / 不含税金额_分** | **★ 是否漏读税额** |")
		fmt.Println("| **大写校验** | ★ 主校验，抓数字读错（实测抓过 10 倍错） |")
		fmt.Println("| 图片SHA256 | ★ 去重主键 |")
		fmt.Println("| **人工审核** | ★ 人在这里改；只有「通过」才进整合表 |")
		fmt.Println("| 已归档 | 幂等标记，防重复归档 |")
	}
}

func printCSV() {
	fmt.Println("table_key,table_name,field_name,type,type_code,options,note")
	for _, t := range []bitable.Table{bitable.ReviewTable(), bitable.IntegratedTable()} {
		for _, f := range t.Fields {
			fmt.Printf("%s,%s,%s,%s,%d,%s,%s\n",
				t.Key, t.Name, f.Name, typeName(f.Type), int(f.Type),
				strings.Join(f.Options, "|"), strings.ReplaceAll(f.Note, ",", "，"))
		}
	}
}
