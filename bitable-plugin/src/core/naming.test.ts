/**
 * 命名／分组契约测试。
 *
 * 校验对象是**冻结的向量文件**（testdata/naming-cases.json），不是本文件的期望值 ——
 * 这样"实现"和"契约"是两份独立的东西，改实现而不同步契约会立刻红。
 *
 * 服务端（Go）将来跑同一份 JSON，所以这里等于在替两边守门。
 *
 * 用法：npm test
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  planRows,
  slug,
  truncateName,
  amountText,
  datePart,
  extOf,
  type PlanResult,
} from './naming.js';

interface FrozenCase {
  name: string;
  why: string;
  options?: Parameters<typeof planRows>[1];
  rows: Parameters<typeof planRows>[0];
  expected: PlanResult;
}

// 只依赖本文件位置（dist-test/core/ → 项目根），不依赖 cwd —— node --test 会让 cwd 变成被测文件所在目录
const vectorsPath = join(import.meta.dirname, '..', '..', 'testdata', 'naming-cases.json');
const { cases } = JSON.parse(readFileSync(vectorsPath, 'utf8')) as {
  version: number;
  cases: FrozenCase[];
};

test('向量文件非空且覆盖多条用例', () => {
  assert.ok(cases.length >= 10, `用例数 ${cases.length}，少于 10 条`);
});

for (const c of cases) {
  test(`向量 · ${c.name}`, () => {
    const got = planRows(c.rows, c.options);
    assert.deepEqual(
      got,
      c.expected,
      `与冻结向量不一致（${c.why}）\n实际：${JSON.stringify(got, null, 2)}`,
    );
  });
}

// ── 纯函数级的不变量（向量之外再钉一遍边界）──────────────────────

test('slug 删除路径穿越与非法字符', () => {
  assert.equal(slug('../../etc/passwd'), 'etc passwd');
  assert.equal(slug('a/b\\c:d*e?f"g<h>i|j'), 'a b c d e f g h i j');
  assert.equal(slug('  多   空   格  '), '多 空 格');
  assert.equal(slug('..hidden..'), 'hidden');
  assert.equal(slug('-_-'), '');
});

test('slug 不以点或空格结尾（Windows 不允许）', () => {
  assert.ok(!slug('名字.').endsWith('.'));
  assert.ok(!slug('名字 ').endsWith(' '));
});

test('日期只取年月日', () => {
  assert.equal(datePart('2026-06-02 12:30'), '2026-06-02');
  assert.equal(datePart('2026/6/2'), '2026-06-02');
  assert.equal(datePart(''), '');
});

test('金额统一两位小数并剥离货币符号', () => {
  assert.equal(amountText(48.9), '48.90');
  assert.equal(amountText('¥1,234.5'), '1234.50');
  assert.equal(amountText('0'), '0.00');
  assert.equal(amountText('abc'), 'abc');
});

test('截断按字节且不切断多字节字符', () => {
  const long = '某某某某某某某某某某某某某某某某某某某某'; // 30 汉字 = 90 字节
  const cut = truncateName(long + '.pdf', 30);
  assert.ok(new TextEncoder().encode(cut).length <= 30);
  assert.ok(cut.endsWith('.pdf'), `扩展名被截掉：${cut}`);
});

test('扩展名优先级：mime 优先于原文件名', () => {
  assert.equal(
    extOf({ index: 1, token: 't', originalName: 'a.png', mime: 'application/pdf' }),
    '.pdf',
  );
  assert.equal(
    extOf({ index: 1, token: 't', originalName: 'a.JPEG', mime: '' }),
    '.jpeg',
  );
  assert.equal(extOf({ index: 1, token: 't', originalName: 'noext', mime: '' }), '');
});

test('单附件时不产生序号，多附件时序号为两位', () => {
  const base = {
    recordId: 'r',
    fields: {
      department: '视觉组',
      buyer: '张某某',
      date: '2026-06-02',
      seller: '某某科技',
      amount: 1,
      invoiceNo: 'INV00000001',
    },
  };
  const one = planRows([{ ...base, attachments: { invoice: [{ index: 1, token: 't1' }] } }]);
  const two = planRows([
    {
      ...base,
      attachments: {
        invoice: [
          { index: 1, token: 't1' },
          { index: 2, token: 't2' },
        ],
      },
    },
  ]);
  assert.equal(one.files.length, 1);
  assert.ok(!/发票\d/.test(one.files[0]!.name), `单附件不该有序号：${one.files[0]!.name}`);
  assert.equal(two.files.length, 2);
  assert.ok(two.files[1]!.name.includes('发票02'), `多附件应有 02：${two.files[1]!.name}`);
});

test('同一批输入重复规划结果逐字一致（幂等）', () => {
  const rows = cases[4]!.rows; // 撞车用例
  const a = planRows(rows);
  const b = planRows(rows);
  assert.deepEqual(a, b);
  assert.deepEqual(
    a.files.map((f) => `${f.dir}/${f.name}`),
    b.files.map((f) => `${f.dir}/${f.name}`),
  );
});

test('zip 内路径唯一（不存在两个文件同路径）', () => {
  for (const c of cases) {
    const got = planRows(c.rows, c.options);
    const paths = got.files.map((f) => `${f.dir}/${f.name}`);
    assert.equal(new Set(paths).size, paths.length, `用例「${c.name}」出现重复路径`);
  }
});
