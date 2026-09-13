-- 0001_init.sql —— 核心表：提交、证据、审计。
--
-- 设计要点：
--  1. 金额一律以【分】存 INTEGER（禁浮点）；
--  2. 用 STRICT 表在 schema 层禁止类型混用；
--  3. ★ 证据表的 sha256 建【唯一索引】—— 这是"防重复报销"在数据库层的保证，
--     也是本项目存在的第一理由（飞书多维表格没有任何唯一索引能力）。
--  4. 审计表 append-only，用触发器拦截 UPDATE/DELETE。

PRAGMA foreign_keys = ON;

-- ── 审批实例（一行一单）──────────────────────────────────────
CREATE TABLE IF NOT EXISTS submission (
    instance_code   TEXT PRIMARY KEY,          -- 审批实例 code，天然唯一
    approval_code   TEXT NOT NULL DEFAULT '',
    approval_name   TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT '',   -- 审批中/已通过/已拒绝/…
    applicant       TEXT NOT NULL DEFAULT '',
    applicant_dept  TEXT NOT NULL DEFAULT '',
    material_type   TEXT NOT NULL DEFAULT '',
    material_name   TEXT NOT NULL DEFAULT '',
    buyer           TEXT NOT NULL DEFAULT '',
    fund_source     TEXT NOT NULL DEFAULT '',
    amount_cent     INTEGER,                    -- 图读价税合计（分）
    tax_cent        INTEGER,                    -- 图读税额（分）
    invoice_date    TEXT,                       -- YYYY-MM-DD
    seller          TEXT NOT NULL DEFAULT '',
    verdict         TEXT NOT NULL DEFAULT '',   -- 一致/存疑/缺件
    first_seen_at   TEXT NOT NULL,
    updated_at      TEXT NOT NULL
) STRICT;

-- ── 证据（一行一图 / PDF 一页）────────────────────────────────
CREATE TABLE IF NOT EXISTS evidence (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_code         TEXT NOT NULL REFERENCES submission(instance_code) ON DELETE CASCADE,
    slot                  TEXT NOT NULL,        -- 发票文件/订单截图/付款截图
    kind                  TEXT NOT NULL DEFAULT '',
    index_no              INTEGER NOT NULL DEFAULT 1,
    filename              TEXT NOT NULL DEFAULT '',
    media_type            TEXT NOT NULL DEFAULT '',
    sha256                TEXT NOT NULL,        -- ★ 原图指纹
    size_bytes            INTEGER NOT NULL DEFAULT 0,
    amount_incl_tax_cent  INTEGER,
    tax_cent              INTEGER,
    amount_excl_tax_cent  INTEGER,
    amount_upper          TEXT NOT NULL DEFAULT '',
    date                  TEXT,
    counterparty          TEXT NOT NULL DEFAULT '',
    provider              TEXT NOT NULL DEFAULT '',
    model                 TEXT NOT NULL DEFAULT '',
    trace_id              TEXT NOT NULL DEFAULT '',
    confidence            REAL,
    upper_check           TEXT NOT NULL DEFAULT '',
    tax_check             TEXT NOT NULL DEFAULT '',
    created_at            TEXT NOT NULL
) STRICT;

-- ★★★ 核心约束：同一张图（sha256）只能进库一次。
-- 这条索引是"防重复报销"的数据库级保证：重复提交会直接被拒，不依赖任何上层检查。
CREATE UNIQUE INDEX IF NOT EXISTS uq_evidence_sha256 ON evidence(sha256);

CREATE INDEX IF NOT EXISTS idx_evidence_instance ON evidence(instance_code);

-- ── 维度字典：部门 open_id → 名称（兜底用，可累积）──────────────
CREATE TABLE IF NOT EXISTS dept_dict (
    open_dept_id TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    learned_at   TEXT NOT NULL
) STRICT;

-- ── 审计：append-only ────────────────────────────────────────
CREATE TABLE IF NOT EXISTS audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          TEXT NOT NULL,
    actor       TEXT NOT NULL DEFAULT 'system',
    entity      TEXT NOT NULL,
    entity_id   TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    row_hash    TEXT NOT NULL DEFAULT '',
    prev_hash   TEXT NOT NULL DEFAULT ''
) STRICT;

-- 触发器：禁止修改与删除审计记录（SQLite 原生支持）
CREATE TRIGGER IF NOT EXISTS trg_audit_no_update
BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE TRIGGER IF NOT EXISTS trg_audit_no_delete
BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;
