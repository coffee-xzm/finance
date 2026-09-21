/**
 * naming —— 「记录 → 落盘文件名」的**可移植核心**。
 *
 * 为什么单独抽出来：
 *   插件（浏览器 TS）和后面的服务端导出（Go）必须对同一批记录算出**逐字相同**的文件名与目录，
 *   否则「插件导的」和「服务端导的」会变成两套口径，对不上账。
 *   所以这里只有纯函数：不碰 SDK、不碰网络、不碰 DOM。
 *
 * 冻结口径见 .scratch/plugin-probe/... 与 docs/30-review/34：任何行为变更都必须同时更新
 * `testdata/naming-cases.json` 的期望值，并有 Go 侧的同名实现一起过同一份向量。
 */

// ── 槽位 ─────────────────────────────────────────────────────────
/** 附件槽位。顺序即 zip 内同名时的序号顺序。 */
export type Slot = 'invoice' | 'order' | 'payment';

/** 槽位中文名（模板变量 {槽位} 用它）。 */
export const SLOT_LABEL: Record<Slot, string> = {
  invoice: '发票',
  order: '订单',
  payment: '付款',
};

/** 用户的三个附件字段名（与多维表格列名一致，见 finance-router/internal/bitable/schema.go）。 */
export const SLOT_FIELD: Record<Slot, string> = {
  invoice: '发票',
  order: '订单截图',
  payment: '付款记录',
};

// ── 模板 ─────────────────────────────────────────────────────────
/** 一行记录里可供命名的字段（都已在多维表格里）。 */
export interface NamingFields {
  /** 物资所属部门（多选，取第一个）。 */
  department?: string;
  /** 购买人。 */
  buyer?: string;
  /** 图读日期，形如 2026-06-02 或 2026-06-02 12:00。 */
  date?: string;
  /** 销方名称。 */
  seller?: string;
  /** 图读金额（元），字符串或数字。 */
  amount?: string | number;
  /** 发票号码（仅发票行有）。 */
  invoiceNo?: string;
}

export interface NamingOptions {
  /**
   * 命名模板。可用变量：
   *   {部门} {购买人} {日期} {销方} {金额} {槽位} {序号} {发票号后6位} {发票号码}
   * 默认见 DEFAULT_TEMPLATE。
   */
  template?: string;
  /** 分组：按部门分文件夹 / 按月（取图读日期）/ 不分。 */
  groupBy?: 'department' | 'month' | 'none';
  /** 单个文件名（含扩展名）的字节上限，默认 120 —— Windows 解压的安全值。 */
  maxNameBytes?: number;
  /** 字段缺省时显示的占位符，默认「未知」。 */
  placeholder?: string;
}

export const DEFAULT_TEMPLATE =
  '{部门}-{购买人}-{日期}-{销方}-{金额}元-{槽位}{序号}';

