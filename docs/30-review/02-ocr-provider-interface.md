# 识别层接口设计（Provider 化 · SiliconFlow Qwen3-VL）

> **决策来源（用户已拍板，2026-09-12）**
> 1. 识别层**远程调用 SiliconFlow**，模型 `Qwen/Qwen3-VL-30B-A3B-Instruct`；
> 2. 本地**不部署模型**，只留**接口**；
> 3. 用户配置存 `config.yml`，**不跟踪 git**，仓库只留一个**空文件**占位。
>
> 本文取代 `02-architecture-v0.1.md` §4.3 中"三级可换 OCR"的**本地部署预设**——接口形状保留，
> 但**只有远程实现需要接**；本地 OCR 从"计划内"降为"仅当触发条件成立时再接"（架构稿 §4.3 第 ③ 级）。
>
> 关联：`docs/30-review/01-plan-review-ocr-matching.md`（为何识别是瓶颈）、`docs/30-review/03-approval-routing.md`（审批如何回流）。

---

## 1. 为什么"只留接口"是对的

| 事实 | 影响 |
|---|---|
| 本机 GPU 为 **RTX 4060 Laptop 8 GB**（实测 `/proc/driver/nvidia/gpus` → `NVIDIA GeForce RTX 4060 Laptop GPU`，驱动 580.159.03，CUDA 13.0） | 8 GB 跑 4B 勉强、**8B/30B 不可能**。而 `Qwen3-VL-30B-A3B` 权重量级（31B 总参）本地需量化到 Q4 后仍要 ~17 GB 以上 |
| 数据量：年 **900–1800 张图**（300–600 单 × 3 图），**日均 3–5 张** | 极低吞吐。本地硬件投入摊不平运维时间；远程边际成本约**几元到几十元/年** |
| 远程模型能力**高于**任何 8 GB 上跑得动的本地模型，且**一张 prompt 覆盖三类图** | 识别质量直接决定三单匹配能否成立 → 远程是**质量最优解**，不只是省事 |

**结论**：同时降低了成本、提高了识别质量、去掉了 Python+TF 运维包袱。**唯一代价是图片离开本机**——
已确认可接受（R1 关闭）。

> `guanshuicheng/invoice` 的价值因此只剩"离线兜底"，而其维护状态（TF1.13 / Python3.6 / 百度网盘权重）
> 使它连兜底都不合适 → **不接**。需要离线时改用 RapidOCR/PaddleOCR sidecar。

---

## 2. SiliconFlow 接口事实（本轮一手核实）

