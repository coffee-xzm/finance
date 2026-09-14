package store

import (
	"context"
	"testing"
)

// 退回状态判定：飞书的多种终态都意味着"这单不报了"，都该剔除。
func TestIsRejectedState(t *testing.T) {
	yes := []string{"REJECTED", "CANCELED", "DELETED", "REVERTED", "TERMINATED",
		"已拒绝", "已撤回", "已撤销", "已终止", "已删除", "rejected"}
	for _, s := range yes {
		if !IsRejectedState(s) {
			t.Errorf("%q 应判为需剔除", s)
		}
	}
	no := []string{"PENDING", "APPROVED", "审批中", "已通过", ""}
	for _, s := range no {
		if IsRejectedState(s) {
			t.Errorf("%q 不该判为需剔除", s)
		}
	}
}

// ★ 退回剔除必须**释放发票号码**。
//
// 否则：队员交了一单被退回 → 改正后重新提交 → 撞上发票号码唯一索引
// → 被误判成"重复报销"，而且他并没有重复报销。这个 bug 会让正确的人被挡住。
func TestDeleteInstanceFreesInvoiceNo(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const inst = "INST_REJECT"
	const invNo = "10000000000000000001"

	ev := mkEvidence(inst, "发票文件", "sha_reject")
	ev.InvoiceNo = invNo
	ev.InvoiceNoSrc = "exact"

	grp := DocGroup{
		GroupIndex: 0, InvoiceNo: invNo, InvoiceSlot: "发票文件",
		OrderSlots: []string{"订单截图"}, Matched: true,
		Reasons: []string{"测试"},
	}
	if err := db.SaveInstanceWithGroups(ctx, mkSub(inst, "一致"), []Evidence{ev}, []DocGroup{grp}); err != nil {
		t.Fatalf("首次入库: %v", err)
	}
	// ★ 该票号已被占用：换一个实例用**同一发票号码**提交，必须被唯一索引挡住
	//   （注意图不同 —— 字节级 sha256 不同，只有票号相同，这正是要防的场景）
	dup := mkEvidence("INST_OTHER", "发票文件", "sha_other")
	dup.InvoiceNo, dup.InvoiceNoSrc = invNo, "exact"
	err := db.SaveInstanceWithGroups(ctx, mkSub("INST_OTHER", "一致"), []Evidence{dup}, nil)
	if err == nil {
		t.Fatal("同一发票号码的另一次提交竟然通过了 —— 发票号码唯一索引没生效")
	}
	t.Logf("✓ 同票号再提交被挡: %v", err)

	gs, _ := db.Groups(ctx, inst)
	if len(gs) != 1 {
		t.Fatalf("分组应落库: %d", len(gs))
	}

	// 退回 → 剔除
	if err := db.DeleteInstance(ctx, inst); err != nil {
		t.Fatalf("剔除: %v", err)
	}
	if exists, _ := db.HasSubmission(ctx, inst); exists {
		t.Fatal("submission 应已删除")
	}
	if gs, _ := db.Groups(ctx, inst); len(gs) != 0 {
		t.Fatalf("分组应已删除，实际 %d", len(gs))
	}
	// ★ 剔除后同一票号必须能重新入库
	re := mkEvidence("INST_OTHER2", "发票文件", "sha_other2")
	re.InvoiceNo, re.InvoiceNoSrc = invNo, "exact"
	if err := db.SaveInstanceWithGroups(ctx, mkSub("INST_OTHER2", "一致"), []Evidence{re}, nil); err != nil {
		t.Fatalf("剔除后票号应已释放，实际仍被挡: %v", err)
	}
	t.Log("✓ 剔除后 submission/分组/证据全清，票号已释放且可重新提交")
}

// 被剔除后，同一张发票可以重新入库（改正后重交的正常路径）
func TestReSubmitAfterRejectWorks(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const invNo = "20000000000000000002"

	ev1 := mkEvidence("INST_A", "发票文件", "sha_a")
	ev1.InvoiceNo, ev1.InvoiceNoSrc = invNo, "exact"
	if err := db.SaveInstanceWithGroups(ctx, mkSub("INST_A", "一致"), []Evidence{ev1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteInstance(ctx, "INST_A"); err != nil {
		t.Fatal(err)
	}
	// 同一张票，改正后重新提交
	ev2 := mkEvidence("INST_B", "发票文件", "sha_b")
	ev2.InvoiceNo, ev2.InvoiceNoSrc = invNo, "exact"
	if err := db.SaveInstanceWithGroups(ctx, mkSub("INST_B", "一致"), []Evidence{ev2}, nil); err != nil {
		t.Fatalf("改正后重交应能通过，实际被挡: %v", err)
	}
	t.Log("✓ 退回后重交同一张发票不再被判重复")
}

// 模型读出的号码**不参与**唯一性判定（读错一位不该锁死一个不存在的号）
func TestModelReadInvoiceNoDoesNotBlock(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const invNo = "30000000000000000003"

	e1 := mkEvidence("INST_X", "发票文件", "sha_x")
	e1.InvoiceNo, e1.InvoiceNoSrc = invNo, "model"
	if err := db.SaveInstanceWithGroups(ctx, mkSub("INST_X", "一致"), []Evidence{e1}, nil); err != nil {
		t.Fatal(err)
	}
	e2 := mkEvidence("INST_Y", "发票文件", "sha_y")
	e2.InvoiceNo, e2.InvoiceNoSrc = invNo, "model"
	if err := db.SaveInstanceWithGroups(ctx, mkSub("INST_Y", "一致"), []Evidence{e2}, nil); err != nil {
		t.Fatalf("模型读的号码不该触发唯一约束: %v", err)
	}
	t.Log("✓ 模型读出的号码只入库、不参与唯一性判定")
}
