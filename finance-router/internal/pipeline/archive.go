// Command archive 把源表里【人工审核=通过 且 未归档】的行复制到整合表。
//
// 这是人工审核闭环的最后一步：机器写源表 → 人在源表点「通过」→ 本命令归档。
//
// 三个设计取舍：
//  1. **复制而非移动**：源表保留全部痕迹（审计要求 append-only）；
//  2. **幂等靠源表的 `已归档` 复选框**：先复制、再回写标记；重复运行不会重复归档；
//  3. **只读人改的字段，绝不写 `人工审核`/`审核备注`**：服务与人分工明确，避免互相覆盖。
//
// 用法：
//
//	go run ./cmd/archive -dry-run    # 只列出待归档
//	go run ./cmd/archive             # 真归档
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
	"github.com/coffee/finance-router/internal/store"
)

// writable 把"读出来的字段值"转成"能写回去的字段值"。
//
// ★ 为什么需要：飞书**读**文本字段返回的是富文本片段数组
// `[{"text":"兴化市永超五金制品有限公司","type":"text"}]`，
// 而**写**文本字段需要纯字符串。直接把数组写回去会报
// `1254060 TextFieldConvFail`（实测踩过）。
//
// 其它形态：人员/多选返回 [{"name":...}]，数字/日期返回标量 —— 分别处理。
func writable(v json.RawMessage) any {
	var segs []map[string]any
	if json.Unmarshal(v, &segs) == nil {
		// 文本片段：拼回字符串
		var sb strings.Builder
		allText := true
		for _, seg := range segs {
			t, ok := seg["text"].(string)
			if !ok {
				allText = false
				break
			}
			sb.WriteString(t)
		}
		if allText {
			return sb.String()
		}
		// 非纯文本数组（如多选/人员）：原样返回（写回多选需要字符串数组）
		return segs
	}
	var anyVal any
	if json.Unmarshal(v, &anyVal) == nil {
		return anyVal
	}
	return nil
}

// carryFields 是从源表复制到整合表的字段（其余留在源表作为过程痕迹）。
var carryFields = []string{
	"审批实例号", "申请编号", "物资所属部门", "物资种类", "物资名称",
	"购买人", "资金来源", "图读金额(元)", "图读税额(元)", "图读日期",
	"销方名称", "审核备注",
	// 一张发票一行：整合表也要能看出"这是哪张票、属于哪一组"。
	"发票号码", "分组序号", "订单号", "归属组",
}

