// Command serve 是常驻服务：通过**长连接**接收飞书审批事件，并自动触发处理。
//
// 架构定位（docs/20-synthesis/02-architecture-v0.1.md §4.8）：
//   - 事件走**出站长连接**（WebSocket），**无需公网 IP / 域名 / 内网穿透**；
//   - 长连接是**集群模式不广播**（多实例只有一个收到）→ 消费端**必须单实例**；
//   - 回调须 **3 秒内返回** → 处理函数只做"落库 + 入队"，重活交给 worker。
//
// 幂等与乱序保护：
//   - `event_id` 落库主键（飞书是"至少一次"投递，同一事件会重复到达）；
//   - 状态机白名单（事件可能乱序，晚到的旧事件不能把终态打回）。
//
// 处理链路（一个新审批被首次看到时）：
//
//	extract（下载 → PDF转PNG → 识别 → 本地入库）
//	  → sync（落到多维表格「报销核对」）
//	  → notify（需人工的行发消息给管理员）
//
// 用法：
//
//	go run ./cmd/serve                # 前台运行
//	go run ./cmd/serve -no-subscribe  # 不自动订阅（只收事件）
//	go run ./cmd/serve -dry-run       # 只收事件，不做处理
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkapproval "github.com/larksuite/oapi-sdk-go/v3/service/approval/v4"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/pipeline"
	"github.com/coffee/finance-router/internal/store"
)

// 版本信息：由构建时的 -ldflags 注入（见 scripts/deploy.sh）。
// 「机器上跑的到底是哪一版」必须能一句话问出来，否则热更新就是盲的。
var (
	version   = "dev"
	buildTime = "unknown"
)

// fakeEventObj 构造一个用于自测的事件对象（不经过网络）。
//
// approvalCode 必须由调用方传入（取自 config），**不要在这里写死**：
// 仓库是公开的，任何真实的审批定义 Code 都不该被编译进二进制。
func fakeEventObj(approvalCode, eventID, instanceCode, status string) *larkapproval.P2InstanceStatusChangedV4 {
	ac := approvalCode
	return &larkapproval.P2InstanceStatusChangedV4{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{
				EventID: eventID, EventType: "approval_instance",
			},
		},
		Event: &larkapproval.P2InstanceStatusChangedV4Data{
			ApprovalCode: &ac, InstanceCode: &instanceCode, Status: &status,
		},
	}
}

// job 是一个待处理任务。
type job struct {
	InstanceCode string
	EventID      string
	Status       string
	// Role 是审批角色（invoice_collect / purchase），决定走哪条链路。
	Role string
}

type service struct {
	cfg    *config.Config
	client *feishu.Client
	db     *store.DB

	queue  chan job
	dryRun bool

	// 计数（原子，供退出时打印）
	received, dup, ignored, failed, processed atomic.Int64
}