| 项 | 值 | 来源 |
|---|---|---|
| Endpoint | `POST https://api.siliconflow.cn/v1/chat/completions` | [创建对话请求](https://api-docs.siliconflow.cn/docs/api/chat-completions-post) |
| 鉴权 | `Authorization: Bearer <API Key>` | 同上 |
| 结构化输出 | ✅ `response_format: {"type":"json_schema","json_schema":{...}}`，也支持 `{"type":"json_object"}` | 同上 |
| **VLM 图片传法** | `content[]` 里 `{"type":"image_url","image_url":{"url":"<URL 或 base64 data URI>","detail":"auto\|low\|high"}}` | [多模态输入](https://docs.siliconflow.cn/docs/userguide/capabilities/multimodal-vision) |
| **重要** | `/v1/files` 上传**只接受 `purpose=batch`** → **VLM 图片不能走文件接口**，必须 base64 或公网 URL | [上传文件](https://api-docs.siliconflow.cn/docs/api/files-post) |
| 可调开关 | `enable_thinking`（bool）、`thinking_budget`、`temperature`、`max_tokens`、`top_p`、`stop` | 创建对话请求 |
| 响应追踪 | 响应头 `x-siliconcloud-trace-id` → **建议入库**（排障用） | 同上 |

### 2.1 针对票据识别的参数决定

| 参数 | 取值 | 理由 |
|---|---|---|
| `model` | `Qwen/Qwen3-VL-30B-A3B-Instruct` | 用户指定 |
| `response_format` | **`json_schema`**（首选；失败降 `json_object`） | 结构化输出是机器可解析的前提 |
| `temperature` | **`0`（或 0.1）** | 抽取任务要确定性，**不要**用默认 0.7 |
| `enable_thinking` | **`false`** | 票据字段抽取是感知任务，不需要思维链；开着会**显著增加 token 成本与延迟** |
| `max_tokens` | **`1024`** | 单张图输出就一个 JSON，不需要更多；防止模型絮叨 |
| `detail` | **`high`** | 发票小字（票号、税号、校验码）必须高分辨率才读得到；`low` 会丢字段 |
| 图片预处理 | **长边 ≤1280 px、JPEG q≈85** | 视觉 token 数随分辨率近似二次增长。**这是最有效的省钱手段**；配合 `detail:"high"` 效果最好 |

> **`detail` 与压图的组合要实测**：压到 1280 + `detail:high` 是起点，若发票票号读不到，再单独对发票走 1600 px。

---

## 3. 接口契约（Go）

**设计原则**：抽取层与匹配层**解耦到 JSON 边界**。Provider 只负责"图 → 字段"，**不做任何判定**；
匹配层只读 `Evidence`，不知道自己面对的是云端还是本地。换引擎 = 加一个实现 + 改一行配置。

```go
// package evidence

// Kind 由模型判定，不要由调用方传（调用方可能传错，且模型判定是免费的）。
type Kind string

const (
    KindInvoice Kind = "invoice" // 发票：强版式，有票号
    KindOrder   Kind = "order"   // 订单详情页截图
    KindPayment Kind = "payment" // 微信/支付宝转账·账单截图
    KindUnknown Kind = "unknown" // 模型无法判定
)

// Evidence 是"一张图"的抽取结果。它刻意不含任何"通过/不通过"的判断。
type Evidence struct {
    Kind         Kind    `json:"kind"`
    Confidence   float64 `json:"confidence"`   // 0~1，模型自评；低置信度只影响展示，不影响入库
    AmountCent   *int64  `json:"amount_cent"`  // 三张图都有 → L2 匹配的主锚点；nil = 没读到
    Currency     string  `json:"currency"`     // 默认 CNY
    Datetime     string  `json:"datetime"`     // YYYY-MM-DD；订单日/付款日/开票日
    Counterparty string  `json:"counterparty"` // 发票销方 / 订单店铺 / 转账收款方
    // 仅发票：L1 唯一性的输入
    InvoiceCode string `json:"invoice_code"`
    InvoiceNo   string `json:"invoice_no"`
    CheckCode   string `json:"check_code"`
    SellerTaxID string `json:"seller_tax_id"`
    // 仅订单
    OrderNo string `json:"order_no"`
    Items   []Item `json:"items,omitempty"`
    // 审计与追溯（本地权威）
    ImageSHA256 string `json:"image_sha256"` // 原图指纹，入库前算好传入
    Provider    string `json:"provider"`     // "siliconflow-qwen-vl" / "feishu-vat" / "rapidocr"
    Model       string `json:"model"`        // 实际调用的模型 id（可回放"当时用的是哪个"）
    TraceID     string `json:"trace_id"`     // x-siliconcloud-trace-id
    LatencyMS   int    `json:"latency_ms"`
    RawResponse string `json:"raw_response"` // 原始响应；入库时按需截断
    Error       string `json:"error,omitempty"`
}

type Item struct {
    Name string `json:"name"`
    Qty  int    `json:"qty,omitempty"`
}

// Provider：换引擎的唯一接缝。实现必须满足 §4 的三条硬约束。
type Provider interface {
    Name() string
    Extract(ctx context.Context, img ImageRef) (*Evidence, error)
}

type ImageRef struct {
    SHA256    string
    Bytes     []byte // 压图后的 JPEG；SiliconFlow 走 base64 data URI
    MediaType string // image/jpeg
}
```

**调用点只有一处**：M2 evidence 在收到 `intake.submitted` 后，对每张图调一次 `Extract`
（**上限 3 并发**，单 worker）。结果落 `evidence` 表，再进入 L1–L4 匹配（评审稿 §2.4）。

---

## 4. Provider 的三条硬约束（比接口签名更重要）

### 4.1 结构化输出必须"机器可解析"，不能靠祈祷

- 请求带 **`json_schema`** 强制结构化输出。
- **仍然要写降级解析**：模型偶尔会在 JSON 外包 ```json 代码块或加一句"好的，以下是识别结果"。
  → ① 直接 `json.Unmarshal` → ② 剥 ``` 代码块 → ③ 截取**第一个 `{` 到最后一个 `}`** → ④ 全失败则 `Error` 非空、字段全 nil。
- **prompt 必须要求：读不到的字段返回 `null`，禁止猜测。**
  这是本设计最重要的一句约束——**"读不到"必须能表达出来**，否则匹配层会把幻觉金额当成真金额去比对（比读不到危险得多）。

### 4.2 绝不在 Provider 层做退回/拦截

Provider 的失败语义只有两种：返回 `Evidence`（可能有 nil 字段）或返回 `error`。
**两者都不会导致提交被退回**——按评审稿 §3，"不匹配就退回、不入库"已反转为
"全部入库 + 标记 `MATCHED/SUSPECT/DEFECTIVE`"。抽取失败同样**入库并标 `NEEDS_MANUAL`**，
只私聊问队员 2 个问题（金额多少 / 哪家商户），**不要求重填整表**。

### 4.3 结果必须可回放

`Provider` + `Model` + `TraceID` + `RawResponse` 四个字段一起入库。
同一条记录三个月后出现争议时，要能回答"当时是谁识别的、原始响应是什么"。
**这也是"供应商可换"的前提**：换模型后新旧记录不能混为一谈，否则无法比较质量。

---

## 5. 工程细节（远程调用的真实坑）

| 项 | 建议 | 理由 |
|---|---|---|
| **压图** | 上传前长边 ≤1280 px、JPEG q≈85 | 视觉 token 随分辨率近似二次增长；**最有效的省钱手段** |
| **传图方式** | **base64 data URI**（`data:image/jpeg;base64,...`） | `/v1/files` 只接受 `purpose=batch`，图片走不了；base64 膨胀 33% 但压图后完全可接受 |
| **并发** | **单 worker、3 并发**，串行落库 | 与飞书"同表禁止并发写"同构；日均 3–5 张本不需要并发 |
| **重试** | 仅对可重试错误（超时 / 5xx / 429）指数退避重试 2 次；**4xx 与内容拒绝不重试** | 避免把无效请求打成费用 |
| **超时** | 每张图 60 s；整单 90 s 内出结果，超时走"先建审批、卡片补发结论" | 与审批链路配合（见 `03-approval-routing.md`） |
| **幂等/缓存** | 以 `image_sha256` 为键缓存识别结果：同一张图重传（催办后重传）**直接命中缓存、不重复计费** | 队员重传同一张图是高频行为，**第二个省钱手段** |
| **配置外置** | `provider`/`model`/`base_url`/`api_key`/`max_edge_px`/`timeout`/`detail` 全在 `config.yml` | "换引擎改一行配置"，兑现架构稿 §6.4 |
| **密钥** | API key 只从 `config.yml`（git 不跟踪）读，**不入库、不进日志、不入 Git** | 基本纪律 |

---

## 6. 配置约定（`config.yml`）

**约定（用户指定）**：真实配置放 `config.yml`，**不跟踪 git**，仓库只留一个**空文件**占位。
本仓库当前**还不是 git 仓库**（`git rev-parse` 失败），因此首次 `git init` 时必须同时落地 `.gitignore`。

```
finance-router/
├── config.yml            ← 空文件，进 git（占位）
├── config.example.yml    ← 带注释的结构示例，进 git（唯一文档来源）
├── .gitignore            ← 忽略 config.yml 与运行期产物
└── data/
```

`.gitignore`（建议最小集）：

```gitignore
# 真实配置（含 API key 与飞书密钥）
config.yml

# 运行期数据
data/
backup/
*.db
*.db-wal
*.db-shm

# 日志
*.log
logs/
```

`config.example.yml`（结构示例，**不含真实值**）：

```yaml
server:
  listen: "127.0.0.1:8080"      # 仅本机；无入站需求，长连接为出站

feishu:
  app_id: ""
  app_secret: ""
  approval_code: ""             # 后台手工建的审批定义 code
  bitable:
    app_token: ""
    tables: { submission: "", evidence: "", ledger: "", budget: "" }
  subscribe:                    # 长连接订阅（启动时自动 subscribe）
    approval: true
    enable: true

ocr:
  provider: "siliconflow-qwen-vl"
  base_url: "https://api.siliconflow.cn/v1"
  api_key: ""                   # ← 唯一必填的密钥
  model: "Qwen/Qwen3-VL-30B-A3B-Instruct"
  response_format: "json_schema"   # 失败自动降级 json_object
  enable_thinking: false
  temperature: 0.0
  max_tokens: 1024
  image_detail: "high"
  max_edge_px: 1280
  jpeg_quality: 85
  timeout_seconds: 60
  concurrency: 3
  retry: { max: 2, backoff_seconds: [2, 8] }

matching:
  amount_tolerance_cent: 1      # L2 金额容差（分）
  payment_after_order_days: 3   # L3：付款日 ≤ 订单日 + N
  counterparty_threshold: 0.72  # L4 字符串相似度阈值

notify:
  cadence_days: [3, 7, 14, 30]  # 催办节奏
  max_per_person_per_week: 2

paths:
  db: "data/finance.db"
  backup_dir: "backup"
  backup_keep: 14
```

> `.example` 文件是**唯一的结构文档**：加配置项必须同时改 `.example`，否则视为未完成。

---

## 7. 风险与未决

| # | 风险/未决 | 处置 |
|---|---|---|
| **R1** | ~~票据图片离开本机~~ | **已关闭（用户确认可接受）** |
| R2 | `Qwen3-VL-30B-A3B` 对**订单/转账截图**的字段抽取准确率未知（发票上普遍很好） | 用真实样本做小实验：只比 `amount_cent` 与 `counterparty`。**在写匹配代码之前做**（评审稿 §8.2） |
| R3 | 供应商限流/故障 | Provider 接口使替换成本 = 加一个实现；`NEEDS_MANUAL` 通道保证故障时业务不停 |
| R4 | 成本随分辨率/张数失控 | 压图 + sha256 缓存 + 单 worker 三条已封住；再加**月度调用计数**告警 |
| R5 | 原架构稿 §4.3 仍写"飞书原生优先 → 视觉模型 → sidecar" | **需同步修订**：飞书 `vat_invoice/recognize` 认不了订单/转账截图，且智能文档解析按**次**计配额（免费版 100 次/月），一单 3 图即接近上限 → 不适合主路径 |
| R6 | `detail:"high"` + 1280 px 的组合对发票小字的实际可读性 | 沙箱实测；读不到则仅对发票放宽到 1600 px |

---

## 8. 与既有架构的差异（需要回填的文档）

| 文档 | 原内容 | 修订为 |
|---|---|---|
| `02-architecture-v0.1.md` §4.3 | "三级可换：①飞书原生 ②视觉模型 ③PaddleOCR sidecar"，本地 OCR 降级为 P4 | **主路径 = SiliconFlow Qwen3-VL-30B-A3B**；飞书原生 = 发票备选；sidecar = 仅离线需求时。**接口形状不变，实现顺序变了** |
| `02-architecture-v0.1.md` §6 技术选型"OCR"行 | 同上 | 同上；补充"上传前压到长边 ≤1280 + `detail:high`" |
| `02-architecture-v0.1.md` §2.1 OCR 行 | "飞书原生优先" | 改为"远程视觉模型优先"（理由：三类图覆盖 + 飞书配额按次） |
| `03-open-decisions.md` §3 | 未列 OCR 供应商风险 | 增加 R2（订单/转账截图准确率待实测）、R6（分辨率组合待实测）；R1 关闭 |

> 本次只新增本文与 `03-approval-routing.md`；上述修订**待你确认后**再改前置文档，避免半改状态。

---

*本文为接口设计，不含实现代码。决策依据：用户 2026-09-12 拍板"远程调用 SiliconFlow `Qwen/Qwen3-VL-30B-A3B-Instruct`，本地只留接口"；SiliconFlow 接口事实见 §2 引用的一手文档。*
