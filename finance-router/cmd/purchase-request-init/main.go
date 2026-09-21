// Command purchase-request-init 把 wiki「27采购申请表」的字段建齐/对齐到
// 参考表 `tbllUFPS…`（见 internal/bitable/PurchaseRequestTable）。
//
// 背景：采购审批通过后要把信息写进这张表，但它一开始只有 1 个「文本」字段，
// 所以先把 20 个字段补齐。默认 **dry-run**，要真写需显式 -dry-run=false。
//
// 用法：
//
//	go run ./cmd/purchase-request-init                 # 只打印计划
//	go run ./cmd/purchase-request-init -dry-run=false  # 真建字段
//	go run ./cmd/purchase-request-init -dry-run=false -align   # 顺带对齐已存在字段的类型/选项
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/bitable"
	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

func main() {
	cfgPath := flag.String("config", "", "config.yml 路径")
	appToken := flag.String("app-token", "", "默认取 bases.purchase_request.app_token")
	tableID := flag.String("table", "", "默认取 bases.purchase_request.tables.request")
	dryRun := flag.Bool("dry-run", true, "只打印计划；要真写请 -dry-run=false")
	align := flag.Bool("align", false, "对已存在字段也做类型/选项对齐")
	sleepMS := flag.Int("sleep", 1500, "每次写字段之间的间隔（毫秒）——飞书字段接口连发会 403")
	retry := flag.Int("retry", 4, "单个字段被拒时的最大尝试次数（指数退避）")
	flag.Parse()

	p := *cfgPath
	if p == "" {
		f, err := config.FindConfigFile()
		if err != nil {
			die(err)
		}
		p = f
	}
	cfg, err := config.Load(p)
	if err != nil {
		die(err)
	}
	if *appToken == "" {
		if b, ok := cfg.Base(config.BasePurchaseRequest); ok {
			*appToken = b.AppToken
		}
	}
	if *tableID == "" {
		*tableID = cfg.Table(config.BasePurchaseRequest, "request")
	}
	if *appToken == "" || *tableID == "" {
		die(fmt.Errorf("缺少 app_token / table_id（配 bases.purchase_request 或用 -app-token/-table）"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	target := bitable.PurchaseRequestTable()
	existing, err := client.ListBitableFields(ctx, *appToken, *tableID)
	if err != nil {
		die(fmt.Errorf("读现有字段失败: %w", err))
	}
	fmt.Printf("app_token=%s table=%s\n现有 %d 个字段，目标结构 %d 个字段\n\n",
		*appToken, *tableID, len(existing), len(target.Fields))

	byName := map[string]feishu.BitableField{}
	for _, f := range existing {
		byName[f.FieldName] = f
	}

	// 先处理主字段：目标表已有的主字段（如「文本」）不在目标字段集合里时，
	// 把它重命名成目标的第一个字段（项目名称），而不是新加一个。
	if len(existing) > 0 {
		primary := existing[0]
		if _, isTarget := byName[target.Fields[0].Name]; !isTarget {
			fmt.Printf("主字段  %s → %s（重命名，保留数据）\n", primary.FieldName, target.Fields[0].Name)
			if !*dryRun {
				if err := client.UpdateBitableField(ctx, *appToken, *tableID, primary.FieldID,
					feishu.FieldSpec{FieldName: target.Fields[0].Name, Type: primary.Type}); err != nil {
					die(fmt.Errorf("重命名主字段失败: %w", err))
				}
			}
			byName[target.Fields[0].Name] = primary
		}
	}

	failed := 0
	for _, tf := range target.Fields {
		if ex, ok := byName[tf.Name]; ok {
			if *align {
				fmt.Printf("对齐    %s（type %d → %d）\n", tf.Name, ex.Type, int(tf.Type))
				if !*dryRun {
					if err := retryWrite(*retry, *sleepMS, func() error {
						return client.UpdateBitableField(ctx, *appToken, *tableID, ex.FieldID, toSpec(tf))
					}); err != nil {
						failed++
						fmt.Printf("  ✗ 对齐失败: %v\n", err)
					}
				}
			} else {
				fmt.Printf("已存在  %s（跳过）\n", tf.Name)
			}
			continue
		}
		fmt.Printf("新建    %s  type=%s%s\n", tf.Name, typeName(tf.Type), optsHint(tf))
		if *dryRun {
			continue
		}
		if err := retryWrite(*retry, *sleepMS, func() error {
			_, e := client.CreateBitableField(ctx, *appToken, *tableID, toSpec(tf))
			return e
		}); err != nil {
			failed++
			fmt.Printf("  ✗ 新建失败: %v\n", err)
			continue
		}
		time.Sleep(time.Duration(*sleepMS) * time.Millisecond)
	}

	if *dryRun {
		fmt.Println("\n（dry-run：没有写入。要真写：-dry-run=false）")
		return
	}
	if failed > 0 {
		fmt.Printf("\n✗ 完成，但有 %d 个字段失败（多为频率限制，稍后重跑本命令会继续补）\n", failed)
		os.Exit(1)
	}
	fmt.Println("\n✓ 完成")
}

// retryWrite 对写操作做有限重试：飞书字段接口在短时间内连续调用会返回 403/91403（频率限制）。
func retryWrite(max int, sleepMS int, fn func() error) error {
	var err error
	for i := 0; i < max; i++ {
		if err = fn(); err == nil {
			return nil
		}
		// 1254606 DataNotChange = 字段本来就是这个样子，属正常（不是失败）
		if strings.Contains(err.Error(), "DataNotChange") || strings.Contains(err.Error(), "1254606") {
			return nil
		}
		if !(strings.Contains(err.Error(), "91403") || strings.Contains(err.Error(), "Forbidden") ||
			strings.Contains(err.Error(), "1254291")) {
			return err
		}
		d := time.Duration(sleepMS*(i+1)) * time.Millisecond
		fmt.Printf("  … 被限流/拒绝，等待 %v 后重试（%d/%d）\n", d, i+1, max)
		time.Sleep(d)
	}
	return err
}

func toSpec(f bitable.Field) feishu.FieldSpec {
	spec := feishu.FieldSpec{FieldName: f.Name, Type: int(f.Type)}
	var prop map[string]any
	if len(f.Options) > 0 {
		opts := make([]map[string]any, 0, len(f.Options))
		for _, o := range f.Options {
			opts = append(opts, map[string]any{"name": o})
		}
		prop = map[string]any{"options": opts}
	}
	if f.Formatter != "" || f.DateFmt != "" {
		if prop == nil {
			prop = map[string]any{}
		}
		if f.Formatter != "" {
			prop["formatter"] = f.Formatter
		}
		if f.DateFmt != "" {
			prop["date_formatter"] = f.DateFmt
		}
	}
	spec.Property = prop
	return spec
}

func optsHint(f bitable.Field) string {
	if len(f.Options) == 0 {
		return ""
	}
	return fmt.Sprintf("  选项=%v", f.Options)
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
	case bitable.TypeURL:
		return "超链接"
	case bitable.TypeAttachment:
		return "附件"
	}
	return fmt.Sprintf("type%d", int(t))
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "✗ %v\n", err)
	os.Exit(1)
}
