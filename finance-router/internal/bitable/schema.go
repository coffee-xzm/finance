// Package bitable 定义多维表格的表结构。
//
// 设计原则（用户 2026-09-13 明确）：
//  1. **多维表格是给人看的** —— 不放底层元信息
//     （sha256 / provider / model / trace_id / 置信度 / 大写原文 / 税额自检… 全部只留本地）；
//  2. **一行 = 一条审批实例**（不是一张图），与现有生产表一致；
//  3. **图片用附件字段**，方便人工复核。
//
// 关于「图片用超链接」的修正（有实测依据）：
//
//	审批附件的下载链接带 authcode，**只有 24 小时有效期**
//	（解码 `_ID:..._1789281781:1789368181_V3`，起止相差 86400 秒），
//	存进表里第二天就点不开了，**不适合当长期超链接**。
//	另外生产表里那种 `www.feishu.cn/approval/admin/previewAttachment?key=` 链接
//	是服务端签名的二进制 blob，**我们无法自己构造**。
//	所以改用**附件字段**（长期有效、可内联预览），
//	另保留 `申请编号` 超链接指向审批原单用于溯源。
package bitable

// FieldType 是飞书多维表格字段类型编号（官方枚举）。
type FieldType int

const (
	TypeText         FieldType = 1
	TypeNumber       FieldType = 2
	TypeSingleSelect FieldType = 3
	TypeMultiSelect  FieldType = 4
	TypeDate         FieldType = 5
	TypeCheckbox     FieldType = 7
	TypeUser         FieldType = 11
	TypePhone        FieldType = 13
	TypeURL          FieldType = 15 // 超链接
	TypeAttachment   FieldType = 17 // 附件
	TypeSingleLink   FieldType = 18 // 单向关联（写 record_id 数组）
	// 只读，不可作为写入目标：
	TypeLookup     FieldType = 19
	TypeFormula    FieldType = 20
	TypeCreatedAt  FieldType = 1001
	TypeUpdatedAt  FieldType = 1002
	TypeCreatedBy  FieldType = 1003
	TypeUpdatedBy  FieldType = 1004
	TypeAutoNumber FieldType = 1005
)

// 选项照抄现有生产表，便于日后合并。
var (
	RealDepartments = []string{
		"哨兵组", "宣管组", "工程组", "无人机组", "机械组", "梯队", "步兵组",
		"电控组", "硬件组", "英雄组", "视觉组", "雷达组", "飞镖组",
	}
	RealMaterialTypes = []string{
		"外购工具", "实验室建设", "宣传物资", "差旅费用", "机械加工",
		"机械外购件", "电控物资", "裁判系统相关", "视觉物资",
	}
	RealFundSources = []string{
		// ★ 必须以**当前审批表单**的选项为准：表单已简化成"个人 / 老师垫付"。
		//   写一个表里没有的选项时飞书会**自动新建**，表里就会慢慢长出一堆
		//   历史遗留选项；反过来，表里有表单已经没有的选项也不会有人用。
		"个人", "老师垫付",
	}
	RealApprovalStatus = []string{
		"审批中", "已通过", "已拒绝", "已取消", "已撤回", "已终止", "已删除",
	}
)

type Field struct {
	Name          string
	Type          FieldType
	Options       []string
	Formatter     string
	DateFmt       string
	Note          string
	SourceOfTruth string // feishu | form | image | local | human
}

type Table struct {
	Key         string
	Name        string
	Authority   string
	Description string
	Fields      []Field
}

// slots 是三个附件槽位的显示名。
//
// ★ 必须与**审批表单里的控件名**逐字一致：它们既决定落表时写哪个附件列，
// 也是本地库里 evidence.slot / doc_group 的证据引用键。
// 表单从「发票文件 / 付款截图」改名为「发票 / 付款记录」，这里跟着改。
var slots = []string{"发票", "订单截图", "付款记录"}

