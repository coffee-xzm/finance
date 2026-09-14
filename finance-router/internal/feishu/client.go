// Package feishu 是飞书开放平台的最小只读客户端。
//
// 只实现 GET：tenant_access_token 获取、审批定义查询、审批实例列表、审批实例详情。
// 刻意不引入官方 SDK —— 侦察阶段只需要三个接口，少一个依赖少一份不确定性。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL    string
	appID      string
	appSecret  string
	httpClient *http.Client

	mu         sync.Mutex
	token      string
	tokenExpAt time.Time
	lastCall   time.Time // 上一次请求时间，用于客户端限速
	minGap     time.Duration
}

func NewClient(baseURL, appID, appSecret string) *Client {
	return &Client{
		baseURL:   baseURL,
		appID:     appID,
		appSecret: appSecret,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		// 飞书审批接口对连续请求很敏感（实测 99991400 request trigger
		// frequency limit 会立刻触发），默认在客户端做保守限速。
		minGap: 300 * time.Millisecond,
	}
}

// SetMinGap 调整请求最小间隔。
func (c *Client) SetMinGap(d time.Duration) { c.minGap = d }

// throttle 保证两次请求之间至少间隔 minGap。
func (c *Client) throttle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.minGap > 0 {
		if wait := c.minGap - time.Since(c.lastCall); wait > 0 {
			time.Sleep(wait)
		}
	}
	c.lastCall = time.Now()
}

// isRetryable 判断错误码是否值得重试。
// 99991400 = request trigger frequency limit（限流）
// 9499 / 1061045 等亦为限流类；这里保守地只认已实测到的那个。
func isRetryable(code int) bool {
	switch code {
	case 99991400, 9499, 429:
		return true
	}
	return false
}

// apiEnvelope 是飞书统一的响应信封。Data 保留原始字节，交给各接口自行解析。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// TenantAccessToken 获取并缓存 tenant_access_token（有效期 2 小时，提前 5 分钟刷新）。
func (c *Client) TenantAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.tokenExpAt) {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]string{
		"app_id":     c.appID,
		"app_secret": c.appSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("获取 tenant_access_token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析 token 响应: %w (原始: %s)", err, truncate(raw, 300))
	}
	if out.Code != 0 {
		return "", fmt.Errorf("获取 tenant_access_token 失败: code=%d msg=%s", out.Code, out.Msg)
	}
	if out.TenantAccessToken == "" {
		return "", fmt.Errorf("tenant_access_token 为空 (原始: %s)", truncate(raw, 300))
	}

	ttl := time.Duration(out.Expire) * time.Second
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	c.mu.Lock()
	c.token = out.TenantAccessToken
	c.tokenExpAt = time.Now().Add(ttl - 5*time.Minute)
	c.mu.Unlock()
	return out.TenantAccessToken, nil
}

// doWithRetryT 是 doWithRetry 的泛型版本：把 data 段交给 extract 解析。
func doWithRetryT[T any](c *Client, ctx context.Context, method, path string,
	build func() (*http.Request, error), extract func(json.RawMessage) (T, error)) (T, error) {
	var zero T
	raw, err := c.doWithRetry(ctx, method, path, build)
	if err != nil {
		return zero, err
	}
	return extract(raw)
}

// doWithRetry 执行一次请求并在限流时退避重试。
// build 每次调用都要重新构造请求（body 不可复用）。
func (c *Client) doWithRetry(ctx context.Context, method, path string,
	build func() (*http.Request, error)) (json.RawMessage, error) {

	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 指数退避：1.5s, 3s, 6s —— 限流窗口通常很短
			backoff := time.Duration(1500*(1<<(attempt-1))) * time.Millisecond
			fmt.Fprintf(os.Stderr, "      ⏳ 限流退避 %v 后重试 (%d/%d)\n", backoff, attempt, maxAttempts-1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		c.throttle()

		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s %s: %w", method, path, err)
			continue // 网络类错误也重试
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}

		var env apiEnvelope
		if jerr := json.Unmarshal(raw, &env); jerr != nil {
			return nil, fmt.Errorf("%s %s 响应非 JSON (HTTP %d): %s",
				method, path, resp.StatusCode, truncate(raw, 300))
		}
		if env.Code == 0 {
			return env.Data, nil
		}
		err = fmt.Errorf("%s %s 失败: HTTP %d code=%d msg=%s",
			method, path, resp.StatusCode, env.Code, env.Msg)
		if !isRetryable(env.Code) {
			return nil, err // 不可重试的错误立刻返回，避免把无效请求打成费用/配额
		}
		lastErr = err
	}
	return nil, fmt.Errorf("重试 %d 次后仍失败: %w", maxAttempts, lastErr)
}

// get 发起带鉴权的 GET 请求，返回 data 段的原始字节。
func (c *Client) get(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return c.doWithRetry(ctx, http.MethodGet, path, func() (*http.Request, error) {
		token, err := c.TenantAccessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		return req, nil
	})
}

// ApprovalDefinition 审批定义。
type ApprovalDefinition struct {
	ApprovalName string `json:"approval_name"`
	Status       string `json:"status"`
	Form         string `json:"form"`      // JSON 字符串：控件数组
	NodeList     []Node `json:"node_list"` // 流程节点
}

type Node struct {
	Name                string `json:"name"`
	NodeType            string `json:"node_type"`
	NeedApprover        bool   `json:"need_approver"`
	ApproverChosenMulti bool   `json:"approver_chosen_multi"`
}