func main() {
	var (
		cfgPath     = flag.String("config", "", "config.yml 路径")
		noSubscribe = flag.Bool("no-subscribe", false, "启动时不调用订阅接口")
		dryRun      = flag.Bool("dry-run", false, "只收事件与落库，不做实际处理")
		queueSize   = flag.Int("queue", 256, "待处理队列长度")
		once        = flag.String("once", "", "处理指定实例后退出（不走长连接；用于回填与自测）")
		fakeV1      = flag.String("fake-v1", "",
			"构造一个 **v1.0 协议**的假事件（原始 JSON 路径），格式 <instance_code>[:STATUS]。用于验证 v1 解析")
		fakeEvent = flag.String("fake-event", "",
			"构造一个假事件走完整事件路径，格式 <instance_code>[:STATUS]，可重复用逗号分隔。用于自测幂等与状态机")
		pidfilePath = flag.String("pidfile", defaultPidfile,
			"pidfile 路径（单实例守卫用；空字符串=关闭）")
		forceStart = flag.Bool("force", false, "忽略 pidfile 里已有的实例，强制启动")
		showVer    = flag.Bool("version", false, "打印版本后退出")
		task       = flag.String("task", "",
			"跑一次性运维任务后退出（机器人上没有 Go，只能靠这个二进制）：\n"+
				"  refresh-meta  从审批单刷新表单元信息（归属组/是否支付宝/…）\n"+
				"  regroup       用库里已有证据重算分组\n"+
				"  sync          按「一张发票一行」落表\n"+
				"  archive       把「通过」的行归档进整合表\n"+
				"  notify        给「待审」的行发私信\n"+
				"  purge-all     ★危险★ 清空本地库 + 核对/整合表的全部行（先自动备份）")
		taskForce = flag.Bool("task-force", false, "配合 -task regroup：连已有分组一起重算")
		taskDry   = flag.Bool("task-dry", false, "配合 -task：只打印，不写库/不写表")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("serve %s (built %s)\n", version, buildTime)
		return
	}

	if *cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			os.Exit(1)
		}
		*cfgPath = p
	}
	// 一次性运维任务：**在开库之前**分流，避免同时开两个连接。
	// 机器人上没装 Go，这些运维动作只能由这个二进制自己提供。
	if *task != "" {
		if err := runTask(*task, *cfgPath, *taskForce, *taskDry); err != nil {
			fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
			os.Exit(1)
		}
		return
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}
	if cfg.Feishu.AppID == "" || cfg.Feishu.AppSecret == "" {
		fmt.Fprintln(os.Stderr, "✗ 配置缺少 feishu.app_id / app_secret")
		os.Exit(1)
	}

	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 打开本地库失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	s := &service{
		cfg:    cfg,
		client: feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret),
		db:     db,
		queue:  make(chan job, *queueSize),
		dryRun: *dryRun,
	}

	// 优雅退出：收到 SIGINT/SIGTERM 时取消 ctx，长连接会关闭
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── 自测入口 1：直接处理一个实例（不走事件）──
	if *once != "" {
		fmt.Println("══ 单次处理模式 ══")
		s.process(ctx, job{InstanceCode: *once, Status: "MANUAL"})
		s.printSummary()
		return
	}

	// ── 自测入口 1.5：注入 v1.0 协议的假事件（走原始 JSON 解析路径）──
	if *fakeV1 != "" {
		fmt.Println("══ v1.0 假事件注入模式 ══")
		code, status := *fakeV1, "APPROVED"
		if i := strings.LastIndex(*fakeV1, ":"); i > 0 {
			code, status = (*fakeV1)[:i], (*fakeV1)[i+1:]
		}
		body := fmt.Sprintf(`{"uuid":"fakev1_%s_%s_%d","event":{"type":"approval_instance","approval_code":"%s","instance_code":"%s","status":"%s","operate_time":"%d"}}`,
			code, status, time.Now().UnixNano(), s.cfg.Feishu.ApprovalCode, code, status, time.Now().UnixMilli())
		req := &larkevent.EventReq{Body: []byte(body)}
		if err := s.handleV1Event(ctx, "approval_instance", req); err != nil {
			fmt.Printf("  ✗ %v\n", err)
		}
		select {
		case j := <-s.queue:
			s.process(ctx, j)
		default:
		}
		s.printSummary()
		return
	}

	// ── 自测入口 2：注入假事件，走完整事件路径（幂等 + 状态机 + 处理）──
	if *fakeEvent != "" {
		fmt.Println("══ 假事件注入模式（不连飞书）══")
		for _, spec := range strings.Split(*fakeEvent, ",") {
			spec = strings.TrimSpace(spec)
			if spec == "" {
				continue
			}
			code, status := spec, "PENDING"
			if i := strings.LastIndex(spec, ":"); i > 0 {
				code, status = spec[:i], spec[i+1:]
			}
			// 用「实例号+状态+时间戳」造一个唯一 event_id；重复注入同一状态可测幂等
			eid := fmt.Sprintf("fake_%s_%s_%d", code, status, time.Now().UnixNano())
			if err := s.handleInstanceEvent(ctx,
				fakeEventObj(s.cfg.Feishu.ApprovalCode, eid, code, status)); err != nil {
				fmt.Printf("  ✗ %v\n", err)
			}
			// 排空队列（worker 在长连接分支才启动，这里手动处理）
			select {
			case j := <-s.queue:
				s.process(ctx, j)
			default:
			}
		}
		s.printSummary()
		return
	}

	fmt.Println("══ 财务常驻服务 ══")
	fmt.Printf("配置      : %s\n", *cfgPath)
	fmt.Printf("本地库    : %s\n", cfg.Paths.DB)

	// ★ 启动即校验"绑的到底是哪张表单"。
	//
	// 租户里有多个审批定义（实测 4 个）。config 里 approval_code 填错不会有任何报错：
	// 只会静默地收不到事件，或收到别的表单的数据。所以这里把 code 解析成**名字**打出来，
	// 并在配置了 approval_name_expect 时做硬校验 —— 对不上直接拒绝启动（fail closed）。
	// ★ 角色化审批绑定（docs/30-review/33 §5.1）：invoice_collect + purchase。
	// 旧配置只有单个 approval_code 时退化成"一个发票收集角色"。
	// 这份列表同时被补漏扫描（catchup.go）使用，所以走同一个方法。
	approvals := s.approvalRoles()
	if len(approvals) == 0 {
		fmt.Println("审批定义  : ⚠ 未配置 feishu.approvals / approval_code")
	}
	for _, a := range approvals {
		def, derr := s.client.GetApprovalDefinition(ctx, a.Code)
		if derr != nil {
			fmt.Printf("审批定义  : [%s] %s\n            ⚠ 解析名称失败: %v\n", a.Role, a.Code, derr)
			continue
		}
		fmt.Printf("审批定义  : [%s] %s  (%s)\n", a.Role, def.ApprovalName, def.Status)
		if a.NameExpect != "" && !strings.Contains(def.ApprovalName, a.NameExpect) {
			fmt.Printf("\n✗ 拒绝启动：角色 %s 期望名称含 %q，实际是 %q\n", a.Role, a.NameExpect, def.ApprovalName)
			fmt.Println("  改 config.yml 的 feishu.approvals，或先看有哪些表单：go run ./cmd/approvals")
			os.Exit(1)
		}
	}
	fmt.Printf("处理模式  : %v\n", map[bool]string{true: "dry-run（不处理）", false: "正常"}[*dryRun])

	// 先订阅审批定义。**不调这一步，一条事件都收不到。**
	if !*noSubscribe {
		if len(approvals) == 0 {
			fmt.Println("⚠ 未配置审批定义，跳过订阅")
		}
		for _, a := range approvals {
			err := s.client.SubscribeApproval(ctx, a.Code)
			switch {
			case err == nil:
				fmt.Printf("✓ [%s] 已订阅该审批定义的实例事件\n", a.Role)
			case strings.Contains(err.Error(), "subscription existed") ||
				strings.Contains(err.Error(), "1390007"):
				fmt.Printf("✓ [%s] 该审批定义已是订阅状态（subscription existed）\n", a.Role)
			default:
				// 其它失败不退出：可能只是缺 approval:approval 权限，
				// 若管理员已在审批后台订阅过，事件仍会到达。
				fmt.Printf("⚠ [%s] 订阅调用失败（若已在审批后台订阅过可忽略）: %v\n", a.Role, err)
			}
			// 「27-流水登记」这一步是走"代建预填+撤回"还是"私信让登记人自己开单"，
			// 由审批定义决定（有没有真实审批人节点）。启动时先说清，省得事后猜。
			if a.Role == config.RoleLedgerRegister {
				m, why := pipeline.ResolveRegisterMode(ctx, s.cfg, s.client)
				fmt.Printf("           流水登记模式 = %s（%s）\n", m, why)
			}
		}
	}

	// 启动 worker（**单个**：串行处理，避免 manifest.jsonl 与飞书写入竞争）
	go s.worker(ctx)

	// ★ 补漏扫描（断网/断电/重启期间漏掉的事件不会补推，只能主动扫）：
	//   启动时先扫一遍，此后每 10 分钟一次，把"该做还没做"的实例补进队列。
	go s.catchUpLoop(ctx)

	// 启动后台备份
	//
	// 为什么必须有：**本地库是"唯一性"的权威**（飞书没有任何唯一索引）。
	// 库一丢，"防重复报销"就失效 —— 同一张发票能再次进库。
	// 实测教训：测试期间反复重建库，导致同一张发票被提交三次都没拦住。
	go s.backupLoop(ctx)

	// ★ 单实例守卫：长连接不广播，两个 serve 会让事件被随机分走。
	//   在建立连接**之前**抢占 pidfile，抢不到就不启动。
	if err := acquirePidfile(*pidfilePath, *forceStart); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ 拒绝启动：%v\n", err)
		os.Exit(1)
	}
	defer releasePidfile(*pidfilePath)
	if *pidfilePath != "" {
		fmt.Printf("pidfile   : %s（PID %d）\n", *pidfilePath, os.Getpid())
	}

	// 长连接 + 事件分发
	dispatcher := newDispatcher(s)
	wsClient := larkws.NewClient(cfg.Feishu.AppID, cfg.Feishu.AppSecret,
		larkws.WithEventHandler(dispatcher),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithAutoReconnect(true),
	)

	fmt.Println("\n正在建立长连接…（无需公网 IP；长连接不广播，故必须单实例）")
	runErr := make(chan error, 1)
	go func() { runErr <- wsClient.Start(ctx) }()

	select {
	case <-ctx.Done():
		fmt.Println("\n收到退出信号，正在关闭…")
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "\n✗ 长连接结束: %v\n", err)
		}
	}

	// 给 worker 一点时间排空队列
	time.Sleep(500 * time.Millisecond)
	s.printSummary()
}

