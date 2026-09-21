// download.go —— 多维表格附件的「取临时链接 → 下载」能力，
// 以及按视图/条件分页搜索记录。
//
// 这是 docs/30-review/32「选择性下载」计划里 P0/P1 所需的客户端能力。
// 所有接口都是**只读**：不写入任何数据。
//
// 三条已核实的纪律（见 docs/30-review/32 §5.1 与 §7）：
//  1. `batch_get_tmp_download_url` 是 **GET**，一次**最多 5 个** file_token；
//  2. 返回的链接**有效期 24 小时**，必须**现取现下**，不许缓存 URL（只存 file_token）；
//  3. 开启了高级权限的表，下载素材要带 `extra`（`bitablePerm`），否则 403。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TmpDownloadURL 是 batch_get_tmp_download_url 返回的一项。
type TmpDownloadURL struct {
	FileToken      string `json:"file_token"`
	TmpDownloadURL string `json:"tmp_download_url"`
}

// MaxTmpDownloadTokens 是官方规定的单次 file_token 上限。
const MaxTmpDownloadTokens = 5

// BatchGetTmpDownloadURL 取附件的临时下载链接。
//
//   - fileTokens 超过 5 个会**报错**而不是静默截断（避免漏下文件却没人发现）；
//   - extra 为空时不带该参数（普通权限表即可下载）；
//     开了高级权限的表要传 `bitablePerm` 的 JSON 字符串，否则 403。
func (c *Client) BatchGetTmpDownloadURL(ctx context.Context, fileTokens []string, extra string) ([]TmpDownloadURL, error) {
	if len(fileTokens) == 0 {
		return nil, fmt.Errorf("file_tokens 为空")
	}
	if len(fileTokens) > MaxTmpDownloadTokens {
		return nil, fmt.Errorf("一次最多 %d 个 file_token，收到 %d 个 —— 请分批调用",
			MaxTmpDownloadTokens, len(fileTokens))
	}
	q := url.Values{}
	for _, t := range fileTokens {
		if strings.TrimSpace(t) == "" {
			return nil, fmt.Errorf("file_token 里有空值")
		}
		q.Add("file_tokens", t)
	}
	if extra != "" {
		q.Set("extra", extra)
	}
	data, err := c.get(ctx, "/drive/v1/medias/batch_get_tmp_download_url", q)
	if err != nil {
		return nil, err
	}
	var out struct {
		TmpDownloadURLs []TmpDownloadURL `json:"tmp_download_urls"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析临时下载链接: %w (原始: %s)", err, truncate(data, 300))
	}
	return out.TmpDownloadURLs, nil
}

// AttachmentValue 是多维表格附件单元格里的一项。
//
// 已核实的返回结构（六个 key）：file_token / name / size / type / url / tmp_url。
// ★ 写入时只认 file_token —— 所以导出只把 file_token 记进审计，不记 URL。
type AttachmentValue struct {
	FileToken string `json:"file_token"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	TmpURL    string `json:"tmp_url"`
}

// ParseAttachmentCell 解析一个附件单元格的原始 JSON。
//
// 兼容两种形态：
//   - 数组（正常记录）：[{file_token,name,size,type,...}]
//   - 空/null：返回 nil
//
// 解析失败时返回错误而不是空列表 —— 否则「没下到文件」会被误当成「本来就没有附件」。
func ParseAttachmentCell(raw json.RawMessage) ([]AttachmentValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	var arr []AttachmentValue
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("解析附件单元格: %w (原始: %s)", err, truncate(raw, 200))
	}
	out := make([]AttachmentValue, 0, len(arr))
	for _, a := range arr {
		if a.FileToken == "" {
			continue // 没有 token 的项下不了，跳过（不制造假条目）
		}
		out = append(out, a)
	}
	return out, nil
}

