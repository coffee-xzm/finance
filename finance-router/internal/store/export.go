// export.go —— 导出审计的读写（表见 migrations/0006_export_audit.sql）。
//
// 一句话：**谁、什么时候、照哪份清单、把哪些票导到了哪里**。
// 这是服务端导出路线相对插件的核心增量（插件把文件落在操作者电脑上，
// 留不下这份底账）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ExportBatch 是一次导出的批次记录。
type ExportBatch struct {
	BatchID    string
	Scope      string // list | view
	TableName  string
	TableID    string
	ViewID     string
	ListSHA256 string // 清单内容哈希（不存清单原文）
	ListLines  int
	Fields     string
	Naming     string
	GroupBy    string
	Operator   string
	StartedAt  string
	FinishedAt string
	Records    int
	Success    int
	Failed     int
	Skipped    int
	EmptyRows  int
	OutDir     string
	ZipPath    string
}

// ExportItem 是批次里的一个文件（成功、跳过、失败都记）。
type ExportItem struct {
	RecordID     string
	InstanceNo   string
	Slot         string
	FileToken    string
	OriginalName string
	OutPath      string
	SHA256       string
	SizeBytes    int64
	Mime         string
	Status       string // ok | skipped | failed
	Error        string
}

// SaveExport 在一个事务里写批次 + 逐文件结果，并追加一条审计（哈希链）。
//
// 审计与结果必须同生共死：只有批次没有明细，等于没法对账；
// 只有明细没有批次，明细就是孤儿行。
func (d *DB) SaveExport(ctx context.Context, b ExportBatch, items []ExportItem) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if b.FinishedAt == "" {
		b.FinishedAt = now()
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO export_batch(batch_id, scope, table_name, table_id, view_id, list_sha256,
			list_lines, fields, naming, group_by, operator, started_at, finished_at,
			records, success, failed, skipped, empty_rows, out_dir, zip_path)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(batch_id) DO UPDATE SET
			finished_at=excluded.finished_at, success=excluded.success, failed=excluded.failed,
			skipped=excluded.skipped, empty_rows=excluded.empty_rows,
			records=excluded.records, zip_path=excluded.zip_path, out_dir=excluded.out_dir`,
		b.BatchID, b.Scope, b.TableName, b.TableID, b.ViewID, b.ListSHA256,
		b.ListLines, b.Fields, b.Naming, b.GroupBy, b.Operator, b.StartedAt, b.FinishedAt,
		b.Records, b.Success, b.Failed, b.Skipped, b.EmptyRows, b.OutDir, b.ZipPath)
	if err != nil {
		return fmt.Errorf("写入 export_batch: %w", err)
	}

	created := now()
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO export_item(batch_id, record_id, instance_no, slot, file_token,
				original_name, out_path, sha256, size_bytes, mime, status, error, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			b.BatchID, it.RecordID, it.InstanceNo, it.Slot, it.FileToken,
			it.OriginalName, it.OutPath, it.SHA256, it.SizeBytes, it.Mime,
			it.Status, it.Error, created); err != nil {
			return fmt.Errorf("写入 export_item(%s): %w", it.OutPath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return d.audit(ctx, b.BatchID, "export_batch",
		fmt.Sprintf("scope=%s table=%s records=%d ok=%d failed=%d skipped=%d zip=%s",
			b.Scope, b.TableID, b.Records, b.Success, b.Failed, b.Skipped, b.ZipPath))
}

// ListExportBatches 列出最近的导出批次（新的在前）。
func (d *DB) ListExportBatches(ctx context.Context, limit int) ([]ExportBatch, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT batch_id, scope, table_name, table_id, view_id, list_sha256, list_lines,
			fields, naming, group_by, operator, started_at, finished_at,
			records, success, failed, skipped, empty_rows, out_dir, zip_path
		FROM export_batch ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportBatch
	for rows.Next() {
		var b ExportBatch
		if err := rows.Scan(&b.BatchID, &b.Scope, &b.TableName, &b.TableID, &b.ViewID,
			&b.ListSHA256, &b.ListLines, &b.Fields, &b.Naming, &b.GroupBy, &b.Operator,
			&b.StartedAt, &b.FinishedAt, &b.Records, &b.Success, &b.Failed, &b.Skipped,
			&b.EmptyRows, &b.OutDir, &b.ZipPath); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetExportItems 取某批次的所有文件记录。
func (d *DB) GetExportItems(ctx context.Context, batchID string) ([]ExportItem, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT record_id, instance_no, slot, file_token, original_name, out_path,
			sha256, size_bytes, mime, status, error
		FROM export_item WHERE batch_id = ? ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportItem
	for rows.Next() {
		var it ExportItem
		if err := rows.Scan(&it.RecordID, &it.InstanceNo, &it.Slot, &it.FileToken,
			&it.OriginalName, &it.OutPath, &it.SHA256, &it.SizeBytes, &it.Mime,
			&it.Status, &it.Error); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ExportedRecordIDs 返回**成功导出过**的 record_id 集合。
// 供后续阶段（P4 闭环）判断"这行交过学校了没有"。
func (d *DB) ExportedRecordIDs(ctx context.Context) (map[string]string, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT record_id, MAX(created_at) FROM export_item
		WHERE status = 'ok' AND record_id <> '' GROUP BY record_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

// ExportCount 返回批次与明细条数（巡检/测试用）。
func (d *DB) ExportCount(ctx context.Context) (batches, items int, err error) {
	err = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM export_batch`).Scan(&batches)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	err = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM export_item`).Scan(&items)
	return batches, items, err
}

// EnsureExportTables 在旧库上确认导出审计表存在（Open 时迁移已保证）。
func (d *DB) EnsureExportTables(ctx context.Context) error {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='export_batch'`).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("export_batch 表不存在：迁移 0006 未应用（%s）", time.Now().Format(time.RFC3339))
	}
	return nil
}