func (c *Client) GetApprovalDefinition(ctx context.Context, approvalCode string) (*ApprovalDefinition, error) {
	data, err := c.get(ctx, "/approval/v4/approvals/"+url.PathEscape(approvalCode), nil)
	if err != nil {
		return nil, err
	}
	var def ApprovalDefinition
	if err := json.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("解析审批定义: %w", err)
	}
	return &def, nil
}

// ApprovalSummary 是审批定义列表中的一项（不含 form，列表接口不返回）。
type ApprovalSummary struct {
	ApprovalCode string `json:"approval_code"`
	ApprovalName string `json:"approval_name"`
	Status       string `json:"status"` // ACTIVE / INACTIVE / DELETED …
}

type approvalListData struct {
	ApprovalList []ApprovalSummary `json:"approval_list"`
	PageToken    string            `json:"page_token"`
	HasMore      bool              `json:"has_more"`
}

// ListApprovalDefinitions 尝试列举本应用可见的全部审批定义。
//
// ⚠️ 实测（2026-09-13）：`GET /approval/v4/approvals` 用 **tenant_access_token 会返回
// 99991663 Invalid access token**，而同一个 token 调 `/approvals/{code}` 正常。
// 飞书 SDK 的 approvalV4 域里也没有"列举审批定义"这个方法；唯一带搜索语义的
// `search_launchable` 明确要求 **user_access_token**。
//
// 结论：**常驻服务（只有 tenant token）无法枚举审批定义**。
// 请改用 DiscoverApprovals（按用户查实例，从中提取 approval_code + name）。
// 这个函数保留是为了在将来拿到 user token 时可直接接上。
func (c *Client) ListApprovalDefinitions(ctx context.Context) ([]ApprovalSummary, error) {
	var out []ApprovalSummary
	pageToken := ""
	for page := 0; page < 100; page++ {
		q := url.Values{}
		q.Set("page_size", "100")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		data, err := c.get(ctx, "/approval/v4/approvals", q)
		if err != nil {
			return nil, err
		}
		var d approvalListData
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("解析审批定义列表: %w", err)
		}
		out = append(out, d.ApprovalList...)
		if !d.HasMore || d.PageToken == "" {
			break
		}
		pageToken = d.PageToken
	}
	return out, nil
}

// InstanceSearchItem 是「查询实例列表」里的一条：实例 + 它所属的审批定义。
type InstanceSearchItem struct {
	Approval struct {
		Code       string `json:"code"`
		Name       string `json:"name"`
		IsExternal bool   `json:"is_external"`
	} `json:"approval"`
	Instance struct {
		Code      string `json:"code"`
		UserID    string `json:"user_id"`
		StartTime string `json:"start_time"`
		Status    string `json:"status"`
		SerialID  string `json:"serial_id"`
	} `json:"instance"`
}

type instanceQueryData struct {
	Count        int                  `json:"count"`
	InstanceList []InstanceSearchItem `json:"instance_list"`
	PageToken    string               `json:"page_token"`
	HasMore      bool                 `json:"has_more"`
}

// QueryInstances 调 POST /approval/v4/instances/query 查询实例列表。
//
// 权限：**approval:approval.list:readonly**（注意与 approval:approval:readonly 不是同一个）。
// user_id / approval_code / instance_code / instance_external_id / group_external_id
// **至少给一个**，否则 1390001。时间跨度**不得大于 30 天**。
//
// 每条返回值都带 approval.code + approval.name —— 这正是我们用来"发现租户里到底
// 有哪些审批表单"的依据（因为 tenant token 没有列举定义的接口）。
func (c *Client) QueryInstances(ctx context.Context, req InstanceQueryRequest) ([]InstanceSearchItem, error) {
	var out []InstanceSearchItem
	pageToken := ""
	for page := 0; page < 100; page++ {
		body := map[string]any{}
		if req.UserID != "" {
			body["user_id"] = req.UserID
		}
		if req.ApprovalCode != "" {
			body["approval_code"] = req.ApprovalCode
		}
		if req.Status != "" {
			body["instance_status"] = req.Status
		}
		if !req.From.IsZero() {
			body["instance_start_time_from"] = fmt.Sprint(req.From.UnixMilli())
			body["instance_start_time_to"] = fmt.Sprint(req.To.UnixMilli())
		}
		if pageToken != "" {
			body["page_token"] = pageToken
		}
		body["page_size"] = 100

		q := url.Values{}
		q.Set("page_size", "100")
		if req.UserIDType != "" {
			q.Set("user_id_type", req.UserIDType)
		}
		data, err := c.post(ctx,
			"/approval/v4/instances/query?"+q.Encode(), body)
		if err != nil {
			return nil, err
		}
		var d instanceQueryData
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("解析实例列表: %w", err)
		}
		out = append(out, d.InstanceList...)
		if !d.HasMore || d.PageToken == "" {
			break
		}
		pageToken = d.PageToken
	}
	return out, nil
}

// InstanceQueryRequest 是 QueryInstances 的入参。
type InstanceQueryRequest struct {
	UserID       string
	ApprovalCode string
	Status       string // PENDING / APPROVED / REJECT / ALL …
	From, To     time.Time
	UserIDType   string // user_id（本应用用这个，见 99992361 跨应用 open_id 问题）
}

