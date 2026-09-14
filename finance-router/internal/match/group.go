package match

import (
	"fmt"
	"sort"

	"github.com/coffee/finance-router/internal/ocr"
)

// Doc 是参与分组的一份单据。比 Item 多了"配对键"与"号"。
type Doc struct {
	Slot      string
	Kind      ocr.Kind
	Amount    *int64
	Date      string
	Party     string
	InvoiceNo string // 发票号码（防重复的键）
	OrderNo   string // 商家订单号 / 发票备注里的订单号
	AlipayTxn string // 支付宝交易号（辅助）
	Keys      KeySet // 长数字段集合 —— 配对主依据

	// Ref 是调用方给的**回指**（本项目里是文件在 manifest 内的序号）。
	// 分组本身不需要它；下游要把"这一组的图挂到哪一行"必须靠它 ——
	// 一单多票时槽位名全都一样，光凭槽位无法区分。
	Ref int
}

// Group 是分组结果：一张发票 + 它的订单 + 这些订单的付款。
//
// 需求定的是「一张发票一行」，所以**每组恒好有一张发票**
// （1 发票 : N 订单 的情形就是一个组里挂多个订单）。
type Group struct {
	Invoice  Doc
	Orders   []Doc
	Payments []Doc

	// Reasons 记录"凭什么配上的"，用于人工复核时解释，也用于回归排查。
	Reasons []string

	// InvoiceTotal / SupportTotal 是两侧金额（分）。相等才叫配上。
	InvoiceTotal int64
	SupportTotal int64

	// InvoiceKnown / SupportKnown 表示**是否真的读到了金额**。
	//
	// 为什么不能只看两个数：读不到金额时两者都是 0，"0 == 0" 会被判成"对上"，
	// 于是一单完全没读出金额的报销会被标成「一致」并自动通过。
	// 在财务系统里这是最危险的一类错误 —— 没数据不等于没问题。
	InvoiceKnown bool
	SupportKnown bool

	// Mismatch 表示组内的订单与付款**彼此**金额不一致。
	// 这时哪怕"发票恰好等于其中一份"，也不能算对上 —— 有一份是错的。
	Mismatch bool
}

// Matched 表示这个组的金额是否对得上。金额缺失一律算"对不上"（交人工）。
func (g Group) Matched(toleranceCent int64) bool {
	if !g.InvoiceKnown || !g.SupportKnown || g.Mismatch {
		return false
	}
	d := g.InvoiceTotal - g.SupportTotal
	if d < 0 {
		d = -d
	}
	return d <= toleranceCent
}

// Grouping 是一次分组的全量结果。
type Grouping struct {
	Groups   []Group
	Leftover []Doc // 没能进任何一组的单据

	// FormProblem 非空表示**提交形态本身不合规**（违反成员必须遵守的规定）。
	// 与"配不上"是两回事：形态不合规要直接打回，不需要再尝试匹配。
	FormProblem string
}

// Form 是提交的形态，决定允许哪些组合。取值来自表单的「是否为支付宝付款」。
type Form string

const (
	FormAlipay    Form = "alipay"     // 支付宝：一发票一订单可多张；一发票多订单只允许一张发票
	FormNonAlipay Form = "non_alipay" // 非支付宝：只允许 1 发票 1 订单 1 付款
)

// 需求定的强制规则（见表单第 3 项的括号说明）：
//
//	支付宝 + 一发票对一订单  → 可上传多张发票、订单、付款
//	支付宝 + 一发票对多订单  → **只允许 1 张发票**
//	非支付宝                  → 只允许 1 发票 1 订单 1 付款
const (
	CodeFormTooManyForNonAlipay   = "FORM_NON_ALIPAY_ONE_EACH"
	CodeFormMultiInvoiceOneToMany = "FORM_ALIPAY_MULTI_INVOICE_WITH_MULTI_ORDER"
	CodeNoInvoice                 = "NO_INVOICE"
	CodeInvoiceUnmatched          = "INVOICE_UNMATCHED"
	CodeOrderUnmatched            = "ORDER_UNMATCHED"
	CodePaymentUnmatched          = "PAYMENT_UNMATCHED"
	CodeAmountMismatch            = "AMOUNT_MISMATCH"
	CodeSplitNotSelfConsistent    = "SPLIT_NOT_SELF_CONSISTENT"
)

