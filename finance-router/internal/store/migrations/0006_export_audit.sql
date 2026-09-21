-- 0006_export_audit.sql —— 「选择性下载」的导出审计底账
--
-- 需求来源（docs/00-brief/01-requirements-brief.md）：
--   「导出发票并保存导出记录」「信息包含：操作人、操作时间、操作事项」（高优先级）。
-- 插件（bitable-plugin/）把文件落在操作者电脑上，**拿不到这份底账** ——
-- 这正是服务端导出路线存在的理由（docs/30-review/34 §4.4）。
--
-- 两条纪律：
--   1. 只存 **file_token**，绝不存临时下载链接（24h 失效，见计划 R1）；
--   2. 清单本身不搬进库，只存**内容 sha256** —— 既能证明"照哪份清单导的"，又不留名单。

CREATE TABLE IF NOT EXISTS export_batch (
  batch_id     TEXT PRIMARY KEY,
  scope        TEXT    NOT NULL,              -- list | view
  table_name   TEXT    NOT NULL DEFAULT '',
  table_id     TEXT    NOT NULL DEFAULT '',
  view_id      TEXT    NOT NULL DEFAULT '',
  list_sha256  TEXT    NOT NULL DEFAULT '',   -- 清单内容哈希（不是清单原文）
  list_lines   INTEGER NOT NULL DEFAULT 0,
  fields       TEXT    NOT NULL DEFAULT '',   -- 槽位
  naming       TEXT    NOT NULL DEFAULT '',
  group_by     TEXT    NOT NULL DEFAULT '',
  operator     TEXT    NOT NULL DEFAULT '',
  started_at   TEXT    NOT NULL,
  finished_at  TEXT    NOT NULL DEFAULT '',
  records      INTEGER NOT NULL DEFAULT 0,
  success      INTEGER NOT NULL DEFAULT 0,
  failed       INTEGER NOT NULL DEFAULT 0,
  skipped      INTEGER NOT NULL DEFAULT 0,
  empty_rows   INTEGER NOT NULL DEFAULT 0,
  out_dir      TEXT    NOT NULL DEFAULT '',
  zip_path     TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE IF NOT EXISTS export_item (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  batch_id      TEXT    NOT NULL REFERENCES export_batch(batch_id),
  record_id     TEXT    NOT NULL DEFAULT '',
  instance_no   TEXT    NOT NULL DEFAULT '',
  slot          TEXT    NOT NULL DEFAULT '',
  file_token    TEXT    NOT NULL DEFAULT '',
  original_name TEXT    NOT NULL DEFAULT '',
  out_path      TEXT    NOT NULL DEFAULT '',
  sha256        TEXT    NOT NULL DEFAULT '',
  size_bytes    INTEGER NOT NULL DEFAULT 0,
  mime          TEXT    NOT NULL DEFAULT '',
  status        TEXT    NOT NULL,             -- ok | skipped | failed
  error         TEXT    NOT NULL DEFAULT '',
  created_at    TEXT    NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_export_item_batch ON export_item(batch_id);
CREATE INDEX IF NOT EXISTS idx_export_item_token ON export_item(file_token);
