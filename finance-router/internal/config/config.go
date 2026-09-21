// Package config 加载 config.yml。
//
// 约定（用户指定）：真实配置在 config.yml，不进 git；config.example.yml 进 git，
// 是配置项的唯一文档来源。缺必填项时明确报错，不静默用默认值。
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Path 是本次加载所用的配置文件路径（不来自 YAML，由 Load 填入）。
	// 常驻服务内部再调 pipeline 时，用它保证用的是同一份配置。
	Path         string            `yaml:"-"`
	Server       Server            `yaml:"server"`
	Feishu       Feishu            `yaml:"feishu"`
	OCR          OCR               `yaml:"ocr"`
	PDF          PDF               `yaml:"pdf"`
	DetailByKind map[string]string `yaml:"detail_by_kind"`
	Review       Review            `yaml:"review"`
	Matching     Matching          `yaml:"matching"`
	Notify       Notify            `yaml:"notify"`
	Paths        Paths             `yaml:"paths"`
	Export       Export            `yaml:"export"`
	// Dict 是静态配置字典（字段名/控件 id/选项/rules…）。用 inline 展开到顶层，
	// 于是 config.yml 里可以直接写 approvals: / fields: / options: / rules: 等。
	// 见 dict.go。
	Dict Dict `yaml:",inline"`
}

// Export 是「选择性下载」的落盘配置（见 docs/30-review/32 §5.4）。
type Export struct {
	// Root 是本机产物根目录；留空 = data/export。
	Root string `yaml:"root"`
	// NASRoot 是 NAS 挂载点；留空 = 不复制。
	NASRoot string `yaml:"nas_root"`
	// Zip 控制是否额外产出 <out>.zip（默认 true）。
	Zip *bool `yaml:"zip"`
	// Naming 是命名模板；留空 = 内置默认（与插件侧共用同一份向量）。
	Naming string `yaml:"naming"`
	// GroupBy: department | month | none；留空 = department。
	GroupBy string `yaml:"group_by"`
	// MaxFilesPerBatch 单批文件数上限，防止一次把爆发期全拖下来。
	MaxFilesPerBatch int `yaml:"max_files_per_batch"`
}

// ZipEnabled 返回是否打 zip（默认 true）。
func (e Export) ZipEnabled() bool {
	if e.Zip == nil {
		return true
	}
	return *e.Zip
}

// PDF 去化配置。Qwen3-VL 不支持 PDF 输入，必须在本地光栅化为 PNG。
type PDF struct {
	Rasterizer string `yaml:"rasterizer"` // pdftoppm | pdftocairo | gs | convert
	DPI        int    `yaml:"dpi"`
	MaxPages   int    `yaml:"max_pages"`
	TmpDir     string `yaml:"tmp_dir"`
}

type Server struct {
	Listen string `yaml:"listen"`
}

type Feishu struct {
	AppID        string `yaml:"app_id"`
	AppSecret    string `yaml:"app_secret"`
	ApprovalCode string `yaml:"approval_code"`
	// ApprovalNameExpect 是"我期望 approval_code 对应的审批叫什么"。
	// 非空时，启动会去飞书解析该 code 的真实名称并比对；对不上就拒绝启动。
	// 目的：租户里有多个审批表单（实测有 4 个），config 里填错一个 code
	// 不会有任何报错，只会静默地一条事件都收不到、或收到别的表单的数据。
	ApprovalNameExpect string `yaml:"approval_name_expect"`
	// ApprovalAppID 是拼 applink 用的飞书审批小程序 appId。
	// 留空则用内置的平台默认值（该值对所有租户相同，不是本项目的 app_id）。
	ApprovalAppID string    `yaml:"approval_app_id"`
	UserID        string    `yaml:"user_id"`       // 走 tasks/search 路径时的查询用户（open_id）
	UserIDType    string    `yaml:"user_id_type"`  // open_id | union_id | user_id
	AdminOpenID   string    `yaml:"admin_open_id"` // 收件人 open_id（★ 必须是本应用的 open_id）
	AdminUserID   string    `yaml:"admin_user_id"` // 收件人 user_id（租户内一致，跨应用通用）
	BaseURL       string    `yaml:"base_url"`
	Bitable       Bitable   `yaml:"bitable"`
	Subscribe     Subscribe `yaml:"subscribe"`
	// Approvals 是角色化的审批定义绑定（role → code + 期望名称）。
	// 旧的单个 approval_code 仍兼容（见 ApprovalByRole）。
	Approvals []ApprovalRole `yaml:"approvals"`
}

type Bitable struct {
	AppToken string            `yaml:"app_token"`
	Tables   map[string]string `yaml:"tables"`
	// Bases 是多文档结构：base 名（review/flow）→ app_token + tables。
	// 旧配置只有单个 app_token 时，视为 review base（见 Base()）。
	Bases map[string]BaseConfig `yaml:"bases"`
}

type Subscribe struct {
	Approval             bool `yaml:"approval"`
	BitableRecordChanged bool `yaml:"bitable_record_changed"`
}

