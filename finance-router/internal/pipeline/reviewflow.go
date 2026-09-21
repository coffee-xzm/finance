// reviewflow 是「27发票收集」审批的新状态机（docs/30-review/33 §4）：
//
//	校验没问题 → 服务自动同意该审批（全部行都干净才自动同意）
//	校验有问题 → 该行人工审核=驳回，等人在飞书里处理
//	审批一旦 已通过（机器或人）→ 该实例所有行 人工审核=通过 → 归档到「报销整合」
//
// 触发源是**审批状态事件**（approval_instance），不是多维表格单元格变更，
// 所以既不需要 bitable_record_changed 订阅，也不需要定时轮询。
package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/store"
)

// AutoApproveIfClean 在同步完成后调用：若该实例**全部行**核对一致（无 problem），
// 就扮演当前节点审批人自动同意该审批。任一行有问题则什么都不做。
func AutoApproveIfClean(ctx context.Context, cfg *config.Config, instanceCode string) error {
	if cfg.Dict.Triggers.AutoApproveWhenClean != nil && !*cfg.Dict.Triggers.AutoApproveWhenClean {
		return nil
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	rows, err := instanceRows(ctx, cfg, client, instanceCode)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	for _, r := range rows {
		if r.Verdict != "一致" {
			fmt.Printf("  ⊘ 实例 %s 有「%s」行，不自动通过审批\n", short(instanceCode), r.Verdict)
			return nil
		}
	}

	appr, ok := cfg.ApprovalByRole(config.RoleInvoiceCollect)
	if !ok || appr.Code == "" {
		return fmt.Errorf("配置缺少 role=invoice_collect 的审批")
	}
	det, _, err := client.GetInstanceDetail(ctx, instanceCode)
	if err != nil {
		return fmt.Errorf("读实例详情: %w", err)
	}
	if !strings.EqualFold(det.Status, "PENDING") {
		fmt.Printf("  ⊘ 实例 %s 状态 %s，无需自动同意\n", short(instanceCode), det.Status)
		return nil
	}
	n := 0
	for _, t := range det.TaskList {
		if !strings.EqualFold(t.Status, "PENDING") {
			continue
		}
		if err := client.ApproveTask(ctx, appr.Code, instanceCode, t.UserID, t.ID,
			"自动通过（校验一致，无需人工）"); err != nil {
			// OR 节点：第一个同意后流程已结束，后面的任务会报错 —— 属正常，不当失败。
			if strings.Contains(err.Error(), "has ended") {
				break
			}
			return fmt.Errorf("自动同意任务 %s 失败: %w", short(t.ID), err)
		}
		n++
	}
	if n > 0 {
		fmt.Printf("  ✓ 实例 %s 全部行一致 → 已自动同意审批（%d 个任务）\n", short(instanceCode), n)
	}
	return nil
}

// FinalizeApproved 在审批变成「已通过」时调用：
// 把该实例所有行的「人工审核」写为通过（覆盖此前的驳回），然后归档到「报销整合」。
func FinalizeApproved(ctx context.Context, cfg *config.Config, instanceCode string) error {
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	base, _ := cfg.Base(config.BaseReview)
	appToken := base.AppToken
	if appToken == "" {
		appToken = cfg.Feishu.Bitable.AppToken
	}
	rows, err := instanceRows(ctx, cfg, client, instanceCode)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Printf("  ⊘ 实例 %s 在核对表里没有行（可能还没同步）\n", short(instanceCode))
		return nil
	}
	pass := cfg.Dict.Rules.ReviewPass
	if pass == "" {
		pass = "通过"
	}
	header := cfg.Field(config.BaseReview, "human_review")
	for _, r := range rows {
		if err := client.UpdateBitableRecord(ctx, appToken,
			cfg.Table(config.BaseReview, "review"), r.RecordID,
			map[string]any{header: pass}); err != nil {
			return fmt.Errorf("回写人工审核失败 record=%s: %w", r.RecordID, err)
		}
	}
	fmt.Printf("  ✓ 实例 %s 审批通过 → %d 行标记「%s」\n", short(instanceCode), len(rows), pass)

	// 归档（只收 人工审核=通过 且 未归档 的行；幂等）
	if err := RunArchive(ArchiveOptions{CfgPath: cfg.Path}); err != nil {
		return fmt.Errorf("归档失败: %w", err)
	}
	// ★ 若这张发票收集单是"采购派生"的 → 回写对应流水行「发票收集进度=已通过」
	if err := markInvoiceCollected(ctx, cfg, client, instanceCode); err != nil {
		fmt.Printf("  ⚠ 回写流水「发票收集进度」失败（可稍后重跑）: %v\n", err)
	}
	return nil
}

