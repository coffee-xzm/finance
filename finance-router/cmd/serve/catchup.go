// catchup 负责"断网 / 断电 / 进程重启之后把漏掉的审批补上"。
//
// 为什么必须要有：飞书审批事件走**出站长连接**，而长连接是"集群模式不广播、
// 离线不补推"——服务没在听的那一刻，事件就永久丢了。实测教训：2026-09-19 21:38
// 通过的采购单 `529A9208` 因为当时本机没有服务在跑，流水行 / 采购申请表 /
// 代建发票单**一个都没生成**，而且事后没有任何机制会去补。
//
// 所以：启动时先扫一遍，此后每 10 分钟扫一遍，按**幂等锚点**判断"该做还没做"。
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/store"
)

// sweepSeen 记"已经入队处理过的终态实例"，避免每 10 分钟重复入队、刷日志。
//
// 只对**终态**打标（已通过 / 已拒绝 / 已剔除）：这些状态不会再变，
// 忘了也不会漏；而"审批中且表单已有附件但本地无证据"这种**不打标**，
// 因为它要一直重试到真的抽取入库为止。
// 进程重启后重新清空 —— 代价只是重启后多扫一轮。
var (
	sweepSeenMu sync.Mutex
	sweepSeen   = map[string]bool{}
)

// sweepFirstSeen 报告该实例是否是本次进程生命周期内第一次被扫到（并记下）。
func sweepFirstSeen(code string) bool {
	sweepSeenMu.Lock()
	defer sweepSeenMu.Unlock()
	if sweepSeen[code] {
		return false
	}
	sweepSeen[code] = true
	return true
}

const (
	// catchUpWindow 每轮回看多久。断电/断网一般不超过这个量级；
	// 更久以前的历史用人工补跑（cmd/purchase / cmd/reviewflow）。
	catchUpWindow = 72 * time.Hour
	// catchUpInterval 扫描间隔，与机器人守护脚本的 10 分钟节拍一致。
	catchUpInterval = 10 * time.Minute
)

// catchUpLoop 启动时立刻补漏一次，此后每 catchUpInterval 一次。
func (s *service) catchUpLoop(ctx context.Context) {
	s.catchUpSweep(ctx)
	t := time.NewTicker(catchUpInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.catchUpSweep(ctx)
		}
	}
}

// catchUpSweep 扫一遍最近 catchUpWindow 内两个审批定义的实例，把"该做还没做"的补进队列。
//
// 判定全部落在**幂等锚点**上，所以重复扫描安全（不会重复记账、不会重复建单）：
//
//	采购：本地 purchase_sync 已有记录 → 跳过；只处理 APPROVED。
//	发票：APPROVED → 补跑（归档/回写本身幂等）；
//	      审批中但**表单里已有附件而本地一条证据都没有** → 重新抽取
//	      （覆盖两种漏法：离线期间本人补齐并提交；代建空单被本人补齐后重提）。
//
// 只入队、不直接处理：worker 是单实例串行的（manifest.jsonl 整体重写 + 飞书同表禁并发写）。
func (s *service) catchUpSweep(ctx context.Context) {
	roles := s.approvalRoles()
	if len(roles) == 0 {
		return
	}
	now := time.Now()
	from := now.Add(-catchUpWindow)
	queued := 0
	for _, a := range roles {
		items, err := s.client.QueryInstances(ctx, feishu.InstanceQueryRequest{
			ApprovalCode: a.Code, From: from, To: now, UserIDType: "user_id",
		})
		if err != nil {
			// 扫描失败不能影响服务本身（可能就是刚断网）：下一轮再试。
			fmt.Printf("  ⚠ 补漏扫描 [%s] 失败（下一轮重试）: %v\n", a.Role, err)
			continue
		}
		for _, it := range items {
			code := it.Instance.Code
			if code == "" {
				continue
			}
			status := it.Instance.Status
			approved := strings.EqualFold(status, "APPROVED") || store.StateWord(status) == "已通过"

			switch {
			case a.Role == config.RolePurchase:
				if !approved {
					continue
				}
				if _, ok, _ := s.db.GetPurchase(ctx, code); ok {
					continue // 幂等锚点：处理过就别再碰
				}
				if !sweepFirstSeen(code) {
					continue // 已入队过，等 worker 落库后由上面的锚点接管
				}
				fmt.Printf("  ⟳ 补漏：采购 %s 已通过但本地没处理过 → 补跑\n", short(code))

			default: // invoice_collect
				switch {
				case approved:
					if !sweepFirstSeen(code) {
						continue
					}
					fmt.Printf("  ⟳ 补漏：发票单 %s 已通过 → 补跑归档/回写\n", short(code))
				case store.IsRejectedState(status):
					// 已拒绝/撤回/撤销类：worker 会把它从本地库与核对表剔掉，
					// 之后本地就没痕迹了（所以不能靠本地库当锚点），只能靠这里打标。
					if !sweepFirstSeen(code) {
						continue
					}
					fmt.Printf("  ⟳ 补漏：发票单 %s（%s）→ 补跑剔除\n",
						short(code), store.StateWord(status))
				case s.hasUncollectedAttachments(ctx, code):
					fmt.Printf("  ⟳ 补漏：发票单 %s（%s）表单已有附件但本地无证据 → 重新抽取\n",
						short(code), store.StateWord(status))
				default:
					continue
				}
			}
			if s.enqueue(job{InstanceCode: code, Status: status, Role: a.Role}) {
				queued++
			}
		}
	}
	if queued > 0 {
		fmt.Printf("  ⟳ 补漏扫描完成：入队 %d 个实例\n", queued)
	}
}

// hasUncollectedAttachments 判断"审批中"的发票单值不值得重新抽取：
// 本地**一条证据都没有**，而审批表单里**已经有附件**（发票/订单/付款任一）。
//
// 用于补上"代建空单 → 本人补齐后重提"这条路：那时代建时已经入过一次库
// （当时 0 附件），事件再来会被"已处理过"挡掉，只能靠这里重抽。
func (s *service) hasUncollectedAttachments(ctx context.Context, instanceCode string) bool {
	if evs, err := s.db.EvidenceOf(ctx, instanceCode); err == nil && len(evs) > 0 {
		return false // 抽过了，不重复下载/识别
	}
	det, _, err := s.client.GetInstanceDetail(ctx, instanceCode)
	if err != nil {
		return false
	}
	ws, err := feishu.ParseForm(det.Form)
	if err != nil {
		return false
	}
	for _, w := range ws {
		if w.Type == "attachmentV2" && len(w.AttachmentURLs()) > 0 {
			return true
		}
	}
	return false
}

// enqueue 非阻塞入队；队列满则记一次失败（下一轮补漏会重试）。
func (s *service) enqueue(j job) bool {
	select {
	case s.queue <- j:
		return true
	default:
		s.failed.Add(1)
		fmt.Printf("  ⚠ 队列已满，暂不入队 %s（下一轮补漏会重试）\n", short(j.InstanceCode))
		return false
	}
}

// approvalRoles 与启动时校验/订阅用的是**同一份**角色列表（含旧配置的单审批退化）。
func (s *service) approvalRoles() []config.ApprovalRole {
	if len(s.cfg.Feishu.Approvals) > 0 {
		return s.cfg.Feishu.Approvals
	}
	if s.cfg.Feishu.ApprovalCode != "" {
		return []config.ApprovalRole{{
			Role: config.RoleInvoiceCollect, Code: s.cfg.Feishu.ApprovalCode,
			NameExpect: s.cfg.Feishu.ApprovalNameExpect,
		}}
	}
	return nil
}