// printSummary 打印本次运行的事件统计。
func (s *service) printSummary() {
	fmt.Println("\n── 本次运行统计 ──")
	fmt.Printf("  收到事件   : %d\n", s.received.Load())
	fmt.Printf("  重复（幂等）: %d\n", s.dup.Load())
	fmt.Printf("  忽略       : %d\n", s.ignored.Load())
	fmt.Printf("  已处理实例 : %d\n", s.processed.Load())
	fmt.Printf("  处理失败   : %d\n", s.failed.Load())
}

// handleInstanceEvent 是事件回调。
//
// ★ 必须 3 秒内返回 —— 这里只做"落库 + 入队"，不做下载/识别。
func (s *service) handleInstanceEvent(ctx context.Context, ev *larkapproval.P2InstanceStatusChangedV4) error {
	if ev == nil || ev.Event == nil {
		return nil
	}
	e := ev.Event

	eventID := ""
	if ev.EventV2Base != nil && ev.EventV2Base.Header != nil {
		eventID = ev.EventV2Base.Header.EventID
	}
	_ = eventID
	instance := deref(e.InstanceCode)
	approvalCode := deref(e.ApprovalCode)
	status := deref(e.Status)

	s.received.Add(1)

	return s.handleInstanceFields(ctx, eventID, "approval_instance",
		approvalCode, instance, status, deref(e.OperateTime))
}

