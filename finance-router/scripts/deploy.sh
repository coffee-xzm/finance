#!/bin/bash
# shellcheck shell=bash
# deploy.sh —— 从 git 拉取新版本并热更新 finance 服务（一条命令）
#
# 设计前提（对着 192.168.1.3 的实际情况定的）：
#
#   1. **不在目标机上编译**。机器人上没装 Go，`go build` 必然失败；
#      而 bin/serve 本来就随 git 一起走。所以部署 = 拉代码 + 原子替换二进制 + 重启。
#   2. **原子替换**。git / mv 都用 rename(2)，不会碰到 ETXTBSY（"Text file busy"）；
#      用 cp 覆盖一个正在运行的可执行文件则会失败。
#   3. **重启靠 pidfile，不靠进程名**。见 serve-ctl.sh 顶部说明。
#   4. **失败要回滚**。所以必须在 git pull **之前**先把旧二进制留一份 ——
#      拉完再备份拿到的已经是新的，回滚就没有意义了。
#
# 用法：
#   scripts/deploy.sh                  # 拉取 main + 部署 + 健康检查
#   scripts/deploy.sh --no-pull        # 不拉取，只用当前工作区的二进制
#   scripts/deploy.sh --branch dev     # 换分支
set -uo pipefail

BASE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="$(cd "$BASE/.." && pwd)"
BRANCH="main"
DO_PULL=1

while [ $# -gt 0 ]; do
    case "$1" in
        --no-pull) DO_PULL=0; shift ;;
        --branch)  BRANCH="${2:-}"; shift 2 ;;
        -h|--help) sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "未知参数: $1" >&2; exit 1 ;;
    esac
done

CTL="$BASE/scripts/serve-ctl.sh"
BIN="$BASE/bin/serve"
PREV="$BASE/bin/.serve.prev"
[ -x "$CTL" ] || { echo "✗ 找不到 $CTL" >&2; exit 1; }

cd "$REPO" || exit 1

# ── 0. 先留旧二进制（回滚用）────────────────────────────────
HAS_PREV=0
if [ -f "$BIN" ]; then
    cp -p "$BIN" "$PREV" && HAS_PREV=1
    echo "· 旧二进制已备份（$( "$BIN" -version 2>/dev/null | awk '{print $2}' )）"
fi

rollback() {
    if [ "$HAS_PREV" = 1 ] && [ -f "$PREV" ]; then
        echo "→ 回滚到上一版" >&2
        mv -f "$PREV" "$BIN"
        "$CTL" restart || echo "✗ 回滚后仍起不来，需要人工介入" >&2
    else
        echo "（没有可回滚的旧版本）" >&2
    fi
}

# ── 1. 拉取 ────────────────────────────────────────────────
if [ "$DO_PULL" = 1 ]; then
    echo "→ git fetch origin $BRANCH"
    git fetch origin "$BRANCH" || { echo "✗ fetch 失败（网络？凭据？）" >&2; exit 1; }

    BEFORE="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
    # ff-only：部署机不该产生 merge commit；有本地改动就报错，
    # 避免本地改动被静默覆盖（部署机上唯一该有的本地文件是 config.yml，它被 gitignore）
    if ! git merge --ff-only "origin/$BRANCH"; then
        echo "✗ 无法快进合并 —— 仓库可能有本地改动，先看：" >&2
        echo "    git -C $REPO status" >&2
        exit 1
    fi
    AFTER="$(git rev-parse --short HEAD)"
    if [ "$BEFORE" = "$AFTER" ]; then
        echo "· 代码已是最新（$AFTER）"
    else
        echo "✓ 代码更新 $BEFORE → $AFTER"
        git --no-pager log --oneline "$BEFORE..$AFTER" | sed 's/^/    /'
    fi
fi

# ── 2. 二进制就位 ──────────────────────────────────────────
[ -f "$BIN" ] || { echo "✗ 仓库里没有 $BIN" >&2; rollback; exit 1; }
chmod +x "$BIN"
NEW_VER="$( "$BIN" -version 2>/dev/null | awk '{print $2}' )"
echo "→ 待部署版本: ${NEW_VER:-未知}"

# ── 3. 重启 ────────────────────────────────────────────────
echo "→ 重启服务"
if ! "$CTL" restart; then
    echo "✗ 重启失败" >&2
    rollback
    exit 1
fi

# ── 4. 健康检查 ────────────────────────────────────────────
echo "→ 健康检查（等 3 秒确认进程没有立刻退出）"
sleep 3
if "$CTL" status; then
    echo
    echo "✓ 部署完成：$( "$BIN" -version )"
    rm -f "$PREV"
else
    echo "✗ 服务没能稳定运行" >&2
    rollback
    exit 1
fi
