# 飞书（Lark）生态能力核实：财务 / 发票 / 流水 / 预算管理系统

> 目的：搞清「用飞书生态实现财务/发票/流水/预算管理系统」时，平台**原生能做到什么、明确做不到什么**。
> 调研日期：**2026-09-12**（CST）。所有链接为调研当日实际成功抓取的页面。
> 本文只回答「能力边界」，不含选型结论；选型见 `docs/10-proposals/`。

---

## 核实方法与可信度说明

### 使用了什么方式

| 方式 | 说明 |
|---|---|
| **CDP 浏览器** | ❌ **不可用**。`web-access` skill 的前置检查 `check-deps.mjs` 返回 `exit 1`：`browser: 未连接 — 没有任何浏览器打开远程调试开关`。实际尝试拉起 Chrome 亦失败：沙箱下 `~/.config/google-chrome` 只读，Chrome 报 `Failed to create .../SingletonLock: 只读文件系统` 并中止。**因此本次调研全程未使用浏览器自动化**，报告结论不含任何「页面渲染所见」。 |
| **一手 Markdown 端点**（主力） | ✅ 飞书开放平台**每个文档页 URL 后加 `.md` 即返回纯 Markdown 原文**，例如 `.../app-table-record/create` → `.../app-table-record/create.md`。这是本次最可靠的一手来源通道，用 `curl` 直接取得官方正文，未经第三方转述。 |
| **全站文档索引** | ✅ `https://open.feishu.cn/sitemap/sitemap.xml`（4300 条 URL）用于穷举接口清单——**「某接口不存在」的结论靠穷举 + 主动探测 404 双重反证**（例如 `app-dashboard/create.md`、`form/create.md` 均 404，而同目录 `list.md` 为 200）。 |
| **WebFetch / WebSearch** | ⚠️ 仅用作**发现入口**，不用于证明。凡搜索结果与官方原文冲突，一律以官方原文为准。 |
| **Jina 渲染** | ✅ 飞书帮助中心 `www.feishu.cn/hc` 与定价页为 JS 渲染（直接 `curl` 只得空壳），改用 `https://r.jina.ai/<url>` 取正文。用于帮助中心与定价页。 |
| **GitHub 原始内容 / REST API** | ✅ 用于官方 SDK 与开源 OCR 方案的 README、star 数、版本号、License（见 F18）。 |

### 可信度分级口径

- **已核实**：官方文档/帮助中心**逐字写明**，且本文引用了原文。
- **推断**：官方未直接陈述，但由多处官方信息交叉推出；本文写明推理依据。
- **未找到**：穷举检索后**未找到官方文档支持**（**不等于"不存在"**，仅表示当前无一手依据）。

### 需要读者注意的三点

1. **多份官方文档口径互相矛盾之处，本文一律并列标注，不代为调和**（例如审批定义能否用 API 修改、公式字段能否用 API 设置表达式）。这些必须**实测确认**，不能据文档下结论。
2. **「推送方式 = Webhook」不能作为「不支持长连接」的证据**：官方事件页对 `im.message.receive_v1`（已证实可走长连接，SDK 示例即用它）与 `drive.file.bitable_record_changed_v1` **标注完全相同**。据此判断该字段不区分传输方式（此判断为**推断**）。
3. 本文中所有「未找到官方文档」项已在《未能核实的问题》一节集中列出。

---

## 主体：逐条核实结论

### A. 多维表格（Bitable）能力

