package store

import (
	"context"
	"testing"
)

func TestSaveExportAndReadBack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	b := ExportBatch{
		BatchID: "export-20260919-150000", Scope: "list",
		TableName: "报销整合", TableID: "tblSYNTHETIC00000",
		ListSHA256: "abc123", ListLines: 3,
		Fields: "发票,订单截图,付款记录", Naming: "{部门}-{购买人}", GroupBy: "department",
		Operator: "cli:coffee", StartedAt: "2026-09-19T15:00:00+08:00",
		Records: 2, Success: 1, Failed: 1, Skipped: 0, EmptyRows: 1,
		OutDir: "data/export/2026-08", ZipPath: "data/export/2026-08.zip",
	}
	items := []ExportItem{
		{RecordID: "recA", InstanceNo: "INST1", Slot: "invoice", FileToken: "tok1",
			OriginalName: "image.png", OutPath: "视觉组/a.pdf", SHA256: "s1",
			SizeBytes: 100, Mime: "application/pdf", Status: "ok"},
		{RecordID: "recB", InstanceNo: "INST2", Slot: "order", FileToken: "tok2",
			OutPath: "电控组/b.jpg", Status: "failed", Error: "取临时链接失败"},
	}
	if err := db.SaveExport(ctx, b, items); err != nil {
		t.Fatalf("SaveExport: %v", err)
	}

	batches, err := db.ListExportBatches(ctx, 10)
	if err != nil {
		t.Fatalf("ListExportBatches: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("批次条数 = %d，期望 1", len(batches))
	}
	got := batches[0]
	if got.BatchID != b.BatchID || got.Success != 1 || got.Failed != 1 ||
		got.ListSHA256 != "abc123" || got.ZipPath != b.ZipPath {
		t.Fatalf("读回的批次与写入不一致: %+v", got)
	}
	if got.FinishedAt == "" {
		t.Fatal("finished_at 应被自动填上")
	}

	back, err := db.GetExportItems(ctx, b.BatchID)
	if err != nil {
		t.Fatalf("GetExportItems: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("明细条数 = %d，期望 2", len(back))
	}
	if back[0].FileToken != "tok1" || back[1].Error == "" {
		t.Fatalf("明细内容不对: %+v", back)
	}

	// 审计必须也留下一条（哈希链）
	if n, err := db.AuditCount(ctx); err != nil || n < 1 {
		t.Fatalf("审计条数 = %d, err=%v；期望至少 1 条", n, err)
	}

	nb, ni, err := db.ExportCount(ctx)
	if err != nil || nb != 1 || ni != 2 {
		t.Fatalf("ExportCount = (%d,%d), err=%v；期望 (1,2)", nb, ni, err)
	}
}

func TestExportBatchUpsert(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	b := ExportBatch{BatchID: "b1", Scope: "list", StartedAt: "t0", Records: 1}
	if err := db.SaveExport(ctx, b, nil); err != nil {
		t.Fatalf("第一次 SaveExport: %v", err)
	}
	b.Success = 5
	b.ZipPath = "x.zip"
	if err := db.SaveExport(ctx, b, nil); err != nil {
		t.Fatalf("第二次 SaveExport（upsert）: %v", err)
	}
	batches, err := db.ListExportBatches(ctx, 10)
	if err != nil {
		t.Fatalf("ListExportBatches: %v", err)
	}
	if len(batches) != 1 || batches[0].Success != 5 || batches[0].ZipPath != "x.zip" {
		t.Fatalf("upsert 后批次不对: %+v", batches)
	}
}

func TestExportedRecordIDs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	b := ExportBatch{BatchID: "b1", Scope: "list", StartedAt: "t0"}
	items := []ExportItem{
		{RecordID: "recA", Status: "ok"},
		{RecordID: "recA", Status: "ok"},
		{RecordID: "recB", Status: "failed"},
	}
	if err := db.SaveExport(ctx, b, items); err != nil {
		t.Fatalf("SaveExport: %v", err)
	}
	ids, err := db.ExportedRecordIDs(ctx)
	if err != nil {
		t.Fatalf("ExportedRecordIDs: %v", err)
	}
	if len(ids) != 1 || ids["recA"] == "" {
		t.Fatalf("期望只有 recA 成功导出，得到 %v", ids)
	}
}
