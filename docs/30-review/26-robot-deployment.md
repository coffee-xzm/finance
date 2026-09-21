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

---

## 6. 2026-09-21 复查：断网/断电恢复能力（含一次真实抢修）

> 起因：用户问「检查现在服务是不是掉了，他需要应对断网、断电，恢复后正常工作」。
> 这次登上了机器（`wdr@192.168.1.3`）逐项核对，发现了 **4 个真问题**，并当场修掉。

### 6.1 现场事实

| 项 | 事实 |
|---|---|
| 服务 | **没掉**：PID 124993，已连续运行 **7 天**（09-14 16:57 起） |
| 版本 | `992210d-srce281d7ba-20260914T165707` —— 09-14 的码，**没有采购链路**（日志里采购事件被"跳过其它审批定义"） |
| 07:00 定时重启 | **空转**：守护进程 PID 1309 是 09-13 **18:30** 启动的，而脚本变成软链（finance 那行才有三段式停止命令）是 09-13 **20:05** → **内存里是旧脚本** → 退化成 `pkill -f build.sh`（永远匹配不到）。§1.3 结尾那句"下次重启自然换成新的"应验了，但没人真的重启过守护进程 |
| ⚠ 断电恢复 | **会起不来**：`config.yml` 里 `approval_name_expect: "系统测试"`，而绑定的定义实际叫「27发票收集」；运行的二进制启动时有 fail-closed 校验（对不上 `os.Exit(1)`）。它活着只是因为 09-14 那次启动之后再没成功重启过 |
| 崩溃恢复 | 那个 10 分钟循环**只做 07:00 定时重启，不查存活** → 崩了最多等 24 小时 |
| 断网恢复 | 长连接会自动重连 ✅，但**离线期间的事件飞书不补推** → 永久丢失，事后没有任何补扫 |
| 双实例 | 机器人（旧码）与开发机上的 serve **同时订阅 27发票收集** → 事件被随机分走 |

### 6.2 这次做的修改

| 文件 | 改动 |
|---|---|
| `deploy/roboware_start.sh` | ① 新增 `ensure_alive()`：每 10 分钟存活检查（配了停止命令的条目调**幂等**启动命令 `serve-ctl.sh start`；其余按进程名判断，避免重复拉起）<br>② **开机初始化也走 `ensure_alive`**（手工重启守护脚本时不会把别的服务拉出重复副本）<br>③ `wait_net` 从无限等改成**最多 5 分钟**（原来路由器/上行不通会永远卡住，连本地服务都起不来）<br>④ 循环改成**先 sleep 再检查**（消除"初始化刚起、首轮又判不在"的重复启动竞态）<br>⑤ 注释掉模板占位项 `/home/wdr/my-go-app/main_binary`（该目录在机器上不存在；加上存活检查后它会每 10 分钟报一次失败，纯噪音） |
| `cmd/serve/catchup.go`（新增） | **补漏扫描**：启动时 + 每 10 分钟，按幂等锚点补「离线期间漏掉的审批」。采购：APPROVED 且本地 `purchase_sync` 无记录 → 补跑；发票：已通过 → 补归档/回写；已拒绝/撤回类 → 补剔除；审批中但**表单已有附件而本地无证据** → 重新抽取（顺带修掉"代建空单→本人补齐后重提"这条路）。只入队、走单 worker，保持串行。终态实例（已通过/已拒绝/采购已处理）只入队一次，避免每轮重复刷日志 |

**事后追加的两处调整**（部署当天又改的）：

| 项 | 说明 |
|---|---|
| 存活检查节拍 600s → **60s** | 600 秒意味着崩掉后最长 10 分钟无人管。`ensure_alive` 只是 stat pidfile / pgrep，60 秒一次开销可忽略；07:00 定时重启另有"每天一次"标记，逐分钟判也只会执行一次 |
| `catchup.go` 加"终态只入队一次"闸门 | 否则已通过 `C0861819`、已拒绝 `C15B6179` 这类终态实例每 10 分钟被重复入队一次，日志持续刷（功能无害但脏） |

### 6.3 部署记录（走了"开发机编译 + scp"这条，没走 git）

原因见 §6.5：仓库的 pre-commit 守卫当前会拦下任何提交。