// markInvoiceCollected 发票收齐（发票收集单审批通过）后，把该采购对应的流水行
// 「发票收集进度」写为「已通过」。不是采购派生的实例则什么都不做。
func markInvoiceCollected(ctx context.Context, cfg *config.Config, client *feishu.Client,
	invoiceInstanceCode string) error {

	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return err
	}
	defer db.Close()
	p, ok, err := db.PurchaseByInvoice(ctx, invoiceInstanceCode)
	if err != nil {
		return err
	}
	if !ok || len(p.LedgerRecordIDs) == 0 {
		return nil // 不是采购派生的（或还没写流水）
	}
	base, ok := cfg.Base(config.BaseFlow)
	tableID := cfg.Table(config.BaseFlow, "ledger")
	header := cfg.Field("ledger", "invoice_progress")
	done := cfg.Dict.Rules.InvoiceProgressDone
	if !ok || tableID == "" || header == "" || done == "" {
		return fmt.Errorf("配置缺少 flow.ledger 或 invoice_progress 口径")
	}
	for _, rid := range p.LedgerRecordIDs {
		// 服务自有列 → 允许覆盖（待办 → 已通过）
		if err := client.UpdateBitableRecord(ctx, base.AppToken, tableID, rid,
			map[string]any{header: done}); err != nil {
			return fmt.Errorf("更新流水行 %s 失败: %w%s", rid, err,
				writeDeniedHint(ctx, client, cfg, base.AppToken, err))
		}
	}
	fmt.Printf("  ✓ 发票收齐 → 流水 %d 行「%s」已写「%s」\n", len(p.LedgerRecordIDs), header, done)
	return nil
}

// instanceRows 取核对表里某个实例的全部行。
func instanceRows(ctx context.Context, cfg *config.Config, client *feishu.Client,
	instanceCode string) ([]existingRow, error) {

	base, _ := cfg.Base(config.BaseReview)
	appToken := base.AppToken
	tableID := cfg.Table(config.BaseReview, "review")
	if appToken == "" || tableID == "" {
		// 兼容旧配置
		appToken = cfg.Feishu.Bitable.AppToken
		tableID = cfg.Feishu.Bitable.Tables["submission"]
	}
	if appToken == "" || tableID == "" {
		return nil, fmt.Errorf("配置缺少核对表 base/table")
	}
	all, err := existingRows(ctx, cfg, client, appToken, tableID)
	if err != nil {
		return nil, err
	}
	var out []existingRow
	for _, r := range all {
		if r.Code == instanceCode {
			out = append(out, r)
		}
	}
	return out, nil
}

// MarkInvoiceCollected 是 markInvoiceCollected 的导出包装（供运维/测试命令调用）。
func MarkInvoiceCollected(ctx context.Context, cfg *config.Config, invoiceInstanceCode string) error {
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	return markInvoiceCollected(ctx, cfg, client, invoiceInstanceCode)
}

// SetLedgerProgress 直接改写某条流水行的「发票收集进度」（仅运维/测试用，服务自有列）。
func SetLedgerProgress(ctx context.Context, cfg *config.Config, ledgerRecordID, value string) error {
	base, ok := cfg.Base(config.BaseFlow)
	tableID := cfg.Table(config.BaseFlow, "ledger")
	header := cfg.Field("ledger", "invoice_progress")
	if !ok || tableID == "" || header == "" {
		return fmt.Errorf("配置缺少 flow.ledger / invoice_progress")
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	return client.UpdateBitableRecord(ctx, base.AppToken, tableID, ledgerRecordID,
		map[string]any{header: value})
}
