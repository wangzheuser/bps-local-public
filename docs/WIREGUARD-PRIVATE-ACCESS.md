# WireGuard 私网接入（本机 WSL → us）

## 拓扑

本机(WSL2) 10.77.92.2/30  <-- wg-local -->  us 10.77.92.1/30 (UDP 51822)
                                              |
                                              +-- wg-us2 10.77.91.0/30 --> us2 10.77.91.2

## 服务访问地址（私网内）

- API:   http://10.77.92.2:8790
- 管理台: http://10.77.92.2:8791/admin/

## us 侧

- `/etc/wireguard/wg-local.conf`（含 PostUp 转发规则）
- `systemctl enable wg-quick@wg-local`
- 路由转发：wg-us2 <-> wg-local，NAT 10.77.91.0/30 → wg-local

## us2 侧

- `/etc/wireguard/wg-us2.conf` 的 `AllowedIPs = 10.77.91.1/32, 10.77.92.0/30`
- 路由：`ip route add 10.77.92.0/30 via 10.77.91.1 dev wg-us2`

## 本机 WSL 侧

- `/etc/wireguard/wg-local.conf`（PostUp 调用 `/etc/wireguard/up.sh`）
- `/etc/wireguard/up.sh`：DNAT 10.77.92.2:8790/8791 → Windows 主机 `$HOST:8790/8791`
- 启动：`wsl -d Ubuntu-24.04 -u root -e wg-quick up wg-local`

## Windows 侧

- bps-server 需监听 `0.0.0.0:8790` / `0.0.0.0:8791`（BPS_API_ADDR / BPS_ADMIN_ADDR）
- 仅 WG 网段可达；鉴权（API Key / 管理员密码）不变
