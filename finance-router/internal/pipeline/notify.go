// notify 扫描源表里需要人工处理的行，并把通知推给管理员。
//
// 为什么需要：没有通知时，低置信度的行只是"躺在表里等人发现"。
// 这一步才把"送达到人"补齐。
//
// 幂等：同一行在 `处理说明` 里已经带过通知标记就跳过，避免反复轰炸。
//
// 用法：
//
//	go run ./cmd/notify                 # 扫描并发送
//	go run ./cmd/notify -dry-run        # 只列出会通知哪些行
//	go run ./cmd/notify -max 3          # 最多发 3 条，防止误触发刷屏
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/notify"
)

const notifiedMark = "【已通知】"

func RunNotify(opts NotifyOptions) error {
	cfgPath, dryRun, maxSend := opts.CfgPath, opts.DryRun, opts.MaxSend
	if maxSend == 0 {
		maxSend = 10
	}
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
	base, _ := cfg.Base(config.BaseReview)
	appToken := base.AppToken
	tableID := cfg.Table(config.BaseReview, "review")
	if appToken == "" || tableID == "" {
		// 兼容旧配置
		appToken = cfg.Feishu.Bitable.AppToken
		tableID = cfg.Feishu.Bitable.Tables["submission"]
	}
	if appToken == "" || tableID == "" {
		return fmt.Errorf("配置缺少核对表 base/table")
	}

	recip := notify.Recipient{OpenID: cfg.Feishu.AdminOpenID, UserID: cfg.Feishu.AdminUserID}
	fmt.Printf("收件人: open_id=%v user_id=%v\n", mask(recip.OpenID), mask(recip.UserID))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	// 筛出需要人工的行：核对结果 = 存疑 / 缺件，且未被通知过。
	// 字段名与状态字面量都来自 config 字典（docs/30-review/33 §3）。
	verdict := cfg.Field(config.BaseReview, "verdict")
	filter := map[string]any{
		"conjunction": "or",
		"conditions": []map[string]any{
			{"field_name": verdict, "operator": "is", "value": []string{"存疑"}},
			{"field_name": verdict, "operator": "is", "value": []string{"缺件"}},
		},
	}
	recs, err := client.SearchBitableRecords(ctx, appToken, tableID, filter, 200)
	if err != nil {
		return fmt.Errorf("筛选待通知记录失败: %w", err)
	}

	explainField := cfg.Field(config.BaseReview, "explain")
	var targets []*feishu.BitableRecord
	skipped := 0
	for i := range recs {
		note := textOf(recs[i].Fields[explainField])
		if strings.Contains(note, notifiedMark) {
			skipped++
			continue
		}
		targets = append(targets, &recs[i])
	}
	fmt.Printf("需人工 %d 条（其中 %d 条已通知过，跳过）\n", len(recs), skipped)

	if len(targets) == 0 {
		fmt.Println("\n没有需要新通知的行。")
		return nil
	}
	if len(targets) > maxSend {
		fmt.Printf("按 -max=%d 截断\n", maxSend)
		targets = targets[:maxSend]
	}
	if dryRun {
		fmt.Println("（dry-run：不发送）")
	}

	sender := &notify.Sender{Client: client, Recipient: recip}
	var sent, failed int
	for _, r := range targets {
		amtYuan := floatOf(r.Fields[cfg.Field(config.BaseReview, "amount")])
		var amtCent *int64
		if amtYuan != 0 {
			c := int64(amtYuan*100 + 0.5)
			amtCent = &c
		}
		// 私信标题用「发票号码 + 销方名称」——「物资名称」那一列已经不在表里了
		//（新表单没有这个控件），用一张票的身份信息反而更好认。
		n := notify.NeedManual{
			InstanceCode: textOf(r.Fields[cfg.Field(config.BaseReview, "instance_no")]),
			Slot: joinNonEmpty(
				textOf(r.Fields[cfg.Field(config.BaseReview, "seller")]),
				textOf(r.Fields[cfg.Field(config.BaseReview, "invoice_no")])),
			Reason:     textOf(r.Fields[explainField]),
			Verdict:    textOf(r.Fields[verdict]),
			AmountCent: amtCent,
		}
		if dryRun {
			fmt.Printf("  → %s / %s  原因=%s\n", short(n.InstanceCode), n.Slot, trunc(n.Reason, 40))
			sent++
			continue
		}
		used, err := sender.Send(ctx, n)
		if err != nil {
			failed++
			fmt.Printf("  ✗ %s / %s 发送失败(%s): %v\n", short(n.InstanceCode), n.Slot, used, err)
			continue
		}
		// 打上"已通知"标记，避免下次重复通知
		newNote := n.Reason
		if newNote != "" {
			newNote += " "
		}
		newNote += notifiedMark
		if err := client.UpdateBitableRecord(ctx, appToken, tableID, r.RecordID,
			map[string]any{explainField: newNote}); err != nil {
			fmt.Printf("  ⚠ %s 已通知但标记失败（下次会重复通知）: %v\n", short(n.InstanceCode), err)
		}
		sent++
		fmt.Printf("  ✓ %s / %s 已通过 %s 通知\n", short(n.InstanceCode), n.Slot, used)
	}
	fmt.Printf("\n完成：通知 %d，失败 %d\n", sent, failed)
	return nil
}

// textOf 把飞书返回的字段值转成字符串（文本是富文本片段数组）。
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var segs []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &segs) == nil && len(segs) > 0 {
		s := ""
		for _, x := range segs {
			s += x.Text
		}
		return s
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func int64Of(raw json.RawMessage) *int64 {
	if len(raw) == 0 {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		v := int64(f)
		return &v
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		var v int64
		if _, err := fmt.Sscan(s, &v); err == nil {
			return &v
		}
	}
	return nil
}

func floatOf(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		var v float64
		fmt.Sscan(s, &v)
		return v
	}
	return 0
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func mask(s string) string {
	if s == "" {
		return "(未配置)"
	}
	if len(s) <= 10 {
		return s
	}
	return s[:6] + "…" + s[len(s)-4:]
}

// NotifyOptions 配置一次"通知"运行。
type NotifyOptions struct {
	CfgPath string
	DryRun  bool
	MaxSend int
}

// joinNonEmpty 用空格把非空片段连起来（私信标题用）。
func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return strings.Join(out, " ")
}
