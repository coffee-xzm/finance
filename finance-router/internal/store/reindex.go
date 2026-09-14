package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ★ 关于"重复报销"的检测位置 —— 这是个容易搞反的地方：
//
//	evidence.sha256 上有 UNIQUE 索引，**库里永远不可能存在重复行**。
//	所以"查库找重复"必然返回空，是假阴性。
//
//	"同一张图出现在多个实例"这个事实，只存在于**磁盘文件**上。
//	正确做法是扫描 data/extract/files/，在入库之前按 sha256 分组。
//	本文件所有重复检测都走 scanExtractFiles。

// extractFile 是扫描到的一个文件（纯文件系统信息，未入库）。
type extractFile struct {
	instance string
	slot     string
	name     string
	path     string
	sha256   string
	size     int64
	// Degraded 表示该 sha 是对**落盘文件**求的哈希，而非原始下载字节。
	// 仅在没有 provenance 旁路表时发生；对 PDF 转出的 PNG 而言这个 sha 是错的。
	Degraded bool
}

// provenanceFile 是每个实例目录下的旁路表（由 pipeline 写入）：
// 落盘文件名 → **原始下载字节**的 sha256。
//
// 为什么必须用它、而不能直接对落盘文件求哈希：
//
//	evidence.sha256 算的是**原始下载字节**（PDF 就是 PDF 的哈希），
//	而磁盘上留的是光栅化后的 PNG。默认不保留 PDF，原始字节随即被删。
//	对 PNG 求哈希得到的是另一个值，重提交同一份 PDF 时永远对不上 ——
//	那样"重建"出来的唯一性保护是假的。
//
// 只有当原始字节没被转换过（JPG/PNG 原样落盘）时，两者才相等，
// 此时缺旁路表也能退化为直接哈希（并标注 degraded）。
const provenanceFile = "_provenance.tsv"

// loadProvenance 读一个实例目录的旁路表：落盘文件名 → 原始 sha256。
func loadProvenance(dir string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(dir, provenanceFile))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) < 2 || p[0] == "" || p[1] == "" {
			continue
		}
		out[p[0]] = p[1] // 后写的覆盖先写的
	}
	return out
}

// scanExtractFiles 扫描 root 下的所有预处理图，**不碰数据库**。
// 返回 (文件列表, 重复组)。重复组 = sha256 → 出现在哪些实例（>1 个才算）。
func scanExtractFiles(root string) ([]extractFile, map[string][]string, error) {
	if root == "" {
		root = "data/extract/files"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 %s: %w", root, err)
	}

	var files []extractFile
	seen := map[string]map[string]bool{} // sha256 → 实例集合

	for _, de := range dirs {
		if !de.IsDir() {
			continue
		}
		instance := de.Name()
		idir := filepath.Join(root, instance)
		entries, err := os.ReadDir(idir)
		if err != nil {
			continue
		}
		prov := loadProvenance(idir)
		for _, fe := range entries {
			if fe.IsDir() {
				continue
			}
			name := fe.Name()
			slot := slotFromFilename(name)
			if slot == "" {
				continue // 含 _provenance.tsv 在内的非图文件
			}
			full := filepath.Join(idir, name)

			// 优先用旁路表里的**原始字节** sha（与 evidence.sha256 同一语义）
			sum, degraded := prov[name], false
			if sum == "" {
				// 没有旁路表（老数据/手工放入的文件）：只能对落盘文件求哈希。
				// 对 JPG/PNG 原样落盘的情况这是正确的；对 PDF 转出的 PNG 是错的。
				sum, err = fileSHA256(full)
				if err != nil {
					continue
				}
				degraded = true
			}

			files = append(files, extractFile{
				instance: instance, slot: slot, name: name,
				path: full, sha256: sum, size: fileSize(full),
				Degraded: degraded,
			})
			if seen[sum] == nil {
				seen[sum] = map[string]bool{}
			}
			seen[sum][instance] = true
		}
	}
	return files, groupDups(seen), nil
}

func groupDups(seen map[string]map[string]bool) map[string][]string {
	dups := map[string][]string{}
	for sum, insts := range seen {
		if len(insts) > 1 {
			list := make([]string, 0, len(insts))
			for i := range insts {
				list = append(list, i)
			}
			sort.Strings(list)
			dups[sum] = list
		}
	}
	return dups
}

// ScanDuplicateGroups 只读扫描：找出"同一张图出现在多个实例"的分组。
// 这是 -dups 的正确实现 —— 读文件，不读库。
// degraded 是没有 provenance 旁路表、只能对落盘文件求哈希的文件数（>0 时结果可能不可靠）。
func ScanDuplicateGroups(root string) (map[string][]string, int, error) {
	files, dups, err := scanExtractFiles(root)
	if err != nil {
		return nil, 0, err
	}
	n := 0
	for _, f := range files {
		if f.Degraded {
			n++
		}
	}
	return dups, n, nil
}