// ── 清洗 ─────────────────────────────────────────────────────────
/** Windows / POSIX 都不接受的文件名字符，外加控制字符。 */
// eslint-disable-next-line no-control-regex
const ILLEGAL = /[\\/:*?"<>|\u0000-\u001f\u007f]/g;

/**
 * slug：把字段值洗成**文件名安全**的片段。
 *
 * 规则（逐条对应 docs/30-review/32 §3.5 的硬规则 1）：
 *   1. 去首尾空白、折叠内部连续空白为一个（中文文件名里空格越多越容易出错）
 *   2. 删除非法字符与路径分隔符，杜绝 `../../x` 这类路径穿越
 *   3. 合并因删除产生的连续分隔符（- 与 _）
 *   4. 去首尾的 . - _ 与空格（Windows 不允许文件名以 . 或空格结尾）
 */
export function slug(raw: string | number | undefined | null): string {
  if (raw === undefined || raw === null) return '';
  let s = String(raw).normalize('NFC');
  s = s.replace(ILLEGAL, ' ');
  s = s.replace(/\s+/g, ' ').trim();
  // 合并因删除产生的连续分隔符（---- → -）
  s = s.replace(/[-_]{2,}/g, (m: string) => m.charAt(0));
  // 去首尾的 . - _ 与空格（Windows 不允许文件名以 . 或空格结尾）
  s = s.replace(/^[.\-_ ]+/, '').replace(/[.\-_ ]+$/, '');
  return s;
}

/** 日期只取 `YYYY-MM-DD`（图读日期可能带时间）。 */
export function datePart(raw: string | undefined | null): string {
  const s = slug(raw);
  // slug 会把 / 与 . 洗成空格，所以分隔符要同时接受「- / . 空格」
  const m = /^(\d{4})[-/. ](\d{1,2})[-/. ](\d{1,2})/.exec(s);
  if (!m) return s; // 解析不出来就原样用（已被 slug 洗过）
  return `${m[1]}-${pad2(m[2] ?? '')}-${pad2(m[3] ?? '')}`;
}

/** 金额：剥掉 ¥ 与千分位，保留两位小数；非数字则原样返回 slug。 */
export function amountText(raw: string | number | undefined | null): string {
  if (raw === undefined || raw === null || raw === '') return '';
  const s = slug(raw).replace(/[¥￥,，\s]/g, '');
  const n = Number(s);
  if (Number.isFinite(n)) return n.toFixed(2);
  const m = s.match(/-?\d+(\.\d+)?/);
  if (m) return Number(m[0]).toFixed(2);
  return s;
}

/** 发票号后 6 位；不足 6 位则原样。 */
export function invoiceTail(invoiceNo: string | undefined | null): string {
  const s = slug(invoiceNo);
  if (!s) return '';
  return s.length > 6 ? s.slice(-6) : s;
}

function pad2(s: string): string {
  return s.length === 1 ? `0${s}` : s;
}

/** 把 UTF-8 字符串按**字节**截断到 limit（不切坏多字节字符）。 */
export function truncateBytes(s: string, limit: number): string {
  const enc = new TextEncoder();
  if (enc.encode(s).length <= limit) return s;
  let out = '';
  for (const ch of s) {
    if (enc.encode(out + ch).length > limit) break;
    out += ch;
  }
  return out;
}

/** 文件名字节上限内**保住扩展名**地截断。 */
export function truncateName(name: string, maxBytes: number): string {
  const enc = new TextEncoder();
  if (enc.encode(name).length <= maxBytes) return name;
  const dot = name.lastIndexOf('.');
  const ext = dot > 0 ? name.slice(dot) : '';
  const stem = dot > 0 ? name.slice(0, dot) : name;
  const room = Math.max(1, maxBytes - enc.encode(ext).length);
  return truncateBytes(stem, room).replace(/[.\-_ ]+$/, '') + ext;
}

// ── 模板渲染 ─────────────────────────────────────────────────────
const VAR_ALIAS: Record<string, keyof NamingFields | 'slot' | 'seq'> = {
  部门: 'department',
  购买人: 'buyer',
  日期: 'date',
  销方: 'seller',
  金额: 'amount',
  槽位: 'slot',
  序号: 'seq',
  发票号码: 'invoiceNo',
};

function renderTemplate(
  template: string,
  vars: Record<string, string>,
): string {
  return template.replace(/\{([^}]+)\}/g, (whole: string, key: string) => {
    const k = key.trim();
    if (k === '发票号后6位') return vars['发票号码后6位'] ?? '';
    const hit = vars[k];
    if (hit !== undefined) return hit;
    // 未知变量：原样保留，便于人肉发现模板写错（不静默吞掉）
    return whole;
  });
}