// v1Event 是 v1.0 协议事件的外层结构。
//
// 与 v2 的两点差异（重要）：
//   - **幂等键是顶层 `uuid`**，不是 header.event_id；
//   - 业务字段平铺在 `event` 对象里（v2 在 header + event 两层）。
type v1Event struct {
	Schema string `json:"schema"`
	UUID   string `json:"uuid"`
	Type   string `json:"type"`
	Event  struct {
		Type         string `json:"type"`
		ApprovalCode string `json:"approval_code"`
		InstanceCode string `json:"instance_code"`
		TaskID       string `json:"task_id"`
		Status       string `json:"status"`
		OperateTime  string `json:"operate_time"`
	} `json:"event"`
}

// handleV1Event 处理 v1.0 协议事件（原始 JSON 自己解析）。
//
// 只有 approval_instance 参与主流程；approval_task / approval_cc 收下留痕即可
// —— 但**必须返回 nil**，否则会被当作失败回调并触发飞书重推。
func (s *service) handleV1Event(ctx context.Context, eventType string, req *larkevent.EventReq) error {
	var ev v1Event
	if err := json.Unmarshal(req.Body, &ev); err != nil {
		fmt.Printf("  ⚠ %s 解析失败: %v\n", eventType, err)
		return nil
	}
	// v1 用 uuid 做幂等键
	id := ev.UUID
	if id == "" {
		id = ev.Event.InstanceCode + "_" + eventType + "_" + ev.Event.OperateTime
	}

	// 防御：解析不出关键字段时把原始 JSON 打出来，便于核对 v1 结构
	if ev.Event.InstanceCode == "" {
		fmt.Printf("  ⚠ %s 未解析出 instance_code，原始 JSON: %s\n",
			eventType, truncateStr(string(req.Body), 400))
		return nil
	}

	if eventType != "approval_instance" {
		return s.handleOtherEvent(ctx, eventType, nil,
			ev.Event.ApprovalCode, ev.Event.InstanceCode, ev.Event.Status, ev.Event.OperateTime)
	}
	return s.handleInstanceFields(ctx, id, eventType,
		ev.Event.ApprovalCode, ev.Event.InstanceCode, ev.Event.Status, ev.Event.OperateTime)
}

