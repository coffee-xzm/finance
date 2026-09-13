-- 0002_events.sql —— 审批事件幂等与状态机。
--
-- 飞书事件是**"至少一次"投递**（重试节奏 15s/5min/1h/6h，最多 4 次），
-- 所以必须按 event_id 去重；且事件**可能乱序**，所以状态流转要有白名单。

-- ── 事件收件箱：event_id 主键 = 幂等 ──────────────────────────
CREATE TABLE IF NOT EXISTS event_log (
    event_id      TEXT PRIMARY KEY,          -- ★ 幂等键：同一事件重复到达会被主键挡下
    event_type    TEXT NOT NULL,
    approval_code TEXT NOT NULL DEFAULT '',
    instance_code TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT '',
    operate_time  TEXT,
    start_user    TEXT NOT NULL DEFAULT '',
    received_at   TEXT NOT NULL,
    result        TEXT NOT NULL DEFAULT 'ACCEPTED',  -- ACCEPTED|DUPLICATE|IGNORED|ERROR
    detail        TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX IF NOT EXISTS idx_event_instance ON event_log(instance_code);
CREATE INDEX IF NOT EXISTS idx_event_received ON event_log(received_at);

-- ── 实例状态（状态机）────────────────────────────────────────
CREATE TABLE IF NOT EXISTS instance_state (
    instance_code TEXT PRIMARY KEY,
    state         TEXT NOT NULL,        -- 审批中/已通过/已拒绝/已撤回/…
    since         TEXT NOT NULL,
    updated_at    TEXT NOT NULL
) STRICT;

-- ── 状态跃迁留痕（append-only）────────────────────────────────
CREATE TABLE IF NOT EXISTS state_transition (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_code TEXT NOT NULL,
    from_state    TEXT NOT NULL,
    to_state      TEXT NOT NULL,
    actor         TEXT NOT NULL DEFAULT 'system',
    at            TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TRIGGER IF NOT EXISTS trg_transition_no_update
BEFORE UPDATE ON state_transition
BEGIN SELECT RAISE(ABORT, 'state_transition is append-only'); END;

CREATE TRIGGER IF NOT EXISTS trg_transition_no_delete
BEFORE DELETE ON state_transition
BEGIN SELECT RAISE(ABORT, 'state_transition is append-only'); END;