func RunArchive(opts ArchiveOptions) error {
	cfgPath, dryRun := opts.CfgPath, opts.DryRun
	if cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			return err
		}
		cfgPath = p
	}
	if cfgPath == "" {
		p, err := config.FindConfigFile()
		if err != nil {
			return err
		}
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	appToken := cfg.Feishu.Bitable.AppToken
	srcID := cfg.Feishu.Bitable.Tables["submission"]
	dstID := cfg.Feishu.Bitable.Tables["integrated"]
	// 附件**不能跨表复用 file_token**（官方限制），所以归档时要从本地重新上传。
	// ★ 路径从**本地库**读，不读 manifest —— manifest 每次运行被整体重写，
	//   只剩最后一批实例，早先处理的行归档时会"没有图"（这个坑踩过）。
	db, err := store.Open(cfg.Paths.DB)
	if err != nil {
		return fmt.Errorf("打开本地库失败: %w", err)
	}
	defer db.Close()
	if appToken == "" || srcID == "" || dstID == "" {
		return fmt.Errorf("配置缺少 bitable.app_token / tables.submission / tables.integrated")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)

	if opts.Repair {
		return runRepair(ctx, client, db, appToken, dstID, dryRun)
	}

	// 筛选：人工审核=通过 且 已归档 未勾选
	filter := map[string]any{
		"conjunction": "and",
		"conditions": []map[string]any{
			{"field_name": "人工审核", "operator": "is", "value": []string{"通过"}},
			{"field_name": "已归档", "operator": "is", "value": []string{"false"}},
		},
	}
	recs, err := client.SearchBitableRecords(ctx, appToken, srcID, filter, 200)
	if err != nil {
		return fmt.Errorf("筛选待归档记录失败: %w", err)
	}

	// ★ 前置去重：先看看整合表里是否已经有这个源行。
	//
	// 为什么需要：正常路径靠源表的「已归档」复选框做幂等，但**若"复制成功、
	// 回写标记失败"（实测发生过一次，因字段不存在），下次就会重复归档**。
	// 这里以整合表的「来源行」为第二道闸，即使标记失败也不会产生重复。
	archived := map[string]bool{}
	if dst, err := client.SearchBitableRecords(ctx, appToken, dstID, nil, 500); err == nil {
		for _, r := range dst {
			if s := fieldText2(r.Fields["来源行"]); s != "" {
				archived[s] = true
			}
		}
	}
	fmt.Printf("待归档 %d 条（源表 %s → 整合表 %s）\n", len(recs), srcID, dstID)
	if len(recs) == 0 {
		fmt.Println("\n没有待归档的行。去源表把需要确认的行改成「人工审核=通过」再跑。")
		return nil
	}
	if dryRun {
		fmt.Println("（dry-run：不写入）")
	}

	var ok, failed, noAttach, dup int
	for _, r := range recs {
		if archived[r.RecordID] {
			dup++
			fmt.Printf("  = %s 整合表已有该行，跳过（防重复归档）\n", r.RecordID)
			// 顺手把标记补上，让源表状态与事实一致
			if !dryRun {
				_ = client.UpdateBitableRecord(ctx, appToken, srcID, r.RecordID,
					map[string]any{"已归档": true})
			}
			continue
		}
		fields := map[string]any{}
		// 重新上传三张图（跨表不能复用 file_token）
		if inst := textOfField(r.Fields["审批实例号"]); inst != "" {
			up := uploadImages(ctx, client, db, appToken, inst, fields)
			if up == 0 {
				noAttach++
			}
		}
		for _, k := range carryFields {
			v, exists := r.Fields[k]
			if !exists || len(v) == 0 || string(v) == "null" {
				continue
			}
			fields[k] = writable(v)
		}
		fields["来源行"] = r.RecordID
		fields["归档时间"] = time.Now().UnixMilli()

		if dryRun {
			b, _ := json.Marshal(fields)
			fmt.Printf("  → %s  %s\n", r.RecordID, string(b))
			ok++
			continue
		}

		if _, err := client.CreateBitableRecord(ctx, appToken, dstID, fields); err != nil {
			failed++
			fmt.Printf("  ✗ %s 复制失败: %v\n", r.RecordID, err)
			continue
		}
		// 复制成功后才标记 —— 保证"标记了 = 确实归档过"
		err = client.UpdateBitableRecord(ctx, appToken, srcID, r.RecordID, map[string]any{
			"已归档":  true,
			"审核时间": time.Now().UnixMilli(),
		})
		if err != nil {
			fmt.Printf("  ⚠ %s 已复制但标记失败（下次会重复归档，需人工核对）: %v\n", r.RecordID, err)
		}
		ok++
		fmt.Printf("  ✓ %s 已归档\n", r.RecordID)
	}
	fmt.Printf("\n完成：归档 %d，跳过(已存在) %d，失败 %d\n", ok, dup, failed)
	if noAttach > 0 {
		fmt.Printf("注意：%d 条没有带上图片（本地文件已清理或未跑过 extract）—— 图仍可在源表查看。\n", noAttach)
	}
	return nil
}

// uploadImages 把某实例的三张图上传到目标表，填进 fields 的附件字段。
//
// 路径来源有**两级**（这是修"归档没图"的关键）：
//  1. 本地库 evidence.local_png —— 新数据都有；
//  2. **扫 data/extract/files/<实例号>/ 目录** —— 兜底。
//     迁移之前入库的行 local_png 是空的，但图还在磁盘上，
//     按文件名前缀（invoice-/order-/payment-）能还原槽位。
func uploadImages(ctx context.Context, c *feishu.Client, db *store.DB,
	appToken, instanceCode string, fields map[string]any) int {

	var imgs []localImg

	if m, err := db.LocalImages(ctx, instanceCode); err == nil {
		for slot, list := range m {
			for _, x := range list {
				imgs = append(imgs, localImg{slot, x.Filename, x.PNG})
			}
		}
	}
	if len(imgs) == 0 {
		imgs = scanLocalImages(instanceCode)
	}

	up := 0
	for _, im := range imgs {
		b, err := readLocalFile(im.path)
		if err != nil {
			continue
		}
		tok, err := c.UploadMedia(ctx, appToken, "bitable_image", im.filename, b)
		if err != nil {
			continue
		}
		prev, _ := fields[im.slot].([]map[string]string)
		fields[im.slot] = append(prev, map[string]string{"file_token": tok})
		up++
	}
	return up
}