// handleInstanceFields 是事件处理的公共主体（v1 与 v2 共用）。
func (s *service) handleInstanceFields(ctx context.Context, eventID, eventType,
	approvalCode, instance, status, operateTime string) error {

	s.received.Add(1)

	// ① 只处理配置里绑定的审批定义；按角色路由（发票收集 / 采购）
	role := s.roleFor(approvalCode)
	if role == "" {
		s.ignored.Add(1)
		fmt.Printf("  − 跳过其它审批定义 %s（实例 %s）\n", short(approvalCode), short(instance))
		_, _, _ = s.db.RecordEvent(ctx, store.Event{
			EventID: eventID, EventType: eventType,
			ApprovalCode: approvalCode, InstanceCode: instance, Status: status,
		}, store.EventIgnored, "非本项目的审批定义")
		return nil
	}
	if instance == "" {
		s.ignored.Add(1)
		return nil
	}

	// ② 幂等
	res, first, err := s.db.RecordEvent(ctx, store.Event{
		EventID: eventID, EventType: eventType,
		ApprovalCode: approvalCode, InstanceCode: instance,
		Status: status, OperateTime: operateTime,
	}, store.EventAccepted, "")
	if err != nil {
		s.failed.Add(1)
		fmt.Printf("  ✗ 事件落库失败: %v\n", err)
		return nil
	}
	if !first || res == store.EventDuplicate {
		s.dup.Add(1)
		fmt.Printf("  ↺ 重复事件（幂等跳过） event=%s 实例=%s\n", short(eventID), short(instance))
		return nil
	}

	// ③ 状态机（乱序保护）
	tr, err := s.db.ApplyState(ctx, instance, status, "feishu", "事件 "+short(eventID))
	if err != nil {
		fmt.Printf("  ⚠ 状态跃迁失败: %v\n", err)
	} else if tr.Applied {
		fmt.Printf("  → 状态 %s%s（实例 %s）\n",
			prefixIf(tr.From != "", tr.From+" → "), tr.To, short(instance))
	} else {
		fmt.Printf("  ⊘ 状态未变: %s（实例 %s）\n", tr.Reason, short(instance))
	}

	// ④ 入队（队列满则丢弃，交给下一轮补漏扫描重试 —— 见 catchup.go）
	if !s.enqueue(job{InstanceCode: instance, EventID: eventID, Status: status, Role: role}) {
		fmt.Printf("     （也可手动补跑：go run ./cmd/extract -instance %s）\n", instance)
	}
	return nil
}

// markEvent 回填事件处理结果。
//
// 补漏扫描（catchup.go）构造的任务没有 event_id（不是事件驱动的），
// 空 id 直接跳过 —— 否则每个补漏任务都会刷一条"留痕失败"。
func (s *service) markEvent(ctx context.Context, eventID string, r store.EventResult, detail string) {
	if eventID == "" {
		return
	}
	_ = s.db.UpdateEventResult(ctx, eventID, r, detail)
}

// roleFor 按 approval_code 找到配置里的角色；找不到返回空（=不是我们的审批）。
func (s *service) roleFor(approvalCode string) string {
	for _, a := range s.cfg.Feishu.Approvals {
		if a.Code != "" && a.Code == approvalCode {
			return a.Role
		}
	}
	if s.cfg.Feishu.ApprovalCode != "" && approvalCode == s.cfg.Feishu.ApprovalCode {
		return config.RoleInvoiceCollect
	}
	return ""
}

// backupLoop 启动时备份一次，此后每 backupEvery 一次。
func (s *service) backupLoop(ctx context.Context) {
	const backupEvery = 24 * time.Hour

	do := func() {
		path, err := s.db.Backup(ctx, s.cfg.Paths.BackupDir, s.cfg.Paths.BackupKeep)
		if err != nil {
			fmt.Printf("  ⚠ 备份失败: %v\n", err)
			return
		}
		fmt.Printf("  ✓ 已备份本地库 → %s\n", path)
	}
	do()

	t := time.NewTicker(backupEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			do()
		}
	}
}

// worker 串行消费队列。
//
// 为什么单实例：manifest.jsonl 是每次运行整体重写的，
// 并发处理会互相覆盖；且飞书写入本来就要求串行（同表禁止并发写）。
func (s *service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.queue:
			s.process(ctx, j)
		}
	}
}

