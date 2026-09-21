package config

// 本文件是"静态配置字典"：把原先散落在 Go 常量里的表头、选项、审批 code、
// 控件 id、业务规则集中到 config.yml 控制。
//
// 约定（用户 2026-09-19）：
//   - config.yml 是唯一可改的地方；这里的内置值是**兼容默认**，任何一项都能被覆盖；
//   - 字段名必须与线上表头**逐字一致**（含 emoji、全角括号、空格）；
//   - 角色名（role）用于把代码逻辑与"哪张审批/哪张表"解耦。
//
// 校验：cmd/doctor 会拿这份字典去比对线上表头，字段名写错会立刻报错，
// 而不是等写飞书时报 1254045（FieldNameNotFound）。

// 审批角色。
const (
	RoleInvoiceCollect = "invoice_collect"
	RolePurchase       = "purchase"
	// RoleLedgerRegister 是「27-流水登记」：采购通过后先给财务登记人开这张单，
	// 由他核对/补全（转入/支出、金额、转账日期、截图、来源/去向），通过后再开票。
	RoleLedgerRegister = "ledger_register"
)

// 多维表格 base 名字。
const (
	BaseReview = "review"
	BaseFlow   = "flow"
	// BasePurchaseRequest 是采购审批通过后要写入的「27采购申请表」（wiki）。
	BasePurchaseRequest = "purchase_request"
)

// ApprovalRole 是一个审批定义的绑定（角色 → code + 期望名称）。
type ApprovalRole struct {
	Role       string `yaml:"role"`
	Code       string `yaml:"code"`
	NameExpect string `yaml:"name_expect"`
}

// BaseConfig 是一个多维表格文档（base）及其表。
type BaseConfig struct {
	AppToken string            `yaml:"app_token"`
	Tables   map[string]string `yaml:"tables"`
}

// Dict 是全部静态字典。
type Dict struct {
	Fields            map[string]map[string]string            `yaml:"fields"`
	Controls          map[string]map[string]string            `yaml:"controls"`
	Options           map[string]map[string]map[string]string `yaml:"options"`
	Slots             map[string]string                       `yaml:"slots"`
	Rules             Rules                                   `yaml:"rules"`
	DeptMap           map[string]string                       `yaml:"dept_map"`
	Write             WritePolicy                             `yaml:"write_policy"`
	Triggers          Triggers                                `yaml:"triggers"`
	PurchaseToInvoice PurchaseToInvoice                       `yaml:"purchase_to_invoice"`
}

// Rules 是业务规则（口径）。
type Rules struct {
	// 科目/去向：项目组 ∈ TechGroups → SubjectTech，否则 SubjectProject。
	TechGroups     []string `yaml:"tech_groups"`
	SubjectTech    string   `yaml:"subject_tech"`
	SubjectProject string   `yaml:"subject_project"`

	// 项目组 → 物资所属部门的别名（例如 工程组/英雄组 → 重装组）。
	DeptAlias map[string]string `yaml:"dept_alias"`

	// 人工审核写什么。
	ReviewPass string `yaml:"review_pass"`
	ReviewFail string `yaml:"review_fail"`

	// 流水「发票收集进度」。
	InvoiceProgressOptions []string `yaml:"invoice_progress_options"`
	InvoiceProgressTodo    string   `yaml:"invoice_progress_todo"`
	InvoiceProgressDone    string   `yaml:"invoice_progress_done"`
}

// WritePolicy 是写表策略。
type WritePolicy struct {
	OnlyFillBlank *bool    `yaml:"only_fill_blank"`
	ServiceOwned  []string `yaml:"service_owned"`
}

// Triggers 是状态机触发开关。
type Triggers struct {
	ArchiveOnApprovalApproved *bool `yaml:"archive_on_approval_approved"`
	AutoApproveWhenClean      *bool `yaml:"auto_approve_when_clean"`
}

// PurchaseToInvoice 是"采购 → 发票收集"的落地方式。
type PurchaseToInvoice struct {
	// rollback_to_start：创建后立刻退回到发起人；submit_again：靠"再次提交"。
	DraftMode string `yaml:"draft_mode"`

	// NameSuffix 是代建发票单时写进「名称（选填，采购提示）」的后缀：
	//
	//	名称 = <采购审批的「项目名称」> + NameSuffix  →  如「测试-采购审批」
	//
	// 后缀是给收票人看的"来源注释"：一眼看出这张发票单是哪笔采购带出来的。
	// 置空串 = 只填项目名称、不加后缀；项目名称为空时一律不填该控件。
	NameSuffix string `yaml:"name_suffix"`
}

