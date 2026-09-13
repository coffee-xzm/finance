// Command db 管理本地 SQLite：初始化迁移、查看统计、自检。
//
// 这是"防重复报销"的落地处 —— 证据表的 sha256 唯一索引是数据库级约束，
// 不依赖任何上层检查或时序假设（飞书多维表格没有这个能力）。
//
// 用法：
//
//	go run ./cmd/db -init           # 建库 + 应用迁移
//	go run ./cmd/db -stats          # 看统计
//	go run ./cmd/db -dup <sha256>   # 查某张图是否已被占用
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/store"
)

func main() {
	var (
		cfgPath = flag.String("config", "", "config.yml 路径")
		init    = flag.Bool("init", false, "建库并应用迁移")
		stats   = flag.Bool("stats", false, "输出统计")
		dupOf   = flag.String("dup", "", "查询该 sha256 是否已被占用")
		backup  = flag.Bool("backup", false, "生成一致性快照到 paths.backup_dir（VACUUM INTO + 轮转）")
		listBak = flag.Bool("backup-list", false, "列出已有备份")
		reindex = flag.Bool("reindex", false,
			"从 data/extract/files 重建唯一性状态（库被删/重建后恢复保护）")
		dups = flag.Bool("dups", false, "列出重复报销嫌疑（同一张图出现在多个实例）")
	)
	flag.Parse()

	if *cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		*cfgPath = p
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	switch {
	case *init:
		fmt.Printf("✓ 已初始化 %s（迁移已应用）\n", cfg.Paths.DB)
	case *dupOf != "":
		inst, found, err := db.DuplicateOf(ctx, *dupOf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		if found {
			fmt.Printf("已被占用：首次来自实例 %s\n", inst)
		} else {
			fmt.Println("未被占用")
		}
	case *backup:
		path, err := db.Backup(ctx, cfg.Paths.BackupDir, cfg.Paths.BackupKeep)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ 备份失败: %v\n", err)
			os.Exit(1)
		}
		fi, _ := os.Stat(path)
		fmt.Printf("✓ 备份完成: %s（%.1f KB）\n", path, float64(fi.Size())/1024)
		fmt.Println("  ⚠ 本地库是「唯一性」的权威 —— 请把备份同步到异地（网盘 / 另一台机器）")
	case *listBak:
		names, err := db.ListBackups(cfg.Paths.BackupDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		if len(names) == 0 {
			fmt.Println("（还没有备份）")
			return
		}
		fmt.Printf("共 %d 份备份（新的在前）：\n", len(names))
		for _, n := range names {
			fmt.Println("  ", n)
		}
	case *reindex:
		n, groups, degraded, err := db.ReindexFromFiles(ctx, "data/extract/files")
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ 重建失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✓ 从本地图补入 %d 条证据（恢复唯一性）\n", n)
		printDegraded(degraded)
		printDupGroups(groups)
		if len(groups) > 0 {
			os.Exit(3) // 有重复 → 非零退出，便于流水线拦截
		}
	case *dups:
		groups, degraded, err := store.ScanDuplicateGroups("data/extract/files")
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		printDegraded(degraded)
		if len(groups) == 0 {
			fmt.Println("未发现重复报销嫌疑")
			return
		}
		printDupGroups(groups)
		os.Exit(3)
	case *stats:
		s, err := db.Stats(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("数据库: %s\n", cfg.Paths.DB)
		fmt.Printf("  审批单 submission : %d\n", s.Submissions)
		fmt.Printf("  证据   evidence   : %d（唯一 sha256: %d）\n", s.Evidence, s.DistinctSHA)
		fmt.Printf("  审计   audit_log  : %d\n", s.AuditRows)
		fmt.Printf("  部门字典 dept_dict: %d\n", s.DeptRows)
		if s.Evidence != s.DistinctSHA {
			fmt.Println("  ⚠ 证据数与唯一 sha256 数不一致 —— 唯一索引可能被破坏")
		}
	default:
		flag.Usage()
	}
}

// printDegraded 提示"没有 provenance 旁路表"的文件数。
// 这些文件的 sha 是对落盘文件求的，而非原始下载字节 —— 对 PDF 转出的 PNG 而言是错的，
// 会导致重建的唯一性保护失效，必须显式告警而不是静默通过。
func printDegraded(n int) {
	if n == 0 {
		return
	}
	fmt.Printf("\n⚠ %d 个文件缺少 provenance 旁路表（_provenance.tsv），\n"+
		"  只能对落盘文件求 sha —— 若原附件是 PDF，这个 sha 与库里的**不是同一个**，\n"+
		"  由此重建的唯一性保护对该文件无效。请重跑 extract 生成旁路表。\n", n)
}

// printDupGroups 打印重复报销嫌疑分组。dup 是"同一张图出现在多个实例"。
func printDupGroups(groups map[string][]string) {
	if len(groups) == 0 {
		return
	}
	// 稳定输出顺序（map 遍历是随机的）
	sums := make([]string, 0, len(groups))
	for s := range groups {
		sums = append(sums, s)
	}
	sort.Strings(sums)

	fmt.Printf("\n⚠ 发现 %d 组重复报销嫌疑（同一张图出现在多个实例）：\n", len(groups))
	for _, sum := range sums {
		insts := groups[sum]
		short := sum
		if len(short) > 16 {
			short = short[:16]
		}
		fmt.Printf("  sha=%s… 出现在 %d 个实例:\n", short, len(insts))
		for _, in := range insts {
			fmt.Printf("      %s\n", in)
		}
	}
	fmt.Println("  → 这些是同一张凭证被多次提交，请人工确认是否重复报销。")
}
