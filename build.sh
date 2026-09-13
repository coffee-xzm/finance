#!/bin/bash
# build.sh —— finance 服务的启动入口（被机器人上的 roboware_start.sh 调用）
#
# 历史问题（2026-09-13 修复）：
#   1. 原来**没有 shebang**。roboware_start.sh 用 `nohup $cmd` 拉起它，
#      没有 shebang 时内核返回 ENOEXEC，调用方回退成 `sh`（dash）执行，
#      于是 `source` 报 "not found" —— devenv.sh 从来没生效过。
#   2. 原来在这里 `go build`。但目标机（192.168.1.3，Ubuntu 22.04）**没装 Go**，
#      必然失败；而且失败是静默的：`go build` 报错后脚本继续跑，
#      于是永远运行的是**上一次留在磁盘上的旧二进制** —— 看起来"热更新没生效"。
#      bin/serve 本来就随 git 一起发布，目标机不需要编译能力。
#   3. 原来直接 `./bin/serve` 前台运行，没有 pidfile，停不掉也查不到。
#
# 现在：启动/停止统一走 serve-ctl.sh（pidfile 版），这里只做转发。
#
# 用法：
#   ./build.sh            # 等价于 serve-ctl.sh start（已在跑且版本一致则不动）
#   ./build.sh restart    # 强制重启
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$HERE/finance-router/scripts/serve-ctl.sh" "${1:-start}"