// process 处理一个实例：已有数据就跳过，否则跑完整链路。
func (s *service) process(ctx context.Context, j job) {
	if s.dryRun {
		fmt.Printf("  [dry-run] 将处理实例 %s（role=%s）\n", short(j.InstanceCode), j.Role)
		return
	}

	// ★ 采购链路：只在「已通过」时动手（写流水 + 代建发票单）
	if j.Role == config.RolePurchase {
		if !strings.EqualFold(j.Status, "APPROVED") {
			fmt.Printf("  − 采购实例 %s 状态 %s，非已通过 → 不处理\n",
				short(j.InstanceCode), store.StateWord(j.Status))
			return
		}
		if err := pipeline.RunPurchase(ctx, pipeline.PurchaseOptions{
			CfgPath: s.cfg.Path, Instance: j.InstanceCode,
		}); err != nil {
			s.failed.Add(1)
			s.markEvent(ctx, j.EventID, store.EventError, err.Error())
			fmt.Printf("  ✗ 采购处理失败: %v\n", err)
			return
		}
		s.processed.Add(1)
		s.markEvent(ctx, j.EventID, store.EventAccepted, "采购处理完成")
		fmt.Printf("  ✓ 采购实例 %s 处理完成\n", short(j.InstanceCode))
		return
	}

	// ★ 流水登记链路：登记单通过 → 用登记数据**覆盖**流水行（用户：以登记数据为准），
	//   并在该采购所有明细都登记完成后给提交人开「27发票收集」。
	//   ★ 必须放在"退回剔除"之前：登记单的状态机与发票单完全不同（无审批人、提交即通过）。
	if j.Role == config.RoleLedgerRegister {
		if !strings.EqualFold(j.Status, "APPROVED") && store.StateWord(j.Status) != "已通过" {
			fmt.Printf("  − 流水登记 %s 状态 %s，非已通过 → 不处理\n",
				short(j.InstanceCode), store.StateWord(j.Status))
			return
		}
		if err := pipeline.RunFlowRegister(ctx, pipeline.FlowRegisterOptions{
			CfgPath: s.cfg.Path, Instance: j.InstanceCode,
		}); err != nil {
			s.failed.Add(1)
			s.markEvent(ctx, j.EventID, store.EventError, err.Error())
			fmt.Printf("  ✗ 流水登记处理失败: %v\n", err)
			return
		}
		s.processed.Add(1)
		s.markEvent(ctx, j.EventID, store.EventAccepted, "流水登记已覆盖流水行")
		fmt.Printf("  ✓ 流水登记实例 %s 处理完成\n", short(j.InstanceCode))
		return
	}

	// ★ 退回/拒绝/撤回类状态 → 把该单从本地库整体剔除。
	//
	// 需求：「被退回的就剔除掉」——原来那个多维表格只记录在审与通过的原数据。
	//
	// 这一步必须在"已处理过 → 跳过"**之前**：一单通常是先以"审批中"被处理入库，
	// 之后才被驳回；若先撞上"已处理过"就直接 return，退回永远清不掉，
	// 而且它占用的发票号码会一直锁着，队员改正后重交会被误判成重复报销。
	if store.IsRejectedState(j.Status) {
		if err := s.db.DeleteInstance(ctx, j.InstanceCode); err != nil {
			fmt.Printf("  ✗ 剔除失败: %v\n", err)
			s.failed.Add(1)
			return
		}
		// 本地库清了还不够：核对表里的行也得删，否则被退回的单会一直挂在
		// 人工待办里，而且它占的那张发票看起来仍"在审"。
		if n, err := pipeline.PurgeInstance(ctx, s.cfg, j.InstanceCode); err != nil {
			fmt.Printf("  ⚠ 本地已剔除，但清理核对表失败（可稍后重跑）: %v\n", err)
		} else if n > 0 {
			fmt.Printf("  ⊖ 已从「报销核对」删除 %d 行\n", n)
		}
		fmt.Printf("  ⊖ 实例 %s 状态为「%s」→ 已从本地库剔除（释放其占用的发票号码）\n",
			short(j.InstanceCode), store.StateWord(j.Status))
		return
	}

	// 是否已抽取入库
	hasSub := false
	if e, err := s.db.HasSubmission(ctx, j.InstanceCode); err == nil {
		hasSub = e
	}
	approved := strings.EqualFold(j.Status, "APPROVED") || store.StateWord(j.Status) == "已通过"

	// ★ 审批「已通过」= 新状态机的终点（docs/30-review/33 §4）：
	//   把该实例所有行标为「通过」并归档。**不再**在每次新审批时扫全表。
	if approved {
		if !hasSub {
			// 少见但可能：只收到 APPROVED（漏了 PENDING）。先补齐抽取+落表。
			if err := s.ingest(ctx, j); err != nil {
				return
			}
		}
		if err := pipeline.FinalizeApproved(ctx, s.cfg, j.InstanceCode); err != nil {
			s.failed.Add(1)
			fmt.Printf("  ⚠ 审批通过后的回写/归档失败: %v\n", err)
			return
		}
		s.processed.Add(1)
		s.markEvent(ctx, j.EventID, store.EventAccepted, "审批通过，已归档")
		fmt.Printf("  ✓ 实例 %s 审批通过处理完成\n", short(j.InstanceCode))
		return
	}

	// 已经处理过（本地库里有该实例）→ 不重复下载与识别
	if hasSub {
		fmt.Printf("  = 实例 %s 已处理过，跳过\n", short(j.InstanceCode))
		return
	}

	fmt.Printf("  ▶ 处理实例 %s（%s）…\n", short(j.InstanceCode), store.StateWord(j.Status))
	if err := s.ingest(ctx, j); err != nil {
		return
	}
	// 通知"有问题"的行（存疑/缺件）
	if err := pipeline.RunNotify(pipeline.NotifyOptions{CfgPath: s.cfg.Path, MaxSend: 3}); err != nil {
		fmt.Printf("  ⚠ 通知失败: %v\n", err)
	}
	// ★ 全部行一致 → 服务自动同意审批；任一行有问题 → 什么都不做，等人处理
	if err := pipeline.AutoApproveIfClean(ctx, s.cfg, j.InstanceCode); err != nil {
		fmt.Printf("  ⚠ 自动同意审批失败: %v\n", err)
	}

	s.processed.Add(1)
	s.markEvent(ctx, j.EventID, store.EventAccepted, "处理完成")
	fmt.Printf("  ✓ 实例 %s 处理完成\n", short(j.InstanceCode))
}

