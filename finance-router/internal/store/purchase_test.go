package store

import (
	"context"
	"testing"
)

func TestPurchaseSyncRoundTrip(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// 不存在时返回 (nil,false,nil)
	if p, ok, err := db.GetPurchase(ctx, "NOPE"); err != nil || ok || p != nil {
		t.Fatalf("不存在的采购应返回 nil,false,nil；得到 %+v ok=%v err=%v", p, ok, err)
	}

	p := PurchaseSync{
		PurchaseInstanceCode: "P-1",
		ApprovalCode:         "CODE-P",
		ApplicantUserID:      "u1",
		PurchaseStatus:       "APPROVED",
		ProjectGroup:         "重装组",
		MirrorRecordID:       "rec1",
		LedgerRecordIDs:      []string{"recA", "recB"},
		InvoiceInstanceCode:  "INV-1",
		DraftState:           "awaiting_applicant",
	}
	if err := db.UpsertPurchase(ctx, p); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, ok, err := db.GetPurchase(ctx, "P-1")
	if err != nil || !ok {
		t.Fatalf("读回失败: ok=%v err=%v", ok, err)
	}
	if len(got.LedgerRecordIDs) != 2 || got.LedgerRecordIDs[0] != "recA" {
		t.Errorf("流水行 id 未正确往返: %+v", got.LedgerRecordIDs)
	}
	if got.ProjectGroup != "重装组" || got.DraftState != "awaiting_applicant" {
		t.Errorf("字段未正确往返: %+v", got)
	}

	// 发票反查（P5 用）
	byInv, ok, err := db.PurchaseByInvoice(ctx, "INV-1")
	if err != nil || !ok || byInv.PurchaseInstanceCode != "P-1" {
		t.Fatalf("按发票反查失败: %+v ok=%v err=%v", byInv, ok, err)
	}

	// 允许部分更新（草稿状态）
	if err := db.SetPurchaseDraft(ctx, "P-1", "submitted", ""); err != nil {
		t.Fatalf("更新草稿状态失败: %v", err)
	}
	got2, _, _ := db.GetPurchase(ctx, "P-1")
	if got2.DraftState != "submitted" {
		t.Errorf("草稿状态未更新: %+v", got2)
	}

	// upsert 第二次应覆盖而不是报主键冲突
	p.DraftState = "failed"
	p.LastError = "boom"
	if err := db.UpsertPurchase(ctx, p); err != nil {
		t.Fatalf("二次 upsert 失败: %v", err)
	}
	got3, _, _ := db.GetPurchase(ctx, "P-1")
	if got3.DraftState != "failed" || got3.LastError != "boom" {
		t.Errorf("upsert 未覆盖: %+v", got3)
	}
}
