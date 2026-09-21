/**
 * 生成／回填命名向量文件：bitable-plugin/testdata/naming-cases.json
 *
 * ⚠ 只有在**有意**改变命名行为时才运行 `npm run vectors`，并且必须在同一提交里
 *   检查 diff 是否符合预期（这份文件是插件与 Go 服务端共用的行为契约）。
 *
 * 用法：npm run vectors
 */
import { planRows, type PlanResult } from '../naming.js';
import { CASES } from '../cases.js';
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

interface FrozenCase {
  name: string;
  why: string;
  options?: Parameters<typeof planRows>[1];
  rows: (typeof CASES)[number]['rows'];
  expected: PlanResult;
}

const out: FrozenCase[] = CASES.map((c) => ({
  name: c.name,
  why: c.why,
  ...(c.options ? { options: c.options } : {}),
  rows: c.rows,
  expected: planRows(c.rows, c.options),
}));

const here = dirname(fileURLToPath(import.meta.url));
const target = join(here, '..', '..', '..', 'testdata', 'naming-cases.json');
mkdirSync(dirname(target), { recursive: true });
writeFileSync(
  target,
  JSON.stringify({ version: 1, cases: out }, null, 2) + '\n',
  'utf8',
);
console.log(`已写入 ${target}（${out.length} 条用例）`);
