# 配置与租户标识符的清理（含历史重写）

> 2026-09-13。起因是要求「config 和数据不要进 git，同时从仓库里去除 config 的记录并推送，
> 保持远程也没有 config 的数据」。

---

## 0. 先说结论

**`config.yml` 和 `data/` 从来没有进过 git。** 逐提交扫描确认：

| 值 | 是否曾出现在 git 历史 |
|---|---|
| `app_secret`（飞书应用密钥） | **0 次** |
| `api_key`（OCR 密钥 `sk-…`） | **0 次** |
| `config.yml` 文件本身 | 从未被跟踪 |
| `data/` / `backup/` | 从未被跟踪 |

所以**不需要轮换密钥** —— 真正致命的两项从未泄露。

但是顺着查下去，发现了**实际存在的问题**：`config.yml` 里的**标识符**已经散落在
文档和脚本里，而**这个仓库是公开的（public）**。

---

## 1. 实际泄露了什么

| 类别 | 数量 | 出现在 |
|---|---|---|
| 多维表格 app_token | 7 处 | docs + `scripts/pipeline.sh` |
| wiki 节点 token | 6 处 | docs |
| 表 ID（4 个以上） | 11 处 | docs + 帮助文本 |
| 审批定义 code（3 个） | 8 处 | docs + `cmd/serve/main.go` + **二进制** |
| 租户域名 | 7 处 | docs |
| 管理员 user id | 4 处 | docs |
| 部门 open id | 2 处 | docs |
| 本应用 app_id | 6 处 | docs |
| 真实审批实例号（4 个） | 4 处 | docs |
| 真实发票号（3 个） | 4 处 | docs + 代码注释 |

> 本文档本身也**不写这些值** —— 第一次写这篇文档时我把它们列了出来，
> 结果被自己的远程复验脚本抓到（见 §3.6）。

---

## 2. 一个我差点搞错的地方 ★

批量替换时，我把 `cli_9cb844403dbb9108` 也替换成了占位符 —— **这是错的**。

