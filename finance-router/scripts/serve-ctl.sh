#!/bin/bash
# serve-ctl.sh —— finance 常驻服务的启停控制（pidfile 版）
# shellcheck shell=bash
#
# 为什么需要它：机器人上的 roboware_start.sh 用
#     proc_name=$(basename "${cmd%% *}")  →  pkill -f "$proc_name"
# 来停进程。对 finance 那一条算出来的是 build.sh，而真正的进程叫 serve，
# **旧进程根本杀不掉**。一旦服务真的跑起来，每天 07:00 的定时重启就会再起一个，
# 而飞书长连接是"集群不广播"——事件会被随机分给两个实例，且没有任何报错。
#
# 所以停进程必须靠 pidfile，不能靠进程名匹配。
#
# 用法：
#   serve-ctl.sh start     确保在跑，且跑的是**磁盘上当前那一版**（版本变了会自动重启）
#   serve-ctl.sh stop      优雅停止（SIGTERM → 超时才 SIGKILL）
#   serve-ctl.sh restart   重启
#   serve-ctl.sh status    打印 PID / 运行版本 / 磁盘版本 / 是否一致
#   serve-ctl.sh logs      跟踪最新日志
set -uo pipefail

BASE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$BASE/bin/serve"
PIDFILE="${SERVE_PIDFILE:-$BASE/data/serve.pid}"
LOG_DIR="${SERVE_LOG_DIR:-$HOME/robo_logs/log6}"
STOP_TIMEOUT="${SERVE_STOP_TIMEOUT:-15}"
# 额外传给 serve 的参数，例如 SERVE_ARGS="-no-subscribe -dry-run"
SERVE_ARGS="${SERVE_ARGS:-}"

die() { echo "✗ $*" >&2; exit 1; }

running_pid() {
    [ -f "$PIDFILE" ] || return 1
    local pid
    pid="$(head -n1 "$PIDFILE" 2>/dev/null | tr -d '[:space:]')"
    [ -n "$pid" ] || return 1
    # 进程存在且确实是 serve（防 PID 复用）
    [ -d "/proc/$pid" ] || return 1
    tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null | grep -q serve || return 1
    echo "$pid"
}

running_version() { sed -n '2p' "$PIDFILE" 2>/dev/null | tr -d '[:space:]'; }
disk_version()    { "$BIN" -version 2>/dev/null | awk '{print $2}'; }

# 运行中的进程是否就是磁盘上那一版。
# 用 /proc/<pid>/exe 的 inode 比对：二进制被 mv 替换后，老进程仍指向旧 inode，
# 两者不同即说明该重启了。（mv 是原子的，也不会触发 ETXTBSY。）
same_binary() {
    local pid="$1"
    local a b
    a="$(stat -Lc '%d:%i' "/proc/$pid/exe" 2>/dev/null)" || return 1
    b="$(stat -Lc '%d:%i' "$BIN" 2>/dev/null)" || return 1
    [ "$a" = "$b" ]
}

do_start() {
    [ -x "$BIN" ] || die "找不到可执行文件 $BIN（先跑 deploy.sh 或 git pull）"

    if pid="$(running_pid)"; then
        if same_binary "$pid"; then
            echo "✓ 已在运行（PID $pid，版本 $(running_version)），无需启动"
            return 0
        fi
        echo "· 检测到二进制已更新（运行中 $(running_version) → 磁盘 $(disk_version)），自动重启"
        do_stop
    else
        # 清掉可能残留的陈旧 pidfile
        [ -f "$PIDFILE" ] && { echo "· 清理陈旧 pidfile"; rm -f "$PIDFILE"; }
    fi

    # 日志目录：默认跟机器人上 roboware_start.sh 的约定一致（~/robo_logs/log6）。
    # 建不出来就退回项目内 logs/ —— 磁盘满、HOME 只读、无权限时不该因为日志
    # 写不了就起不来服务。
    if ! mkdir -p "$LOG_DIR" 2>/dev/null; then
        echo "⚠ 日志目录 $LOG_DIR 不可写，退回 $BASE/logs"
        LOG_DIR="$BASE/logs"
    fi
    mkdir -p "$LOG_DIR" "$(dirname "$PIDFILE")" 2>/dev/null
    local log="$LOG_DIR/serve_$(date '+%Y%m%d_%H%M%S').log"

    echo "→ 启动 $BIN ${SERVE_ARGS:-}"
    # 必须让 CWD = 项目根：serve 的 -pidfile 默认值是相对路径 data/serve.pid，
    # CWD 不固定的话 pidfile 会落到别处，status/stop 就找不到进程。
    local oldpwd="$PWD"
    cd "$BASE" || return 1
    nohup "$BIN" $SERVE_ARGS >>"$log" 2>&1 &
    local pid=$!
    cd "$oldpwd" || true

    # 等它把 pidfile 写出来 / 或直接失败
    for _ in $(seq 1 30); do
        sleep 0.2
        if ! kill -0 "$pid" 2>/dev/null; then
            echo "✗ 进程启动即退出，日志尾部：" >&2
            tail -n 25 "$log" >&2
            return 1
        fi
        [ -f "$PIDFILE" ] && break
    done

    echo "✓ 已启动（PID $pid，版本 $(running_version)）"
    echo "  日志: $log"
}

do_stop() {
    local pid
    if ! pid="$(running_pid)"; then
        echo "· 未在运行"
        rm -f "$PIDFILE"
        return 0
    fi
    echo "→ 停止 PID $pid（SIGTERM，最多等 ${STOP_TIMEOUT}s 让它排空队列）"
    kill -TERM "$pid" 2>/dev/null

    local waited=0
    while kill -0 "$pid" 2>/dev/null; do
        sleep 0.5
        waited=$((waited + 1))
        if [ "$waited" -ge $((STOP_TIMEOUT * 2)) ]; then
            echo "⚠ ${STOP_TIMEOUT}s 未退出，SIGKILL"
            kill -9 "$pid" 2>/dev/null
            sleep 0.5
            break
        fi
    done
    rm -f "$PIDFILE"
    echo "✓ 已停止"
}

do_status() {
    local pid
    if ! pid="$(running_pid)"; then
        echo "状态    : ✗ 未运行"
        [ -f "$PIDFILE" ] && echo "          （残留 pidfile: $PIDFILE）"
        return 1
    fi
    local rv dv etime
    rv="$(running_version)"; dv="$(disk_version)"
    etime="$(ps -o etime= -p "$pid" 2>/dev/null | tr -d ' ')"
    echo "状态    : ✓ 运行中"
    echo "PID     : $pid（已运行 $etime）"
    echo "运行版本: $rv"
    echo "磁盘版本: $dv"
    if same_binary "$pid"; then
        echo "二进制  : ✓ 一致（无需重启）"
    else
        echo "二进制  : ⚠ 不一致 —— 磁盘上的新版还没生效，跑 serve-ctl.sh restart"
        return 2
    fi
}

case "${1:-}" in
    start)   do_start ;;
    stop)    do_stop ;;
    restart) do_stop && do_start ;;
    status)  do_status ;;
    version) "$BIN" -version ;;
    logs)
        f="$(ls -t "$LOG_DIR"/*.log 2>/dev/null | head -1)"
        [ -n "$f" ] || die "没有日志（$LOG_DIR）"
        echo "→ $f"; tail -f "$f" ;;
    *)
        sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
        exit 1 ;;
esac
