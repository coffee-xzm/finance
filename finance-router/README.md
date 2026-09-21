# finance-router

财务流程重构的本地服务。当前处于**并行只读侦察期**。

## 现状

| 项 | 状态 |
|---|---|
| 提交入口 | **复用现有飞书审批**，不改任何定义、不改前端 |
| 本地服务权限 | **纯只读**（`approval:approval:readonly`） |
| 写入范围 | 只写本地磁盘 + 自己个人文件夹的测试多维表格；**绝不写现有表** |
| 旧自动化 | 触发源 = 审批通过；**跑通后下岗**（现在不能动它） |

## 目录

```
finance-router/
├── cmd/recon/           只读侦察：读审批定义 + 历史实例 → 本地报告
├── cmd/pdf2png/         PDF 票据 → PNG（Qwen3-VL 不支持 PDF 输入）
├── cmd/bitable-schema/  输出测试多维表格的**建议**表结构清单（我设计的）
├── cmd/bitable-read/    只读读取**已有**多维表格的真实结构（支持 wiki 链接）
├── cmd/extract/         下载三张图 → PDF转PNG → 抽取（产可验证语料）
├── cmd/sync/            抽取结果 → 写入飞书源表（按 sha256 幂等）
├── cmd/archive/         源表「人工审核=通过」→ 复制到整合表
├── cmd/doctor/          逐项体检外部权限（区分 scope / 数据范围 / 字段权限）
├── cmd/db/              本地 SQLite：初始化迁移、统计、查重
├── cmd/bitable-init/    在指定文档里建好源表+整合表两张表
├── cmd/download-probe/  附件下载链路探测（P0 门禁：临时链接/权限/extra/字节数）
├── cmd/export/          按清单或视图导出附件 → 命名 → zip + manifest + 审计
├── internal/bitable/    表结构定义（字段类型编号已核实）+ 字段取值读取
├── internal/config/     配置加载（gopkg.in/yaml.v3）
├── internal/export/     选择性下载主流程（清单解析/下载/落盘/审计）
├── internal/feishu/     飞书 API 最小客户端（只读）
├── internal/naming/     记录 → 文件名/目录（与 bitable-plugin 共用冻结向量）
├── config.yml           真实配置（不进 git）
├── config.example.yml   配置结构（进 git，唯一文档来源）
└── data/                运行产物（不进 git）
```

## 快速开始（手动跑一遍）

```bash
cd finance-router
bash scripts/pipeline.sh 5            # 跑 5 个实例：体检→建库→抽取→落表→通知
bash scripts/pipeline.sh 5 --reset    # 先清空本地库（演示"首次入库"）
```

跑完是**人工环节**：在飞书里核对「核对结果」，把「人工审核」改成「通过」，再：

```bash
go run ./cmd/archive
```

> ⚠️ **目前没有常驻服务** —— 架构稿里的 `finance-router` 长驻进程
> （订阅审批事件、自动触发、outbox 投递）**尚未实现**。
> 现在是一组**手动命令**，按上面的顺序跑。

## 环境

本机 `~/.cache/go-build` 与 `~/go/pkg` 位于**只读挂载**，Go 默认写入会失败。
**每次执行前先 source：**

```bash
cd finance-router
source ./devenv.sh    # 把 GOCACHE/GOMODCACHE 指到仓库内，关 CGO
go build ./...
```

## 配置

```bash
cp config.example.yml config.yml
# 填入 feishu.app_id / app_secret，以及下面【两条发现路径之一】
```

**发现审批实例有两条路，配一条即可：**

| 路径 | 需要 | 覆盖范围 | 说明 |
|---|---|---|---|
| **A · 按定义枚举**（推荐） | `approval_code` | 该审批的**全部**实例 | `GET /approval/v4/instances`；**此参数不敏感**，会出现在审批链接与浏览器 URL 里 |
| **B · 按用户任务枚举** | `user_id` | 只是**此人是审批人**的那些任务 | `POST /approval/v4/tasks/search`；**不需要 approval_code** |

> ⚠ `instance/list`（路径 A）的 `approval_code` / `start_time` / `end_time` **三个都是必填**，
> 且**单次查询时间范围不能超过 10 小时** —— 工具会自动切片，用 `-from/-to` 指定区间（默认最近 90 天）。

缺必填项时程序明确报错，不会静默用默认值。

## 命令

### 1. 只读侦察

```bash
go run ./cmd/recon                        # 默认最近 90 天
go run ./cmd/recon -from 2026-01-01 -to 2026-09-13
```

三件事，**全部 GET/POST 只读，不写飞书任何数据**：

