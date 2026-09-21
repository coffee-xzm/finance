// Command reconx 是只读侦察工具（新需求对齐期使用）：只 GET，不写任何飞书数据。
//
// 用法：
//
//	go run ./cmd/reconx                       # 默认读核对/流水 base + 两个审批表单
//	go run ./cmd/reconx -approval CODE1,CODE2 # 指定要读的审批定义
//	go run ./cmd/reconx -base APP_TOKEN       # 额外读一个 base
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

// s_approvalRoles 取角色化审批列表（兼容旧配置的单审批写法）。
func s_approvalRoles(cfg *config.Config) []config.ApprovalRole {
	if len(cfg.Feishu.Approvals) > 0 {
		return cfg.Feishu.Approvals
	}
	if cfg.Feishu.ApprovalCode != "" {
		return []config.ApprovalRole{{Role: config.RoleInvoiceCollect, Code: cfg.Feishu.ApprovalCode}}
	}
	return nil
}

func main() {
	cfgPath := flag.String("config", "", "config.yml 路径")
	// ★ 这几个默认值原来写死了**真实租户标识符**（仓库是公开的，不能带）。
	//   留空即从 config.yml 取（reconx 本来就已经加载了配置）。
	flowBase := flag.String("flow-base", "", "流水 base（留空=取 config 的 bases.flow）")
	reviewBase := flag.String("review-base", "", "核对 base（留空=取 config 的 bases.review）")
	extraBase := flag.String("base", "", "额外要读的 base app_token")
	codes := flag.String("approval", "", "要读的审批 code，逗号分隔（留空=取 config 的 feishu.approvals）")
	instances := flag.Bool("instances", true, "是否读最近一条实例的值")
	records := flag.Int("records", 2, "每表只读拉多少条记录")
	rawInstance := flag.String("raw-instance", "", "只打印该实例详情的原始 JSON 后退出")
	perm := flag.String("perm", "", "只读：查某文档的协作者列表（传 app_token / 文档 token）")
	permType := flag.String("perm-type", "bitable", "文档类型：bitable|docx|sheet|wiki")
	probeWrite := flag.Bool("probe-write", false, "★写：在 -base 的每张表建一个临时文本字段再删掉（探测写权限）")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	if *rawInstance != "" {
		_, raw, err := c.GetInstanceDetail(ctx, *rawInstance)
		if err != nil {
			die(err)
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			fmt.Println(string(raw))
			return
		}
		fmt.Println(buf.String())
		return
	}

	if *probeWrite {
		if *extraBase == "" {
			die(fmt.Errorf("需要 -base <app_token>"))
		}
		tables, err := c.ListBitableTables(ctx, *extraBase)
		if err != nil {
			die(err)
		}
		for _, t := range tables {
			fid, err := c.CreateBitableField(ctx, *extraBase, t.TableID, feishu.FieldSpec{
				FieldName: "__写权限探测__", Type: 1,
			})
			if err != nil {
				fmt.Printf("  ✗ %s (%s) 不可写: %v\n", t.Name, t.TableID, err)
				continue
			}
			fmt.Printf("  ✓ %s (%s) 可写，临时字段 %s 已建\n", t.Name, t.TableID, fid)
			if err := c.DeleteBitableField(ctx, *extraBase, t.TableID, fid); err != nil {
				fmt.Printf("    ⚠ 临时字段删除失败（请手工删「__写权限探测__」）: %v\n", err)
			} else {
				fmt.Printf("    已删除临时字段\n")
			}
		}
		return
	}

	if *perm != "" {
		data, err := c.RawGet(ctx, "/drive/v1/permissions/"+*perm+"/members?type="+*permType)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ 查协作者失败（可能需要 drive:drive 或 drive:permission 权限）: %v\n", err)
			os.Exit(1)
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, data, "", "  "); err != nil {
			fmt.Println(string(data))
			return
		}
		fmt.Println(buf.String())
		return
	}

	// 标志留空 → 回落到 config 里的真实值（这样源码里就不必写死租户标识符）
	rb, fb := *reviewBase, *flowBase
	if rb == "" {
		if b, ok := cfg.Base(config.BaseReview); ok {
			rb = b.AppToken
		}
	}
	if fb == "" {
		if b, ok := cfg.Base(config.BaseFlow); ok {
			fb = b.AppToken
		}
	}
	cds := *codes
	if cds == "" {
		var list []string
		for _, a := range s_approvalRoles(cfg) {
			list = append(list, a.Code)
		}
		cds = strings.Join(list, ",")
	}

	for _, b := range []struct{ name, tok string }{
		{"核对 base", rb}, {"流水 base", fb}, {"额外 base", *extraBase},
	} {
		if b.tok == "" {
			continue
		}
		fmt.Printf("\n═══ %s %s ═══\n", b.name, b.tok)
		dumpBase(ctx, c, b.tok, *records)
	}

	for _, cd := range strings.Split(cds, ",") {
		cd = strings.TrimSpace(cd)
		if cd == "" {
			continue
		}
		fmt.Printf("\n═══ 审批 %s ═══\n", cd)
		def, err := c.GetApprovalDefinition(ctx, cd)
		if err != nil {
			fmt.Printf("  ✗ %v\n", err)
			continue
		}
		ws, _ := feishu.ParseForm(def.Form)
		fmt.Printf("  名称=%s 状态=%s 控件=%d 节点=%d\n", def.ApprovalName, def.Status, len(ws), len(def.NodeList))
		for _, n := range def.NodeList {
			fmt.Printf("    节点 %s type=%s needApprover=%v\n", n.Name, n.NodeType, n.NeedApprover)
		}
		for i, w := range ws {
			fmt.Printf("  [%2d] %-30s type=%-14s id=%s opt=%s\n",
				i+1, trunc(w.Name, 30), w.Type, w.Key(), trunc(string(w.Option), 300))
		}
		if !*instances {
			continue
		}
		items, err := c.QueryInstances(ctx, feishu.InstanceQueryRequest{
			ApprovalCode: cd, From: time.Now().AddDate(0, 0, -30), To: time.Now(), UserIDType: "user_id",
		})
		if err != nil || len(items) == 0 {
			fmt.Printf("  （无可读实例: %v）\n", err)
			continue
		}
		det, _, err := c.GetInstanceDetail(ctx, items[0].Instance.Code)
		if err != nil {
			fmt.Printf("  ✗ 实例详情: %v\n", err)
			continue
		}
		fmt.Printf("  ── 实例 %s status=%s dept=%s ──\n", det.InstanceCode, det.Status, det.DepartmentID)
		iws, _ := feishu.ParseForm(det.Form)
		for i, w := range iws {
			fmt.Printf("     [%2d] %-22s type=%-14s value=%s\n",
				i+1, trunc(w.Name, 22), w.Type, trunc(string(w.Value), 240))
		}
	}
}