// instanceListData 是 GET /approval/v4/instances 的 data 段。
type instanceListData struct {
	InstanceCodeList []string `json:"instance_code_list"`
	PageToken        string   `json:"page_token"`
	HasMore          bool     `json:"has_more"`
}

// listWindow 是接口允许的单次查询时间跨度。
//
// ⚠ 官方注意事项原文：「单次查询时间范围不要超过 10 小时」。
// 因此拉全量历史必须按 ≤10 小时切片循环，不能一次传一个月。
const listWindow = 10 * time.Hour

// ListInstancesInRange 拉取 [from, to) 内某审批定义的全部实例 code。
//
// 关键：approval_code / start_time / end_time 三个参数**都是必填**（官方参数表标注"是"）。
// 实现会自动把区间切成 ≤10 小时的窗口。
func (c *Client) ListInstancesInRange(ctx context.Context, approvalCode string, from, to time.Time, maxWindows int) ([]string, []json.RawMessage, error) {
	if !to.After(from) {
		return nil, nil, fmt.Errorf("时间区间非法: from=%s to=%s", from, to)
	}
	var (
		codes     []string
		rawPages  []json.RawMessage
		windowNum int
	)
	for cur := from; cur.Before(to); {
		end := cur.Add(listWindow)
		if end.After(to) {
			end = to
		}
		windowNum++
		if maxWindows > 0 && windowNum > maxWindows {
			return codes, rawPages, fmt.Errorf("时间窗口数超过上限 %d（区间 %s ~ %s）—— "+
				"请缩小 -from/-to 区间", maxWindows, from.Format("2006-01-02"), to.Format("2006-01-02"))
		}

		pageToken := ""
		for page := 0; page < 50; page++ {
			q := url.Values{}
			q.Set("approval_code", approvalCode)
			q.Set("start_time", fmt.Sprint(cur.UnixMilli()))
			q.Set("end_time", fmt.Sprint(end.UnixMilli()))
			q.Set("page_size", "100")
			if pageToken != "" {
				q.Set("page_token", pageToken)
			}
			data, err := c.get(ctx, "/approval/v4/instances", q)
			if err != nil {
				return codes, rawPages, err
			}
			rawPages = append(rawPages, data)
			var d instanceListData
			if err := json.Unmarshal(data, &d); err != nil {
				return codes, rawPages, fmt.Errorf("解析实例列表: %w", err)
			}
			codes = append(codes, d.InstanceCodeList...)
			if !d.HasMore || d.PageToken == "" {
				break
			}
			pageToken = d.PageToken
		}
		cur = end
	}
	return codes, rawPages, nil
}

// ── 按「用户任务」查询：**不需要 approval_code** 的另一条路径 ──────────
//
// POST /approval/v4/tasks/search
// 约束：user_id / approval_code / instance_code / instance_external_id / group_external_id
//       不能同时为空。即只传 user_id 合法。
// 权限：approval:approval:readonly（或 approval:approval.list:readonly）
// 返回里带 instance_code + approval_code + status + title → 可再逐条取详情。

type TaskSearchRequest struct {
	UserID         string   `json:"user_id,omitempty"`
	ApprovalCode   string   `json:"approval_code,omitempty"`
	InstanceCode   string   `json:"instance_code,omitempty"`
	TaskTitle      string   `json:"task_title,omitempty"`
	TaskStatus     string   `json:"task_status,omitempty"`
	TaskStatusList []string `json:"task_status_list,omitempty"`
	PageSize       int      `json:"page_size,omitempty"`
	PageToken      string   `json:"page_token,omitempty"`
}

type TaskSearchItem struct {
	ApprovalCode string `json:"approval_code"`
	InstanceCode string `json:"instance_code"`
	UserID       string `json:"user_id"`
	Status       string `json:"status"`
	Title        string `json:"title"`
}

type taskSearchData struct {
	Tasks     []TaskSearchItem `json:"tasks"`
	PageToken string           `json:"page_token"`
	HasMore   bool             `json:"has_more"`
}

// SearchTasks 按用户/定义搜索任务，自动翻页。
func (c *Client) SearchTasks(ctx context.Context, req TaskSearchRequest, userIDType string, maxPages int) ([]TaskSearchItem, []json.RawMessage, error) {
	if req.UserID == "" && req.ApprovalCode == "" && req.InstanceCode == "" {
		return nil, nil, fmt.Errorf("user_id / approval_code / instance_code 不能同时为空")
	}
	var (
		items    []TaskSearchItem
		rawPages []json.RawMessage
	)
	pageToken := req.PageToken
	for page := 0; page < maxPages; page++ {
		if req.PageSize == 0 {
			req.PageSize = 100
		}
		req.PageToken = pageToken
		if userIDType == "" {
			userIDType = "open_id"
		}
		path := "/approval/v4/tasks/search?user_id_type=" + url.QueryEscape(userIDType)
		data, err := c.post(ctx, path, req)
		if err != nil {
			return items, rawPages, err
		}
		rawPages = append(rawPages, data)
		var d taskSearchData
		if err := json.Unmarshal(data, &d); err != nil {
			return items, rawPages, fmt.Errorf("解析任务搜索: %w", err)
		}
		items = append(items, d.Tasks...)
		if !d.HasMore || d.PageToken == "" {
			break
		}
		pageToken = d.PageToken
	}
	return items, rawPages, nil
}

