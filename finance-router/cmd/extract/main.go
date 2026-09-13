// Command extract 手动按实例跑抽取（下载 → PDF转PNG → 识别 → 本地入库）。
//
// 核心逻辑在 internal/pipeline，与常驻服务 cmd/serve 共用同一份代码。
//
// 用法：
//
//	go run ./cmd/extract -limit 2                  # 用 recon 已抓到的前 2 个实例
//	go run ./cmd/extract -instance <instance_code> # 指定实例
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/coffee/finance-router/internal/pipeline"
)

func main() {
	var o pipeline.Options
	flag.StringVar(&o.CfgPath, "config", "", "config.yml 路径")
	flag.StringVar(&o.FormsPath, "from-files", "data/recon/forms.jsonl", "recon 产出的表单文件")
	flag.StringVar(&o.Instance, "instance", "", "只处理指定 instance_code")
	flag.IntVar(&o.Limit, "limit", 2, "处理前 N 个实例（-instance 时忽略）")
	flag.StringVar(&o.OutDir, "out", "data/extract", "输出目录")
	flag.IntVar(&o.DPI, "dpi", 0, "PDF 光栅化 DPI（0 = 用 config.yml 的 pdf.dpi）")
	flag.BoolVar(&o.KeepPDF, "keep-pdf", false, "保留原始 PDF（默认只留转出的 PNG）")
	flag.Parse()

	if err := pipeline.Run(o); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}
