#!/bin/bash
# roboware_start.sh —— 机器人（192.168.1.3）上的开机启动 + 守护脚本
#
# ⚠ 这份是**仓库里的权威副本**。机器人上的 /home/wdr/roboware_start.sh
#   是一个指向本文件的符号链接，所以 `git pull` 就能更新它 ——
#   以前它是台机器上的孤立文件，git 永远碰不到，只能手工改。
#
# 2026-09-13 修复的两个问题：
#
#   ① **停不掉进程**。原来用
#          proc_name=$(basename "${cmd%% *}")  →  pkill -f "$proc_name"
#      对 finance 那一条算出来是 `build.sh`，而真正的进程叫 `serve`，
#      于是旧进程从来没被杀死。每天 07:00 的定时重启会再起一个，
#      而飞书长连接「集群模式不广播」—— 事件被随机分给两个实例，
#      表现为「有些审批单死活处理不了」，且没有任何报错。
#      → 现在支持可选的**停止命令**字段（三段式），finance 用 pidfile 精确停。
#
#   ② **脚本本身无法热更新**。它是 git 之外的孤立文件 → 改成仓库内副本 + 软链。
#
# 配置格式（两段式兼容旧写法）：
#     "启动命令|日志目录"
#     "启动命令|停止命令|日志目录"     ← 推荐；停止命令留空则退回按进程名匹配

PROGRAMS=(
    "python3 /home/wdr/RoboWarehouse/run.py|$HOME/robo_logs/log1"
    # ★ 2026-09-21 注释掉：/home/wdr/my-go-app/ 在这台机器上**不存在**（模板里的占位项）。
    #   以前守护脚本不查存活，所以一直没人发现；加上存活检查后它会每 10 分钟
    #   报一次"启动失败"，纯噪音。真要跑这个服务时再解开。
    #"/home/wdr/my-go-app/main_binary|$HOME/robo_logs/log2"
    #"python /home/wdr/feishu_chat_bot/feishu_ai_bot.py|$HOME/robo_logs/log3"
    #"/home/wdr/github-commit-monitor/commit-monitor|$HOME/robo_logs/log4"
    "/home/wdr/bitable2docx/awesomeProject|$HOME/robo_logs/log5"
    # ★ finance：用 serve-ctl.sh 精确启停（pidfile），不能按进程名 pkill。
    "/home/wdr/finance/build.sh|/home/wdr/finance/finance-router/scripts/serve-ctl.sh stop|$HOME/robo_logs/log6"
)

RESTART_FLAG_FILE="$HOME/.robo_restarted_today"

# --- 解析一条配置到全局 START_CMD / STOP_CMD / LOG_DIR -------------
parse_item() {
    local item="$1"
    local n
    n=$(awk -F'|' '{print NF}' <<<"$item")
    if [ "$n" -ge 3 ]; then
        START_CMD="$(cut -d'|' -f1 <<<"$item")"
        STOP_CMD="$(cut -d'|' -f2 <<<"$item")"
        LOG_DIR="$(cut -d'|' -f3- <<<"$item")"
    else
        START_CMD="$(cut -d'|' -f1 <<<"$item")"
        STOP_CMD=""
        LOG_DIR="$(cut -d'|' -f2- <<<"$item")"
    fi
}

start_programs() {
    for item in "${PROGRAMS[@]}"; do
        parse_item "$item"
        mkdir -p "$LOG_DIR"

        log_file="$LOG_DIR/log_$(date '+%Y%m%d_%H%M%S').log"
        # 用 eval 执行命令，确保参数解析正确
        nohup $START_CMD > "$log_file" 2>&1 &
        echo "[$(date '+%H:%M:%S')] 已启动: $START_CMD -> $log_file"
    done
}

stop_programs() {
    for item in "${PROGRAMS[@]}"; do
        parse_item "$item"

        if [ -n "$STOP_CMD" ]; then
            # 有停止命令：精确停（finance 走这里，靠 pidfile）
            echo "[$(date '+%H:%M:%S')] 停止: $STOP_CMD"
            $STOP_CMD || echo "  ⚠ 停止命令返回非零: $STOP_CMD"
        else
            # 没有停止命令：退回按进程名匹配（保留旧行为）
            proc_name=$(basename "${START_CMD%% *}")
            pkill -f "$proc_name"
        fi
    done
    sleep 5
    # 强制清理：只对"没有停止命令"的条目做，避免误杀已经干净退出的服务
    for item in "${PROGRAMS[@]}"; do
        parse_item "$item"
        [ -n "$STOP_CMD" ] && continue
        proc_name=$(basename "${START_CMD%% *}")
        pkill -9 -f "$proc_name" 2>/dev/null
    done
}

