# bitable-plugin · 附件批量导出（飞书多维表格侧边栏插件）

> 目标：在飞书多维表格里**勾选若干记录**，把它们的三个附件列（发票 / 订单截图 / 付款记录）
> 下载并按规则重命名、打包成 zip。
> **不经过任何自建服务器**：文件在浏览器里下载与打包，直接落到操作者电脑。
>
> 这是 `docs/30-review/32-selective-attachment-download.md` 里定下的 **路线 ③B-1**，
> 决策记录见 `docs/30-review/34-attachment-plugin-route-decision.md`。

---

## 为什么不走服务端

| 约束（官方已核实） | 后果 |
|---|---|
| 插件页面托管在 `*.feishupkg.com`，宿主是 `*.feishu.com` | 是跨域 iframe |
| 插件发网络请求必须 **HTTPS + 有效证书 + 服务端开 CORS（`Access-Control-Allow-Origin` 不能是 `*`）** | 局域网里的服务（NAS / 机器人 192.168.1.3）**够不着** |
| 另外还有 60s 超时、**并发 5** 上限、不支持 WebSocket | 大批量下载必须自己排队 |

所以本地打包是唯一"今天就能跑起来"的形态。代价是：**没有 NAS、没有导出审计**——
这两件事留给后面的服务端路线（`docs/30-review/32` §5）。

---

## 目录

```
bitable-plugin/
├── src/
│   ├── core/                  ← 纯逻辑，不碰 SDK/DOM，可在 node 里测
│   │   ├── naming.ts          记录 → 文件名/目录（**与服务端共用的契约**）
│   │   ├── naming.test.ts     21 个测试，含向量逐条比对
│   │   ├── cases.ts           向量用例定义
│   │   ├── download.ts        并发下载（tt.request 优先，退 fetch；并发 3/上限 5）
│   │   ├── zip.ts             fflate 打包 + 触发保存
│   │   └── manifest.ts        manifest.csv / README.txt（UTF-8+BOM）
│   ├── api/bitable.ts         ← 唯一 import 官方 SDK 的地方（读表/视图/记录/附件 URL）
│   ├── App.tsx                ← 界面
│   └── main.tsx
├── testdata/naming-cases.json ← ★ 冻结的行为契约（插件与 Go 两侧同跑）
├── scripts/check-vectors.sh   ← 本地一条命令跑两侧校验
└── dist/                      ← 构建产物（npm run build）
```

**分层纪律**：`core/` 里不许 import `@lark-base-open/js-sdk`，也不许碰 `document`/`window`。
所以命名与打包逻辑能在 node 里跑测试，不必进浏览器——这是这个仓库唯一可自动验证的部分。

---

## 快速开始

### 0. 本机（Linux + node ≥ 20）

```bash
cd bitable-plugin
npm install
npm test            # 纯逻辑测试（21 项，含冻结向量）
npm run build       # 产出 dist/
```

> ⚠️ 本机 `~/.npm` 可能是只读的。构建机若报 `EROFS ... /home/*/.npm/_cacache`，
> 用 `npm install --cache ./.npm-cache`（仓库里已放 `.npmrc`，但环境变量优先级更高会盖掉它）。

### 1. 托管到 Replit（拿到填给多维表格的 HTTPS 地址）

官方侧边栏插件模板本身就是 **Vite + React**（`.replit` 里 `run = "npm run dev"`），
与本工程同构，所以对接只需要三件事。

**推荐做法：从官方模板 Fork，再把本工程的源码覆盖进去**（不用装依赖、不用打包上传）

1. 打开官方 React 模板仓库 <https://github.com/Lark-Base-Team/react-template>，
   按官方《准备开发环境》的做法在 Replit 里 **Fork** 它
   （<https://lark-base-team.github.io/js-sdk-docs/zh/start/env> → 「Replit 官网开发」）。
2. 在 Replit 的文件树里，把**本工程这些文件/目录**覆盖进 Fork 出来的项目：

   | 覆盖 | 说明 |
   |---|---|
   | `src/` | 整个目录替换（含 `api/`、`core/`） |
   | `index.html`、`package.json`、`vite.config.ts`、`tsconfig*.json` | 直接替换 |
   | `.replit` | 用本工程的（里面已设 `run = "npm run dev"`） |
   | `testdata/`、`scripts/` | 可选；只影响本机测试，插件运行不需要 |

   模板里原有的 `src/App.jsx`、`src/index.jsx`、`public/` 等删掉或不再被引用即可。
