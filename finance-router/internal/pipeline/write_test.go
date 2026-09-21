package pipeline

import (
	"encoding/json"
	"testing"

	"github.com/coffee/finance-router/internal/config"
)

func TestBlankOnly(t *testing.T) {
	cfg := &config.Config{}
	cfg.Dict = config.DefaultDict()

	existing := map[string]json.RawMessage{
		"购买人":  json.RawMessage(`"张三"`),                   // 非空 → 不覆盖
		"差异说明": json.RawMessage(`"发票 48.90 元"`),           // 非空 → 不覆盖
		"物资种类": json.RawMessage(`""`),                     // 空 → 可写
		"发票":   json.RawMessage(`[{"file_token":"tok"}]`), // 非空 → 不重传
		"人工审核": json.RawMessage(`"驳回"`),                   // 非空，但服务自有 → 可覆盖
	}
	desired := map[string]any{
		"购买人":  "李四",
		"差异说明": "新的差异",
		"物资种类": "视觉物资",
		"发票":   []map[string]string{{"file_token": "new"}},
		"人工审核": "通过",
	}
	got := blankOnly(cfg, existing, desired, serviceOwnedSet(cfg))

	if _, ok := got["购买人"]; ok {
		t.Error("非空的人类字段不该被覆盖")
	}
	if _, ok := got["差异说明"]; ok {
		t.Error("非空的差异说明不该被覆盖")
	}
	if _, ok := got["发票"]; ok {
		t.Error("已有附件不该重传")
	}
	if got["物资种类"] != "视觉物资" {
		t.Error("空单元格应被填上")
	}
	if got["人工审核"] != "通过" {
		t.Error("服务自有列应允许覆盖（驳回→通过）")
	}
}

func TestIsEmptyCell(t *testing.T) {
	empty := []string{``, `null`, `""`, `[]`, `{}`, `[{"text":""}]`}
	for _, s := range empty {
		if !isEmptyCell(json.RawMessage(s)) {
			t.Errorf("%s 应判为空", s)
		}
	}
	nonEmpty := []string{`"x"`, `[{"text":"a"}]`, `0`, `[{"file_token":"t"}]`}
	for _, s := range nonEmpty {
		if isEmptyCell(json.RawMessage(s)) {
			t.Errorf("%s 不应判为空", s)
		}
	}
}
