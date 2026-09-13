-- 0003_local_paths.sql —— 证据表记录预处理图的本地路径。
--
-- 为什么需要：归档到"整合表"时**不能复用 file_token**（官方限制，跨表无效），
-- 必须从本地重新上传。而原来归档是去读 manifest.jsonl 找路径 ——
-- 但 manifest 每次运行被整体重写，**只剩最后一批实例**，
-- 于是早先处理的实例归档时"没有图"。
--
-- 把路径落到库里，归档就能按实例号查到任意历史批次的图。

ALTER TABLE evidence ADD COLUMN local_png TEXT NOT NULL DEFAULT '';
