# 权限体检结果与修复指引

> 用 `go run ./cmd/doctor` 逐项实测，2026-09-13。
> 目的：把"权限到底缺哪一项"从猜测变成可复现的结论。

---

## 1. 体检结果

```
配置文件: finance-router/config.yml

✓ [1/6] 获取 tenant_access_token    需 （app_id/app_secret 正确即可）
✓ [2/6] 读审批定义                  需 approval:approval:readonly
✓ [3/6] 读多维表格字段              需 bitable:app:readonly 或 base:field:read
✗ [4/6] ★ 写多维表格字段            需 bitable:app 或 base:field:create
        ★ scope 已开通，但【文档级权限】不足
        症状: HTTP 403 code=91403 msg=Forbidden
✓ [5/6] 解析 wiki 节点              需 wiki:wiki:readonly 或 wiki:node:read
− [6/6] 发消息（机器人）             需 im:message（未配置 admin_open_id，跳过）
```

**5 项通，1 项卡住。**

---

## 2. 为什么能断定「不是 scope 问题」

关键证据是**两类错误的形态不同**。同一台机器上，缺 scope 的调用会**明确列出所需 scope**：

| 测试 | 错误码 | 错误信息 |
|---|---|---|
| 发消息（scope 确实没给） | `99991672` | **`Access denied. One of the following scopes is required: [im:message:send, im:message, im:message:send_as_bot]`** + 申请链接 |
| 写多维表格字段 | `91403` | `Forbidden`（**不列任何 scope**） |

> 飞书对"缺 scope"会直接告诉你缺哪个；**不告诉你缺哪个，就是 scope 有了、是文档权限不够。**

另一个佐证：**读**多维表格是通的。说明应用**已经能访问**这个文档了——只是**只读**级别。

---

## 3. 需要你做的（二选一）

### 方案 A（推荐）：把应用加为 wiki 空间的「可编辑」成员

知识库的权限**继承自知识空间**，节点本身不能单独授权。所以要在**空间**一级加：

1. 打开该知识库（wiki）
2. 右上角 **「…」** 或 **设置** → **「成员管理」/「权限设置」**
3. **添加成员** → 搜索**你的应用名**（`<APP_ID>` 对应的那个自建应用）
4. 权限设为 **「可编辑」**（不要只给「可阅读」）

> 若搜不到应用：有些租户的 wiki 成员管理只允许添加"人"。
> 那就走方案 B。

### 方案 B：换一个普通多维表格（不在 wiki 里）

1. 在你的**云空间**（不是知识库）新建一个多维表格
2. 打开该表 → **分享** → 添加协作者 → 搜**应用名** → 给 **「可编辑」**
3. 把 URL 里的 `/base/<app_token>` 填进 `config.yml` 的 `feishu.bitable.app_token`

普通文档可以直接把应用加成协作者，这条路最不容易卡。

### 两个方案都试不通时

还有个兜底：**让应用自己创建文档**（应用创建的文档天然归应用所有，可写）。
但这需要 `drive:drive` 或 `bitable:app` 的创建文档能力，且文档会落在应用的独立空间里，
你需要再手动分享/移动到自己能看到的位置。**不推荐，除非前两个都失败。**

---

## 4. 验证方式

改完权限后跑同一条命令，看到这一行变 ✓ 即可：

```bash
cd finance-router && source ./devenv.sh
go run ./cmd/doctor -wiki-token <WIKI_NODE_TOKEN>
```

期望：

```
✓ [4/6] ★ 写多维表格字段
```

---

## 5. 权限通过后要执行的（已就绪，等权限）

```bash
# 1) 先看计划（默认 dry-run，不写入）
go run ./cmd/bitable-init -app-token <app_token> -table <table_id>

# 2) 确认无误后真建表
go run ./cmd/bitable-init -app-token <app_token> -table <table_id> -dry-run=false
```

它会：

| 动作 | 结果 |
|---|---|
| 重命名默认表的主字段 | `文本` → `审批实例号` |
| 新增 19 个字段 | 源表「识别结果_待审」齐 20 字段（含单选选项） |
| 新建数据表 | 「报销整合」 |
| 建 12 个字段 | 整合表字段齐全 |
| 打印两行配置 | 直接粘进 `config.yml` 的 `feishu.bitable` |

`bitable-init` 的特性：**幂等**（已存在的字段会跳过）、**默认 dry-run**（必须显式 `-dry-run=false` 才写）。

---

## 6. 顺带：新增的 `cmd/doctor`

```
cmd/doctor   逐项体检外部能力，按错误码区分「缺 scope」与「文档权限不足」
```

它把本次调试的经验固化成工具：**以后再遇到权限问题，先跑 doctor，不要再猜。**

---

## 7. 待办清单

| # | 事项 | 谁 |
|---|---|---|
| 1 | 给应用**可编辑**的文档/空间权限（方案 A 或 B） | 你 |
| 2 | 补 `im:message` + 开机器人能力 + 配 `admin_open_id` | 你 |
| 3 | 跑 `bitable-init` 建两张表 | 我（等 1） |
| 4 | 把识别结果写进源表 | 我 |
| 5 | 轮询归档：源表`人工审核=通过` → 复制到整合表 | 我 |

---

*本文结论均为实测（doctor 输出、两类错误码对比）。修复步骤中"wiki 成员管理"的具体入口名称可能因租户版本略有差异。*