查飞书官方文档确认（[打开飞书审批](https://open.feishu.cn/document/applink-protocol/supported-protocol/open-an-approval-page)）：

> appId｜审批的应用 ID。其中**飞书品牌下审批的应用 ID 为 `cli_9cb844403dbb9108`**，
> Lark 品牌下审批的应用 ID 为 `cli_9c7cc8a9a9edd105`。

它是**平台公开常量**，所有租户都一样，不是本项目的 app_id。而且它是拼 applink 必需的 ——
替换掉会让写进多维表格的所有「申请编号」链接变成
`appId=<APPROVAL_MINIAPP_APP_ID>`，**直接坏掉**。

已恢复，并在 pre-commit 守卫里把它和 Lark 的值列入白名单。

> 教训：批量替换标识符时，必须先分清「租户数据」和「平台常量」。

---

## 3. 做了什么

### 3.1 源码层：不再硬编码

| 位置 | 原来 | 现在 |
|---|---|---|
| `cmd/serve/main.go` 的 `fakeEventObj` | 写死 approval_code | 由调用方传入（取自 config） |
| `cmd/approvals` 的 `-match` 默认值 | 写死审批名 | 默认取 config 的 `approval_name_expect`，两者都空则报错退出 |
| `internal/pipeline/extract.go` | 写死平台常量 | 保留（公开常量）+ 新增 `feishu.approval_app_id` 可覆盖（Lark 品牌用） |
| `scripts/pipeline.sh` 帮助文本 | 真实表格 URL | 占位符 |

`bin/serve` 是编译产物，源码里的硬编码会被编译进去 —— 所以**改完必须重新构建**，
否则二进制里仍有真实 code。

### 3.2 文档层：全部替换为占位符

统一的占位符命名，如 `<BITABLE_APP_TOKEN>` / `<TABLE_SUBMISSION>` /
`<APPROVAL_CODE>` / `<INSTANCE_A>` / `<DEPT_ID>` / `<ADMIN_USER_ID>` / `<TENANT>.feishu.cn`。

共修改 25 个文件。做的是**两遍**扫描：第一遍精确匹配，第二遍才发现
`<APPROVAL_CODE>-...` 这类**截断形式**漏了 —— 只做一遍会以为清干净了。

### 3.3 历史重写

即使工作区清理干净，**旧提交里还有**。而且 `bin/serve` 是二进制，
文本替换对它无效（旧二进制里嵌着 approval_code）。

做法：**把历史重建为单个提交**。

- 旧历史只有 5 个提交（`first commit`/`init` + 今天的 3 个），历史价值低；
- 重建后 `.git` 从 17M+ 降到 **11M** —— 顺带解决了「每次部署往 git 塞 18MB 二进制」的膨胀；
- 旧历史打了 bundle 备份留在本机 `.scratch/`（不进 git）。

```
$ git log --oneline
b696f50 fix(push-to-robot): scp/ssh 未走 sshpass，密码认证失效
9a438e4 chore: 重建二进制（版本戳指向真实提交）+ 新增开发机直推脚本
a80cd97 chore: 加 pre-commit 守卫…
4531d08 feishu 报销自动化：查重、审批绑定、机器人部署（历史已重建）
```

### 3.4 验证：从远程全新克隆

只在本地验证不算数 —— 必须看**远程实际有什么**：

```
$ git clone https://github.com/coffee-xzm/finance.git /tmp/verify-clone
$ cd /tmp/verify-clone && grep -rqa <每个真实值> .
  （全部无命中）
$ 密钥扫描
  ✓ 未发现 sk-vwyhuz…
  ✓ 未发现 iCU9qmkg…
$ ls finance-router/config.yml   → 不存在
$ ls -d data                      → 不存在
```

### 3.5 pre-commit 守卫

事后清理不如事前拦住。`.githooks/pre-commit` 两层检查：

1. **路径级**：`config.yml` / `data/` / `backup/` / `*.db` / `*.bundle` / `logs/` / `*.pem` 直接拒。
2. **内容级**：扫暂存内容里的 API key、`app_secret`、`table id`、部门/用户 open id、
   租户域名、app_token 形态。

实测四种情况：

| 提交内容 | 结果 |
|---|---|
| 强行 `git add -f config.yml` | ✗ 拒绝 |
| 含 `sk-abc…` 的假密钥 | ✗ 拒绝 |
| 含真实表 ID | ✗ 拒绝 |
| 正常改动 | ✓ 放行 |

启用（每个 clone 一次）：`git config core.hooksPath .githooks`

---

## 4. 机器人侧的收尾

历史被重写后，机器人的 `git pull --ff-only` 必然失败（不是 fast-forward）。
而且机器人到 github.com 的链路**很不稳**：

```
error: RPC 失败。curl 16 Error in the HTTP2 framing layer
fatal: 无法访问 'https://github.com/…'：Failed to connect to github.com port 443
       after 132496 ms: 连接超时
```

拉一次 18MB 要两三分钟且常常失败。所以新增了**开发机直推**：
`finance-router/scripts/push-to-robot.sh` —— 走局域网传 git bundle，一两秒完成。

```
→ 将推送提交 b696f50
→ bundle 11M
→ 已传输
HEAD 现在位于 b696f50
→ 重启服务
✓ 已启动（PID 19037）
状态    : ✓ 运行中
二进制  : ✓ 一致（无需重启）
✓ 推送完成
```

用 `fetch + reset --hard` 而不是 `pull`：历史可能被重写过，ff-only 会失败。
机器人的 `config.yml` 全程未受影响（`reset --hard` 不碰被忽略的文件）。

---

## 5. 结论与遗留

**结论：**
- `app_secret` / `api_key` 从未进过 git → **无需轮换密钥**。
- 租户标识符已从工作区**和全部历史**清除，远程经全新克隆验证。
- pre-commit 守卫已上线，路径与内容两层拦截。
- `.git` 体积 17M+ → 11M。

**遗留：**
- **GitHub 可能仍缓存旧对象**。force push 后，旧提交通常不再可达，但
  GitHub 不保证立即物理删除，且若有人在此之前 fork/clone 过则无法追回。
  考虑到泄露的只是标识符（无密钥），风险可接受；若要绝对干净，只能**删库重建**。
- **Lark 品牌**用户需在 config 里设 `feishu.approval_app_id: cli_9c7cc8a9a9edd105`，
  否则 applink 指向飞书品牌的审批小程序。
- 机器人上 `/tmp/finance-push.bundle` 用完即删（脚本里已处理）。
- 机器人仍可能因为外网不稳而无法 `git pull`；日常部署建议直接用 `push-to-robot.sh`。
