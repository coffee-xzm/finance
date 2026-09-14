package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/match"
	"github.com/coffee/finance-router/internal/ocr"
	"github.com/coffee/finance-router/internal/store"
)

// Regroup 用库里**已有的证据**重算分组，不重新下载、不重新识别。
//
// 什么时候需要：
//   - 分组规则改过（例如新增"1 发票 1 订单 1 付款 直接绑定"）；
//   - 早期实例是在 doc_group 表存在之前处理的，压根没有分组记录 ——
//     没有分组，落表时只能给出"存疑"，会把人已经核对过的单重新变成待办。
//
// 为什么能从证据重算：配对需要的一切（金额/日期/发票号码/订单号/支付宝交易号）
// 都已经存在 evidence 表里，重算不需要再碰飞书。
//
// force=false 时只补"完全没有分组"的实例，不动已有分组。
func Regroup(cfgPath string, force, dryRun bool) error {
	if cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			return err
		}
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	insts, err := db.SyncInstances(ctx, 0)
	if err != nil {
		return err
	}

	tol := cfg.Matching.AmountToleranceCent
	if tol <= 0 {
		tol = 1
	}

	var done, skipped int
	for _, in := range insts {
		code := in.Sub.InstanceCode
		if len(in.Groups) > 0 && !force {
			skipped++
			continue
		}
		docs := docsFromEvidence(in.Evidence)
		g := match.Build(docs, formOfSub(in.Sub), tol)
		groups := storeGroups(g)
		var leftover []string
		for _, d := range g.Leftover {
			leftover = append(leftover, store.EvRef(d.Slot, d.Ref))
		}
		fmt.Printf("  %s → %d 组，%d 份未配上%s\n",
			short(code), len(groups), len(leftover), formProblemSuffix(g.FormProblem))
		if dryRun {
			done++
			continue
		}
		if err := db.ReplaceGroups(ctx, code, groups, g.FormProblem, leftover); err != nil {
			return fmt.Errorf("回写 %s 失败: %w", short(code), err)
		}
		done++
	}
	fmt.Printf("\n完成：重算 %d 个实例，跳过 %d 个（已有分组）\n", done, skipped)
	return nil
}

func formProblemSuffix(s string) string {
	if s == "" {
		return ""
	}
	return "；形态不合规：" + s
}

// formOfSub 从提交记录判断表单形态。
func formOfSub(s store.Submission) match.Form {
	switch strings.ToLower(strings.TrimSpace(s.IsAlipay)) {
	case "是", "true", "1", "yes":
		return match.FormAlipay
	}
	return match.FormNonAlipay
}

// docsFromEvidence 把库里存的证据还原成分组用的单据。
//
// 两件必须处理的事：
//  1. **同一 (槽位,序号) 可能有多行** —— 历史原因（早期抽取一次、reindex 又一次）
//     会在库里留下重复行，其中一行有金额没图、另一行有图没金额。
//     必须合并成一行再分组，否则分组用了 A 行、落表挂图用了 B 行。
//  2. 金额要**归一化符号**（付款/订单截图上的金额常是负数）。
func docsFromEvidence(evs []store.Evidence) []match.Doc {
	byKey := map[string]*store.Evidence{}
	var order []string
	for i := range evs {
		e := evs[i]
		key := store.EvRef(e.Slot, e.IndexNo)
		if prev, ok := byKey[key]; ok {
			mergeEvidence(prev, e)
			continue
		}
		cp := e
		byKey[key] = &cp
		order = append(order, key)
	}
	sort.Strings(order)

	var out []match.Doc
	for _, k := range order {
		e := *byKey[k]
		kind := ocr.Kind(e.Kind)
		if kind == ocr.KindUnknown || kind == "" {
			continue
		}
		out = append(out, match.Doc{
			Slot:      e.Slot,
			Kind:      kind,
			Amount:    match.NormalizeAmount(e.AmountInclTaxCent, kind),
			Date:      e.Date,
			Party:     e.Counterparty,
			InvoiceNo: e.InvoiceNo,
			OrderNo:   e.OrderNo,
			AlipayTxn: e.AlipayTxnID,
			Keys:      match.KeysOf(e.OrderNo, e.AlipayTxnID, e.InvoiceNo),
			Ref:       e.IndexNo,
		})
	}
	return out
}