3. 在 Replit 点 **Run**（或 Shell 里 `npm install` 后 `npm run dev`）。
   起来之后，**点 IDE 里 Webview 右上角的「New Tab」，复制那个 URL**（官方原文：
   「点击 IDE 页面中上方的「Run」按钮启动项目，然后点击 Webview 右侧的"New Tab"，并复制 URL…
   在输入框内填入**预览地址**后点击「确定」」）。
   地址形如 `https://<uuid>.servername.replit.dev`。
   **这个地址就是要填进多维表格的「服务地址」。**

#### ⚠️ Dev URL 不能当长期地址（官方原文，务必知道）

| 事实 | 出处原文 |
|---|---|
| Dev URL 只在**你正在编辑该项目**时存活，**每次重开项目地址可能变化** | 「Development URLs are only live while you actively work on a Replit App. **The URL can change each time you reopen the app.**」 |
| 官方明确它只适合测试 | 「**Development URLs are temporary and intended for testing only.**」 |
| 免费 Starter 账号：**1 个**已发布应用，**30 天后自动下线**（可重发） | 「Get **1** free published app… **This published link will automatically go down after 30 days.**」 |
| Static 部署：托管**免费**，只按流量计费（$0.05/GB），无后端 | 「Hosting: **Free**；Data transfer (per GB): **$0.05**」 |
| Autoscale 部署：空闲 **15 分钟**后缩到 0，来请求时自动唤醒 | 「Goes idle after 15 minutes of inactivity」 |

结论与建议：

- **自己用 / 给小范围用**：先用 Dev URL 完全可行（官方流程就是这么写的），
  代价是"Repl 没在跑就打不开、地址可能变"。
- **要长期稳定**：用 **Static 部署**（本插件是纯静态前端，最匹配；托管免费）。
  Replit 的 Static 部署配置就是「公共目录 + 可选 build 命令」：
  公共目录填构建产物目录（`dist` 或 `.`），build 命令填 `npm run build`。
- **不要用 Autoscale** 图省事：会有冷启动延迟（虽然会被唤醒）。

**所以本工程 `package.json` 里已经写好了官方要求的两个静态托管前提**（见上一节「静态托管的三条硬要求」）。

**备选做法：直接上传本工程**

```bash
bash bitable-plugin/scripts/bundle-for-replit.sh   # 生成只含源码的 zip（约 60KB）
```

把生成的 zip 传到 Replit（Create Repl → Import from zip / 或先推到 GitHub 再 Import from GitHub），
再点 Run 即可。该 zip 已排除 `node_modules`（71MB）与 `dist/`，因为 Replit 会自己 `npm install`。

> ⚠️ 免费账号的 Repl 在一段时间无人访问后会休眠。插件被打开时若 Repl 正在休眠，
> 第一次加载可能要等它唤醒（官方文档也提到「有时候会因为网络以及部署等原因，部署较慢，需要耐心等待」）。
> 需要稳定常驻时，用 Replit 的 Deployments（build = `npm run build`，run = `npm run preview`）。

### 2. 把地址填进多维表格

帮助中心原文：「打开任意多维表格，点击右上角 **多维表格插件** 图标，点击底部的 **自定义插件** 按钮。
在自定义插件面板中点击 **+ 新增插件** 按钮，**填写插件名称及服务地址**，点击 **确定** 即可。」
（新版文档写「点击左下角侧边栏的「更多」按钮 → 自定义插件 → +新增插件」，入口会随版本变，认「自定义插件」这个词即可。）

- 地址要求：官方原文「**我们没有对域名进行限制，只要是 HTTPS 协议连接都可以正常运行。**」
  ——**只要求 HTTPS**；用 Replit 给的地址即可。
- 用本地调试地址（`http://localhost:5173`）也能加载（官方本地调试流程就是这么做的）。
- 官方还说明：**前端插件的权限跟随"执行插件的那个人"**
  （「如果该用户在多维表格界面上无权看某些数据，那么插件中也看不到」），
  所以不需要在插件里配 AppID / 密钥，也**不要**把任何密钥放进前端代码。

### 静态托管的三条硬要求（如果你走部署而不是 Dev URL）

官方对"把前端产物交给多维表格/自己服务器托管"有明确要求，本工程**已经全部满足**：

| 官方要求（原文） | 本工程的做法 |
|---|---|
| 「打包产物的资源引用路径不可以使用绝对路径，请使用相对路径，如在 vite.config.js 中指定 `base:'./'`」 | `vite.config.ts` 里 `base: './'` ✅ |
| 「**禁止使用 history 路由，请使用 hash 路由**」 | 本插件没有前端路由（单页无跳转），天然满足 ✅ |
| 「在 package.json 中设置 `output` 属性值为 `dist`」；上传时**要连 dist 一起提交** | `package.json` 里有 `"output": "dist"` ✅；但**上传产物时不要 .gitignore 掉 dist** |

### 3. 本地调试（可选）

```bash
npm install
npm run dev        # vite dev server，默认 http://localhost:5173
```

