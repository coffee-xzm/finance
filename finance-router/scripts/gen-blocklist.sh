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
    has_cjk = any('\u4e00' <= c <= '\u9fff' for c in v)
    ident_like = re.fullmatch(r'[A-Za-z0-9_.-]{6,}', v) and any(c.isdigit() for c in v)
    long_token = re.fullmatch(r'[A-Za-z0-9_.\-]{16,}', v)
    if has_cjk or ident_like or long_token:
        vals.add(v)

with open(out_path, 'w', encoding='utf-8') as f:
    for v in sorted(vals):
        f.write(v + '\n')
print(f"✓ 黑名单 {len(vals)} 条 → {out_path}")
PY
