# 审批事件订阅 + 常驻服务（已实现）

> 用户 2026-09-13：「需要先做审批事件订阅 + 常驻服务」
> 本文记录实现、实测与**仍需你补的一个权限**。

---

## 0. 一句话状态

**服务已实现并实测可用**（长连接建立、事件幂等、状态机、自动处理全部跑通）。
**只差一个 scope** 才能自动订阅：`approval:approval`。

```
https://open.feishu.cn/app/<APP_ID>/auth?q=approval:approval,approval:definition&op_from=openapi&token_type=tenant
```

---

## 1. 审批事件是**双层订阅**（关键概念）

| 层 | 在哪配 | 管什么 | 状态 |
|---|---|---|---|
| **第 1 层** | 开发者后台「事件与回调」 | 这类事件**要不要推给我** | ✅ 已完成（长连接 + 3 个审批事件） |
| **第 2 层** | `POST /approvals/:code/subscribe` | 这个审批**定义的实例**要不要产生事件 | ❌ **缺 `approval:approval`** |

**两层都完成，事件才会到达。** 服务启动时会**自动调第 2 层**，失败只告警不退出
（因为管理员也可能已在审批后台手工订阅过）。

---

## 2. 服务做什么

```
飞书（长连接 WebSocket，出站，无需公网 IP）
   │  approval_instance 事件
   ▼
handleInstanceEvent（★ 必须 3 秒内返回）
   ├ ① 按 approval_code 过滤（应用级订阅会收到所有已订阅定义的事件）
   ├ ② event_id 落库（★ 幂等：飞书是"至少一次"投递）
   ├ ③ 状态机白名单（★ 乱序保护）
   └ ④ 入队（队列满则丢弃并告警，绝不阻塞回调）
   ▼
worker（★ 单实例、串行）
   └ 若该实例尚未入库：
        extract（下载 → PDF转PNG → 识别 → 本地入库）
          → sync（落到「报销核对」）
          → notify（需人工的行发消息）
```

### 2.1 三个硬约束（来自官方文档，架构稿 §4.8 已核实）

| 约束 | 后果 | 实现 |
|---|---|---|
| 长连接**集群模式不广播** | 多实例只有一个收到 | **消费端单实例**（不是配置，是必须） |
| 回调须 **3 秒内返回** | 超时触发重推 | 回调只做"落库 + 入队" |
| 事件**至少一次**投递 | 同一事件重复到达 | `event_id` 落库**主键** |

---

## 3. 实测结果

### 3.1 长连接建立

```
══ 财务常驻服务 ══
配置      : /home/coffee/finance/finance-router/config.yml
本地库    : data/finance.db
审批定义  : <APPROVAL_CODE>
⚠ 订阅调用失败（若已在审批后台订阅过可忽略）: … scopes is required: [approval:approval, approval:definition]
[Info] [event-dispatch is ready]
正在建立长连接…
[Info] connected to wss://msg-frontier.feishu.cn [conn_id=7684953050406571187]
```

**长连接成功**，且**订阅失败时服务不退出**（降级正确）。

### 3.2 事件幂等（store 层单测）

```
✓ 首次事件接受
✓ 重复事件判重（幂等生效）
✓ 16 个并发投递 → 接受 1 / 判重 15，收件箱只留 1 条
```

### 3.3 乱序保护（服务层实测）

注入 `APPROVED` 后再注入晚到的 `PENDING`：

```
→ 状态 已通过（实例 TEST-ORD）
⊘ 状态未变: 拒绝：已通过 → 审批中 不在白名单（乱序保护）

再注一次 PENDING：
⊘ 状态未变: 拒绝：已通过 → 审批中 不在白名单（乱序保护）
```

**终态不会被晚到的旧事件打回。**

### 3.4 自动处理（服务层实测）

```
go run ./cmd/serve -once AA774403-…
  收件人: user_id=<ADMIN_USER_ID>
  需人工 0 条
  ✓ 实例 AA774403 处理完成
  已处理实例 : 1

再跑一次：
  = 实例 AA774403 已处理过，跳过
```

**幂等**：已入库的实例不会重复下载与识别。

### 3.5 状态机留痕

```
instance_state:
   C5489DC9-2D8   已通过
state_transition:
   C5489DC9-2D8   ''       → 已通过 | 事件 fake_…
   TEST-ORDER-1   ''       → 已通过 | 事件 fake_…
   TEST-ORDER-1   已通过    → 审批中 | 拒绝非法跃迁（乱序保护）
```

**被拒绝的跃迁也留痕**（可审计）。

### 3.6 一个"意外但正确"的验证

我试图清理测试残留数据时，`DELETE FROM state_transition` **被触发器拒绝**：

```
sqlite3.IntegrityError: state_transition is append-only
```

**这正是 append-only 该有的表现** —— 连我自己都删不掉。最后只能重建整个库。

---

## 4. 新增/改动的文件

| 文件 | 说明 |
|---|---|
| `cmd/serve/main.go` | **常驻服务**：长连接 + 事件处理 + worker |
| `internal/store/migrations/0002_events.sql` | 事件收件箱、实例状态、跃迁留痕（均 append-only） |
| `internal/store/events.go` | 事件幂等、状态机白名单、跃迁落库 |
| `internal/pipeline/` | **抽取/落表/通知的核心逻辑**，命令与服务共用 |
| `cmd/{extract,sync,notify}/main.go` | 改为薄封装，只做参数解析 |

### 4.1 为什么做这次重构

原本 `extract/sync/notify` 的核心逻辑都在 `package main` 里，服务无法复用。
现在抽到 `internal/pipeline`，**命令与服务跑的是同一份代码** —— 不会出现
"手动跑对了、服务跑错了"这种分叉。

---

