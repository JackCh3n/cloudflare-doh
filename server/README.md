# Go 版 DoH 代理（server/）

与仓库根目录的 Cloudflare Worker / EdgeOne 中间件**路由规则完全一致**的实现，用于部署在自有服务器（当前：腾讯云日本节点，EdgeOne 回源）。

只依赖 Go 标准库，无第三方模块，编译产物约 5.5MB，单文件直接跑。

## 构建

```bash
cd server
go vet ./...
go test ./...

# 本机（Windows）交叉编译到 Linux
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o doh-proxy .
```

## 运行

```bash
./doh-proxy -listen :8080
# 需要直接提供 HTTPS 时
./doh-proxy -listen :8443 -tls-cert /path/fullchain.pem -tls-key /path/privkey.pem
```

## 环境变量

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `LISTEN_ADDR` | 监听地址（也可用 `-listen`） | `:8080` |
| `DOMAIN_MAPPINGS` | 覆盖整张映射表，JSON 结构与 Worker 版一致 | 内置 6 家 |
| `ALLOWED_HOSTS` | 允许的 Host，逗号分隔；空 = 不限制 | 空 |
| `DEFAULT_UPSTREAM` | 裸路径（`/`、`/dns-query`、`/query-dns`）走哪家 | `cloudflare` |
| `CACHE_TTL` | `GET + application/dns-message` 的 `max-age`，`0` = 只发 `no-store` | `0` |
| `HEALTH_PATH` | 健康检查路径，空 = 关闭 | 空 |
| `VERBOSE` | `1` 时打印每条请求 | 关闭 |

## 与 Worker 版的差异

| 项 | Worker / EdgeOne | Go 版 |
| --- | --- | --- |
| 边缘缓存 | Workers Cache（`cache.enabled`） | 无边缘缓存，只输出 `Cache-Control`，由前置 CDN（EdgeOne）决定 |
| 入口限制 | `workers_dev:false` + `custom_domain` + `ALLOWED_HOSTS` | 端口 + `ALLOWED_HOSTS`（EdgeOne 回源时 Host 是加速域名） |
| 伪装 404 | 有 | 有（页面逐字一致） |
| 裸路径 DoH | 有 | 有 |
| 上游选择 | 同 | 同（6 家，含 dnspod/alidns） |

## 部署到日本节点（已执行过的步骤）

```bash
# 1. 上传二进制
pscp -P 22 doh-proxy root@203.0.113.10:/tmp/doh-proxy

# 2. 安装
ssh -p 22 root@203.0.113.10 \
  "mkdir -p /opt/doh-proxy && install -m 0755 /tmp/doh-proxy /opt/doh-proxy/doh-proxy"

# 3. systemd（单元文件见 scripts/doh-proxy.service）
scp -P 22 scripts/doh-proxy.service root@203.0.113.10:/etc/systemd/system/
ssh -p 22 root@203.0.113.10 "systemctl daemon-reload && systemctl enable --now doh-proxy"

# 4. 验证
curl http://203.0.113.10:8080/healthz
curl -X POST -H 'content-type: application/dns-message' -H 'accept: application/dns-message' \
  --data-binary @q.bin http://203.0.113.10:8080/cloudflare/query-dns
```

## 接入 EdgeOne 加速

EdgeOne（CDN/加速，不是 Pages）侧需要新建**加速域名**并把源站指向日本节点：

| 配置项 | 值 |
| --- | --- |
| 加速域名 | 例如 `doh2.example.com`（需已备案才能用中国大陆节点） |
| 源站类型 | IP / 域名 → `203.0.113.10` |
| 源站协议 | HTTP |
| 源站端口 | `8080` |
| 回源 Host | 与加速域名一致（Go 版默认 `ALLOWED_HOSTS` 为空，不校验；收紧时填该域名） |
| HTTPS | EdgeOne 侧申请免费证书，源站保持 HTTP |
| 缓存 | 建议关闭该域名的缓存规则或设为「遵循源站」，DoH 走 POST 本来也不会被缓存 |

> 注意：EdgeOne 中国大陆节点要求域名完成 ICP 备案；未备案只能用「全球（不含中国大陆）」区域。

## 日本节点实测（2026-10-31，客户端在中国大陆）

| 目标 | 时延 |
| --- | --- |
| `http://203.0.113.10:8080/cloudflare/query-dns`（直连源站） | 8 次 **0.106–0.120s**，非常稳定 |
| `https://doh.example.com/cloudflare/query-dns`（Cloudflare Worker） | 8 次 0.885–3.706s，波动大 |

源站直连比 Cloudflare Worker 快约 3–10 倍，这也是用 EdgeOne 加速它的理由：EdgeOne 大陆节点回源日本，终端只看到国内 CDN 节点。
