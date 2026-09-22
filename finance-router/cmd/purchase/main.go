// Command purchase 手动跑一次"采购审批通过后"的链路（写流水 + 代建发票单 + 退回）。
//
// 常驻服务收到采购 APPROVED 事件时会自动跑同一份代码（internal/pipeline.RunPurchase）；
// 这个命令用于手动补跑 / 演练。
//
// 用法：
//
//	go run ./cmd/purchase -instance <采购实例code> -dry-run   # 只看要写什么
//	go run ./cmd/purchase -instance <采购实例code>            # 真写
//	go run ./cmd/purchase -instance <采购实例code> -force     # 忽略本地已完成标记
//	go run ./cmd/purchase -instance <采购实例code> -resync-ledger   # 补流水行的空单元格（如「关联人」）
//	go run ./cmd/purchase -instance <采购实例code> -resync-request  # 补采购申请表的空单元格
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/pipeline"
	"github.com/coffee/finance-router/internal/store"
)

func main() {
	cfgPath := flag.String("config", "", "config.yml 路径")
	instance := flag.String("instance", "", "采购审批实例 code")
	dryRun := flag.Bool("dry-run", false, "只打印不写入")
	force := flag.Bool("force", false, "忽略本地已完成标记，重跑")
	show := flag.Bool("show", false, "只打印本地 purchase_sync 记录后退出")
	onlyImages := flag.Bool("only-images", false, "只给已写过的采购申请表行补传商品图片（不改其它）")
	resync := flag.Bool("resync-request", false, "按「只填空」补正已写的采购申请表行（如补发起人部门）")
	resyncLedger := flag.Bool("resync-ledger", false,
		"按「只填空」补正已写的**流水行**（如线上新增的「关联人」列）")
	notice := flag.Bool("notice", false,
		"只打印 notify 模式会私信给流水登记人的那段文字（不写、不发）")
	flag.Parse()
	if *instance == "" {
		fmt.Fprintln(os.Stderr, "✗ 必须给 -instance <采购实例code>")
		os.Exit(1)
	}
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
	if *show {
		db, err := store.Open(cfg.Paths.DB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		defer db.Close()
		rec, ok, err := db.GetPurchase(context.Background(), *instance)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		if !ok {
			fmt.Println("（本地没有该采购的记录）")
			return
		}
		b, _ := json.MarshalIndent(rec, "", "  ")
		fmt.Println(string(b))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if *onlyImages {
		if err := pipeline.BackfillPurchaseImages(ctx, cfg, *instance); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		return
	}
	if *notice {
		text, err := pipeline.PreviewRegisterNotice(ctx, cfg, *instance)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		fmt.Println(text)
		return
	}
	if *resync {
		if err := pipeline.ResyncPurchaseRequest(ctx, cfg, *instance); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		return
	}
	if *resyncLedger {
		if err := pipeline.ResyncPurchaseLedger(ctx, cfg, *instance); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := pipeline.RunPurchase(ctx, pipeline.PurchaseOptions{
		CfgPath: p, Instance: *instance, DryRun: *dryRun, Force: *force,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}