// ReviewTable 是**人工复核用**的表：一行一张发票，图片作为附件内联可看。
//
// 字段取舍的唯一标准：**这一列对人核对有没有用**。
// 只给人看结论、图片、以及能回答"这单是谁报的、报了多少钱"的字段。
// 技术性的中间产物（配对键、分组序号、号码来源…）一律不进表。
func ReviewTable() Table {
	f := []Field{
		{Name: "审批实例号", Type: TypeText, Note: "关联键，用来回查审批", SourceOfTruth: "feishu"},
		// ★「发票号码」是"一张发票一行"的**幂等键**。
		//   落表时要靠它判断"这一行写过了没有"，所以必须留在表里 ——
		//   删掉它，同一张票每次同步都会被当成新行重复写入。
		//   它同时也是人核对时最常拿来对照原图的字段。
		{Name: "发票号码", Type: TypeText, Note: "★ 幂等键之一，勿删", SourceOfTruth: "image"},
		{Name: "申请编号", Type: TypeURL, Note: "点开直达审批原单（溯源用；图片本身在附件列）", SourceOfTruth: "feishu"},
		{Name: "申请状态", Type: TypeSingleSelect, Options: RealApprovalStatus, SourceOfTruth: "feishu"},
		{Name: "发起时间", Type: TypeDate, DateFmt: "yyyy-MM-dd HH:mm", SourceOfTruth: "feishu"},
		{Name: "发起人", Type: TypeText, SourceOfTruth: "feishu"},
		{Name: "发起人部门", Type: TypeText, SourceOfTruth: "feishu"},

		{Name: "物资所属部门", Type: TypeMultiSelect, Options: RealDepartments, SourceOfTruth: "form"},
		{Name: "是否为支付宝付款", Type: TypeSingleSelect, Options: []string{"否", "是"}, SourceOfTruth: "form"},
		{Name: "物资种类", Type: TypeSingleSelect, Options: RealMaterialTypes, SourceOfTruth: "form"},
		{Name: "购买人", Type: TypeText, SourceOfTruth: "form"},
		{Name: "资金来源", Type: TypeSingleSelect, Options: RealFundSources, SourceOfTruth: "form"},

		// ── 从图读出（供人核对）──
		{Name: "图读金额(元)", Type: TypeNumber, Formatter: "0.00", Note: "发票价税合计（含税），单位：元", SourceOfTruth: "image"},
		{Name: "图读税额(元)", Type: TypeNumber, Formatter: "0.00", Note: "单位：元。★ 仅供参考 —— 无人独立校验税额分摊", SourceOfTruth: "image"},
		{Name: "图读日期", Type: TypeDate, DateFmt: "yyyy-MM-dd", SourceOfTruth: "image"},
		{Name: "销方名称", Type: TypeText, SourceOfTruth: "image"},

		// ── 图片（附件：长期有效、可内联预览）──
		{Name: "发票", Type: TypeAttachment, SourceOfTruth: "image"},
		{Name: "订单截图", Type: TypeAttachment, SourceOfTruth: "image"},
		{Name: "付款记录", Type: TypeAttachment, SourceOfTruth: "image"},

		// ── 机器核对结论（人话，不含技术细节）──
		{Name: "核对结果", Type: TypeSingleSelect,
			Options: []string{"一致", "存疑", "缺件"}, SourceOfTruth: "local"},
		{Name: "差异说明", Type: TypeText,
			Note:          "为什么这么判：金额对照 + 配对依据（原文来自 配对依据 列，已合并到这里）",
			SourceOfTruth: "local"},

		// ── 人工审核 ──
		{Name: "人工审核", Type: TypeSingleSelect, Options: []string{"待审", "通过", "驳回"},
			Note: "★ 人只改这一列与「审核备注」", SourceOfTruth: "human"},
		{Name: "审核备注", Type: TypeText, SourceOfTruth: "human"},
		{Name: "审核时间", Type: TypeDate, DateFmt: "yyyy-MM-dd HH:mm",
			Note: "服务在归档时回写（记录人工确认的时间点）", SourceOfTruth: "local"},
		{Name: "已归档", Type: TypeCheckbox, Note: "幂等标记：已复制进整合表", SourceOfTruth: "local"},
	}
	return Table{
		Key:       "review",
		Name:      "报销核对",
		Authority: "机器预填 + 人工确认",
		Description: "一行 = 一张发票。图片在附件列可直接查看；" +
			"配对键、分组序号、号码来源等技术性中间产物只保存在本地，不进表。",
		Fields: f,
	}
}

