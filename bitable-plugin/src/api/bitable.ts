/**
 * 与飞书多维表格宿主交互的全部代码都收在这里。
 *
 * 隔离原因：SDK 的 API 面很大，且宿主通信是 postMessage 握手（SDK 内部完成）。
 * 其余模块（core / zip / pipeline）只依赖本文件导出的**窄接口**，不直接 import SDK，
 * 这样核心逻辑可以在 node 里跑测试，不必进浏览器。
 */
import {
  bitable,
  FieldType,
  type IOpenAttachment,
  type IRecordValue,
} from '@lark-base-open/js-sdk';
import type { AttachmentRef, NamingFields, RowInput, Slot } from '../core/naming.js';
import { SLOT_FIELD } from '../core/naming.js';

/** 插件在宿主里能看到的表。 */
export interface TableOption {
  id: string;
  name: string;
}

export interface ViewOption {
  id: string;
  name: string;
}

/** 一张表里三个附件槽位的 fieldId。 */
export type SlotFields = Partial<Record<Slot, string>>;

/** 用户勾选的一条记录（已把 SDK 值翻译成核心层的形态）。 */
export interface SelectedRecord extends RowInput {
  /** 记录在视图里的显示位置（从 1 开始），仅用于界面序号。 */
  viewIndex: number;
}

/** 列出当前多维表格里的所有数据表。 */
export async function listTables(): Promise<TableOption[]> {
  const metas = await bitable.base.getTableMetaList();
  return metas.map((m) => ({ id: m.id, name: m.name }));
}

/** 列出某张表里的所有视图。 */
export async function listViews(tableId: string): Promise<ViewOption[]> {
  const table = await bitable.base.getTableById(tableId);
  const metas = await table.getViewMetaList();
  return metas.map((m) => ({ id: m.id, name: m.name }));
}

/** 当前宿主打开的表/视图（用于默认选中）。 */
export async function activeSelection(): Promise<{ tableId?: string; viewId?: string }> {
  try {
    const sel = await bitable.base.getSelection();
    const out: { tableId?: string; viewId?: string } = {};
    if (sel?.tableId) out.tableId = sel.tableId;
    if (sel?.viewId) out.viewId = sel.viewId;
    return out;
  } catch {
    return {};
  }
}

/**
 * 找出三个附件槽位对应的 fieldId。
 *
 * 只在**附件类型**的字段里按名字找（与 finance-router 的教训一致：
 * 按名字找会被名字里含关键词的单选控件抢走，必须先按类型过滤）。
 * 槽位名已随业务改名过一次（发票文件→发票、付款截图→付款记录），所以每个槽位给多个候选名。
 */
export async function findSlotFields(tableId: string): Promise<SlotFields> {
  const fields = await listAttachmentFields(tableId);
  const byName = new Map(fields.map((f) => [f.name, f.id]));
  const out: SlotFields = {};
  for (const [slot, wanted] of Object.entries(SLOT_FIELD) as [Slot, string][]) {
    const hit = byName.get(wanted);
    if (hit) out[slot] = hit;
  }
  return out;
}

/** 表里全部附件字段（供用户手改槽位映射）。 */
export async function listAttachmentFields(
  tableId: string,
): Promise<{ id: string; name: string }[]> {
  const table = await bitable.base.getTableById(tableId);
  const metas = await table.getFieldMetaList();
  return metas
    .filter((m) => m.type === FieldType.Attachment)
    .map((m) => ({ id: m.id, name: m.name ?? '' }));
}

/** 取某表某视图的可见记录 id（按视图顺序）。 */
export async function visibleRecordIds(tableId: string, viewId: string): Promise<string[]> {
  const table = await bitable.base.getTableById(tableId);
  const view = await table.getViewById(viewId);
  const ids = (await view.getVisibleRecordIdList()) ?? [];
  return ids.filter((x): x is string => typeof x === 'string' && x.length > 0);
}

/** 解析一条记录的字段值（含附件 token 与权限三元组）。 */
export interface RecordAttachments {
  recordId: string;
  /** 槽位 → 附件引用（已带 index/token/name/mime/size 与高级权限所需信息）。 */
  bySlot: Partial<Record<Slot, AttachmentRef[]>>;
}

export interface AttachmentExtra {
  /** 高级权限文档下获取下载链接必须带的三个 id。 */
  tableId: string;
  recordId: string;
  fieldId: string;
}

/** 与 core 层解耦：核心层只认 AttachmentRef，额外信息由调用方单独保存。 */
export function toAttachmentRefs(cellValue: unknown): AttachmentRef[] {
  const list = Array.isArray(cellValue) ? (cellValue as IOpenAttachment[]) : [];
  const out: AttachmentRef[] = [];
  let i = 0;
  for (const a of list) {
    if (!a || typeof a !== 'object') continue;
    const token = (a as IOpenAttachment).token;
    if (!token) continue;
    i += 1;
    const ref: AttachmentRef = { index: i, token };
    if ((a as IOpenAttachment).name) ref.originalName = (a as IOpenAttachment).name;
    if ((a as IOpenAttachment).type) ref.mime = (a as IOpenAttachment).type;
    if (typeof (a as IOpenAttachment).size === 'number') ref.size = (a as IOpenAttachment).size;
    out.push(ref);
  }
  return out;
}

