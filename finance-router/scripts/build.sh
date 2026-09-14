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
#
# 版本 = 基础提交 + **源码内容哈希** + 构建时间。
#
# 为什么要源码哈希：二进制是在提交**之前**构建的（构建 → 提交 → 推送），
# 所以 GITREV 永远比实际包含它的那个提交**晚一代**，光看短哈希会误判。
# 源码哈希只取决于 .go/.sql/go.mod 的内容，提交前后**完全一样** ——
# 拿它比对就能确认"机器人跑的这个二进制，是不是当前这份源码构建的"。
# 时间戳则保证每次构建的版本号唯一。
GITREV="$(git describe --tags --always 2>/dev/null || echo nogit)"
BUILDTIME="$(date -Iseconds)"
SRCHASH="$({
    git ls-files -z -- '*.go' '*.sql' 'go.mod' 'go.sum' 2>/dev/null |
        sort -z | xargs -0 -r cat
} | sha256sum | cut -c1-8)"
VERSION="${GITREV}-src${SRCHASH}-$(date '+%Y%m%dT%H%M%S')"

# 本机 ~/.cache 只读，必须走工作区内的缓存
if [ -f ./devenv.sh ]; then
    # shellcheck disable=SC1091
    source ./devenv.sh
fi

go build -ldflags "-X main.version=${VERSION} -X main.buildTime=${BUILDTIME}" \
    -o bin/serve ./cmd/serve

echo "✓ bin/serve  version=${VERSION}  built=${BUILDTIME}"
echo "  大小: $(du -h bin/serve | cut -f1)"