type OCR struct {
	Provider       string  `yaml:"provider"`
	BaseURL        string  `yaml:"base_url"`
	APIKey         string  `yaml:"api_key"`
	Model          string  `yaml:"model"`
	ResponseFormat string  `yaml:"response_format"`
	EnableThinking bool    `yaml:"enable_thinking"`
	Temperature    float64 `yaml:"temperature"`
	MaxTokens      int     `yaml:"max_tokens"`
	ImageDetail    string  `yaml:"image_detail"`
	MaxEdgePx      int     `yaml:"max_edge_px"`
	JPEGQuality    int     `yaml:"jpeg_quality"`
	TimeoutSeconds int     `yaml:"timeout_seconds"`
	Concurrency    int     `yaml:"concurrency"`
}

// Review 是人工审核策略。
type Review struct {
	// AutoPassClean: 核对结果=一致 时自动把「人工审核」设为通过。
	// 语义：**只有出错的行才需要人** —— 没察觉到错误就不打扰人。
	AutoPassClean *bool `yaml:"auto_pass_clean"`
}

// AutoPass 返回是否自动通过（默认 true）。
func (r Review) AutoPass() bool {
	if r.AutoPassClean == nil {
		return true
	}
	return *r.AutoPassClean
}

type Matching struct {
	AmountToleranceCent   int64   `yaml:"amount_tolerance_cent"`
	PaymentAfterOrderDays int     `yaml:"payment_after_order_days"`
	CounterpartyThreshold float64 `yaml:"counterparty_threshold"`
}

type Notify struct {
	CadenceDays         []int `yaml:"cadence_days"`
	MaxPerPersonPerWeek int   `yaml:"max_per_person_per_week"`
}

type Paths struct {
	DB         string `yaml:"db"`
	BackupDir  string `yaml:"backup_dir"`
	BackupKeep int    `yaml:"backup_keep"`
}

// Load 读取配置文件并填默认值。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("配置 %s 是空文件 —— 请复制 config.example.yml 并填入真实值", path)
	}
	var c = Config{Dict: DefaultDict()}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if c.Feishu.BaseURL == "" {
		c.Feishu.BaseURL = "https://open.feishu.cn/open-apis"
	}
	if c.Paths.DB == "" {
		c.Paths.DB = "data/finance.db"
	}
	if c.PDF.Rasterizer == "" {
		c.PDF.Rasterizer = "pdftoppm"
	}
	if c.PDF.DPI == 0 {
		c.PDF.DPI = 150
	}
	if c.PDF.MaxPages == 0 {
		c.PDF.MaxPages = 5
	}
	if c.PDF.TmpDir == "" {
		c.PDF.TmpDir = "data/tmp"
	}
	c.Path = path
	if c.Export.Root == "" {
		c.Export.Root = "data/export"
	}
	if c.Export.GroupBy == "" {
		c.Export.GroupBy = "department"
	}
	if c.Export.MaxFilesPerBatch == 0 {
		c.Export.MaxFilesPerBatch = 2000
	}
	if c.DetailByKind == nil {
		c.DetailByKind = map[string]string{
			"invoice": "high",
			"order":   "low",
			"payment": "low",
		}
	}
	return &c, nil
}

// FindConfigFile 从当前目录向上找 config.yml。
//
// 会跳过零字节文件：仓库里若存在空的同名占位文件（历史遗留），不应让它
// 截住查找 —— 否则从仓库根运行时只会得到一句"配置是空的"，掩盖了真正的配置。
func FindConfigFile() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	var skipped []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "config.yml")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if st.Size() > 0 {
				return p, nil
			}
			skipped = append(skipped, p)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if len(skipped) > 0 {
		return "", fmt.Errorf("跳过 %d 个空配置占位文件 %v，未找到可用的 config.yml\n"+
			"  请复制 config.example.yml 为 config.yml 并填入真实值", len(skipped), skipped)
	}
	return "", fmt.Errorf("在 %s 及其上级目录未找到 config.yml", mustGetwd())
}

func mustGetwd() string {
	d, _ := os.Getwd()
	return d
}

// ValidateRecon 校验只读侦察阶段所需的必填项。
//
// 注意：approval_code **不是**读取单条审批实例的必要条件 ——
// 只有"按审批定义枚举实例"这一条路径需要它。另有按用户任务查询的路径
// （POST /approval/v4/tasks/search，只传 user_id 即可），见 Server.UserID。
func (c *Config) ValidateRecon() error {
	var missing []string
	if c.Feishu.AppID == "" {
		missing = append(missing, "feishu.app_id")
	}
	if c.Feishu.AppSecret == "" {
		missing = append(missing, "feishu.app_secret")
	}
	if c.Feishu.ApprovalCode == "" && c.Feishu.UserID == "" {
		missing = append(missing,
			"feishu.approval_code 或 feishu.user_id（二选一：前者按定义枚举，后者按待办任务枚举）")
	}
	if len(missing) > 0 {
		return fmt.Errorf("配置缺少必填项：%v", missing)
	}
	return nil
}
