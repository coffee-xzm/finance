// Package store 是本地 SQLite 存储层。
//
// 它承担三件"飞书做不到"的事（见 docs/20-synthesis/02-architecture-v0.1.md §3.1）：
//  1. **唯一性**：证据表 sha256 唯一索引 —— 飞书多维表格没有任何唯一索引能力；
//  2. **审计**：append-only 的 audit_log（触发器拦截 UPDATE/DELETE）；
//  3. **可回放**：每条证据保留 provider/model/trace_id/原始指纹。
//
// 约定：金额一律以【分】存 INTEGER；表用 STRICT 防类型混用。
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无 CGO
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type DB struct {
	sql *sql.DB
}

// Open 打开（或创建）数据库并应用迁移。
func Open(path string) (*DB, error) {
	if path == "" {
		path = "data/finance.db"
	}
	if dir := filepath.Dir(path); dir != "." {
		_ = mkdirAll(dir)
	}
	// _pragma 参数在连接建立时生效（modernc.org/sqlite 的写法）
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite: %w", err)
	}
	// SQLite 写锁是库级：把写并发收敛到 1，避免 database is locked
	sdb.SetMaxOpenConns(1)
	if err := sdb.Ping(); err != nil {
		return nil, fmt.Errorf("连接 SQLite: %w", err)
	}
	db := &DB{sql: sdb}
	if err := db.migrate(); err != nil {
		return nil, err
	}
	return db, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// SQL 暴露底层句柄（供测试与只读查询）。
func (d *DB) SQL() *sql.DB { return d.sql }

