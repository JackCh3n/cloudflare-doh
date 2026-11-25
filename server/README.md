# Go 版 DoH 代理（server/）

与仓库根目录的 Cloudflare Worker / EdgeOne 中间件**路由规则完全一致**的实现，用于部署在自有服务器（下文域名与 IP 均为占位示例，替换为你自己的）。

只依赖 Go 标准库，无第三方模块，编译产物约 5.5MB，单文件直接跑。

## 构建

```bash
cd server
go vet ./...
go test ./...

# 本机（Windows）交叉编译到 Linux。-X main.version 把版本号编进二进制：
# ./doh-proxy -version、启动日志和 /metrics 都会报出来（CI 发布的产物由流水线自动注入）
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=1.17.0" -o doh-proxy .
./doh-proxy -version
```

不想本地编译：给仓库打 `v*` 标签后，CI 会把 `doh-proxy-linux-amd64` / `doh-proxy-linux-arm64`、`doh-proxy-windows-amd64.exe` 与 `SHA256SUMS` 发到 [GitHub Releases](https://github.com/JackCh3n/cloudflare-doh/releases)，`scripts/deploy-linux.sh` 默认从这里下载。

## 运行

```bash
# 纯 HTTP（内网 / 直连排障）
./doh-proxy -listen :8080

# 只开 HTTPS：-listen 指定的端口直接变 TLS
./doh-proxy -listen :8080 -tls-cert /opt/doh-proxy/tls/server.crt -tls-key /opt/doh-proxy/tls/server.key

# HTTP + HTTPS 双监听
./doh-proxy -listen :8080 -tls-listen :8444 -tls-cert ... -tls-key ...
```

规则：给了证书而没给 `-tls-listen`，`-listen` 就变 HTTPS；给了 `-tls-listen` 则两个一起跑。
AdGuard Home 的 DoH 上游只接受 `https://`，所以想让 AGH 用它，必须开 TLS。

证书与私钥必须**成对**配置：只给其中一个会拒绝启动并说明原因（否则会悄悄按纯 HTTP 跑起来，以为开了 TLS 的部署实际在明文监听）。证书路径写错、私钥与证书不匹配同样在启动时直接报错。服务端最低 TLS 1.2。

## 环境变量

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `LISTEN_ADDR` | 监听地址（也可用 `-listen`） | `:8080` |
| `TLS_LISTEN` | 额外 TLS 监听（也可用 `-tls-listen`），留空则 `LISTEN_ADDR` 变 HTTPS | 空 |
| `TLS_CERT` / `TLS_KEY` | 证书与私钥路径（也可用 `-tls-cert` / `-tls-key`）；必须成对配置 | 空 |
| `DOMAIN_MAPPINGS` | 覆盖整张映射表，JSON 结构与 Worker 版一致 | 内置 6 家 |
| `ALLOWED_HOSTS` | 允许的 Host，逗号分隔；空 = 不限制 | 空 |
| `DEFAULT_UPSTREAM` | 裸路径（`/`、`/dns-query`、`/query-dns`）走哪家 | `cloudflare` |
| `CACHE_TTL` | `GET + application/dns-message` 的 `max-age`，`0` = 只发 `no-store` | `0` |
| `HEALTH_PATH` | 健康检查路径，空 = 关闭 | 空 |
| `METRICS_PATH` | 运行指标 JSON（版本 / 在途数 / 累计请求 / 上游错误 / 缓存命中与占用 / 各上游健康 / 运行时长） | 空（关闭） |
| `MAX_INFLIGHT` | 全局在途请求上限，满则 `503 + Retry-After` | `512` |
| `MAX_CONNS_PER_HOST` | 单上游连接上限，超出排队（背压），防连接数爆炸 | `64` |
| `MAX_RPS` | 全局限速（请求/秒），`0` = 不限 | `0` |
| `MEM_LIMIT_MB` | Go 软内存上限，到顶先 GC 降速而非 OOM | `512` |
| `READ_TIMEOUT_SEC` / `WRITE_TIMEOUT_SEC` | 读完整请求 / 写响应超时，防 slow-loris 与慢客户端；`0` 与负值按非法值回退默认 | `20` / `60` |
| `CACHE_MAX_TTL` | 内存缓存单条最长保留秒数（同时限制记录 TTL 上限），`0` = 关闭 | `300` |
| `CACHE_ENTRIES` | 缓存 LRU 条目上限（另有 64 MiB 总字节上限） | `10000` |
| `CB_THRESHOLD` | 上游连续失败多少次触发熔断 | `5` |
| `CB_COOLDOWN_SEC` | 熔断冷却秒数，到期后恢复尝试 | `30` |
| `FAILOVER_CHAIN` | 首选上游不可用时按序尝试的其它上游（重复项自动去重） | `cloudflare,google,adguard` |
| `VERBOSE` | `1` 时打印每条请求 | 关闭 |

## 高并发防护（对标 nginx）

| nginx 概念 | 本服务对应实现 |
| --- | --- |
| `worker_connections` | `MAX_INFLIGHT`（信号量槽位，满则 503） |
| `keepalive` 上游连接池 | `MaxIdleConnsPerHost` + `MaxConnsPerHost`（复用 + 背压） |
| `connect_timeout` | 上游建连超时 5 秒：黑洞型故障（SYN 无应答）要尽快失败，同一请求里的故障转移才来得及 |
| `ssl_session_cache` | `tls.NewLRUClientSessionCache(128)`（TLS 会话复用，省重复握手） |
| `client_body_timeout` / `send_timeout` | `ReadTimeout` / `WriteTimeout` |
| `limit_req` | `MAX_RPS`（秒窗口计数） |
| 大文件头攻击 | `MaxHeaderBytes` 16KB（DoH 头很小） |
| 大请求体 | 超过 64KB 直接 413（`Content-Length` 已超限时不必读完），不转发截断的畸形报文 |
| 错误应答不缓存 | 400 / 413 / 429 / 502 / 503 一律 `Cache-Control: no-store`，前置 CDN 不会把一次瞬时错误缓存成粘性故障 |
| panic 隔离 | 每请求 `recover` 并打印堆栈，单请求异常不断服务 |

压测（经 EdgeOne 全链路，2026-11-01）：200 请求 / 50 并发，全部 200，零错误；压测后服务 RSS 13MB、CPU 0.2%，`/metrics` 显示 `upstream_errors: 0`。

systemd 侧还叠了 cgroup 硬护栏，Go 运行时失控也不会拖垮整机：

```ini
MemoryMax=768M      # 硬上限，超过直接 OOM kill 本服务
MemoryHigh=640M     # 软阈值，先施压回收
CPUQuota=200%       # 最多 2 核
TasksMax=512
```

## 应答缓存（本地 LRU）

热门域名第二次查询直接本地返回，不再打上游。

- **缓存键 = 上游名 + DNS flags + question 段 + EDNS 信息**（QNAME/QTYPE/QCLASS，是否带 OPT，是否置 DO 位）。事务 ID 不参与，因为不同客户端 ID 不同但应答相同
- **EDNS 信息为什么要进键**：自己做 DNSSEC 验证的客户端（DO=1）要的应答里有 RRSIG 与 OPT，不带的客户端拿到的没有。两者共用一个键的话，先到的那份应答会被错发给另一种客户端
- **只缓存「确实是这条查询的完整 DNS 应答」**：媒体类型是 `application/dns-message`、QR=1、事务 ID 与 question 原样回显、未截断（TC=0）、`rcode=0/3` 且最小记录 TTL>0。`rcode=3` 且无 SOA 的应答（没有负缓存 TTL）自然不缓存
  - 截断的应答只是完整应答的一部分：缓存之后，带了更大 EDNS 缓冲区的客户端也会一直拿到它，而它自己重新问一次又命中同一份，等于这次查询在 TTL 内永远解析不完
  - 上游用 200 返回的非 DNS 内容（例如门户认证页）不会进缓存
- **命中时**：把应答 ID 改写成客户端的 ID，并按已过秒数**递减所有记录 TTL**（不做递减会返回"过期"的 TTL，客户端缓存会集体早退）
- **跳过伪记录**：EDNS0 OPT（type 41）的 TTL 字段是扩展 RCODE/标志位、恒为 0，参与计算会把整条应答误判成不可缓存 —— 这是实测踩出来的坑（doh.pub 的应答全带 OPT）
- 单条 TTL = `min(记录最小 TTL, CACHE_MAX_TTL)`，过期即淘汰
- **双重上限**：条目数（`CACHE_ENTRIES`）与总字节（64 MiB）。只限条目数不够：单条应答最大 64KB，10000 条最坏能撑到约 640MB；单条就超过总预算的应答不入缓存
- `?dns=` 参数按 RFC 8484 是不带填充的 base64url，带填充或标准 base64（`+` `/`）的写法也接受，都能命中同一份缓存
- 响应头 `X-Cache: HIT|MISS` 便于排障；`/metrics` 的 `cache_hits` / `cache_misses` / `cache_entries` / `cache_bytes` / `cache_max_bytes` 反映运行情况

实测（日本源站本地，`/dnspod` 上游在国内约 200ms）：

```
第 1 次  0.215s  X-Cache: MISS
第 2 次  0.0057s X-Cache: HIT
第 3 次  0.0055s X-Cache: HIT     → 快约 39 倍
```

## 上游熔断与故障转移

- 每个上游独立计数：连续失败 `CB_THRESHOLD` 次即熔断，冷却 `CB_COOLDOWN_SEC` 内不再尝试；冷却结束后恢复尝试（期间到达的请求都会打过去，并不是只放行一个探测），成功一次即清零
- 判定失败：连接 / 超时错误，上游返回 `5xx`，或上游返回 `429`（限流）。`429` 是上游在限「本机出口」的量，换一家才能把这次查询做成功
- 其它 `4xx`（如 400）是查询本身的问题：原样透传、不转移、不计失败
- **客户端先断开不算上游失败**：客户端中途取消时，发往上游的请求必然报错。把它记成失败的话，几个没耐心的客户端就能把整条候选链逐家熔断
- **上游建连超时 5 秒**：黑洞型故障（SYN 无应答）要等满 15 秒的整体超时才失败的话，同一请求里的故障转移形同虚设
- 熔断期间按 `FAILOVER_CHAIN` 顺序转移到下一个健康上游（同一请求内完成，客户端无感）；链里的重复项自动去重
- 转移目标与裸路径一样走上游的 DoH 端点：`pathMapping` 把 `/dns-query` 或 `/query-dns` 映射到别处时（如 `dns.nextdns.io/<profile>`），端点路径跟着映射走
- 全部上游都不可用时返回 `502/503 + Retry-After`，且 `Cache-Control: no-store`
- `/metrics` 的 `providers` 字段暴露每个上游的 `healthy` / `consecutive_failures`

## 与 Worker 版的差异

v1.5.0 起 Worker / EdgeOne 版已同步本实现的应答缓存与熔断/故障转移，差异只剩运行时模型：

| 项 | Worker / EdgeOne | Go 版 |
| --- | --- | --- |
| 进程内应答缓存 | 有（isolate 级 best-effort，环境变量同名） | 有（进程级稳定，实测 215ms→5.5ms） |
| 熔断/故障转移 | 有（`CB_*` / `FAILOVER_CHAIN`，isolate 内计数） | 有（进程级） |
| healthz / metrics | 有（`HEALTH_PATH` / `METRICS_PATH`，metrics 为单 isolate 口径） | 有（进程级口径） |
| 边缘缓存提示 | Workers Cache（`cache.enabled`，按 `CACHE_TTL`） | 只输出 `Cache-Control`，由前置 CDN（EdgeOne）决定 |
| 并发/内存护栏 | 平台托管（无 `MAX_INFLIGHT` 等） | 用户态护栏 + systemd cgroup 硬护栏 |
| 入口限制 | `workers_dev:false` + `custom_domain` + `ALLOWED_HOSTS` | 端口 + TLS + `ALLOWED_HOSTS` |
| 伪装 404 / 裸路径 DoH | 有 | 有（页面逐字一致） |
| 上游选择 | 6 家（含 dnspod / alidns） | 相同 |

## 部署到日本节点

```bash
# 1. 上传二进制（<SSH端口>/<服务器IP> 换成你自己的）
pscp -P <SSH端口> doh-proxy root@203.0.113.10:/tmp/doh-proxy

# 2. 安装
ssh -p <SSH端口> root@203.0.113.10 "mkdir -p /opt/doh-proxy && install -m 0755 /tmp/doh-proxy /opt/doh-proxy/doh-proxy"

# 3. 生成自签证书（10 年），见下一节
# 4. 装 systemd 单元并启动
scp -P <SSH端口> scripts/doh-proxy.service root@203.0.113.10:/etc/systemd/system/
ssh -p <SSH端口> root@203.0.113.10 "systemctl daemon-reload && systemctl enable --now doh-proxy"

# 5. 验证（源站已开 TLS）
curl -k https://203.0.113.10:8080/healthz
curl -k -X POST -H 'content-type: application/dns-message' -H 'accept: application/dns-message' \
  --data-binary @q.bin https://203.0.113.10:8080/cloudflare/query-dns
```

一键脚本：`scripts\deploy-jp.bat`（需先 `set JP_SSH_PASS=...`）。

服务器侧通用一键脚本 `scripts/deploy-linux.sh`（在目标服务器执行：自动下载 Release 二进制 → `/opt/doh-proxy` → 生成 systemd 单元 → 启动 → 健康检查；已有单元时默认只换二进制不覆盖配置，幂等可重复执行）：

```bash
curl -fsSL https://raw.githubusercontent.com/JackCh3n/cloudflare-doh/main/scripts/deploy-linux.sh | sudo bash
```

也可以用 Docker：镜像 `ghcr.io/jackch3n/cloudflare-doh`（CI 打 tag 自动构建，amd64/arm64），用法见根目录 README「Docker 部署」。

## 接入 EdgeOne 加速（示例：`dohj.example.com`）

EdgeOne（CDN/加速，不是 Pages）侧新建**加速域名**，源站指向日本节点：

| 配置项 | 值 |
| --- | --- |
| 加速域名 | `dohj.example.com`（示例；需已备案才能用中国大陆节点） |
| 源站 | `203.0.113.10` |
| 源站协议 / 端口 | **HTTPS** / `8080` |
| 回源 Host | 与加速域名一致 |
| HTTPS | EdgeOne 侧申请免费证书（边缘证书），源站用自签即可 |
| 缓存 | 关闭缓存或设为「遵循源站」，DoH 走 POST 本来也不缓存 |

> 关键点：EdgeOne 严格按配置的协议回源。源站只有 HTTP 而回源协议选了 HTTPS，会立刻返回
> `525 SSL Handshake Failed with Origin Server`。两种解法：把回源协议改成 HTTP，或按下面给源站加 TLS
> （本仓库选了后者，整条链路保持加密）。

### 源站自签 TLS（10 年）

EdgeOne **不校验源站证书**——证据是：换证前源站证书已过期 5 个月，EdgeOne 仍能正常回源。所以自签够用：

```bash
mkdir -p /opt/doh-proxy/tls
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout /opt/doh-proxy/tls/server.key -out /opt/doh-proxy/tls/server.crt \
  -subj '/CN=dohj.example.com' \
  -addext 'subjectAltName=DNS:dohj.example.com,IP:203.0.113.10'
chmod 600 /opt/doh-proxy/tls/server.key
chmod 644 /opt/doh-proxy/tls/server.crt
```

systemd 里用 `-listen :8080 -tls-cert ... -tls-key ...`，即 `:8080` 直接变 HTTPS（`scripts/doh-proxy.service` 已按此配置）。

### 收紧 Host 白名单

```
Environment=ALLOWED_HOSTS=dohj.example.com,203.0.113.10
```

实测三种情形：经 EdgeOne（Host=`dohj.example.com`）→ 200；直连 IP（Host=`203.0.113.10`）→ 200；伪造 Host → 伪装 404。

## 上线实测（2026-10-31，POST 真实查询）

| 路径（上海客户端） | 时延 |
| --- | --- |
| `https://dohj.example.com/cloudflare/query-dns`（EdgeOne → 日本） | 5 次 **0.171–0.216s**，非常稳定 |
| `https://doh.example.com/cloudflare/query-dns`（Cloudflare Worker） | 8 次 0.63–2.17s |
| `https://203.0.113.10:8080/...`（直连源站，绕过 EdgeOne） | 约 0.27s，但抖动大（0.28–1.35s） |

四家上游经 `dohj`：cloudflare 0.20s、google 0.26s、adguard 0.57s、dnspod 0.57s，全部 200。

源站证书：自签 10 年（CN 取加速域名）。

## 版本号

CI 发布的二进制与镜像都把 `package.json` 里的版本号编进了二进制，三处都能看到：

```bash
./doh-proxy -version            # 打印版本后退出
journalctl -u doh-proxy | head  # 启动日志第一行：doh-proxy X.Y.Z 启动：...
curl -s http://127.0.0.1:8080/metrics | grep -o '"version":"[^"]*"'
```

自己 `go build` 出来的二进制是 `dev`；要带版本号需要加 `-ldflags "-X main.version=X.Y.Z"`。`scripts/deploy-linux.sh` 安装完成后会打印所装二进制的版本。

## 常见问题

| 现象 | 原因 / 处理 |
| --- | --- |
| EdgeOne 返回 `525` | 回源协议选了 HTTPS 但源站没 TLS。改成 HTTP 回源，或按上文给源站加 TLS |
| 浏览器提示证书错误 | 源站是自签证书，属正常；公网应访问 EdgeOne 加速域名（边缘证书有效） |
| AGH 报“上游地址无效” | AGH 只接受 `https://`，`http://IP:8080` 填不进去 |
| `/healthz` 返回 404 | 没设 `HEALTH_PATH`，该接口默认关闭 |
| 启动即退出，提示 `TLS_CERT … 与 TLS_KEY … 必须同时配置` | 证书与私钥只配了其中一个。成对配置，或都清空走纯 HTTP |
| 启动即退出，提示 `加载 TLS 证书失败` | 证书路径写错，或私钥与证书不匹配；错误信息里带具体文件与原因 |
| 某家上游一直被熔断，但它其实是通的 | 看是不是黑洞型故障（SYN 无应答）：建连超时 5 秒后才会转移。客户端主动断开不再计入失败；上游返回 `429` 会被当成限流并转移 |
| 自定义上游的端点不在 `/dns-query`（如 `dns.nextdns.io/<profile>`），裸路径 `/dns-query` 与故障转移打不到 | 裸路径与故障转移走的是上游的 DoH 端点：在 `DOMAIN_MAPPINGS` 里用 `pathMapping` 把 `/dns-query` 或 `/query-dns` 映射到真实路径 |
| `/metrics` 里 `version` 是 `dev` | 二进制不是 CI 发布的产物（本地编译未注入版本号） |
