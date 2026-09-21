// Command flowregister 手动触发「27-流水登记」这一步（docs/30-review/33 §12.15）：
//
//	-settle <登记实例code>   按登记数据覆盖流水行（+ 全部明细登记完则开票）
//
// 常驻服务在登记单「已通过」事件到达时自动跑同一份代码；这个命令用于手动补跑 /
// 演练 / 排错（例如事件在断网期间丢了）。
//
// 用法：
//
//	go run ./cmd/flowregister -settle <登记实例code> -dry-run   # 只看要覆盖什么
//	go run ./cmd/flowregister -settle <登记实例code>            # 真写
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/pipeline"
)

func main() {
	cfgPath := flag.String("config", "", "config.yml 路径")
	settle := flag.String("settle", "", "流水登记实例 code：覆盖流水行")
	dryRun := flag.Bool("dry-run", false, "只打印不写入")
	flag.Parse()

	p := *cfgPath
	if p == "" {
		f, err := config.FindConfigFile()
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		p = f
	}
	if *settle == "" {
		fmt.Fprintln(os.Stderr, "✗ 必须给 -settle <流水登记实例code>")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := pipeline.RunFlowRegister(ctx, pipeline.FlowRegisterOptions{
		CfgPath: p, Instance: *settle, DryRun: *dryRun,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}
