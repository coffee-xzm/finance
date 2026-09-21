package export

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/naming"
)

func TestLoadListParsesMessyCopyPaste(t *testing.T) {
	// 真实形态：Excel/飞书复制出来带 BOM、表头、引号、空行、尾随逗号、重复
	content := "\ufeff审批实例号\n" +
		"7DB9ADCF001\n" +
		"\"7DB9ADCF002\"\n" +
		"\n" +
		"recABC123\n" +
		"7DB9ADCF001\r\n" + // 重复 + CRLF
		"recABC123,备注列\n" + // 多列，只取第一列
		"审批实例号\n" // 表头又出现一次

	dir := t.TempDir()
	p := filepath.Join(dir, "list.csv")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ids, sha, lines, err := LoadList(p, nil)
	if err != nil {
		t.Fatalf("LoadList: %v", err)
	}
	if sha == "" {
		t.Fatal("清单 sha256 不应为空")
	}
	if lines != 8 {
		t.Fatalf("原始行数 = %d，期望 8", lines)
	}
	if len(ids) != 3 {
		t.Fatalf("去重后标识 = %d，期望 3：%+v", len(ids), ids)
	}
	if ids[0].Value != "7DB9ADCF001" || ids[0].Kind != kindInstance {
		t.Fatalf("第 1 个标识不对: %+v", ids[0])
	}
	if ids[1].Value != "7DB9ADCF002" || ids[1].Kind != kindInstance {
		t.Fatalf("引号应被剥掉: %+v", ids[1])
	}
	if ids[2].Value != "recABC123" || ids[2].Kind != kindRecord {
		t.Fatalf("rec 前缀应识别为 record_id: %+v", ids[2])
	}
	// 行号要指回**原始行**，便于人对照
	if ids[0].Line != 2 || ids[2].Line != 5 {
		t.Fatalf("行号不对: %+v", ids)
	}
}

func TestLoadListRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.csv")
	if err := os.WriteFile(p, []byte("\n\n审批实例号\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadList(p, nil); err == nil {
		t.Fatal("全空清单应当报错，而不是当成 0 条静默通过")
	}
}

func TestLoadListInlineAndFileMerge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("INST1\n"), 0o644)
	ids, _, _, err := LoadList(p, []string{"INST2", " INST1 "})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("文件+命令行合并去重后 = %d，期望 2", len(ids))
	}
}

// raw 是构造记录字段的小工具。
func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fakeRecord() feishu.BitableRecord {
	return feishu.BitableRecord{
		RecordID: "recTEST01",
		Fields: map[string]json.RawMessage{
			"审批实例号":   json.RawMessage(`"7DB9ADCF"`),
			"发票号码":    json.RawMessage(`"12345678901234567890"`),
			"购买人":     json.RawMessage(`"张某某"`),
			"物资所属部门":  json.RawMessage(`["视觉组"]`),
			"图读金额(元)": json.RawMessage(`48.9`),
			"图读日期":    json.RawMessage(`1780329600000`), // 2026-06-02
			"销方名称":    json.RawMessage(`"某公司"`),
			"发票": json.RawMessage(`[{"file_token":"tokInv","name":"image.png","size":100,"type":"image/png"}]`),
			"订单截图": json.RawMessage(`[{"file_token":"tokOrd","name":"o.png","size":50,"type":"image/png"}]`),
			"付款记录": json.RawMessage(`[]`),
		},
	}
}

// 导出侧最关键的契约：记录 → 命名输入 → 文件名。
// 命名本身由 internal/naming 的冻结向量保证；这里保证**映射**没接错字段。
func TestToNamingRowAndPlan(t *testing.T) {
	rec := fakeRecord()
	row := toNamingRow(rec, []string{"发票", "订单截图", "付款记录"})

	if row.RecordID != "recTEST01" || row.InstanceNo != "7DB9ADCF" {
		t.Fatalf("标识映射错: %+v", row)
	}
	if row.Fields.Department != "视觉组" || row.Fields.Buyer != "张某某" ||
		row.Fields.Seller != "某公司" || row.Fields.InvoiceNo != "12345678901234567890" {
		t.Fatalf("字段映射错: %+v", row.Fields)
	}
	if row.Fields.Date != "2026-06-02" {
		t.Fatalf("日期应转成 2026-06-02，得到 %q", row.Fields.Date)
	}
	if row.Fields.Amount == nil || *row.Fields.Amount != 48.9 {
		t.Fatalf("金额应读到 48.9，得到 %v", row.Fields.Amount)
	}
	if len(row.Attachments[naming.SlotInvoice]) != 1 ||
		len(row.Attachments[naming.SlotOrder]) != 1 ||
		len(row.Attachments[naming.SlotPayment]) != 0 {
		t.Fatalf("附件槽位映射错: %+v", row.Attachments)
	}

	plan := naming.PlanRows([]naming.Row{row}, naming.Options{})
	if len(plan.Files) != 2 {
		t.Fatalf("应产出 2 个文件（发票+订单），得到 %d: %+v", len(plan.Files), plan.Files)
	}
	// 默认模板 + 按部门分组：部门进目录、不进文件名
	for _, f := range plan.Files {
		if f.Dir != "视觉组" {
			t.Fatalf("目录应为部门名，得到 %q", f.Dir)
		}
		if f.Name == "" || f.Path != f.Dir+"/"+f.Name {
			t.Fatalf("路径拼接不对: %+v", f)
		}
	}
	if plan.Counts[naming.SlotInvoice] != 1 || plan.Counts[naming.SlotOrder] != 1 {
		t.Fatalf("槽位计数不对: %+v", plan.Counts)
	}
}

// 空附件行必须产出 NO_ATTACHMENTS 告警，绝不静默少文件。
func TestEmptyRowYieldsWarning(t *testing.T) {
	rec := feishu.BitableRecord{
		RecordID: "recEMPTY",
		Fields: map[string]json.RawMessage{
			"审批实例号": json.RawMessage(`"X"`),
			"发票":    json.RawMessage(`[]`),
			"订单截图":  json.RawMessage(`[]`),
			"付款记录":  json.RawMessage(`[]`),
		},
	}
	row := toNamingRow(rec, []string{"发票", "订单截图", "付款记录"})
	plan := naming.PlanRows([]naming.Row{row}, naming.Options{})
	if len(plan.Files) != 0 {
		t.Fatalf("空附件行不应产出文件")
	}
	found := false
	for _, w := range plan.Warnings {
		if w.Code == "NO_ATTACHMENTS" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺 NO_ATTACHMENTS 告警: %+v", plan.Warnings)
	}
}

// 附件里 type 是 MIME 时，扩展名要按 MIME 定（PDF 才不会变成 .png）。
func TestMimeDrivesExtension(t *testing.T) {
	rec := feishu.BitableRecord{
		RecordID: "recPDF",
		Fields: map[string]json.RawMessage{
			"审批实例号": json.RawMessage(`"Y"`),
			"购买人":   json.RawMessage(`"李四"`),
			"发票": json.RawMessage(
				`[{"file_token":"t1","name":"发票文件","size":1,"type":"application/pdf"}]`),
		},
	}
	row := toNamingRow(rec, []string{"发票"})
	plan := naming.PlanRows([]naming.Row{row}, naming.Options{GroupBy: "none"})
	if len(plan.Files) != 1 {
		t.Fatalf("应有 1 个文件: %+v", plan.Files)
	}
	if got := plan.Files[0].Name; len(got) < 4 || got[len(got)-4:] != ".pdf" {
		t.Fatalf("PDF 扩展名不对: %q", got)
	}
}