// post 发起带鉴权的 POST 请求，返回 data 段的原始字节。
func (c *Client) post(ctx context.Context, path string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.doWithRetry(ctx, http.MethodPost, path, func() (*http.Request, error) {
		token, err := c.TenantAccessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		return req, nil
	})
}

// InstanceDetail 审批实例详情。原始字节由调用方另行落盘。
type InstanceDetail struct {
	ApprovalCode string         `json:"approval_code"`
	ApprovalName string         `json:"approval_name"`
	InstanceCode string         `json:"instance_code"`
	DepartmentID string         `json:"department_id"` // 发起人部门 ID（需另查名称）
	Status       string         `json:"status"`
	StartTime    string         `json:"start_time"`
	EndTime      string         `json:"end_time"`
	Form         string         `json:"form"` // JSON 字符串
	Timeline     []TimelineItem `json:"timeline"`
	TaskList     []TaskItem     `json:"task_list"`
}

type TimelineItem struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type TaskItem struct {
	Status string `json:"status"`
}

func (c *Client) GetInstanceDetail(ctx context.Context, instanceCode string) (*InstanceDetail, json.RawMessage, error) {
	data, err := c.get(ctx, "/approval/v4/instances/"+url.PathEscape(instanceCode), nil)
	if err != nil {
		return nil, nil, err
	}
	var d InstanceDetail
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, data, fmt.Errorf("解析实例详情 %s: %w", instanceCode, err)
	}
	return &d, data, nil
}

// FormWidget 是审批表单里的一个控件实例值。
//
// 注意：飞书 form 是一个 JSON 字符串，其控件不一定有 id（可能只有 custom_id），
// 所以三个字段都要保留，展示时按 custom_id → id → name 的优先级取名。
type FormWidget struct {
	ID       string          `json:"id"`
	CustomID string          `json:"custom_id"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Value    json.RawMessage `json:"value"`
	Option   json.RawMessage `json:"option"`
}

// ParseForm 把 form 字符串解析为控件列表。
func ParseForm(form string) ([]FormWidget, error) {
	if form == "" {
		return nil, nil
	}
	var ws []FormWidget
	if err := json.Unmarshal([]byte(form), &ws); err != nil {
		return nil, fmt.Errorf("解析 form: %w", err)
	}
	return ws, nil
}

// Key 返回控件的展示键。
func (w FormWidget) Key() string {
	if w.CustomID != "" {
		return w.CustomID
	}
	if w.ID != "" {
		return w.ID
	}
	return w.Name
}

// AttachmentURLs 从附件控件的 value 里提取下载 URL。
//
// 【实测结构（2026-09-13，真实审批实例）】审批实例里 attachmentV2 / image 控件的
// value 是**字符串数组，元素是带 authcode 的下载直链**，形如：
//
//	["https://internal-api-drive-stream.feishu.cn/space/api/box/stream/download/authcode/?code=<base64>"]
//
// 要点：
//   - **没有 file_token 对象**（所以不要试图去 drive/v1/medias 下载，那个走不通）；
//   - 该 URL **直接 GET 即可，无需额外鉴权头**（authcode 自身即凭证，实测 HTTP 200）；
//   - authcode 解码含 `_ID:<id>_<起>_<止>_V3`，**有效期 24 小时** → 必须现读现用，
//     不能缓存 URL；缓存了也要重取实例详情。
func (w FormWidget) AttachmentURLs() []string {
	if len(w.Value) == 0 {
		return nil
	}
	// 注意：不要把同一个切片变量复用于两次 Unmarshal。
	// json.Unmarshal 失败时可能已把目标切片**部分填充**（例如把 [{"a":1}] 解到 []string
	// 会留下长度 1 的空字符串），复用同一个变量会导致"看起来有值、其实为空"的假阳性。
	var out []string

	// 形态 1：字符串数组（实测形态）
	var strs []string
	if err := json.Unmarshal(w.Value, &strs); err == nil {
		for _, s := range strs {
			if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	// 形态 2：对象数组兜底（[{url:...}] / [{file_token:...}]）
	var objs []map[string]any
	if err := json.Unmarshal(w.Value, &objs); err != nil {
		return nil
	}
	for _, m := range objs {
		for _, k := range []string{"url", "download_url", "file_token", "tmp_url"} {
			if s, ok := m[k].(string); ok && s != "" {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// IsAttachmentType 判断控件类型是否是附件/图片（可承载票据）。
func IsAttachmentType(t string) bool {
	switch t {
	case "attachmentV2", "attachment", "image", "imageV2":
		return true
	}
	return false
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(截断)"
}

// ── 多维表格只读自省（用于对齐表结构，不写任何数据）──────────────────
//
// 权限：base:table:read / base:field:read 或 bitable:app:readonly（只读即可）

type BitableTable struct {
	TableID  string `json:"table_id"`
	Name     string `json:"name"`
	Revision int    `json:"revision"`
}

type BitableField struct {
	FieldID     string          `json:"field_id"`
	FieldName   string          `json:"field_name"`
	Type        int             `json:"type"`
	UIType      string          `json:"ui_type"`
	IsPrimary   bool            `json:"is_primary"`
	Property    json.RawMessage `json:"property"`
	Description json.RawMessage `json:"description"`
}

type bitableListData[T any] struct {
	Items     []T    `json:"items"`
	PageToken string `json:"page_token"`
	HasMore   bool   `json:"has_more"`
}

// ListBitableTables 列出多维表格里的所有数据表。
func (c *Client) ListBitableTables(ctx context.Context, appToken string) ([]BitableTable, error) {
	var all []BitableTable
	pageToken := ""
	for page := 0; page < 50; page++ {
		q := url.Values{}
		q.Set("page_size", "100")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		data, err := c.get(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+"/tables", q)
		if err != nil {
			return all, err
		}
		var d bitableListData[BitableTable]
		if err := json.Unmarshal(data, &d); err != nil {
			return all, fmt.Errorf("解析数据表列表: %w", err)
		}
		all = append(all, d.Items...)
		if !d.HasMore || d.PageToken == "" {
			break
		}
		pageToken = d.PageToken
	}
	return all, nil
}

// ListBitableFields 列出某张数据表的全部字段。
func (c *Client) ListBitableFields(ctx context.Context, appToken, tableID string) ([]BitableField, error) {
	var all []BitableField
	pageToken := ""
	for page := 0; page < 50; page++ {
		q := url.Values{}
		q.Set("page_size", "100")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		data, err := c.get(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
			"/tables/"+url.PathEscape(tableID)+"/fields", q)
		if err != nil {
			return all, err
		}
		var d bitableListData[BitableField]
		if err := json.Unmarshal(data, &d); err != nil {
			return all, fmt.Errorf("解析字段列表: %w", err)
		}
		all = append(all, d.Items...)
		if !d.HasMore || d.PageToken == "" {
			break
		}
		pageToken = d.PageToken
	}
	return all, nil
}

// BitableRecord 是多维表格里的一条记录（只读）。
type BitableRecord struct {
	RecordID string                     `json:"record_id"`
	Fields   map[string]json.RawMessage `json:"fields"`
}

// ListBitableRecords 只读拉取若干条记录（用于确认字段真实取值形态）。
//
// 权限：bitable:app:readonly 即可。**不写任何数据。**
func (c *Client) ListBitableRecords(ctx context.Context, appToken, tableID string, limit int) ([]BitableRecord, error) {
	if limit <= 0 {
		limit = 2
	}
	q := url.Values{}
	q.Set("page_size", fmt.Sprint(limit))
	data, err := c.get(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
		"/tables/"+url.PathEscape(tableID)+"/records", q)
	if err != nil {
		return nil, err
	}
	var d bitableListData[BitableRecord]
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("解析记录列表: %w", err)
	}
	return d.Items, nil
}

// ── 知识库（Wiki）节点解析 ─────────────────────────────────────
//
// 知识库 URL 形如 https://xxx.feishu.cn/wiki/<node_token>，
// 其中 node_token 不是云文档 token，必须先解析出 obj_token 才能操作文档内容。
// 权限：wiki:node:read 或 wiki:wiki:readonly。

type WikiNode struct {
	SpaceID         string `json:"space_id"`
	NodeToken       string `json:"node_token"`
	ObjToken        string `json:"obj_token"`
	ObjType         string `json:"obj_type"`
	ParentNodeToken string `json:"parent_node_token"`
	Title           string `json:"title"`
}

// GetWikiNode 解析知识库节点，返回其真实云文档 token（obj_token）。
// objType 传空则按 wiki 类型查询。
func (c *Client) GetWikiNode(ctx context.Context, nodeToken, objType string) (*WikiNode, error) {
	q := url.Values{}
	q.Set("token", nodeToken)
	if objType == "" {
		objType = "wiki"
	}
	q.Set("obj_type", objType)
	data, err := c.get(ctx, "/wiki/v2/spaces/get_node", q)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Node WikiNode `json:"node"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("解析知识库节点: %w", err)
	}
	return &wrapper.Node, nil
}

