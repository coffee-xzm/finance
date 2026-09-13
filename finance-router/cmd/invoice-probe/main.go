// Command invoice-probe 探测电子发票的"精确通道"能拿到什么。
//
// 背景：电子发票 PDF（含全电发票/数电票）通常带**文字层**，且票面有**二维码**。
// 这两条通道都是确定性的：要么读出、要么读不出，不存在"识别错了"。
// 而模型识别（VLM/OCR）是概率性的。本工具用来在排查时快速看清：
//
//	这张发票能不能走精确通道？二维码里到底写了什么？
//
// 用法：
//
//	go run ./cmd/invoice-probe <发票.pdf>
//	go run ./cmd/invoice-probe <已渲染的.png>      # 只探二维码
package main

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: invoice-probe <发票.pdf|图片>")
		os.Exit(1)
	}
	path := os.Args[1]
	ext := strings.ToLower(filepath.Ext(path))

	if ext == ".pdf" {
		probeTextLayer(path)
	}
	probeQR(path, ext)
}

// probeTextLayer 用 pdftotext 取文字层 —— 这是电子发票最可靠的一条通道。
func probeTextLayer(pdf string) {
	fmt.Println("═══ ① 文字层（pdftotext -layout）═══")
	out, err := exec.Command("pdftotext", "-layout", pdf, "-").Output()
	if err != nil {
		fmt.Printf("  ✗ 取文字层失败: %v\n", err)
		return
	}
	text := string(out)
	if strings.TrimSpace(text) == "" {
		fmt.Println("  ✗ 无文字层（扫描件/照片）→ 只能退回二维码或模型识别")
		return
	}
	fmt.Printf("  ✓ 有文字层，%d 字符\n", len([]rune(text)))
	for _, line := range strings.Split(text, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			fmt.Println("   ", s)
		}
	}
}

// probeQR 在图上找二维码并解码。PDF 会先渲染成图。
func probeQR(path, ext string) {
	fmt.Println("\n═══ ② 二维码 ═══")
	imgPath := path

	if ext == ".pdf" {
		dir, err := os.MkdirTemp("", "qrsrc")
		if err != nil {
			fmt.Printf("  ✗ %v\n", err)
			return
		}
		defer os.RemoveAll(dir)
		base := filepath.Join(dir, "page")
		// dpi 用 300：与管道一致；二维码在 150dpi 下也常能读到，但 300 更稳
		if out, err := exec.Command("pdftoppm", "-png", "-r", "300",
			"-f", "1", "-l", "1", path, base).CombinedOutput(); err != nil {
			fmt.Printf("  ✗ 渲染失败: %v (%s)\n", err, strings.TrimSpace(string(out)))
			return
		}
		matches, _ := filepath.Glob(base + "*.png")
		if len(matches) == 0 {
			fmt.Println("  ✗ 未生成页面图")
			return
		}
		imgPath = matches[0]
	}

	f, err := os.Open(imgPath)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		fmt.Printf("  ✗ 解码图片: %v\n", err)
		return
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		fmt.Printf("  ✗ %v\n", err)
		return
	}
	res, err := qrcode.NewQRCodeReader().Decode(bmp,
		map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_TRY_HARDER: true})
	if err != nil {
		fmt.Printf("  ✗ 未找到/未解出二维码: %v\n", err)
		return
	}
	raw := res.GetText()
	fmt.Printf("  ✓ 原文: %s\n", raw)

	// 按已知协议拆开（老式增值税票与数电票字段语义略有差异，见 docs）
	parts := strings.Split(raw, ",")
	names := []string{"固定前缀", "票种代码", "发票代码", "发票号码",
		"金额(含义随票种)", "开票日期", "校验码", "随机码"}
	fmt.Println("  拆解:")
	for i, p := range parts {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		fmt.Printf("    [%d] %-16s %q\n", i, name, p)
	}
}
