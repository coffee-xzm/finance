// Command approvals 发现租户里有哪些审批表单，并把订阅绑定到指定的那一个。
//
// 为什么不能直接"列举审批定义"：
//
//	`GET /approval/v4/approvals` 用 tenant_access_token 会返回
//	99991663 Invalid access token（同一个 token 调 /approvals/{code} 却正常）；
//	飞书 SDK 的 approvalV4 域也没有"列举定义"的方法；唯一带搜索语义的
//	search_launchable 明确要求 user_access_token。
//	常驻服务只有 tenant token → **无法枚举定义**。
//
// 可行的替代：`POST /approval/v4/instances/query` 按用户查实例，
// 每条结果都带 approval.code + approval.name，把去重后的结果当作"实际出现过的表单清单"。
// 代价：只能看到该用户发起/参与过的审批。
//
// 用法：
//
//	go run ./cmd/approvals                          # 发现 + 显示当前绑定是否匹配
//	go run ./cmd/approvals -match <审批名关键字>      # 换关键字
//	go run ./cmd/approvals -subscribe               # 订阅匹配到的唯一一个
//	go run ./cmd/approvals -unsubscribe-all         # 退订除它以外的全部（清理误绑）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

func main() {
	cfgPath := flag.String("config", "config.yml", "config.yml 路径")
	// 默认不写死审批名（仓库公开，真实名称不该进源码）：
	// 未传 -match 时用 config 里的 approval_name_expect。
	match := flag.String("match", "", "要绑定的审批名称关键字（默认取 config 的 approval_name_expect）")
	userID := flag.String("user", "", "按哪个用户查实例（默认取配置里的 admin_user_id）")
	days := flag.Int("days", 30, "回溯天数（接口单次跨度上限 30 天，会自动分段）")
	shallow := flag.Bool("shallow", false, "只查 -user 一个用户（默认会用已绑定审批的全体发起人做种子，发现更全）")
	doSub := flag.Bool("subscribe", false, "订阅匹配到的审批")
	unsubAll := flag.Bool("unsubscribe-all", false, "退订除匹配项外的全部审批")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 读配置: %v\n", err)
		os.Exit(1)
	}
	if *match == "" {
		*match = cfg.Feishu.ApprovalNameExpect
	}
	if *match == "" {
		fmt.Fprintln(os.Stderr, "✗ 没给 -match，config 里也没配 approval_name_expect，无法判断要绑哪个审批")
		os.Exit(1)
	}

	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	ctx := context.Background()

	// ── ① 配置里当前绑的是哪一个 ──────────────────────────────
	boundName, boundStatus := "（未配置）", ""
	if cfg.Feishu.ApprovalCode != "" {
		def, err := client.GetApprovalDefinition(ctx, cfg.Feishu.ApprovalCode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ 读取已配置的审批定义失败: %v\n", err)
		} else {
			boundName, boundStatus = def.ApprovalName, def.Status
		}
	}
	fmt.Printf("config.yml 当前绑定：\n  %s\n  %s  [%s]\n\n",
		cfg.Feishu.ApprovalCode, boundName, boundStatus)

	// ── ② 发现实际出现过的审批表单 ─────────────────────────────
	uid := *userID
	if uid == "" {
		uid = cfg.Feishu.AdminUserID
	}
	if uid == "" {
		fmt.Println("⚠ 没有可用的 user_id（config 里 admin_user_id 为空），跳过发现。")
		fmt.Println("  只能看到已配置的那一个。要发现其它表单，请给 -user 传一个用户 user_id。")
		checkMatch(*match, boundName, cfg.Feishu.ApprovalCode)
		return
	}

	found, err := discover(ctx, client, cfg.Feishu.ApprovalCode, uid, *days, !*shallow)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 查询实例失败: %v\n", err)
		fmt.Fprintln(os.Stderr, "  需要权限 approval:approval.list:readonly（注意与 approval:approval:readonly 不是同一个）")
		os.Exit(1)
	}
	if len(found) == 0 {
		fmt.Printf("在近 %d 天内没有查到任何审批实例。\n", *days)
		return
	}

	codes := make([]string, 0, len(found))
	for c := range found {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool {
		if found[codes[i]].count() != found[codes[j]].count() {
			return found[codes[i]].count() > found[codes[j]].count()
		}
		return codes[i] < codes[j]
	})

	fmt.Printf("近 %d 天内实际出现过的审批表单（%d 个，按实例号去重）：\n\n", *days, len(codes))
	for _, c := range codes {
		a := found[c]
		mark := "  "
		if strings.Contains(a.name, *match) {
			mark = "★ "
		}
		bound := ""
		if c == cfg.Feishu.ApprovalCode {
			bound = "   ← 当前绑定"
		}
		fmt.Printf("%s%-12s %-30s %5d 个实例%s\n", mark, short(c), a.name, a.count(), bound)
	}
	fmt.Printf("\n★ = 名称含 %q\n", *match)

	checkMatch(*match, boundName, cfg.Feishu.ApprovalCode)

	// ── ③ 订阅变更 ────────────────────────────────────────────
	if !*doSub && !*unsubAll {
		return
	}
	hits := codesMatching(found, *match)
	if len(hits) == 0 {
		fmt.Fprintf(os.Stderr, "\n✗ 没有任何审批名匹配 %q，未做任何订阅变更\n", *match)
		os.Exit(1)
	}
	if len(hits) > 1 {
		fmt.Fprintf(os.Stderr, "\n✗ 匹配到 %d 个，不唯一，拒绝自动绑定（避免绑错）：%v\n", len(hits), hits)
		os.Exit(1)
	}
	target := hits[0]

	if *doSub {
		if err := client.SubscribeApproval(ctx, target); err != nil {
			// 1390007「订阅已存在」= 本来就订着，属正常，不是失败。
			// （与 cmd/serve 的处理保持一致。）
			if strings.Contains(err.Error(), "1390007") ||
				strings.Contains(err.Error(), "subscription existed") {
				fmt.Printf("\n✓ %s（%s）已是订阅状态，无需重复订阅\n", found[target].name, target)
			} else {
				fmt.Fprintf(os.Stderr, "✗ 订阅失败: %v\n", err)
				fmt.Fprintln(os.Stderr, "  需要权限 approval:approval（或 approval:definition）—— readonly 不够")
				os.Exit(1)
			}
		} else {
			fmt.Printf("\n✓ 已订阅 %s（%s）\n", found[target].name, target)
		}
	}

	if *unsubAll {
		n := 0
		for _, c := range codes {
			if c == target {
				continue
			}
			if err := client.UnsubscribeApproval(ctx, c); err != nil {
				// 1390007「订阅不存在」等价于本来就没订阅，不算失败
				fmt.Printf("  · 跳过 %s（%s）: %v\n", found[c].name, short(c), err)
				continue
			}
			fmt.Printf("  ✓ 已退订 %s（%s）\n", found[c].name, short(c))
			n++
		}
		fmt.Printf("✓ 退订 %d 个，只保留 %s\n", n, found[target].name)
	}
}

