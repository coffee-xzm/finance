#!/bin/bash
# shellcheck shell=bash
# build.sh —— 本机（有 Go 的机器）构建入库二进制 bin/serve
#
# 为什么要单独一个脚本：版本信息必须打进二进制，否则
# 「机器人上跑的到底是哪一版」就说不清，热更新等于盲发。
# 版本取自 git，没有 git 时退回时间戳。
set -euo pipefail
BASE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$BASE"

# 不用 --dirty：二进制文件本身是构建产物、提交时必然处于"未提交"状态，
# 加 --dirty 会让每个版本都带上 dirty 后缀，反而分不清真实状态。
# 用「基础提交 + 构建时间」组合：短哈希给可追溯性，时间戳保证唯一性。
GITREV="$(git describe --tags --always 2>/dev/null || echo nogit)"
BUILDTIME="$(date -Iseconds)"
VERSION="${GITREV}-$(date '+%Y%m%dT%H%M%S')"

# 本机 ~/.cache 只读，必须走工作区内的缓存
if [ -f ./devenv.sh ]; then
    # shellcheck disable=SC1091
    source ./devenv.sh
fi

go build -ldflags "-X main.version=${VERSION} -X main.buildTime=${BUILDTIME}" \
    -o bin/serve ./cmd/serve

echo "✓ bin/serve  version=${VERSION}  built=${BUILDTIME}"
echo "  大小: $(du -h bin/serve | cut -f1)"