# --- 存活检查（初始化与每轮循环共用）---
#
# ★ 2026-09-21 新增。原来那个循环**只做 07:00 的定时重启**：
#   进程崩了（panic / OOM / 被误杀）要等最多 24 小时才会被拉起来，
#   期间审批事件全部丢失（飞书长连接离线不补推）。
#
# 约定：配了停止命令（三段式）的条目，其启动命令必须是**幂等**的
#   —— finance 的 `serve-ctl.sh start` 正是幂等的（在跑就不动，二进制变了才自动重启）。
#   没配停止命令的条目仍按进程名判断，避免每 10 分钟拉起一个重复进程。
#
# 所以**开机初始化也用它**（而不是 start_programs）：手工重启守护脚本时
# 不会把另外三个服务拉出重复副本。
ensure_alive() {
    for item in "${PROGRAMS[@]}"; do
        parse_item "$item"
        mkdir -p "$LOG_DIR" 2>/dev/null

        if [ -n "$STOP_CMD" ]; then
            local keep_log="$LOG_DIR/keepalive_$(date '+%Y%m%d').log"
            if ! $START_CMD >>"$keep_log" 2>&1; then
                echo "[$(date '+%H:%M:%S')] ⚠ 存活检查启动失败: $START_CMD（详见 $keep_log）"
            fi
            continue
        fi

        local proc_name
        proc_name=$(basename "${START_CMD%% *}")
        if pgrep -f "$proc_name" > /dev/null 2>&1; then
            continue
        fi
        local log_file="$LOG_DIR/log_$(date '+%Y%m%d_%H%M%S').log"
        nohup $START_CMD > "$log_file" 2>&1 &
        echo "[$(date '+%H:%M:%S')] 存活检查: $proc_name 不在，已重新启动 -> $log_file"
    done
}

# --- 初始化 ---
# 等待网络
#
# ★ 2026-09-21：加上超时上限。原来是无上限 `while ! ping 8.8.8.8; do sleep 2; done`
#   —— 如果路由器/上行挂了（或 ICMP 被禁），脚本会**永远卡在这里**，
#   连本地那三个不需要外网的服务也起不来。现在最多等 5 分钟就先起本地服务。
wait_net() {
    local waited=0
    while ! ping -c 1 -W 1 8.8.8.8 &> /dev/null; do
        waited=$((waited + 1))
        if [ "$waited" -ge 150 ]; then
            echo "[$(date '+%H:%M:%S')] ⚠ 等网络超过 5 分钟（外网不通？），先启动本地服务"
            return 1
        fi
        sleep 2
    done
    return 0
}

wait_net

# 幂等启动：开机时什么都没跑 → 全部拉起；手工重启守护脚本时 → 已在跑的不动。
ensure_alive

# --- 监控循环 ---
#
# ★ 先 sleep 再检查（原来是先检查后 sleep）：初始化那次 ensure_alive 已经把所有
#   该起的都起了，若紧接着再检查一遍，遇到"启动慢半拍"的进程会被判成不在而**重复拉起**。
#   先睡一小段可避免这个竞态。
#
# ★ 节拍 60 秒（原来 600 秒）：600 秒意味着进程崩掉后最长有 10 分钟无人处理，
#   期间审批事件会丢（虽然重启后补漏扫描能补回来，但没必要留这么长的盲区）。
#   ensure_alive 只是 stat pidfile / pgrep，60 秒一次的开销可以忽略；
#   07:00 的定时重启另有"每天一次"标记保护，逐分钟判也只会真正执行一次。
LIVENESS_INTERVAL="${LIVENESS_INTERVAL:-60}"
while true; do
    sleep "$LIVENESS_INTERVAL"

    ensure_alive

    current_hour=$(date "+%H")
    today=$(date "+%Y%m%d")

    if [[ "$current_hour" =~ ^(07|7)$ ]]; then
        if [[ ! -f "$RESTART_FLAG_FILE" ]] || [[ "$(cat $RESTART_FLAG_FILE)" != "$today" ]]; then
            echo "[$(date '+%H:%M:%S')] 定时重启任务开始..."
            echo "$today" > "$RESTART_FLAG_FILE"
            stop_programs
            start_programs
        fi
    fi
done
