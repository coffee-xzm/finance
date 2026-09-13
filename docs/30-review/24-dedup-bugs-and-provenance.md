# 重复报销检测的三个真 bug（已修复）

> 2026-09-13。起因是问题「为什么现在归档中也会有重复的」。
> 查下去发现不是归档的问题，而是**查重链路本身有三处是坏的**：
> 一处假阴性、一处误判、一处"重建的保护是假的"。
> 三处都已修复并有回归测试。

---

## 0. 结论先行

用户看到的两张表各 8 行**没有行级重复**。真实情况是：

**同一张发票被提交了 3 次**（`<INSTANCE_A>` / `<INSTANCE_B>` / `<INSTANCE_C>`，物资名"测试测试，不要批"，
是测试数据）。三张图（发票 / 订单 / 付款）在三个实例间**字节完全相同**。

但"归档里看起来有重复"只是表象。顺着查下去，查重链路本身有三处 bug。

---

## 1. Bug ①：`-dups` 永远是假阴性

### 现象

`evidence.sha256` 上有 `UNIQUE` 索引。于是**库里永远不可能存在重复行**。

而原来的 `-dups` 实现是：

```sql
SELECT sha256, GROUP_CONCAT(DISTINCT instance_code)
FROM evidence GROUP BY sha256 HAVING COUNT(DISTINCT instance_code) > 1
```

这个查询**恒为空**。它报告"未发现重复报销嫌疑"，但它不可能发现任何东西。

### 为什么危险

比没有这个命令更糟 —— 它给出的是**负向结论**。
运维看到"未发现重复"，会以为查重是好的。

### 修复

重复的事实只存在于**磁盘文件**上，不在库里。改为扫描 `data/extract/files/`，
在入库**之前**按 sha256 分组。`-dups` 现在读文件，不读库。

```
⚠ 发现 3 组重复报销嫌疑（同一张图出现在多个实例）：
  sha=3023b2bc63b9ce7e… 出现在 3 个实例:
      <INSTANCE_B>
      <INSTANCE_C>
      <INSTANCE_A>
```

有重复时退出码 **3**（非零），可被流水线拦截。

回归测试：`TestDuplicateScanReadsDiskNotDB`。

---

## 2. Bug ②：重跑实例会被自己判成"重复报销"

### 现象

`SaveInstance` 对 `evidence` 是**纯 INSERT**，没有先清理本实例的旧行。

所以对同一个实例跑第二次：

```
第一次：INST_A 的发票 sha=3023b2bc 入库 ✓
第二次：INST_A 的发票 sha=3023b2bc 再次 INSERT → 撞唯一索引 → DupError
        → 报「⛔ 重复报销拦截：首次来自实例 INST_A」
```

**它自己挡自己。** 唯一索引要防的是*别的实例*复用同一张图，不是本实例自己更新自己。

### 影响面

- `pipeline.sh --reset` 重跑会全线失败
- 修了 prompt / 换了模型后想重抽历史单，全部被误判为重复报销
- `submission` 行用了 `ON CONFLICT DO UPDATE`（作者本来就想支持重跑），
  证据行却是纯 INSERT —— **两者语义矛盾**

### 修复

两处：

1. `SaveInstance` 在插入前 `DELETE FROM evidence WHERE instance_code = ?`。
   同实例重跑 = 整体替换本单证据。
2. `extract` 的 OCR 前查重，跳过 `owner == 当前实例` 的命中。

回归测试：`TestRerunSameInstanceIsIdempotent`、`TestRerunReleasesRemovedEvidence`。

第二条测试覆盖一个容易漏的推论：**换图后旧 sha 必须释放**，
否则那张图被永久锁死，别人再也提交不了。

---

## 3. Bug ③（最隐蔽）：reindex 重建出来的保护是**假的**

### 背景

`evidence.sha256` 算的是**原始下载字节**的哈希。发票附件是 PDF（146 KB），
所以发票的 sha 是 **PDF 的哈希**。

但磁盘上留下的是 `pdftoppm` **光栅化后的 PNG**。默认 `-keep-pdf` 关闭，
**PDF 原始字节随即被删除**。

于是同一张发票有两个 sha：

| 来源 | sha256 |
|---|---|
| 库 / manifest（原始 PDF 字节） | `3023b2bc63b9…` |
| 磁盘上的 PNG（光栅化结果） | `ed7d5c57b0ab…` |

### 后果

`ReindexFromFiles` 原本对**落盘文件**求哈希来"从磁盘重建唯一性"。
对 PDF 类附件，它算出来的 sha 与库里**不是同一个值**。

这个失效是**静默的**，而且方向最坏：

