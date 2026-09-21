package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// PurchaseSync 是"一条采购审批被本服务处理过"的本地记录（表见 migrations/0007）。
//
// 它是采购链路的幂等锚点：事件重放时看它是否已有 invoice_instance_code / ledger_record_ids，
// 有就跳过对应动作。
type PurchaseSync struct {
	PurchaseInstanceCode string
	ApprovalCode         string
	ApplicantUserID      string
	PurchaseStatus       string
	ProjectGroup         string
	MirrorRecordID       string
	LedgerRecordIDs      []string
	RequestRecordIDs     []string // 写进 wiki「27采购申请表」的行 id
	InvoiceInstanceCode  string
	DraftState           string
	LastError            string
	CreatedAt            string
	UpdatedAt            string
}

// GetPurchase 取一条采购处理记录；不存在返回 (nil,false,nil)。
func (d *DB) GetPurchase(ctx context.Context, instanceCode string) (*PurchaseSync, bool, error) {
	var p PurchaseSync
	var ledger, request string
	err := d.sql.QueryRowContext(ctx, `
		SELECT purchase_instance_code, approval_code, applicant_user_id, purchase_status,
		       project_group, mirror_record_id, ledger_record_ids, request_record_ids,
		       invoice_instance_code, draft_state, last_error, created_at, updated_at
		  FROM purchase_sync WHERE purchase_instance_code = ?`, instanceCode).
		Scan(&p.PurchaseInstanceCode, &p.ApprovalCode, &p.ApplicantUserID, &p.PurchaseStatus,
			&p.ProjectGroup, &p.MirrorRecordID, &ledger, &request, &p.InvoiceInstanceCode,
			&p.DraftState, &p.LastError, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if ledger != "" {
		_ = json.Unmarshal([]byte(ledger), &p.LedgerRecordIDs)
	}
	if request != "" {
		_ = json.Unmarshal([]byte(request), &p.RequestRecordIDs)
	}
	return &p, true, nil
}

// UpsertPurchase 写入/更新一条采购处理记录。
func (d *DB) UpsertPurchase(ctx context.Context, p PurchaseSync) error {
	now := time.Now().Format(time.RFC3339)
	if p.CreatedAt == "" {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	ledger, _ := json.Marshal(p.LedgerRecordIDs)
	request, _ := json.Marshal(p.RequestRecordIDs)
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO purchase_sync (
			purchase_instance_code, approval_code, applicant_user_id, purchase_status,
			project_group, mirror_record_id, ledger_record_ids, request_record_ids,
			invoice_instance_code, draft_state, last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(purchase_instance_code) DO UPDATE SET
			approval_code=excluded.approval_code,
			applicant_user_id=excluded.applicant_user_id,
			purchase_status=excluded.purchase_status,
			project_group=excluded.project_group,
			mirror_record_id=excluded.mirror_record_id,
			ledger_record_ids=excluded.ledger_record_ids,
			request_record_ids=excluded.request_record_ids,
			invoice_instance_code=excluded.invoice_instance_code,
			draft_state=excluded.draft_state,
			last_error=excluded.last_error,
			updated_at=excluded.updated_at`,
		p.PurchaseInstanceCode, p.ApprovalCode, p.ApplicantUserID, p.PurchaseStatus,
		p.ProjectGroup, p.MirrorRecordID, string(ledger), string(request), p.InvoiceInstanceCode,
		p.DraftState, p.LastError, p.CreatedAt, p.UpdatedAt)
	return err
}

// SetPurchaseDraft 只更新代建/退回状态（失败原因可空）。
func (d *DB) SetPurchaseDraft(ctx context.Context, instanceCode, state, lastErr string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE purchase_sync SET draft_state=?, last_error=?, updated_at=? WHERE purchase_instance_code=?`,
		state, lastErr, time.Now().Format(time.RFC3339), instanceCode)
	return err
}

// PurchaseByInvoice 反查：某个发票收集实例是由哪条采购代建的（发票收齐后要回写流水进度）。
func (d *DB) PurchaseByInvoice(ctx context.Context, invoiceInstanceCode string) (*PurchaseSync, bool, error) {
	if invoiceInstanceCode == "" {
		return nil, false, nil
	}
	var p PurchaseSync
	var ledger, request string
	err := d.sql.QueryRowContext(ctx, `
		SELECT purchase_instance_code, approval_code, applicant_user_id, purchase_status,
		       project_group, mirror_record_id, ledger_record_ids, request_record_ids,
		       invoice_instance_code, draft_state, last_error, created_at, updated_at
		  FROM purchase_sync WHERE invoice_instance_code = ? LIMIT 1`, invoiceInstanceCode).
		Scan(&p.PurchaseInstanceCode, &p.ApprovalCode, &p.ApplicantUserID, &p.PurchaseStatus,
			&p.ProjectGroup, &p.MirrorRecordID, &ledger, &request, &p.InvoiceInstanceCode,
			&p.DraftState, &p.LastError, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if ledger != "" {
		_ = json.Unmarshal([]byte(ledger), &p.LedgerRecordIDs)
	}
	if request != "" {
		_ = json.Unmarshal([]byte(request), &p.RequestRecordIDs)
	}
	return &p, true, nil
}
