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
    "/home/wdr/my-go-app/main_binary|$HOME/robo_logs/log2"
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

# --- 初始化 ---
# 等待网络
while ! ping -c 1 -W 1 8.8.8.8 &> /dev/null; do sleep 2; done

start_programs

# --- 监控循环 ---
while true; do
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
    sleep 600
done
