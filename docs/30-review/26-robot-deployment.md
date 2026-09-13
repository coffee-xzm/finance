# 机器人（192.168.1.3）部署与热更新

> 2026-09-13。起因是问题「已经通过 192.168.1.3 用 git 同步部署，但 roboware_start.sh 不好热更新」。

---

## 0. 现场实际是什么样

连上机器看清楚之后，情况比"脚本不好更新"严重得多：**finance 服务根本没在运行。**

```
$ ps -eo pid,ppid,cmd | grep -E 'serve|roboware'
   1309     936  /bin/bash /home/wdr/roboware_start.sh     ← 守护脚本活着
   2228    1309  python3 /home/wdr/RoboWarehouse/run.py
   2236    1309  /home/wdr/bitable2docx/awesomeProject
                  （没有 serve）                            ← finance 是死的
```

日志里躺着四条互不相干的错误：

```
/home/wdr/finance/build.sh: 2: source: not found
/home/wdr/finance/build.sh: 3: go: not found
✗ 在 /home/wdr/finance/finance-router 及其上级目录未找到 config.yml
```

| # | 问题 | 后果 |
|---|---|---|
| 1 | `build.sh` **没有 shebang** | `nohup` 拉起时内核返回 ENOEXEC，调用方回退用 `sh`(dash) 执行 → `source: not found`，`devenv.sh` 从来没生效过 |
| 2 | 机器上**没装 Go** | `go build` 必然失败 |
| 3 | 失败是**静默**的 | `go build` 报错后脚本继续往下跑，于是永远执行**上一次留在磁盘上的旧二进制** —— 这正是"热更新看起来没生效"的直接原因 |
| 4 | `config.yml` 不在 git（含密钥） | 机器上根本没有，serve 起不来 |
| 5 | **`stop_programs` 杀不掉 serve** | 见下 |

### 第 5 条是个埋着的雷

```bash
proc_name=$(basename "${cmd%% *}")   # "/home/wdr/finance/build.sh" → "build.sh"
pkill -f "$proc_name"                # pkill -f build.sh
```

真正的进程叫 **`serve`**，命令行里没有 `build.sh` 这个词 —— **旧进程从来没被杀死过**。

而飞书长连接是**集群模式不广播**（多个实例只有一个能收到某条事件）。所以一旦服务真的跑起来，每天 07:00 的定时重启就会再叠一个实例，事件被随机分给两个进程，表现为「**有些审批单死活处理不了**」，而且**没有任何报错**。

---

## 1. 方案

### 1.1 不在目标机上编译

机器上没有 Go，而 `bin/serve` 本来就随 git 走。所以部署 = **拉代码 + 原子替换二进制 + 重启**，不是"在机器上 build"。

### 1.2 停进程靠 pidfile，不靠进程名

新增 `serve` 的 `-pidfile`（默认 `data/serve.pid`）与**单实例守卫**：

```
✗ 拒绝启动：已有 serve 实例在运行（PID 120，/…/bin/serve -no-subscribe -dry-run）
  —— 长连接不广播，多实例会导致事件被随机分走。
  先停掉它：/…/scripts/serve-ctl.sh stop
```

pidfile 写三行：PID / 版本 / 构建时间。第二行让 `status` 能回答
「**正在跑的**是哪一版」，而不只是"磁盘上是哪一版"。

### 1.3 `start` 幂等 + 自动识别版本变化

`serve-ctl.sh start`：

- 已在跑且**二进制一致** → 什么都不做（所以旧守护脚本那套错误的 `pkill` 也造不出重复实例）
- 已在跑但**二进制变了** → 自动重启

判定用 `/proc/<pid>/exe` 的 inode 与磁盘二进制比对：`mv` 替换后老进程仍指向旧 inode，
两者不同即说明该重启了。（用 `mv` 而不是 `cp`：`cp` 覆盖运行中的可执行文件会 ETXTBSY。）

```
· 检测到二进制已更新（运行中 v1 → 磁盘 v2），自动重启
✓ 已启动（PID 303，版本 v2）
```

### 1.4 `roboware_start.sh` 纳入 git

它原来只存在于机器本地（`/home/wdr/roboware_start.sh`），git 永远碰不到 —— 这才是"不好热更新"的根因。

现在：仓库里的权威副本 `deploy/roboware_start.sh`，机器上改成**软链**，`git pull` 即更新。

```
/home/wdr/roboware_start.sh -> /home/wdr/finance/deploy/roboware_start.sh
```

同时把配置格式扩成可选三段式（两段式旧写法仍兼容）：

```
"启动命令|日志目录"                    ← 旧写法
"启动命令|停止命令|日志目录"            ← 新写法，finance 用这条
```

finance 那一条：

```
"/home/wdr/finance/build.sh|/home/wdr/finance/finance-router/scripts/serve-ctl.sh stop|$HOME/robo_logs/log6"
```