// 文件名前缀 → 审批表单里的槽位名
var slotByPrefix = map[string]string{
	"invoice": "发票文件",
	"order":   "订单截图",
	"payment": "付款截图",
}

// scanLocalImages 扫 data/extract/files/<实例号>/ 还原三张图。
// 用于 local_png 为空的历史数据。
// 注意：返回的 path 必须与 evidence.local_png 的约定一致 ——
// **相对 data/extract**（如 files/<实例号>/invoice-1-1.png），
// 因为 readLocalFile 会自己拼 data/extract 前缀。
// （曾经返回含前缀的路径，导致又被拼一次，读文件失败。）
func scanLocalImages(instanceCode string) []localImg {
	dir := filepath.Join("data/extract", "files", instanceCode)
	rel := filepath.Join("files", instanceCode)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []localImg
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base := strings.ToLower(name)
		slot := ""
		for prefix, s := range slotByPrefix {
			if strings.HasPrefix(base, prefix) {
				slot = s
				break
			}
		}
		if slot == "" {
			continue
		}
		out = append(out, localImg{slot, name, filepath.Join(rel, name)})
	}
	return out
}

// localImg 是一张待上传的本地图。
type localImg struct {
	slot     string
	filename string
	path     string
}

// ArchiveOptions 配置一次归档运行。
type ArchiveOptions struct {
	CfgPath string
	DryRun  bool
	// Repair: 补历史 —— 扫描整合表里**没有附件**的行，从本地把图补传上去。
	// 用于修"归档时没转图"的遗留数据（迁移 0003 之前归档的行）。
	Repair bool
}

func readLocalFile(p string) ([]byte, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join("data/extract", p)
	}
	return os.ReadFile(p)
}

// textOfField 兼容"纯字符串"与"富文本片段数组"两种返回形态。
func textOfField(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var segs []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &segs) == nil && len(segs) > 0 {
		var sb strings.Builder
		for _, x := range segs {
			sb.WriteString(x.Text)
		}
		return sb.String()
	}
	return ""
}

func fieldText2(raw json.RawMessage) string { return textOfField(raw) }

// runRepair 给整合表里缺附件的行补传图片。
//
// 为什么会有"缺附件"的行：归档时本地路径取不到（manifest 被覆盖 + local_png 是后加的列），
// 于是复制了字段但没传图。图其实还在磁盘上，本函数把它们补回去。
func runRepair(ctx context.Context, c *feishu.Client, db *store.DB,
	appToken, dstID string, dryRun bool) error {

	recs, err := c.SearchBitableRecords(ctx, appToken, dstID, nil, 500)
	if err != nil {
		return err
	}
	fmt.Printf("扫描整合表 %d 行，找缺附件的\n", len(recs))

	fixed, skipped := 0, 0
	for _, r := range recs {
		inst := textOfField(r.Fields["审批实例号"])
		if inst == "" {
			continue
		}
		has := 0
		for _, k := range []string{"发票文件", "订单截图", "付款截图"} {
			var arr []any
			if json.Unmarshal(r.Fields[k], &arr) == nil {
				has += len(arr)
			}
		}
		if has > 0 {
			skipped++
			continue
		}
		if dryRun {
			fmt.Printf("  将补图: %s\n", short(inst))
			fixed++
			continue
		}
		fields := map[string]any{}
		up := uploadImages(ctx, c, db, appToken, inst, fields)
		if up == 0 {
			fmt.Printf("  ⚠ %s 本地找不到图，跳过\n", short(inst))
			continue
		}
		if err := c.UpdateBitableRecord(ctx, appToken, dstID, r.RecordID, fields); err != nil {
			fmt.Printf("  ✗ %s 更新失败: %v\n", short(inst), err)
			continue
		}
		fixed++
		fmt.Printf("  ✓ %s 已补 %d 张图\n", short(inst), up)
	}
	fmt.Printf("\n完成：补图 %d 行，跳过（已有图）%d 行\n", fixed, skipped)
	return nil
}
