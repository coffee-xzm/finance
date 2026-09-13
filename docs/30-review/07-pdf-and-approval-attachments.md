# PDF 票据链路与审批附件控件（已核实）

> 回答用户 2026-09-12 的四个问题中的两个技术问题：
> ① 发票一般是 PDF，能兼容吗？② 跑起来后产物放哪？
> 并**关闭** `06-direct-approval-initiation.md` 里的待实测项 **T3（审批表单能否放附件）**。

---

## 1. 结论速览

| 问题 | 结论 |
|---|---|
| Qwen3-VL 能直接吃 PDF 吗？ | **不能。** SiliconFlow 文档「支持模型概览」里 **PDF 只标注在 DeepSeek-OCR 上**；Qwen3-VL 只有「视觉 + 视频」，没有 PDF |
| 所以要拆成 PNG？ | **对，且必须在本地拆**（去 PDF 化）。本机工具链已就绪，已验证可用 |
| 审批表单能放图片吗？ | **能。** 控件类型 `image`（图片）与 `attachmentV2`（附件）**都存在于审批定义表单** |
| 它们是「创建实例 API 不支持的控件」吗？ | **不是。** 不支持清单只有 `tripGroup`/`hrOnboardingGroup`/`hrRegularateGroup`/`remedyGroupV2`/`hrJobAdjustGroup`/`hrOffboardingGroup` 六个 HR 控件组 |

**T3 关闭** → 三张图确实可以走审批附件，三单匹配在复用现有审批的前提下**成立**。

---

## 2. PDF 去化：本机工具链（已实测）

| 工具 | 路径 | 用途 |
|---|---|---|
| **`pdftoppm`** | `/usr/bin/pdftoppm` | **首选**：PDF → PNG |
| `pdfinfo` | `/usr/bin/pdfinfo` | 看页数/尺寸，判断多页票 |
| `pdftocairo` | `/usr/bin/pdftocairo` | 备选 |
| `gs` | `/usr/bin/gs` | 备选（Ghostscript 9.55） |
| `convert` | `/usr/bin/convert` | 备选（ImageMagick 6.9） |
| PyMuPDF | `fitz` 1.20.0 | 备选（Python） |

**实测**：2 页 A4 PDF → `pdftoppm -png -r 150` **0.1 秒**出 2 张 1240×1755 PNG。

已实现为独立命令 `cmd/pdf2png`（与识别层解耦：它只产图，不认识发票）：

```bash
cd finance-router && source ./devenv.sh
go run ./cmd/pdf2png -in invoice.pdf -out out -dpi 150
```

---

## 3. ★ DPI 是最大的一根成本杠杆