1. `GET /approval/v4/approvals/:approval_code` → 表单控件结构、流程节点（**仅路径 A**）
2. 发现实例：
   - 路径 A：`GET /approval/v4/instances?approval_code=&start_time=&end_time=`（≤10h 切片 + 分页）
   - 路径 B：`POST /approval/v4/tasks/search`（body 只传 `user_id`）
3. `GET /approval/v4/instances/:instance_code` → 表单值（限速 5 req/s）

**产物全部落在 `data/recon/`：**

| 文件 | 内容 |
|---|---|
| **`report.md`** | **给人看的**：字段清单、类型分布、状态分布、附件控件识别、关联键判断 |
| `definition.json` | 审批定义原始 JSON（仅路径 A） |
| `instances_list.jsonl` | 实例列表原始响应（仅路径 A） |
| `tasks_search.jsonl` | 任务搜索原始响应（仅路径 B） |
| `instances_detail.jsonl` | 每条实例详情原始 JSON（含 `form`） |
| `forms.jsonl` | 解析后的表单值（一行一实例） |

常用参数：`-max-detail 50`（限制详情条数）、`-max-windows 240`、`-out data/recon`。

### 2. PDF 转 PNG

```bash
go run ./cmd/pdf2png -in invoice.pdf -out out -dpi 150
```

**为什么需要**：SiliconFlow 上 Qwen3-VL 系列**不支持 PDF 输入**（文档里 PDF 只标注在 DeepSeek-OCR 上）。
所以 PDF 必须在本地光栅化。

**为什么 DPI 重要**：Qwen 系列视觉 token = `ceil(h/28)*ceil(w/28)`，DPI 直接决定成本：

| DPI | A4 像素 | 视觉 token |
|---|---|---|
| 120 | 992×1404 | 1,836 |
| **150** | 1240×1755 | **2,835** |
| 200 | 1653×2339 | 5,040 |
| 300 | 2480×3509 | 11,214 |

**成本分层**：`detail=low` 恒为 448×448 ≈ **256 token**，比 high 便宜约 11 倍。
但 448×448 下发票票号必糊 → **发票用 `high`，订单/付款截图用 `low`**（见 `detail_by_kind`）。

依赖系统 `pdftoppm`（`poppler-utils`，本机已装）。

### 3. 多维表格表结构

```bash
go run ./cmd/bitable-schema              # Markdown 清单（照着手建）
go run ./cmd/bitable-schema -format csv  # CSV
```

输出 4 张表的完整字段定义（字段名 / 类型 / 官方类型码 / 选项 / 说明）。
**默认只打印，不调用任何写接口** —— 测试期先在 UI 手工建表。

`config.yml` 里要填的两个值：

| 值 | 取法 |
|---|---|
| `bitable.app_token` | 多维表格 URL 形如 `https://xxx.feishu.cn/base/<app_token>?table=<table_id>`，`/base/` 后面那段；**整份文档共用** |
| `bitable.tables.*` | URL 里 `?table=` 后面那段（`tbl` 开头），**每张表一个** |

**别忘了**：把应用加为该文档的**协作者**，否则 API 写入 403；测试期**先别开高级权限**。

> ⚠️ `bitable-schema` 输出的是**我设计的建议结构**，不是从任何真实表格推导的。
> 要沿用现有命名，请先跑下面的 `bitable-read` 读出真实结构再对照。

### 4. 读取已有表格的真实结构（只读）

```bash
go run ./cmd/bitable-read -app-token <app_token>          # 读全部表
go run ./cmd/bitable-read -app-token <app_token> -table tblXXXX
```

只调两个只读 GET：列数据表、列字段。权限 `base:table:read` / `base:field:read`
或 `bitable:app:readonly` 即可。**不写任何数据、不改任何字段。**

输出 `data/bitable/schema.md`（字段名/field_id/type/ui_type/选项）与 `schema.json`。

用途：把"测试表该建哪些字段"从**我拍脑袋设计**改成**照着你实际在用的表来**。

### 5. 取出三张图并抽取（产验证语料）

```bash
go run ./cmd/extract -limit 2           # recon 抓到的前 2 个实例
go run ./cmd/extract -instance <code>   # 指定实例
```

流程：**现取**实例详情（附件 URL 只有 24h 有效期）→ 下载 → 算 sha256/文件名/Content-Type
→ PDF 转 PNG → 若有 `ocr.api_key` 则调 SiliconFlow 抽取（否则标 `SKIPPED`）。

产物：`data/extract/manifest.jsonl` 与 `data/extract/files/<实例号>/`。