// migrate 按文件名顺序应用 migrations/*.sql，记录已应用的版本。
func (d *DB) migrate() error {
	if _, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var n int
		if err := d.sql.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := d.sql.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("应用迁移 %s: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
			name, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ── 写入 ──────────────────────────────────────────────────────

// Submission 是一行审批单。
type Submission struct {
	InstanceCode  string
	ApprovalCode  string
	ApprovalName  string
	Status        string
	Applicant     string
	ApplicantDept string
	MaterialType  string
	MaterialName  string
	Buyer         string
	FundSource    string
	AmountCent    *int64
	TaxCent       *int64
	InvoiceDate   string
	Seller        string
	Verdict       string
}

// Evidence 是一行证据（一张图）。
type Evidence struct {
	InstanceCode      string
	Slot              string
	Kind              string
	IndexNo           int
	Filename          string
	MediaType         string
	SHA256            string
	SizeBytes         int64
	AmountInclTaxCent *int64
	TaxCent           *int64
	AmountExclTaxCent *int64
	AmountUpper       string
	Date              string
	Counterparty      string
	Provider          string
	Model             string
	TraceID           string
	Confidence        *float64
	UpperCheck        string
	TaxCheck          string
	LocalPNG          string // 预处理图的本地路径（归档时要重新上传）

	// ── 配对键与发票号码（见 migrations/0004）──
	InvoiceNo    string
	InvoiceNoSrc string // exact | model | ''
	OrderNo      string
	AlipayTxnID  string
}

// DupError 表示命中了 sha256 唯一约束 —— 即"这张图已经进过库"。
//
// 这是本系统**唯一一个靠数据库保证**的判定，不依赖任何上层检查或时序假设。
type DupError struct {
	SHA256       string
	ExistingInst string // 首次占用该图/票号的审批实例
	Reason       string // 空=图片重复；"发票号码"=同一张发票的另一次拍照/导出
}

func (e *DupError) Error() string {
	if e.Reason == "发票号码" {
		return fmt.Sprintf("发票号码 %s 已报销过（首次来自实例 %s）", e.SHA256, e.ExistingInst)
	}
	return fmt.Sprintf("该图已存在（sha256=%s…，首次来自实例 %s）", short(e.SHA256), e.ExistingInst)
}

// SaveInstance 保存一个实例及其全部证据，**整体在一个事务里**。
//
// 任一条证据命中 sha256 唯一约束 → 整个事务回滚并返回 *DupError。
// 这样"部分写入"不会发生：要么这单完整入库，要么完全不入。
func (d *DB) SaveInstance(ctx context.Context, s Submission, evs []Evidence) error {
	return d.SaveInstanceWithGroups(ctx, s, evs, nil)
}

// SaveInstanceWithGroups 与 SaveInstance 相同，但**在同一个事务里**连分组一起写。
// 分组与证据必须同生共死：只写了一半会让"一发票一行"的表出现无依据的行。
func (d *DB) SaveInstanceWithGroups(ctx context.Context, s Submission, evs []Evidence, groups []DocGroup) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	t := now()
	// upsert 提交行：同实例重跑时更新业务字段，保留 first_seen_at
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO submission (instance_code, approval_code, approval_name, status,
			applicant, applicant_dept, material_type, material_name, buyer, fund_source,
			amount_cent, tax_cent, invoice_date, seller, verdict, first_seen_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(instance_code) DO UPDATE SET
			status=excluded.status, applicant=excluded.applicant,
			applicant_dept=excluded.applicant_dept,
			material_type=excluded.material_type, material_name=excluded.material_name,
			buyer=excluded.buyer, fund_source=excluded.fund_source,
			amount_cent=excluded.amount_cent, tax_cent=excluded.tax_cent,
			invoice_date=excluded.invoice_date, seller=excluded.seller,
			verdict=excluded.verdict, updated_at=excluded.updated_at`,
		s.InstanceCode, s.ApprovalCode, s.ApprovalName, s.Status,
		s.Applicant, s.ApplicantDept, s.MaterialType, s.MaterialName, s.Buyer, s.FundSource,
		s.AmountCent, s.TaxCent, s.InvoiceDate, s.Seller, s.Verdict, t, t); err != nil {
		return fmt.Errorf("写入 submission: %w", err)
	}

	// ★ 同实例重跑：先清掉本实例的旧证据行。
	//
	// 否则它**自己的** sha 会撞上唯一索引，被误判成"重复报销" —— 唯一索引要防的是
	// 别的实例复用同一张图，不是本实例自己更新自己。submission 行用 upsert 保留
	// first_seen_at，证据行则整体替换（重新抽取的结果才是最新的）。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM evidence WHERE instance_code = ?`, s.InstanceCode); err != nil {
		return fmt.Errorf("清理本实例旧证据: %w", err)
	}

	for _, e := range evs {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO evidence (instance_code, slot, kind, index_no, filename, media_type,
				sha256, size_bytes, amount_incl_tax_cent, tax_cent, amount_excl_tax_cent,
				amount_upper, date, counterparty, provider, model, trace_id, confidence,
				upper_check, tax_check, local_png, created_at,
				invoice_no, invoice_no_src, order_no, alipay_txn_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.InstanceCode, e.Slot, e.Kind, e.IndexNo, e.Filename, e.MediaType,
			e.SHA256, e.SizeBytes, e.AmountInclTaxCent, e.TaxCent, e.AmountExclTaxCent,
			e.AmountUpper, nullIfEmpty(e.Date), e.Counterparty, e.Provider, e.Model,
			e.TraceID, e.Confidence, e.UpperCheck, e.TaxCheck, e.LocalPNG, t,
			e.InvoiceNo, e.InvoiceNoSrc, e.OrderNo, e.AlipayTxnID)
		if err != nil {
			if isUniqueViolation(err) {
				// 可能是 sha256 冲突（同一张图），也可能是**发票号码**冲突
				// （同一张发票的另一次拍照/导出 —— 字节不同但票号相同）。
				var owner string
				_ = tx.QueryRowContext(ctx,
					`SELECT instance_code FROM evidence WHERE sha256 = ?`, e.SHA256).Scan(&owner)
				if owner == "" && e.InvoiceNo != "" {
					_ = tx.QueryRowContext(ctx,
						`SELECT instance_code FROM evidence WHERE invoice_no = ?`,
						e.InvoiceNo).Scan(&owner)
					return &DupError{
						SHA256:       e.InvoiceNo,
						ExistingInst: owner,
						Reason:       "发票号码",
					}
				}
				return &DupError{SHA256: e.SHA256, ExistingInst: owner}
			}
			return fmt.Errorf("写入 evidence(%s/%s): %w", e.Slot, short(e.SHA256), err)
		}
	}
	if groups != nil {
		if err := saveGroupsTx(ctx, tx, s.InstanceCode, groups); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return d.audit(ctx, s.InstanceCode, "save_instance",
		fmt.Sprintf("verdict=%s evidence=%d groups=%d", s.Verdict, len(evs), len(groups)))
}

// DuplicateOf 只查不写：判断某个 sha256 是否已被占用，返回占用它的实例。
func (d *DB) DuplicateOf(ctx context.Context, sha256 string) (string, bool, error) {
	var owner string
	err := d.sql.QueryRowContext(ctx,
		`SELECT instance_code FROM evidence WHERE sha256 = ?`, sha256).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return owner, true, nil
}

// LocalImages 返回某实例已入库的预处理图路径（归档时重新上传附件用）。
func (d *DB) LocalImages(ctx context.Context, instanceCode string) (map[string][]localImage, error) {
	out := map[string][]localImage{}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT slot, filename, local_png FROM evidence WHERE instance_code=? AND local_png<>''`,
		instanceCode)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var slot, name, png string
		if err := rows.Scan(&slot, &name, &png); err != nil {
			return out, err
		}
		out[slot] = append(out[slot], localImage{Slot: slot, Filename: name, PNG: png})
	}
	return out, rows.Err()
}

// localImage 是一张本地预处理图。
type localImage struct {
	Slot     string
	Filename string
	PNG      string
}

// HasSubmission 判断该实例是否已入库（常驻服务用它避免重复下载与识别）。
func (d *DB) HasSubmission(ctx context.Context, instanceCode string) (bool, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM submission WHERE instance_code = ?`, instanceCode).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// LearnDept 记录部门 open_id → 名称的对照（累积）。
func (d *DB) LearnDept(ctx context.Context, openDeptID, name string) error {
	if openDeptID == "" || name == "" {
		return nil
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO dept_dict(open_dept_id, name, learned_at) VALUES (?,?,?)
		ON CONFLICT(open_dept_id) DO UPDATE SET name=excluded.name`, openDeptID, name, now())
	return err
}

// DeptName 查部门名。
func (d *DB) DeptName(ctx context.Context, openDeptID string) (string, bool, error) {
	var name string
	err := d.sql.QueryRowContext(ctx,
		`SELECT name FROM dept_dict WHERE open_dept_id = ?`, openDeptID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// ── 审计（append-only + 哈希链）────────────────────────────────

// audit 追加一条审计记录，并把上一条的哈希串进来（哈希链）。
func (d *DB) audit(ctx context.Context, entityID, action, detail string) error {
	var prev string
	_ = d.sql.QueryRowContext(ctx,
		`SELECT row_hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&prev)
	at := now()
	row := fmt.Sprintf("%s|%s|%s|%s|%s", at, entityID, action, detail, prev)

	// 纯 Go 实现，避免引入额外依赖
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO audit_log(at, actor, entity, entity_id, action, detail, row_hash, prev_hash)
		VALUES (?,?,?,?,?,?,?,?)`,
		at, "system", "submission", entityID, action, detail, sha256Hex(row), prev)
	return err
}

// AuditCount 返回审计条数（测试与巡检用）。
func (d *DB) AuditCount(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&n)
	return n, err
}

// ── 小工具 ────────────────────────────────────────────────────

// isUniqueViolation 判断是否命中唯一约束。
// modernc 驱动的错误文本形如 "constraint failed: UNIQUE constraint failed: evidence.sha256"。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToUpper(err.Error())
	return strings.Contains(s, "UNIQUE CONSTRAINT FAILED") ||
		strings.Contains(s, "CONSTRAINT_FAILED")
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func now() string { return time.Now().Format(time.RFC3339) }

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Stats 是数据库统计。
type Stats struct {
	Submissions int
	Evidence    int
	DistinctSHA int
	AuditRows   int
	DeptRows    int
}

// Stats 汇总各表行数，并校验唯一性未被破坏。
func (d *DB) Stats(ctx context.Context) (*Stats, error) {
	s := &Stats{}
	for _, q := range []struct {
		sql string
		dst *int
	}{
		{`SELECT COUNT(*) FROM submission`, &s.Submissions},
		{`SELECT COUNT(*) FROM evidence`, &s.Evidence},
		{`SELECT COUNT(DISTINCT sha256) FROM evidence`, &s.DistinctSHA},
		{`SELECT COUNT(*) FROM audit_log`, &s.AuditRows},
		{`SELECT COUNT(*) FROM dept_dict`, &s.DeptRows},
	} {
		if err := d.sql.QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// AllDepts 返回全部部门对照（内存缓存初始化用）。
func (d *DB) AllDepts(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	rows, err := d.sql.QueryContext(ctx, `SELECT open_dept_id, name FROM dept_dict`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return out, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// ── 备份（SQLite 在线备份 + 轮转）──────────────────────────────
//
// 为什么必须做：**本地库是"唯一性"的权威**。
// 飞书多维表格没有任何唯一索引能力，所以一旦本地库丢失，
// "防重复报销"就彻底失效 —— 同一张发票可以再次进库。
//
// 用 `VACUUM INTO`：不需停服务、不会撕裂（比复制文件安全）。

// Backup 生成一份一致性快照到 backupDir，并按 keep 轮转旧备份。
// 返回生成的文件路径。
func (d *DB) Backup(ctx context.Context, backupDir string, keep int) (string, error) {
	if backupDir == "" {
		backupDir = "backup"
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	name := "finance-" + time.Now().Format("20060102-150405") + ".db"
	dest := filepath.Join(backupDir, name)

	// VACUUM INTO 的目标文件必须不存在
	_ = os.Remove(dest)
	if _, err := d.sql.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return "", fmt.Errorf("VACUUM INTO %s: %w", dest, err)
	}
	if err := d.rotate(backupDir, keep); err != nil {
		return dest, err // 备份成功但轮转失败，仍返回路径
	}
	return dest, nil
}

// rotate 只保留最近 keep 份备份（按文件名排序，含时间戳）。
func (d *DB) rotate(backupDir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "finance-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		old := filepath.Join(backupDir, names[0])
		if err := os.Remove(old); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

// ListBackups 列出备份文件（新的在前）。
func (d *DB) ListBackups(backupDir string) ([]string, error) {
	if backupDir == "" {
		backupDir = "backup"
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".db") {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}
