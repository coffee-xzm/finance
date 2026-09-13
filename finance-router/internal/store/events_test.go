package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func mkEvent(id string) Event {
	return Event{
		EventID: id, EventType: "approval_instance",
		ApprovalCode: "APPROVAL_X", InstanceCode: "INST_1",
		Status: "PENDING", OperateTime: "1789281781000", StartUser: "ou_x",
	}
}

// ★ 幂等：同一 event_id 第二次到达必须判为重复，且不产生副作用。
func TestEventIdempotency(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	res, first, err := db.RecordEvent(ctx, mkEvent("ev_1"), EventAccepted, "")
	if err != nil || !first || res != EventAccepted {
		t.Fatalf("首次应接受：res=%s first=%v err=%v", res, first, err)
	}
	t.Log("✓ 首次事件接受")

	res, first, err = db.RecordEvent(ctx, mkEvent("ev_1"), EventAccepted, "")
	if err != nil {
		t.Fatal(err)
	}
	if first || res != EventDuplicate {
		t.Fatalf("重复事件应判重：res=%s first=%v", res, first)
	}
	t.Log("✓ 重复事件判重（幂等生效）")

	s, _ := db.EventStats(ctx)
	if s.Total != 1 {
		t.Errorf("收件箱应只有 1 条，实际 %d", s.Total)
	}
}

// ★ 并发：多个 goroutine 同时投同一事件，恰好 1 次被接受。
// 飞书"至少一次"投递 + 重试下会出现这种情形。
func TestConcurrentEventExactlyOneAccepted(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const n = 16

	var wg sync.WaitGroup
	accepted := make([]bool, n)
	duplicated := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, first, err := db.RecordEvent(ctx, mkEvent("ev_same"), EventAccepted, "")
			accepted[i] = err == nil && first
			duplicated[i] = err == nil && !first && res == EventDuplicate
		}(i)
	}
	wg.Wait()

	ok, dup := 0, 0
	for i := 0; i < n; i++ {
		if accepted[i] {
			ok++
		}
		if duplicated[i] {
			dup++
		}
	}
	if ok != 1 {
		t.Fatalf("%d 个并发投递，应恰好 1 次被接受，实际 %d", n, ok)
	}
	if dup != n-1 {
		t.Errorf("应有 %d 次被判定为重复，实际 %d", n-1, dup)
	}
	// ★ 重复事件**不写库**（避免收件箱膨胀）：表里只应留下 1 条。
	s, _ := db.EventStats(ctx)
	if s.Total != 1 {
		t.Errorf("收件箱应只有 1 条（重复不落库），实际 %d", s.Total)
	}
	t.Logf("✓ %d 个并发投递 → 接受 1 / 判重 %d，收件箱只留 1 条", n, dup)
}

// ★ 乱序保护：先到 APPROVED，后到的 PENDING 必须被拒绝（不能把终态打回）。
func TestOutOfOrderDoesNotRevertTerminalState(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	r, err := db.ApplyState(ctx, "I1", "APPROVED", "审批人", "先到的通过事件")
	if err != nil || !r.Applied || r.To != "已通过" {
		t.Fatalf("首次应落为已通过：%+v err=%v", r, err)
	}
	t.Logf("✓ 先到 APPROVED → %s", r.To)

	r, err = db.ApplyState(ctx, "I1", "PENDING", "审批人", "晚到的旧事件")
	if err != nil {
		t.Fatal(err)
	}
	if r.Applied {
		t.Fatal("晚到的 PENDING 竟然把终态打回了 —— 乱序保护失效")
	}
	if r.From != "已通过" || r.To != "审批中" {
		t.Errorf("应记录 已通过 → 审批中 被拒绝，实际 %+v", r)
	}
	t.Logf("✓ 晚到 PENDING 被拒: %s", r.Reason)

	st, _, _ := db.StateOf(ctx, "I1")
	if st != "已通过" {
		t.Errorf("状态应仍为已通过，实际 %s", st)
	}
	// 被拒绝的跃迁也要留痕
	if n, _ := db.TransitionCount(ctx); n < 2 {
		t.Errorf("应有 2 条跃迁记录（含被拒），实际 %d", n)
	}
	t.Log("✓ 非法跃迁已留痕")
}

// 终态之间不可互转。
func TestTerminalStatesAreFinal(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for _, seq := range [][2]string{
		{"APPROVED", "REJECTED"},
		{"REJECTED", "APPROVED"},
		{"CANCELED", "APPROVED"},
	} {
		db.ApplyState(ctx, "I2", seq[0], "a", "先")
		r, _ := db.ApplyState(ctx, "I2", seq[1], "a", "后")
		if r.Applied {
			t.Errorf("%s → %s 不应被允许", StateWord(seq[0]), StateWord(seq[1]))
		} else {
			t.Logf("✓ %s → %s 被拒", StateWord(seq[0]), StateWord(seq[1]))
		}
	}
}

// 同状态重复到达不应产生冗余跃迁。
func TestSameStateIsNoop(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	db.ApplyState(ctx, "I3", "PENDING", "a", "首次")
	r, _ := db.ApplyState(ctx, "I3", "PENDING", "a", "重复")
	if r.Applied {
		t.Error("同状态不应算跃迁")
	}
	if n, _ := db.TransitionCount(ctx); n != 1 {
		t.Errorf("应有 1 条跃迁，实际 %d", n)
	}
	t.Log("✓ 同状态重复到达是 no-op")
}

// 状态跃迁表 append-only。
func TestTransitionAppendOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	db.ApplyState(ctx, "I4", "APPROVED", "a", "x")
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM state_transition`); err == nil {
		t.Error("DELETE 跃迁表竟然成功")
	} else {
		t.Logf("✓ DELETE 被拒: %v", err)
	}
}

// 状态词映射。
func TestStateWordMapping(t *testing.T) {
	for raw, want := range map[string]string{
		"PENDING": "审批中", "APPROVED": "已通过", "REJECTED": "已拒绝",
		"CANCELED": "已撤回", "DELETED": "已删除",
	} {
		if got := StateWord(raw); got != want {
			t.Errorf("StateWord(%s) = %s，期望 %s", raw, got, want)
		}
	}
	t.Log("✓ 状态词映射正确")
	_ = fmt.Sprint()
}
