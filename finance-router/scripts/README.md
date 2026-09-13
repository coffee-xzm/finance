# scripts

## pipeline.sh —— 手动跑一遍

```bash
bash scripts/pipeline.sh 5            # 体检→建库→抽取→落表→通知
bash scripts/pipeline.sh 5 --reset    # 先清空本地库（演示"首次入库"）
```

## 环境：`source ./devenv.sh` 只在**编译**时需要

本机 `~/.cache/go-build` 与 `~/go/pkg` 是**只读挂载**，Go 默认写入会失败。
`devenv.sh` 把缓存改到仓库内。所以：

| 场景 | 是否要 source |
|---|---|
| `go build` / `go run` / `go test` | ✅ **要** |
| 直接跑已编译的二进制 `./bin/serve` | ❌ **不要** |

**推荐**：编译一次，之后直接跑二进制：

```bash
cd finance-router
source ./devenv.sh                  # 只这一次
go build -o bin/serve ./cmd/serve   # 编译
./bin/serve                         # 以后直接跑，不用 source
```
