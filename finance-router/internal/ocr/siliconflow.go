package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// SiliconFlow 实现 SignalFlow（硅基流动）的 OpenAI 兼容 /chat/completions。
//
// 已核实接口事实（docs/30-review/02-ocr-provider-interface.md §2）：
//   - 图片走 content[] 的 {"type":"image_url","image_url":{"url":"<URL 或 base64 data URI>","detail":...}}
//   - /v1/files 只接受 purpose=batch → **图片不能走文件接口**，必须 base64
//   - 结构化输出：response_format = {"type":"json_schema","json_schema":{...}}
//   - 响应头 x-siliconcloud-trace-id 建议入库
type SiliconFlow struct {
	BaseURL        string
	APIKey         string
	Model          string
	ResponseFormat string // json_schema | json_object | text
	EnableThinking bool
	Temperature    float64
	MaxTokens      int
	HTTPClient     *http.Client
}

func NewSiliconFlow(baseURL, apiKey, model string) *SiliconFlow {
	if baseURL == "" {
		baseURL = "https://api.siliconflow.cn/v1"
	}
	return &SiliconFlow{
		BaseURL:        strings.TrimRight(baseURL, "/"),
		APIKey:         apiKey,
		Model:          model,
		ResponseFormat: "json_schema",
		MaxTokens:      1024,
		HTTPClient:     &http.Client{Timeout: 90 * time.Second},
	}
}

func (s *SiliconFlow) Name() string { return "siliconflow-qwen-vl" }

// ── 提示词 ────────────────────────────────────────────────────
//
// 三条硬约束写进 system prompt：
//  1. 读不到填 null，**绝不猜测**
//  2. 金额一律给【分】(整数)，避免小数与货币符号解析歧义
//  3. 发票必须区分【价税合计】/【税额】/【金额(不含税)】——这是最容易读错的地方
const systemPrompt = `你是票据信息抽取器。只输出 JSON，不要解释。

【最重要的规则】
1. 读不到的字段一律填 null。**绝对不要猜测、不要编造、不要用相近值代替。**
   宁可交白卷，也不要把不确定的数字填进去。
2. 所有金额字段一律输出**整数「分」**（元×100）。例如 228.31 元 → 22831。
   不要带货币符号、不要小数点、不要千分位。
3. 先判断图片类型 kind：
   - "invoice"  增值税发票（有发票代码/号码、价税合计等栏位）
   - "itinerary" 行程单/报销单（如"滴滴出行行程报销单"）**不是发票**
   - "order"    电商订单详情页截图
   - "payment"  微信/支付宝转账、账单、支付成功页截图
   - "unknown"  看不出是什么

【发票的三个金额，最容易搞错，务必分清】
- amount_incl_tax_cent = **价税合计**（含税总额，也叫"价税合计(小写)"）← 这是主口径
- tax_cent             = **税额**
- amount_excl_tax_cent = **金额**（不含税，即价税合计减去税额）
若票面上只有"价税合计"一栏，则 tax_cent 与 amount_excl_tax_cent 填 null。
若票面只有"金额"和"税额"两栏、没有"价税合计"，则 amount_incl_tax_cent 填
金额+税额 的合计值，另外两栏照填。

【订单/转账】
只填 amount_incl_tax_cent（实付/转账金额），另外两个金额填 null。

【多行明细与负数行 —— 极易算错，务必注意】
- 发票可能有**多行商品**，还可能有**优惠行/折扣行**（金额与税额是负数）。
- 必须取**票面印好的「合计」栏**的数字，**不要自己把各行相加**。
  票面「合计」行的金额与税额就是标准答案。
- 若票面上「价税合计(小写)」与「合计」不一致（例如有折扣），
  以「**价税合计(小写)**」为 amount_incl_tax_cent。

【必须同时抄下大写金额（★ 这是票面自带的校验码）】
- 票面「价税合计（大写）」栏的中文金额，**逐字原样抄写**，填 amount_incl_tax_upper。
  例如票面印的是「肆圆陆角整」，就填 "肆圆陆角整"。
- 不要转换成数字，不要改写字形，**原样抄写**。
- 同时输出小写与大写，两者互相印证；若你发现两者对不上，请重新读一遍再输出。

【日期与年份 —— ★ 最容易"编"错的地方，务必遵守】
票面上没有年份时，**绝对不要猜年份**。
  - 票面写"2026年04月20日" → date 填 "2026-04-20"
  - 票面只写"6月26日"（电商订单页常这样）→ date 填 "06-26"（只给月日，不给年份）
  - 票面写"2026-04-20" → date 填 "2026-04-20"
  - 完全看不到日期 → date 填 null
判断依据只有一个：**年份是否真的印在图上**。不要根据"截图时间"或"看起来像哪一年"来推断。
（下游会用其它单据的年份补全，你多填一个猜的年份反而会污染数据。）

【发票号码 vs 发票代码 —— 位置固定，请看仔细】
- 「**发票号码**」印在**票面右上角**，位数较长（全电发票为 20 位数字）。
  → 填 invoice_no。
- 「发票代码」只在**老式发票**上出现（10~12 位）。
  → 填 invoice_code；**全电发票没有这一栏，填 null**。
- 两者都可能被印得很小，请放大看清楚，**不要因为小就填 null**。

【其它字段】
- date: 单据上的日期，**原样抄写**，并遵守下面的「年份规则」
- counterparty: 发票的销售方名称 / 订单的店铺名 / 转账的收款方
- invoice_code/invoice_no/check_code/seller_tax_id: 仅发票有
- order_no: **商家/店铺的订单号**
- alipay_txn_id: **支付宝交易号**
- confidence: 你对本次识别的整体把握，0~1 的浮点数

【★ 支付宝交易号 vs 订单号 —— 两个截图上的叫法不一样，极易搞混】

这两个号都是 15~30 位纯数字。它们长得很像，但**是不同的东西**，
用途也不同（交易号用于"订单↔付款"配对，订单号用于"发票↔订单"配对）。
请严格按下面两张图各自的**标签**来填：

1) **淘宝/天猫的订单详情页**（标题常是"交易成功"）：
   - 标签写着「**支付宝交易号**」→ 填 alipay_txn_id
   - 标签写着「**订单号**」      → 填 order_no

2) **支付宝的账单详情页**（标题常是"账单详情"，有"支付时间/付款方式"）：
   - 标签写着「**订单号**」      → 这其实是**支付宝交易号**，填 alipay_txn_id
     ⚠️ 不要因为它叫"订单号"就填进 order_no —— 这是最容易错的一处。
   - 标签写着「**商家订单号**」  → 填 order_no
     若值带前缀（如 T200P 开头），**去掉前缀只保留后面的数字**。

两者都看不到就填 null。**不要用金额或时间凑一个号出来。**`

