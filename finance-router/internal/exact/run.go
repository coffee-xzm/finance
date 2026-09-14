package exact

import (
	"fmt"
	"image"
	_ "image/jpeg" // 让 image.Decode 认识 jpeg
	_ "image/png"
	"os"
	"os/exec"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// FromPDF 用 pdftotext 读取 PDF 文字层并解析。
//
// pdftotext 由 poppler-utils 提供，目标机（含机器人）已确认可用 ——
// 我们已经依赖它的兄弟 pdftoppm 做栅格化了。
// 读不到文字层（扫描件/照片）返回 nil，调用方自然退回二维码或模型。
func FromPDF(pdfPath string) (*Fields, error) {
	out, err := exec.Command("pdftotext", "-layout", pdfPath, "-").Output()
	if err != nil {
		return nil, fmt.Errorf("pdftotext: %w", err)
	}
	return ParseInvoiceText(string(out)), nil
}

// FromImageFile 从图片里找二维码并解析。
// 找不到/解不出返回 (nil, nil) —— 这不是错误，只是"这条通道没有内容"。
func FromImageFile(imgPath string) (*Fields, error) {
	f, err := os.Open(imgPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("解码图片: %w", err)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return nil, err
	}
	res, err := qrcode.NewQRCodeReader().Decode(bmp,
		map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_TRY_HARDER: true})
	if err != nil {
		return nil, nil // 没有二维码 / 解不出来
	}
	return ParseQR(res.GetText()), nil
}

// Read 读取一张凭证的精确通道。
//
//	pdfPath 非空且文件存在 → 先取文字层
//	imgPath 非空 → 再解二维码
//
// 两者都拿到就 Merge 并返回冲突；只有一条就返回那条。
func Read(imgPath, pdfPath string) (*Fields, []Conflict, error) {
	var textF, qrF *Fields
	if pdfPath != "" {
		if _, err := os.Stat(pdfPath); err == nil {
			f, err := FromPDF(pdfPath)
			if err != nil {
				// 读不到文字层不该让整条链路失败 —— 还有二维码和模型兜底
				fmt.Fprintf(os.Stderr, "      · 文字层读取失败（退回其它通道）: %v\n", err)
			} else {
				textF = f
			}
		}
	}
	if imgPath != "" {
		f, err := FromImageFile(imgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "      · 二维码解码失败（退回其它通道）: %v\n", err)
		} else {
			qrF = f
		}
	}
	if textF == nil && qrF == nil {
		return nil, nil, nil
	}
	merged, conflicts := Merge(textF, qrF)
	return merged, conflicts, nil
}

// Sufficient 判断精确通道是否已经够用（可以不调模型）。
//
// 发票至少要拿到**发票号码 + 价税合计**：号码是防重复的键，金额是匹配的键。
// 少任何一个都退回模型补。
func (f *Fields) Sufficient() bool {
	return f != nil && f.InvoiceNo != "" && f.AmountCent != nil
}

// HasUpper 表示拿到了票面大写金额（可用于独立校验小写）。
func (f *Fields) HasUpper() bool { return f != nil && strings.TrimSpace(f.AmountUpper) != "" }
