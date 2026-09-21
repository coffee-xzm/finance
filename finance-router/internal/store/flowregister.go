package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// FlowRegisterSync 是"一张「27-流水登记」登记单"的本地留痕（表见 migrations/0009）。
//
// 它是这一步的幂等锚点：事件重放时看 state 是否已 applied，是就跳过；
// 也是"全部明细登记完成后才开票"的判据。
type FlowRegisterSync struct {
	InstanceCode         string
	PurchaseInstanceCode string
	LedgerRecordID       string
	ItemIndex            int
	State                string // awaiting_applicant / applied / failed
	LastError            string
	CreatedAt            string
	UpdatedAt            string
}

// 登记单状态。
const (
	FlowRegisterAwaiting = "awaiting_applicant" // 已代建并退回给登记人
	FlowRegisterApplied  = "applied"            // 数据已覆盖到流水行
	FlowRegisterFailed   = "failed"
)

// UpsertFlowRegister 写入/更新一张登记单的留痕。
func (d *DB) UpsertFlowRegister(ctx context.Context, r FlowRegisterSync) error {
	now := time.Now().Format(time.RFC3339)
	if r.CreatedAt == "" {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO flow_register (
			instance_code, purchase_instance_code, ledger_record_id, item_index,
			state, last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(instance_code) DO UPDATE SET
			purchase_instance_code=excluded.purchase_instance_code,
			ledger_record_id=excluded.ledger_record_id,
			item_index=excluded.item_index,
			state=excluded.state,
			last_error=excluded.last_error,
			updated_at=excluded.updated_at`,
		r.InstanceCode, r.PurchaseInstanceCode, r.LedgerRecordID, r.ItemIndex,
		r.State, r.LastError, r.CreatedAt, r.UpdatedAt)
	return err
}

// GetFlowRegister 取一张登记单留痕；不存在返回 (nil,false,nil)。
func (d *DB) GetFlowRegister(ctx context.Context, instanceCode string) (*FlowRegisterSync, bool, error) {
	var r FlowRegisterSync
	err := d.sql.QueryRowContext(ctx, `
		SELECT instance_code, purchase_instance_code, ledger_record_id, item_index,
		       state, last_error, created_at, updated_at
		  FROM flow_register WHERE instance_code = ?`, instanceCode).
		Scan(&r.InstanceCode, &r.PurchaseInstanceCode, &r.LedgerRecordID, &r.ItemIndex,
			&r.State, &r.LastError, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &r, true, nil
}

// SetFlowRegisterState 只更新状态与失败原因。
func (d *DB) SetFlowRegisterState(ctx context.Context, instanceCode, state, lastErr string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE flow_register SET state=?, last_error=?, updated_at=? WHERE instance_code=?`,
		state, lastErr, time.Now().Format(time.RFC3339), instanceCode)
	return err
}

// DeleteFlowRegister 丢掉一张登记单的留痕（用于"上一张没退回去、换 uuid 重建"）。
func (d *DB) DeleteFlowRegister(ctx context.Context, instanceCode string) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM flow_register WHERE instance_code = ?`, instanceCode)
	return err
}

// FlowRegistersOfPurchase 取某采购带出的全部登记单（按明细序号）。
func (d *DB) FlowRegistersOfPurchase(ctx context.Context, purchaseInstanceCode string) ([]FlowRegisterSync, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT instance_code, purchase_instance_code, ledger_record_id, item_index,
		       state, last_error, created_at, updated_at
		  FROM flow_register WHERE purchase_instance_code = ? ORDER BY item_index`, purchaseInstanceCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowRegisterSync
	for rows.Next() {
		var r FlowRegisterSync
		if err := rows.Scan(&r.InstanceCode, &r.PurchaseInstanceCode, &r.LedgerRecordID, &r.ItemIndex,
			&r.State, &r.LastError, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