// jsonSchema 是结构化输出的契约。刻意保持扁平、只用受支持的构造。
var jsonSchema = map[string]any{
	"name":   "invoice_extraction",
	"strict": true,
	"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string",
				"enum": []string{"invoice", "itinerary", "order", "payment", "unknown"},
			},
			"confidence":            map[string]any{"type": "number"},
			"amount_incl_tax_cent":  map[string]any{"type": []string{"integer", "null"}},
			"tax_cent":              map[string]any{"type": []string{"integer", "null"}},
			"amount_excl_tax_cent":  map[string]any{"type": []string{"integer", "null"}},
			"amount_incl_tax_upper": map[string]any{"type": []string{"string", "null"}},
			"date":                  map[string]any{"type": []string{"string", "null"}},
			"counterparty":          map[string]any{"type": []string{"string", "null"}},
			"invoice_code":          map[string]any{"type": []string{"string", "null"}},
			"invoice_no":            map[string]any{"type": []string{"string", "null"}},
			"check_code":            map[string]any{"type": []string{"string", "null"}},
			"seller_tax_id":         map[string]any{"type": []string{"string", "null"}},
			"order_no":              map[string]any{"type": []string{"string", "null"}},
			"alipay_txn_id":         map[string]any{"type": []string{"string", "null"}},
		},
		"required": []string{
			"kind", "confidence", "amount_incl_tax_cent", "tax_cent", "amount_excl_tax_cent",
			"amount_incl_tax_upper",
			"date", "counterparty", "invoice_code", "invoice_no", "check_code",
			"seller_tax_id", "order_no", "alipay_txn_id",
		},
		"additionalProperties": false,
	},
}

// rawResult 对应模型输出（金额用 *int64，null 与 0 可区分）。
type rawResult struct {
	Kind               string  `json:"kind"`
	Confidence         float64 `json:"confidence"`
	AmountInclTaxCent  *int64  `json:"amount_incl_tax_cent"`
	TaxCent            *int64  `json:"tax_cent"`
	AmountExclTaxCent  *int64  `json:"amount_excl_tax_cent"`
	AmountInclTaxUpper *string `json:"amount_incl_tax_upper"`
	Date               *string `json:"date"`
	Counterparty       *string `json:"counterparty"`
	InvoiceCode        *string `json:"invoice_code"`
	InvoiceNo          *string `json:"invoice_no"`
	CheckCode          *string `json:"check_code"`
	SellerTaxID        *string `json:"seller_tax_id"`
	OrderNo            *string `json:"order_no"`
	AlipayTxnID        *string `json:"alipay_txn_id"`
}

