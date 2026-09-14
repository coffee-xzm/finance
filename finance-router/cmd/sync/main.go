// Command sync 把本地库里的抽取结果写进飞书多维表格「报销核对」表。
//
// 一张发票一行；幂等键 = 审批实例号 + 发票号码。
//
// 用法：
//
//	go run ./cmd/sync                 # 写全部
//	go run ./cmd/sync -dry-run        # 只打印
//	go run ./cmd/sync -update         # 已存在的行就地更新（保留人工字段）
//	go run ./cmd/sync -instance <code>  # 只处理一个实例
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/coffee/finance-router/internal/pipeline"
)

func main() {
	var o pipeline.SyncOptions
	flag.StringVar(&o.CfgPath, "config", "", "config.yml 路径")
	flag.StringVar(&o.Only, "instance", "", "只处理这一个审批实例")
	flag.IntVar(&o.Limit, "limit", 0, "只处理前 N 个实例")
	flag.BoolVar(&o.DryRun, "dry-run", false, "只打印，不写入飞书")
	flag.BoolVar(&o.Update, "update", false, "已存在的行就地更新（保留人工字段）")
	flag.BoolVar(&o.Recheck, "recheck", false,
		"不读本地库，直接按 review 策略修正已有行（待审+一致 → 通过）")
	flag.Parse()

	if err := pipeline.RunSync(o); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}
