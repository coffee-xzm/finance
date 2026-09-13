// Command doctor 逐项体检：把每项外部能力单独测一遍，并给出确切的补救措施。
//
// 动机：调试权限时，"猜"是最耗时的。本命令把每项能力拆成一次最小调用，
// 并按错误码区分两类失败：
//   - 99991672（Access denied, one of the following scopes is required: [...]）
//     → **scope 没开**，错误信息里就带申请链接
//   - 91403 Forbidden（不列 scope）
//     → **scope 有，但文档级权限不足**（应用没被加成协作者/编辑者）
//
// 用法：go run ./cmd/doctor
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

type check struct {
	name   string
	need   string
	fn     func(context.Context, *feishu.Client, *config.Config) error
	remedy string
	// warn 为真时，失败只提示不计数：该项不影响主流程
	// （如部门 name 字段权限缺失 → 有对照表兜底，仍能填出发起人部门）
	warn bool
}

func main() {
	cfgPath := flag.String("config", "", "config.yml 路径")
	wikiTok := flag.String("wiki-token", "", "可选：wiki 节点 token（默认用 config 里的）")
	deptIDFlag := flag.String("dept-id", "99a8a57fac21fd39", "可选：用于测试部门解析的部门 ID")
	flag.Parse()

	if *cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		*cfgPath = p
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}

	appTok := cfg.Feishu.Bitable.AppToken
	tabID := cfg.Feishu.Bitable.Tables["submission"]
	approval := cfg.Feishu.ApprovalCode
	wtok := *wikiTok

	deptID := *deptIDFlag
	checks := []check{
		{
			name: "获取 tenant_access_token",
			need: "（app_id/app_secret 正确即可）",
			fn: func(ctx context.Context, c *feishu.Client, _ *config.Config) error {
				_, err := c.TenantAccessToken(ctx)
				return err
			},
			remedy: "检查 feishu.app_id / app_secret",
		},
		{
			name: "读审批定义",
			need: "approval:approval:readonly",
			fn: func(ctx context.Context, c *feishu.Client, cfg *config.Config) error {
				if approval == "" {
					return fmt.Errorf("未配置 approval_code，跳过")
				}
				def, err := c.GetApprovalDefinition(ctx, approval)
				if err != nil {
					return err
				}
				// 租户里有多个审批表单（实测 4 个），code 填错不会有任何报错，
				// 只会静默收不到事件、或收到别的表单的数据。这里把真实名称显式打出来，
				// 并在配置了 approval_name_expect 时做硬校验。
				fmt.Printf("        实际绑定: %q  [%s]\n", def.ApprovalName, def.Status)
				if want := cfg.Feishu.ApprovalNameExpect; want != "" &&
					!strings.Contains(def.ApprovalName, want) {
					return fmt.Errorf("绑定的审批是 %q，但期望名称含 %q", def.ApprovalName, want)
				}
				return nil
			},
			remedy: "补 approval:approval:readonly；名称不符则改 approval_code 或 approval_name_expect（先跑 go run ./cmd/approvals 看清单）",
		},
		{
			name: "读多维表格字段",
			need: "bitable:app:readonly 或 base:field:read",
			fn: func(ctx context.Context, c *feishu.Client, _ *config.Config) error {
				if appTok == "" || tabID == "" {
					return fmt.Errorf("未配置 bitable.app_token / tables.submission，跳过")
				}
				_, err := c.ListBitableFields(ctx, appTok, tabID)
				return err
			},
			remedy: "补 bitable:app:readonly（并把应用加为文档协作者）",
		},
		{
			name: "★ 写多维表格字段",
			need: "bitable:app 或 base:field:create",
			fn: func(ctx context.Context, c *feishu.Client, _ *config.Config) error {
				if appTok == "" || tabID == "" {
					return fmt.Errorf("未配置 bitable，跳过")
				}
				// ★ 探针必须**自清理**：建完就删。
				// 否则每次体检都在别人的表里留一个 __doctor_probe__ 垃圾字段
				//（实测发生过，事后才发现表里多了一列）。
				const probe = "__doctor_probe__"
				// 先删残留（可能是上次中断留下的）
				if fs, err := c.ListBitableFields(ctx, appTok, tabID); err == nil {
					for _, f := range fs {
						if f.FieldName == probe {
							_ = c.DeleteBitableField(ctx, appTok, tabID, f.FieldID)
						}
					}
				}
				id, err := c.CreateBitableField(ctx, appTok, tabID, feishu.FieldSpec{
					FieldName: probe, Type: 1,
				})
				if err != nil {
					return err
				}
				return c.DeleteBitableField(ctx, appTok, tabID, id)
			},
			remedy: "补 bitable:app；若已给仍失败 → 把【应用】加为该文档/wiki 空间的【可编辑】成员",
		},
		{
			name: "解析 wiki 节点",
			need: "wiki:wiki:readonly 或 wiki:node:read",
			fn: func(ctx context.Context, c *feishu.Client, _ *config.Config) error {
				if wtok == "" {
					return fmt.Errorf("未提供 wiki token，跳过")
				}
				_, err := c.GetWikiNode(ctx, wtok, "")
				return err
			},
			remedy: "补 wiki:wiki:readonly",
		},
		{
			name: "解析部门名（发起人部门）",
			need: "contact:contact.base:readonly + ★ 通讯录数据范围要覆盖该部门",
			fn: func(ctx context.Context, c *feishu.Client, cfg *config.Config) error {
				if deptID == "" {
					return fmt.Errorf("未提供部门 ID，跳过")
				}
				_, err := c.GetDepartmentName(ctx, deptID)
				return err
			},
			remedy: "三层依次检查：① scope（contact:contact.base:readonly）；" +
				"② 【权限管理 → 数据权限范围】覆盖该部门；" +
				"③ 【字段权限】contact:department.base:readonly（name 属于字段级权限）",
			// 不阻塞：拿不到 name 时会用「部门 ID 对照表」兜底，仍能填出发起人部门。
			warn: true,
		},
		{
			name: "发消息（机器人）",
			need: "im:message + ★ 机器人能力已启用 + 收件人在可用范围",
			fn: func(ctx context.Context, c *feishu.Client, cfg *config.Config) error {
				if cfg.Feishu.AdminUserID != "" {
					return c.SendTextMessage(ctx, "user_id", cfg.Feishu.AdminUserID, "doctor 探测消息")
				}
				if cfg.Feishu.AdminOpenID != "" {
					return c.SendTextMessage(ctx, "open_id", cfg.Feishu.AdminOpenID, "doctor 探测消息")
				}
				return fmt.Errorf("未配置 admin_user_id / admin_open_id，跳过")
			},
			remedy: "① 补 im:message；② 【应用能力 → 添加应用能力 → 机器人】并发布；" +
				"③ 把收件人加入应用可用范围",
		},
	}

	fmt.Printf("配置文件: %s\n\n", *cfgPath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	var fail int
	for i, ck := range checks {
		err := ck.fn(ctx, client, cfg)
		mark := "✓"
		note := ""
		if err != nil {
			switch {
			case strings.Contains(err.Error(), "跳过"):
				mark, note = "−", "（未配置，跳过）"
			case ck.warn:
				mark, note = "⚠", diagnose(err)+"（不阻塞：有兜底）"
			default:
				mark, note = "✗", diagnose(err)
				fail++
			}
		}
		_ = mark
		fmt.Printf("%s [%d/%d] %-22s 需 %s\n", mark, i+1, len(checks), ck.name, ck.need)
		if note != "" {
			fmt.Printf("        %s\n", note)
		}
		if err != nil && !strings.Contains(err.Error(), "跳过") {
			fmt.Printf("        症状: %s\n", firstLine(err.Error()))
			fmt.Printf("        补救: %s\n", ck.remedy)
		}
	}
	fmt.Println()
	if fail == 0 {
		fmt.Println("全部通过。")
	} else {
		fmt.Printf("%d 项未通过 —— 按上面的「补救」逐条处理。\n", fail)
	}
}

// diagnose 按错误码区分两类失败。
func diagnose(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "99991672"):
		return "★ scope 未开通（错误里带申请链接）"
	case strings.Contains(s, "91403"):
		return "★ scope 已开通，但【文档级权限】不足"
	case strings.Contains(s, "230006"):
		return "★ scope 已开通，但【机器人能力未启用】"
	case strings.Contains(s, "99992361"):
		return "★ open_id 跨应用（该 open_id 不是本应用的），改用 user_id"
	case strings.Contains(s, "40004") || strings.Contains(s, "no dept authority"):
		return "★ scope 已开通，但【通讯录数据范围】不包含该部门"
	case strings.Contains(s, "99992357"):
		return "★ 部门 ID 类型不对（open_department_id 需 od- 前缀）"
	case strings.Contains(s, "没有 name") || strings.Contains(s, "department.base:readonly"):
		return "★ 接口能调、部门能看，但【name 是字段级权限】未开通"
	case strings.Contains(s, "跳过"):
		return "（未配置，跳过）"
	default:
		return "其它错误"
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