// GetBitableRecord 按 record_id 取单条记录（只读）。
// 权限：bitable:app:readonly 即可。
func (c *Client) GetBitableRecord(ctx context.Context, appToken, tableID, recordID string) (*BitableRecord, error) {
	data, err := c.get(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
		"/tables/"+url.PathEscape(tableID)+"/records/"+url.PathEscape(recordID), nil)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Record BitableRecord `json:"record"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("解析记录 %s: %w", recordID, err)
	}
	if wrapper.Record.RecordID == "" {
		return nil, fmt.Errorf("记录 %s 的响应里没有 record_id: %s", recordID, truncate(data, 200))
	}
	return &wrapper.Record, nil
}

// SearchOptions 是 SearchBitableRecordsAll 的入参。
type SearchOptions struct {
	// ViewID 与 Filter **互斥**：官方明确「传了 filter/sort 时 view_id 会被忽略」。
	// 同时给这两个值会直接报错，避免静默导出超集（对应风险 R11）。
	ViewID     string
	Filter     map[string]any
	FieldNames []string
	PageSize   int // 上限 500，0 = 500
	MaxPages   int // 0 = 200
}

// SearchBitableRecordsAll 是 SearchBitableRecords 的**翻页版**（只读）。
//
// 与旧函数的区别：① 支持 view_id；② 用 page_token 翻完所有页；
// ③ 支持 field_names 只取需要的列（附件列原样拉回来会撑爆响应体，
// 官方在响应过大时返回 1254030 TooLargeResponse）。
func (c *Client) SearchBitableRecordsAll(ctx context.Context, appToken, tableID string, opts SearchOptions) ([]BitableRecord, error) {
	if opts.ViewID != "" && len(opts.Filter) > 0 {
		return nil, fmt.Errorf("view_id 与 filter 不能同时传：接口在传了 filter 时会忽略 view_id，" +
			"结果会与预期不符（要把筛选写进视图，或不用视图全用 filter）")
	}
	pageSize := opts.PageSize
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 500
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = 200
	}

	var all []BitableRecord
	pageToken := ""
	for page := 0; page < maxPages; page++ {
		payload := map[string]any{"page_size": pageSize}
		if opts.ViewID != "" {
			payload["view_id"] = opts.ViewID
		}
		if len(opts.Filter) > 0 {
			payload["filter"] = opts.Filter
		}
		if len(opts.FieldNames) > 0 {
			payload["field_names"] = opts.FieldNames
		}
		if pageToken != "" {
			payload["page_token"] = pageToken
		}
		data, err := c.post(ctx, "/bitable/v1/apps/"+url.PathEscape(appToken)+
			"/tables/"+url.PathEscape(tableID)+"/records/search", payload)
		if err != nil {
			return all, err
		}
		var d bitableListData[BitableRecord]
		if err := json.Unmarshal(data, &d); err != nil {
			return all, fmt.Errorf("解析记录搜索结果(第 %d 页): %w", page+1, err)
		}
		all = append(all, d.Items...)
		if !d.HasMore || d.PageToken == "" {
			break
		}
		pageToken = d.PageToken
	}
	return all, nil
}

// DownloadResult 是一次素材下载的结果摘要。
type DownloadResult struct {
	Bytes int64
	// ETag / ContentType 仅供探测打印，不参与业务判断。
	ContentType string
}

// DownloadTmpURL 用临时链接把素材下载到 w。
//
// ★ 不要把 URL 落盘或缓存：链接 24 小时失效（R1），审计里只留 file_token。
// 这里不设 Authorization 头 —— 临时链接自带鉴权参数，带 token 反而多余。
func (c *Client) DownloadTmpURL(ctx context.Context, tmpURL string, w io.Writer) (*DownloadResult, error) {
	if !strings.HasPrefix(tmpURL, "https://") && !strings.HasPrefix(tmpURL, "http://") {
		return nil, fmt.Errorf("临时链接格式不对: %q", truncate([]byte(tmpURL), 120))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tmpURL, nil)
	if err != nil {
		return nil, err
	}
	// 下载大文件不能沿用 30s 的接口超时
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载素材: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("下载素材失败: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("写下载内容: %w", err)
	}
	return &DownloadResult{
		Bytes:       n,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

// BitablePermExtra 构造高级权限表的下载鉴权参数 `extra`。
//
// 官方要求（能力稿 A7）：下载开启了高级权限的多维表格附件时，
// 必须带 `extra={"bitablePerm":{"tableId":"...","rev":<n>,"attachments":{...}}}`，
// 否则 403。rev 与 attachments 的精确要求官方文档在不同版本里写法不一致，
// 这里按「给了什么就传什么」的最小实现，真实取值由 P0 探测确认
// （见 cmd/download-probe 的 -extra 直传开关）。
func BitablePermExtra(tableID string, rev int64, attachments map[string]any) string {
	perm := map[string]any{"tableId": tableID}
	if rev > 0 {
		perm["rev"] = rev
	}
	if len(attachments) > 0 {
		perm["attachments"] = attachments
	}
	b, err := json.Marshal(map[string]any{"bitablePerm": perm})
	if err != nil {
		return ""
	}
	return string(b)
}

// PagesFor 计算 ceil(n/pageSize)，供调用方打印进度。
func PagesFor(n, pageSize int) int {
	if pageSize <= 0 {
		return 0
	}
	return int(math.Ceil(float64(n) / float64(pageSize)))
}
