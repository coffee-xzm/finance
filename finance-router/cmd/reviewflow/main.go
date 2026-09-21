// Command reviewflow 手动触发发票侧状态机的两个动作（docs/30-review/33 §4）：
//
//	-auto-approve <实例code>  全部行一致 → 自动同意该审批
//	-finalize     <实例code>  审批已通过 → 所有行写「通过」并归档 + 回写流水进度
//	-remind       <实例code>  重发「请补齐发票并提交」的提醒给提交人
//
// 常驻服务在事件到达时自动跑同一份代码；这个命令用于手动补跑 / 演练。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/pipeline"
)

func main() {
	cfgPath := flag.String("config", "", "config.yml 路径")
	auto := flag.String("auto-approve", "", "实例 code：全部行一致则自动同意审批")
	finalize := flag.String("finalize", "", "实例 code：写通过 + 归档 + 回写流水进度")
	markCollected := flag.String("mark-collected", "", "发票实例 code：把它对应的流水行进度写「已通过」（P5 验证用）")
	remind := flag.String("remind", "", "发票实例 code：把「请补齐并提交」的提醒重发给提交人（权限修好后补发 / 对方说没收到时重发）")
	setProgress := flag.String("set-progress", "", "运维：改某条流水行进度，格式 <recordID>=<值>")
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
	cfg, err := config.Load(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_ = feishu.NewClient // 保持依赖显式

	switch {
	case *auto != "":
		if err := pipeline.AutoApproveIfClean(ctx, cfg, *auto); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
	case *finalize != "":
		if err := pipeline.FinalizeApproved(ctx, cfg, *finalize); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
	case *markCollected != "":
		if err := pipeline.MarkInvoiceCollected(ctx, cfg, *markCollected); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
	case *remind != "":
		if err := pipeline.RemindInvoiceDraft(ctx, cfg, *remind); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ 提醒已发送")
	case *setProgress != "":
		parts := strings.SplitN(*setProgress, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintln(os.Stderr, "✗ -set-progress 格式应为 <recordID>=<值>")
			os.Exit(1)
		}
		if err := pipeline.SetLedgerProgress(ctx, cfg, parts[0], parts[1]); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✓ 流水行 %s 进度已写为 %s\n", parts[0], parts[1])
	default:
		fmt.Println("用 -auto-approve / -finalize / -mark-collected <实例code>，或 -set-progress <recordID>=<值>")
	}
}
