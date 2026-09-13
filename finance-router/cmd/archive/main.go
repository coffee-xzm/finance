// Command archive 手动把源表里【人工审核=通过 且 未归档】的行复制到整合表。
//
// 核心逻辑在 internal/pipeline，与常驻服务 cmd/serve 共用。
// 注意：开 review.auto_pass_clean 时，核对结果=一致的行会被**自动通过**，
// 服务会顺带归档它们；本命令用于手动补跑归档。
//
// 用法：
//
//	go run ./cmd/archive -dry-run    # 只列出待归档
//	go run ./cmd/archive             # 真归档
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/coffee/finance-router/internal/pipeline"
)

func main() {
	var o pipeline.ArchiveOptions
	flag.StringVar(&o.CfgPath, "config", "", "config.yml 路径")
	flag.BoolVar(&o.DryRun, "dry-run", false, "只列出，不写入")
	flag.BoolVar(&o.Repair, "repair", false, "补历史：给整合表里缺附件的行补传图片")
	flag.Parse()

	if err := pipeline.RunArchive(o); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}