// Build 按规则把单据分组。
//
// 顺序很关键：
//  1. 先配「订单 ↔ 付款」—— 用共有长数字段（商家订单号），这是确定性最强的键；
//  2. 再配「发票 ↔ 订单」—— 优先用发票备注里的订单号（也是确定性的），
//     读不到才退回按金额；
//  3. 汇总成组；配不上的进 Leftover，交给人工纠错。
func Build(docs []Doc, form Form, toleranceCent int64) *Grouping {
	g := &Grouping{}

	var invoices, orders, payments []Doc
	for _, d := range docs {
		switch d.Kind {
		case ocr.KindInvoice:
			invoices = append(invoices, d)
		case ocr.KindOrder:
			orders = append(orders, d)
		case ocr.KindPayment:
			payments = append(payments, d)
		}
	}

	// ⓪ 需求明确定死的一条：**1 发票 + 1 订单 + 1 付款 直接绑定**。
	//
	// 用户原话："一张发票、一张订单、一张付款的这种直接绑定，如果金额对不上就交给人工。"
	// 这是非支付宝的强制形态，也是支付宝里最常见的一票一单。
	//
	// 为什么不能只靠配对键：实测很多订单/付款截图根本读不到商家订单号
	// （老表单、或识别失败），只靠键会让这些本该直接绑定的单全进人工。
	// 一票一单时**不存在歧义**，唯一的组合就是正确答案。
	if len(invoices) == 1 && len(orders) == 1 && len(payments) == 1 {
		g.Groups = []Group{directBind(invoices[0], orders[0], payments[0], toleranceCent)}
		g.FormProblem = checkForm(form, invoices, g.Groups)
		return g
	}

	// ① 订单 ↔ 付款：共有长数字段
	orderToPay := map[int][]Doc{}
	usedPay := map[int]bool{}
	for oi, o := range orders {
		for pi, p := range payments {
			if usedPay[pi] {
				continue
			}
			if k := o.Keys.SharedKey(p.Keys); k != "" {
				orderToPay[oi] = append(orderToPay[oi], p)
				usedPay[pi] = true
			}
		}
	}

	// ② 发票 ↔ 订单
	usedOrder := map[int]bool{}
	for _, inv := range invoices {
		grp := Group{Invoice: inv}
		if inv.Amount != nil {
			grp.InvoiceTotal = *inv.Amount
			grp.InvoiceKnown = true
		}

		// 2a. 先用发票备注里的订单号精确命中 —— 这比金额可靠得多
		var orderIdx []int
		for oi, o := range orders {
			if usedOrder[oi] || o.OrderNo == "" {
				continue
			}
			if inv.Keys.Contains(o.OrderNo) {
				orderIdx = append(orderIdx, oi)
				grp.Reasons = append(grp.Reasons,
					fmt.Sprintf("发票备注里的订单号 %s 命中 %s", o.OrderNo, o.Slot))
			}
		}

		// 2b. 没命中就按金额找；支持"一张发票对多个订单"的求和
		if len(orderIdx) == 0 && inv.Amount != nil {
			if picked, how := pickByAmount(*inv.Amount, orders, usedOrder, toleranceCent); len(picked) > 0 {
				orderIdx = picked
				grp.Reasons = append(grp.Reasons, how)
			}
		}
		for _, oi := range orderIdx {
			usedOrder[oi] = true
		}

		// 2c. 挂上这些订单，以及已配到它们的付款
		var total int64
		supportKnown := false
		for _, oi := range orderIdx {
			grp.Orders = append(grp.Orders, orders[oi])
			gotPay := false
			for _, p := range orderToPay[oi] {
				grp.Payments = append(grp.Payments, p)
				if p.Amount != nil {
					total += *p.Amount
					supportKnown = true
				}
				gotPay = true
			}
			// 该订单没有配上付款时，用订单金额兜底（例如"老师垫付"根本没有付款记录）
			if !gotPay && orders[oi].Amount != nil {
				total += *orders[oi].Amount
				supportKnown = true
			}
		}
		grp.SupportTotal = total
		grp.SupportKnown = supportKnown
		g.Groups = append(g.Groups, grp)
	}

	// ③ 剩余
	for oi, o := range orders {
		if !usedOrder[oi] {
			g.Leftover = append(g.Leftover, o)
		}
	}
	for pi, p := range payments {
		if !usedPay[pi] {
			g.Leftover = append(g.Leftover, p)
		}
	}

	g.FormProblem = checkForm(form, invoices, g.Groups)
	return g
}