// approvalHit 是发现到的一个审批定义。
// 用 instanceCode 集合去重 —— 同一个实例会从"按 approval_code 查"和
// "按发起人查"两条路各回来一次，直接累加会把数量算大（曾经算成 10197）。
type approvalHit struct {
	name  string
	codes map[string]bool
}

func (h approvalHit) count() int { return len(h.codes) }

// discover 发现租户里出现过的审批表单。
//
// 思路（因为 tenant token 没有"列举定义"接口）：
//
//	① 先用已绑定的 approval_code 查出它的**全部实例** —— 能拿到所有发起人的 user_id；
//	② 再拿这些 user_id 逐个查实例 —— 他们在别的表单里提交过什么，就一并暴露出来；
//	③ 汇总去重，得到"实际存在过"的表单清单。
//
// 只查 -user 一个人会严重低估：一个采购负责人可能从没发起过报销单。
// shallow=true 时退化为只查 admin 一个用户。
func discover(ctx context.Context, c *feishu.Client,
	approvalCode, adminUID string, days int, deep bool) (map[string]approvalHit, error) {

	if days <= 0 {
		days = 30
	}
	users := map[string]bool{}
	if adminUID != "" {
		users[adminUID] = true
	}
	out := map[string]approvalHit{}
	add := func(items []feishu.InstanceSearchItem) {
		for _, it := range items {
			code := it.Approval.Code
			if code == "" {
				continue
			}
			h := out[code]
			h.name = it.Approval.Name
			if h.codes == nil {
				h.codes = map[string]bool{}
			}
			if ic := it.Instance.Code; ic != "" {
				h.codes[ic] = true // 按实例号去重
			}
			out[code] = h
		}
	}

	// ① 已绑定审批的全量实例 → 收集发起人
	if deep && approvalCode != "" {
		for _, w := range windows(days) {
			items, err := c.QueryInstances(ctx, feishu.InstanceQueryRequest{
				ApprovalCode: approvalCode, From: w[0], To: w[1], UserIDType: "user_id",
			})
			if err != nil {
				return nil, err
			}
			add(items)
			for _, it := range items {
				if u := it.Instance.UserID; u != "" {
					users[u] = true
				}
			}
		}
	}

	// ② 逐个用户查实例
	for u := range users {
		for _, w := range windows(days) {
			items, err := c.QueryInstances(ctx, feishu.InstanceQueryRequest{
				UserID: u, From: w[0], To: w[1], UserIDType: "user_id",
			})
			if err != nil {
				// 单个用户查不动不该让整体失败（可能没权限看到某些人）
				fmt.Fprintf(os.Stderr, "  · 用户 %s 查询跳过: %v\n", u, err)
				continue
			}
			add(items)
		}
	}
	return out, nil
}

// windows 把 days 切成每段 ≤30 天的 [from, to] 区间（接口硬性上限）。
func windows(days int) [][2]time.Time {
	var out [][2]time.Time
	now := time.Now()
	for offset := 0; offset < days; offset += 30 {
		span := 30
		if days-offset < span {
			span = days - offset
		}
		out = append(out, [2]time.Time{
			now.AddDate(0, 0, -(offset + span)),
			now.AddDate(0, 0, -offset),
		})
	}
	return out
}

func codesMatching(found map[string]approvalHit, kw string) []string {
	var hits []string
	for c, a := range found {
		if strings.Contains(a.name, kw) {
			hits = append(hits, c)
		}
	}
	sort.Strings(hits)
	return hits
}

// checkMatch 提醒"当前绑定的那个是否就是关键字要找的那个"。
func checkMatch(kw, boundName, boundCode string) {
	fmt.Println()
	switch {
	case boundCode == "":
		fmt.Printf("⚠ config.yml 里 approval_code 为空 —— 需要填 %q 对应的 code\n", kw)
	case strings.Contains(boundName, kw):
		fmt.Printf("✓ 当前绑定 %s 就是 %q 对应的表单，无需改动\n", short(boundCode), kw)
	default:
		fmt.Printf("⚠ 当前绑定的是 %q，与要找的 %q 不一致 —— 需要改 config.yml 的 approval_code\n",
			boundName, kw)
	}
}

// short 缩短 code 用于日志。
func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
