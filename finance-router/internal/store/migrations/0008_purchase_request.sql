-- 0008_purchase_request.sql —— 采购审批通过后写入 wiki「27采购申请表」的幂等留痕
--
-- 为什么要记：采购 APPROVED 事件可能重放；"写采购申请表"不是天然幂等的动作。
-- 记下已写出的 record_id，重跑时跳过（与 ledger_record_ids 同一思路）。

ALTER TABLE purchase_sync ADD COLUMN request_record_ids TEXT NOT NULL DEFAULT '';