// mergeEvidence 把 src 里"有值"的字段补进 dst（dst 缺失才补）。
func mergeEvidence(dst *store.Evidence, src store.Evidence) {
	if dst.AmountInclTaxCent == nil {
		dst.AmountInclTaxCent = src.AmountInclTaxCent
	}
	if dst.TaxCent == nil {
		dst.TaxCent = src.TaxCent
	}
	if dst.AmountExclTaxCent == nil {
		dst.AmountExclTaxCent = src.AmountExclTaxCent
	}
	if dst.Date == "" {
		dst.Date = src.Date
	}
	if dst.Counterparty == "" {
		dst.Counterparty = src.Counterparty
	}
	if dst.InvoiceNo == "" {
		dst.InvoiceNo = src.InvoiceNo
	}
	if dst.InvoiceNoSrc == "" {
		dst.InvoiceNoSrc = src.InvoiceNoSrc
	}
	if dst.OrderNo == "" {
		dst.OrderNo = src.OrderNo
	}
	if dst.AlipayTxnID == "" {
		dst.AlipayTxnID = src.AlipayTxnID
	}
	if len(dst.LocalPNGs) == 0 {
		dst.LocalPNGs = src.LocalPNGs
	}
	if dst.LocalPNG == "" {
		dst.LocalPNG = src.LocalPNG
	}
	if dst.UpperCheck == "" {
		dst.UpperCheck = src.UpperCheck
	}
	if dst.TaxCheck == "" {
		dst.TaxCheck = src.TaxCheck
	}
	if dst.Filename == "" {
		dst.Filename = src.Filename
	}
	if dst.MediaType == "" {
		dst.MediaType = src.MediaType
	}
}

// RefreshMeta 从审批单重新取一次**表单元信息**（归属组/是否支付宝/资金来源/…），
// 只更新 submission 里的元信息列，**不碰证据、不碰分组**。
//
// 什么时候需要：早期实例入库时这些列还不存在（或还没写），表里那几列是空的。
// 重新跑 extract 会把附件再下载一遍（URL 也可能已过期），代价大得多 ——
// 而这些字段全都在审批实例详情里，一次 API 调用就能取到。
func RefreshMeta(cfgPath string, dryRun bool) error {
	if cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			return err
		}
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	insts, err := db.SyncInstances(ctx, 0)
	if err != nil {
		return err
	}
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	deptCache, _ := db.AllDepts(ctx)

	var done, failed int
	for _, in := range insts {
		s := in.Sub
		detail, _, err := client.GetInstanceDetail(ctx, s.InstanceCode)
		if err != nil {
			fmt.Printf("  ✗ %s 取详情失败: %v\n", short(s.InstanceCode), err)
			failed++
			continue
		}
		widgets, err := feishu.ParseForm(detail.Form)
		if err != nil {
			fmt.Printf("  ✗ %s 解析表单失败: %v\n", short(s.InstanceCode), err)
			failed++
			continue
		}
		m := buildMeta(cfg, s.InstanceCode, detail, widgets)
		// 购买人是 contact 控件：拿到的是用户 ID 时要换成姓名。
		resolveBuyerName(ctx, client, m)
		// 发起人部门：与 extract 同样的两步兜底（通讯录 → 本地对照表）。
		// 都拿不到名字就**留空** —— 不要把 open_department_id / 部门 ID
		// 写进「发起人部门」那一列，那是给人看的，一串哈希只会误导。
		if detail.DepartmentID != "" {
			info, derr := client.GetDepartment(ctx, detail.DepartmentID)
			switch {
			case derr != nil:
				if name, ok := deptCache[detail.DepartmentID]; ok {
					m.ApplicantDept = name
				} else {
					fmt.Printf("      ⚠ 部门 %s 名称未知（缺部门字段权限，对照表也没有），留空\n",
						detail.DepartmentID)
				}
			case info.Name != "":
				m.ApplicantDept = info.Name
				m.ApplicantDeptID = info.OpenDeptID
				// 学到就记下来：同一个部门下一个实例未必还能查到名字
				// （实测同一部门时有时无），缓存住才是稳的。
				if info.OpenDeptID != "" {
					_ = db.LearnDept(ctx, info.OpenDeptID, info.Name)
					deptCache[info.OpenDeptID] = info.Name
				}
			default:
				m.ApplicantDeptID = info.OpenDeptID
				if name, ok := deptCache[info.OpenDeptID]; ok {
					m.ApplicantDept = name
				} else {
					fmt.Printf("      ⚠ 部门 %s（%s）名称未知，留空\n", detail.DepartmentID, info.OpenDeptID)
				}
			}
		}
		fmt.Printf("  %s 归属组=%v 支付宝=%q 资金来源=%q 大创=%q 购买人=%q 发起人部门=%q\n",
			short(s.InstanceCode), m.Departments, m.IsAlipay, m.FundSource, m.Dachuang,
			m.Buyer, m.ApplicantDept)
		if dryRun {
			done++
			continue
		}
		s.ApprovalName, s.Status, s.Applicant = m.ApprovalName, m.Status, m.Applicant
		s.ApplicantDept, s.ApplicantDeptID = m.ApplicantDept, m.ApplicantDeptID
		s.MaterialType, s.MaterialName, s.Buyer = m.MaterialType, m.MaterialName, m.Buyer
		s.FundSource, s.IsAlipay, s.Dachuang = m.FundSource, m.IsAlipay, m.Dachuang
		s.Remark, s.Applink, s.Departments = m.Remark, m.Applink, m.Departments
		if m.StartTime != "" {
			if ms, perr := strconv.ParseInt(m.StartTime, 10, 64); perr == nil {
				s.StartTimeMS = &ms
			}
		}
		if err := db.UpdateSubmissionMeta(ctx, s); err != nil {
			fmt.Printf("  ✗ %s 回写失败: %v\n", short(s.InstanceCode), err)
			failed++
			continue
		}
		done++
	}
	fmt.Printf("\n完成：刷新 %d 个，失败 %d 个\n", done, failed)
	return nil
}

