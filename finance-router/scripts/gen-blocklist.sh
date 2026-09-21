#!/bin/bash
# shellcheck shell=bash
# gen-blocklist.sh —— 从 config.yml 抽取"不该进 git 的真实值"，写入 .git/secret-blocklist
#
# 为什么用生成而不是写死正则：正则只能覆盖**形态**（UUID、tbl 前缀、sk- 前缀…），
# 而 8 位 user_id、20 位发票号这类没有独特前缀的值很容易漏。
# 2026-09-13 的教训：pre-commit 的正则没覆盖 UUID / 短码 / 长数字，
# 结果真实值又被写进了文档，直到远程复验才发现。
#
# 黑名单文件放在 .git/ 下，**不进 git**（也永远不会被提交）。
# pre-commit 每次提交前自动调用本脚本刷新。
set -euo pipefail
BASE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${1:-$BASE/config.yml}"
OUT="$(git -C "$BASE" rev-parse --git-dir)/secret-blocklist"

[ -f "$CONFIG" ] || { echo "· 无 $CONFIG，跳过" >&2; exit 0; }

python3 - "$CONFIG" "$OUT" <<'PY'
import re, sys

cfg_path, out_path = sys.argv[1], sys.argv[2]
text = open(cfg_path, encoding='utf-8').read()

# 这些是公开值/通用值，命中它们会造成误报
#
# ★ 2026-09-21 修：原来还有个 has_cjk 分支，把 config.yml 里**所有含中文的值**
#   都当"真实值"拉黑。结果是 `通过 / 待办 / 驳回 / 技术组物资 / 项目组物资 / -采购审批`
#   这类**业务词汇**也被拉黑，而源码里必然出现它们（HEAD 里 55 个文件含"通过"），
#   于是任何一次正常提交都会被拒（实测：只暂存 cmd/serve/catchup.go 就被拦）。
#   现在只保留"标识符型"判定（含数字的短标识 / 长 token），它们才是真正的租户标识符
#   （审批 code、app_token、表 id、部门 id、user_id、app_id…）。
#   若某个**中文**值确实敏感（例如某个专有表单名），请手工写进 `.git/secret-extra`
#   —— 那个文件只在本机 .git/ 下，永不入库。
ALLOW = {
    'siliconflow-qwen-vl', 'Qwen/Qwen3-VL-32B-Instruct', 'pdftoppm',
    'json_schema', 'data/finance.db', 'data/tmp', 'backup',
    'cli_9cb844403dbb9108',   # 飞书审批小程序平台常量（官方文档公开）
    'cli_9c7cc8a9a9edd105',   # 同上，Lark 品牌
}

vals = set()
for line in text.splitlines():
    line = line.split('#')[0]
    m = re.match(r'\s*[a-z_]+:\s*"(.*)"\s*$', line)
    if not m:
        continue
    v = m.group(1).strip()
    if not v or v in ALLOW or v.startswith('http'):
        continue
    ident_like = re.fullmatch(r'[A-Za-z0-9_.-]{6,}', v) and any(c.isdigit() for c in v)
    long_token = re.fullmatch(r'[A-Za-z0-9_.\-]{16,}', v)
    if ident_like or long_token:
        vals.add(v)

with open(out_path, 'w', encoding='utf-8') as f:
    for v in sorted(vals):
        f.write(v + '\n')
print(f"✓ 黑名单 {len(vals)} 条 → {out_path}")
PY
