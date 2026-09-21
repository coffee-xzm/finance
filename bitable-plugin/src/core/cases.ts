/**
 * 命名／分组向量的**用例定义**（不含期望值）。
 *
 * 期望值冻结在 `bitable-plugin/testdata/naming-cases.json`，由 gen-vectors 生成、由 naming.test 校验。
 * 拆成独立文件的原因是：测试只依赖冻结的 JSON（不依赖生成逻辑），避免"用实现证明实现"。
 */
import type { NamingOptions, RowInput } from './naming.js';

export interface VectorCase {
  /** 用例名（中文，便于人读）。 */
  name: string;
  /** 这条用例想钉住的不变量。 */
  why: string;
  options?: NamingOptions;
  rows: RowInput[];
}

const att = (
  index: number,
  token: string,
  originalName: string,
  mime: string,
  size: number,
) => ({ index, token, originalName, mime, size });

/**
 * 用例清单。数据全部是**编造的**：发票号/公司名/金额/人名都不是真实票据。
 */
export const CASES: VectorCase[] = [
  {
    name: '标准一行三件套',
    why: '最常见形态：发票 + 订单 + 付款，各有附件；按部门分目录。',
    rows: [
      {
        recordId: 'recA1',
        instanceNo: 'INS-0001',
        fields: {
          department: '视觉组',
          buyer: '张某某',
          date: '2026-06-02 12:30',
          seller: '某某科技有限公司',
          amount: 48.9,
          invoiceNo: '24912000000012345678',
        },
        attachments: {
          invoice: [att(1, 'boxinv1', 'invoice.pdf', 'application/pdf', 102400)],
          order: [att(1, 'boxord1', 'image.png', 'image/png', 204800)],
          payment: [att(1, 'boxpay1', 'image.png', 'image/png', 307200)],
        },
      },
    ],
  },
  {
    name: '一槽多附件（序号只在多附件时加）',
    why: '同一槽位有 2 个附件时，模板里的 {序号} 必须出现且为 2 位；单附件时不加序号。',
    rows: [
      {
        recordId: 'recA2',
        fields: {
          department: '电控组',
          buyer: '李某某',
          date: '2026-07-15',
          seller: '某某电子商行',
          amount: '1,234.5',
          invoiceNo: 'INV20260715000099',
        },
        attachments: {
          invoice: [
            att(1, 'boxinv2a', 'a.pdf', 'application/pdf', 1000),
            att(2, 'boxinv2b', 'b.pdf', 'application/pdf', 2000),
          ],
          order: [att(1, 'boxord2', 'order.jpg', 'image/jpeg', 3000)],
        },
      },
    ],
  },
  {
    name: '字段缺失用占位符',
    why: '缺部门/销方/金额时不得产出空片段（会出现 -- 或连续分隔符），统一用「未知」。',
    rows: [
      {
        recordId: 'recA3',
        fields: {
          buyer: '王某某',
          date: '2026-08-01',
          invoiceNo: '',
        },
        attachments: {
          invoice: [att(1, 'boxinv3', 'scan.pdf', 'application/pdf', 500)],
        },
      },
    ],
  },
  {
    name: '非法字符被清洗',
    why: '字段里带 / \\ : * ? " < > | 与换行时，文件名必须仍可被 Windows 解压。',
    rows: [
      {
        recordId: 'recA4',
        fields: {
          department: '视觉组/硬件组',
          buyer: '赵某:某',
          date: '2026-08-02',
          seller: '某某*公司<测试>|分部',
          amount: '¥88.00',
          invoiceNo: '24912000000000000001',
        },
        attachments: {
          invoice: [att(1, 'boxinv4', 'x.pdf', 'application/pdf', 700)],
          payment: [att(1, 'boxpay4', 'y.png', 'image/png', 800)],
        },
      },
    ],
  },
  {
    name: '文件名撞车追加 ~2',
    why: '同目录同名（同日同额同人不同票）时必须可区分，且不覆盖。',
    rows: [
      {
        recordId: 'recA5',
        fields: {
          department: '机械组',
          buyer: '孙某某',
          date: '2026-08-03',
          seller: '某某五金',
          amount: 100,
          invoiceNo: 'AAAAAA111111',
        },
        attachments: {
          invoice: [att(1, 'boxinv5', 'a.pdf', 'application/pdf', 100)],
        },
      },
      {
        recordId: 'recA6',
        fields: {
          department: '机械组',
          buyer: '孙某某',
          date: '2026-08-03',
          seller: '某某五金',
          amount: 100,
          invoiceNo: 'BBBBBB222222',
        },
        attachments: {
          invoice: [att(1, 'boxinv6', 'b.pdf', 'application/pdf', 100)],
        },
      },
    ],
  },
  {
    name: '整行无附件只报警告不产文件',
    why: '清单里勾了但没有附件的行，必须显式告警，不能静默少文件。',
    rows: [
      {
        recordId: 'recA7',
        fields: { department: '梯队', buyer: '周某某', date: '2026-08-04' },
        attachments: {},
      },
    ],
  },
  {
    name: '超长名称按字节截断且保住扩展名',
    why: '中文 3 字节/字，长公司名必须截断到字节上限内，且 .pdf 不能被截掉。',
    options: { maxNameBytes: 60 },
    rows: [
      {
        recordId: 'recA8',
        fields: {
          department: '无人机组',
          buyer: '某某某某某某某某某某某某某某某某某某',
          date: '2026-08-05',
          seller:
            '某某某某某某某某某某某某某某某某某某某某某某某某某某某某某某某某科技有限公司',
          amount: 99999.99,
          invoiceNo: '24912000000000000002',
        },
        attachments: {
          invoice: [att(1, 'boxinv8', 'long.pdf', 'application/pdf', 12345)],
        },
      },
    ],
  },
  {
    name: '按月份分组（groupBy=month）',
    why: '月度报销场景：目录是 YYYY-MM，且不受部门名影响。',
    options: { groupBy: 'month' },
    rows: [
      {
        recordId: 'recA9',
        fields: {
          department: '视觉组',
          buyer: '张某某',
          date: '2026-09-09 08:00',
          seller: '某某科技',
          amount: 1,
          invoiceNo: '24912000000000000003',
        },
        attachments: {
          invoice: [att(1, 'boxinv9', 'a.pdf', 'application/pdf', 10)],
        },
      },
    ],
  },
  {
    name: '不分组（groupBy=none）',
    why: 'zip 根目录直接放文件，path 里不应出现 /。',
    options: { groupBy: 'none' },
    rows: [
      {
        recordId: 'recA10',
        fields: {
          department: '视觉组',
          buyer: '张某某',
          date: '2026-09-10',
          seller: '某某科技',
          amount: 2,
          invoiceNo: '24912000000000000004',
        },
        attachments: {
          invoice: [att(1, 'boxinv10', 'a.pdf', 'application/pdf', 10)],
        },
      },
    ],
  },
  {
    name: '自定义模板与发票号后6位',
    why: '模板变量 {发票号后6位} 必须取末 6 位；未知变量原样保留便于发现写错。',
    options: {
      template: '{日期}_{部门}_{发票号后6位}_{未知变量}',
      groupBy: 'none',
    },
    rows: [
      {
        recordId: 'recA11',
        fields: {
          department: '视觉组',
          buyer: '张某某',
          date: '2026-09-11',
          seller: '某某科技',
          amount: 3,
          invoiceNo: '24912000000012345678',
        },
        attachments: {
          invoice: [att(1, 'boxinv11', 'a.pdf', 'application/pdf', 10)],
        },
      },
    ],
  },
  {
    name: 'mime 缺失时用原文件名推扩展名',
    why: '附件没有 mime 时不能丢扩展名。',
    options: { groupBy: 'none' },
    rows: [
      {
        recordId: 'recA12',
        fields: {
          department: '视觉组',
          buyer: '张某某',
          date: '2026-09-12',
          seller: '某某科技',
          amount: 4,
          invoiceNo: '24912000000000000005',
        },
        attachments: {
          invoice: [att(1, 'boxinv12', 'photo.JPEG', '', 10)],
        },
      },
    ],
  },
];