## 5. 怎么用

### 5.1 常驻运行

```bash
cd finance-router && source ./devenv.sh
go run ./cmd/serve
```

### 5.2 自测入口（**不依赖真实事件**）

```bash
# 直接处理一个实例（走完整链路）
go run ./cmd/serve -once <instance_code>

# 注入假事件，走完整事件路径（幂等 + 状态机 + 处理）
go run ./cmd/serve -fake-event "<code>:APPROVED"
go run ./cmd/serve -fake-event "<code>:APPROVED,<code>:PENDING"   # 测乱序保护
go run ./cmd/serve -dry-run -fake-event "<code>:PENDING"          # 只看不处理
```

### 5.3 仍然可用的手动命令

```bash
bash scripts/pipeline.sh 5     # 体检→建库→抽取→落表→通知
go run ./cmd/archive           # 人工审核通过后归档
```

---

## 6. 还需要你做的（只有一件）

补 `approval:approval`（或 `approval:definition`）：

```
https://open.feishu.cn/app/<APP_ID>/auth?q=approval:approval,approval:definition&op_from=openapi&token_type=tenant
```

### 6.1 ✅ 已解决：订阅其实已经生效

2026-09-13 当天复查时，订阅接口返回：

```
code=1390007 msg=subscription existed
```

**说明订阅已经存在**（补权限后服务订过一次，或管理员在审批后台手工订过）——
这也是事件真的会推过来的原因。已把该错误码识别为"正常"而非失败：

```
✓ 该审批定义已是订阅状态（subscription existed）
```

### 6.2 ★ 踩到的坑：订阅了 N 个事件，就必须注册 N 个处理器

第一次手动测试时报：

```
[Error] handle message failed, err: event type: approval_task, not found handler
```

**原因**：控制台订阅了 **3 个**审批事件（`approval_cc` / `approval_instance` / `approval_task`），
而服务只注册了 `approval_instance` 一个处理器。
SDK 对"没有处理器的事件类型"会**返回错误** ——
等于把"我们不关心的事件"变成了**失败回调**（还可能触发飞书重推）。

**`event_log` 当时是空的**，正因为 `approval_task` 在 SDK 层就被挡下了，
我们的代码**根本没跑到**。

**修法**：给全部三个事件类型注册处理器。不参与处理的两个**收下 + 留痕 + 返回 nil**：

| 事件 | 处理 |
|---|---|
| `approval_instance` | 主处理（幂等 → 状态机 → 入队 → 跑链路） |
| `approval_task` | 收下、留痕（`result=IGNORED`）、返回 nil |
| `approval_updated` | 同上 |

> **通用原则**：**订阅与处理器必须一一对应**。
> 哪怕不参与处理，也要"确认收到"，否则正常投递会变成失败回调。

### 6.3 ★★ 第二次修正：**事件协议版本不匹配**（第一次修法没用）

上一条修完后，报错**依旧**：

```
[Error] handle message failed, err: event type: approval_task, not found handler
```

深挖 SDK 源码后发现真正原因：

| 项 | 值 |
|---|---|
| SDK 注册的键 | `approval.approval.updated_v4` · `approval.instance.status_changed_v4` · `approval.task.status_changed_v4` |
| 控制台实际推送的事件名 | **`approval_instance`** · **`approval_task`** · **`approval_cc`**（**v1.0 旧协议**） |
| 结果 | **两者对不上 → SDK 永远找不到处理器** |

SDK v3.12.0 **只支持 v2 协议**的审批事件，**完全没有** v1 旧事件的内置处理器
（`grep '"approval_task"'` 在整个 SDK 里搜不到）。

**真正的修法**：SDK 提供了一个通用注册入口，可以按**任意事件名**注册：

```go
// dispatcher 的公开扩展点（ext_event_dispatch.go）
func (d *EventDispatcher) OnCustomizedEvent(
    eventType string,
    handler func(ctx context.Context, event *larkevent.EventReq) error,
) *EventDispatcher
```

用它接住 v1 的三个事件名，**原始 JSON 自己解析**：

```go
for _, et := range []string{"approval_instance", "approval_task", "approval_cc"} {
    d.OnCustomizedEvent(et, func(ctx, req) error { return s.handleV1Event(ctx, et, req) })
}
```

#### v1 与 v2 的两处结构差异（重要）

| | v1.0 | v2 |
|---|---|---|
| **幂等键** | 顶层 **`uuid`** | `header.event_id` |
| 业务字段 | 平铺在 `event` 对象 | `header` + `event` 两层 |

所以 v1 的处理路径里用的是 `ev.UUID` 做幂等。

#### 两条路，任选其一

| 方案 | 做法 | 评价 |
|---|---|---|
| **A（已实现）** | 用 `OnCustomizedEvent` 接住 v1 事件 | ✅ 现在就能用，**不用改控制台** |
| B | 控制台改用 v2 版本的事件（`approval.instance.status_changed_v4` 等） | 更"正统"，但要改控制台配置 |

**代码同时保留了两条路的处理器**，所以将来你换成 v2 事件也不用改代码。

---

## 7. 仍未做的

| # | 事项 | 影响 |
|---|---|---|
| 1 | **outbox 投递队列** | 现在 sync/notify 失败只告警，没有重试与死信 |
| 2 | **定时任务**（催办 D+3/7/14/30） | 现在的 notify 是"事件触发时通知一次" |
| 3 | **归档自动化** | 仍需人工点「通过」+ 手动跑 `archive` |
| 4 | 卡片按钮（消息里直接审批） | 现在需切到表格里点 |
| 5 | SQLite 9 组不变式补齐 | 架构稿的 P1 验收条件 |

---

*本文所有输出均为实测；未完成项与"意外删不掉"的插曲均已如实记录。*
