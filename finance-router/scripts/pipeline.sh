#!/usr/bin/env bash
# 手动跑完整链路：抽取 → 落库 → 通知 → 归档。
#
# 用法：
#   ./scripts/pipeline.sh              # 跑 5 个实例
#   ./scripts/pipeline.sh 10           # 跑 10 个实例
#   ./scripts/pipeline.sh 5 --reset    # 清空本地库后重跑（演示"首次入库"）
#
# 只读侦察（不发消息、不写表）：
#   go run ./cmd/recon -from 2026-09-01 -to 2026-09-13
#
# 注意：这是**手动流水线**，不是常驻服务。
# 常驻服务（订阅审批事件自动触发）尚未实现。
set -euo pipefail

LIMIT="${1:-5}"
cd "$(dirname "$0")/.."

# --reset：清空本地库，从头演示（不影响飞书里的数据）
if [[ "${2:-}" == "--reset" || "${1:-}" == "--reset" ]]; then
  echo "⚠ --reset：将删除本地库 data/finance.db*（飞书里的数据不动）"
  rm -f ../data/finance.db* data/finance.db*
  LIMIT="${LIMIT/--reset/}"; LIMIT="${LIMIT:-5}"
fi
source ./devenv.sh >/dev/null

step() { printf '\n\033[1m══ %s ══\033[0m\n' "$1"; }

step "1/5 权限体检"
go run ./cmd/doctor -wiki-token <WIKI_NODE_TOKEN> 2>&1 | tail -3

step "2/5 初始化本地库"
go run ./cmd/db -init

step "3/5 抽取 ${LIMIT} 个实例（下载 → 转PNG → 识别 → 入库）"
go run ./cmd/extract -limit "${LIMIT}" 2>&1 | tail -6

step "4/5 落到多维表格"
go run ./cmd/sync 2>&1 | tail -6

step "5/5 通知需人工的行"
go run ./cmd/notify 2>&1 | tail -6

step "本地库统计"
go run ./cmd/db -stats

cat <<'TIP'

────────────────────────────────────────────────
接下来是**人工环节**（在飞书里做）：

  1. 打开源表，看「核对结果」列
  2. 重点看「存疑 / 缺件」的行（这些会发消息通知你）
  3. 核对无误后，把「人工审核」改成「通过」
  4. 然后跑归档：

       go run ./cmd/archive

源表:   https://<TENANT>.feishu.cn/base/<BITABLE_APP_TOKEN>?table=<TABLE_SUBMISSION>
整合表: https://<TENANT>.feishu.cn/base/<BITABLE_APP_TOKEN>?table=<TABLE_INTEGRATED>
────────────────────────────────────────────────
TIP
