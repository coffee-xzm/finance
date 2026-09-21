-- 0009_flow_register.sql —— 「27-流水登记」这一步的留痕（2026-09-21 新流程）
--
-- 新流程（用户 2026-09-21 定）：
--   采购审批通过 → 写「27 - 收支表」 → **每条明细**给财务登记人开一张预填好的
--   「27-流水登记」并退回到发起（同时私信通知） → 登记单通过后用它的数据覆盖那一行
--   → 全部明细都登记完成后，才给采购提交人开「27发票收集」。
--
-- 为什么要本地记：登记单 ↔ 流水行 ↔ 采购 是三方映射，
-- 光靠飞书表里的「流水审批ID」列能反查（那是幂等锚点），但本地记一份才能
--   ① 判断"这一笔采购的所有明细是否都已经登记完成"（决定何时开票）；
--   ② 事件重放 / 补漏扫描时不必重新解析整个审批表单。
CREATE TABLE IF NOT EXISTS flow_register (
    instance_code          TEXT PRIMARY KEY,          -- ★ 幂等键：登记单实例 code
    purchase_instance_code TEXT NOT NULL DEFAULT '',  -- 来源采购（自建登记单为空）
    ledger_record_id       TEXT NOT NULL DEFAULT '',  -- 对应的「27 - 收支表」行 id
    item_index             INTEGER NOT NULL DEFAULT 0,-- 第几条费用明细（从 1 开始；自建为 0）
    state                  TEXT NOT NULL DEFAULT '',  -- awaiting_applicant / applied / failed
    last_error             TEXT NOT NULL DEFAULT '',
    created_at             TEXT NOT NULL,
    updated_at             TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_flow_register_purchase ON flow_register(purchase_instance_code);
CREATE INDEX IF NOT EXISTS idx_flow_register_ledger ON flow_register(ledger_record_id);

-- purchase_sync 上再记一份"这张采购带出了哪几张登记单"（与 ledger_record_ids 同序）。
ALTER TABLE purchase_sync ADD COLUMN flow_register_codes TEXT NOT NULL DEFAULT '';