把 `http://localhost:5173` 填进多维表格的插件地址即可预览
（官方《准备开发环境》里的「本地编辑器开发」就是这个方式）。

---

## 用起来是什么样

1. **数据源**：选数据表（默认当前打开的）与视图（如「报销整合 · 待交学校」）。
2. **附件字段**：自动映射三个槽位（发票 / 订单截图 / 付款记录）。
   列名对不上时会显示提示，可手动改；选「（不导出）」可跳过某个槽位。
3. **命名与分组**：
   - 模板默认 `{部门}-{购买人}-{日期}-{销方}-{金额}元-{槽位}{序号}`
   - 可用变量：`{部门} {购买人} {日期} {销方} {金额} {槽位} {序号} {发票号码} {发票号码后6位}`
   - **分组已包含的维度会自动从文件名去掉**（按部门分组 → 文件名不再重复部门）
   - 文件夹分类：按物资所属部门 / 按月份 / 不分类
4. **读取记录** → 列表显示每条记录的部门/购买人/金额/日期/附件情况。
5. **勾选**要导的行（默认已勾选"有附件"的行），或点「全选有附件的」。
6. **下载所选记录** → 逐条下载 + 进度显示 → 打包 zip → 浏览器保存。
7. zip 里除文件外还有：
   - `manifest.csv`（zip 内路径 / 文件名 / 槽位 / 记录ID / 审批实例号 / 原始文件名 / token / MIME / 字节数 / 失败原因）
   - `README.txt`（本次导出参数摘要）

---

## 命名规则（契约，改动必须同步两侧）

| 规则 | 行为 |
|---|---|
| 非法字符 | 删除 `/ \ : * ? " < > \|` 与控制字符（**含 `/`，防路径穿越**） |
| 首尾 | 去掉 `. - _` 与空格（Windows 不允许文件名以 `.` 或空格结尾） |
| 连续分隔符 | `---` → `-` |
| 字段缺失 | 用「未知」占位，并给出 `MISSING_FIELD` 告警 |
| 日期 | 只取 `YYYY-MM-DD`（`2026/6/2`、`2026-06-02 12:30` 都能解析） |
| 金额 | 剥 `¥`、千分位，保留两位小数（`¥1,234.5` → `1234.50`） |
| 序号 | **仅当同一槽位有多个附件**时出现在文件名里，两位（`发票01`、`发票02`） |
| 扩展名 | mime 优先，缺失时回退原文件名 |
| 字节上限 | 默认 120 字节（含扩展名）；超长时**逐级均匀压缩各部件**并保扩展名 |
| 重名 | 同目录内追加 `~2`、`~3`，并给出 `COLLISION` 告警 |
| 空附件行 | 不产文件，给出 `NO_ATTACHMENTS` 告警（**绝不静默少文件**） |

---

## 测试

```bash
npm test                      # 插件侧：21 项
bash scripts/check-vectors.sh # 两侧一起跑（插件 TS + 服务端 Go）
```

`testdata/naming-cases.json` 是**冻结契约**：插件与 Go 两侧的实现都跑它。
改动命名行为时必须**有意**执行 `npm run vectors` 并 review diff，然后在同一提交里让两侧都过。

---

## 已知限制（如实记录）

| # | 限制 | 说明 |
|---|---|---|
| L1 | **文件落在操作者电脑** | 没有 NAS、没有导出审计；需要审计就走服务端路线 |
| L2 | **附件链接只有效 10 分钟** | 所以"取链接 → 立刻下载"必须紧挨着，不能预取一批 URL |
| L3 | **网络并发上限 5** | 这里用 3；超大导出（爆发期一周约 588 张图）会比较慢 |
| L4 | **不能后台跑** | 插件只在人打开侧边栏时存在，不能定时 |
| L5 | **userId 与开放平台不通** | `Bridge.getUserId` 与 `open_id/user_id` 不是一套，审计里的"操作人"另需口径 |
| L6 | **需要公网 HTTPS 地址** | 本地 `localhost` 只能调试；正式使用必须托管（当前选 Replit） |
| L7 | **勾选粒度是"行"** | 表里是「一张发票一行」，勾一行 = 一张票的附件（同一审批单多张发票是多行） |

---

## 与服务端路线的关系

- **命名/分组规则完全共用**（同一份向量，见 `finance-router/internal/naming/`）。
  将来做服务端导出时，应直接复用 Go 侧这个包，不要再写第二套。
- 服务端路线要解决的是本插件**拿不到**的两件事：落 NAS、留导出审计
  （需求矩阵里的「导出发票并保存导出记录」是高优先级）。
- 两条路线的产物应能互相核对：靠的就是 `manifest.csv` 里的记录 ID 与 token。