// CreateWikiNode 在知识库下新建节点（如多维表格），返回新节点的信息。
// 权限：wiki:node:create 或 wiki:wiki（**写操作**，非只读期慎用）。
func (c *Client) CreateWikiNode(ctx context.Context, spaceID, objType, title string) (*WikiNode, error) {
	payload := map[string]any{
		"obj_type":  objType,  // bitable | docx | sheet | ...
		"node_type": "origin", // origin=实体, shortcut=快捷方式
	}
	if title != "" {
		payload["title"] = title
	}
	data, err := c.post(ctx, "/wiki/v2/spaces/"+url.PathEscape(spaceID)+"/nodes", payload)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Node WikiNode `json:"node"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("解析新建节点响应: %w", err)
	}
	return &wrapper.Node, nil
}

// ── 多维表格字段写入（★ 写操作，仅在明确授权时使用）──────────────
//
// 权限：base:field:create / base:field:update 或 bitable:app（非只读）。
// 新增字段仅接受 name/type/property 等；**不能设置公式表达式**（官方限制）。

// FieldSpec 是新增/更新字段的请求体。
type FieldSpec struct {
	FieldName string         `json:"field_name"`
	Type      int            `json:"type"`
	Property  map[string]any `json:"property,omitempty"`
}

// CreateBitableField 在指定表里新增一个字段。
func (c *Client) CreateBitableField(ctx context.Context, appToken, tableID string, spec FieldSpec) (string, error) {
	data, err := c.post(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
		"/tables/"+url.PathEscape(tableID)+"/fields", spec)
	if err != nil {
		return "", err
	}
	var wrapper struct {
		Field struct {
			FieldID   string `json:"field_id"`
			FieldName string `json:"field_name"`
		} `json:"field"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return "", fmt.Errorf("解析新增字段响应: %w", err)
	}
	return wrapper.Field.FieldID, nil
}

