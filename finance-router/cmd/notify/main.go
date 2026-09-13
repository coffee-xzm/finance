// Command notify 扫描源表里需要人工处理的行，并把通知推给管理员。
//
// 核心逻辑在 internal/pipeline，与常驻服务 cmd/serve 共用。
//
// 用法：
//
//	go run ./cmd/notify            # 扫描并发送
//	go run ./cmd/notify -dry-run   # 只列出
//	go run ./cmd/notify -max 3     # 最多发 3 条，防刷屏
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/coffee/finance-router/internal/pipeline"
)

func main() {
	var o pipeline.NotifyOptions
	flag.StringVar(&o.CfgPath, "config", "", "config.yml 路径")
	flag.BoolVar(&o.DryRun, "dry-run", false, "只列出，不发送")
	flag.IntVar(&o.MaxSend, "max", 10, "单次最多发送条数（防刷屏）")
	flag.Parse()

	if err := pipeline.RunNotify(o); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}