---

## 2. 新增/改动的东西

| 文件 | 作用 |
|---|---|
| `finance-router/scripts/serve-ctl.sh` | 启停控制（pidfile 版）：`start/stop/restart/status/logs` |
| `finance-router/scripts/deploy.sh` | `git pull` → 原子替换 → 重启 → 健康检查 → **失败回滚** |
| `finance-router/scripts/build.sh` | 本机构建，注入版本戳 `<短哈希>-<时间戳>` |
| `finance-router/cmd/serve/pidfile.go` | 单实例守卫 |
| `build.sh`（仓库根） | 补 shebang；不再编译，转发给 `serve-ctl.sh` |
| `deploy/roboware_start.sh` | 守护脚本权威副本（支持三段式停止命令） |

`deploy.sh` 的**回滚**是必须的：机器人没人盯着，一次坏部署不能让它一直躺着。
所以旧二进制在 `git pull` **之前**就要备份好 —— 拉完再备份拿到的已经是新的，回滚就没有意义了。

---

## 3. 验收（全部在真机上跑过）

### 3.1 服务起来了

```
══ 财务常驻服务 ══
配置      : /home/wdr/finance/finance-router/config.yml
本地库    : data/finance.db
审批定义  : <APPROVAL_NAME>
            <APPROVAL_CODE>  [ACTIVE]
✓ 该审批定义已是订阅状态（subscription existed）
pidfile   : data/serve.pid（PID 12018）
正在建立长连接…（无需公网 IP；长连接不广播，故必须单实例）
  ✓ 已备份本地库 → backup/finance-20260913-200509.db
[Info] connected to wss://msg-frontier.feishu.cn [conn_id=<CONN_ID>]
```

### 3.2 单实例守卫有效

| 动作 | 结果 |
|---|---|
| `serve-ctl.sh start` | ✓ 已启动（PID 120，版本 v1） |
| 直接再起一个 `bin/serve` | ✗ 拒绝启动，exit 1（提示用 serve-ctl 停） |
| 替换二进制后 `start` | · 检测到 v1→v2，自动重启 → ✓ PID 303，版本 v2 |
| 再 `start` 一次 | ✓ 已在运行，无需启动 |
| `stop` | ✓ SIGTERM 优雅退出 |

### 3.3 完整的 pull 部署

```
· 旧二进制已备份（07a375d-dirty）
→ git fetch origin main
✓ 代码更新 e91a67f → 8955aed
    8955aed fix(build): 版本号去掉误导性的 dirty 后缀…
→ 重启服务
✓ 已停止 / ✓ 已启动（PID 12536）
→ 健康检查（等 3 秒确认进程没有立刻退出）
状态    : ✓ 运行中
二进制  : ✓ 一致（无需重启）
✓ 部署完成
```

---

## 4. 以后怎么操作

**部署新版本（在机器人上）：**
```bash
/home/wdr/finance/finance-router/scripts/deploy.sh
```

**日常查看：**
```bash
serve-ctl.sh status     # 跑没跑 / 哪一版 / 磁盘和运行是否一致
serve-ctl.sh logs       # 跟踪日志
serve-ctl.sh restart    # 强制重启
```

**开发侧（本机）：**
```bash
./scripts/build.sh      # 编译并注入版本戳
git add -A && git commit && git push
```

`roboware_start.sh` 不需要再手工改 —— 它是软链，`git pull` 就更新了。
（当前正在跑的那个实例仍是旧逻辑，但它已经不可能制造重复进程了，
见 §1.3；下次重启自然换成新的。）

---

## 5. 遗留 / 成本

- **git 里存二进制会持续膨胀**。`bin/serve` 18MB，每部署一次就多一个 blob，
  100 次部署约 1.8GB。目前已把另外 8 个开发二进制（合计 ~97MB）移出跟踪，
  只留 `bin/serve`。
  若在意体积，可改成：**开发机编译 + scp 推二进制**，git 只走源码和脚本。
  这次没改，是因为它会影响你既有的"git 同步"工作习惯，需要你点头。
- **当前跑的二进制版本号显示 `07a375d-dirty`**：它是提交前构建的，
  版本戳取自当时的工作区。功能与源码一致，只是标签是旧的；
  下次用 `scripts/build.sh` 重新构建并提交就会是新格式 `<短哈希>-<时间戳>`。
- `roboware_start.sh` 的定时重启依赖它自己那个 `while true` 循环（每 10 分钟醒一次）。
  这次没改成 systemd —— 你选的是"一条命令重启"，而且换成 systemd 会动到
  其它三个服务（RoboWarehouse / my-go-app / bitable2docx）的启动方式，风险不划算。
- 非 finance 的条目仍用 `pkill -f <basename>` 停进程，可能出现同名误杀；
  这次只给 finance 上了精确停止命令，其余保持原样（不动没坏的东西）。
