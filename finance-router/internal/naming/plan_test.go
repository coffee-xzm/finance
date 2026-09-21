package naming

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// vectorFile 是**插件与 Go 两侧共用的行为契约**。
// 由 bitable-plugin 的 `npm run vectors` 生成。
type vectorFile struct {
	Version int          `json:"version"`
	Cases   []vectorCase `json:"cases"`
}

type vectorOptions struct {
	Template     string `json:"template"`
	GroupBy      string `json:"groupBy"`
	MaxNameBytes int    `json:"maxNameBytes"`
	Placeholder  string `json:"placeholder"`
}

type vectorFields struct {
	Department string  `json:"department"`
	Buyer      string  `json:"buyer"`
	Date       string  `json:"date"`
	Seller     string  `json:"seller"`
	Amount     flexNum `json:"amount"`
	InvoiceNo  string  `json:"invoiceNo"`
}

// flexNum 兼容向量里 amount 既可能是数字（48.9）也可能是字符串（"1,234.5"）。
// 与 TS 侧 amountText 对字符串的处理保持一致：先剥货币符号与千分位再解析。
type flexNum struct {
	Num  *float64
	Text string
}

func (f *flexNum) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		f.Text = s
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	f.Num = &n
	return nil
}

// toFields 把向量字段翻译成命名层的 Fields（字符串金额也走 amountText 的同样清洗）。
func (f vectorFields) toFields() Fields {
	out := Fields{
		Department: f.Department,
		Buyer:      f.Buyer,
		Date:       f.Date,
		Seller:     f.Seller,
		InvoiceNo:  f.InvoiceNo,
	}
	if f.Amount.Num != nil {
		out.Amount = f.Amount.Num
	} else if f.Amount.Text != "" {
		if parsed, err := strconv.ParseFloat(AmountText(f.Amount.Text), 64); err == nil {
			out.Amount = &parsed
		}
	}
	return out
}

type vectorAttachment struct {
	Index        int    `json:"index"`
	Token        string `json:"token"`
	OriginalName string `json:"originalName"`
	Mime         string `json:"mime"`
	Size         int64  `json:"size"`
}

type vectorRow struct {
	RecordID    string                      `json:"recordId"`
	InstanceNo  string                      `json:"instanceNo"`
	Fields      vectorFields                `json:"fields"`
	Attachments map[Slot][]vectorAttachment `json:"attachments"`
}

type vectorCase struct {
	Name     string        `json:"name"`
	Why      string        `json:"why"`
	Options  vectorOptions `json:"options"`
	Rows     []vectorRow   `json:"rows"`
	Expected struct {
		Files      []PlannedFile `json:"files"`
		Warnings   []Warning     `json:"warnings"`
		Counts     map[Slot]int  `json:"counts"`
		TotalBytes int64         `json:"totalBytes"`
	} `json:"expected"`
}

// findVectors 定位共享向量文件；找不到就跳过（例如只 checkout 了 finance-router 的场景）。
func findVectors(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("无法定位当前文件")
	}
	// finance-router/internal/naming → 仓库根
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	candidates := []string{
		filepath.Join(root, "bitable-plugin", "testdata", "naming-cases.json"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Skipf("未找到共享向量文件（看了 %v）；插件侧跑 `npm run vectors` 生成", candidates)
	return ""
}

// TestVectors 是本仓库里**唯一**保证「插件（TS）与服务端（Go）命名一致」的机制。
// 任何一侧改了行为而没同步向量，这里都会红。
func TestVectors(t *testing.T) {
	path := findVectors(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读向量文件失败：%v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("解析向量文件失败：%v", err)
	}
	if len(vf.Cases) == 0 {
		t.Fatal("向量文件里没有用例")
	}

	for _, c := range vf.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			rows := make([]Row, 0, len(c.Rows))
			for _, r := range c.Rows {
				atts := map[Slot][]Attachment{}
				for slot, list := range r.Attachments {
					out := make([]Attachment, 0, len(list))
					for _, a := range list {
						out = append(out, Attachment{
							Index:        a.Index,
							Token:        a.Token,
							OriginalName: a.OriginalName,
							Mime:         a.Mime,
							Size:         a.Size,
						})
					}
					atts[slot] = out
				}
				rows = append(rows, Row{
					RecordID:    r.RecordID,
					InstanceNo:  r.InstanceNo,
					Fields:      r.Fields.toFields(),
					Attachments: atts,
				})
			}

			got := PlanRows(rows, Options{
				Template:     c.Options.Template,
				GroupBy:      c.Options.GroupBy,
				MaxNameBytes: c.Options.MaxNameBytes,
				Placeholder:  c.Options.Placeholder,
			})

			if !reflect.DeepEqual(got.Files, c.Expected.Files) {
				t.Errorf("文件清单不一致（%s）\n实际：%s\n期望：%s",
					c.Why, dump(got.Files), dump(c.Expected.Files))
			}
			if !reflect.DeepEqual(got.Warnings, c.Expected.Warnings) {
				t.Errorf("告警不一致（%s）\n实际：%s\n期望：%s",
					c.Why, dump(got.Warnings), dump(c.Expected.Warnings))
			}
			if !reflect.DeepEqual(got.Counts, c.Expected.Counts) {
				t.Errorf("槽位计数不一致：实际 %v，期望 %v", got.Counts, c.Expected.Counts)
			}
			if got.TotalBytes != c.Expected.TotalBytes {
				t.Errorf("总字节不一致：实际 %d，期望 %d", got.TotalBytes, c.Expected.TotalBytes)
			}
		})
	}
}

func dump(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "（无法序列化）"
	}
	return string(b)
}
