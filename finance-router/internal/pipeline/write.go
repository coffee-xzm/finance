// write 实现"只填空单元格"的写入策略（docs/30-review/33 §6）。
//
// 用户口径：**只填当前为空的单元格，不覆盖任何非空单元格**；
// 但"服务自有状态列"（人工审核/已归档/审核时间/发票收集进度）例外 ——
// 否则「驳回 → 通过」这种状态跃迁根本写不进去。
//
// 为什么重要：
//   - 附件字段每同步一次就重传一遍，既慢又费额度 —— 已有图就跳过；
//   - 机器把「已归档」写成 false 会把已归档的行"退回未归档"。
package pipeline

import (
	"encoding/json"

	"github.com/coffee/finance-router/internal/config"
)

// blankOnly 从 desired 里剔除"目标单元格已非空"的键，返回真正要写的字段。
//
// allowOverwrite 里的键（对应服务自有列）不受此限制。
func blankOnly(cfg *config.Config, existing map[string]json.RawMessage, desired map[string]any,
	allowOverwrite map[string]bool) map[string]any {

	out := map[string]any{}
	for k, v := range desired {
		if allowOverwrite[k] || cfg == nil || !cfg.OnlyFillBlank() {
			out[k] = v
			continue
		}
		if !isEmptyCell(existing[k]) {
			continue // 非空 → 不覆盖
		}
		out[k] = v
	}
	return out
}

// serviceOwnedSet 把 config 里的"服务自有列"转成查找表。
func serviceOwnedSet(cfg *config.Config) map[string]bool {
	m := map[string]bool{}
	if cfg == nil {
		return m
	}
	for _, h := range cfg.Dict.Write.ServiceOwned {
		m[h] = true
	}
	return m
}

// isEmptyCell 判断飞书记录里某个字段值是否为空。
// 空 = 没这个键 / null / 空字符串 / 空数组 / 空白富文本。
func isEmptyCell(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	s := string(raw)
	switch s {
	case "null", `""`, "[]", "{}":
		return true
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		if len(arr) == 0 {
			return true
		}
		// 只有"纯富文本片段数组且每段 text 都为空"才算空。
		// ★ 附件等对象数组没有 text 键 → 一律非空（曾经把附件误判成空 → 重复上传）。
		for _, e := range arr {
			var seg map[string]json.RawMessage
			if json.Unmarshal(e, &seg) != nil {
				return false
			}
			txt, ok := seg["text"]
			if !ok {
				return false
			}
			var s string
			if json.Unmarshal(txt, &s) != nil || s != "" {
				return false
			}
		}
		return true
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str == ""
	}
	return false
}