/**
 * 生成文件名主体（不含扩展名），并保证**总字节数**（含扩展名）不超限。
 *
 * 为什么不是"先拼再截"：中文 3 字节/字，长公司名一截就把「谁/多少钱」这些关键信息切没了
 * （实测会出现 `某某某某……-2.pdf` 这种读不出是谁的名字）。
 * 所以按**分部件**处理：逐级降低每部分预算；某一级已经装不下时，直接把长值截到预算内。
 *
 * 返回空串表示这一行没有任何可用信息（调用方用「未知」）。
 *
 * @param templateToRender 传 DEFAULT_TEMPLATE 表示按部件拼装；传别的（=用户自定义）则走模板渲染，
 *   两者都受字节上限约束。
 */
function fitStem(
  parts: string[],
  maxNameBytes: number,
  ext: string,
  customTemplate: string,
  vars: Record<string, string>,
): string {
  const enc = new TextEncoder();
  const budget = Math.max(8, maxNameBytes - enc.encode(ext).length);
  const sep = '-';

  // 自定义模板：行为可预期（完全按模板），只做整体截断
  if (customTemplate !== DEFAULT_TEMPLATE) {
    const rendered = slug(renderTemplate(customTemplate, vars));
    return truncateBytes(rendered, budget).replace(/[.\-_ ]+$/, '') || 'unnamed';
  }

  const clean = parts.map((p) => p.trim()).filter(Boolean);
  if (clean.length === 0) return '';

  // 逐级**均匀**收紧每个部件的预算，直到装得下。
  // 为什么不"先拼再截"：那样会把末尾的类型/金额整段切掉，而长公司名反而保留 —— 实测会出现
  // `某某某某……-2.pdf` 这种"读得出公司、读不出是谁/什么"的名字。
  // 均匀收紧保证每个部件都留一点信息（谁-何时-哪家-多少-什么）。
  for (const per of [64, 48, 32, 24, 16, 12, 8, 6, 4]) {
    const joined = clean.map((p) => truncateBytes(p, per)).join(sep);
    if (enc.encode(joined).length <= budget) return joined;
  }
  return truncateBytes(clean.map((p) => truncateBytes(p, 4)).join(sep), budget);
}

// ── 主入口 ───────────────────────────────────────────────────────
/** 一条待下载的附件（来自 cell 的 IOpenAttachment）。 */
export interface AttachmentRef {
  /** 附件在单元格里的序号，从 1 开始 —— 同一 (记录, 槽位) 多附件时用它区分。 */
  index: number;
  /** 飞书附件 token（不是下载 URL；URL 只有效 10 分钟，见 docs/30-review/32 §4.1 F7）。 */
  token: string;
  /** 原始文件名（多半不可读，如 image.png）。 */
  originalName?: string;
  /** mime，如 image/jpeg、application/pdf。 */
  mime?: string;
  /** 字节数。 */
  size?: number;
}

export interface RowInput {
  /** 多维表格 record_id。 */
  recordId: string;
  /** 审批实例号（可空）。 */
  instanceNo?: string;
  fields: NamingFields;
  /** 每个槽位的附件列表；缺省 = 该槽无附件。 */
  attachments: Partial<Record<Slot, AttachmentRef[]>>;
}

export interface PlannedFile {
  /** 仅文件名（含扩展名）。 */
  name: string;
  /** 仅目录（无分组时为空串）。 */
  dir: string;
  /** 目录+文件名，用于日志/报错。zip 里请分别用 dir 与 name。 */
  path: string;
  /** 同目录内的显示序号，从 1 开始（不参与排序语义，仅给人看）。 */
  ordinal: number;
  slot: Slot;
  recordId: string;
  instanceNo?: string;
  token: string;
  originalName: string;
  mime: string;
  /** 附件声明的字节数（来自单元格元数据；真实字节数以下载到的 Blob 为准）。 */
  declaredSize: number;
}

export interface PlanWarning {
  recordId: string;
  /** 机器可读的原因码。 */
  code: 'EMPTY_ROW' | 'NO_ATTACHMENTS' | 'MISSING_FIELD' | 'COLLISION' | 'RENAMED';
  message: string;
}