```
库被删 → reindex 从 PNG 重建，写入 sha=ed7d5c57（错的）
       → 声称"已恢复唯一性保护"
同一份 PDF 再次提交 → sha=3023b2bc → 查不到 → 放行
       → 重复报销漏过去了，而且系统显示一切正常
```

实测确认：修复前 reindex 把 `ed7d5c57` 写成了 `<INSTANCE_B>` 的发票证据。

### 修复

新增 **provenance 旁路表**：`data/extract/files/<实例>/_provenance.tsv`

```
<落盘文件名>\t<原始sha256>\t<media_type>\t<原始字节数>
```

append-only，由 `downloadOne` 在落盘后写入。reindex 优先读旁路表取原始 sha；
没有旁路表时才退化为对文件求哈希，**并且显式告警**：

```
⚠ 15 个文件缺少 provenance 旁路表（_provenance.tsv），
  只能对落盘文件求 sha —— 若原附件是 PDF，这个 sha 与库里的**不是同一个**，
  由此重建的唯一性保护对该文件无效。请重跑 extract 生成旁路表。
```

> 对 JPG/PNG 原样落盘的附件，两者恰好相等，所以这个问题只影响 PDF。
> 而**发票几乎都是 PDF** —— 也就是最重要的那一类。

### 附带修复：reindex 改为自校正

reindex 现在先 `DELETE FROM evidence WHERE provider='reindex'` 再重建。
陈旧占位行带的是过时/错误的 sha，会参与唯一性判定 —— **既可能挡住不该挡的，
又可能放行该挡的**，比没有这行更危险。只删自己插入的行，绝不碰真正 OCR 出来的证据。

回归测试：`TestReindexIsSelfCorrecting`、`TestReindexPrefersProvenanceRawSHA`。

---

## 4. 另一个独立的浪费：重复单仍然全额付 OCR 钱

原来的流程顺序是：

```
下载 → 光栅化 → **OCR（付费）** → 三道比对 → 入库时才发现重复 → 拦截
```

一张重复的发票单要白烧 ≈5,800 tok。查重完全可以在**下载完、算完 sha 之后、
OCR 之前**做 —— 已经入库的图不可能带来新信息。

修复后：

```
⛔ 发票文件  #1 重复图（首次来自 <INSTANCE_A>）跳过 OCR，省 ≈5340 tok
⛔ 订单截图  #1 重复图（首次来自 <INSTANCE_A>）跳过 OCR，省 ≈256 tok
⛔ 付款截图  #1 重复图（首次来自 <INSTANCE_A>）跳过 OCR，省 ≈256 tok
⛔ 重复报销拦截：本单含 3 张已入库的图，整单跳过
```

整单拦截语义不变（`SaveInstance` 本来就是整单一个事务）。

---

## 5. 实测验证（真实数据）

对三个重复实例重跑：

| 实例 | 结果 | OCR 花费 |
|---|---|---|
| `<INSTANCE_A>`（首次提交者） | ✓ 正常抽取入库 3 张 | 正常 |
| `<INSTANCE_B>` | ⛔ 整单拦截 | **0** |
| `<INSTANCE_C>` | ⛔ 整单拦截 | **0** |

修复后 reindex：

```
✓ 从本地图补入 11 条证据（恢复唯一性）
⚠ 15 个文件缺少 provenance 旁路表 …（其他更早处理的实例，无旁路表）
⚠ 发现 3 组重复报销嫌疑（同一张图出现在多个实例）
```

库内最终状态：

```
<INSTANCE_A> | 付款截图 | 16b7d6414784 | siliconflow-qwen-vl
<INSTANCE_A> | 发票文件 | 3023b2bc63b9 | siliconflow-qwen-vl   ← PDF 原始字节
<INSTANCE_A> | 订单截图 | 5fe1c7bab868 | siliconflow-qwen-vl
<INSTANCE_B> | （无证据）
<INSTANCE_C> | （无证据）

全库 evidence=20 唯一sha=20 一致=true
错误 sha ed7d5c57 残留行: 0
```

发票 sha 从错误的 `ed7d5c57`（PNG）纠正为正确的 `3023b2bc`（PDF）。

---

## 6. 遗留

- `data/extract/files/` 下另有 15 个文件是修复前处理的，没有旁路表。
  重跑 `extract` 即可生成；在那之前，这些实例的 reindex 保护是 degraded 的（已告警）。
- 查重是**按图片**的。理论上"同一张发票截成不同尺寸再传"能绕过 sha 比对。
  当前不做感知哈希 —— 实际场景里队员是直接转发原图，不是重新截图。
  若将来出现绕过，再上 pHash 作为第二道。