func boolPtr(b bool) *bool { return &b }

// defaultDict 返回与当前线上结构一致的默认字典。
func DefaultDict() Dict {
	return Dict{
		Fields: map[string]map[string]string{
			// 报销核对（源表）+ 报销整合（归档表）
			BaseReview: {
				"instance_no":    "审批实例号",
				"invoice_no":     "发票号码",
				"applink":        "申请编号",
				"apply_status":   "申请状态",
				"start_time":     "发起时间",
				"applicant":      "发起人",
				"applicant_dept": "发起人部门",
				"departments":    "物资所属部门",
				"material_type":  "物资种类",
				"buyer":          "购买人",
				"fund_source":    "资金来源",
				"is_alipay":      "是否为支付宝付款",
				"amount":         "图读金额(元)",
				"tax":            "图读税额(元)",
				"invoice_date":   "图读日期",
				"seller":         "销方名称",
				"invoice_att":    "发票",
				"order_att":      "订单截图",
				"payment_att":    "付款记录",
				"verdict":        "核对结果",
				"explain":        "差异说明",
				"human_review":   "人工审核",
				"review_note":    "审核备注",
				"review_time":    "审核时间",
				"archived":       "已归档",
				"archive_time":   "归档时间",
				"source_row":     "来源行",
			},
			// 27 - 收支表
			"ledger": {
				// ★ 主字段（原 AutoNumber「记录ID」，2026-09-21 用户改成 Url 类型并改名
				//   「流水审批ID」）→ 指向「27-流水登记」实例，是本服务写流水的幂等锚点。
				"flow_id":          "流水审批ID",
				"invoice_task":     "🔗 关联发票任务",
				"subject":          "🔗 科目 / 去向",
				"direction":        "收支方向（支出/收入）",
				"group":            "🔗 项目组",
				"amount":           "🔗 金额",
				"screenshots":      "🔗 付款/收款截图",
				"occurred_at":      "🔗 发生日期（付款/下单/到账）",
				"apply_link":       "🔗 关联申请单ID",
				"note":             "🔗 备注",
				"related_user":     "关联人",
				"amount_source":    "金额来源", // 2026-09-21 新增：对齐「27-流水登记」的「金额来源」控件
				"amount_dest":      "金额去向", // 2026-09-21 新增：对齐「27-流水登记」的「金额去向」控件
				"registrant":       "登记人",
				"registered_at":    "登记时间",
				"invoice_progress": "发票收集进度（待配置）",
				"current_amount":   "当前金额",
				"purchase_link":    "27 - 流动资金采购审批",
			},
			// 27 - 流动资金采购审批（只读镜像）
			"purchase": {
				"apply_link":      "申请编号",
				"apply_status":    "申请状态",
				"flow":            "审批流程",
				"start_time":      "发起时间",
				"finish_time":     "完成时间",
				"applicant":       "发起人",
				"applicant_dept":  "发起人部门",
				"category":        "采购类别",
				"project_name":    "项目名称",
				"detail_name":     "费用明细_名称",
				"detail_amount":   "费用明细_金额",
				"detail_qty":      "费用明细_数量",
				"image":           "商品图片",
				"reason":          "采购事由",
				"expect_delivery": "期望交付时间",
				"source_id":       "SourceID",
			},
		},
		Controls: map[string]map[string]string{
			RoleInvoiceCollect: {
				"departments":   "widget17893053389170001",
				"name":          "widget17898262745720001", // 「名称（选填，采购提示）」，2026-09-19 表单新增
				"material_type": "widget17893752856430001",
				"fund_source":   "widget17893057137390001",
				"buyer":         "widget17893729431580001",
				"is_alipay":     "widget17893593019510001",
				"invoice_att":   "widget17893528488520001",
				"order_att":     "widget17893592618150001",
				"payment_att":   "widget17893592631050001",
			},
			RolePurchase: {
				"group":        "widget17897890872260001",
				"category":     "widget16510608666360001",
				"project_name": "widget17812356472430001",
				"detail":       "widget16510609006710001",
				"image":        "widget16510609389860001",
				// 「费用明细」明细控件里的三个子控件（建单/测试要用）
				"detail_name":   "widget16510609105290001",
				"detail_amount": "widget16510609358260001",
				"detail_qty":    "widget16510609215120001",
			},
			// 「27-流水登记」表单控件（2026-09-21 只读读到的 8 个控件）。
			RoleLedgerRegister: {
				"date":            "widget16487160384360001", // 转账日期（date）
				"kind":            "widget17319363822710001", // 类型（radioV2：转入/支出）
				"transfer_amount": "widget16487160475640001", // 转账金额（amount）
				"expense_amount":  "widget17319364514860001", // 支出金额（amount）
				"source":          "widget17303849526100001", // 金额来源（radioV2）
				"destination":     "widget17319364643930001", // 金额去向（radioV2）
				"screenshot":      "widget16487161419060001", // 转账截图（attachmentV2）
				"note":            "widget16487161430270001", // 备注（textarea）
			},
		},
		Options: map[string]map[string]map[string]string{
			RoleInvoiceCollect: {
				"资金来源":     {"个人": "mtzuf8sr-mvm2wd5akv-0", "老师垫付": "mtzuf8sr-46q7o85kxbk-0"},
				"是否为支付宝付款": {"是": "mu0qbtq8-jdbrodratq8-0", "否": "mu0qbtq8-55q2zizlc3k-0"},
			},
			RolePurchase: {
				"项目组": {
					"重装组": "mu7u7m62-myow8r18b5-0", "步兵组": "mu7u7m62-gt80cdnl1bn-0",
					"哨兵组": "mu7u7m62-715zsbh5xew-0", "无人机组": "mu7u7vid-6mpdp0gf78p-1",
					"飞镖组": "mu7u7vid-w5ms6rfn3e-3", "雷达组": "mu7u7vid-mt3xd38m78-5",
				},
				"采购类别": {
					"机械成品件": "l2hj0e3b-x7ckiyvutmb-0", "机械加工件": "l2hj0e3h-so9egdaof-1",
					"电控物资": "l2hj0e3h-81aosv8klcb-3", "其他": "l2hj0e3h-6t5pibskxcq-5",
				},
			},
			// 「27-流水登记」的单选项内部值（创建实例时单选必须传 value，不能传文字）。
			RoleLedgerRegister: {
				"类型": {
					"转入": "m3n27dc0-euksyzgrrz-0", "支出": "m3n27dc0-gpddu5bqtma-0",
				},
				"金额来源": {
					"学校报销": "$i18n-m2xeg1m2-dtjfn4a6ndd-8", "竞赛经费": "m2xek5tw-pi5yii5a0s7-1",
					"众筹资金（个人补贴）": "m2xek5tw-89y3di7otih-3", "大创经费": "m2xek5tw-o62ab6m6vpi-5",
					"指导老师垫付": "m2xek5tw-c78pchsqron-7",
				},
				"金额去向": {
					"指导老师还款": "m3n294p6-ypo3fzid12s-0", "个人垫付还款": "m3n294p6-ujd0s1hqu68-0",
					"物资购买": "m40u8hze-vukqk2r3ftl-1", "差旅垫付": "m3n294p6-19178gxt9m4-0",
				},
			},
		},
		Slots: map[string]string{"invoice": "发票", "order": "订单截图", "payment": "付款记录"},
		Rules: Rules{
			TechGroups:             []string{"硬件组", "机械组", "电控组", "视觉组"},
			SubjectTech:            "技术组物资",
			SubjectProject:         "项目组物资",
			DeptAlias:              map[string]string{"工程组": "重装组", "英雄组": "重装组"},
			ReviewPass:             "通过",
			ReviewFail:             "驳回",
			InvoiceProgressOptions: []string{"待办", "已通过"},
			InvoiceProgressTodo:    "待办",
			InvoiceProgressDone:    "已通过",
		},
		// 部门名 → open_department_id：**刻意留空**。
		//
		// 真实部门 id 属于租户数据，不进公开仓库（源码里写死过一次，2026-09-21 清掉）。
		// 现由 config.yml 的 dept_map 提供；再配合通讯录权限（contact:department.base:readonly）
		// 自动学习，所以这里不需要兜底值。
		DeptMap: map[string]string{},
		Write: WritePolicy{
			OnlyFillBlank: boolPtr(true),
			// 服务自有状态列：允许覆盖（否则 驳回→通过 写不进去）
			ServiceOwned: []string{"人工审核", "已归档", "审核时间", "发票收集进度（待配置）"},
		},
		Triggers: Triggers{
			ArchiveOnApprovalApproved: boolPtr(true),
			AutoApproveWhenClean:      boolPtr(true),
		},
		PurchaseToInvoice: PurchaseToInvoice{
			DraftMode: "rollback_to_start",
			// 「名称（选填，采购提示）」= <项目名称> + 此后缀。
			NameSuffix: "-采购审批",
		},
	}
}

