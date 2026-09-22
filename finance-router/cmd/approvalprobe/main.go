// Command approvalprobe 是 P0 沙箱验证工具：验证"创建审批实例 / 自动同意 / 退回发起人 /
// 部门名解析"四条能力。**部分子命令会写飞书（create/approve/rollback）**，只在明确
// 传入时才执行；默认只读。
//
// 用法（先只读）：
//
//	go run ./cmd/approvalprobe -dept 视觉组
//	go run ./cmd/approvalprobe -depts
//	go run ./cmd/approvalprobe -detail <instance_code>
//
// 写探针（★ 会在飞书里真的建单/同意/退回，用测试审批）：
//
//	go run ./cmd/approvalprobe -create -user <user_id>
//	go run ./cmd/approvalprobe -approve <instance_code>
//	go run ./cmd/approvalprobe -rollback <instance_code>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

func main() {
	cfgPath := flag.String("config", "", "config.yml 路径")
	code := flag.String("code", "", "审批 code（默认取 config.feishu.approval_code）")
	dept := flag.String("dept", "", "只读：按名字查部门（返回 open_department_id）")
	deptID := flag.String("deptid", "", "只读：按部门 id 查（验证 name 字段权限）")
	depts := flag.Bool("depts", false, "只读：列出根部门")
	harvest := flag.Bool("harvest", false, "只读：从历史实例里采集 部门名→open_department_id")
	detail := flag.String("detail", "", "只读：打印实例状态/任务/时间线")
	create := flag.Bool("create", false, "★写：创建一个 27发票收集 测试实例")
	createPurchase := flag.Bool("create-purchase", false,
		"★写：创建一个「采购审批」测试实例（2 条费用明细，走完整采购→流水登记链路）")
	purchaseGroup := flag.String("purchase-group", "视觉组", "-create-purchase 的项目组")
	purchaseAmounts := flag.String("purchase-amounts", "12.5,7", "-create-purchase 的各条明细单价（逗号分隔）")
	purchaseQtys := flag.String("purchase-qtys", "2,1", "-create-purchase 的各条明细数量（逗号分隔）")
	createReg := flag.Bool("create-register", false,
		"★写：创建一张「27-流水登记」测试实例（用于联调 notify→覆盖流水→开票 全链路）")
	regKind := flag.String("reg-kind", "支出", "-create-register 的类型：转入/支出")
	regAmount := flag.String("reg-amount", "0", "-create-register 的金额")
	regNote := flag.String("reg-note", "", "-create-register 的备注（★ 要与流水行备注一致，系统靠它认行）")
	regSource := flag.String("reg-source", "", "-create-register 的金额来源")
	regDest := flag.String("reg-dest", "", "-create-register 的金额去向")
	regShot := flag.String("reg-screenshot", "", "-create-register 的转账截图文件路径（可选）")
	regDate := flag.String("reg-date", "", "-create-register 的转账日期 YYYY-MM-DD（默认今天）")
	regUser := flag.String("reg-user", "", "-create-register 的提交人 user_id（默认 flow_register.user_id）")
	cancelInstance := flag.String("cancel", "", "★写：撤回一条审批实例（清理试探单；只能撤还在审批中的）")
	approve := flag.String("approve", "", "★写：同意该实例的全部 PENDING 任务")
	rollback := flag.String("rollback", "", "★写：把该实例退回到 START（发起人）")
	user := flag.String("user", "", "提交人 user_id（默认 config.feishu.admin_user_id）")
	prefillDept := flag.String("fill-dept", "视觉组", "-create 时预填的物资所属部门名")
	prefillDeptID := flag.String("fill-dept-id", "", "-create 时直接预填部门 open_department_id（验证 department 控件写值格式）")
	uuid := flag.String("uuid", "", "-create 的幂等 uuid（默认自动生成时间戳）")
	flag.Parse()

	p := *cfgPath
	if p == "" {
		f, err := config.FindConfigFile()
		if err != nil {
			die(err)
		}
		p = f
	}
	cfg, err := config.Load(p)
	if err != nil {
		die(err)
	}
	if *code == "" {
		*code = cfg.Feishu.ApprovalCode
	}
	if *user == "" {
		*user = cfg.Feishu.AdminUserID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	switch {
	case *depts:
		items, err := c.ListDepartments(ctx, "0")
		if err != nil {
			die(err)
		}
		for _, d := range items {
			fmt.Printf("  %-28s %s\n", d.Name, d.OpenDepartmentID)
		}
		return
	case *dept != "":
		d, err := c.FindDepartmentByName(ctx, *dept)
		if err != nil {
			die(err)
		}
		fmt.Printf("✓ %s → open_department_id=%s department_id=%s\n", d.Name, d.OpenDepartmentID, d.DepartmentID)
		return
	case *deptID != "":
		info, err := c.GetDepartment(ctx, *deptID)
		if err != nil {
			die(err)
		}
		fmt.Printf("✓ id=%s name=%q hasNameField=%v open=%s\n",
			*deptID, info.Name, info.HasNameField, info.OpenDeptID)
		return
	case *harvest:
		runHarvest(ctx, c, *code)
		return
	case *detail != "":
		det, _, err := c.GetInstanceDetail(ctx, *detail)
		if err != nil {
			die(err)
		}
		printDetail(det)
		return
	case *create:
		runCreate(ctx, c, *code, *user, *prefillDept, *prefillDeptID, *uuid)
		return
	case *createPurchase:
		runCreatePurchase(ctx, cfg, c, *user, *purchaseGroup, *purchaseAmounts, *purchaseQtys, *uuid)
		return
	case *createReg:
		u := *regUser
		if u == "" {
			u = cfg.RegisterUserID()
		}
		runCreateRegister(ctx, cfg, c, u, *regKind, *regAmount, *regNote,
			*regSource, *regDest, *regShot, *regDate, *uuid)
		return
	case *cancelInstance != "":
		if err := c.CancelInstance(ctx, *code, *cancelInstance, *user); err != nil {
			die(err)
		}
		fmt.Printf("✓ 已撤回实例 %s（user_id=%s）\n", *cancelInstance, *user)
		return
	case *approve != "":
		runApprove(ctx, c, *approve)
		return
	case *rollback != "":
		runRollback(ctx, c, *rollback)
		return
	}
	fmt.Println("没给动作。用 -dept/-depts/-detail 只读，或 -create/-approve/-rollback 写探针。")
}

func printDetail(det *feishu.InstanceDetail) {
	fmt.Printf("实例 %s  名称=%s  状态=%s  提交人=%s  部门=%s\n",
		det.InstanceCode, det.ApprovalName, det.Status, det.UserID, det.DepartmentID)
	fmt.Println("  task_list:")
	for i, t := range det.TaskList {
		fmt.Printf("    [%d] %-9s node=%s(%s) user=%s id=%s\n",
			i+1, t.Status, t.NodeName, t.NodeID, t.UserID, t.ID)
	}
	fmt.Println("  timeline:")
	for i, t := range det.Timeline {
		fmt.Printf("    [%d] type=%-6s node_key=%-24s user=%s task=%s\n",
			i+1, t.Type, t.NodeKey, t.UserID, t.TaskID)
	}
}

func runCreate(ctx context.Context, c *feishu.Client, code, user, deptName, deptID, uuid string) {
	form := []map[string]any{}
	switch {
	case deptID != "":
		fmt.Printf("  预填部门（直接给 open_department_id） %s\n", deptID)
		form = append(form, map[string]any{
			"id": "widget17893053389170001", "type": "department",
			"value": []map[string]any{{"open_id": deptID}},
		})
	case deptName != "":
		d, err := c.FindDepartmentByName(ctx, deptName)
		if err != nil {
			fmt.Printf("  ⚠ 部门 %q 解析失败（跳过预填）: %v\n", deptName, err)
		} else {
			fmt.Printf("  预填部门 %s → %s\n", d.Name, d.OpenDepartmentID)
			form = append(form, map[string]any{
				"id": "widget17893053389170001", "type": "department",
				"value": []map[string]any{{"open_id": d.OpenDepartmentID}},
			})
		}
	}
	if user != "" {
		form = append(form, map[string]any{
			"id": "widget17893729431580001", "type": "contact", "value": []string{user},
		})
	}
	if uuid == "" {
		uuid = fmt.Sprintf("probe-%d", time.Now().Unix())
	}
	fmt.Printf("★ 创建实例 approval=%s 提交人=%s uuid=%s 控件=%d\n", code, user, uuid, len(form))
	ic, err := c.CreateInstance(ctx, feishu.CreateInstanceRequest{
		ApprovalCode:  code,
		UserID:        user,
		Form:          form,
		UUID:          uuid,
		AllowResubmit: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ 创建失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  ✓ instance_code=%s\n", ic)
	det, _, err := c.GetInstanceDetail(ctx, ic)
	if err == nil {
		printDetail(det)
	}
}

// runCreatePurchase 建一条**采购审批**测试实例（P0/链路联调用）。
//
// 为什么需要：新流程（采购 → 流水登记 → 开票）要端到端验证，而这条链路的入口
// 只能是"一条采购审批通过"。用测试审批表单（`采购审批 - 27Test`）建单即为此用途。
// ★ 会真的写飞书：建实例 + （可再 -approve）同意，并触发下游流水登记/开票。
func runCreatePurchase(ctx context.Context, cfg *config.Config, c *feishu.Client,
	user, group, amounts, qtys, uuid string) {

	appr, ok := cfg.ApprovalByRole(config.RolePurchase)
	if !ok || appr.Code == "" {
		die(fmt.Errorf("config 里没有 role=purchase 的审批"))
	}
	if uuid == "" {
		uuid = "probe-purchase-" + time.Now().Format("20060102T150405")
	}
	amts := splitFloats(amounts)
	qs := splitFloats(qtys)
	nameID := cfg.Control(config.RolePurchase, "detail_name")
	amtID := cfg.Control(config.RolePurchase, "detail_amount")
	qtyID := cfg.Control(config.RolePurchase, "detail_qty")

	var rows [][]map[string]any
	names := []string{"流程测试-明细A", "流程测试-明细B", "流程测试-明细C"}
	for i, a := range amts {
		q := 1.0
		if i < len(qs) {
			q = qs[i]
		}
		rows = append(rows, []map[string]any{
			{"id": nameID, "type": "input", "value": names[i%len(names)]},
			{"id": amtID, "type": "amount", "value": a, "currency": "CNY"},
			{"id": qtyID, "type": "number", "value": q},
		})
	}
	form := []map[string]any{
		{"id": cfg.Control(config.RolePurchase, "group"), "type": "radioV2",
			"value": cfg.OptionValue(config.RolePurchase, "项目组", group)},
		{"id": cfg.Control(config.RolePurchase, "category"), "type": "radioV2",
			"value": cfg.OptionValue(config.RolePurchase, "采购类别", "其他")},
		{"id": cfg.Control(config.RolePurchase, "project_name"), "type": "input",
			"value": "流水登记流程测试"},
		{"id": cfg.Control(config.RolePurchase, "detail"), "type": "fieldList", "value": rows},
	}
	newCode, err := c.CreateInstance(ctx, feishu.CreateInstanceRequest{
		ApprovalCode: appr.Code, UserID: user, Form: form, UUID: uuid, AllowResubmit: true,
	})
	if err != nil {
		die(err)
	}
	fmt.Printf("✓ 已创建采购审批测试实例 %s（提交人=%s，项目组=%s，%d 条明细）\n",
		newCode, user, group, len(amts))
	fmt.Printf("  下一步：go run ./cmd/approvalprobe -approve %s\n", newCode)
}

// runCreateRegister 建一张「27-流水登记」测试实例。
//
// 用途：这个审批的节点是「自动通过」，用 API 建单即 APPROVED —— 正好可以拿来
// 端到端验证"登记单通过 → 覆盖流水行 → 全部登记完开票"（见 docs/33 §12.15）。
// 备注必须与流水行的备注逐字一致，系统靠它把登记数据匹配回那一行。
func runCreateRegister(ctx context.Context, cfg *config.Config, c *feishu.Client,
	user, kind, amount, note, source, dest, shot, date, uuid string) {

	appr, ok := cfg.ApprovalByRole(config.RoleLedgerRegister)
	if !ok || appr.Code == "" {
		die(fmt.Errorf("config 里没有 role=ledger_register 的审批"))
	}
	if uuid == "" {
		uuid = "probe-register-" + time.Now().Format("20060102T150405")
	}
	ctrl := func(k string) string { return cfg.Control(config.RoleLedgerRegister, k) }
	amt, _ := strconv.ParseFloat(strings.TrimSpace(amount), 64)
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	form := []map[string]any{}
	if v := cfg.OptionValue(config.RoleLedgerRegister, "类型", kind); v != "" {
		form = append(form, map[string]any{"id": ctrl("kind"), "type": "radioV2", "value": v})
	}
	if kind == "转入" {
		form = append(form, map[string]any{"id": ctrl("transfer_amount"), "type": "amount",
			"value": amt, "currency": "CNY"})
	} else {
		form = append(form, map[string]any{"id": ctrl("expense_amount"), "type": "amount",
			"value": amt, "currency": "CNY"})
	}
	if t, err := time.ParseInLocation("2006-01-02", date, time.Local); err == nil {
		form = append(form, map[string]any{"id": ctrl("date"), "type": "date", "value": t.Format(time.RFC3339)})
	}
	if note != "" {
		form = append(form, map[string]any{"id": ctrl("note"), "type": "textarea", "value": note})
	}
	if v := cfg.OptionValue(config.RoleLedgerRegister, "金额来源", source); source != "" && v != "" {
		form = append(form, map[string]any{"id": ctrl("source"), "type": "radioV2", "value": v})
	}
	if v := cfg.OptionValue(config.RoleLedgerRegister, "金额去向", dest); dest != "" && v != "" {
		form = append(form, map[string]any{"id": ctrl("destination"), "type": "radioV2", "value": v})
	}
	if shot != "" {
		data, err := os.ReadFile(shot)
		if err != nil {
			die(fmt.Errorf("读截图失败: %w", err))
		}
		name := filepath.Base(shot)
		code, err := c.UploadApprovalFile(ctx, name, "attachment", data)
		if err != nil {
			die(fmt.Errorf("上传截图到审批失败: %w", err))
		}
		fmt.Printf("  截图已上传审批：%s（%s）\n", name, code)
		form = append(form, map[string]any{"id": ctrl("screenshot"), "type": "attachmentV2",
			"value": []string{code}})
	}
	newCode, err := c.CreateInstance(ctx, feishu.CreateInstanceRequest{
		ApprovalCode: appr.Code, UserID: user, Form: form, UUID: uuid, AllowResubmit: true,
	})
	if err != nil {
		die(err)
	}
	fmt.Printf("✓ 已创建流水登记实例 %s（提交人=%s 类型=%s 金额=%s）\n", newCode, user, kind, amount)
}

// splitFloats 解析 "12.5,7" 这样的数字列表。
func splitFloats(s string) []float64 {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if v, err := strconv.ParseFloat(part, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func runApprove(ctx context.Context, c *feishu.Client, instanceCode string) {
	det, _, err := c.GetInstanceDetail(ctx, instanceCode)
	if err != nil {
		die(err)
	}
	n := 0
	for _, t := range det.TaskList {
		if !strings.EqualFold(t.Status, "PENDING") {
			continue
		}
		fmt.Printf("★ 同意任务 node=%s user=%s task=%s\n", t.NodeName, t.UserID, t.ID)
		if err := c.ApproveTask(ctx, det.ApprovalCode, instanceCode, t.UserID, t.ID,
			"自动通过（校验一致，无需人工）"); err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ 同意失败: %v\n", err)
			os.Exit(1)
		}
		n++
	}
	fmt.Printf("  ✓ 已同意 %d 个待办任务\n", n)
	det2, _, _ := c.GetInstanceDetail(ctx, instanceCode)
	if det2 != nil {
		fmt.Printf("  实例状态现在是: %s\n", det2.Status)
	}
}

func runRollback(ctx context.Context, c *feishu.Client, instanceCode string) {
	det, _, err := c.GetInstanceDetail(ctx, instanceCode)
	if err != nil {
		die(err)
	}
	var task *feishu.TaskItem
	for i := range det.TaskList {
		if strings.EqualFold(det.TaskList[i].Status, "PENDING") {
			task = &det.TaskList[i]
			break
		}
	}
	if task == nil {
		fmt.Fprintln(os.Stderr, "✗ 该实例当前没有 PENDING 任务，无法退回")
		os.Exit(1)
	}
	fmt.Printf("★ 退回实例 %s：用任务 %s（审批人 %s）退回到 START\n", instanceCode, task.ID, task.UserID)
	if err := c.SpecifiedRollback(ctx, task.UserID, task.ID, []string{"START"},
		"请补充发票/订单/付款截图"); err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ 退回失败: %v\n", err)
		os.Exit(1)
	}
	det2, _, _ := c.GetInstanceDetail(ctx, instanceCode)
	if det2 != nil {
		fmt.Printf("  ✓ 退回成功。实例状态=%s reverted=%v\n", det2.Status, det2.Reverted)
		printDetail(det2)
	}
}

func runHarvest(ctx context.Context, c *feishu.Client, code string) {
	items, err := c.QueryInstances(ctx, feishu.InstanceQueryRequest{
		ApprovalCode: code,
		From:         time.Now().AddDate(0, 0, -30),
		To:           time.Now(),
		UserIDType:   "user_id",
	})
	if err != nil {
		die(err)
	}
	seen := map[string]string{}
	for _, it := range items {
		det, _, err := c.GetInstanceDetail(ctx, it.Instance.Code)
		if err != nil {
			continue
		}
		ws, _ := feishu.ParseForm(det.Form)
		for _, w := range ws {
			if w.Type != "department" || len(w.Value) == 0 {
				continue
			}
			var arr []struct {
				Name   string `json:"name"`
				OpenID string `json:"open_id"`
			}
			if json.Unmarshal(w.Value, &arr) != nil {
				continue
			}
			for _, d := range arr {
				if d.Name != "" && d.OpenID != "" {
					seen[d.Name] = d.OpenID
				}
			}
		}
	}
	var names []string
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("采集 %d 个实例，得到 %d 组 部门名→open_department_id：\n", len(items), len(names))
	for _, n := range names {
		fmt.Printf("  %-12s %s\n", n, seen[n])
	}
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "✗ %v\n", err)
	os.Exit(1)
}
