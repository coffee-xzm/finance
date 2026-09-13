# 多审批表单下的绑定：只认 26 赛季报销单

> 2026-09-13。起因是问题「我看审批有多个表单，希望（系统）只绑定 26 赛季报销单审批那个」。

---

## 0. 租户里到底有几个表单

**结论：4 个。** 实测（近 30 天，按实例号去重）：

| approval_code | 审批名称 | 近 30 天实例数 | 是否本项目 |
|---|---|---|---|
| `<APPROVAL_CODE>` | **<APPROVAL_NAME>** | 197 | ✅ 绑定 |
| `87E23604-…` | <OTHER_APPROVAL_2> | 13 | ❌ |
| `<APPROVAL_CODE_OTHER1>` | <OTHER_APPROVAL_1> | 9 | ❌ |
| `CE4E54B8-…` | 费用报销 | 2 | ❌ |

`config.yml` 里绑的 `<APPROVAL_CODE>` 名称正是「<APPROVAL_NAME>」，**绑定本来就是对的**。

---

## 1. 为什么"列举审批表单"这么费劲

### 1.1 `GET /approval/v4/approvals` 用不了

第一反应是调"获取审批定义列表"。实测：

```
GET /approval/v4/approvals          → 99991663 Invalid access token
GET /approval/v4/approvals/{code}   → 200 OK（同一个 token）
```

同一个 tenant_access_token，单查正常、列举报"token 无效"。查证：

- 飞书 SDK 的 **approvalV4 域里根本没有"列举审批定义"这个方法**
  （只有 get / create / subscribe / unsubscribe）；
- 唯一带搜索语义的 `search_launchable`（搜索可发起的审批定义）
  文档明确要求 **`user_access_token`**。

**结论：只有 tenant token 的常驻服务无法枚举审批定义。**

### 1.2 改用"按实例反查"

`POST /approval/v4/instances/query`（tenant token + `approval:approval.list:readonly`）
返回的每条实例都带 `approval.code` + `approval.name`。于是：

```
① 用已绑定的 approval_code 查它的全部实例 → 拿到所有发起人的 user_id
② 拿这些 user_id 逐个查实例 → 他们在**别的表单**里提交过什么，一并暴露
③ 按 instance_code 去重汇总
```

第 ① 步是关键：只查管理员一个人会严重低估（采购负责人可能从没发起过报销单）。
实测由 197 个报销实例带出 24 个发起人，再查这 24 人，4 个表单全部现形。

> ⚠️ 这一步最初写错过：`approval_code` 查询和每个用户的查询结果**直接累加**，
> 同一个实例被数了两次（26赛季那个算成 10197 个）。必须按 `instance_code` 去重，
> 去重后是 **197**，与早先用 `ListInstancesInRange` 估的 ~211 一致。

工具：`cmd/approvals`
```
go run ./cmd/approvals                  # 列出所有表单 + 显示当前绑定是否匹配
go run ./cmd/approvals -subscribe       # 订阅匹配到的那一个（幂等）
go run ./cmd/approvals -unsubscribe-all # 退订除它以外的全部
go run ./cmd/approvals -days 90         # 回溯更久（自动按 30 天分段）
go run ./cmd/approvals -shallow         # 只查 admin 一个用户（默认会用全体发起人做种子）
```

---

## 2. 找到两处真实越界（已修）

"配置填对了"不等于"不会处理错"。审计所有接触审批的调用点后，发现两处真的会漏：

### 2.1 `recon` 的 path B 不过滤 approval_code ★

`tasks/search` 是按**用户**查任务，返回该用户的**全部**任务，不区分审批定义。
原代码把结果里的 instance_code 一股脑写进 `forms.jsonl`：

```go
for _, it := range items {
    if it.InstanceCode != "" && !seen[it.InstanceCode] { ... }   // ← 没有 approval_code 过滤
}
```

后果：跑一次 `recon`（path B），「<OTHER_APPROVAL_1>」的实例会混进 `forms.jsonl`，
后续 `extract` / `sync` 把它们当报销单抽取、入库、写进多维表格。

**修复**：按 `cfg.Feishu.ApprovalCode` 过滤，并打印过滤掉多少条。

### 2.2 `extract` 不校验实例归属 ★

`extract` 信任 `forms.jsonl` 或 `-instance` 入参。传一个别的表单的实例号，
它照样下载附件、跑 OCR、入库。

**修复**：以**实例详情的 `approval_code`** 为准做硬校验（比信任入参可靠）：

```
[1/1] <INSTANCE_D>
      ⛔ 跳过：该实例属于「<OTHER_APPROVAL_1>」，不是本项目绑定的审批定义
      ⛔ 跳过 1 单（不属于本项目绑定的审批定义）
```

### 2.3 已经在守的

`cmd/serve` 的两个事件入口（`handleInstanceFields` / `handleOtherEvent`）
**已经有** approval_code 过滤 —— 这两处原本就是对的。

---

## 3. 新增：启动时硬校验绑定（fail closed）

配置填错 `approval_code` 原本**不会有任何报错**：只会静默地收不到事件，
或收到别的表单的数据 —— 而且能正常跑起来，看不出异常。

新增配置项：

```yaml
feishu:
  approval_code: "<APPROVAL_CODE>"
  approval_name_expect: "<APPROVAL_NAME>"     # 留空 = 不校验
```

`serve` 启动时解析该 code 的**真实名称**并比对，对不上拒绝启动：

```
══ 财务常驻服务 ══
审批定义  : <APPROVAL_NAME>
            <APPROVAL_CODE>  [ACTIVE]

✗ 拒绝启动：期望绑定名称含 "采购审批"，实际是 "<APPROVAL_NAME>"
  要么改 config.yml 的 approval_code，要么改 approval_name_expect。
  先看清有哪些表单：go run ./cmd/approvals
```

`cmd/doctor` 的 [2/7] 检查也一并打印实际名称并做同样校验：

```
        实际绑定: "<APPROVAL_NAME>"  [ACTIVE]
✓ [2/7] 读审批定义
```

---

## 4. 验收

| 检查 | 结果 |
|---|---|
| 当前绑定是否为目标表单 | ✅ `<APPROVAL_CODE>` = <APPROVAL_NAME> |
| 订阅状态 | ✅ 已订阅（`1390007 subscription existed` = 本来就在，按成功处理） |
| 期望名称写错时是否拒绝启动 | ✅ exit 1 |
| 喂别的表单的实例给 extract | ✅ `⛔ 跳过`，未下载/未 OCR/未入库 |
| recon path B 过滤 | ✅ 已加 approval_code 过滤 |
| `go build` / `go vet` / `go test` | ✅ 全绿 |

---

## 5. 遗留

- **`-unsubscribe-all` 尚未执行**。它会把另外 3 个表单的订阅退掉。
  需要先确认那 3 个是否被其它系统（或同一应用的其它用途）订阅着 —— 退订是不可逆的。
  当前"只处理 26 赛季"已由代码层的 4 道过滤保证，**不依赖订阅隔离**，
  所以不退订是安全的。
- `approval_name_expect` 是**子串**匹配。「<APPROVAL_NAME>」能唯一匹配到目标；
  若将来出现「<APPROVAL_NAME>（无发票）」之类的兄弟表单，子串会同时命中两个，
  此时必须改用 `-match` 精确关键字并在 config 里写全名。
