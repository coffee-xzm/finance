// Package naming 是「记录 → 落盘文件名」的 **Go 侧实现**，
// 必须与 bitable-plugin/src/core/naming.ts（TS 侧）**逐字一致**。
//
// 为什么要有两份实现：插件在浏览器里打包（不经服务器），服务端导出是 Go；
// 两条路都要产出同样的文件名，否则「插件导的」和「服务端导的」对不上账。
// 唯一约束办法：两边跑**同一份冻结向量**
//
//	bitable-plugin/testdata/naming-cases.json   （由 `npm run vectors` 生成）
//
// 见 plan_test.go 的 TestVectors —— 一旦行为漂移就会红。
//
// 口径来源：docs/30-review/32 §3.5 与 docs/30-review/34。
package naming

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Slot 附件槽位；顺序即序号分配顺序（发票、订单、付款）。
type Slot string

const (
	SlotInvoice Slot = "invoice"
	SlotOrder   Slot = "order"
	SlotPayment Slot = "payment"
)

// Slots 是槽位的固定处理顺序（也是 {序号} 的分配顺序）。
type Slots []Slot

// AllSlots 按发票 → 订单 → 付款的固定顺序。
var AllSlots = Slots{SlotInvoice, SlotOrder, SlotPayment}

// SlotLabel 槽位中文名（模板变量 {槽位} 用它）。
var SlotLabel = map[Slot]string{
	SlotInvoice: "发票",
	SlotOrder:   "订单",
	SlotPayment: "付款",
}

// SlotField 是多维表格里的列名（见 finance-router/internal/bitable/schema.go）。
var SlotField = map[Slot]string{
	SlotInvoice: "发票",
	SlotOrder:   "订单截图",
	SlotPayment: "付款记录",
}

// DefaultTemplate 默认命名模板。
const DefaultTemplate = "{部门}-{购买人}-{日期}-{销方}-{金额}元-{槽位}{序号}"

// Fields 一行记录里可供命名的字段。
type Fields struct {
	Department string
	Buyer      string
	Date       string
	Seller     string
	// Amount 用指针区分「没读到」与「读到了 0」
	Amount    *float64
	InvoiceNo string
}

// Attachment 一条待下载的附件。
type Attachment struct {
	// Index 附件在单元格里的序号，从 1 开始。
	Index        int
	Token        string
	OriginalName string
	Mime         string
	Size         int64
}

// Row 一行输入。
type Row struct {
	RecordID    string
	InstanceNo  string
	Fields      Fields
	Attachments map[Slot][]Attachment
}

// Options 命名与分组选项。
type Options struct {
	// Template 空 = DefaultTemplate。
	Template string
	// GroupBy 取值 department | month | none；空 = department。
	GroupBy string
	// MaxNameBytes 单个文件名（含扩展名）的字节上限；0 = 120。
	MaxNameBytes int
	// Placeholder 字段缺省占位符；空 = "未知"。
	Placeholder string
}

// PlannedFile 一个待下载文件。
type PlannedFile struct {
	Name         string `json:"name"`
	Dir          string `json:"dir"`
	Path         string `json:"path"`
	Ordinal      int    `json:"ordinal"`
	Slot         Slot   `json:"slot"`
	RecordID     string `json:"recordId"`
	InstanceNo   string `json:"instanceNo,omitempty"`
	Token        string `json:"token"`
	OriginalName string `json:"originalName"`
	Mime         string `json:"mime"`
	DeclaredSize int64  `json:"declaredSize"`
}