```bash
# 开发机
go build -o bin/serve ./cmd/serve
scp bin/serve            wdr@192.168.1.3:/tmp/serve.new
scp config.yml(合并后)    wdr@192.168.1.3:/tmp/config.new
scp deploy/roboware_start.sh wdr@192.168.1.3:/tmp/roboware_start.sh.new
scp seedpurchase(交叉编译)  wdr@192.168.1.3:/tmp/seedpurchase

# 机器人
./finance-router/scripts/serve-ctl.sh stop
cp config.yml config.yml.bak-20260921-172429
cp data/finance.db /tmp/finance.db.bak-20260921-172429
install -m 755 /tmp/serve.new bin/serve
install -m 644 /tmp/config.new config.yml
install -m 755 /tmp/roboware_start.sh.new deploy/roboware_start.sh
/tmp/seedpurchase -db data/finance.db   # 把开发机已处理过的 3 单种进 purchase_sync，防重复建行
./finance-router/scripts/serve-ctl.sh start
```

`config.yml` 的合并内容：修 `approval_name_expect` → `27发票收集`；新增 `feishu.approvals`（两个角色）、
`feishu.bitable.bases`（review / flow / purchase_request）、顶层 `dept_map` 与 `purchase_to_invoice`。
机器人的密钥等值原样保留。

**回滚**：`cp config.yml.bak-20260921-172429 config.yml`，二进制回滚需重新 install 旧版本（或在 git 里
`git checkout -- bin/serve` 到 b413500 那版），数据库备份在 `/tmp/finance.db.bak-20260921-172429`。

### 6.4 验证证据（都是日志里跑出来的）

- 启动校验通过：`[invoice_collect] 27发票收集 (ACTIVE)`、`[purchase] 采购审批 - 27Test (ACTIVE)`，两个定义都订阅成功；
- **补漏扫描一次捞出 2 张真正漏掉的采购单**：
  - `EB06A19D`（硬件组，**3 条费用明细**，提交人 f7e624e9）—— 旧服务一直在"跳过其它审批定义"，开发机也没处理过；
    这次补出 3 行流水 + 3 行采购申请表 + 代建发票单（顺带首次跑通"多明细→多行"）；
  - `529A9208`（21:38 那单）—— 补出流水行、采购申请表行（含商品图片转附件）、代建发票单 `65F791F5`，
    **名称预填成 `213-采购审批`**（新口径：取费用明细名称），并退回给发起人；
  - 同轮还正确剔除了已拒绝的 `C15B6179`；`C0861819` 在核对表无行 → 跳过（不会把测试单归档）；
- 守护进程换成新脚本后：另两个服务（RoboWarehouse / bitable2docx）**PID 未变、没有重复副本**；
  `keepalive_*.log` 里是 `✓ 已在运行（PID …, 版本 dev），无需启动`（幂等路径生效）。
- **崩溃恢复实测**：`kill -9` 掉 serve（17:26:51）→ 由守护进程在 17:33:24 拉起（PID 917666），
  停机 6.5 分钟；随后把节拍从 600s 改成 60s，并再次重启守护进程（`keepalive_*.log` 里能看到
  连着两次"已在运行"，说明 60 秒节拍在跑）。停机期间没有审批事件丢失风险：重启后补漏扫描会补。
- **金额口径修正当日也推上机器人**（`单价 × 数量`，见 `33-...md` §12.12）：新二进制 PID 919021。

### 6.5 遗留

- ⚠ **仓库 pre-commit 守卫当前会拦下任何提交**：`gen-blocklist.sh` 把 config.yml 里所有含中文的值
  都当成"真实值"（`has_cjk` 分支），于是 `通过/待办/驳回/技术组物资` 这类**业务词**也被拉黑，
  而源码里必然出现这些词（HEAD 里 55 个文件含"通过"）。实测：只暂存 `cmd/serve/catchup.go`
  就被拒（"命中真实值黑名单（已通…）"）。**要先修 `gen-blocklist.sh`（去掉 has_cjk 或加业务词白名单），
  并把文档/代码里的真实租户标识符（表 id / 部门 id / 审批 code / 实例号 / app_token）脱敏，才能正常提交。**
- 机器人现在的 `bin/serve` 与 `deploy/roboware_start.sh` **领先于它的 git HEAD（b413500）**：
  下次若用 `push-to-robot.sh`（会 `git reset --hard`）会把它们换回旧版。所以脱敏后要尽快补一次正式提交 + 推送。
- 复现这次"断电恢复"最彻底的办法：真重启一次机器人（脚本已在开机路径上，等待网络有 5 分钟上限）。
  本次只验证了"进程被 kill -9 后由存活检查拉起"与"守护脚本重启后幂等启动"两条路径。