export interface PlanResult {
  files: PlannedFile[];
  warnings: PlanWarning[];
  /** 每个槽位实际下载的文件数，用于「空附件行」统计与对账。 */
  counts: Record<Slot, number>;
  /** 附件总字节数。 */
  totalBytes: number;
}

const SLOT_ORDER: Slot[] = ['invoice', 'order', 'payment'];

/** 扩展名优先级：mime → 原始文件名 → 无。 */
export function extOf(att: AttachmentRef): string {
  const byMime: Record<string, string> = {
    'application/pdf': '.pdf',
    'image/jpeg': '.jpg',
    'image/jpg': '.jpg',
    'image/png': '.png',
    'image/webp': '.webp',
    'image/gif': '.gif',
    'image/bmp': '.bmp',
    'image/heic': '.heic',
  };
  const mime = ((att.mime ?? '').toLowerCase().match(/^[^;]+/)?.[0] ?? '').trim();
  if (byMime[mime]) return byMime[mime];
  const orig = att.originalName ?? '';
  const dot = orig.lastIndexOf('.');
  if (dot > 0 && orig.length - dot <= 6) {
    const ext = orig.slice(dot).toLowerCase();
    if (/^\.[a-z0-9]+$/.test(ext)) return ext;
  }
  return '';
}

/** 原始文件名（用于 manifest 与「原文件名称」模式）。 */
function originalNameOf(att: AttachmentRef): string {
  const orig = slug(att.originalName ?? '');
  return orig || `attachment-${att.index}`;
}

/**
 * 把一批记录规划成「待下载文件清单」。
 *
 * 纯函数：同样的输入必然得到同样的输出；不做任何网络/磁盘操作。
 * 幂等：同一批记录重复规划，路径完全一致（便于「跳过已下载」）。
 */
