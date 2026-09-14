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