// UpdateBitableField 更新字段（改名/改类型/改选项）。用于重命名主字段。
func (c *Client) UpdateBitableField(ctx context.Context, appToken, tableID, fieldID string, spec FieldSpec) error {
	_, err := c.put(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
		"/tables/"+url.PathEscape(tableID)+"/fields/"+url.PathEscape(fieldID), spec)
	return err
}

// ListBitableViews 列出数据表的视图（只读）。
func (c *Client) ListBitableViews(ctx context.Context, appToken, tableID string) ([]map[string]any, error) {
	q := url.Values{}
	q.Set("page_size", "50")
	data, err := c.get(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
		"/tables/"+url.PathEscape(tableID)+"/views", q)
	if err != nil {
		return nil, err
	}
	var d bitableListData[map[string]any]
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return d.Items, nil
}

// put 发起带鉴权的 PUT 请求。
func (c *Client) put(ctx context.Context, path string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.doWithRetry(ctx, http.MethodPut, path, func() (*http.Request, error) {
		token, err := c.TenantAccessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		return req, nil
	})
}

// CreateBitableTable 在指定多维表格里新建一张数据表，返回 table_id。
//
// 权限：base:table:create 或 bitable:app（★ 写操作）。
func (c *Client) CreateBitableTable(ctx context.Context, appToken, name string) (string, error) {
	payload := map[string]any{
		"table": map[string]any{
			"name":              name,
			"default_view_name": "表格",
			"fields": []map[string]any{
				{"field_name": "文本", "type": 1},
			},
		},
	}
	data, err := c.post(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+"/tables", payload)
	if err != nil {
		return "", err
	}
	var wrapper struct {
		TableID string `json:"table_id"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return "", fmt.Errorf("解析新建数据表响应: %w", err)
	}
	if wrapper.TableID == "" {
		return "", fmt.Errorf("响应里没有 table_id: %s", truncate(data, 200))
	}
	return wrapper.TableID, nil
}

// SendTextMessage 发送纯文本单聊消息（用于通知管理员）。
//
// 权限：im:message 或 im:message:send_as_bot。
// 还需：开启机器人能力 + 把收件人加入应用可用范围。
func (c *Client) SendTextMessage(ctx context.Context, receiveIDType, receiveID, text string) error {
	payload := map[string]any{
		"receive_id": receiveID,
		"msg_type":   "text",
		"content":    `{"text":` + jsonString(text) + `}`,
	}
	q := url.Values{}
	q.Set("receive_id_type", receiveIDType)
	_, err := c.post(ctx, "/im/v1/messages?"+q.Encode(), payload)
	return err
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ── 多维表格记录写入（★ 写操作）────────────────────────────────
//
// 权限：base:record:create / base:record:update 或 bitable:app。
//
// ⚠️ 只读字段绝不能作为写入目标：公式(20)、查找引用(19)、自动编号(1005)、
//    创建时间(1001)、最后更新时间(1002)、创建人(1003)、修改人(1004)。

// CreateBitableRecord 新增一条记录，返回 record_id。
func (c *Client) CreateBitableRecord(ctx context.Context, appToken, tableID string, fields map[string]any) (string, error) {
	payload := map[string]any{"fields": fields}
	data, err := c.post(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
		"/tables/"+url.PathEscape(tableID)+"/records", payload)
	if err != nil {
		return "", err
	}
	var wrapper struct {
		Record struct {
			RecordID string `json:"record_id"`
		} `json:"record"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return "", fmt.Errorf("解析新增记录响应: %w", err)
	}
	return wrapper.Record.RecordID, nil
}

// UpdateBitableRecord 更新一条记录（只传要改的字段）。
func (c *Client) UpdateBitableRecord(ctx context.Context, appToken, tableID, recordID string, fields map[string]any) error {
	payload := map[string]any{"fields": fields}
	_, err := c.put(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
		"/tables/"+url.PathEscape(tableID)+"/records/"+url.PathEscape(recordID), payload)
	return err
}

// SearchBitableRecords 按条件搜索记录（只读）。
//
// 用于轮询"人工审核=通过 且 已归档=false"的行。
// filter 使用飞书 FilterInfo 语法：{"conjunction":"and","conditions":[{"field_name":..,"operator":"is","value":[..]}]}
func (c *Client) SearchBitableRecords(ctx context.Context, appToken, tableID string, filter map[string]any, pageSize int) ([]BitableRecord, error) {
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 100
	}
	payload := map[string]any{"page_size": pageSize}
	if filter != nil {
		payload["filter"] = filter
	}
	data, err := c.post(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
		"/tables/"+url.PathEscape(tableID)+"/records/search", payload)
	if err != nil {
		return nil, err
	}
	var d bitableListData[BitableRecord]
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("解析记录搜索结果: %w", err)
	}
	return d.Items, nil
}

