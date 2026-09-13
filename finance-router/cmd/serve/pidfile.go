package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pidfile 负责"单实例"这件事。
//
// 为什么这个守卫是必需的：飞书长连接是**集群模式不广播**——同一应用的多个实例里
// 只有一个能收到某条事件。一旦跑起两个 serve，事件会被随机分给其中一个，
// 表现为"有些审批单死活处理不了"，且**没有任何报错**。
//
// 触发场景不是假想的：机器人上的 roboware_start.sh 用
// `pkill -f $(basename $(cmd%% *))` 停进程，对 finance 那一条算出来的名字是
// `build.sh`，而真正的进程叫 `serve` —— **旧进程根本杀不掉**，
// 每天 07:00 的定时重启就会再起一个。
const defaultPidfile = "data/serve.pid"

// acquirePidfile 抢占 pidfile。已有活着的实例时返回错误（由调用方决定是否退出）。
func acquirePidfile(path string, force bool) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 pidfile 目录: %w", err)
	}

	if b, err := os.ReadFile(path); err == nil {
		if old, perr := strconv.Atoi(pidFromPidfile(string(b))); perr == nil && old > 0 {
			if alive, what := processAlive(old); alive {
				// PID 复用保护：确认这个 PID 真的是我们的 serve，而不是恰好复用了号段的别的进程。
				if isOurServe(what) {
					if !force {
						return fmt.Errorf(
							"已有 serve 实例在运行（PID %d，%s）—— 长连接不广播，多实例会导致事件被随机分走。\n"+
								"  先停掉它：%s\n"+
								"  确实要强起：加 -force", old, what, pidfileHint(path))
					}
					fmt.Printf("⚠ -force：忽略正在运行的实例 PID %d（%s）\n", old, what)
				} else {
					fmt.Printf("· pidfile 里的 PID %d 已被别的进程占用（%s），视为陈旧记录\n", old, what)
				}
			} else {
				fmt.Printf("· 清理陈旧 pidfile（PID %d 已不存在）\n", old)
			}
		}
	}

	// 第二行记下版本：这样 `serve-ctl.sh status` 能回答
	//「**正在跑的**是哪一版」，而不只是磁盘上那一版。热更新最怕的就是这点说不清。
	content := fmt.Sprintf("%d\n%s\n%s\n", os.Getpid(), version, buildTime)
	return os.WriteFile(path, []byte(content), 0o644)
}

// pidFromPidfile 取 pidfile 第一行（其余行是版本信息）。
func pidFromPidfile(content string) string {
	if i := strings.IndexByte(content, '\n'); i >= 0 {
		return content[:i]
	}
	return content
}

// releasePidfile 只在 pidfile 仍属于自己时删除，避免把后来者的记录删掉。
func releasePidfile(path string) {
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if pid, _ := strconv.Atoi(pidFromPidfile(string(b))); pid == os.Getpid() {
		_ = os.Remove(path)
	}
}

// processAlive 判断 PID 是否存在，并返回它的命令行（用于确认身份）。
func processAlive(pid int) (bool, string) {
	// 先看 /proc：比 signal 0 更可靠，也不会误伤。
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		cmd := strings.ReplaceAll(string(b), "\x00", " ")
		return true, strings.TrimSpace(cmd)
	}
	// 非 Linux 或 /proc 不可用：退回 signal 0
	if err := syscallSignal0(pid); err == nil {
		return true, "(无法读取 cmdline)"
	}
	return false, ""
}

// isOurServe 宽松判断"这个进程是不是我们的 serve"。
// 只看 cmdline 里有没有 serve —— 二进制名、路径、go run 的临时目录都算。
func isOurServe(cmdline string) bool {
	if cmdline == "" {
		return true // 读不到 cmdline 时保守认为可能是（宁可拦住）
	}
	return strings.Contains(cmdline, "serve")
}

// pidfileHint 给出"该用哪条命令停它"。
// 用可执行文件位置反推：<root>/bin/serve → <root>/scripts/serve-ctl.sh。
// 机器人上进程的 CWD 未必是项目根，所以不能靠相对路径猜。
func pidfileHint(_ string) string {
	exe, err := os.Executable()
	if err != nil {
		return "serve-ctl.sh stop"
	}
	root := filepath.Dir(filepath.Dir(exe)) // bin/serve → 项目根
	ctl := filepath.Join(root, "scripts", "serve-ctl.sh")
	if _, err := os.Stat(ctl); err == nil {
		return ctl + " stop"
	}
	return "serve-ctl.sh stop"
}
