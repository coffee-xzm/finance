-- 0007_purchase_sync.sql —— 采购审批 → 流水 / 发票收集 的幂等与产物留痕
--
-- 为什么要本地记：飞书事件是"至少一次"投递，采购 APPROVED 可能重复到达；
-- 而"写流水""代建发票单"都不是天然幂等的动作。用采购实例 code 做主键，
-- 事件重放时先查这里，已经做过就不再重复。
--
-- 字段口径见 docs/30-review/33 §8。

CREATE TABLE IF NOT EXISTS purchase_sync (
    purchase_instance_code TEXT PRIMARY KEY,          -- ★ 幂等键：采购审批实例 code
    approval_code          TEXT NOT NULL DEFAULT '',
    applicant_user_id      TEXT NOT NULL DEFAULT '',
    purchase_status        TEXT NOT NULL DEFAULT '',
    project_group          TEXT NOT NULL DEFAULT '',
    mirror_record_id       TEXT NOT NULL DEFAULT '',  -- 镜像表（tbllUFPS…）行 id
    ledger_record_ids      TEXT NOT NULL DEFAULT '',  -- JSON 数组：写进收支表的行 id
    invoice_instance_code  TEXT NOT NULL DEFAULT '',  -- 代建出来的 27发票收集 实例 code
    draft_state            TEXT NOT NULL DEFAULT '',  -- awaiting_applicant / submitted / failed
    last_error             TEXT NOT NULL DEFAULT '',
    created_at             TEXT NOT NULL,
    updated_at             TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_purchase_invoice ON purchase_sync(invoice_instance_code);
