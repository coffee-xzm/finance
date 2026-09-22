// 临时探针：打印审批定义的 node_list 原始 JSON。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/coffee/finance-router/internal/config"
	"github.com/coffee/finance-router/internal/feishu"
)

func main() {
	code := flag.String("code", "", "审批定义 code")
	flag.Parse()
	p, _ := config.FindConfigFile()
	cfg, err := config.Load(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	c := feishu.NewClient(cfg.Feishu.BaseURL, cfg.Feishu.AppID, cfg.Feishu.AppSecret)
	raw, err := c.RawGet(context.Background(), "/approval/v4/approvals/"+*code)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = raw
	var d map[string]any
	_ = json.Unmarshal(raw, &d)
	data, _ := d["data"].(map[string]any)
	if data == nil {
		data = d
	}
	nodes, _ := json.MarshalIndent(data["node_list"], "", " ")
	fmt.Println(string(nodes))
}