> ⚠️ OCR token 写在 `config.yml` 的 **`ocr.api_key`**。
> 取值：<https://cloud.siliconflow.cn/> → 左侧「API 密钥」→ 新建 → `sk-...`

### 6. 落库与人工审核闭环

```bash
go run ./cmd/bitable-init -app-token <token> -table <table_id>   # 建两张表（默认 dry-run）
go run ./cmd/sync                                                # 抽取结果 → 源表
#   ↓ 人在源表把「人工审核」改成「通过」
go run ./cmd/archive                                             # 源表 → 整合表
```

| 命令 | 幂等依据 | 说明 |
|---|---|---|
| `sync` | **图片SHA256** | 同一文件重跑不会产生重复行 |
| `archive` | 源表 **`已归档`复选框** | 先复制后标记；重复运行不重复归档 |

**人只看两个字段**：`人工审核`（待审/通过/驳回）与 `审核备注`；其余字段由服务权威写入。

## 权限体检

```bash
go run ./cmd/doctor -wiki-token <wiki_node_token>
```

按错误码区分两类失败：`99991672`（scope 未开通，错误里带申请链接）
vs `91403`（scope 已开、**文档级权限不足**）。

### 7. 本地 SQLite（防重复报销的数据库级保证）

```bash
go run ./cmd/db -init            # 建库 + 应用迁移
go run ./cmd/db -stats           # 统计
go run ./cmd/db -dup <sha256>    # 查某张图是否已被占用
```

`extract` 会**自动入库**。核心约束在 `internal/store/migrations/0001_init.sql`：

```sql
-- ★ 同一张图只能进库一次
CREATE UNIQUE INDEX uq_evidence_sha256 ON evidence(sha256);

-- ★ 审计 append-only
CREATE TRIGGER trg_audit_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
```

### 8. 选择性下载（按清单导出附件）

口径：**人在飞书里勾选行 → 导出成清单 → 服务端下载、命名、打包、留审计**。
详见 `docs/30-review/32`（计划）与 `docs/30-review/35`（插件侧实操单）。

```bash
# P0 门禁：先证明这条记录的附件真的下得下来（退出码 0 = 通过）
go run ./cmd/download-probe -table integrated -record recXXXXXXXX
go run ./cmd/download-probe -table integrated -instance 7DB9ADCF... -out data/probe

# P1 清单导出（永远先干跑）
go run ./cmd/export -table integrated -records-file list.csv -out data/export/2026-08 -dry-run
go run ./cmd/export -table integrated -records-file list.csv -out data/export/2026-08
go run ./cmd/export -table integrated -view <view_id> -out data/export/2026-08   # 备选口径
go run ./cmd/export -batches                                                     # 看历史批次（不联网）
```

| 纪律 | 落点 |
|---|---|
| **只存 `file_token`，不缓存临时链接**（链接 24h 失效） | `export_item` 表 + `internal/feishu/download.go` |
| **失败不静默**：空附件行 / 缺失字段 / 下载失败都单独统计 | `manifest.csv` + 汇总行 + `NO_ATTACHMENTS` 告警 |
| **命名与插件共用一份契约** | `internal/naming` ↔ `bitable-plugin/src/core/naming.ts`，同跑 `testdata/naming-cases.json` |
| **审计底账**（操作人/时间/事项） | `export_batch` + `export_item`（迁移 0006） |

**为什么这条索引重要**：飞书多维表格**没有任何唯一索引能力**，
"先查重再写入"在飞书上天然是 TOCTOU 竞态（两人同时提交同一张发票，两次查重都会通过）。
放到 SQLite 后，这变成**数据库保证**：

```
12 个并发提交同一张图 → 恰好 1 次成功，11 次被拒（见 internal/store/store_test.go）
```

整单在一个事务内写入，任一条证据冲突则整单回滚 —— **不会出现半写状态**。

## 端口

**本项目不需要任何入站端口** —— 事件走出站长连接，API 调用也是出站；两个命令都是纯 CLI，不监听。

配置里的 `server.listen` 只是将来健康检查的占位，默认 `127.0.0.1:18080`
（本机 4000/4001/3080/11434/7001/7897 等已被占用；8080 空闲但为避开撞车未采用）。

## 纪律

1. **只读期不订阅现有定义**（`subscribe.approval: false`）→ 物理上不会收到现有审批的事件。
2. 若日后订阅，代码里**必须按 `approval_code` 过滤**，否则会把老单据当新单据处理。
3. **绝不写现有的多维表格**，任何字段都不写。
4. 票据原图不长期落盘，只保留 `sha256` 指纹。
