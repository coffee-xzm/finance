package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Event 是一条收到的审批事件。
type Event struct {
	EventID      string
	EventType    string
	ApprovalCode string
	InstanceCode string
	Status       string // 飞书原始状态：PENDING/APPROVED/REJECTED/CANCELED/DELETED
	OperateTime  string
	StartUser    string
}

// EventResult 是事件的落库结果。
type EventResult string

const (
	EventAccepted  EventResult = "ACCEPTED"
	EventDuplicate EventResult = "DUPLICATE"
	EventIgnored   EventResult = "IGNORED"
	EventError     EventResult = "ERROR"
)

// RecordEvent 把事件写进收件箱，**以 event_id 主键做幂等**。
//
// 返回 (结果, 是否首次)。首次返回 EventAccepted+true；
// 重复到达返回 EventDuplicate+false（调用方应直接返回，不做任何副作用）。
func (d *DB) RecordEvent(ctx context.Context, e Event, result EventResult, detail string) (EventResult, bool, error) {
	if e.EventID == "" {
		return EventError, false, fmt.Errorf("event_id 为空，无法做幂等")
	}
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO event_log(event_id, event_type, approval_code, instance_code,
			status, operate_time, start_user, received_at, result, detail)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		e.EventID, e.EventType, e.ApprovalCode, e.InstanceCode,
		e.Status, nullIfEmpty(e.OperateTime), e.StartUser, now(), string(result), detail)
	if err != nil {
		if isUniqueViolation(err) {
			// ★ 命中主键 = 这个事件已经处理过 → 幂等命中
			return EventDuplicate, false, nil
		}
		return EventError, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return EventDuplicate, false, nil
	}
	return result, true, nil
}

