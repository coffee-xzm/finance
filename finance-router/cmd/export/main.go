// Command export 按「勾选出来的清单」把多维表格里的附件导出成 zip。
//
// 这是 docs/30-review/32 计划里的 **P1（清单口径 MVP）**。
//
// 用法：
//
//	# ① 主路径：清单文件（一行一个 审批实例号 或 record_id，支持 csv 带表头）
//	go run ./cmd/export -table integrated -records-file list.csv -out data/export/2026-08
//
//	# ② 少量记录直接写在命令行
//	go run ./cmd/export -table integrated -records recA,recB -out data/export/tmp
//
//	# ③ 备选：按视图全量（与清单互斥）
//	go run ./cmd/export -table integrated -view <view_id> -out data/export/2026-08
//
//	# ④ 永远是先干跑：只打印"会导哪些行、命名成什么、哪些行有问题"
//	go run ./cmd/export -table integrated -records-file list.csv -out /tmp/x -dry-run
//
//	# ⑤ 查历史批次（审计底账，不需要联网）
//	go run ./cmd/export -batches
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/export"
	"github.com/coffee/finance-router/internal/store"
)

func main() {
	var (
		cfgPath      = flag.String("config", "", "config.yml 路径")
		table        = flag.String("table", "integrated", "表：integrated|submission|报销整合|报销核对|tblXXXX")
		recordsFile  = flag.String("records-file", "", "清单文件（csv/txt，一行一个标识）")
		records      = flag.String("records", "", "清单直接写在命令行（逗号分隔）")
		view         = flag.String("view", "", "视图 id（与清单口径互斥）")
		fields       = flag.String("fields", "发票,订单截图,付款记录", "要导出的附件槽位（逗号分隔）")
		namingTmpl   = flag.String("naming", "", "命名模板，默认取配置")
		groupBy      = flag.String("group-by", "", "分组：department|month|none")
		out          = flag.String("out", "", "输出目录（zip 与 manifest.csv 落在这里）")
		nas          = flag.String("nas", "", "NAS 挂载点（留空 = 不复制）")
		dryRun       = flag.Bool("dry-run", false, "只规划不下载")
		force        = flag.Bool("force", false, "覆盖已存在的同名文件（默认跳过）")
		extra        = flag.String("extra", "", "高级权限表的 extra JSON（见 cmd/download-probe）")
		operator     = flag.String("operator", "", "操作人（写进导出审计）")
		allowMissing = flag.Bool("allow-missing", false, "清单里有对不上的标识时继续（默认报错退出）")
		maxFiles     = flag.Int("max", 0, "单批文件数上限（0 = 取配置）")
		listBatches  = flag.Bool("batches", false, "只列最近导出批次（不联网）")
		batchLimit   = flag.Int("batches-limit", 20, "配合 -batches：列出条数")
		timeout      = flag.Duration("timeout", 30*time.Minute, "整体超时")
	)
	flag.Parse()

	if *listBatches {
		if err := printBatches(*cfgPath, *batchLimit); err != nil {
			fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	opts := export.Options{
		CfgPath:      *cfgPath,
		Table:        *table,
		RecordsFile:  *recordsFile,
		Records:      splitCSV(*records),
		ViewID:       *view,
		Slots:        splitCSV(*fields),
		Naming:       *namingTmpl,
		GroupBy:      *groupBy,
		OutDir:       *out,
		NASRoot:      *nas,
		DryRun:       *dryRun,
		Force:        *force,
		Extra:        *extra,
		Operator:     *operator,
		AllowMissing: *allowMissing,
		MaxFiles:     *maxFiles,
	}
	if err := export.Run(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// printBatches 打印最近的导出批次（不联网）。
func printBatches(cfgPath string, limit int) error {
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
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return err
	}
	defer db.Close()

	batches, err := db.ListExportBatches(context.Background(), limit)
	if err != nil {
		return err
	}
	if len(batches) == 0 {
		fmt.Println("（还没有任何导出批次）")
		return nil
	}
	fmt.Printf("%-26s %-6s %-10s %6s %5s %5s %5s  %s\n",
		"批次", "口径", "表", "行数", "成功", "跳过", "失败", "操作人 / zip")
	for _, b := range batches {
		fmt.Printf("%-26s %-6s %-10s %6d %5d %5d %5d  %s\n",
			b.BatchID, b.Scope, b.TableName, b.Records, b.Success, b.Skipped, b.Failed,
			b.Operator+" "+b.ZipPath)
	}
	return nil
}
