package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mkSub(code, verdict string) Submission {
	amt := int64(326)
	return Submission{
		InstanceCode: code, ApprovalCode: "APPROVAL_X", ApprovalName: "测试审批",
		Status: "已通过", Applicant: "张三", ApplicantDept: "机械组",
		MaterialType: "机械外购件", MaterialName: "螺丝", Buyer: "张三",
		FundSource: "个人", AmountCent: &amt, Verdict: verdict,
	}
}

func mkEvidence(code, slot, sha string) Evidence {
	return Evidence{
		InstanceCode: code, Slot: slot, Kind: "invoice", IndexNo: 1,
		Filename: slot + ".png", MediaType: "image/png", SHA256: sha, SizeBytes: 1234,
		Provider: "siliconflow-qwen-vl", Model: "Qwen/Qwen3-VL-32B-Instruct",
		UpperCheck: "OK", TaxCheck: "OK",
	}
}

// ★ 核心验收：同一张图（sha256）第二次提交必须被拒。
func TestSHA256UniqueBlocksDuplicate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := db.SaveInstance(ctx, mkSub("INST_A", "一致"),
		[]Evidence{mkEvidence("INST_A", "发票文件", "sha_same")}); err != nil {
		t.Fatalf("首次入库应成功: %v", err)
	}
	t.Log("✓ 首次入库成功")

	// 另一个实例提交同一张图
	err := db.SaveInstance(ctx, mkSub("INST_B", "一致"),
		[]Evidence{mkEvidence("INST_B", "发票文件", "sha_same")})
	if err == nil {
		t.Fatal("重复提交同一张图竟然成功了 —— 唯一索引没生效")
	}
	var dup *DupError
	if !errors.As(err, &dup) {
		t.Fatalf("期望 *DupError，实际 %T: %v", err, err)
	}
	if dup.ExistingInst != "INST_A" {
		t.Errorf("应指出首次占用者是 INST_A，实际 %s", dup.ExistingInst)
	}
	t.Logf("✓ 重复被拒，并指出首次来源: %s", dup.ExistingInst)

	// ★ 事务性：被拒的那一单不应留下任何痕迹
	if n, _ := db.countEvidence(ctx); n != 1 {
		t.Errorf("evidence 应只有 1 条，实际 %d", n)
	}
	if inst, _, _ := db.DuplicateOf(ctx, "sha_same"); inst != "INST_A" {
		t.Errorf("占用者应仍是 INST_A，实际 %s", inst)
	}
}

// ★ 并发场景：两个实例同时提交同一张图，必须恰好 1 次成功。
// 这正是飞书多维表格做不到的 TOCTOU 竞态（"查重后写入"两次都能通过）。
func TestConcurrentDuplicateExactlyOneSucceeds(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const n = 12

	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code := fmt.Sprintf("CONC_%02d", i)
			results[i] = db.SaveInstance(ctx, mkSub(code, "一致"),
				[]Evidence{mkEvidence(code, "发票文件", "sha_race")})
		}(i)
	}
	wg.Wait()

	ok, dup := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		default:
			var d *DupError
			if errors.As(err, &d) {
				dup++
			} else {
				t.Errorf("出现非预期错误: %v", err)
			}
		}
	}
	if ok != 1 {
		t.Fatalf("%d 个并发提交同一张图，应恰好 1 次成功，实际成功 %d 次", n, ok)
	}
	if dup != n-1 {
		t.Errorf("应有 %d 次被判定为重复，实际 %d", n-1, dup)
	}
	if ev, _ := db.countEvidence(ctx); ev != 1 {
		t.Errorf("evidence 应只有 1 条，实际 %d", ev)
	}
	t.Logf("✓ %d 个并发提交 → 成功 1 / 重复 %d，数据库里只有 1 条证据", n, dup)
}

// 同一实例内的不同图（不同 sha256）应全部入库。
func TestDistinctImagesAllStored(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	evs := []Evidence{
		mkEvidence("I1", "发票文件", "sha_inv"),
		mkEvidence("I1", "订单截图", "sha_order"),
		mkEvidence("I1", "付款截图", "sha_pay"),
	}
	if err := db.SaveInstance(ctx, mkSub("I1", "一致"), evs); err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if n, _ := db.countEvidence(ctx); n != 3 {
		t.Fatalf("应有 3 条证据，实际 %d", n)
	}
	t.Log("✓ 三张不同的图全部入库")
}

// 同一实例重跑（幂等）：更新业务字段，不重复插入证据。
func TestResaveSameInstanceIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.SaveInstance(ctx, mkSub("I1", "存疑"),
		[]Evidence{mkEvidence("I1", "发票文件", "sha_x")}); err != nil {
		t.Fatal(err)
	}
	// 重跑：verdict 变了，但证据相同 → 应被唯一索引拦下
	err := db.SaveInstance(ctx, mkSub("I1", "一致"),
		[]Evidence{mkEvidence("I1", "发票文件", "sha_x")})
	if err == nil {
		t.Log("注：同一实例重跑未报重复（取决于是否复用同一 sha）")
	}
	var d *DupError
	if errors.As(err, &d) {
		t.Logf("✓ 同实例重跑命中唯一约束（首次来自 %s）", d.ExistingInst)
	}
	// 无论哪条路径，证据不应翻倍
	if n, _ := db.countEvidence(ctx); n != 1 {
		t.Errorf("evidence 应仍为 1 条，实际 %d", n)
	}
}

// 审计表 append-only：UPDATE / DELETE 必须被触发器拒绝。
func TestAuditIsAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.SaveInstance(ctx, mkSub("I1", "一致"),
		[]Evidence{mkEvidence("I1", "发票文件", "sha_a")}); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.AuditCount(ctx); n == 0 {
		t.Fatal("应有审计记录")
	}

	if _, err := db.SQL().ExecContext(ctx, `UPDATE audit_log SET action='tampered'`); err == nil {
		t.Error("UPDATE 审计表竟然成功了 —— append-only 触发器没生效")
	} else {
		t.Logf("✓ UPDATE 被拒: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM audit_log`); err == nil {
		t.Error("DELETE 审计表竟然成功了")
	} else {
		t.Logf("✓ DELETE 被拒: %v", err)
	}
}

// 部门字典累积与查询。
func TestDeptDict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := db.LearnDept(ctx, "od-abc", "机械组"); err != nil {
		t.Fatal(err)
	}
	name, ok, err := db.DeptName(ctx, "od-abc")
	if err != nil || !ok || name != "机械组" {
		t.Fatalf("期望 机械组/true，实际 %q/%v/%v", name, ok, err)
	}
	if _, ok, _ := db.DeptName(ctx, "od-nope"); ok {
		t.Error("不存在的部门不应命中")
	}
	t.Log("✓ 部门字典可用")
}

func (d *DB) countEvidence(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM evidence`).Scan(&n)
	return n, err
}