// GetUserOpenID 用 user_id 换取**本应用有效**的 open_id。
//
// 为什么需要：飞书的 open_id 是**按应用隔离**的（同一用户在不同应用的 open_id 不同），
// 所以从别处拿到的 open_id 直接发消息会报 `99992361 open_id cross app`。
// 而 user_id 在租户内一致，可以用它换取本应用的 open_id。
//
// 权限：contact:user.base:readonly。
func (c *Client) GetUserOpenID(ctx context.Context, userID string) (string, error) {
	q := url.Values{}
	q.Set("user_id_type", "user_id")
	data, err := c.get(ctx, "/contact/v3/users/"+url.PathEscape(userID), q)
	if err != nil {
		return "", err
	}
	var wrapper struct {
		User struct {
			OpenID string `json:"open_id"`
			Name   string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return "", fmt.Errorf("解析用户信息: %w", err)
	}
	if wrapper.User.OpenID == "" {
		return "", fmt.Errorf("响应里没有 open_id: %s", truncate(data, 200))
	}
	return wrapper.User.OpenID, nil
}

// ── 素材上传（把票据图存进多维表格附件字段）──────────────────────
//
// 为什么走这条路而不是"超链接"：
//   审批附件的下载链接带 authcode，**只有 24 小时有效期**（实测解码
//   `_ID:..._1789281781:1789368181_V3`，起止相差 86400 秒），
//   存进表里第二天就点不开了。而多维表格附件是**长期有效且可内联预览**的。
//
// 权限：bitable:app（或 docs:document.media:upload / drive:drive）。
// 限制：单文件 ≤ 20 MB；**file_token 不可跨表复用**。

// UploadMedia 上传一个素材到指定多维表格，返回 file_token。
// parentType 取 bitable_image（图片）或 bitable_file（其它附件）。
func (c *Client) UploadMedia(ctx context.Context, appToken, parentType, fileName string, data []byte) (string, error) {
	if parentType == "" {
		parentType = "bitable_image"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{
		"file_name":   fileName,
		"parent_type": parentType,
		"parent_node": appToken,
		"size":        fmt.Sprint(len(data)),
	} {
		if err := mw.WriteField(k, v); err != nil {
			return "", err
		}
	}
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	return doWithRetryT(c, ctx, http.MethodPost, "/drive/v1/medias/upload_all",
		func() (*http.Request, error) {
			token, err := c.TenantAccessToken(ctx)
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				c.baseURL+"/drive/v1/medias/upload_all", bytes.NewReader(buf.Bytes()))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			return req, nil
		},
		func(data json.RawMessage) (string, error) {
			var out struct {
				FileToken string `json:"file_token"`
			}
			if err := json.Unmarshal(data, &out); err != nil {
				return "", fmt.Errorf("解析上传响应: %w", err)
			}
			if out.FileToken == "" {
				return "", fmt.Errorf("响应里没有 file_token: %s", truncate(data, 200))
			}
			return out.FileToken, nil
		})
}

// DeleteBitableTable 删除一张数据表（★ 破坏性操作）。
// 权限：base:table:delete 或 bitable:app。
func (c *Client) DeleteBitableTable(ctx context.Context, appToken, tableID string) error {
	_, err := c.doWithRetry(ctx, http.MethodDelete,
		"/bitable/v1/apps/"+url.PathEscape(appToken)+"/tables/"+url.PathEscape(tableID),
		func() (*http.Request, error) {
			token, err := c.TenantAccessToken(ctx)
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
				c.baseURL+"/bitable/v1/apps/"+url.PathEscape(appToken)+"/tables/"+url.PathEscape(tableID), nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			return req, nil
		})
	return err
}

// DeleteBitableRecord 删除一条记录（★ 破坏性操作，仅用于清理误写数据）。
func (c *Client) DeleteBitableRecord(ctx context.Context, appToken, tableID, recordID string) error {
	_, err := c.doWithRetry(ctx, http.MethodDelete,
		"/bitable/v1/apps/"+url.PathEscape(appToken)+"/tables/"+url.PathEscape(tableID)+
			"/records/"+url.PathEscape(recordID),
		func() (*http.Request, error) {
			token, err := c.TenantAccessToken(ctx)
			if err != nil {
				return nil, err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
				c.baseURL+"/bitable/v1/apps/"+url.PathEscape(appToken)+"/tables/"+
					url.PathEscape(tableID)+"/records/"+url.PathEscape(recordID), nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			return req, nil
		})
	return err
}

// DeleteBitableField 删除一个字段（★ 破坏性操作）。
// 权限：base:field:delete 或 bitable:app。
func (c *Client) DeleteBitableField(ctx context.Context, appToken, tableID, fieldID string) error {
	path := "/bitable/v1/apps/" + url.PathEscape(appToken) + "/tables/" +
		url.PathEscape(tableID) + "/fields/" + url.PathEscape(fieldID)
	_, err := c.doWithRetry(ctx, http.MethodDelete, path, func() (*http.Request, error) {
		token, err := c.TenantAccessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return req, nil
	})
	return err
}

// GetDepartmentName 把部门 ID 解析成部门名称。
//
// 为什么需要：审批实例里只给 `department_id`（ID，人看不懂），
// 而生产表里的「发起人部门」是审批连接器合成的人类可读名称。
// 权限：contact:contact.base:readonly（或 contact:department.base:readonly）。
// DeptInfo 是部门解析结果。
type DeptInfo struct {
	Name         string // 部门名；**可能为空**（缺 contact:department.base:readonly 字段权限）
	OpenDeptID   string // od-…，可用于在已知对照表里查名称
	HasNameField bool   // 响应里是否带 name 字段
}

// GetDepartment 按部门 ID 查询部门信息。
//
// ID 类型按前缀判断：open_department_id 固定以 `od-` 开头，其余按自定义 department_id。
// （实测审批实例给的是 "99a8a57fac21fd39"，无前缀 → 自定义 ID；
//
//	用错类型会报 99992357 "not a valid {open_department_id}"。）
//
// 注意：拿到部门对象但 **name 为空**是常见情况 —— name 是**字段级权限**，
// 需要 contact:department.base:readonly。此时仍返回 OpenDeptID，便于用已知对照表兜底。
func (c *Client) GetDepartment(ctx context.Context, departmentID string) (*DeptInfo, error) {
	order := []string{"department_id", "open_department_id"}
	if strings.HasPrefix(departmentID, "od-") {
		order = []string{"open_department_id", "department_id"}
	}

	var firstErr error
	for _, idType := range order {
		q := url.Values{}
		q.Set("department_id_type", idType)
		data, err := c.get(ctx, "/contact/v3/departments/"+url.PathEscape(departmentID), q)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// 响应形态：data.department.{name, open_department_id}
		var wrapper struct {
			Department struct {
				Name             string `json:"name"`
				OpenDepartmentID string `json:"open_department_id"`
			} `json:"department"`
			Data struct {
				Name             string `json:"name"`
				OpenDepartmentID string `json:"open_department_id"`
			} `json:"data"`
			Name             string `json:"name"`
			OpenDepartmentID string `json:"open_department_id"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		info := &DeptInfo{}
		for _, v := range []struct{ n, o string }{
			{wrapper.Department.Name, wrapper.Department.OpenDepartmentID},
			{wrapper.Data.Name, wrapper.Data.OpenDepartmentID},
			{wrapper.Name, wrapper.OpenDepartmentID},
		} {
			if v.n != "" {
				info.Name, info.HasNameField = v.n, true
			}
			if v.o != "" {
				info.OpenDeptID = v.o
			}
		}
		if info.Name != "" || info.OpenDeptID != "" {
			return info, nil
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("响应里既没有 name 也没有 open_department_id: %s", truncate(data, 200))
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("未能解析部门 %s", departmentID)
	}
	return nil, firstErr
}

// GetUserName 用 open_id 换用户姓名。
//
// 为什么需要：审批表单里的 **contact 控件只给出用户 ID**（ou_…），
// 不是人名。直接把它写进「购买人」列，等于对人说了一串乱码。
// 需要权限 contact:user.base:readonly；拿不到时返回错误，
// 调用方应当**不要**把 ID 当名字写下去。
func (c *Client) GetUserName(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", fmt.Errorf("用户 ID 为空")
	}
	// ★ 必须按**实际的 ID 类型**去查：审批 contact 控件实测给的是 user_id
	//   （如 "u7x2k9qz"），不是 open_id。类型传错接口直接报 not found，
	//   然后人就会以为是"没权限"，白折腾一圈。
	idType := "user_id"
	switch {
	case strings.HasPrefix(userID, "ou_"):
		idType = "open_id"
	case strings.HasPrefix(userID, "on_"):
		idType = "union_id"
	}
	q := url.Values{}
	q.Set("user_id_type", idType)
	data, err := c.get(ctx, "/contact/v3/users/"+url.PathEscape(userID), q)
	if err != nil {
		return "", err
	}
	// 响应形态：data.user.name（外面可能还包一层）
	var wrapper struct {
		User struct {
			Name string `json:"name"`
		} `json:"user"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return "", fmt.Errorf("解析用户 %s: %w", userID, err)
	}
	for _, n := range []string{wrapper.User.Name, wrapper.Data.Name, wrapper.Name} {
		if n != "" {
			return n, nil
		}
	}
	return "", fmt.Errorf("用户 %s 的响应里没有 name（缺 contact:user.base:readonly？）", userID)
}

// GetDepartmentName 是 GetDepartment 的便捷包装，只取名称。
func (c *Client) GetDepartmentName(ctx context.Context, departmentID string) (string, error) {
	info, err := c.GetDepartment(ctx, departmentID)
	if err != nil {
		return "", err
	}
	if info.Name == "" {
		return "", fmt.Errorf("缺少字段权限 contact:department.base:readonly，响应里没有 name"+
			"（department 存在，open_department_id=%s）", info.OpenDeptID)
	}
	return info.Name, nil
}

// RawGet 是调试用的只读 GET，直接返回 data 段原始字节。
func (c *Client) RawGet(ctx context.Context, path string) (json.RawMessage, error) {
	return c.get(ctx, path, nil)
}

// ── 审批事件订阅 ──────────────────────────────────────────────
//
// 审批事件是**双层订阅**：
//   第 1 层（控制台）：这类事件要不要推给我 —— 已完成
//   第 2 层（本接口）：这个审批定义的实例要不要产生事件 —— 必须显式调
//
// ⚠️ 不调本接口，一条审批事件都收不到。
// 权限：approval:approval 或 approval:definition（**readonly 不够**）。

// SubscribeApproval 开启指定审批定义的事件订阅（幂等，可重复调用）。
func (c *Client) SubscribeApproval(ctx context.Context, approvalCode string) error {
	return c.approvalSubscribeAction(ctx, approvalCode, "subscribe")
}

// UnsubscribeApproval 取消订阅。
func (c *Client) UnsubscribeApproval(ctx context.Context, approvalCode string) error {
	return c.approvalSubscribeAction(ctx, approvalCode, "unsubscribe")
}

func (c *Client) approvalSubscribeAction(ctx context.Context, approvalCode, action string) error {
	_, err := c.post(ctx,
		"/approval/v4/approvals/"+url.PathEscape(approvalCode)+"/"+action, map[string]any{})
	return err
}