| 编号 | 结论（能/不能/有限制） | 关键限制与数字 | 来源链接 | 可信度 |
|---|---|---|---|---|
| **A1** 字段类型与 API 可写性 | **有限制**：22 类 `type` 可创建，但 **7 类硬性只读**，3 类不可创建 | 只读（不可写入记录值）：**公式(20)、查找引用(19)、创建时间(1001)、最后更新时间(1002)、创建人(1003)、修改人(1004)、自动编号(1005)**。不可创建：**查找引用(19)**（原文「不支持新增 19 查找引用字段类型」）、**流程(24) / 按钮(3001)**（原文「不支持通过写接口新增或编辑，仅支持读接口」）。**可写**：文本(1)/数字(2)/单选(3)/多选(4)/日期(5)/复选框(7)/人员(11)/电话(13)/超链接(15)/附件(17)/单向关联(18)/双向关联(21)/地理(22)/群组(23)。官方「新增记录」`fields` 参数**逐项枚举**了可写类型，公式/查找引用/自动编号/四个系统字段**均不在其中** | [字段编辑指南](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-field/guide) · [新增字段](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-field/create) · [新增记录](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-record/create) · [记录数据结构](https://open.feishu.cn/document/docs/bitable-v1/app-table-record/bitable-record-data-structure-overview) | 已核实（类型枚举与只读集合）；**推断**（「不在 `fields` 枚举中 ⇒ 不可写」这一推理）；⚠️ 公式表达式可否写入**官方文档自相矛盾**，见下 |
| **A2** 唯一性约束 / 去重校验 | **不能**（无原生唯一索引） | **平台不存在字段级唯一约束（unique index）**；字段 API 全部参数中无任何 `unique` 项，官方 FAQ 通篇无此能力。**只有事后方案**：`UNIQUE()` 公式函数、`IF+COUNTIF` 标记重复、UI「删除重复记录」插件、条件格式高亮——**均不能阻止重复写入**。语义最接近的是写接口 `client_token`（uuidv4），但它**只保证单次请求幂等**，不约束值唯一 | [字段编辑指南](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-field/guide) · [多维表格 UNIQUE 函数](https://www.feishu.cn/hc/zh-CN/articles/094296730015) · [查找重复内容](https://www.feishu.cn/hc/zh-CN/articles/974691484068) · [新增记录](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-record/create) | 已核实（无 unique 参数、UNIQUE() 为公式）；**未找到**（「删除重复记录插件」的官方文档） |
| **A3** 容量与 API 限流 | **有限制** | **批量**：新增/更新单次上限 **1,000 条**、删除 **500 条**，且**全部成功或全部失败，无部分成功**。**读**：list/search 500 行/次、batch_get 100 条/次。**单表行数**：开放平台称「不同租户不同，开放平台没有额外限制，以 UI 显示为准」；帮助中心按版本给 **2,000 / 2,000 / 20,000 / 50,000 行**。**资源上限**：字段 300（公式≤100）、视图 200、数据表+仪表盘 100、高级权限角色 30、协作者 200。**QPS**：记录写 **50/s**、记录读 20/s、字段写 10/s、导出 100/min、上传素材 **5 QPS 且 10,000 次/天**。**⚠️ 同一数据表禁止并发写**（错误码 `1254291 Write conflict`：写接口含新增/修改/删除记录、字段、修改表单、修改视图），只能串行 | [多维表格概述·使用限制](https://open.feishu.cn/document/server-docs/docs/bitable-v1/bitable-overview) · [频控策略](https://open.feishu.cn/document/ukTMukTMukTM/uUzN04SN3QjL1cDN) · [批量新增](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-record/batch_create) · [批量更新](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-record/batch_update) · [新增视图](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-view/create) · [素材概述](https://open.feishu.cn/document/server-docs/docs/drive-v1/media/introduction) · [多维表格付费权益说明](https://www.feishu.cn/hc/zh-CN/articles/487931070605) | 已核实（批量 1000/500、写冲突 1254291、QPS）；**已核实**（版本行数表）；**未找到**（「每日配额」——飞书为**月度**总量，见 E17） |
| **A4** 仪表盘 Dashboard | **有限制**：**不能通过 API 创建** | 官方仅有 **2 个接口**：`GET /bitable/v1/apps/:app_token/dashboards`（列出，20 次/秒）与 `POST .../dashboards/:block_id/copy`（复制，10 次/秒）。**无 create / patch / delete**——由 sitemap 穷举 + `app-dashboard/create.md` 主动探测 404 双重反证。仪表盘只能 **UI 创建**，或 API **复制**已有仪表盘当模板。UI 组件（15 类）：柱状/条形/折线/散点/组合/面积/雷达/饼图/漏斗/词云/指标卡/排行榜/透视表/切片器/进度；筛选靠「切片器」与图表联动。名称 ≤100 字符且不能含 `[]` | [列出仪表盘](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-dashboard/list) · [复制仪表盘](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-dashboard/copy) | 已核实（仅 list/copy、无 create） |
| **A5** 表单 Form | **能**（更正一处常见误解） | **无 `form/create` 接口**（404 反证），**但表单视图可由「新增视图」接口创建**：`POST /bitable/v1/apps/:app_token/tables/:table_id/views`，`view_type` 枚举含 **`form：表单视图`**——**已由本人独立复核确认**。表单问题项可 patch 的 `required` / `visible` 控制必填与可见。**预填支持**：分享链接 `?prefill_[问题名称]=[默认值]`，多值用 `&`，`&hide_问题名` 可隐藏；**预填链接上限 16,000 字符**。**提交后触发自动化**：由触发器「添加新记录时」覆盖，**无独立"表单提交"触发器**。**附件支持**：外部用户可上传附件，但**占用多维表格所有者的存储空间** | [新增视图](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-view/create) · [列出表单问题](https://open.feishu.cn/document/server-docs/docs/bitable-v1/form/list) · [更新表单问题](https://open.feishu.cn/document/server-docs/docs/bitable-v1/form/patch) · [更新表单元数据](https://open.feishu.cn/document/server-docs/docs/bitable-v1/form/patch-2) · [使用多维表格高级权限](https://www.feishu.cn/hc/zh-CN/articles/962169212093) | 已核实（`view_type=form`、预填格式）；**未找到**（表单附件单文件上限官方未给） |
| **A6** 高级权限 | **能**（实现「只看/只改自己创建或负责的记录」），**但免费版无行列权限** | **官方内建选项**，记录范围三选一：「所有记录」/「**与成员本人相关的记录**：仅可编辑和删除成员本人创建的记录，或特定字段中包含了成员本人的记录」/「满足特定条件的记录」；UI 操作项含「**成员本人创建的记录**」与「**以下字段包含成员本人的记录**」。**API 等价能力**：`app-role/create` 的 `rec_rule.conditions`，官方原文「**协作者可编辑自己的记录** 和 **可编辑指定字段** 是 **可编辑记录** 的特殊情况，可通过指定 `rec_rule` 或 `field_perm` 参数实现相同的效果」；`field_name` 说明「记录筛选条件是"创建人包含访问者本人"时，此参数值为 `""`」。**粒度**：按人（角色）× 数据表 × 记录行 × 字段列 + 仪表盘 + 视图。`table_perm`：0 无权限 / 1 可阅读 / 2 可编辑记录 / 4 可编辑字段和记录；`field_perm`：1 可阅读 / 2 可编辑。**⚠️ 硬门槛**：官方权益表「行/列权限设置」列 **基础版免费=不支持、基础版标准=不支持**，并原文「飞书商业标准版不支持多维表格高级权限功能，如需…建议升级至商业专业版或企业版」。限制：角色 30 / 协作者 200；**在线文档、电子表格中嵌入的多维表格及知识库中的多维表格不支持开启高级权限**；开启需先 `app/update` 且**有延迟** | [高级权限概述](https://open.feishu.cn/document/server-docs/docs/bitable-v1/advanced-permission/advanced-permission-guide) · [新增自定义角色](https://open.feishu.cn/document/server-docs/docs/bitable-v1/advanced-permission/app-role/create) · [批量新增协作者](https://open.feishu.cn/document/server-docs/docs/bitable-v1/advanced-permission/app-role-member/batch_create) · [使用多维表格高级权限](https://www.feishu.cn/hc/zh-CN/articles/962169212093) · [多维表格付费权益说明](https://www.feishu.cn/hc/zh-CN/articles/487931070605) | 已核实（记录范围选项、`rec_rule`、版本门槛）；⚠️ 官方文档自相矛盾一处：「高级权限接口暂不支持设置仪表盘权限」 vs `app-role` 请求体已含 `block_roles` |
| **A7** 附件 | **能**（上传 + 绑定 + 外部下载） | **两步**：① `upload_all`（或分片）拿 `file_token` → ② 写入记录 `{"附件":[{"file_token":"box..."}]}`。**单文件上限 20 MB**（原文「素材大小不得超过 20 MB」，参数 `size` 最大 `20971520`）；超过走**分片上传，平台固定 4 MB 分片**，支持一天内恢复。上传点 `parent_type` = `bitable_image` / `bitable_file`。**下载**：`drive/v1/medias/:file_token/download` 或 `batch_get_tmp_download_url`（**临时链接有效期 24 小时**）。**⚠️ 两个坑**：① **开启高级权限的多维表格下载素材必须额外传 `extra` 鉴权参数**（`bitablePerm`），否则 403；② **`file_token` 仅能在当前多维表格内使用，跨表需重新上传** | [附件字段说明](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-field/attachment) · [上传素材](https://open.feishu.cn/document/server-docs/docs/drive-v1/media/upload_all) · [素材概述](https://open.feishu.cn/document/server-docs/docs/drive-v1/media/introduction) · [获取素材临时下载链接](https://open.feishu.cn/document/server-docs/docs/drive-v1/media/batch_get_tmp_download_url) | 已核实（20 MB / 4 MB 分片 / 两步绑定 / 24 小时 / extra 鉴权）；**未找到**（附件字段独立单文件上限、附件总容量上限的官方条目） |
| **A8** 自动化流程 Workflow | **能，但限制多** | **触发器 9 类**：添加新记录时 / 修改记录时 / 新增·修改的记录满足条件时 / 到达记录中的时间时 / 定时触发 / 点击按钮时 / 接收到飞书消息时（内测）/ 接收到 webhook 时 / 接收到 Outlook 邮件时。**无独立"表单提交"触发器**（由「添加新记录时」覆盖），**不支持删除触发，也不支持删除操作**。**动作 20+**：发消息/发邮件/新增记录/修改记录/查找记录/**发送 HTTP 请求（GET/POST/PUT/PATCH/DELETE，禁内网）**/延迟/飞书日程·任务·群组/AI 生成文本·AI 更新记录·AI 分类·AI Agent/自动化插件。**⚠️ 条件分支与多分支、循环「仅在工作流（Workflow）中支持」，经典自动化流程不支持**。工作流循环的**单节点最大循环次数按版本为 5 / 5 / 100 / 1,000 次**（基础免费 / 基础标准 / 商业专业 / 商业旗舰及企业版）；另有「单工作流≤5 个循环节点、嵌套≤3 层、循环体内不能加条件判断」的限制（**推断**，来自官方工作流文档整理，未逐字复核）。**执行次数配额（次/月，工作流与自动化共用）**：基础版免费 **200** / 商业标准 **200** / 商业专业 **5,000** / 商业旗舰 **50 万** / 企业版 **50 万**（企业旗舰 = 50 万 + 60×购买人数）。次月 1 日重计；每个多维表格自动化/工作流各上限 200 个。**⚠️ API 只能 list + 启停，不能创建** | [列出自动化流程](https://open.feishu.cn/document/docs/bitable-v1/app-workflow/list) · [多维表格付费权益说明](https://www.feishu.cn/hc/zh-CN/articles/487931070605) | 已核实（触发/动作清单、配额表、API 仅 list+启停）；**未找到**（定时触发**最短粒度**官方未写明；可引用的最近似数字是「延迟」动作最短 1 分钟、最长 2 小时） |
| **A9** 「字段捷径」/ AI 字段与 OCR | **能，但 API 不可触达** | 官方名称为「**字段捷径**」，是 **UI 配置**能力。飞书科技提供 **6 个文本类捷径**：AI 自动分类（→单选）、智能标签（→多选）、翻译（**12 语种**）、信息提取、总结、自定义 AI 填充（→文本/数字/单选/多选/日期）。火山引擎捷径含**通用文字识别 OCR（0.2 点）**、AI 图片理解、PDF 转文本等。**⚠️ API 不能创建这类字段**（字段 API 的 `type`/`ui_type` 与视图扩展 `FieldType` 三处枚举**均无 AI 类型**）；**API 读取其计算结果无官方说明**。**发票识别有两条官方原生路径**：① 服务端 API **`POST /open-apis/document_ai/v1/vat_invoice/recognize`**（增值税发票，支持 JPG/JPEG/PNG/PDF/BMP/OFD，**<10 MB**，10 QPS/租户，返回结构化实体：`invoice_code` 发票代码、`invoice_no` 发票号码、`invoice_date` 开票日期、`total_price` 合计金额、`total_tax` 合计税额、`total_price_and_tax` 合计总额、购买方/销售方名称与税号等）；另有 `train_invoice`（火车票）、`taxi_invoice`（出租车票）、`vehicle_invoice`。② 通用 OCR **`/open-apis/optical_char_recognition/v1/image/basic_recognize`**（base64 图片，**<5 MB**，20 QPS，仅返回分段文本 `text_list`，**无结构化**）。**⚠️ 计费**：按 **AI 点数**计费，官方错误码 `2110003` 原文「You have reached the Intelligent document parsing limit. To continue using this function, please contact sales to purchase more.」；智能文档处理另有独立配额（**基础免费版：完成账号认证前不支持，认证后 100 次/月**；基础标准 100 / 商业专业 1,000 / 商业旗舰 10,000 / 企业标准 1,000 / 企业专业 10,000 / 企业旗舰 10,000 次/月）。**写回**：字段捷径结果直接落在字段内，有「自动更新」开关，**生成全列会覆盖原内容且不可恢复** | [增值税发票识别](https://open.feishu.cn/document/ai/document_ai-v1/vat_invoice/recognize) · [火车票识别](https://open.feishu.cn/document/ai/document_ai-v1/train_invoice/recognize) · [图片文字识别](https://open.feishu.cn/document/server-docs/ai/optical_char_recognition-v1/basic_recognize) · [AI 字段捷径](https://www.feishu.cn/content/article/7592…) · [使用智能文档处理应用](https://www.feishu.cn/hc/zh-CN/articles/068282371382) · [飞书 AI 版本权益与额度消耗规则](https://www.feishu.cn/hc/zh-CN/articles/629644238181) · [多维表格付费权益说明](https://www.feishu.cn/hc/zh-CN/articles/487931070605) | 已核实（两条 OCR API、格式/大小/QPS、结构化字段、`2110003` 计费报错、API 无 AI 字段类型）；**推断**（「字段捷径支持直接用附件图片作输入」——官方未逐字说明）；**未找到**（AI 免费额度的具体点数、API 读 AI 字段结果的官方说明） |
| **A10** 数据导出 | **有限制**：有官方导出 API，但**导出不含附件** | **无多维表格专属导出 API**，走云文档通用 `POST /open-apis/drive/v1/export_tasks`（**100 次/分钟，仅 Custom App**）。`type=bitable` **仅支持 `xlsx` / `csv`**；导出 CSV **必须传 `sub_id` = `table_id`（即按数据表逐表导出）**。**⚠️ 不支持导出为 `.base`**（UI 可，API 不可）。**⚠️ 附件不在导出产物内**，须另行调用 `drive/v1/medias/:file_token/download` 或 `batch_get_tmp_download_url`。**产物在任务结束 10 分钟后被删除**，必须及时下载。**导出条数上限官方未公布** | [导出云文档概述](https://open.feishu.cn/document/server-docs/docs/drive-v1/export_task/export-user-guide) · [创建导出任务](https://open.feishu.cn/document/server-docs/docs/drive-v1/export_task/create) | 已核实（仅 xlsx/csv、需 sub_id、10 分钟删除、附件需另取）；**未找到**（导出条数上限） |

### B. 飞书审批（Approval）能力

| 编号 | 结论（能/不能/有限制） | 关键限制与数字 | 来源链接 | 可信度 |
|---|---|---|---|---|
| **B11** 审批定义与实例 | **有限制**；⚠️ **官方文档自相矛盾** | **定义可通过 API 创建**：`POST /open-apis/approval/v4/approvals`（1000 次/分、50 次/秒）。接口正文称**传 `approval_code` 即为全量覆盖更新**；**但** `approval-related-faqs` 写「是否支持 API 方式修改审批定义？**不支持，只能在后台修改**」——**两处正冲突，必须实测**。API 建的定义**无法停用/删除**；**API 不支持条件分支**；不支持含 `formula`/`mutableGroup`/`serialNumber` 等 8 种控件。**实例状态订阅**：事件 `approval_instance` / `approval_task` / `approval_cc` / 自定义定义更新；**须先调 `POST .../approvals/:approval_code/subscribe`**（100 次/分，按审批定义粒度、面向应用），再在后台订阅事件。事件重试 15s/5min/1h/6h，**最多 4 次**，用 `event_id` 幂等。业务数据：定义侧 `form_content`、实例侧 `form`（JSON 压缩字符串）。**⚠️ 审批流程节点无原生调用外部 HTTP 的能力**（多页检索后未找到任何官方文档）；唯一原生外部 HTTP 是**表单单选/多选控件的「关联外部选项」**——由飞书**反向调用你自研的 HTTP/HTTPS 接口**取选项，约 4 秒响应 | [创建审批定义](https://open.feishu.cn/document/server-docs/approval-v4/approval/create) · [创建审批实例](https://open.feishu.cn/document/server-docs/approval-v4/instance/create) · [审批事件订阅](https://open.feishu.cn/document/server-docs/approval-v4/event/event-interface/subscribe) · [审批常见问题](https://open.feishu.cn/document/server-docs/approval-v4/approval-related-faqs) | 已核实（接口与订阅机制）；⚠️ **官方自相矛盾**（定义可否修改）；**未找到**（审批节点调外部 HTTP） |
| **B12** 审批 ↔ 多维表格联动 | **不能**（无原生回写） | **官方没有「审批通过后写回 Bitable 记录」的原生能力。** 原生仅有：多维表格「连接器中心 → 飞书审批」，**单向**、**每小时自动同步**、生成的同步表**只读**（不能手动增删改记录/字段）——Bitable API 侧亦印证：「从其它数据源同步的数据表，不支持对记录进行增加、删除、和修改操作」。官方《工作流和自动化触发条件与执行操作一览》**9 项触发条件逐项核对，无「审批」触发器**。**因此双向联动必须自研中间服务**：订阅 `approval_instance` 事件 → 你的服务 → Bitable `POST .../records`（50 次/秒）。反向（Bitable → 审批）可借自动化「发送 HTTP 请求」。⚠️ 官方「审批官方连接器」方向相反（把三方 OA 审批推进飞书），**不要误用** | [审批概述](https://open.feishu.cn/document/server-docs/approval-v4/approval-overview) · [自动化流程](https://open.feishu.cn/document/docs/bitable-v1/app-workflow/list) · [创建记录](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-record/create) | 已核实（连接器单向 + 9 项触发器无审批） |

### C. 事件与回调

| 编号 | 结论（能/不能/有限制） | 关键限制与数字 | 来源链接 | 可信度 |
|---|---|---|---|---|
| **C13** 事件订阅方式（长连接） | **能**，且**适合无公网 IP 的局域网** | **决定性原文**：「只需保证运行环境具备**访问公网的能力**即可，**无需提供公网 IP 或域名、无需使用内网穿透工具**，通过长连接模式在本地开发环境中即可接收事件消息，后续在线上部署本地服务后也可以直接生效。」→ **主动出网即可，不需要入站公网**。**⚠️ 仅支持企业自建应用**（原文「长连接模式仅支持企业自建应用」/「目前长连接模式不支持商店应用」）。**每应用最多 50 个连接**；消息推送为**集群模式，不支持广播**——多 client 只有**随机一个**收到；**须 3 秒内处理完成且不抛异常**，否则触发重推。内置加密与鉴权，无需自行解密验签。**四语言官方 SDK 齐全**：`lark-oapi`(Python)、`github.com/larksuite/oapi-sdk-go/v3`、`com.larksuite.oapi:oapi-sdk`(Java)、`@larksuiteoapi/node-sdk`(≥1.24.0)。Webhook 侧需 **IPv4 公网地址**、每应用仅 1 个地址、challenge 须 1 秒内返回。**事件重发**：15s/5min/1h/6h，**最多 4 次**；**平台为「至少一次」投递，必须用 `uuid`(v1.0) / `event_id`(v2.0) 做幂等** | [事件概述](https://open.feishu.cn/document/server-docs/event-subscription-guide/overview) · [使用长连接接收事件](https://open.feishu.cn/document/server-docs/event-subscription-guide/event-subscription-configure-/request-url-configuration-case) · [事件订阅 FAQ](https://open.feishu.cn/document/event-subscription-guide/event-subscriptions/faq) | 已核实（免公网原文、仅自建应用、50 连接、集群不广播、3 秒、幂等要求）；**未找到**（推送频率上限、单应用可订阅事件数量上限） |
| **C14** 多维表格记录变更事件 | **存在，记录级，含前后值**，但**订阅成本高** | 事件类型 **`drive.file.bitable_record_changed_v1`**。**粒度**：表级触发（含 `table_id`），事件体含 `action_list[]`，逐条给出 `record_id` + `action`（`record_added`/`record_deleted`/`record_edited`）+ **`before_value` 与 `after_value`**（后者为 JSON 序列化字符串）。**⚠️ 三个硬限制**：① **必须先按文档订阅**——调 `POST /open-apis/drive/v1/files/:file_token/subscribe`（**按文档粒度，不能按事件类型筛选**，即订阅该文档的全部相关事件）；② **仅文档拥有者/管理者可订阅**，用 `tenant_access_token` 订阅时需**同时开通应用+用户双身份**的 `bitable:app` 或 `drive:drive` 权限；③ **公式字段的值变化不触发事件，且事件体不含公式字段的值**。字段级另有 `drive.file.bitable_field_changed_v1` | [多维表格记录变更](https://open.feishu.cn/document/docs/bitable-v1/events/bitable_record_changed) · [多维表格字段变更](https://open.feishu.cn/document/server-docs/docs/drive-v1/event/list/bitable_field_changed) | 已核实（事件类型、记录级 action_list、前后值、前置订阅、公式字段例外）；**推断（高置信）**（可否走长连接：官方事件页标注「推送方式=Webhook」，但**同一字段对已证实支持长连接的 `im.message.receive_v1` 标注完全相同**，故该字段不区分传输方式；⚠️ 仍建议实测）；**未找到**（延迟量级、是否属「有序事件」） |
| **C15** 机器人（Bot） | **能**（主动单聊 + 互动卡片 + 长连接回调） | **可主动单聊推送**：官方前提仅「用户需在**机器人的可用范围**内」，**未找到任何「必须先有会话」的官方限制**。**限频**：同一用户 **5 QPS**、同一群组 **5 QPS**（群内共享）；接口级 1000/分、50/秒；自定义机器人（群 webhook）**100 次/分钟、5 次/秒**（且不计入 API 调用量）。**消息体上限**：文本 150 KB、卡片/富文本 30 KB。**互动卡片**：`msg_type: interactive`；卡片回调 **`card.action.trigger` 可走长连接**（四语言 SDK 均有示例）。**⚠️ 但旧版「消息卡片回传交互（旧）」回调不支持长连接，只能 Webhook**。卡片回调**无补推机制**，须 3 秒内同步响应 | [发送消息](https://open.feishu.cn/document/server-docs/im-v1/message/create) · [回调概述](https://open.feishu.cn/document/event-subscription-guide/callback-subscription/callback-overview) · [频控策略](https://open.feishu.cn/document/ukTMukTMukTM/uUzN04SN3QjL1cDN) | 已核实（主动单聊、5 QPS、卡片回调支持长连接、旧版不支持） |

### D. 云文档 / 电子表格（Sheets）与多维表格的差异

| 编号 | 结论（能/不能/有限制） | 关键限制与数字 | 来源链接 | 可信度 |
|---|---|---|---|---|
| **D16** Sheets vs Bitable | **定位不同，不可互替** | **API**：Bitable 为 record/field/view/table **对象化**接口，**有自动化 API、有行列级高级权限 API**；Sheets 为**单元格 + range** 模型，**无自动化 API、无仪表盘/图表 API**（但有筛选、条件格式、保护范围、浮动图片）。**附件**：Bitable 原生**附件字段（type 17）**，走「上传素材 → 挂记录」两步，且**附件可被 API 下载**；Sheets **无附件 API**，仅有「写入图片 / 浮动图片」，单元格附件（图片≤20 MB、其他≤300 MB）**移动端不可传**。**权限**：Bitable 高级权限为**角色制、行/列/视图/仪表盘粒度**（行列权限需商业专业版起）；Sheets 为**保护范围（protected range）**。**仪表盘**：Bitable **有**仪表盘 + 图表 + 智能总结（但 API 仅 list/copy）；Sheets **无仪表盘**，仅有图表面板且**无 API** | [电子表格概述](https://open.feishu.cn/document/server-docs/docs/sheets-v3/overview) · [写入图片](https://open.feishu.cn/document/server-docs/docs/sheets-v3/data-operation/write-images) · [多维表格概述](https://open.feishu.cn/document/server-docs/docs/bitable-v1/bitable-overview) | 已核实（四项差异）；**推断**（Sheets「无仪表盘/图表 API」由 sitemap 穷举 + 文档结构推得） |

### E. 账号与版本门槛

| 编号 | 结论（能/不能/有限制） | 关键限制与数字 | 来源链接 | 可信度 |
|---|---|---|---|---|
| **E17** 版本门槛与配额 | **存在多个硬门槛** | **⚠️ 最关键的硬门槛——多维表格行/列高级权限**：官方权益表「行/列权限设置」列 **基础版免费=不支持、基础版标准=不支持**；官方原文「飞书商业标准版不支持多维表格高级权限功能，如需…建议升级至商业专业版或企业版」。**⚠️ 免费版 API 总量**：基础免费版内**单租户所有自建应用的 API 调用总量上限为 100 万次/月**（2026-06 限时档；此前基线为 10,000 次/月），**每月 1 号刷新**，超限返回 `429` / `99991403`。**注意**：事件订阅、通讯录、**AI 能力（含 OCR）**、aily 等业务**不计入**该调用量。**按版本分档**：单表行数 2,000/2,000/20,000/50,000；自动化+工作流运行次数 200/200/5,000/50 万次/月；智能文档处理次数 100/100/1,000/10,000。**定价入口**：`www.feishu.cn/price` **已 404**，正确入口为 `www.feishu.cn/pricing`（JS SPA）；2026-09-12 抓取：免费版 ¥0（100 用户）/ 商业标准 ¥50 / 商业专业 ¥80 / 商业旗舰 ¥120（各 500 用户）/ 企业旗舰定制，**均按年付费**；商业专业版权益明确标注「多维表格高级能力」。**自建应用**：**未找到任何「某些 scope 需付费版才能申请」的官方口径**（《申请 API 权限》全文无付费字样）——但**确有 API 自带版本门槛**，如翻译 API「免费版不支持调用」、通用 OCR「不支持通过飞书个人版调试」 | [多维表格付费权益说明](https://www.feishu.cn/hc/zh-CN/articles/487931070605) · [自建应用 API 调用量上限调整说明](https://open.feishu.cn/document/platform-notices/platform-updates-/custom-app-api-call-limit) · [飞书 AI 版本权益与额度消耗规则说明](https://www.feishu.cn/hc/zh-CN/articles/629644238181) · [飞书定价](https://www.feishu.cn/pricing) | 已核实（行列权限门槛、API 总量 100 万/月、版本配额表、定价入口）；⚠️ 官方口径冲突一处：《飞书定价版本介绍》为旧/粗口径（基础版 200 / 商业版 50 万），与 7 档细表不一致，**以细表为准**；**未找到**（scope 级付费门槛、AI 免费额度具体点数） |

### F. 替代与互补：本地离线开源 OCR

| 编号 | 结论（能/不能/有限制） | 关键限制与数字 | 来源链接 | 可信度 |
|---|---|---|---|---|
| **F18** 离线开源 OCR 候选 | **能**（纯 CPU 可跑），但**发票结构化能力分化明显** | **① PaddleOCR**（89,368★，Apache-2.0，最新 v3.7.0/2026-06-11）：默认模型 PP-OCRv6；**PP-StructureV3** 做版面分析（20/23 类，含表格与印章）+ 表格识别；**KIE 走 PP-ChatOCRv4，但官方明确「需要准备大语言模型的 api_key…或者本地部署的标准 OpenAI 接口大模型服务」→ 离线必须自起本地 LLM**，或退回 v2 的 VI-LayoutXLM 判别式 SER/RE。模型体积：v5 mobile det+rec ≈20.7 MB，v6 small det+rec ≈30 MB。⚠️ 官方 v2 发票场景页自述模型与文档「许久未更新、均为 Demo、大概率不能直接应用到生产环境」。**② RapidOCR**（7,797★，Apache-2.0，v3.9.2/2026-07-21）：**默认即 CPU（ONNX Runtime）**，`default_models.yaml` 把 URL 钉到 v3.9.2 并带 SHA256 → **可完整离线镜像**；默认三件套 **≈30.3 MiB**。⚠️ **本仓库无版面/表格/KIE**（config 仅 `use_det/use_cls/use_rec`），需配官方姊妹项目 RapidLayout / RapidTable / RapidDoc（RapidDoc 官方称移除 VLM、CPU 上解析速度不错、模型来自 PP-StructureV3 全转 ONNX）。**③ Umi-OCR**（hiroi-sora）：**官方支持 Linux 无 GUI 部署**（`docker run -e HEADLESS=true -p 1224:1224`，原文「适合在没有显示器的云服务器…让 Umi-OCR 提供 HTTP 接口服务」）；但**表格识别仍在 README「预想中的功能」未完成清单**，插件库无 KIE/发票插件；Linux 侧仅 PaddleOCR-json 一种引擎，**硬性要求 CPU 支持 AVX**，Python 限 3.8~3.10；官方自陈「对并发支持较差」；引擎部署体积 **≈369 MB**、建议预留内存 **2000 MB**；**维护活跃度偏低**（最新 Release v2.1.5/2025-03-25）。**⚠️ 许可红线**：Surya 代码 Apache-2.0 但**模型权重为 OpenRAIL-M 修改版**（仅研究/个人/年营收或融资 <$5M 免费）；MinerU 已从 AGPLv3 改为自定义许可（GitHub 标 NOASSERTION），**二者都不能当 Apache-2.0 用** | [PaddleOCR](https://github.com/PaddlePaddle/PaddleOCR) · [PP-StructureV3 文档](https://www.paddleocr.ai/main/version3.x/pipeline_usage/PP-StructureV3.html) · [RapidOCR](https://github.com/RapidAI/RapidOCR) · [Umi-OCR](https://github.com/hiroi-sora/Umi-OCR) · [Umi-OCR Linux 运行库](https://raw.githubusercontent.com/hiroi-sora/Umi-OCR_runtime_linux/main/README-docker.md) | 已核实（star/版本/模型体积/CPU 可行性/License 均取自官方仓库与文档）；**未找到**（PP-OCRv6 的 CPU/GPU 耗时官方表为 `-`、各方案内存门槛官方未统一给出） |

---

## 关键原文摘录（决定性引用）

> **C13 长连接免公网**（[来源](https://open.feishu.cn/document/server-docs/event-subscription-guide/event-subscription-configure-/request-url-configuration-case)）
> 「只需保证运行环境具备访问公网的能力即可，**无需提供公网 IP 或域名、无需使用内网穿透工具**，通过长连接模式在本地开发环境中即可接收事件消息，后续在线上部署本地服务后也可以直接生效。」
> 「长连接模式**仅支持企业自建应用**。」「每个应用**最多建立 50 个连接**。」「长连接模式的消息推送为**集群模式，不支持广播**，即如果同一应用部署了多个客户端（client），那么只有**其中随机一个**客户端会收到消息。」

> **A3 串行写入硬约束**（[来源](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-record/batch_create)）
> 「多维表格底层对数据表的处理基于**版本维度的串行方式，不支持并发**。因此，并发请求时容易出现此类错误，**不建议开发者对单个数据表进行并发请求**。」
> （[多维表格概述](https://open.feishu.cn/document/server-docs/docs/bitable-v1/bitable-overview)）「对于接口的批量操作，**单次最高为 1,000 条记录**，且响应状态是**全部成功或者失败，不存在部分成功或失败**的结果。」

> **A6 高级权限可按「本人」过滤**（[来源](https://open.feishu.cn/document/server-docs/docs/bitable-v1/advanced-permission/app-role/create)）
> 「**协作者可编辑自己的记录** 和 **可编辑指定字段** 是 **可编辑记录** 的特殊情况，可通过指定 `rec_rule` 或 `field_perm` 参数实现相同的效果。」
> 「`field_name` … 记录筛选条件是"**创建人包含访问者本人**"时，此参数值为 `""`。」
> （[帮助中心](https://www.feishu.cn/hc/zh-CN/articles/588604550568)）「**可编辑与自己相关的记录**：设置成员是否可以编辑**自己创建或者提及自己**的记录。」

> **E17 高级权限版本硬门槛**（[来源](https://www.feishu.cn/hc/zh-CN/articles/487931070605)）
> 权益表「**行/列权限设置**」：基础版免费 **不支持** · 基础版标准 **不支持** · 其余档位 支持。
> 「不同版本对高级权限的使用 **仅** 体现在是否能设置行列权限上。」

> **E17 免费版 API 总量**（[来源](https://open.feishu.cn/document/platform-notices/platform-updates-/custom-app-api-call-limit)）
> 「飞书基础免费版内，单租户下所有企业自建应用的 **API 调用总量上限调整为 2026 年 6 月份限时 100 万次**，该上限在每个自然月的 1 号刷新。」「当月超过 API 调用量上限后，将无法继续调用计算调用量的 OpenAPI…返回 `99991403` 错误码。」
> 不计入调用量的业务含「**AI 能力**」「事件订阅」「通讯录」等。

> **A9 发票 OCR 计费门槛**（[来源](https://open.feishu.cn/document/ai/document_ai-v1/vat_invoice/recognize)）
> 错误码 `2110003`：「You have reached the **Intelligent document parsing limit**. To continue using this function, please contact sales to purchase more.」
> 接口说明：「增值税发票识别接口，支持 JPG/JPEG/PNG/PDF/BMP/OFD 六种文件类型的**一次性的识别**。文件大小需要**小于 10 M**。」「单租户限流：10 QPS。」

> **A1 只读字段（写接口不支持）**（[来源](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-field/guide)）
> 「24：流程（**不支持通过写接口新增或编辑，仅支持读接口**）」「3001：按钮（**不支持通过写接口新增或编辑，仅支持读接口**）」
> （[新增字段](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-field/create)）「要新增的字段类型。**不支持新增 19 查找引用字段类型**。」

> **B12 同步表只读**（[来源](https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-table-record/create)）
> 「从其它数据源同步的数据表，**不支持对记录进行增加、删除、和修改操作**。」

---

## 对方案的硬约束结论

1. **只读字段绝不能作为写入目标**：公式(20)、查找引用(19)、自动编号(1005)、创建时间(1001)、最后更新时间(1002)、创建人(1003)、修改人(1004) **无法通过 API 写入**——所有「计算所得」的值必须由本地服务算完写进**普通字段**，不要设计成 Bitable 公式字段再回头写入。
2. **不存在唯一索引，防重复报销（发票号去重）必须在自研服务层实现**：Bitable 只能提供 `UNIQUE()` / `COUNTIF` 等**事后**校验与展示，**不能阻止重复写入**；`client_token` 只防重试。→ 需要在本地服务维护唯一性校验（写入前查重 + 串行落库），或接受 UI 层告警。
3. **同一数据表禁止并发写，所有写入必须串行化**：Bitable 底层是版本串行模型，并发写会触发 `1254291 Write conflict` / `1254607`。→ 本地服务必须实现**单表单写入队列**（按 `table_id` 串行），不要用并发批量导入。
4. **容量要按最坏情况设计**：单次批量新增/更新 ≤1,000 条、删除 ≤500 条且**全成全败**；单表行数按版本仅 2,000–50,000 行（免费/标准版 2,000）。→ 流水类高频数据建议按年/月分表，避免单表撞上限；大批量导入必须分批 + 可重入。
5. **仪表盘无法用 API 创建**：只有 list 与 copy。→ 看板必须**手工在 UI 建一次**当模板，或用 copy 克隆；**不能把「自动生成看板」写进方案**。
6. **表单可以 API 创建**（走 `新增视图` + `view_type=form`，不是 `form/create`），预填走分享链接 `?prefill_字段名=` 且**链接上限 16,000 字符**。→ 「一键跳转填写」需求可实现，但长表单预填会撞长度上限。
7. **「每人只能看自己创建的记录」依赖付费版本**：免费版/基础标准版**不支持**行列高级权限。→ 若项目在免费版运行，**此需求必须由本地服务做数据裁剪**（应用身份读全量 + 按人过滤后返回），而不是靠 Bitable 权限。
8. **附件链路有三个硬点**：单文件直传 **20 MB**（超出走 4 MB 分片）、`file_token` **不可跨表复用**、**开启高级权限后下载附件必须带 `extra` 鉴权参数**（否则 403）。→ 发票原图建议**同时落 NAS 一份**，不要把附件当作唯一副本；不要设计跨表复用同一 `file_token`。
9. **自动化流程不能由 API 创建**：仅 list + 启停；**分支/循环仅工作流支持**，经典自动化流程没有；**免费版运行配额仅 200 次/月**，且**不支持删除触发与删除操作**。→ 催办等逻辑若量大，应放在**本地服务**而非 Bitable 自动化。
10. **事件接收用长连接，但必须做幂等与单实例消费**：长连接**无需公网 IP**（局域网可行），但**仅限企业自建应用**、每应用 ≤50 连接、**集群模式不广播**（多实例只有一个收到）、3 秒内必须处理完。→ 消费端要**单实例或做消费者分组**，并用 `event_id` 幂等；重活不要放在回调里同步做。
11. **Bitable 变更事件必须按文档逐个订阅，且公式字段不触发**：需先调「订阅云文档事件」，**不能按事件类型筛选**，且 `tenant_access_token` 只能订阅本应用拥有/管理的文档。→ 若用事件做流水同步，需在**建表时自动注册订阅**，并接受「公式字段变化无事件」。
12. **审批→多维表格没有原生回写，必须自研中间服务**：原生连接器是**单向、每小时、只读同步**，且官方自动化 **9 项触发器中没有「审批」**。→ 「审批通过自动写回记录」= 订阅 `approval_instance` 事件 + 本地服务调 Bitable 写接口；**不要把审批节点当作能直接调外部 HTTP 的地方**（节点内无原生 HTTP 能力）。

> 补充（未列入 12 条但同样重要）：**AI 字段捷径不能由 API 创建，其结果能否被 API 读取官方未说明**——因此**不要把 AI 字段作为系统的数据契约**，AI 提取结果应落回普通字段。**发票结构化提取有官方原生 API**（`document_ai/v1/vat_invoice/recognize`，<10 MB，10 QPS），但**受「智能文档解析」次数配额约束、超出需付费购买**；本地离线 OCR 的定位应是**大批量/超配额时的兜底**，而非「飞书没有 OCR 所以必须自建」。

---

## 未能核实的问题

以下问题经穷举检索（开放平台 sitemap 4300 条 + 帮助中心 + 官方仓库）后**未找到官方文档**，或存在**官方文档互相矛盾**。**均不得作为确定性结论使用，需实测或向飞书技术支持确认。**

| # | 问题 | 状态 |
|---|---|---|
| 1 | **审批定义能否通过 API 修改** | ⚠️ **官方文档自相矛盾**：接口正文「传 `approval_code` = 全量覆盖更新」 vs FAQ「不支持，只能在后台修改」。**必须实测** |
| 2 | **公式字段新增时能否同时设置表达式** | ⚠️ **官方文档自相矛盾**：`create` 的 `type` 枚举注「公式（不支持设置公式表达式）」，但同一接口 `property` 表列出 `formula_expression`。**必须实测** |
| 3 | Bitable 记录变更事件**延迟量级**、是否为「有序事件」 | 未找到官方文档 |
| 4 | Bitable 记录变更事件**能否走长连接** | **推断（高置信）**：事件页标「推送方式=Webhook」，但对已证实支持长连接的 `im.message.receive_v1` 标注完全相同，故判断该字段不区分传输方式。**建议实测** |
| 5 | 事件推送的**频率上限**、单应用可订阅**事件数量上限** | 未找到官方文档（仅有重试节奏 15s/5min/1h/6h 最多 4 次） |
| 6 | 自动化流程**定时触发的最短粒度** | 未找到官方文档（官方仅写「自定义日期、时间」；最近似可引用数字为「延迟」动作最短 1 分钟 / 最长 2 小时） |
| 7 | 多维表格**导出记录条数上限** | 未找到官方文档（仅有 `job_status 107 导出文档过大` 类状态码） |
| 8 | 多维表格**每日 API 配额** | 未找到；飞书为**月度**总量口径（基础免费版 100 万次/月） |
| 9 | **附件字段**的独立单文件上限、附件**总容量上限** | 未找到官方文档（20 MB 是「上传素材」接口口径；HC《文件上传大小要求》按版本给的是另一套口径，**未敢等同**） |
| 10 | **AI 免费额度的具体点数** | 官方未公开（仅知按月有免费额度，超出消耗企业 AI 额度/个人会员额度） |
| 11 | **API 能否读取 AI 字段捷径的计算结果** | 未找到官方文档；**推断**为：结果落地为文本/单选/多选时可读，但官方未确认——**方案中不要承诺** |
| 12 | 字段捷径是否支持**直接用「附件（图片）」作为输入** | 未找到官方逐字说明 |
| 13 | 是否存在「**某些 scope 需付费版本才能申请**」 | 未找到任何官方口径（《申请 API 权限》全文无付费字样）；但有 API 自带版本门槛（翻译 API 免费版不可调用） |
| 14 | 「**签字**」字段的 API `type` 枚举值 | 未找到官方文档（帮助中心提及该字段，开放平台类型表无对应项） |
| 15 | 单选/多选**选项数量上限** | ⚠️ 官方三处口径不一：变更公告 5,000 / 记录结构文档 20,000 / HC 常见上限 10,000 |
| 16 | 单表**记录上限** | 开放平台称「不同租户不同、无额外限制」；HC 按版本给 2,000–50,000 行。**应以目标租户 UI 实际显示为准** |
| 17 | 官方 GitHub `SKILL.md` 的「批量上限 500、单表 20,000」 | ⚠️ 与开放平台「1,000 条」及 HC「2,000–50,000 行」**冲突**；本文以开放平台 + 帮助中心为准 |
| 18 | 飞书**版本对比**的官方帮助文章 | 未找到独立版本对比文档（仅《多维表格付费权益说明》含对比表）；《飞书定价版本介绍》为旧口径，与细表冲突 |

### 抓取失败/不可用的来源（如实记录）

- `https://www.feishu.cn/price` → **404**（正确入口为 `/pricing`）。
- `https://www.feishu.cn/hc/zh-CN/articles/350837430825`（行数扩容）→ **404**。
- `https://open.feishu.cn/document/server-docs/docs/bitable-v1/app-dashboard/create.md`、`.../form/create.md` → **404**（作为「接口不存在」的反证使用）。
- `scope-list`（API 权限清单）为 `<md-scope-list>` JS 组件，静态抓取拿不到完整 scope 列表。
- 飞书帮助中心正文为 JS 渲染，直接 `curl` 只得空壳，须经 Jina 渲染。

---

*调研与核实：2026-09-12。CDP 浏览器不可用，全部结论基于静态抓取的一手官方文档；凡「未找到」或「官方矛盾」处均已显式标注，未作任何补全或推测性断言。*