/** 读记录的原始值（不做业务解释）。 */
export async function readRecords(
  tableId: string,
  recordIds: string[],
): Promise<IRecordValue[]> {
  const table = await bitable.base.getTableById(tableId);
  const out: IRecordValue[] = [];
  // 单次上限 1000，分批以防勾选过多
  for (let i = 0; i < recordIds.length; i += 500) {
    const chunk = recordIds.slice(i, i + 500);
    const got = await table.getRecordsByIds(chunk);
    out.push(...(got ?? []));
  }
  return out;
}

/** 从单元格值里取「人可读文本」（多选取第一个，公式取结果）。 */
export function cellText(v: unknown): string {
  if (v === null || v === undefined) return '';
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  if (Array.isArray(v)) {
    for (const item of v) {
      const t = cellText(item);
      if (t) return t;
    }
    return '';
  }
  if (typeof v === 'object') {
    const o = v as Record<string, unknown>;
    for (const k of ['text', 'name', 'value', 'en_name']) {
      const t = cellText(o[k]);
      if (t) return t;
    }
  }
  return '';
}

/** 数值字段取值；取不到返回 undefined（让核心层去决定占位符）。 */
export function cellNumber(v: unknown): number | undefined {
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  const t = cellText(v).replace(/[¥￥,，\s]/g, '');
  if (!t) return undefined;
  const n = Number(t);
  return Number.isFinite(n) ? n : undefined;
}

/** 时间戳字段（毫秒或秒）→ YYYY-MM-DD。 */
export function cellDate(v: unknown): string | undefined {
  const raw = cellNumber(v);
  if (raw === undefined) {
    const t = cellText(v);
    return t || undefined;
  }
  const ms = raw > 1e12 ? raw : raw * 1000;
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return undefined;
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

/**
 * 把一条记录的字段值翻译成核心层的 NamingFields。
 *
 * 列名与 `finance-router/data/bitable/schema.md` 一致；某列不存在就留空（核心层用占位符）。
 */
export function toNamingFields(
  values: Record<string, unknown>,
  fieldIdByName: Map<string, string>,
): NamingFields {
  const pick = (name: string): unknown => {
    const fid = fieldIdByName.get(name);
    return fid ? values[fid] : undefined;
  };
  const f: NamingFields = {};
  const dept = cellText(pick('物资所属部门'));
  if (dept) f.department = dept;
  const buyer = cellText(pick('购买人'));
  if (buyer) f.buyer = buyer;
  const date = cellDate(pick('图读日期'));
  if (date) f.date = date;
  const seller = cellText(pick('销方名称'));
  if (seller) f.seller = seller;
  const amount = cellNumber(pick('图读金额(元)'));
  if (amount !== undefined) f.amount = amount;
  const inv = cellText(pick('发票号码'));
  if (inv) f.invoiceNo = inv;
  return f;
}

/** 取记录里的审批实例号（用于 manifest 对账）。 */
export function instanceNoOf(
  values: Record<string, unknown>,
  fieldIdByName: Map<string, string>,
): string | undefined {
  const fid = fieldIdByName.get('审批实例号');
  const t = fid ? cellText(values[fid]) : '';
  return t || undefined;
}

/** 建「字段名 → fieldId」映射（列名→id）。 */
export async function fieldIdMap(tableId: string): Promise<Map<string, string>> {
  const table = await bitable.base.getTableById(tableId);
  const metas = await table.getFieldMetaList();
  return new Map(metas.map((m) => [m.name ?? '', m.id]));
}

/** 单条的下载链接（10 分钟有效）。 */
export async function attachmentUrl(
  tableId: string,
  fieldId: string,
  recordId: string,
  token: string,
): Promise<string> {
  const table = await bitable.base.getTableById(tableId);
  return table.getAttachmentUrl(token, fieldId, recordId);
}

/** 批量的下载链接（一次给同一单元格的多个 token）。 */
export async function attachmentUrls(
  tableId: string,
  fieldId: string,
  recordId: string,
  tokens: string[],
): Promise<string[]> {
  const table = await bitable.base.getTableById(tableId);
  return table.getCellAttachmentUrls(tokens, fieldId, recordId);
}

/**
 * 按视图分页取**全部**记录 id。
 *
 * 为什么不用 `view.getVisibleRecordIdList()`：它一次只返回一页（SDK 侧上限 200），
 * 而 `table.getRecordsByPage({viewId, pageSize:200})` 自带 pageToken，翻页更省事，
 * 且我们本来就要用 `getRecordsByIds` 取记录值。
 */
export async function fetchRecordIds(tableId: string, viewId: string): Promise<string[]> {
  const table = await bitable.base.getTableById(tableId);
  const out: string[] = [];
  let pageToken: string | undefined;
  for (let page = 0; page < 200; page += 1) {
    const params: Record<string, unknown> = { pageSize: 200, viewId };
    if (pageToken) params.pageToken = pageToken;
    const res = (await table.getRecordsByPage(
      params as unknown as Parameters<typeof table.getRecordsByPage>[0],
    )) as {
      records?: { recordId?: string }[];
      hasMore?: boolean;
      pageToken?: string;
    };
    for (const r of res?.records ?? []) {
      if (r?.recordId) out.push(r.recordId);
    }
    if (!res?.hasMore || !res.pageToken) break;
    pageToken = res.pageToken;
  }
  return out;
}