// Field 取某个角色的真实表头。字典来自内置默认 + config.yml 覆盖，正常不会为空。
func (c *Config) Field(role, key string) string {
	if m, ok := c.Dict.Fields[role]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return ""
}

// Control 取某审批控件 id。
func (c *Config) Control(role, key string) string {
	if m, ok := c.Dict.Controls[role]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return ""
}

// OptionValue 取某单选选项的内部 value（创建/更新实例写单选要用 value）。
func (c *Config) OptionValue(role, field, text string) string {
	if m, ok := c.Dict.Options[role]; ok {
		if opts, ok := m[field]; ok {
			if v, ok := opts[text]; ok {
				return v
			}
		}
	}
	return ""
}

// ApprovalByRole 按角色取审批绑定。
func (c *Config) ApprovalByRole(role string) (ApprovalRole, bool) {
	for _, a := range c.Feishu.Approvals {
		if a.Role == role {
			return a, true
		}
	}
	// 兼容旧配置：只有单个 feishu.approval_code
	if role == RoleInvoiceCollect && c.Feishu.ApprovalCode != "" {
		return ApprovalRole{
			Role:       role,
			Code:       c.Feishu.ApprovalCode,
			NameExpect: c.Feishu.ApprovalNameExpect,
		}, true
	}
	return ApprovalRole{}, false
}

