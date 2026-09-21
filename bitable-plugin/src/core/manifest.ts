/**
 * manifest.csv —— 导出批次的对账底账（浏览器内生成，随 zip 一起给出去）。
 *
 * 为什么必须带 UTF-8 BOM：中文财务场景 Excel 打开无 BOM 的 CSV 是乱码。
 * 为什么列里留 token 与原始文件名：将来要跟「多维表格里的那一行」对回去，
 * 光靠重命名后的文件名是认不出来的（重名/改名都会发生）。
 */
import type { PlannedFile } from './naming.js';
import { SLOT_LABEL } from './naming.js';

/** CSV 单元格转义：包含 , " 换行 时用双引号包裹，内部引号翻倍。 */
export function csvCell(v: string | number | undefined | null): string {
  const s = v === undefined || v === null ? '' : String(v);
  if (/[",\n\r]/.test(s)) return `"${s.replace(/"/g, '""')}"`;
  return s;
}

export interface ManifestRow {
  plan: PlannedFile;
  /** 已下载字节数；未下载（失败）时留空。 */
  bytes?: number;
  /** 下载失败原因；成功时留空。 */
  error?: string;
}

export const MANIFEST_HEADER = [
  'zip内路径',
  '文件名',
  '槽位',
  '记录ID',
  '审批实例号',
  '原始文件名',
  '附件token',
  'MIME',
  '字节数',
  '失败原因',
] as const;

/** 生成 manifest.csv 文本（不含 BOM；BOM 由 csvBytes 加）。 */
export function buildManifest(rows: ManifestRow[]): string {
  const lines = [MANIFEST_HEADER.join(',')];
  for (const r of rows) {
    lines.push(
      [
        csvCell(r.plan.path),
        csvCell(r.plan.name),
        csvCell(SLOT_LABEL[r.plan.slot]),
        csvCell(r.plan.recordId),
        csvCell(r.plan.instanceNo),
        csvCell(r.plan.originalName),
        csvCell(r.plan.token),
        csvCell(r.plan.mime),
        csvCell(r.bytes ?? ''),
        csvCell(r.error),
      ].join(','),
    );
  }
  return lines.join('\r\n') + '\r\n';
}

/** 批次摘要文本（放在 zip 里，人一眼能看懂这次导了什么）。 */
export function buildReadme(info: {
  exportedAt: string;
  tableName: string;
  viewName: string;
  template: string;
  groupBy: string;
  rows: number;
  files: number;
  failures: number;
  operator?: string;
}): string {
  return [
    '附件批量导出（飞书多维表格 · 侧边栏插件）',
    '='.repeat(40),
    `导出时间：${info.exportedAt}`,
    `数据表：${info.tableName}`,
    `视图：${info.viewName}`,
    `导出记录数：${info.rows}`,
    `文件数：${info.files}`,
    `失败数：${info.failures}`,
    `命名模板：${info.template}`,
    `分组方式：${info.groupBy}`,
    info.operator ? `操作人（Bitable 用户标识）：${info.operator}` : '',
    '',
    '说明：',
    '· 文件按「物资所属部门」分目录（可在插件里改成按月或不分组）。',
    '· manifest.csv 记录了每个文件对应的记录 ID 与附件 token，便于与多维表格对回去。',
    '· 本文件由插件在浏览器本地生成，未经过任何中转服务器。',
    '',
  ]
    .filter((l) => l !== '')
    .join('\r\n');
}
