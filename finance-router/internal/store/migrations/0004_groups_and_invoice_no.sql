-- 0004_groups_and_invoice_no.sql —— 按发票号码防重复 + 分组（一张发票一行）
--
-- 为什么需要按**发票号码**去重，而不是只靠图片 sha256：
--   sha256 是**字节级**的。同一张发票重新拍一次、或换个格式导出，字节就变了，
--   sha256 完全不同 —— 重复提交照样能过。实测这确实是漏洞（拍照上传的场景）。
--   发票号码是税务机关给的唯一标识，与"这张图长什么样"无关，才是真正的键。
--
-- 但发票号码只有精确通道（文字层/二维码）能给。模型识别出的号码有读错风险，
-- 拿它做唯一约束会把"读错的号码"锁死。所以：
--   唯一索引**只对精确通道读出来的号码生效**（invoice_no_src = 'exact'），
--   模型读出来的只入库、不参与唯一性判定。

-- ── 证据表补三列（配对键与发票号码）────────────────────────
ALTER TABLE evidence ADD COLUMN invoice_no TEXT NOT NULL DEFAULT '';
ALTER TABLE evidence ADD COLUMN invoice_no_src TEXT NOT NULL DEFAULT ''; -- exact | model | ''
ALTER TABLE evidence ADD COLUMN order_no TEXT NOT NULL DEFAULT '';
ALTER TABLE evidence ADD COLUMN alipay_txn_id TEXT NOT NULL DEFAULT '';

-- ★ 真正的防重复：同一张发票号码只能进库一次（仅对精确通道生效）
CREATE UNIQUE INDEX IF NOT EXISTS uq_evidence_invoice_no
    ON evidence(invoice_no) WHERE invoice_no <> '' AND invoice_no_src = 'exact';

-- ── 分组：一张发票一行 ──────────────────────────────────────
-- 需求定的是「一张发票一行」，所以一张审批实例可以有 0..N 行。
-- 订单/付款用槽位名列表记录（一发票多订单时会有多个）。
CREATE TABLE IF NOT EXISTS doc_group (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_code       TEXT NOT NULL REFERENCES submission(instance_code) ON DELETE CASCADE,
    group_index         INTEGER NOT NULL,          -- 实例内序号，从 0 开始
    invoice_no          TEXT NOT NULL DEFAULT '',
    invoice_slot        TEXT NOT NULL DEFAULT '',
    order_slots         TEXT NOT NULL DEFAULT '',  -- 逗号分隔
    payment_slots       TEXT NOT NULL DEFAULT '',
    invoice_total_cent  INTEGER,
    support_total_cent  INTEGER,
    matched             INTEGER NOT NULL DEFAULT 0,
    reasons             TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_group ON doc_group(instance_code, group_index);
CREATE INDEX IF NOT EXISTS idx_doc_group_invoice ON doc_group(invoice_no);
