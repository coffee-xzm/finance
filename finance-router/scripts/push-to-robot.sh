#!/bin/bash
# shellcheck shell=bash
# push-to-robot.sh —— 从**开发机**把当前版本直接推到机器人（走局域网，不经过 GitHub）
#
# 为什么需要：机器人到 github.com 的链路不稳（实测出现
#   `RPC 失败。curl 16 Error in the HTTP2 framing layer`
#   以及 `Failed to connect to github.com port 443 after 132496 ms`）。
# 拉一次 18MB 二进制要两三分钟，还常常失败。
# 走局域网传 git bundle 只要一两秒，且不受外网影响。
#
# 用法（在开发机上，仓库根目录）：
#   finance-router/scripts/push-to-robot.sh              # 构建 + 提交 + 推送 + 部署
#   finance-router/scripts/push-to-robot.sh --no-commit  # 只用已提交的内容
#   ROBOT=wdr@192.168.1.3 finance-router/scripts/push-to-robot.sh
set -euo pipefail

ROBOT="${ROBOT:-wdr@192.168.1.3}"
REPO_DIR="${ROBOT_REPO:-/home/wdr/finance}"
BRANCH="${BRANCH:-main}"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO"

# 是否需要密码由 ssh/scp 自己决定；有 sshpass 就提示用法
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10)

echo "→ 目标: $ROBOT:$REPO_DIR  (分支 $BRANCH)"

# ── 1. 工作区必须干净：bundle 只能反映已提交的内容 ──────────
if [ -n "$(git status --porcelain)" ]; then
    echo "✗ 工作区有未提交改动，先提交（或用 --no-commit 只推已有提交）：" >&2
    git status --short >&2
    exit 1
fi

REV="$(git rev-parse --short HEAD)"
echo "→ 将推送提交 $REV"

# ── 2. 打 bundle 并传过去 ──────────────────────────────────
TMP_BUNDLE="$(mktemp -u /tmp/finance-XXXXXX.bundle)"
git bundle create "$TMP_BUNDLE" "$BRANCH" >/dev/null
echo "→ bundle $(du -h "$TMP_BUNDLE" | cut -f1)"
trap 'rm -f "$TMP_BUNDLE"' EXIT

scp "${SSH_OPTS[@]}" "$TMP_BUNDLE" "$ROBOT:/tmp/finance-push.bundle"
echo "→ 已传输"

# ── 3. 目标机取新历史并重置 ────────────────────────────────
# 用 fetch+reset 而不是 pull：历史可能被重写过，ff-only 会失败。
ssh "${SSH_OPTS[@]}" "$ROBOT" bash -s <<REMOTE
set -e
cd "$REPO_DIR"
git fetch --force /tmp/finance-push.bundle "refs/heads/$BRANCH:refs/remotes/origin/$BRANCH" 2>&1 | tail -2
git reset --hard "refs/remotes/origin/$BRANCH"
git config core.hooksPath .githooks
rm -f /tmp/finance-push.bundle
echo "→ 目标机 HEAD: \$(git rev-parse --short HEAD)"
REMOTE

# ── 4. 重启并健康检查 ──────────────────────────────────────
echo "→ 重启服务"
ssh "${SSH_OPTS[@]}" "$ROBOT" "$REPO_DIR/finance-router/scripts/serve-ctl.sh restart"
sleep 3
ssh "${SSH_OPTS[@]}" "$ROBOT" "$REPO_DIR/finance-router/scripts/serve-ctl.sh status"

echo
echo "✓ 推送完成: $REV"
