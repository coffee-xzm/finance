#!/bin/bash
# check-vectors.sh —— 一条命令跑「插件（TS）↔ 服务端（Go）」的命名契约校验。
#
# 为什么需要：命名/分组逻辑有两份实现（浏览器要 TS、服务端要 Go），
# 唯一防止漂移的办法是两边跑**同一份冻结向量**（bitable-plugin/testdata/naming-cases.json）。
#
# 用法：bash bitable-plugin/scripts/check-vectors.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"

fail=0

echo "== 1/2 插件侧（node --test）=============================="
if (cd "$HERE" && npm test 2>&1 | tail -8); then
  echo "✅ 插件侧通过"
else
  echo "❌ 插件侧失败"
  fail=1
fi

echo
echo "== 2/2 服务端侧（go test ./internal/naming）=============="
# 本机 ~/.cache/go-build 与 ~/go/pkg/mod 可能是只读的；仓库里有可写的缓存目录。
export GOCACHE="${GOCACHE:-$ROOT/.gocache}"
export GOMODCACHE="${GOMODCACHE:-$ROOT/.gomodcache}"
if (cd "$ROOT/finance-router" && go test ./internal/naming/ 2>&1 | tail -12); then
  echo "✅ 服务端侧通过（与插件跑的是同一份向量）"
else
  echo "❌ 服务端侧失败：命名行为已与插件漂移，或向量未同步"
  echo "   提示：若是有意改行为，先在 bitable-plugin 里跑 npm run vectors 并 review diff"
  fail=1
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "全部通过：两侧命名契约一致。"
else
  echo "存在失败项（见上）。"
fi
exit "$fail"