// directBind 处理"1 发票 + 1 订单 + 1 付款"：直接绑成一组。
//
// 这个形态下**不存在歧义** —— 唯一的组合就是正确答案，所以不需要配对键。
// 但仍然记录"是否读到了共同单号"，作为人工复核时的旁证。
func directBind(inv, ord, pay Doc, tol int64) Group {
	g := Group{Invoice: inv, Orders: []Doc{ord}, Payments: []Doc{pay}}
	if inv.Amount != nil {
		g.InvoiceTotal, g.InvoiceKnown = *inv.Amount, true
	}
	// 支撑金额优先取**订单**（那是货款本身）；没有订单金额才退回付款金额。
	switch {
	case ord.Amount != nil:
		g.SupportTotal, g.SupportKnown = *ord.Amount, true
	case pay.Amount != nil:
		g.SupportTotal, g.SupportKnown = *pay.Amount, true
	}
	// 订单与付款都有金额时，它们必须一致 —— 否则有一份是错的，
	// 不能因为"发票恰好等于其中一份"就放行。
	if ord.Amount != nil && pay.Amount != nil {
		if d := *ord.Amount - *pay.Amount; d > tol || d < -tol {
			g.Mismatch = true
		}
	}
	switch {
	case inv.Keys.SharedKey(ord.Keys) != "":
		g.Reasons = append(g.Reasons, fmt.Sprintf("发票与订单共有单号 %s（1 发票 1 订单 1 付款）",
			inv.Keys.SharedKey(ord.Keys)))
	case ord.Keys.SharedKey(pay.Keys) != "":
		g.Reasons = append(g.Reasons, fmt.Sprintf("订单与付款共有单号 %s（1 发票 1 订单 1 付款）",
			ord.Keys.SharedKey(pay.Keys)))
	default:
		g.Reasons = append(g.Reasons, "1 发票 1 订单 1 付款，按提交形态直接绑定（无需配对键）")
	}
	return g
}

// pickByAmount 为一张发票挑订单：先试"单个订单金额相等"，再试"若干订单求和相等"。
//
// 求和是为了"一张发票对多个订单"（合并开票）。订单数通常个位数，
// 枚举子集完全可行。
func pickByAmount(target int64, orders []Doc, used map[int]bool, tol int64) ([]int, string) {
	var free []int
	for i, o := range orders {
		if used[i] || o.Amount == nil {
			continue
		}
		free = append(free, i)
	}
	// 单个
	for _, i := range free {
		if d := abs64(target - *orders[i].Amount); d <= tol {
			return []int{i}, fmt.Sprintf("金额相等（%s）", Yuan(*orders[i].Amount))
		}
	}
	// 子集求和（最多 12 个，2^12=4096）
	if len(free) > 12 {
		free = free[:12]
	}
	best := []int{}
	for mask := 1; mask < (1 << len(free)); mask++ {
		var sum int64
		var picked []int
		for b, i := range free {
			if mask&(1<<b) != 0 {
				sum += *orders[i].Amount
				picked = append(picked, i)
			}
		}
		if abs64(target-sum) <= tol {
			// 取订单数最少的解：更可能是真的合并开票，而不是碰巧凑上
			if len(best) == 0 || len(picked) < len(best) {
				best = picked
			}
		}
	}
	if len(best) > 0 {
		sort.Ints(best)
		return best, fmt.Sprintf("%d 个订单金额之和等于发票金额（%s）", len(best), Yuan(target))
	}
	return nil, ""
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// checkForm 校验提交形态是否违反强制规则。返回空串表示合规。
func checkForm(form Form, invoices []Doc, groups []Group) string {
	switch form {
	case FormNonAlipay:
		// 非支付宝：只允许 1 发票 1 订单 1 付款
		if len(invoices) > 1 {
			return fmt.Sprintf("%s：非支付宝只允许 1 张发票，实际 %d 张",
				CodeFormTooManyForNonAlipay, len(invoices))
		}
		for _, g := range groups {
			if len(g.Orders) > 1 {
				return fmt.Sprintf("%s：非支付宝只允许 1 个订单，实际组内 %d 个",
					CodeFormTooManyForNonAlipay, len(g.Orders))
			}
			if len(g.Payments) > 1 {
				return fmt.Sprintf("%s：非支付宝只允许 1 条付款，实际组内 %d 条",
					CodeFormTooManyForNonAlipay, len(g.Payments))
			}
		}
	case FormAlipay:
		// 支付宝：出现"一发票多订单"时，只允许存在 1 张发票
		hasMulti := false
		for _, g := range groups {
			if len(g.Orders) > 1 {
				hasMulti = true
			}
		}
		if hasMulti && len(invoices) > 1 {
			return fmt.Sprintf("%s：一发票对多订单时只允许 1 张发票，实际 %d 张",
				CodeFormMultiInvoiceOneToMany, len(invoices))
		}
	}
	return ""
}

// Yuan 把"分"格式化成元（供提示文案用）。
func Yuan(cent int64) string {
	neg := ""
	if cent < 0 {
		neg, cent = "-", -cent
	}
	return fmt.Sprintf("%s%d.%02d", neg, cent/100, cent%100)
}
