// value.go —— 多维表格字段值的读取辅助。
//
// 为什么需要单独一层：多维表格同一个字段类型在 API 里有多种返回形态
// （文本可能是纯字符串或富文本片段数组；日期是毫秒时间戳或字符串；
// 多选是字符串数组）。导出与探测都依赖这些读取规则，
// 放在这里可以避免 cmd 里各写一份、各有各的 bug。
package bitable

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TextOf 取文本字段的值，兼容「纯字符串」与「富文本片段数组」两种形态。
func TextOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var segs []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &segs) == nil {
		var sb strings.Builder
		for _, x := range segs {
			sb.WriteString(x.Text)
		}
		return sb.String()
	}
	// 单选字段有时返回 {"text":...} 形态
	var obj struct {
		Text string `json:"text"`
		// 日期等复合形态
		Value any `json:"value"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Text != "" {
		return obj.Text
	}
	return ""
}

// StringsOf 取多选字段的值（字符串数组）。单个字符串也接受。
// 数组元素若是对象（如 {"text":"x"} / {"name":"x"}），取其可读文本。
func StringsOf(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		if s := TextOf(raw); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		var s string
		if json.Unmarshal(e, &s) == nil {
			if s != "" {
				out = append(out, s)
			}
			continue
		}
		var obj struct {
			Text string `json:"text"`
			Name string `json:"name"`
		}
		if json.Unmarshal(e, &obj) == nil {
			if obj.Text != "" {
				out = append(out, obj.Text)
			} else if obj.Name != "" {
				out = append(out, obj.Name)
			}
		}
	}
	return out
}

// FirstString 取多选里的第一个值（命名时「物资所属部门」只用一个）。
func FirstString(raw json.RawMessage) string {
	if v := StringsOf(raw); len(v) > 0 {
		return v[0]
	}
	return TextOf(raw)
}

// NumberOf 取数字字段。解析不出时返回 nil（区分「没读到」与「读到 0」）。
func NumberOf(raw json.RawMessage) *float64 {
	if len(raw) == 0 {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return &f
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.NewReplacer("¥", "", "￥", "", ",", "", "，", "", " ", "").Replace(s)
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return &v
		}
	}
	return nil
}

// DateOf 取日期字段，统一成 YYYY-MM-DD。
//
// 多维表格的日期字段在 API 里返回**毫秒时间戳**（数字）；但历史数据里
// 也可能是文本（如「2026/6/2」）。两种都要能读，否则命名会退化成「未知」。
func DateOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var ms int64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		// 毫秒时间戳；用本地时区（表里显示的就是本地日期）
		return time.UnixMilli(ms).Local().Format("2006-01-02")
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil && f > 0 {
		return time.UnixMilli(int64(f)).Local().Format("2006-01-02")
	}
	s := TextOf(raw)
	if s == "" {
		return ""
	}
	for _, layout := range []string{"2006-01-02", "2006/1/2", "2006/01/02", "2006-1-2",
		"2006-01-02 15:04:05", "2006/1/2 15:04:05"} {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(s), time.Local); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return s
}

// FieldValue 是「某条记录在某个字段上的人类可读值」的调试形态（探测命令打印用）。
func FieldValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// RequiredMissing 返回 fields 里缺失（未设置或空）的字段名。
func RequiredMissing(fields map[string]json.RawMessage, names []string) []string {
	var out []string
	for _, n := range names {
		raw, ok := fields[n]
		if !ok || len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
			out = append(out, n)
		}
	}
	return out
}

// ErrFieldType 用于把「字段类型不符」讲清楚（探测时有用）。
func ErrFieldType(name, want string, raw json.RawMessage) error {
	return fmt.Errorf("字段 %s 期望 %s，实际得到 %s", name, want, FieldValue(raw))
}
