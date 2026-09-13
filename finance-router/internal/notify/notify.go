// Package notify 负责把"需要人工处理"的行推送给管理员。
//
// 一个飞书坑：**open_id 是按应用隔离的**。从别处（另一个应用、审批后台）拿到的
// open_id 直接使用会报 `99992361 open_id cross app`。因此收件人优先用 user_id
// （租户内一致），其次才是本应用有效的 open_id。
package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/coffee/finance-router/internal/feishu"
)

// Recipient 是通知收件人。
type Recipient struct {
	OpenID string // 本应用有效的 open_id（优先）
	UserID string // 租户级 user_id（跨应用通用）
}

// Empty 判断是否没配收件人。
func (r Recipient) Empty() bool { return r.OpenID == "" && r.UserID == "" }

// Sender 封装"按可用的 ID 类型发送"。
type Sender struct {
	Client    *feishu.Client
	Recipient Recipient
}

// NeedManual 描述一条需要人工处理的行。
type NeedManual struct {
	InstanceCode string
	Slot         string
	Reason       string
	AmountCent   *int64
	UpperCheck   string
	TaxCheck     string
	Confidence   float64
	Verdict      string // 核对结果：存疑 / 缺件
	TableURL     string
}

// Send 发送单条待人工通知。返回实际使用的 ID 类型，便于日志。
func (s *Sender) Send(ctx context.Context, n NeedManual) (string, error) {
	if s.Recipient.Empty() {
		return "", fmt.Errorf("未配置收件人（feishu.admin_open_id 或 admin_user_id）")
	}
	text := format(n)
	if s.Recipient.UserID != "" {
		if err := s.Client.SendTextMessage(ctx, "user_id", s.Recipient.UserID, text); err != nil {
			return "user_id", err
		}
		return "user_id", nil
	}
	if err := s.Client.SendTextMessage(ctx, "open_id", s.Recipient.OpenID, text); err != nil {
		return "open_id", err
	}
	return "open_id", nil
}

func format(n NeedManual) string {
	var b strings.Builder
	b.WriteString("【财务系统】有一条识别结果需要人工确认\n")
	b.WriteString("━━━━━━━━━━━━━━━\n")
	if n.InstanceCode != "" {
		id := n.InstanceCode
		if len(id) > 8 {
			id = id[:8]
		}
		fmt.Fprintf(&b, "实例: %s\n", id)
	}
	if n.Slot != "" {
		fmt.Fprintf(&b, "槽位: %s\n", n.Slot)
	}
	if n.AmountCent != nil {
		fmt.Fprintf(&b, "价税合计: %.2f 元\n", float64(*n.AmountCent)/100)
	}
	if n.Verdict != "" {
		fmt.Fprintf(&b, "⚠ 核对结果: %s\n", n.Verdict)
	}
	if n.UpperCheck != "" && n.UpperCheck != "OK" && n.UpperCheck != "NO_UPPER" {
		fmt.Fprintf(&b, "⚠ 大写金额与小写对不上\n")
	}
	if n.TaxCheck == "TAX_MISMATCH" {
		fmt.Fprintf(&b, "⚠ 税额自检不通过\n")
	}
	if n.Reason != "" {
		fmt.Fprintf(&b, "原因: %s\n", n.Reason)
	}
	b.WriteString("━━━━━━━━━━━━━━━\n")
	b.WriteString("请在多维表格里核对后，把「人工审核」改为 通过/驳回。")
	if n.TableURL != "" {
		fmt.Fprintf(&b, "\n%s", n.TableURL)
	}
	return b.String()
}