// Warning 一条提示（不阻断导出）。
type Warning struct {
	RecordID string `json:"recordId"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// Result 规划结果。
type Result struct {
	Files      []PlannedFile `json:"files"`
	Warnings   []Warning     `json:"warnings"`
	Counts     map[Slot]int  `json:"counts"`
	TotalBytes int64         `json:"totalBytes"`
}

// fieldLabel 字段中文名（告警里给人看）。
var fieldLabel = map[string]string{
	"department": "物资所属部门",
	"buyer":      "购买人",
	"date":       "图读日期",
	"seller":     "销方名称",
	"amount":     "图读金额",
}

var (
	illegalRe   = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f\x7f]`)
	spaceRe     = regexp.MustCompile(`\s+`)
	multiSepRe  = regexp.MustCompile(`[-_]{2,}`)
	dateRe      = regexp.MustCompile(`^(\d{4})[-/. ](\d{1,2})[-/. ](\d{1,2})`)
	amountNumRe = regexp.MustCompile(`-?\d+(\.\d+)?`)
	monthRe     = regexp.MustCompile(`^(\d{4})-(\d{2})`)
	varRe       = regexp.MustCompile(`\{([^}]+)\}`)
)

// Slug 把字段值洗成文件名安全片段。规则与 TS 侧逐条对应：
//  1. 去首尾空白、折叠内部空白
//  2. 删非法字符与路径分隔符（防路径穿越）
//  3. 合并连续分隔符
//  4. 去首尾 . - _ 与空格
func Slug(raw string) string {
	s := strings.ToValidUTF8(raw, "")
	s = illegalRe.ReplaceAllString(s, " ")
	s = spaceRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = multiSepRe.ReplaceAllStringFunc(s, func(m string) string { return m[:1] })
	s = strings.TrimLeft(s, ".-_ ")
	s = strings.TrimRight(s, ".-_ ")
	return s
}

// DatePart 只取 YYYY-MM-DD。
func DatePart(raw string) string {
	s := Slug(raw)
	m := dateRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	return m[1] + "-" + pad2(m[2]) + "-" + pad2(m[3])
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// AmountText 剥掉货币符号与千分位，保留两位小数；非数字原样返回。
func AmountText(raw string) string {
	s := strings.NewReplacer("¥", "", "￥", "", ",", "", "，", "", " ", "").Replace(Slug(raw))
	if s == "" {
		return ""
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return strconv.FormatFloat(f, 'f', 2, 64)
	}
	if m := amountNumRe.FindString(s); m != "" {
		if f, err := strconv.ParseFloat(m, 64); err == nil {
			return strconv.FormatFloat(f, 'f', 2, 64)
		}
	}
	return s
}

// InvoiceTail 发票号后 6 位。
func InvoiceTail(invoiceNo string) string {
	s := Slug(invoiceNo)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > 6 {
		return string(r[len(r)-6:])
	}
	return s
}

// TruncateBytes 按字节截断，且不切断多字节字符。
func TruncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		n := utf8.RuneLen(r)
		if used+n > limit {
			break
		}
		b.WriteRune(r)
		used += n
	}
	return b.String()
}

// TruncateName 在字节上限内保住扩展名。
func TruncateName(name string, maxBytes int) string {
	if len(name) <= maxBytes {
		return name
	}
	ext := ""
	stem := name
	if dot := strings.LastIndex(name, "."); dot > 0 {
		ext = name[dot:]
		stem = name[:dot]
	}
	if len(ext) >= maxBytes {
		return TruncateBytes(name, maxBytes)
	}
	room := maxBytes - len(ext)
	stem = strings.TrimRight(TruncateBytes(stem, room), ".-_ ")
	return stem + ext
}

// ExtOf 扩展名优先级：mime → 原始文件名。
func ExtOf(a Attachment) string {
	byMime := map[string]string{
		"application/pdf": ".pdf",
		"image/jpeg":      ".jpg",
		"image/jpg":       ".jpg",
		"image/png":       ".png",
		"image/webp":      ".webp",
		"image/gif":       ".gif",
		"image/bmp":       ".bmp",
		"image/heic":      ".heic",
	}
	mime := strings.ToLower(a.Mime)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.TrimSpace(mime)
	if v, ok := byMime[mime]; ok {
		return v
	}
	orig := a.OriginalName
	dot := strings.LastIndex(orig, ".")
	if dot > 0 && len(orig)-dot <= 6 {
		ext := strings.ToLower(orig[dot:])
		if extRe.MatchString(ext) {
			return ext
		}
	}
	return ""
}

var extRe = regexp.MustCompile(`^\.[a-z0-9]+$`)