// Base 按名字取 base。
func (c *Config) Base(name string) (BaseConfig, bool) {
	if b, ok := c.Feishu.Bitable.Bases[name]; ok && b.AppToken != "" {
		return b, true
	}
	// 兼容旧配置：feishu.bitable.app_token/tables 视为核对 base
	if name == BaseReview && c.Feishu.Bitable.AppToken != "" {
		return BaseConfig{AppToken: c.Feishu.Bitable.AppToken, Tables: c.Feishu.Bitable.Tables}, true
	}
	return BaseConfig{}, false
}

// Table 取 base 下的 table_id。
func (c *Config) Table(base, key string) string {
	b, ok := c.Base(base)
	if !ok {
		return ""
	}
	return b.Tables[key]
}

// OnlyFillBlank 返回是否只填空单元格（默认 true）。
func (c *Config) OnlyFillBlank() bool {
	if c.Dict.Write.OnlyFillBlank == nil {
		return true
	}
	return *c.Dict.Write.OnlyFillBlank
}

// IsServiceOwned 判断某表头是否服务自有（允许覆盖）。
func (c *Config) IsServiceOwned(header string) bool {
	for _, h := range c.Dict.Write.ServiceOwned {
		if h == header {
			return true
		}
	}
	return false
}

// DeptAlias 应用"项目组 → 物资所属部门"的别名。
func (c *Config) DeptAlias(name string) string {
	if v, ok := c.Dict.Rules.DeptAlias[name]; ok && v != "" {
		return v
	}
	return name
}

// ResolveDept 把项目组名解析成 open_department_id（先别名，再查字典）。
func (c *Config) ResolveDept(name string) (openDeptID string, ok bool) {
	n := c.DeptAlias(name)
	if v, ok := c.Dict.DeptMap[n]; ok && v != "" {
		return v, true
	}
	return "", false
}

// SubjectForGroup 按"项目组 → 科目/去向"规则返回科目。
func (c *Config) SubjectForGroup(group string) string {
	for _, g := range c.Dict.Rules.TechGroups {
		if g == group {
			return c.Dict.Rules.SubjectTech
		}
	}
	return c.Dict.Rules.SubjectProject
}