export function planRows(
  rows: RowInput[],
  opts: NamingOptions = {},
): PlanResult {
  const userTemplate = opts.template?.trim();
  const template = userTemplate || DEFAULT_TEMPLATE;
  const groupBy = opts.groupBy ?? 'department';
  const maxNameBytes = opts.maxNameBytes ?? 120;
  const placeholder = opts.placeholder ?? '未知';

  const files: PlannedFile[] = [];
  const warnings: PlanWarning[] = [];
  const counts: Record<Slot, number> = { invoice: 0, order: 0, payment: 0 };
  const used = new Set<string>();
  // 目录里已有的维度不在文件名里重复（视觉组/视觉组-张… → 视觉组/张…）
  const effectiveTemplate = adaptTemplate(template, groupBy);
  let totalBytes = 0;

  for (const row of rows) {
    const present = SLOT_ORDER.filter((s) => (row.attachments[s]?.length ?? 0) > 0);
    if (present.length === 0) {
      warnings.push({
        recordId: row.recordId,
        code: 'NO_ATTACHMENTS',
        message: '这条记录的三个附件列都是空的',
      });
      continue;
    }

    const missing: string[] = [];
    const val = (k: keyof NamingFields): string => {
      const raw = row.fields?.[k];
      let v = '';
      if (k === 'date') v = datePart(raw as string);
      else if (k === 'amount') v = amountText(raw as string | number);
      else v = slug(raw as string);
      if (!v) {
        missing.push(FIELD_LABEL[k] ?? String(k));
        return placeholder;
      }
      return v;
    };

    const department = val('department');
    const buyer = val('buyer');
    const date = val('date');
    const seller = val('seller');
    const amount = val('amount');
    const invoiceNo = slug(row.fields?.invoiceNo);
    const invoiceTailText = invoiceTail(invoiceNo) || placeholder;
    let ordinal = 0;

    for (const slot of SLOT_ORDER) {
      const list = row.attachments[slot] ?? [];
      const multi = list.length > 1;
      for (const att of list) {
        ordinal += 1;
        const seq = String(att.index).padStart(2, '0');
        const baseVars: Record<string, string> = {
          部门: department,
          购买人: buyer,
          日期: date,
          销方: seller,
          金额: amount,
          槽位: SLOT_LABEL[slot],
          序号: multi ? seq : '',
          发票号码: invoiceNo || placeholder,
          发票号码后6位: invoiceTailText,
        };
        // 目录里已经有的维度（部门 / 月份）不再进文件名
        const parts = [
          ...(groupBy === 'department' ? [] : [department]),
          buyer,
          ...(groupBy === 'month' ? [] : [date]),
          seller,
          `${amount}元`,
          SLOT_LABEL[slot] + (multi ? seq : ''),
        ];
        const ext = extOf(att);
        // ⚠ 判断「是否自定义模板」必须用**用户给的原文**，不能用已剥掉部门前缀的 effectiveTemplate——
        // 否则默认模板会被误判成自定义，走回模板渲染并把部门又加回文件名（实测踩过）。
        const stem =
          fitStem(
            parts,
            maxNameBytes,
            ext,
            userTemplate ? effectiveTemplate : DEFAULT_TEMPLATE,
            baseVars,
          ) || 'unnamed';
        let name = truncateName(stem + ext, maxNameBytes);
        const dir = groupDir(groupBy, department, date);

        // 同目录内重名 → 追加 ~2 ~3（要同时改 stem，再重新截断）
        let attempt = 1;
        let key = `${dir}\u0000${name}`;
        while (used.has(key)) {
          attempt += 1;
          name = truncateName(`${stem}~${attempt}${ext}`, maxNameBytes);
          key = `${dir}\u0000${name}`;
        }
        used.add(key);

        if (attempt > 1) {
          warnings.push({
            recordId: row.recordId,
            code: 'COLLISION',
            message: `文件名撞车，已改为 ${name}`,
          });
        }

        files.push({
          name,
          dir,
          path: dir ? `${dir}/${name}` : name,
          ordinal,
          slot,
          recordId: row.recordId,
          ...(row.instanceNo ? { instanceNo: row.instanceNo } : {}),
          token: att.token,
          originalName: originalNameOf(att),
          mime: att.mime ?? '',
          declaredSize: att.size ?? 0,
        });
        counts[slot] += 1;
        totalBytes += att.size ?? 0;
      }
    }

    // 发票行缺订单/付款不是错误（老师垫付时本来就只有发票），所以只在**部分缺失**时提示
    if (missing.length > 0 && present.includes('invoice')) {
      warnings.push({
        recordId: row.recordId,
        code: 'MISSING_FIELD',
        message: `命名所需字段缺失，已用「${placeholder}」占位：${missing.join('、')}`,
      });
    }
  }

  return { files, warnings, counts, totalBytes };
}

/** 字段中文名（告警里给人看）。 */
export const FIELD_LABEL: Record<string, string> = {
  department: '物资所属部门',
  buyer: '购买人',
  date: '图读日期',
  seller: '销方名称',
  amount: '图读金额',
  invoiceNo: '发票号码',
};

/** 分组目录；不分组的返回空串（zip 根目录）。 */
export function groupDir(
  groupBy: 'department' | 'month' | 'none',
  department: string,
  isoDate: string,
): string {
  if (groupBy === 'none') return '';
  if (groupBy === 'department') return slug(department);
  const m = /^(\d{4})-(\d{2})/.exec(isoDate);
  return m ? `${m[1]}-${m[2]}` : '未知月份';
}

/**
 * 智能去重：目录里已经有的维度，就不在文件名里再写一遍。
 *   groupBy=department → 从模板去掉开头的 `{部门}-`
 *   groupBy=month      → 从模板去掉开头的 `{日期}-`
 * 只动开头的那一处，其余位置（例如 `{日期}_{部门}_…`）原样保留。
 */
export function adaptTemplate(
  template: string,
  groupBy: 'department' | 'month' | 'none',
): string {
  if (groupBy === 'department') return template.replace(/^\s*\{部门\}\s*[-_]\s*/, '');
  if (groupBy === 'month') return template.replace(/^\s*\{日期\}\s*[-_]\s*/, '');
  return template;
}

