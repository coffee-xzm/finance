// list.go —— 解析「勾选出来的清单」。
//
// 清单的来源（计划 §3.1）：财务在飞书多维表格里多选行 → 把「审批实例号」
// 或 record_id 列复制出来 → 存成 csv/txt，一行一个。
//
// 这里刻意把清单**只当输入**，并做四类校验（计划 §3.3）：
//   - 标识类型识别：`rec…` = record_id，其余 = 审批实例号（两种都支持）
//   - 空行/注释行跳过（复制粘贴常带表头与空行）
//   - 去重并保留行号（报错时能指回原文第几行）
//   - 内容 sha256：审计里只留哈希，不把名单搬进库
package export

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

type idKind int

const (
	kindInstance idKind = iota
	kindRecord
)

type identifier struct {
	Line  int
	Value string
	Kind  idKind
}

// LoadList 读取清单文件与命令行里的标识，去重后返回。
// 返回：标识列表、清单内容 sha256、原始行数。
func LoadList(file string, inline []string) ([]identifier, string, int, error) {
	var raw []string
	lines := 0

	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return nil, "", 0, fmt.Errorf("读取清单 %s: %w", file, err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			lines++
			raw = append(raw, sc.Text())
		}
		if err := sc.Err(); err != nil {
			return nil, "", lines, fmt.Errorf("读取清单 %s: %w", file, err)
		}
	}
	lines += len(inline)
	raw = append(raw, inline...)

	h := sha256.New()
	for _, r := range raw {
		h.Write([]byte(r))
		h.Write([]byte("\n"))
	}
	sum := hex.EncodeToString(h.Sum(nil))

	var out []identifier
	seen := map[string]bool{}
	for i, line := range raw {
		v := normalizeID(line)
		if v == "" {
			continue
		}
		key := strings.ToLower(v)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, identifier{
			Line:  i + 1,
			Value: v,
			Kind:  classify(v),
		})
	}
	if len(out) == 0 {
		return nil, sum, lines, fmt.Errorf("清单里没有任何有效标识（%d 行都是空行/表头）", lines)
	}
	return out, sum, lines, nil
}

// normalizeID 去掉 BOM、引号、空白与常见表头。
//
// 为什么这么啰嗦：清单是**人从飞书里复制粘贴**出来的，最常见的形态是
// 带表头「审批实例号」、每行带引号、或者尾随逗号。把这些当标识去查，
// 会得到一串「表里没有这个记录」的假报错。
func normalizeID(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	s = strings.TrimSpace(s)
	// csv 只取第一列（复制多列时）
	if i := strings.IndexAny(s, ",\t;"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.Trim(s, `"'`)
	if s == "" {
		return ""
	}
	switch strings.ToLower(s) {
	case "审批实例号", "instance_code", "instance", "record_id", "recordid", "记录id", "记录":
		return ""
	}
	return s
}

func classify(v string) idKind {
	if strings.HasPrefix(strings.ToLower(v), "rec") && len(v) > 3 {
		return kindRecord
	}
	return kindInstance
}
