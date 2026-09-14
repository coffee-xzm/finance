package exact

import (
	"strings"
)

// QR 协议（实测 + 公开资料）：
//
//	01,<票种代码>,<发票代码>,<发票号码>,<金额>,<开票日期>,<校验码>,<随机码>
//
// 实测样例（数电票）：01,32,,<20位发票号码>,<价税合计>,<YYYYMMDD>,,<随机码>
// 老式增值税票：      01,10,<12位发票代码>,<8位发票号码>,<金额>,<YYYYMMDD>,<校验码>,<随机码>
//
// ⚠️ 第 4 段（下标 4）的语义**随票种变化**：
//   - 老式票的公开资料称其为"开票金额"，可能是不含税金额；
//   - 数电票实测为**价税合计**（与票面"（小写）"一致）。
//
// 所以这里**不做假设**：解出来先当"金额"用，然后再由 Merge 与文字层的
// 价税合计/金额+税额交叉核对 —— 对不上就报冲突，交人工。
//
// 票种代码：01=增值税专用发票，04=增值税普通发票，10=增值税电子普通发票，
// 31/32 等为全电发票（数电票）系列。
const (
	qrIdxKind     = 1
	qrIdxCode     = 2
	qrIdxNo       = 3
	qrIdxAmount   = 4
	qrIdxDate     = 5
	qrIdxCheck    = 6
	qrMinSegments = 6
)

// ParseQR 解析发票二维码文本。不是已知协议时返回 nil（不猜）。
func ParseQR(raw string) *Fields {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) < qrMinSegments {
		return nil
	}
	// 首段固定为 "01"；不满足就不认，避免把别的二维码当成发票。
	if strings.TrimSpace(parts[0]) != "01" {
		return nil
	}
	f := &Fields{Source: "qr", RawQR: raw}

	if v := strings.TrimSpace(parts[qrIdxCode]); v != "" {
		f.InvoiceCode = v
	}
	if v := strings.TrimSpace(parts[qrIdxNo]); v != "" {
		f.InvoiceNo = v
	}
	if v := strings.TrimSpace(parts[qrIdxAmount]); v != "" {
		f.AmountCent = ParseAmount(v)
	}
	if v := strings.TrimSpace(parts[qrIdxDate]); len(v) == 8 {
		f.Date = v[0:4] + "-" + v[4:6] + "-" + v[6:8]
	}
	return f
}

// QRKind 返回票种代码（下标 1），未知时返回空串。
func QRKind(raw string) string {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) > qrIdxKind {
		return strings.TrimSpace(parts[qrIdxKind])
	}
	return ""
}