// UpdateEventResult 回填处理结果（接收时先写 ACCEPTED，处理完再更新）。
func (d *DB) UpdateEventResult(ctx context.Context, eventID string, result EventResult, detail string) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE event_log SET result=?, detail=? WHERE event_id=?`, string(result), detail, eventID)
	return err
}

// EventStats 是事件统计。
type EventStats struct {
	Total     int
	Accepted  int
	Duplicate int
	Ignored   int
	Error     int
}

func (d *DB) EventStats(ctx context.Context) (*EventStats, error) {
	s := &EventStats{}
	for _, q := range []struct {
		sql string
		dst *int
	}{
		{`SELECT COUNT(*) FROM event_log`, &s.Total},
		{`SELECT COUNT(*) FROM event_log WHERE result='ACCEPTED'`, &s.Accepted},
		{`SELECT COUNT(*) FROM event_log WHERE result='DUPLICATE'`, &s.Duplicate},
		{`SELECT COUNT(*) FROM event_log WHERE result='IGNORED'`, &s.Ignored},
		{`SELECT COUNT(*) FROM event_log WHERE result='ERROR'`, &s.Error},
	} {
		if err := d.sql.QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ── 状态机 ────────────────────────────────────────────────────
//
// 为什么需要：飞书事件**可能乱序**（例如 APPROVED 先到、PENDING 后到）。
// 若不做白名单，晚到的 PENDING 会把已通过的实例打回去。
// 这是乱序下唯一可靠的护栏 —— 不依赖事件顺序假设。

var stateAlias = map[string]string{
	"PENDING":    "审批中",
	"APPROVED":   "已通过",
	"REJECTED":   "已拒绝",
	"CANCELED":   "已撤回",
	"DELETED":    "已删除",
	"REVERTED":   "已撤销",
	"TERMINATED": "已终止",
}

// StateWord 把飞书状态枚举转成表里的中文。
func StateWord(s string) string {
	if w, ok := stateAlias[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return w
	}
	return s
}

// allowedTransitions 是状态流转白名单。不在表里的一律拒绝。
//
// 设计：**终态不可再变**；只有"审批中"能走向终态。
var allowedTransitions = map[string][]string{
	"审批中": {"已通过", "已拒绝", "已撤回", "已撤销", "已终止", "已删除"},
	"已通过": {}, "已拒绝": {}, "已撤回": {}, "已撤销": {}, "已终止": {}, "已删除": {},
}

// rejectedStates 是"这单作废"的状态集合。
//
// 需求原话：「被退回的就剔除掉」「在报销核对表单可通过审批通过或退回选项…」。
// 飞书侧对应的终态有多个（拒绝/撤回/撤销/终止/删除），它们都意味着
// **这单不会报销了**，因此都该从「报销核对」里剔除，释放它占用的发票号码。
//
// ⚠️ 注意「退回」在飞书里通常表现为 REJECTED（审批人驳回）或 REVERTED（撤销），
//
//	两者都在这里。若将来飞书新增"退回给发起人"的独立状态，需要补进来 ——
//	漏掉的后果是该单永远留在核对表里，占着发票号码。
var rejectedStates = map[string]bool{
	"已拒绝": true, "已撤回": true, "已撤销": true, "已终止": true, "已删除": true,
}

// IsRejectedState 判断原始状态（飞书枚举或中文）是否属于"该剔除"。
func IsRejectedState(raw string) bool {
	return rejectedStates[StateWord(raw)]
}

// TransitionResult 是一次状态跃迁的结果。
type TransitionResult struct {
	Applied bool // 是否真的发生了跃迁
	From    string
	To      string
	Reason  string
}

// ApplyState 尝试推进实例状态。**非法跃迁会被拒绝并留痕**。
//
// 这是乱序保护的落点：晚到的旧事件不会把终态打回去。
func (d *DB) ApplyState(ctx context.Context, instanceCode, rawStatus, actor, reason string) (TransitionResult, error) {
	to := StateWord(rawStatus)
	if instanceCode == "" || to == "" {
		return TransitionResult{}, fmt.Errorf("instance_code / status 不能为空")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return TransitionResult{}, err
	}
	defer tx.Rollback()

	var from string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM instance_state WHERE instance_code = ?`, instanceCode).Scan(&from)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 首次见到：直接落状态
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO instance_state(instance_code, state, since, updated_at)
			VALUES (?,?,?,?)`, instanceCode, to, now(), now()); err != nil {
			return TransitionResult{}, err
		}
		if err := insertTransition(ctx, tx, instanceCode, "", to, actor, reason); err != nil {
			return TransitionResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return TransitionResult{}, err
		}
		return TransitionResult{Applied: true, From: "", To: to, Reason: "首次记录"}, nil
	case err != nil:
		return TransitionResult{}, err
	}

	if from == to {
		return TransitionResult{Applied: false, From: from, To: to, Reason: "状态未变"}, nil
	}
	// 白名单检查
	ok := false
	for _, a := range allowedTransitions[from] {
		if a == to {
			ok = true
			break
		}
	}
	if !ok {
		// 非法跃迁：留痕但**不改状态**
		if err := insertTransition(ctx, tx, instanceCode, from, to, actor,
			"拒绝非法跃迁（乱序保护）: "+reason); err != nil {
			return TransitionResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return TransitionResult{}, err
		}
		return TransitionResult{Applied: false, From: from, To: to,
			Reason: fmt.Sprintf("拒绝：%s → %s 不在白名单（乱序保护）", from, to)}, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE instance_state SET state=?, updated_at=? WHERE instance_code=?`,
		to, now(), instanceCode); err != nil {
		return TransitionResult{}, err
	}
	if err := insertTransition(ctx, tx, instanceCode, from, to, actor, reason); err != nil {
		return TransitionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return TransitionResult{}, err
	}
	return TransitionResult{Applied: true, From: from, To: to, Reason: reason}, nil
}

func insertTransition(ctx context.Context, tx *sql.Tx, instance, from, to, actor, reason string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO state_transition(instance_code, from_state, to_state, actor, at, reason)
		VALUES (?,?,?,?,?,?)`, instance, from, to, actor, now(), reason)
	return err
}

// StateOf 查实例当前状态。
func (d *DB) StateOf(ctx context.Context, instanceCode string) (string, bool, error) {
	var s string
	err := d.sql.QueryRowContext(ctx,
		`SELECT state FROM instance_state WHERE instance_code=?`, instanceCode).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return s, true, nil
}

// TransitionCount 返回跃迁记录数（含被拒绝的）。
func (d *DB) TransitionCount(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_transition`).Scan(&n)
	return n, err
}
