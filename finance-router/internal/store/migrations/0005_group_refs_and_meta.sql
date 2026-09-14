-- 0005_group_refs_and_meta.sql —— 让「一张发票一行」能真正落表
--
-- 背景（为什么非改不可）：
--
-- 1. doc_group 原本只记**槽位名**（order_slots = "订单截图,订单截图"）。
--    一单多票时，每条分组都写同一个槽位名，落表时**无法判断哪张订单截图属于哪一组** ——
--    结果是每一行都把该实例的全部订单截图挂上，图片张冠李戴。
--    改用 **slot:index**（槽位 + 该槽位内的序号，见 evidence.index_no）做精确归属。
--    保留原列，格式向后兼容：同步时遇到不含 ":" 的旧值按"整个槽位的全部图"处理。
--
-- 2. submission 少了一批表单元信息（申请编号/发起时间/归属组/是否支付宝/…）。
--    同步需要它们才能填满一行；以前靠 manifest.jsonl，但那个文件每次 extract 都被
--    os.Create 截断（常驻服务一次只处理一个实例），**留不住历史**。
--    本地库才是权威来源，所以把字段补到库里。

-- ── 分组的精确证据引用 ──────────────────────────────────────
ALTER TABLE doc_group ADD COLUMN invoice_ev   TEXT NOT NULL DEFAULT ''; -- "发票文件:1"
ALTER TABLE doc_group ADD COLUMN order_evs    TEXT NOT NULL DEFAULT ''; -- 逗号分隔的 slot:index
ALTER TABLE doc_group ADD COLUMN payment_evs  TEXT NOT NULL DEFAULT '';

-- 多页 PDF 会转出多张 PNG，原来只存了第 1 页（local_png）。同步要上传**全部**页，
-- 只留第 1 页会让多页发票在表里缺页。这里补一列存全部（JSON 数组）。
ALTER TABLE evidence ADD COLUMN local_pngs TEXT NOT NULL DEFAULT '';

-- ── 提交表的表单元信息 ──────────────────────────────────────
ALTER TABLE submission ADD COLUMN applink           TEXT    NOT NULL DEFAULT '';
ALTER TABLE submission ADD COLUMN start_time_ms     INTEGER;
ALTER TABLE submission ADD COLUMN applicant_dept_id TEXT    NOT NULL DEFAULT '';
ALTER TABLE submission ADD COLUMN departments_json  TEXT    NOT NULL DEFAULT ''; -- 归属组/物资所属部门（多选）
ALTER TABLE submission ADD COLUMN is_alipay         TEXT    NOT NULL DEFAULT ''; -- 是/否
ALTER TABLE submission ADD COLUMN dachuang          TEXT    NOT NULL DEFAULT ''; -- 是否走大创资金报销
ALTER TABLE submission ADD COLUMN remark            TEXT    NOT NULL DEFAULT '';
-- 形态不合规（违反成员提交规定）时记原因；非空 = 直接打回，不尝试匹配。
ALTER TABLE submission ADD COLUMN form_problem      TEXT    NOT NULL DEFAULT '';
-- 没配上任何分组的单据（进「报销核对」时标为待人工）。
ALTER TABLE submission ADD COLUMN leftover_json     TEXT    NOT NULL DEFAULT ''; -- JSON 数组