// ingest 抽取 + 落表（不归档、不决定审批）。失败时记 failed。
func (s *service) ingest(ctx context.Context, j job) error {
	if err := pipeline.Run(pipeline.Options{
		CfgPath:  s.cfg.Path,
		Instance: j.InstanceCode,
		Limit:    1,
		OutDir:   "data/extract",
		Quiet:    true,
	}); err != nil {
		s.failed.Add(1)
		s.markEvent(ctx, j.EventID, store.EventError, err.Error())
		fmt.Printf("  ✗ 抽取失败: %v\n", err)
		return err
	}
	if err := pipeline.RunSync(pipeline.SyncOptions{
		CfgPath: s.cfg.Path,
		Only:    j.InstanceCode,
		Update:  true,
	}); err != nil {
		s.failed.Add(1)
		fmt.Printf("  ⚠ 落表失败（本地已入库，可稍后重跑 sync）: %v\n", err)
	}
	return nil
}

// handleOtherEvent 收下"我们不参与处理"的事件：留痕后返回 nil。
//
// 为什么不直接忽略：这些事件在控制台被订阅了，飞书会真的推过来。
// 若没有处理器，SDK 会返回错误，等于把正常投递变成失败回调。
// 收下 + 返回 nil 才是正确的"确认收到"。
func (s *service) handleOtherEvent(ctx context.Context, eventType string, base *larkevent.EventV2Base,
	approvalCode, instanceCode, status, operateTime string) error {

	eventID := ""
	if base != nil && base.Header != nil {
		eventID = base.Header.EventID
	}
	// 非本项目的审批定义直接忽略（连痕迹都不留）
	if s.cfg.Feishu.ApprovalCode != "" && approvalCode != "" &&
		approvalCode != s.cfg.Feishu.ApprovalCode {
		return nil
	}

	// ★ 有些事件（实测 approval_task 的 v2 事件）**不带 event_id**，而留痕表用
	//   event_id 做幂等键 —— 硬写只会刷一行"留痕失败"，噪音盖住真问题。直接跳过。
	if eventID == "" {
		return nil
	}
	res, first, err := s.db.RecordEvent(ctx, store.Event{
		EventID: eventID, EventType: eventType,
		ApprovalCode: approvalCode, InstanceCode: instanceCode,
		Status: status, OperateTime: operateTime,
	}, store.EventIgnored, "仅留痕，不参与处理")
	if err != nil {
		fmt.Printf("  ⚠ %s 留痕失败: %v\n", eventType, err)
		return nil
	}
	if first {
		fmt.Printf("  · %s 收到并留痕（实例 %s，状态 %s）\n", eventType, short(instanceCode), status)
	}
	_ = res
	return nil
}

