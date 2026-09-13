package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ★ 重跑同一个实例必须幂等 —— 它的**自己的**旧证据不能把它判成重复报销。
//
// 这是真实发生过的 bug：SaveInstance 只 INSERT 不清理，重跑时自己的 sha 撞唯一索引，
// 被误判成"重复报销拦截"。唯一索引要防的是**别的实例**复用同一张图。
func TestRerunSameInstanceIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	evs := []Evidence{
		mkEvidence("INST_A", "发票文件", "sha_x"),
		mkEvidence("INST_A", "订单截图", "sha_y"),
	}
	if err := db.SaveInstance(ctx, mkSub("INST_A", "一致"), evs); err != nil {
		t.Fatalf("首次入库应成功: %v", err)
	}
	// 原样重跑
	if err := db.SaveInstance(ctx, mkSub("INST_A", "一致"), evs); err != nil {
		t.Fatalf("同实例重跑不该被拦（会误判成重复报销）: %v", err)
	}
	// 重跑后证据不应翻倍
	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evidence WHERE instance_code='INST_A'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("重跑后证据数应为 2（替换而非累加），实际 %d", n)
	}
	// 重跑后仍要能保护跨实例重复
	err := db.SaveInstance(ctx, mkSub("INST_B", "一致"),
		[]Evidence{mkEvidence("INST_B", "发票文件", "sha_x")})
	var dup *DupError
	if !errors.As(err, &dup) {
		t.Fatalf("重跑后跨实例重复仍须被拦，实际 %v", err)
	}
	if dup.ExistingInst != "INST_A" {
		t.Fatalf("DupError 应指向 INST_A，实际 %s", dup.ExistingInst)
	}
	t.Log("✓ 重跑幂等，且重跑后跨实例保护依然有效")
}

// ★ 重跑时若改了图（旧图从本单移除），旧 sha 必须被释放，
//
//	否则那张图就永远锁死在库里，别人再也提交不了。
func TestRerunReleasesRemovedEvidence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := db.SaveInstance(ctx, mkSub("INST_A", "一致"),
		[]Evidence{mkEvidence("INST_A", "发票文件", "sha_old")}); err != nil {
		t.Fatal(err)
	}
	// INST_A 换成新图
	if err := db.SaveInstance(ctx, mkSub("INST_A", "一致"),
		[]Evidence{mkEvidence("INST_A", "发票文件", "sha_new")}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := db.DuplicateOf(ctx, "sha_old"); found {
		t.Fatal("换图后旧 sha 仍被占用 —— 那张图被永久锁死")
	}
	if owner, found, _ := db.DuplicateOf(ctx, "sha_new"); !found || owner != "INST_A" {
		t.Fatalf("新 sha 应归属 INST_A，实际 found=%v owner=%s", found, owner)
	}
	t.Log("✓ 换图后旧 sha 正确释放")
}

// ★ reindex 必须自校正：清掉自己上次插入的占位行再重建。
//
//	陈旧占位行带的是过时/错误的 sha，会制造**虚假保护**。
func TestReindexIsSelfCorrecting(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	root := t.TempDir()

	write := func(inst, name, content string) {
		dir := filepath.Join(root, inst)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 没有 provenance 旁路表 → 退化为对落盘文件求哈希
	write("INST_A", "invoice-1-1.png", "AAA")

	n, _, degraded, err := db.ReindexFromFiles(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("首次重建应补 1 条，实际 %d", n)
	}
	if degraded != 1 {
		t.Fatalf("缺旁路表时应报 1 个 degraded，实际 %d", degraded)
	}

	// 文件内容变了 → 重建必须反映新 sha，旧 sha 必须释放
	write("INST_A", "invoice-1-1.png", "BBB")
	n, _, _, err = db.ReindexFromFiles(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var cnt int
	db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evidence WHERE provider='reindex'`).Scan(&cnt)
	if cnt != 1 {
		t.Fatalf("自校正后应恰好 1 条 reindex 行，实际 %d（陈旧占位行没清掉）", cnt)
	}
	_ = n
	t.Log("✓ reindex 自校正，陈旧占位行已清理")
}

// ★ provenance 旁路表：sha 必须取自**原始下载字节**，而不是落盘文件。
//
//	对 PDF 转出的 PNG，两者不同；用错会让重建的保护对 PDF 完全失效。
func TestReindexPrefersProvenanceRawSHA(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	root := t.TempDir()

	dir := filepath.Join(root, "INST_A")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 落盘的是 PNG（内容 PNG_BYTES），但原始附件是 PDF（sha PDF_RAW）
	if err := os.WriteFile(filepath.Join(dir, "invoice-1-1.png"), []byte("PNG_BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	prov := "invoice-1-1.png\tPDF_RAW\tapplication/pdf\t999\n"
	if err := os.WriteFile(filepath.Join(dir, provenanceFile), []byte(prov), 0o644); err != nil {
		t.Fatal(err)
	}

	n, _, degraded, err := db.ReindexFromFiles(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应补 1 条，实际 %d", n)
	}
	if degraded != 0 {
		t.Fatalf("有旁路表就不该 degraded，实际 %d", degraded)
	}
	// 必须用原始字节的 sha
	if _, found, _ := db.DuplicateOf(ctx, "PDF_RAW"); !found {
		t.Fatal("重建用的是落盘 PNG 的哈希，而不是原始 PDF 的 sha —— 对 PDF 保护失效")
	}
	t.Log("✓ reindex 正确采用原始字节 sha")
}

// ★ 重复检测必须读**文件系统**：库里因为有唯一索引，永远查不到重复行。
func TestDuplicateScanReadsDiskNotDB(t *testing.T) {
	root := t.TempDir()
	for _, inst := range []string{"INST_A", "INST_B"} {
		dir := filepath.Join(root, inst)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"invoice-1-1.png", "order-1.jpg", "payment-1.jpg"} {
			content := "shared-invoice"
			if f != "invoice-1-1.png" {
				content = inst + "-" + f // 订单/付款各不相同
			}
			if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	groups, _, err := ScanDuplicateGroups(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("应恰好发现 1 组共享发票，实际 %d 组: %v", len(groups), groups)
	}
	for _, insts := range groups {
		if len(insts) != 2 {
			t.Fatalf("该组应含 2 个实例，实际 %v", insts)
		}
	}
	t.Log("✓ 重复检测读文件系统，能发现库查不到的重复")
}
