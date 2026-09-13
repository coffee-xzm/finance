// Command pdf2png 把 PDF 票据转成 PNG，交给下游视觉模型识别。
//
// 为什么需要：SiliconFlow 上 Qwen3-VL 系列**不支持 PDF 输入**（文档"支持模型概览"里
// PDF 只标注在 DeepSeek-OCR 上）。所以 PDF 必须在本地先光栅化。
//
// 为什么本地转而不是换模型：本地转免费、可控、可调 DPI —— 而 DPI 直接决定
// 视觉 token 数（Qwen 系列 token ≈ ceil(h/28)*ceil(w/28)），是最大的一根成本杠杆。
//
// 用法：
//
//	go run ./cmd/pdf2png -in invoice.pdf -out outdir -dpi 150
//
// 输出：outdir/<basename>-p1.png, -p2.png, ...
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	var (
		in   = flag.String("in", "", "输入 PDF 路径（必填）")
		out  = flag.String("out", "", "输出目录（默认与输入同目录）")
		dpi  = flag.Int("dpi", 150, "光栅化 DPI")
		page = flag.Int("page", 0, "只转指定页（1 起；0 = 全部）")
	)
	flag.Parse()

	if *in == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*in, *out, *dpi, *page); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %v\n", err)
		os.Exit(1)
	}
}

func run(in, out string, dpi, page int) error {
	if _, err := os.Stat(in); err != nil {
		return fmt.Errorf("输入文件不可读: %w", err)
	}
	if !strings.EqualFold(filepath.Ext(in), ".pdf") {
		return fmt.Errorf("只接受 .pdf（收到 %s）—— 图片无需本工具，直接送模型", filepath.Ext(in))
	}
	if out == "" {
		out = filepath.Dir(in)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	base := strings.TrimSuffix(filepath.Base(in), filepath.Ext(in))
	prefix := filepath.Join(out, base)

	args := []string{"-png", "-r", fmt.Sprint(dpi)}
	if page > 0 {
		args = append(args, "-f", fmt.Sprint(page), "-l", fmt.Sprint(page))
	}
	args = append(args, in, prefix)

	bin, err := exec.LookPath("pdftoppm")
	if err != nil {
		return fmt.Errorf("未找到 pdftoppm（安装：apt install poppler-utils）: %w", err)
	}

	fmt.Printf("转换: %s → %s-*.png  (%d dpi)\n", in, prefix, dpi)
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pdftoppm 执行失败: %w", err)
	}

	files, _ := filepath.Glob(prefix + "-*.png")
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("未生成任何 PNG —— 检查 PDF 是否加密或为空")
	}

	fmt.Printf("\n生成 %d 页:\n", len(files))
	for _, f := range files {
		st, _ := os.Stat(f)
		w, h, derr := pngSize(f)
		dim := "尺寸未知"
		if derr == nil {
			dim = fmt.Sprintf("%dx%d", w, h)
		}
		fmt.Printf("  %-40s %8.1f KB  %s  视觉token≈%d\n",
			filepath.Base(f), float64(st.Size())/1024, dim, estTokens(w, h))
	}

	fmt.Printf("\n提示：Qwen 系列视觉 token ≈ ceil(h/28)*ceil(w/28)。\n")
	fmt.Printf("      DPI 越低 token 越少但小字越糊；票据建议 150 dpi 起，识别不出票号再升到 200。\n")
	return nil
}

// pngSize 从 PNG 文件头直接读宽高（PNG: 8 字节签名 + 4 字节长度 + 'IHDR' + w + h）。
func pngSize(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	hdr := make([]byte, 24)
	if _, err := f.Read(hdr); err != nil {
		return 0, 0, err
	}
	if string(hdr[12:16]) != "IHDR" {
		return 0, 0, fmt.Errorf("不是 PNG")
	}
	be := func(b []byte) int {
		return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	}
	return be(hdr[16:20]), be(hdr[20:24]), nil
}

func estTokens(w, h int) int {
	if w == 0 || h == 0 {
		return 0
	}
	cw := (w + 27) / 28
	ch := (h + 27) / 28
	return cw * ch
}