// ── 小工具 ────────────────────────────────────────────────────

func newDispatcher(s *service) *larkdispatcher.EventDispatcher {
	d := larkdispatcher.NewEventDispatcher("", "")
	d.OnP2InstanceStatusChangedV4(func(ctx context.Context, ev *larkapproval.P2InstanceStatusChangedV4) error {
		return s.handleInstanceEvent(ctx, ev)
	})
	// ★ 控制台订阅了几个事件，就必须给几个注册处理器。
	//   否则 SDK 会打印 "event type: xxx, not found handler" 并**返回错误**，
	//   等于把"我们不关心的事件"变成了失败回调（可能触发飞书重推）。
	//   这两个我们**不参与处理**，但要收下、留痕、返回 nil。
	d.OnP2TaskStatusChangedV4(func(ctx context.Context, ev *larkapproval.P2TaskStatusChangedV4) error {
		var ac, ic, st, ot string
		if e := ev.Event; e != nil {
			ac, ic, st, ot = deref(e.ApprovalCode), deref(e.InstanceCode),
				deref(e.Status), deref(e.OperateTime)
		}
		return s.handleOtherEvent(ctx, "approval_task", ev.EventV2Base, ac, ic, st, ot)
	})
	d.OnP2ApprovalUpdatedV4(func(ctx context.Context, ev *larkapproval.P2ApprovalUpdatedV4) error {
		return s.handleOtherEvent(ctx, "approval_updated", ev.EventV2Base, "", "", "定义已更新", "")
	})

	// ★ v1.0 旧协议的事件：SDK v3 **没有内置处理器**（它只有 v2 的
	//   approval.instance.status_changed_v4 等）。若控制台订阅的是 v1.0
	//   （事件名 approval_instance / approval_task / approval_cc），
	//   就必须用 OnCustomizedEvent 自己接住，否则 SDK 会报
	//   "event type: approval_task, not found handler" 并把它当成失败回调。
	for _, et := range []string{"approval_instance", "approval_task", "approval_cc"} {
		eventType := et
		d.OnCustomizedEvent(eventType, func(ctx context.Context, req *larkevent.EventReq) error {
			return s.handleV1Event(ctx, eventType, req)
		})
	}
	return d
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func prefixIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

// 保留 lark 包引用：后续加"消息卡片按钮"回调时用得到（卡片回调支持长连接）。
var _ = lark.NewClient

// runTask 跑一次性运维任务。
//
// 为什么放在 serve 里：机器人上**没有 Go**，只有这个编译好的二进制。
// 没有这条路径，任何回填（分组规则改了、元信息列后加）都只能靠
// "把库拷回来改完再拷回去"，既麻烦又容易出错。
func runTask(task, cfgPath string, force, dry bool) error {
	switch task {
	case "refresh-meta":
		return pipeline.RefreshMeta(cfgPath, dry)
	case "regroup":
		return pipeline.Regroup(cfgPath, force, dry)
	case "sync":
		return pipeline.RunSync(pipeline.SyncOptions{CfgPath: cfgPath, DryRun: dry, Update: true})
	case "archive":
		return pipeline.RunArchive(pipeline.ArchiveOptions{CfgPath: cfgPath, DryRun: dry})
	case "notify":
		return pipeline.RunNotify(pipeline.NotifyOptions{CfgPath: cfgPath, DryRun: dry, MaxSend: 5})
	case "purge-all":
		return pipeline.PurgeAll(cfgPath, dry)
	default:
		return fmt.Errorf("未知任务 %q（可选：refresh-meta | regroup | sync | archive | notify | purge-all）", task)
	}
}
