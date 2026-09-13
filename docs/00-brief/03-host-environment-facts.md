# 环境事实（本机实测，用于部署方案）

采集时间：本会话；采集命令输出为准。**未经核实的内容已标注。**

## Linux 主机（当前会话所在主机）
| 项 | 实测值 |
|---|---|
| 发行版 | Ubuntu 22.04.5 LTS (Jammy) |
| CPU | 32 逻辑核 |
| 内存 | 31 GiB（已用 11 GiB，可用约 18 GiB），Swap 8 GiB |
| 根分区 | `/dev/nvme0n1p9` 423 G，已用 330 G（**82%**），可用 74 G |
| 网络接口 | `wlo1 192.168.1.36/24`（无线局域网）、`docker0 172.17.0.1/16`、`tailscale0 100.110.91.90/32` |
| 默认网关 | 192.168.1.1（局域网 192.168.1.0/24） |
| 容器 | Docker 29.8.0 + Docker Compose v5.5.1（已装，`docker ps` 无运行容器） |
| 运行时 | Python 3.10.12、uv 0.11.14、Node v24.11.0（pnpm 存在但调用报错）、git 2.34.1 |
| 缺失组件 | sqlite3 CLI、psql、nginx、caddy、frpc、cloudflared 均未安装 |

## 对方案有直接影响的事实
1. **已有 Tailscale**（`tailscale0`，IP 100.110.91.90）。这意味着：
   - 远程运维、跨设备访问已有成熟通道，不必再自建 VPN；
   - 若需要给飞书回调提供公网 HTTPS，`Tailscale Funnel` 是一条现成路径（**需核实当前 tailnet 是否允许 Funnel、以及是否满足飞书回调域名要求**）。
2. Docker 与 Compose 已就绪 → 推荐用 Compose 编排全部自托管组件，避免污染宿主 Python/Node 环境（宿主 Python 3.10 偏旧，OCR 依赖建议放容器）。
3. 主机为**无线网卡**（wlo1）接入局域网。无线链路对长期在线的服务不是理想宿主；若 NAS 是有线连接或支持容器（群晖/威联通等），**把常驻服务放 NAS、把 OCR/批处理放这台 32 核主机**是更稳的分工。NAS 型号与是否支持 Docker 需用户确认。
4. 根分区已用 82%（仅剩 74 G）。票据原图与备份**必须**落 NAS，不能膨胀根分区。
5. 无 nginx/caddy：反向代理与 TLS 终止需新装（建议 Caddy，自动证书；或直接用 Tailscale Funnel 免证书运维）。

## 待用户确认的环境问题
- NAS 品牌/型号、是否支持 Docker/虚拟机、可用容量、是否有 RAID/快照、是否有线接入局域网。
- 该主机是否长期开机（7×24）？是否 UPS？
- 局域网内是否有其他可用的常驻服务器（例如已有 HomeLab/软路由）。
- 是否有域名（用于飞书回调或内网 HTTPS 证书）；若无，是否接受 Tailscale Funnel / Cloudflare Tunnel 方案。