func originalNameOf(a Attachment) string {
	if s := Slug(a.OriginalName); s != "" {
		return s
	}
	return "attachment-" + itoa(a.Index)
}

// GroupDir 分组目录；none 返回空串。
func GroupDir(groupBy, department, isoDate string) string {
	switch groupBy {
	case "none":
		return ""
	case "month":
		if m := monthRe.FindStringSubmatch(isoDate); m != nil {
			return m[1] + "-" + m[2]
		}
		return "未知月份"
	default:
		return Slug(department)
	}
}

// AdaptTemplate 目录里已有的维度不在文件名里重复。
func AdaptTemplate(template, groupBy string) string {
	switch groupBy {
	case "department":
		return trimVarPrefix(template, "部门")
	case "month":
		return trimVarPrefix(template, "日期")
	default:
		return template
	}
}

var deptPrefixRe = regexp.MustCompile(`^\s*\{部门\}\s*[-_]\s*`)
var datePrefixRe = regexp.MustCompile(`^\s*\{日期\}\s*[-_]\s*`)

func trimVarPrefix(template, name string) string {
	if name == "部门" {
		return deptPrefixRe.ReplaceAllString(template, "")
	}
	return datePrefixRe.ReplaceAllString(template, "")
}

// PlanRows 把一批记录规划成待下载文件清单。与 TS 侧 planRows 行为一致。
func PlanRows(rows []Row, opts Options) Result {
	template := strings.TrimSpace(opts.Template)
	userTemplate := template != ""
	if !userTemplate {
		template = DefaultTemplate
	}
	groupBy := opts.GroupBy
	if groupBy == "" {
		groupBy = "department"
	}
	maxNameBytes := opts.MaxNameBytes
	if maxNameBytes == 0 {
		maxNameBytes = 120
	}
	placeholder := opts.Placeholder
	if placeholder == "" {
		placeholder = "未知"
	}

	res := Result{
		Files:    []PlannedFile{},
		Warnings: []Warning{},
		Counts:   map[Slot]int{SlotInvoice: 0, SlotOrder: 0, SlotPayment: 0},
	}
	used := map[string]bool{}
	effectiveTemplate := AdaptTemplate(template, groupBy)

	for _, row := range rows {
		present := make([]Slot, 0, 3)
		for _, s := range AllSlots {
			if len(row.Attachments[s]) > 0 {
				present = append(present, s)
			}
		}
		if len(present) == 0 {
			res.Warnings = append(res.Warnings, Warning{
				RecordID: row.RecordID,
				Code:     "NO_ATTACHMENTS",
				Message:  "这条记录的三个附件列都是空的",
			})
			continue
		}

		var missing []string
		val := func(kind string) string {
			var v string
			switch kind {
			case "department":
				v = Slug(row.Fields.Department)
			case "buyer":
				v = Slug(row.Fields.Buyer)
			case "date":
				v = DatePart(row.Fields.Date)
			case "seller":
				v = Slug(row.Fields.Seller)
			case "amount":
				if row.Fields.Amount != nil {
					v = strconv.FormatFloat(*row.Fields.Amount, 'f', 2, 64)
				}
			}
			if v == "" {
				missing = append(missing, fieldLabel[kind])
				return placeholder
			}
			return v
		}

		department := val("department")
		buyer := val("buyer")
		date := val("date")
		seller := val("seller")
		amount := val("amount")
		invoiceNo := Slug(row.Fields.InvoiceNo)
		invoiceTailText := InvoiceTail(invoiceNo)
		if invoiceTailText == "" {
			invoiceTailText = placeholder
		}
		ordinal := 0

		for _, slot := range AllSlots {
			list := row.Attachments[slot]
			multi := len(list) > 1
			for _, att := range list {
				ordinal++
				seq := pad2(itoa(att.Index))
				vars := map[string]string{
					"部门":      department,
					"购买人":     buyer,
					"日期":      date,
					"销方":      seller,
					"金额":      amount,
					"槽位":      SlotLabel[slot],
					"序号":      "",
					"发票号码":    invoiceNo,
					"发票号码后6位": invoiceTailText,
				}
				if invoiceNo == "" {
					vars["发票号码"] = placeholder
				}
				if multi {
					vars["序号"] = seq
				}

				parts := make([]string, 0, 6)
				if groupBy != "department" {
					parts = append(parts, department)
				}
				parts = append(parts, buyer)
				if groupBy != "month" {
					parts = append(parts, date)
				}
				parts = append(parts, seller, amount+"元", SlotLabel[slot]+vars["序号"])

				ext := ExtOf(att)
				var stem string
				if userTemplate {
					stem = fitStem(parts, maxNameBytes, ext, effectiveTemplate, vars)
				} else {
					stem = fitStem(parts, maxNameBytes, ext, DefaultTemplate, vars)
				}
				if stem == "" {
					stem = "unnamed"
				}
				name := TruncateName(stem+ext, maxNameBytes)
				dir := GroupDir(groupBy, department, date)

				attempt := 1
				key := dir + "\x00" + name
				for used[key] {
					attempt++
					name = TruncateName(stem+"~"+itoa(attempt)+ext, maxNameBytes)
					key = dir + "\x00" + name
				}
				used[key] = true

				if attempt > 1 {
					res.Warnings = append(res.Warnings, Warning{
						RecordID: row.RecordID,
						Code:     "COLLISION",
						Message:  "文件名撞车，已改为 " + name,
					})
				}

				path := name
				if dir != "" {
					path = dir + "/" + name
				}
				res.Files = append(res.Files, PlannedFile{
					Name:         name,
					Dir:          dir,
					Path:         path,
					Ordinal:      ordinal,
					Slot:         slot,
					RecordID:     row.RecordID,
					InstanceNo:   row.InstanceNo,
					Token:        att.Token,
					OriginalName: originalNameOf(att),
					Mime:         att.Mime,
					DeclaredSize: att.Size,
				})
				res.Counts[slot]++
				res.TotalBytes += att.Size
			}
		}

		hasInvoice := false
		for _, s := range present {
			if s == SlotInvoice {
				hasInvoice = true
			}
		}
		if len(missing) > 0 && hasInvoice {
			res.Warnings = append(res.Warnings, Warning{
				RecordID: row.RecordID,
				Code:     "MISSING_FIELD",
				Message:  "命名所需字段缺失，已用「" + placeholder + "」占位：" + strings.Join(missing, "、"),
			})
		}
	}
	return res
}