// PurgeAll 全清：本地库的实例 + 「报销核对」与「报销整合」里的全部行。
//
// 只用于"测试数据整体作废、干干净净重来"。它会**释放**所有 sha256 与发票号码 ——
// 不清掉的话，测试单占着这些键，真实发票再提交会被误判成重复报销。
//
// 三件容易漏掉的事，这里都做了：
//  1. 先备份本地库（VACUUM INTO），后悔了还能捞回来；
//  2. 把 data/extract/files 挪走 —— 不挪的话下次 -reindex 会把证据全装回来；
//  3. 表里的行也删 —— 本地清了表没清，人看到的就是一堆对不上号的孤儿行。
func PurgeAll(cfgPath string, dryRun bool) error {
	if cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			return err
		}
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	appToken := cfg.Feishu.Bitable.AppToken
	if appToken == "" {
		return fmt.Errorf("配置缺少 bitable.app_token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()

	insts, err := db.SyncInstances(ctx, 0)
	if err != nil {
		return err
	}
	fmt.Printf("本地库 %s：%d 个实例将被清空\n", cfg.Paths.DB, len(insts))
	for _, in := range insts {
		fmt.Printf("  - %s  %s  %s\n", short(in.Sub.InstanceCode),
			in.Sub.ApprovalName, in.Sub.Applicant)
	}

	// 表里的行
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	type tablePlan struct {
		key, id  string
		recordID []string
	}
	var plans []tablePlan
	for _, key := range []string{"submission", "integrated"} {
		id := cfg.Feishu.Bitable.Tables[key]
		if id == "" {
			continue
		}
		recs, err := client.SearchBitableRecords(ctx, appToken, id, nil, 500)
		if err != nil {
			return fmt.Errorf("读取表 %s(%s) 失败: %w", key, id, err)
		}
		p := tablePlan{key: key, id: id}
		for _, r := range recs {
			p.recordID = append(p.recordID, r.RecordID)
		}
		plans = append(plans, p)
		fmt.Printf("表 %-10s %s：%d 行将被删除\n", key, id, len(p.recordID))
	}
	if dryRun {
		fmt.Println("\n（dry-run：什么都没删）")
		return nil
	}

	// ① 先备份
	if path, err := db.Backup(ctx, cfg.Paths.BackupDir, cfg.Paths.BackupKeep); err != nil {
		return fmt.Errorf("清空前备份失败，已中止（不敢在没有备份的情况下清库）: %w", err)
	} else {
		fmt.Printf("✓ 已备份本地库 → %s\n", path)
	}
	// ② 清本地库
	n, ev, err := db.PurgeAll(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("✓ 本地库已清空：%d 个实例 / %d 条证据\n", n, ev)
	// ③ 挪走图片目录（否则 -reindex 会把证据装回来）
	if moved, err := parkLocalFiles(cfg.Paths.BackupDir); err != nil {
		fmt.Printf("⚠ 图片目录没挪走（下次 -reindex 会把证据装回来）: %v\n", err)
	} else if moved != "" {
		fmt.Printf("✓ 图片目录已挪到 %s（不会丢，只是不再被 reindex 扫到）\n", moved)
	}
	// ④ 删表里的行
	for _, p := range plans {
		gone := 0
		for _, rid := range p.recordID {
			if err := client.DeleteBitableRecord(ctx, appToken, p.id, rid); err != nil {
				return fmt.Errorf("删除表 %s 的行 %s 失败: %w", p.key, rid, err)
			}
			gone++
		}
		fmt.Printf("✓ 表 %s：已删除 %d 行\n", p.key, gone)
	}
	return nil
}

// parkLocalFiles 把 data/extract/files 挪到备份目录下，返回新位置。
//
// 不直接删：图片是原始凭证，删了不可逆；挪走只是让 -reindex 扫不到。
func parkLocalFiles(backupDir string) (string, error) {
	src := filepath.Join("data", "extract", "files")
	if _, err := os.Stat(src); err != nil {
		return "", nil // 没有就不用管
	}
	if backupDir == "" {
		backupDir = filepath.Join("data", "backup")
	}
	dst := filepath.Join(backupDir, "purged-"+time.Now().Format("20060102-150405"))
	// ★ 必须建 dst **本身**，不是它的父目录：下面要把 src 重命名成 dst/files，
	//   而 rename 不会自动创建目标路径的父目录 —— 只建 backup/ 是不够的，
	//   会以 ENOENT 失败（实测踩过：库清空了、表也清空了，图片却还留在原地）。
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(src, filepath.Join(dst, "files")); err != nil {
		return "", err
	}
	return dst, nil
}