SiliconFlow 文档（[多模态输入](https://docs.siliconflow.cn/docs/userguide/capabilities/multimodal-vision) §7.1）给出的计费口径：

| Qwen 系列 | 规则 |
|---|---|
| 尺寸约束 | 最小 56×56，最大 3584×3584；按 **28** 的倍数取整 |
| **`detail=low`** | **统一压到 448×448 ≈ 256 token** |
| **`detail=high`** | 长宽上取整到 28 的倍数，再等比裁剪 |
| **Token 公式** | **`ceil(h/28) × ceil(w/28)`** |

### 3.1 实测：A4 票据在不同 DPI 下的 token

| DPI | 像素 | 视觉 token | 相对 150 dpi |
|---|---|---|---|
| 72 | 595×842 | 682 | 0.24× |
| 100 | 827×1170 | 1,260 | 0.44× |
| **120** | **992×1404** | **1,836** | **0.65×** |
| **150** | **1240×1755** | **2,835** | **1.00×** |
| 200 | 1653×2339 | 5,040 | 1.78× |
| 300 | 2480×3509 | 11,214 | 3.96× |

### 3.2 由此得到的分层策略（重要）

`detail=low` 恒为 **256 token**，比 150 dpi 的 `high` 便宜约 **11 倍**。但 448×448 下发票票号/税号必糊。
所以**按图类型分层**，而不是一律 high：

| 图类型 | detail | 预览分辨率 | 理由 |
|---|---|---|---|
| **发票** | **`high`** | 长边 1280（约 150 dpi A4） | 票号/税号/校验码是小字，压到 448 必丢 |
| **订单截图** | **`low`** | 不敏感 | 只需金额/日期/店铺名，字体大 |
| **付款记录** | **`low`** | 不敏感 | 只需金额/时间/收款方，字体大 |

**量级**：年 900–1,800 张图；发票约 1/3、按 2,835 token，另 2/3 按 256 token →
年约 **2.0–4.1 M 输入 token**。这个量级下，即使按较高单价（¥30/M）也只有**几十元/年**。

> ⚠️ **未取到确切单价**：`cloud.siliconflow.cn/models/detail/...` 需要登录（跨域跳转到 account）。
> 请在控制台核对一次单价，然后乘以上面的 token 量。**但结论不因此改变**：钱不是这个方案的约束。

---

## 4. 审批附件控件（已核实，T3 关闭）

来自[审批定义表单控件参数](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/approval-v4/approval/approval-definition-form-control-parameters)：

| 控件 | type | 说明 |
|---|---|---|
| **图片** | **`image`** | 审批表单可放图片控件 |
| **附件** | **`attachmentV2`** | 审批表单可放附件控件 |
| 其他 | `input` / `textarea` / `number` / `amount` / `text` / `radioV2` / `checkboxV2` / `date` / `dateInterval` / `connect` / `contact` / `address` / `telephone` / `fieldList` / `leaveGroupV2` … | |

**「创建实例 API 不支持的控件」完整清单**（[来源](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/approval-v4/instance/approval-instance-form-control-parameters)）：
`tripGroup`、`apaascorehrOnboardingGroup`、`apaascorehrRegularateGroup`、`remedyGroupV2`、`apaascorehrJobAdjustGroup`、`apaascorehrOffboardingGroup`
—— **全是 HR 控件组，`image`/`attachmentV2` 不在其中**。

### 4.1 但仍有一个必须实测的问题

`image` / `attachmentV2` 的 **value 结构**（下载所需字段）尚未确认。侦察报告会自动检测：
- 若 `value` 是 `[{"file_token":"...", ...}]` → 走 `drive/v1/medias/:file_token/download`；
- 若 `value` 是富文本内嵌图 → 需要另解析。

**这一步由 `cmd/recon` 的 §4.1 报告自动回答**，跑一次即可。

### 4.2 一个结构性限制（已核实，与 06 文档一致）

`instance/get` 返回的是**静态 form 快照**：表单值在**创建实例时固定**。所以：
- 队员**必须在提交时就上传三张图** → 服务**无法事后往审批表单里补图片或结论**；
- 这与 06 文档的结论一致：**服务纯只读，结论只能靠机器人卡片补发**。

---

## 5. 产物落在哪里

所有命令的相对路径**以 CWD 为基准**。从 `finance-router/` 运行时的实际落点：

| 内容 | 路径 | 说明 |
|---|---|---|
| 配置 | `finance-router/config.yml` | 唯一真实配置；被 git 忽略 |
| **侦察报告** | `finance-router/data/recon/report.md` | **给人看的主报告** |
| 审批定义原始 JSON | `finance-router/data/recon/definition.json` | |
| 实例列表原始响应 | `finance-router/data/recon/instances_list.jsonl` | |
| 实例详情原始 JSON | `finance-router/data/recon/instances_detail.jsonl` | 含原始 `form`，排障用 |
| 解析后的表单值 | `finance-router/data/recon/forms.jsonl` | 一行一实例 |
| **PDF 转出的 PNG** | **由 `-out` 指定**（默认与 PDF 同目录） | 仅测试产物，**正式流程不落盘保留** |
| 本地数据库（后续） | `finance-router/data/finance.db` | 尚未实现 |
| Go 构建缓存 | `.gocache/`、`.gomodcache/`（仓库根） | 本机 `~/.cache` 只读，被迫放这 |

**两条纪律**（与架构稿一致）：
1. `data/` 全程被 git 忽略；
2. **票据原图不长期落盘** —— 识别完只保留 `sha256` 指纹。`pdf2png` 的 PNG 是**临时中间产物**，
   正式流程应输出到带 TTL 的临时目录（如 `data/tmp/<sha256>/`），识别后删除。

---

## 6. 需要回填的文档

| 文档 | 修订 |
|---|---|
| `06-direct-approval-initiation.md` §5 | **T3 关闭**：审批表单支持 `image` / `attachmentV2`；T5（附件下载鉴权）仍待实测 |
| `02-ocr-provider-interface.md` §2 | 补「Qwen3-VL **不支持 PDF**」与 `detail=low/high` 的 token 规则 |
| `02-ocr-provider-interface.md` §5 | `image_detail` 由全局 `high` 改为**按图类型分层** |
| `config.example.yml` | 增加 `pdf:` 节（DPI、最大边长、临时目录） |

---

*本文只新增核实结论，不改写前置裁定。所有飞书/模型侧事实均引自官方文档（链接见正文），
"单价"一项因需登录未能取到，已显式标注。*
