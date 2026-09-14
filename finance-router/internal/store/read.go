package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SyncInstance 是"落表"需要的完整一个实例：元信息 + 全部证据 + 全部分组。
//
// 为什么从库里读、而不是读 manifest.jsonl：
// manifest.jsonl 每次 extract 都被 os.Create **截断**，常驻服务一次只处理一个实例，
// 那个文件里永远只剩最后一个实例 —— 拿它当同步来源会漏掉其余全部。
// 本地库才是权威来源。
type SyncInstance struct {
	Sub      Submission
	Evidence []Evidence
	Groups   []DocGroup
}

// SyncInstances 读出全部实例（按首次入库时间排序），供「报销核对」落表用。
// limit > 0 时只取前 limit 个。
func (d *DB) SyncInstances(ctx context.Context, limit int) ([]SyncInstance, error) {
	q := `SELECT ` + submissionCols + ` FROM submission ORDER BY first_seen_at`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	return d.loadSync(ctx, q)
}

// SyncInstanceOne 只读一个实例。
//
// 常驻服务用它：每收到一单就落一次表，若每次都对**全部**实例重跑，
// 附件会被反复重传（飞书附件不能跨表复用 token，只能重传），
// 实例一多就是 N×N 次上传 —— 又慢又浪费配额。
func (d *DB) SyncInstanceOne(ctx context.Context, instanceCode string) ([]SyncInstance, error) {
	return d.loadSync(ctx, `SELECT `+submissionCols+` FROM submission WHERE instance_code = ?`,
		instanceCode)
}

func (d *DB) loadSync(ctx context.Context, q string, args ...any) ([]SyncInstance, error) {
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncInstance
	for rows.Next() {
		s, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, SyncInstance{Sub: s})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 证据与分组按实例逐个取。实例数是个位数~几十，N+1 完全可接受；
	// 换成 join 反而要处理笛卡尔积去重，得不偿失。
	for i := range out {
		code := out[i].Sub.InstanceCode
		evs, err := d.EvidenceOf(ctx, code)
		if err != nil {
			return nil, err
		}
		gs, err := d.Groups(ctx, code)
		if err != nil {
			return nil, err
		}
		out[i].Evidence, out[i].Groups = evs, gs
	}
	return out, nil
}

const submissionCols = `instance_code, approval_code, approval_name, status,
	applicant, applicant_dept, material_type, material_name, buyer, fund_source,
	amount_cent, tax_cent, invoice_date, seller, verdict,
	applink, start_time_ms, applicant_dept_id, departments_json, is_alipay,
	dachuang, remark, form_problem, leftover_json`

// rowScanner 抽象 *sql.Row 与 *sql.Rows 的公共部分。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSubmission(sc rowScanner) (Submission, error) {
	var s Submission
	var deptsJSON, leftoverJSON string
	// 注意：invoice_date 在 schema 里可空（历史行没有发票日期），
	// 直接 Scan 进 string 会报 "converting NULL to string is unsupported"。
	var invoiceDate sql.NullString
	err := sc.Scan(&s.InstanceCode, &s.ApprovalCode, &s.ApprovalName, &s.Status,
		&s.Applicant, &s.ApplicantDept, &s.MaterialType, &s.MaterialName, &s.Buyer,
		&s.FundSource, &s.AmountCent, &s.TaxCent, &invoiceDate, &s.Seller, &s.Verdict,
		&s.Applink, &s.StartTimeMS, &s.ApplicantDeptID, &deptsJSON, &s.IsAlipay,
		&s.Dachuang, &s.Remark, &s.FormProblem, &leftoverJSON)
	if err != nil {
		return s, err
	}
	s.InvoiceDate = invoiceDate.String
	s.Departments = parseList(deptsJSON)
	s.Leftover = parseList(leftoverJSON)
	return s, nil
}

// UpdateSubmissionMeta 只回写「表单元信息」，不动证据与分组。
//
// 用途：早期实例入库时还没有这些列，事后从审批单重新取一次填上。
func (d *DB) UpdateSubmissionMeta(ctx context.Context, s Submission) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE submission SET
		approval_name=?, status=?, applicant=?, applicant_dept=?, applicant_dept_id=?,
		material_type=?, material_name=?, buyer=?, fund_source=?, is_alipay=?,
		dachuang=?, remark=?, applink=?, start_time_ms=?, departments_json=?, updated_at=?
		WHERE instance_code=?`,
		s.ApprovalName, s.Status, s.Applicant, s.ApplicantDept, s.ApplicantDeptID,
		s.MaterialType, s.MaterialName, s.Buyer, s.FundSource, s.IsAlipay,
		s.Dachuang, s.Remark, s.Applink, s.StartTimeMS, jsonList(s.Departments), now(),
		s.InstanceCode)
	if err != nil {
		return fmt.Errorf("回写 submission 元信息: %w", err)
	}
	return d.audit(ctx, s.InstanceCode, "refresh_meta", "从审批单刷新表单字段")
}

// EvidenceOf 读出某实例的全部证据行（含本地图路径与配对键）。
func (d *DB) EvidenceOf(ctx context.Context, instanceCode string) ([]Evidence, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT slot, kind, index_no, filename, media_type, sha256, size_bytes,
		       amount_incl_tax_cent, tax_cent, amount_excl_tax_cent, amount_upper,
		       date, counterparty, upper_check, tax_check, local_png,
		       invoice_no, invoice_no_src, order_no, alipay_txn_id, local_pngs
		FROM evidence WHERE instance_code = ? ORDER BY slot, index_no`, instanceCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var pngsJSON string
		var date sql.NullString // 可空：历史行可能没有单据日期
		e.InstanceCode = instanceCode
		if err := rows.Scan(&e.Slot, &e.Kind, &e.IndexNo, &e.Filename, &e.MediaType,
			&e.SHA256, &e.SizeBytes, &e.AmountInclTaxCent, &e.TaxCent,
			&e.AmountExclTaxCent, &e.AmountUpper, &date, &e.Counterparty,
			&e.UpperCheck, &e.TaxCheck, &e.LocalPNG,
			&e.InvoiceNo, &e.InvoiceNoSrc, &e.OrderNo, &e.AlipayTxnID, &pngsJSON); err != nil {
			return nil, err
		}
		e.Date = date.String
		e.LocalPNGs = parseList(pngsJSON)
		if len(e.LocalPNGs) == 0 && e.LocalPNG != "" {
			e.LocalPNGs = []string{e.LocalPNG} // 老数据只有第 1 页
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