func dumpBase(ctx context.Context, c *feishu.Client, appToken string, records int) {
	tables, err := c.ListBitableTables(ctx, appToken)
	if err != nil {
		fmt.Printf("  ✗ 列表失败: %v\n", err)
		return
	}
	for _, t := range tables {
		fmt.Printf("  --- 表 %s (%s) ---\n", t.Name, t.TableID)
		fields, err := c.ListBitableFields(ctx, appToken, t.TableID)
		if err != nil {
			fmt.Printf("    ✗ %v\n", err)
			continue
		}
		for _, f := range fields {
			fmt.Printf("    %-28s type=%2d ui=%-14s %s\n", trunc(f.FieldName, 28), f.Type, f.UIType, optsOf(f.Property))
		}
		if records <= 0 {
			continue
		}
		recs, _ := c.ListBitableRecords(ctx, appToken, t.TableID, records)
		for i, r := range recs {
			fmt.Printf("    [记录 %d] %s\n", i+1, r.RecordID)
			var ks []string
			for k := range r.Fields {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			for _, k := range ks {
				v := string(r.Fields[k])
				if v == "null" || v == "" {
					continue
				}
				fmt.Printf("        %-28s = %s\n", trunc(k, 28), trunc(v, 150))
			}
		}
	}
}

func optsOf(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var p struct {
		Options []struct {
			Name string `json:"name"`
		} `json:"options"`
		DateFormatter string `json:"date_formatter"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	var out string
	if len(p.Options) > 0 {
		var ns []string
		for _, o := range p.Options {
			ns = append(ns, o.Name)
		}
		out = "opts=[" + strings.Join(ns, "/") + "]"
	}
	if p.DateFormatter != "" {
		out += " date=" + p.DateFormatter
	}
	return out
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "✗ %v\n", err)
	os.Exit(1)
}
