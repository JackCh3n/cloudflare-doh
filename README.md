# Cloudflare DoH 转发代理（Cloudflare / EdgeOne / 自建 Go 三端）

![使用演示](https://i.imgur.com/hu6F10P.gif)

这是一个基于边缘函数运行的 DNS over HTTPS (DoH) 转发代理服务。它按**请求路径前缀**把请求反向代理到不同的 DoH 提供商，并原样保留查询参数与请求体。

同一套路由规则有四个部署入口：

| 平台 | 入口文件 | 运行时 | 状态 |
| --- | --- | --- | --- |
| Cloudflare Workers | `_worker.js` | V8 Worker | 已验证 |
| Cloudflare Pages | `_worker.js`（Pages 高级模式） | V8 Worker | 已验证（已降级为备选） |
| 腾讯 EdgeOne Pages | `middleware.js`（边缘中间件） | V8 Edge Middleware | 已验证（2026-10-31 线上部署通过） |
| 自建服务器 + EdgeOne 加速 | `server/main.go` | Go 1.22 单进程 | 已上线（源站直连实测 ~110ms） |

## 功能特点

- **基于路径转发**：访问 `doh.example.com/google/query-dns` → `dns.google/dns-query`
- **平台无关核心**：路由/代理逻辑集中在 `src/core.js`，两个平台只写十几行适配层
- **参数与请求体完整透传**：保留查询串，支持 DoH 的 GET 与 POST（`application/dns-message` 二进制体）
- **路径映射可配置**：可用环境变量 `DOMAIN_MAPPINGS` 覆盖，也可以直接改默认映射
- **伪装 404**：未命中映射的路径（含站点根路径）返回一个「浏览器断网」样式的 404 页面，不暴露代理特征
- **应答缓存与熔断转移（v1.5.0 起，三端一致）**：进程内 LRU 缓存热门查询（键不含事务 ID，POST 也命中，命中时改写 ID 并按存活时间递减 TTL、跳过 OPT）；上游连续失败熔断、按 `FAILOVER_CHAIN` 同请求内无缝转移，全部不可用时 502/503 + Retry-After

## 目录结构

```
cloudflare-doh/
├── _worker.js            # Cloudflare Workers / Pages 入口（薄适配层）
├── middleware.js         # 腾讯 EdgeOne Pages 入口（边缘中间件）
├── index.html            # 静态兜底页（内容与伪装 404 一致）
├── src/
│   └── core.js           # 共享核心：默认映射、路径解析、代理转发、伪装 404
├── server/               # Go 版实现（自建服务器 + EdgeOne 加速），见 server/README.md
│   ├── main.go           # 单文件服务：路由、转发、白名单、缓存头、优雅退出
│   ├── main_test.go      # 单元测试（映射/白名单/伪装 404/缓存策略/裸路径）
│   ├── go.mod
│   └── README.md
├── test/
│   └── smoke.mjs         # 零依赖冒烟测试（node test/smoke.mjs）
├── scripts/
│   ├── dev.bat           # 本地调试：杀旧进程 → 装依赖 → 启动 → 打开浏览器
│   ├── kill-dev.bat      # 结束占用 8787/8088/6699 的调试进程
│   ├── deploy-edgeone.bat# 一键部署到 EdgeOne Pages
│   ├── deploy-jp.bat     # Windows 交叉编译 + 上传日本节点（plink/pscp）
│   ├── deploy-linux.sh   # Linux 服务器一键部署（Releases/本地二进制 → systemd）
│   ├── doh-proxy.service # systemd 单元模板
│   ├── pack-edgeone.mjs  # 生成手动上传目录 dist/edgeone（3 个文件）
│   └── check-doh.mjs     # 线上自检：真实 DoH 查询并解析 A 记录
├── Dockerfile            # Go 版容器镜像（多阶段构建，amd64/arm64）
├── .cnb.yml              # CNB（cnb.cool）构建配置：分支/tag 推送自动构建 Docker 制品
├── .github/
│   └── workflows/
│       └── release.yml   # 打 tag v* → 多平台二进制发 Releases + Docker 发 GHCR
├── dist/edgeone/         # 手动上传产物（git 忽略，npm run pack:edgeone 生成）
├── docs/
│   └── adguard-home.md   # AdGuard Home 分流配置、延迟实测、腾讯游戏等分流清单
├── wrangler.jsonc        # Cloudflare 部署配置
└── package.json
```

## 工作原理

1. 取请求路径，按**最长前缀**匹配映射表（避免 `/google` 抢占 `/google-v2`）
2. 命中后去掉前缀，按 `pathMapping` 重写剩余路径，拼出目标 URL 并保留查询串
3. 用 `fetch` 反向代理：剥离 `host`/`content-length`，非 GET/HEAD 方法把请求体读成 `ArrayBuffer` 再转发
4. 未命中则返回伪装 404 页面

### 默认路径映射

| 前缀 | 目标 | 路径重写 |
| --- | --- | --- |
| `/google` | `dns.google` | `/query-dns` → `/dns-query` |
| `/cloudflare` | `one.one.one.one` | `/query-dns` → `/dns-query` |
| `/quad9` | `dns.quad9.net` | `/query-dns` → `/dns-query` |
| `/adguard` | `dns.adguard-dns.com` | `/query-dns` → `/dns-query` |
| `/dnspod` | `doh.pub`（国内可达） | `/query-dns`、`/dns-query` → `/dns-query` |
| `/alidns` | `dns.alidns.com`（国内可达） | `/query-dns` → `/dns-query` |

> 前缀之后没有剩余路径时会补 `/`，例如 `/google?name=a.com` → `https://dns.google/?name=a.com`。

### 部署位置决定能用哪几家上游（真实域名实测）

同一份代码，边缘节点所在地区不同，能连通的上游完全不同。下表用 `scripts/check-doh.mjs` 对三个线上站点各发真实 DoH 查询（`POST application/dns-message`，域名 `qq.com` / `test2.example.com`），判据是「返回 `rcode=0` 且至少一条 A 记录」；`502 BAD_GATEWAY` / `504 GATEWAY_TIMEOUT` / `505` 都是 EdgeOne 边缘自己返回的错误页，说明请求没到上游。

| 站点 | google | cloudflare | quad9 | adguard | dnspod | alidns |
| --- | --- | --- | --- | --- | --- | --- |
| **Cloudflare Worker（`doh.example.com`）** | ✅ 2/2 | ✅ 2/2 | ✅ 2/2 | ✅ 2/2 | ✅ 2/2 | ✅ 2/2 |
| `dohcn.example.com`（EdgeOne `-a global`，大陆边缘） | ❌ 504 | ❌ 504 | ❌ 504 | ❌ 504 | ✅ 2/2 | ✅ 2/2 |
| EdgeOne `-a overseas`（203.0.113.30） | ✅ 2/2 | ⚠️ 1/2（qq 通过，test2 504） | ❌ 502/505 | ✅ 2/2 | ✅ 2/2 | ✅ 2/2 |
| EdgeOne 国际版（`.edgeone.dev`） | ✅ 2/2 | ⚠️ 1/2（qq 504，test2 通过） | ❌ 505/502 | ✅ 2/2 | ✅ 2/2 | ✅ 2/2 |

参照值（同一批查询的真实应答）：

- `qq.com` → `123.150.76.218`、`203.205.254.157`、`113.108.81.189`（腾讯网）
- `test2.example.com` → `76.76.21.x` 段（Vercel 托管的测试域）

结论：**Cloudflare Worker 是唯一六家上游全通的部署方式**（12/12，实测 1.0–3.1s，含 TLS 握手）。EdgeOne 侧三家都有短板：Quad9 全军覆没、Cloudflare 约一半成功率（且各站失败的域名还不一样）、Google / AdGuard 只在海外节点可用；只有 dnspod / alidns 在哪都稳。所以：**要全功能就用 Worker，EdgeOne 那套适合只跑国内上游的场景。**

## 边缘版与 Go 版的能力对照

v1.5.0 起三端能力对齐，差异只剩运行时模型：

| 能力 | Worker / EdgeOne（边缘） | Go 版（自建） |
| --- | --- | --- |
| 路径转发 / 伪装 404 / 裸路径 DoH | ✅ | ✅ |
| 进程内应答缓存（POST 也命中） | ✅ isolate 级，best-effort | ✅ 进程级，稳定（实测 215ms→5.5ms） |
| 上游熔断 + `FAILOVER_CHAIN` 转移 | ✅（isolate 内计数） | ✅（进程级） |
| 健康检查 / metrics | ✅（`HEALTH_PATH` / `METRICS_PATH`，默认关） | ✅（默认关） |
| `Cache-Control` 边缘缓存提示（`CACHE_TTL`） | ✅ | ✅ |
| 并发闸门 / 限速 / 内存护栏（`MAX_INFLIGHT` 等） | ➖ 并发治理归平台 | ✅ + systemd cgroup 硬护栏 |
| TLS 监听 / 自签证书 | ➖ 平台终结 TLS | ✅ |

> **isolate 说明**：边缘函数的缓存与熔断计数存活于单个 isolate（同一节点通常复用一段时间，
> 部署或闲置回收后重置），是 best-effort 而非承诺值；热门域名的命中率随流量与节点分布浮动，
> `/metrics` 的数字也是单 isolate 口径。缓存键/报文改写/熔断阈值等语义与 Go 版逐条一致。

## 配置

### 环境变量一览

| 变量 | 作用 | 默认 |
| --- | --- | --- |
| `DOMAIN_MAPPINGS` | 覆盖整张路径映射表（JSON 字符串或对象） | 内置 6 家 |
| `ALLOWED_HOSTS` | 允许访问的 Host 白名单，逗号分隔；配置后其余域名一律返回伪装 404 | 不限制 |
| `CACHE_TTL` | `GET + application/dns-message` 响应的缓存秒数（响应头，供浏览器/CDN 用）；`0` 表示完全不缓存 | `0` |
| `DEFAULT_UPSTREAM` | 裸路径（`/`、`/dns-query`、`/query-dns`）兜底走哪家上游 | `cloudflare` |
| `CACHE_MAX_TTL` | 进程内应答缓存单条最长保留秒数（同时限制记录 TTL 上限），`0` = 关闭 | `300` |
| `CACHE_ENTRIES` | 进程内缓存 LRU 条目上限 | `10000` |
| `CB_THRESHOLD` | 上游连续失败多少次触发熔断 | `5` |
| `CB_COOLDOWN_SEC` | 熔断冷却秒数，到期后放行探测 | `30` |
| `FAILOVER_CHAIN` | 首选上游熔断后按序转移的其它上游，逗号分隔（空 = 禁用转移） | `cloudflare,google,adguard` |
| `HEALTH_PATH` | 健康检查路径，命中返回 200 ok；空 = 关闭 | 空 |
| `METRICS_PATH` | 运行指标 JSON（累计请求/上游错误/缓存命中/各上游健康），空 = 关闭 | 空 |

> `CACHE_TTL` 只对 GET 生效（浏览器/CDN 层）；进程内缓存（`CACHE_MAX_TTL`）对 GET/POST 一视同仁，AdGuard Home 的 POST 查询同样受益。

地址形式都可用（`DEFAULT_UPSTREAM=cloudflare` 时等价）：

```
https://doh.example.com/                     # 仅当请求头是 DoH 协议时才转发，浏览器访问仍是伪装 404
https://doh.example.com/dns-query            # 标准 DoH 路径
https://doh.example.com/query-dns            # 本项目的别名
https://doh.example.com/cloudflare/query-dns # 显式指定上游
```

Cloudflare 侧写在 `wrangler.jsonc` 的 `vars`（或控制台「变量和机密」）；EdgeOne 侧写在项目根目录 `.env`。

### 方式一：环境变量 `DOMAIN_MAPPINGS`（不重新发版）

格式为 JSON（对象或 JSON 字符串均可）：

```json
{
	"/demo": {
		"targetDomain": "dns.example.com",
		"pathMapping": {
			"/query-dns": "/dns-query"
		}
	}
}
```

- **Cloudflare**：控制台 → 该 Worker/Pages 项目 → 变量和机密 → 新增 `DOMAIN_MAPPINGS`
- **EdgeOne Pages**：写在项目根目录 `.env` 中（构建时注入 `context.env`），或 `edgeone makers env set DOMAIN_MAPPINGS '<json>'`

> 注意：配置了 `DOMAIN_MAPPINGS` 会**整体覆盖**默认映射，需要把保留的条目一并写上。

### 方式二：直接修改 `src/core.js`

改 `DEFAULT_PATH_MAPPINGS` 后重新部署即可，两个平台同时生效。

## 部署

### 一、Cloudflare Workers

```bash
npm install
npx wrangler deploy
```

或登录 [Cloudflare 控制台](https://dash.cloudflare.com/) 创建 Worker，把 `_worker.js` 与 `src/core.js` 一并粘贴/上传后部署。

生产部署建议直接用仓库里的 `wrangler.jsonc`（已经按「只允许绑定域名 + 启用 Workers Cache」配好）：

```jsonc
{
	"name": "cloudflare-doh",
	"main": "_worker.js",
	"workers_dev": false,          // 关闭 *.workers.dev 测试域名
	"preview_urls": false,         // 关闭版本预览域名
	"routes": [{ "pattern": "doh.example.com", "custom_domain": true }],
	"cache": { "enabled": true },  // Workers Cache（需要 wrangler ≥ 4.69）
	"vars": {
		"ALLOWED_HOSTS": "doh.example.com",
		"CACHE_TTL": "30"
	}
}
```

三层「只允许绑定域名」的保障：

1. `workers_dev: false` + `preview_urls: false` —— 平台层面不生成 `*.workers.dev` 与版本预览域名
2. `routes` 只声明 `custom_domain` —— Worker 只在这一个域名上被触发
3. `ALLOWED_HOSTS` —— 应用层再校验一次 Host，不是绑定域名的请求直接返回伪装 404（防止旧域名/误绑定漏进来）

当前生产状态（2026-10-31 从 Pages 迁移到 Worker 完成）：

| 项 | 值 |
| --- | --- |
| 入口 | 自定义域名（同一 Worker 可绑定多个域名，各自签发证书） |
| workers.dev | 已关闭（API 回读 `enabled: false, previews_enabled: false`） |
| 版本预览 URL | 已关闭 |
| 旧 Pages 项目 | 已删除（`cloudflare-doh-5s9.pages.dev` 已 NXDOMAIN） |
| Workers Cache | 已启用，`GET + application/dns-message` 实测 `MISS` → `HIT`，`Cache-Control: public, max-age=30` |
| POST 行为 | `Cache-Control: no-store`，不进缓存 |
| 六家上游实测 | 12/12 全通（`qq.com` / `test2.example.com`），见下方矩阵 |

#### Workers Cache 的两个坑（本仓库已规避）

- **没写 `Cache-Control` 不等于不缓存**：未设置时按 RFC 9111 启发式缓存，`200` 会被缓存 **2 小时**，`404` 缓存 3 分钟。对 DoH 是灾难，所以 `src/core.js` 对每条响应都显式声明策略：默认 `no-store`，只有 `GET + Accept: application/dns-message + CACHE_TTL>0` 才给 `public, max-age=N` 并附加 `Vary: Accept`。
- **POST 永远不进缓存**：AdGuard Home 用 POST 发 DoH 查询，所以缓存对它没有收益；`CACHE_TTL` 只对浏览器/GET 客户端有效。想让 AGH 也受益就得改成 HTTP/3 或 DoH GET，本仓库不做。

验证缓存是否真的生效（第二次请求 Worker 不再执行，日志里没有新行）：

```bash
npx wrangler tail
curl -s -o /dev/null -D - -H 'accept: application/dns-message' \
  'https://doh.example.com/google/query-dns?dns=AAABAAABAAAAAAAAA3d3dwdleGFtcGxlA2NvbQAAAQAB' | grep -i 'cf-cache-status\|cache-control'
```

> 如需跨版本共享缓存（部署后不重置命中率），升级 wrangler 到 4.107+ 后在 `cache` 里加 `"cross_version_cache": true`。

### 二、Cloudflare Pages

> 本项目原先部署在 Pages，2026-10-31 已迁移到 Worker。Pages 有两个硬限制：`*.pages.dev` 无法关闭、也用不了 Workers Cache，因此只建议作为备选（或不想迁移时的临时方案）。

1. Fork 本库
2. 控制台 → Workers and Pages → 创建 → Pages → 连接到 Git → 选择 Fork 库
3. （可选）添加 `DOMAIN_MAPPINGS` 变量
4. 保存并部署

`_worker.js` 位于仓库根目录，Pages 会以高级模式把它作为唯一入口，并自动打包 `src/` 里的 import。

### 三、腾讯 EdgeOne Pages

```bash
# 前置：npm i -g edgeone@latest && edgeone login --site china
npm run deploy:edgeone -- -n cloudflare-doh
# 或带 Token（免交互）
edgeone makers deploy -n cloudflare-doh -t <API_TOKEN> --json
# 或双击 scripts/deploy-edgeone.bat
```

部署完成后 CLI 会输出形如 `https://<项目>-<hash>.edgeone.cool?eo_token=...&eo_time=...` 的地址，**预览域名必须带完整 `eo_token` 查询参数**才能访问（缺参数会被网关 302 到登录页）。长期使用请绑定已备案的自定义域名。

#### 区域（`-a/--area`）：`global`（默认，含中国大陆）与 `overseas`（仅海外）

```bash
# 中国大陆 + 海外节点（域名后缀 .edgeone.cool）—— 大陆节点只能访问国内上游
edgeone makers deploy -n cloudflare-doh          -a global   -t <TOKEN> --json
# 仅海外节点（域名后缀 .edgeone.dev）—— 可访问 Google 等海外上游
edgeone makers deploy -n cloudflare-doh-overseas -a overseas -t <TOKEN> --json
```

| 项目 | area | ProjectId | 预览域名 |
| --- | --- | --- | --- |
| `cloudflare-doh` | global | `makers-demo1` | `*.edgeone.cool` |
| `cloudflare-doh-overseas` | overseas | `makers-demo2` | `*.edgeone.dev` |

> `.edgeone/project.json` 只记录**最后一次**部署的项目，切换目标时务必显式带 `-n`（和 `-a`），否则会部署到上一次的项目。`.edgeone/` 已被 git 忽略。

#### 手动上传（控制台「直接上传」，含 EdgeOne 国际版）

不要直接上传仓库根目录（会带上 `.git`、`test/`、`scripts/`、`wrangler.jsonc` 等无关文件）。执行：

```bash
npm run pack:edgeone
```

会生成 `dist/edgeone/`，只有三个文件，这就是要上传的文件夹（控制台只接受文件夹或 zip，压成 zip 上传同样可以）：

```
dist/edgeone/
├── middleware.js    # 平台入口（边缘中间件）
├── src/core.js      # 共享核心，构建时被内联进 middleware，必须保留这层目录
└── index.html       # 静态兜底页
```

已在 EdgeOne 上用这个最小目录实际部署验证过：`/` 返回伪装 404、`/google/` 返回 Google Public DNS 首页。

> 国际版控制台（console.intl.cloud.tencent.com）操作一致；CLI 也能直接用，只是需要国际版的 API Token（`edgeone login --site global` 或控制台 → EdgeOne Pages → 设置 → API Token）：
>
> ```bash
> edgeone makers deploy dist/edgeone -n cloudflare-doh -t <INTL_TOKEN> --json
> ```

线上验证记录（2026-10-31）：

- `GET /` → 伪装 404 页面（中间件正常执行）
- `GET /google/` → 转发到 `https://dns.google/`，返回 Google Public DNS 首页
- `GET /google/query-dns?name=example.com&type=A` → 转发成功（浏览器未带 DoH `accept` 头，上游返回 400，属预期）
- `GET /does-not-exist/abc` → 伪装 404

自定义域名（`dohcn.example.com`，EdgeOne 中国站，示例）实测：

- `GET /` → 伪装 404，说明域名 CNAME 到 `*.pages.dnsoe6.com` 后中间件正常执行
- `POST /dnspod/query-dns`、`POST /alidns/query-dns`（`application/dns-message`）→ 200，返回合法 DNS 应答（`rcode=0`，2 条 A 记录，约 0.2s）
- `POST /google|cloudflare|quad9|adguard/query-dns` → 504（约 3s），大陆边缘访问不到这四家
- 证书未签发时该域名返回腾讯默认证书 `*.cdn.myqcloud.com`，TLS 校验会失败（DoH 客户端 / AdGuard Home 会因此报证书错误）

### 四、自建服务器（Go 版）+ EdgeOne 加速

当边缘函数的上游连通性/时延不满足要求时，可以自己起一个 Go 服务，再用 EdgeOne 做前置加速。这是唯一不受「边缘节点出网限制」影响的方案——上游由你自己的服务器直连。

```bash
cd server
go test ./...
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o doh-proxy .
# 上传 + systemd（详见 server/README.md）
```

已在腾讯云日本节点上线（下文以加速域名 `dohj.example.com`、源站 `203.0.113.10:8080` 为例，源站自签证书 10 年）。上海客户端实测（POST 真实查询）：

| 目标 | 耗时 |
| --- | --- |
| `https://dohj.example.com/cloudflare/query-dns`（EdgeOne → 日本） | 5 次 **0.171–0.216s**，非常稳定 |
| `https://doh.example.com/cloudflare/query-dns`（Cloudflare Worker） | 8 次 0.63–2.17s，波动大 |
| `https://203.0.113.10:8080/...`（直连源站，绕过 EdgeOne） | 约 0.27s，抖动 0.28–1.35s |

EdgeOne 侧配置：源站 `203.0.113.10`、协议 **HTTPS**、端口 `8080`、回源 Host 同加速域名；源站用自签证书即可（EdgeOne 不校验源站证书）。注意若回源协议选 HTTPS 而源站没 TLS，会直接 525。完整步骤见 **[server/README.md](server/README.md)**。

一键部署脚本：`scripts\deploy-jp.bat`（Windows 侧交叉编译 + pscp 上传；需先 `set JP_HOST=... JP_PORT=... JP_SSH_PASS=...`）。

#### 一键部署（Linux 服务器侧，通用）

除了从 Windows 推送，也可以直接在目标服务器上执行 `scripts/deploy-linux.sh`：自动下载 Release 二进制 → 安装到 `/opt/doh-proxy` → 按需生成自签证书 → 写 systemd 单元并启动 → 健康检查。脚本幂等，重复执行安全；**检测到已有单元文件时默认只替换二进制并重启，不覆盖已有配置**（`--force-unit` 才重写单元）。

```bash
# 直接管道执行（真·一键）
curl -fsSL https://raw.githubusercontent.com/JackCh3n/cloudflare-doh/main/scripts/deploy-linux.sh | sudo bash

# 或下载后执行
wget -O deploy-linux.sh https://raw.githubusercontent.com/JackCh3n/cloudflare-doh/main/scripts/deploy-linux.sh
sudo bash deploy-linux.sh                        # GitHub Releases 最新版
sudo VERSION=v1.4.0 bash deploy-linux.sh         # 指定版本
sudo bash deploy-linux.sh --file ./doh-proxy     # 本地二进制（内网/访问 GitHub 困难时）
sudo bash deploy-linux.sh --no-tls               # 纯 HTTP（TLS 由前置 CDN 终结时）
sudo ALLOWED_HOSTS=doh.example.com bash deploy-linux.sh
```

常用环境变量：`REPO`（默认 `JackCh3n/cloudflare-doh`）、`INSTALL_DIR`（`/opt/doh-proxy`）、`LISTEN`（`:8080`，配了证书自动变 HTTPS）、`TLS_CN`（自签证书 CN/SAN，默认主机名）、`ALLOWED_HOSTS`、`CACHE_TTL`、`HEALTH_PATH`、`METRICS_PATH` 等。改配置 = 编辑 `/etc/systemd/system/doh-proxy.service` 后 `systemctl daemon-reload && systemctl restart doh-proxy`。

> GitHub 无法直连的环境：用 `--url` 指向镜像直链，或 `--file` 安装本地编译产物（如 Windows 上 `scripts\deploy-jp.bat` 交叉编译后 pscp 上传的产物）。

#### GitHub Releases 二进制（CI 构建）

给仓库打 `v*` 标签后，GitHub Actions 自动构建并发布：

| 产物 | 说明 |
| --- | --- |
| `doh-proxy-linux-amd64` / `doh-proxy-linux-arm64` | 静态编译（仅标准库），可直接运行 |
| `doh-proxy-windows-amd64.exe` | Windows 本地调试用 |
| `SHA256SUMS` | 全部产物的 sha256 校验（`deploy-linux.sh` 会自动校验） |

下载地址（`deploy-linux.sh` 默认来源）：

```
https://github.com/JackCh3n/cloudflare-doh/releases/latest/download/doh-proxy-linux-amd64
```

#### Docker 部署

每次打 tag 自动构建多架构镜像（linux/amd64 + linux/arm64）发布到 GHCR：

```bash
# 最简启动（HTTP :8080，TLS 交给前置 CDN/反代终结）
docker run -d --name doh-proxy -p 8080:8080 --restart unless-stopped \
  ghcr.io/jackch3n/cloudflare-doh:latest
curl http://127.0.0.1:8080/healthz

# 容器内直接开 TLS（AdGuard Home 等只认 https 的客户端可用）
docker run -d --name doh-proxy -p 8443:8080 \
  -v /opt/doh-proxy/tls:/tls:ro \
  -e TLS_CERT=/tls/server.crt -e TLS_KEY=/tls/server.key \
  -e ALLOWED_HOSTS=dohj.example.com \
  --restart unless-stopped ghcr.io/jackch3n/cloudflare-doh:latest
```

环境变量与 Go 版完全一致（`DOMAIN_MAPPINGS` / `ALLOWED_HOSTS` / `CACHE_TTL` / `MAX_INFLIGHT` ...，见 [server/README.md](server/README.md)）。注意：改 `LISTEN_ADDR` 时需同步覆盖容器健康检查（默认探测 `127.0.0.1:8080`）。

## 作为 AdGuard Home 上游使用

> 完整的分流清单（含腾讯游戏等国内域名规则）、google / cloudflare 实测延迟对比、备选方案，见 **[docs/adguard-home.md](docs/adguard-home.md)**。

AdGuard Home 的 DoH 上游语法就是 `https://域名/路径`，它会以 `POST` + `application/dns-message` 请求该地址；本代理正好原样透传方法与请求体。

核心结论（HTTP/2 keep-alive 实测 15 次）：**国外域名走 `/cloudflare` 比 `/google` 更快更稳**（p50 345 vs 362ms，平均 564 vs 761ms）；**国内域名必须直连腾讯 `doh.pub`**（实测 48ms，绕 Worker 会慢到 375–550ms）。阿里 `dns.alidns.com` 更快约 20ms，但本清单统一用腾讯系解析，保证腾讯域名调度一致。

「设置 → DNS 设置 → 上游 DNS 服务器」最简两行（国内直连 + 国外走 EdgeOne 加速的日本节点）：

```
[/*.cn/]https://dns.pub/dns-query
https://dohj.example.com/cloudflare/query-dns
```

> 国外线路现状（108 实测 6 次）：自建 EdgeOne 加速入口（dohj.example.com）0.23–0.33s；Cloudflare Worker 入口（doh.example.com）0.60–3.09s。
> 两者路径完全一致，把加速域名换成你的其它入口域名即可切换，不要同时填。

「Bootstrap DNS 服务器」必须填明文 DNS（AdGuard Home 要靠它解析代理域名本身，不能用自己的 DoH 上游）：

```
119.29.29.29
223.5.5.5
```

验证是否可用（不做 TLS 跳过就说明证书已就绪）：浏览器直接打开
`https://doh.example.com/cloudflare/dns-query?name=example.com&type=A` 应返回一段 JSON。

## 本地调试与测试

```bash
npm test          # 零依赖冒烟测试，覆盖映射解析/头部剥离/POST 透传/双平台入口
npm run dev       # Cloudflare 本地调试（wrangler dev，默认 8787）
scripts\dev.bat   # Windows 一键调试（杀旧进程 → 装依赖 → 启动 → 打开浏览器）
```

EdgeOne 侧：

```bash
edgeone makers dev --port 8088     # 本地预览（见下方已知限制）
edgeone makers deploy -e preview   # 部署到预览环境验证
```

**EdgeOne 本地预览的已知限制**：本仓库根目录同时存在 Cloudflare 的 `wrangler.jsonc` / `_worker.js`，`edgeone makers dev` 会复用该 wrangler 配置作为回源服务，导致中间件拿不到运行所需的 `hookCtx`，控制台报 `Cannot read properties of undefined (reading 'fetch')`，请求会落到 Cloudflare 入口而不是中间件。**这不影响线上部署**（线上已按上表验证通过）。需要端到端验证 EdgeOne 行为时，请用 `edgeone makers deploy -e preview`。

### 线上自检（真实 DoH 查询）

`scripts/check-doh.mjs` 会对指定站点逐个上游发真实 `POST application/dns-message` 查询，解析应答并打印 A 记录，用来确认「转发真的能出结果」而不只是「页面上有点东西」。

```bash
# 自定义域名（证书还没签发时加 --insecure，本地 DNS 解析不到时加 --ip 直连）
node scripts/check-doh.mjs --host dohcn.example.com --ip 203.0.113.20 --insecure

# 只测能用的两家
node scripts/check-doh.mjs --host dohcn.example.com --ip 203.0.113.20 --insecure --providers dnspod,alidns

# 预览域名（*.edgeone.cool / *.edgeone.dev）先取 cookie：curl -D - "<预览URL>" | grep -i set-cookie
node scripts/check-doh.mjs --host my-app.edgeone.dev --cookie "eo_token=xxx; eo_time=123"
```

参数：`--host` 必填，其余可选 `--ip`、`--insecure`、`--cookie`、`--providers`、`--domains`。全部失败时退出码为 1，方便接 CI。

## 注意事项

- 只转发请求，不修改也不存储 DNS 查询内容
- 请遵守各 DoH 服务提供商的使用政策；大规模使用请评估各家的限流
- Cloudflare 免费版每日约 10 万次请求，仅够个人使用，注意避免暴露 DoH 连接
- EdgeOne 平台保留了 `/cdn-cgi/*` 路径（会被平台自身接管，不会透传到上游），Cloudflare 的 `/cdn-cgi/trace` 之类的自检路径在该平台上不可用
- EdgeOne 会把部署目录内的所有文件当作静态资源上传；命中映射之外的路径都会被中间件拦截返回 404，因此静态文件实际不可访问

## 版本记录

| 版本 | 日期 | 说明 |
| --- | --- | --- |
| 1.5.0 | 2026-11-01 | Worker/EdgeOne 版同步 Go 版能力：进程内应答缓存（DNS 报文级缓存键/TTL 递减/跳过 OPT，POST 也命中）、上游熔断与 `FAILOVER_CHAIN` 故障转移、`HEALTH_PATH`/`METRICS_PATH`、hop-by-hop 头剥离、64KB 体上限（413）、上游 15s 超时、读体失败降级转移；冒烟测试 26→46 项 |
| 1.4.1 | 2026-11-01 | 新增 CNB 构建配置 `.cnb.yml`（分支/tag 推送自动构建 Docker 制品到 docker.cnb.cool）；全库移除个人部署信息（域名/IP/SSH 端口改为 doh.example.com 等占位示例），并重写历史 |
| 1.4.0 | 2026-11-01 | 新增 Linux 一键部署脚本 `scripts/deploy-linux.sh`（Releases/本地二进制 → systemd，幂等，已有单元不覆盖）；GitHub Actions 自动发布：打 tag 构建多平台二进制到 Releases + 多架构 Docker 镜像到 GHCR（`ghcr.io/jackch3n/cloudflare-doh`）；README 移除赞助商广告并同步部署文档 |
| 1.3.0 | 2026-11-01 | Go 版新增应答缓存（LRU + ID 改写 + TTL 递减 + 跳过 OPT，实测 215ms→5.5ms）与上游熔断/故障转移（连续失败熔断、冷却探测、FAILOVER_CHAIN 无缝切换）；systemd 增加 cgroup 硬护栏（768M/640M/200% CPU） |
| 1.2.0 | 2026-11-01 | Go 版高并发加固：在途上限(503+Retry-After)、单上游连接背压、TLS 会话复用、全局限速、软内存上限、读写超时族、panic 隔离、Content-Length 显式化、/metrics 指标端点；经 EdgeOne 压测 200 请求/50 并发零错误 |
| 1.1.2 | 2026-10-31 | 108 的 AGH 上游切换为「dns.pub 国内 + 自建 EdgeOne 加速入口国外」并重启生效；实测自建入口 0.23–0.33s（Worker 入口为 0.60–3.09s）；文档同步推荐配置 |
| 1.1.1 | 2026-10-31 | Go 版新增双监听（HTTP + TLS，`-tls-listen`）；日本节点启用自签 10 年证书并前置 EdgeOne 加速，上海侧 0.171–0.216s 稳定；修复 EdgeOne 回源 525；Host 白名单收紧至加速域名 + 源站 IP |
| 1.1.0 | 2026-10-31 | 新增 Go 版实现 `server/`（单文件、仅标准库、含单元测试）并部署到腾讯云日本节点（systemd 托管）；源站直连实测 0.106–0.120s，远优于 Cloudflare Worker 的 0.885–3.706s；新增 scripts/deploy-jp.bat 与 scripts/doh-proxy.service |
| 1.0.11 | 2026-10-31 | 新增备用入口域名（同一 Worker 双域名 + 双证书），ALLOWED_HOSTS 同步；docs 补双入口说明与「新增域名后本地解析不了多为 AGH 负缓存」的排查记录 |
| 1.0.10 | 2026-10-31 | AGH 国内上游统一改为腾讯 doh.pub（去掉阿里），108 实测 qq.com / game.qq.com / pvp.qq.com 均返回腾讯云 CDN 就近节点 |
| 1.0.9 | 2026-10-31 | 兼容裸路径：根路径 / 与 /dns-query、/query-dns 均可作为 DoH 端点（仅当请求头为 DoH 协议时转发，浏览器访问仍是伪装 404），新增 DEFAULT_UPSTREAM；docs/adguard-home.md 补 108 实机排查记录 |
| 1.0.8 | 2026-10-31 | 新增 docs/adguard-home.md：google/cloudflare 延迟实测（cloudflare 更快）、国内直连与绕 Worker 对比、含腾讯游戏的完整分流清单；README 精简为结论 + 链接 |
| 1.0.7 | 2026-10-31 | 迁移完成：删除 Pages 项目与旧域名，Worker 以 custom_domain 绑定唯一自定义入口域名，workers.dev 与版本预览关闭，Workers Cache 实测 MISS→HIT，六家上游 12/12 全通 |
| 1.0.6 | 2026-10-31 | 新增 `ALLOWED_HOSTS`（只允许绑定域名访问）与 `CACHE_TTL`（Workers Cache 显式缓存策略）；wrangler 配置关闭 workers_dev 与 preview_urls 并启用 `cache.enabled`；README 补充 Worker 生产配置与缓存两个坑 |
| 1.0.5 | 2026-10-31 | 新增 `scripts/check-doh.mjs` 线上自检脚本（真实 DoH 查询 + 解析 A 记录）；用 `qq.com` / `test2.example.com` 复测三个站点六家上游，README 实测表改为真实域名结果 |
| 1.0.4 | 2026-10-31 | 补记国际版手动上传站点实测结果：中间件被正确识别，Google / AdGuard / doh.pub / AliDNS 可达，Quad9 不可达、Cloudflare 不稳定 |
| 1.0.3 | 2026-10-31 | 新增 `npm run pack:edgeone` 生成 3 文件手动上传目录 `dist/edgeone`（已在 EdgeOne 实测部署通过），README 补充控制台直接上传与国际版说明 |
| 1.0.2 | 2026-10-31 | 新增 `-a overseas` 海外区域站点（`cloudflare-doh-overseas`）并实测各上游可达性；README 补充双区域部署与项目 ID 对照 |
| 1.0.1 | 2026-10-31 | 新增 `/dnspod`（doh.pub）、`/alidns`（dns.alidns.com）国内可达映射；补充 EdgeOne 大陆边缘的出网实测结论与 AdGuard Home 配置说明 |
| 1.0.0 | 2026-10-31 | 抽取 `src/core.js` 共享核心，新增腾讯 EdgeOne Pages 中间件入口，冒烟测试替代原 vitest 模板测试 |

## 许可协议

本项目采用 [MIT 许可协议](LICENSE)。您可以自由地使用、修改和分发本代码，但需要在您的项目中包含原始许可证和版权声明。