// IntegratedTable 是归档表：只收人工确认通过的行。
func IntegratedTable() Table {
	src := ReviewTable()
	keep := map[string]bool{
		"审批实例号": true, "发票号码": true,
		"申请编号": true, "物资所属部门": true, "物资种类": true,
		"购买人": true, "资金来源": true, "是否为支付宝付款": true,
		"图读金额(元)": true, "图读税额(元)": true, "图读日期": true, "销方名称": true,
		"发票": true, "订单截图": true, "付款记录": true,
		"审核备注": true,
	}
	var f []Field
	for _, x := range src.Fields {
		if keep[x.Name] {
			f = append(f, x)
		}
	}
	f = append(f,
		Field{Name: "归档时间", Type: TypeDate, DateFmt: "yyyy-MM-dd HH:mm", SourceOfTruth: "local"},
		Field{Name: "来源行", Type: TypeText, Note: "源表 record_id，便于回溯", SourceOfTruth: "local"},
	)
	return Table{
		Key:         "integrated",
		Name:        "报销整合",
		Authority:   "人工确认后的归档",
		Description: "只收「人工审核=通过」的行；复制而非移动，源表保留痕迹。",
		Fields:      f,
	}
}

// Slots 返回三个附件槽位的显示名。
func Slots() []string { return slots }

// PurchaseRequestTable 是「27采购申请表」（wiki）的目标结构（2026-09-19 用户要求）：
// 采购审批通过后，把采购信息写进这张表，**字段对齐参考表 `tbllUFPS…`**。
//
// 参考表实测 20 个字段（名称/类型/选项逐字照抄）；其中：
//   - 参考表的主字段是「申请编号」(Url)。目标表已有的主字段是文本「项目名称」，
//     无法安全改成 URL 类型，所以这里用**「项目名称」当主字段**（同为文本、人更易读）。
//   - 「商品图片」在参考表里是 Url；我们拿到的是审批附件的 24 小时临时直链，
//     写进去第二天就点不开，所以在字段类型上改成**附件**（下载转存 → file_token）。
//
// ★ 2026-09-19 用户从表里删掉了 4 列，这里**同步删除**，否则
// `purchase-request-init` 会把它们重建回来：
// 当前处理人 / 审批节点 / 费用明细_规格 / 费用明细_金额-币种。
// 现为 16 个字段（与线上一致）。
func PurchaseRequestTable() Table {
	f := []Field{
		{Name: "项目名称", Type: TypeText, SourceOfTruth: "form"},
		{Name: "申请编号", Type: TypeURL, SourceOfTruth: "feishu"},
		{Name: "申请状态", Type: TypeSingleSelect,
			Options:       []string{"审批中", "已通过", "已拒绝", "已取消", "已撤回", "已终止", "已删除"},
			SourceOfTruth: "feishu"},
		{Name: "审批流程", Type: TypeSingleSelect,
			Options: []string{"流动资金采购审批", "采购审批 - 27Test"}, SourceOfTruth: "feishu"},
		{Name: "发起时间", Type: TypeDate, DateFmt: "yyyy-MM-dd HH:mm", SourceOfTruth: "feishu"},
		{Name: "完成时间", Type: TypeDate, DateFmt: "yyyy-MM-dd HH:mm", SourceOfTruth: "feishu"},
		{Name: "发起人", Type: TypeUser, SourceOfTruth: "feishu"},
		{Name: "发起人部门", Type: TypeText, SourceOfTruth: "feishu"},
		{Name: "采购类别", Type: TypeSingleSelect,
			Options: []string{"机械成品件", "机械加工件", "电控物资", "其他"}, SourceOfTruth: "form"},
		{Name: "费用明细_名称", Type: TypeText, SourceOfTruth: "form"},
		{Name: "费用明细_金额", Type: TypeNumber, Formatter: "0.00", SourceOfTruth: "form"},
		{Name: "费用明细_数量", Type: TypeNumber, Formatter: "0.00", SourceOfTruth: "form"},
		{Name: "商品图片", Type: TypeAttachment,
			Note: "审批里的商品图片（临时直链 24h 失效）→ 转存为附件，长期有效", SourceOfTruth: "image"},
		{Name: "采购事由", Type: TypeText, SourceOfTruth: "form"},
		{Name: "期望交付时间", Type: TypeDate, DateFmt: "yyyy-MM-dd", SourceOfTruth: "form"},
		{Name: "SourceID", Type: TypeText, Note: "对账/幂等键：写采购审批实例 code", SourceOfTruth: "local"},
	}
	return Table{
		Key:       "purchase_request",
		Name:      "27采购申请表",
		Authority: "服务写（采购审批通过后）",
		Description: "采购审批通过后写入；字段对齐参考表 tbllUFPS…。" +
			"主字段为「项目名称」（参考表主字段是 URL 的「申请编号」，不便照搬）。",
		Fields: f,
	}
}

