#!/bin/bash
# bundle-for-replit.sh —— 打出「只含源码、可直接上传到 Replit」的 zip。
#
# 排除：node_modules（71MB）、dist/dist-test（构建产物）、.npm-cache（154MB）、本地 .npmrc
# 包含：.replit（Replit 运行配置）、源码、测试、冻结向量、README
#
# 用法：bash bitable-plugin/scripts/bundle-for-replit.sh [输出目录]
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${1:-$HERE/../.scratch/dist}"
mkdir -p "$OUT_DIR"

STAMP="$(date +%Y%m%d-%H%M)"
OUT="$OUT_DIR/bitable-plugin-$STAMP.zip"

cd "$HERE"
zip -r -q "$OUT" \
  .replit \
  index.html \
  package.json \
  package-lock.json \
  vite.config.ts \
  tsconfig.json \
  tsconfig.core.json \
  src \
  testdata \
  README.md \
  scripts \
  -x '*/node_modules/*' -x '*/dist/*' -x '*/dist-test/*' -x '*/.npm-cache/*' -x '*/.npmrc'

echo "已生成：$OUT"
echo "大小：$(du -h "$OUT" | cut -f1)"
echo
echo "里面包含的文件："
unzip -l "$OUT" | tail -n +4 | head -n -2 | awk '{print "  " $4}'