// fitStem 生成文件名主体（不含扩展名），并保证含扩展名的总字节数不超限。
// 与 TS 侧 fitStem 一致：逐级**均匀**收紧每个部件的预算。
func fitStem(parts []string, maxNameBytes int, ext, template string, vars map[string]string) string {
	budget := maxNameBytes - len(ext)
	if budget < 8 {
		budget = 8
	}
	const sep = "-"

	if template != DefaultTemplate {
		rendered := Slug(renderTemplate(template, vars))
		rendered = strings.TrimRight(rendered, ".-_ ")
		return TruncateBytes(rendered, budget)
	}

	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	for _, per := range []int{64, 48, 32, 24, 16, 12, 8, 6, 4} {
		clipped := make([]string, len(clean))
		for i, p := range clean {
			clipped[i] = TruncateBytes(p, per)
		}
		joined := strings.Join(clipped, sep)
		if len(joined) <= budget {
			return joined
		}
	}
	clipped := make([]string, len(clean))
	for i, p := range clean {
		clipped[i] = TruncateBytes(p, 4)
	}
	return TruncateBytes(strings.Join(clipped, sep), budget)
}

func renderTemplate(template string, vars map[string]string) string {
	return varRe.ReplaceAllStringFunc(template, func(whole string) string {
		m := varRe.FindStringSubmatch(whole)
		if m == nil {
			return whole
		}
		k := strings.TrimSpace(m[1])
		if k == "发票号后6位" {
			return vars["发票号码后6位"]
		}
		if v, ok := vars[k]; ok {
			return v
		}
		return whole
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