func (s *SiliconFlow) Extract(ctx context.Context, img ImageRef) (*Result, error) {
	start := time.Now()

	dataURI := "data:" + img.MediaType + ";base64," +
		base64.StdEncoding.EncodeToString(img.Bytes)

	detail := img.Detail
	if detail == "" {
		detail = "high"
	}

	reqBody := map[string]any{
		"model": s.Model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": []map[string]any{
				{"type": "image_url", "image_url": map[string]any{
					"url": dataURI, "detail": detail,
				}},
				{"type": "text", "text": "抽取这张票据的信息。金额一律给整数「分」。读不到填 null。"},
			}},
		},
		"temperature": s.Temperature,
		"max_tokens":  s.MaxTokens,
	}
	// 只在该模型确实支持时才传 enable_thinking：
	// 实测 Qwen3-VL-30B-A3B 会报 20015 "current model does not support parameter"。
	if s.EnableThinking {
		reqBody["enable_thinking"] = true
	}
	switch s.ResponseFormat {
	case "json_schema":
		reqBody["response_format"] = map[string]any{
			"type": "json_schema", "json_schema": jsonSchema,
		}
	case "json_object":
		reqBody["response_format"] = map[string]any{"type": "json_object"}
	}

	raw, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 SiliconFlow: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	traceID := resp.Header.Get("x-siliconcloud-trace-id")

	// 容错：若报错说某参数不被支持（实测 20015），剥离该参数重试一次。
	if resp.StatusCode != http.StatusOK {
		if bad := unsupportedParam(body); bad != "" {
			if _, ok := reqBody[bad]; ok {
				delete(reqBody, bad)
				raw2, _ := json.Marshal(reqBody)
				req2, _ := http.NewRequestWithContext(ctx, http.MethodPost,
					s.BaseURL+"/chat/completions", bytes.NewReader(raw2))
				req2.Header.Set("Authorization", "Bearer "+s.APIKey)
				req2.Header.Set("Content-Type", "application/json")
				resp2, err2 := s.HTTPClient.Do(req2)
				if err2 == nil {
					defer resp2.Body.Close()
					body, _ = io.ReadAll(resp2.Body)
					traceID = resp2.Header.Get("x-siliconcloud-trace-id")
					resp = resp2
				}
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SiliconFlow HTTP %d: %s", resp.StatusCode, truncate(body, 400))
	}

	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("解析响应信封: %w (%s)", err, truncate(body, 300))
	}
	if len(envelope.Choices) == 0 {
		return nil, fmt.Errorf("响应没有 choices: %s", truncate(body, 300))
	}
	content := envelope.Choices[0].Message.Content

	res := &Result{
		Provider:    s.Name(),
		Model:       s.Model,
		TraceID:     traceID,
		LatencyMS:   int(time.Since(start).Milliseconds()),
		RawResponse: truncate([]byte(content), 2000),
	}

	var rr rawResult
	if err := unmarshalLoose(content, &rr); err != nil {
		res.Error = "JSON 解析失败: " + err.Error()
		return res, nil // 不返回 error：抽取失败要记录并入库，不是让流程崩
	}

	res.Kind = Kind(rr.Kind)
	res.Confidence = rr.Confidence
	res.AmountInclTaxCent = rr.AmountInclTaxCent
	res.TaxCent = rr.TaxCent
	res.AmountExclTaxCent = rr.AmountExclTaxCent
	res.AmountInclTaxUpper = deref(rr.AmountInclTaxUpper)
	res.Date = deref(rr.Date)
	res.Counterparty = deref(rr.Counterparty)
	res.InvoiceCode = deref(rr.InvoiceCode)
	res.InvoiceNo = deref(rr.InvoiceNo)
	res.CheckCode = deref(rr.CheckCode)
	res.SellerTaxID = deref(rr.SellerTaxID)
	res.OrderNo = deref(rr.OrderNo)
	res.AlipayTxnID = deref(rr.AlipayTxnID)
	return res, nil
}

// unmarshalLoose 是降级解析：模型偶尔会在 JSON 外面裹 ```json 或加一句废话。
// 顺序：① 直接解析 → ② 剥代码块 → ③ 截取第一个 { 到最后一个 }
func unmarshalLoose(s string, v any) error {
	s = strings.TrimSpace(s)
	if err := json.Unmarshal([]byte(s), v); err == nil {
		return nil
	}
	re := regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")
	if m := re.FindStringSubmatch(s); len(m) == 2 {
		if err := json.Unmarshal([]byte(m[1]), v); err == nil {
			return nil
		}
	}
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		if err := json.Unmarshal([]byte(s[i:j+1]), v); err == nil {
			return nil
		}
	}
	return fmt.Errorf("无法从响应中解析出 JSON")
}

// unsupportedParam 从错误体里提取"不支持的参数名"，提取不到返回空串。
func unsupportedParam(body []byte) string {
	re := regexp.MustCompile("`([a-z_]+)`")
	m := re.FindSubmatch(body)
	if len(m) == 2 {
		return string(m[1])
	}
	return ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func truncate(b []byte, n int) string {
	s := string(b)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