// LedgerTable 是「27 - 收支表」的结构契约（flow base）。
//
// 为什么要在这里定义：这张表是**线上生产表**，结构由用户维护，但它现在是
// 「27-流水登记」审批的落地目标 —— 登记表单的控件必须有对应列可写，
// 所以缺的列由 `cmd/ledger-init` 按这份清单补齐（见 docs/30-review/33 §12.15）。
//
// 只读列（登记人/登记时间/当前金额/余额段）+ 主字段「流水审批ID」**不在此列**：
// 前者不能写，后者由用户在表里改成 Url 类型（我们只写值）。
// 这里列出的是"本服务会写"的列 + 需要存在的列。
func LedgerTable() Table {
	f := []Field{
		{Name: "流水审批ID", Type: TypeURL,
			Note: "主字段：本行对应的「27-流水登记」实例深链（幂等锚点）", SourceOfTruth: "local"},
		{Name: "🔗 关联发票任务", Type: TypeAttachment, SourceOfTruth: "human"},
		{Name: "🔗 科目 / 去向", Type: TypeSingleSelect,
			Options: []string{"项目组物资", "技术组物资", "差旅相关", "个人/老师还款",
				"裁判系统赔款", "官方物资", "其他（备注）"}, SourceOfTruth: "form"},
		{Name: "收支方向（支出/收入）", Type: TypeSingleSelect,
			Options: []string{"当前本金", "支出", "收入"}, SourceOfTruth: "form"},
		{Name: "🔗 项目组", Type: TypeSingleSelect,
			Options: []string{"机械组", "宣管组", "电控组", "哨兵组", "视觉组", "步兵组",
				"英雄组", "工程组", "无人机组", "飞镖组", "雷达组", "硬件组", "重装组"},
			SourceOfTruth: "form"},
		{Name: "🔗 金额", Type: TypeNumber, Formatter: "0.00", SourceOfTruth: "form"},
		{Name: "🔗 付款/收款截图", Type: TypeAttachment,
			Note: "登记单「转账截图」转存（临时直链 24h 失效）", SourceOfTruth: "image"},
		{Name: "🔗 发生日期（付款/下单/到账）", Type: TypeDate, DateFmt: "yyyy/MM/dd", SourceOfTruth: "form"},
		{Name: "🔗 关联申请单ID", Type: TypeURL, SourceOfTruth: "local"},
		{Name: "🔗 备注", Type: TypeText, SourceOfTruth: "form"},
		{Name: "发票收集进度（待配置）", Type: TypeSingleSelect,
			Options: []string{"待办", "已通过"}, SourceOfTruth: "local"},
		{Name: "关联人", Type: TypeUser, Note: "该行对应审批的提交人", SourceOfTruth: "feishu"},
		{Name: "金额来源", Type: TypeSingleSelect,
			Options: []string{"学校报销", "竞赛经费", "众筹资金（个人补贴）", "大创经费", "指导老师垫付"},
			Note:    "2026-09-21 新增：对齐登记表单的「金额来源」控件", SourceOfTruth: "form"},
		{Name: "金额去向", Type: TypeSingleSelect,
			Options: []string{"指导老师还款", "个人垫付还款", "物资购买", "差旅垫付"},
			Note:    "2026-09-21 新增：对齐登记表单的「金额去向」控件", SourceOfTruth: "form"},
		{Name: "27 - 流动资金采购审批", Type: TypeSingleLink,
			Note: "单向关联到采购镜像表（写 record_id 数组）", SourceOfTruth: "local"},
	}
	return Table{
		Key:         "ledger",
		Name:        "27 - 收支表",
		Authority:   "服务写（采购派生 + 流水登记覆盖）",
		Description: "一行 = 一笔流水（采购的每条费用明细一行）。登记单通过后按登记数据覆盖。",
		Fields:      f,
	}
}