// ReindexFromFiles 从本地磁盘上的预处理图**重建唯一性状态**。
//
// 为什么需要：本地库是"唯一性"的权威。若库被删/重建（或在库存在之前处理过一些实例），
// 唯一索引就失去了历史记录，同一张发票可能被再次放行。
// 本函数扫描 data/extract/files/<实例号>/ 下的图，把缺失的 sha256 补进 evidence，
// 从而恢复保护。重复的会被唯一索引挡下，不会被重复补入。
//
// 返回 (新增条数, 发现的重复组, 错误)。
func (d *DB) ReindexFromFiles(ctx context.Context, root string) (int, map[string][]string, int, error) {
	files, dups, err := scanExtractFiles(root)
	if err != nil {
		return 0, nil, 0, err
	}

	degraded := 0
	for _, f := range files {
		if f.Degraded {
			degraded++
		}
	}

	added := 0
	// ★ 先清掉自己上次插入的占位行，再按当前磁盘状态重建。
	//   reindex 行是纯派生数据（没有 OCR 结果），而陈旧的占位行会用**过时/错误的** sha
	//   参与唯一性判定 —— 既可能挡住不该挡的、又可能放行该挡的，比没有这行更危险。
	//   只删 provider='reindex'，绝不碰真正 OCR 出来的证据行。
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM evidence WHERE provider = 'reindex'`); err != nil {
		return 0, dups, degraded, fmt.Errorf("清理旧 reindex 占位行: %w", err)
	}

	ensured := map[string]bool{}
	for _, f := range files {
		// evidence 有外键指向 submission，缺了要先补一个最小行，
		// 否则 INSERT 会报 FOREIGN KEY constraint failed。
		if !ensured[f.instance] {
			if err := d.ensureSubmission(ctx, f.instance); err != nil {
				return added, dups, degraded, err
			}
			ensured[f.instance] = true
		}
		exists, err := d.hasEvidenceSHA(ctx, f.sha256)
		if err != nil {
			return added, dups, degraded, err
		}
		if exists {
			continue // 已有同图（唯一索引的既有记录），无需再补
		}
		// 补一条最小证据行：只为恢复唯一性，抽取字段留空
		if _, err := d.sql.ExecContext(ctx, `
			INSERT INTO evidence (instance_code, slot, kind, index_no, filename, media_type,
				sha256, size_bytes, amount_upper, date, counterparty, provider, model,
				trace_id, upper_check, tax_check, local_png, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			f.instance, f.slot, kindFromSlot(f.slot), 1, f.name, mediaTypeOf(f.name),
			f.sha256, f.size, "", nil, "", "reindex", "", "", "", "",
			filepath.Join("files", f.instance, f.name), now()); err != nil {
			if isUniqueViolation(err) {
				continue // 并发或已存在，忽略
			}
			return added, dups, degraded, err
		}
		added++
	}
	return added, dups, degraded, nil
}

// ensureSubmission 保证 submission 里有该实例（最小行，仅用于恢复唯一性）。
func (d *DB) ensureSubmission(ctx context.Context, instance string) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO submission (instance_code, approval_code, approval_name, status,
			applicant, applicant_dept, material_type, material_name, buyer, fund_source,
			invoice_date, seller, verdict, first_seen_at, updated_at)
		VALUES (?, '', '', '', '', '', '', '', '', '', NULL, '', 'reindex', ?, ?)
		ON CONFLICT(instance_code) DO NOTHING`, instance, now(), now())
	return err
}

func (d *DB) hasEvidenceSHA(ctx context.Context, sum string) (bool, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evidence WHERE sha256 = ?`, sum).Scan(&n)
	return n > 0, err
}

func slotFromFilename(name string) string {
	b := strings.ToLower(name)
	switch {
	case strings.HasPrefix(b, "invoice"):
		return "发票"
	case strings.HasPrefix(b, "order"):
		return "订单截图"
	case strings.HasPrefix(b, "payment"):
		return "付款记录"
	}
	return ""
}

func kindFromSlot(slot string) string {
	switch slot {
	case "发票":
		return "invoice"
	case "订单截图":
		return "order"
	case "付款记录":
		return "payment"
	}
	return ""
}

func mediaTypeOf(name string) string {
	b := strings.ToLower(name)
	switch {
	case strings.HasSuffix(b, ".png"):
		return "image/png"
	case strings.HasSuffix(b, ".jpg"), strings.HasSuffix(b, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(b, ".pdf"):
		return "application/pdf"
	}
	return "application/octet-stream"
}

func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}
