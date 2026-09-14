package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// DocGroup 是一个分组：一张发票 + 它的订单 + 这些订单的付款。
//
// 需求定的是「一张发票一行」，所以存储层以它为单位，一个审批实例可以有 0..N 个。
type DocGroup struct {
	GroupIndex       int
	InvoiceNo        string
	InvoiceSlot      string
	OrderSlots       []string
	PaymentSlots     []string
	InvoiceTotalCent *int64
	SupportTotalCent *int64
	Matched          bool
	Reasons          []string
}

// txExec 是 *sql.Tx 的最小接口，便于在事务与直连间复用。
type txExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// saveGroupsTx 在事务里替换某实例的全部分组（与证据一样：同实例重跑=整体替换）。
func saveGroupsTx(ctx context.Context, tx txExec, instanceCode string, groups []DocGroup) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM doc_group WHERE instance_code = ?`, instanceCode); err != nil {
		return fmt.Errorf("清理旧分组: %w", err)
	}
	t := now()
	for _, g := range groups {
		reasons, _ := json.Marshal(g.Reasons)
		matched := 0
		if g.Matched {
			matched = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO doc_group (instance_code, group_index, invoice_no, invoice_slot,
				order_slots, payment_slots, invoice_total_cent, support_total_cent,
				matched, reasons, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			instanceCode, g.GroupIndex, g.InvoiceNo, g.InvoiceSlot,
			strings.Join(g.OrderSlots, ","), strings.Join(g.PaymentSlots, ","),
			g.InvoiceTotalCent, g.SupportTotalCent, matched, string(reasons), t); err != nil {
			return fmt.Errorf("写入分组 #%d: %w", g.GroupIndex, err)
		}
	}
	return nil
}

// Groups 读回某实例的分组（按 group_index 排序）。
func (d *DB) Groups(ctx context.Context, instanceCode string) ([]DocGroup, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT group_index, invoice_no, invoice_slot, order_slots, payment_slots,
		       invoice_total_cent, support_total_cent, matched, reasons
		FROM doc_group WHERE instance_code = ? ORDER BY group_index`, instanceCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocGroup
	for rows.Next() {
		var g DocGroup
		var orderSlots, paySlots, reasons string
		var matched int
		if err := rows.Scan(&g.GroupIndex, &g.InvoiceNo, &g.InvoiceSlot, &orderSlots,
			&paySlots, &g.InvoiceTotalCent, &g.SupportTotalCent, &matched, &reasons); err != nil {
			return nil, err
		}
		g.Matched = matched == 1
		if orderSlots != "" {
			g.OrderSlots = strings.Split(orderSlots, ",")
		}
		if paySlots != "" {
			g.PaymentSlots = strings.Split(paySlots, ",")
		}
		_ = json.Unmarshal([]byte(reasons), &g.Reasons)
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteInstance 把一个实例及其证据、分组全部删除。
//
// 用途：审批**被退回**时，该单不该留在「报销核对」里（需求原话：
// "被退回的就剔除掉"）。本地库同样清掉，否则发票号码唯一索引会把
// 这张票永远锁住 —— 队员改正后重新提交会被误判成重复报销。
//
// ⚠️ 这会同时释放该单占用的 sha256 / 发票号码。这是**有意**的：
//
//	退回意味着这单作废，它占用的票号应该能被重新提交。
//	审计记录（audit_log）保留，可追溯"曾经有过这一单"。
func (d *DB) DeleteInstance(ctx context.Context, instanceCode string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM doc_group WHERE instance_code = ?`, instanceCode); err != nil {
		return fmt.Errorf("删除分组: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM evidence WHERE instance_code = ?`, instanceCode); err != nil {
		return fmt.Errorf("删除证据: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM submission WHERE instance_code = ?`, instanceCode)
	if err != nil {
		return fmt.Errorf("删除提交: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	return d.audit(ctx, instanceCode, "delete_instance",
		fmt.Sprintf("审批被退回，已从本地库剔除（submission 删除 %d 行）", n))
}
