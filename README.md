# Cloudflare DoH 转发代理（Cloudflare + 腾讯 EdgeOne 双平台）

![使用演示](https://i.imgur.com/hu6F10P.gif)

这是一个基于边缘函数运行的 DNS over HTTPS (DoH) 转发代理服务。它按**请求路径前缀**把请求反向代理到不同的 DoH 提供商，并原样保留查询参数与请求体。

同一份路由逻辑同时支持三个部署入口：

| 平台 | 入口文件 | 运行时 | 状态 |
| --- | --- | --- | --- |
| Cloudflare Workers | `_worker.js` | V8 Worker | 已验证 |
| Cloudflare Pages | `_worker.js`（Pages 高级模式） | V8 Worker | 已验证 |
| 腾讯 EdgeOne Pages | `middleware.js`（边缘中间件） | V8 Edge Middleware | 已验证（2026-10-31 线上部署通过） |

## 功能特点

- **基于路径转发**：访问 `doh.example.com/google/query-dns` → `dns.google/dns-query`
- **平台无关核心**：路由/代理逻辑集中在 `src/core.js`，两个平台只写十几行适配层
- **参数与请求体完整透传**：保留查询串，支持 DoH 的 GET 与 POST（`application/dns-message` 二进制体）
- **路径映射可配置**：可用环境变量 `DOMAIN_MAPPINGS` 覆盖，也可以直接改默认映射
- **伪装 404**：未命中映射的路径（含站点根路径）返回一个「浏览器断网」样式的 404 页面，不暴露代理特征

## 目录结构

```
cloudflare-doh/
├── _worker.js            # Cloudflare Workers / Pages 入口（薄适配层）
├── middleware.js         # 腾讯 EdgeOne Pages 入口（边缘中间件）
├── index.html            # 静态兜底页（内容与伪装 404 一致）
├── src/
│   └── core.js           # 共享核心：默认映射、路径解析、代理转发、伪装 404
├── test/
│   └── smoke.mjs         # 零依赖冒烟测试（node test/smoke.mjs）
├── scripts/
│   ├── dev.bat           # 本地调试：杀旧进程 → 装依赖 → 启动 → 打开浏览器
│   ├── kill-dev.bat      # 结束占用 8787/8088/6699 的调试进程
│   └── deploy-edgeone.bat# 一键部署到 EdgeOne Pages
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

> 前缀之后没有剩余路径时会补 `/`，例如 `/google?name=a.com` → `https://dns.google/?name=a.com`。

## 配置

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

### 二、Cloudflare Pages

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

线上验证记录（2026-10-31）：

- `GET /` → 伪装 404 页面（中间件正常执行）
- `GET /google/` → 转发到 `https://dns.google/`，返回 Google Public DNS 首页
- `GET /google/query-dns?name=example.com&type=A` → 转发成功（浏览器未带 DoH `accept` 头，上游返回 400，属预期）
- `GET /does-not-exist/abc` → 伪装 404

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

## 注意事项

- 只转发请求，不修改也不存储 DNS 查询内容
- 请遵守各 DoH 服务提供商的使用政策；大规模使用请评估各家的限流
- Cloudflare 免费版每日约 10 万次请求，仅够个人使用，注意避免暴露 DoH 连接
- EdgeOne 平台保留了 `/cdn-cgi/*` 路径（会被平台自身接管，不会透传到上游），Cloudflare 的 `/cdn-cgi/trace` 之类的自检路径在该平台上不可用
- EdgeOne 会把部署目录内的所有文件当作静态资源上传；命中映射之外的路径都会被中间件拦截返回 404，因此静态文件实际不可访问

## 版本记录

| 版本 | 日期 | 说明 |
| --- | --- | --- |
| 1.0.0 | 2026-10-31 | 抽取 `src/core.js` 共享核心，新增腾讯 EdgeOne Pages 中间件入口，冒烟测试替代原 vitest 模板测试 |

## 许可协议

本项目采用 [MIT 许可协议](LICENSE)。您可以自由地使用、修改和分发本代码，但需要在您的项目中包含原始许可证和版权声明。

## 赞助商

[宁屏去广告](https://www.nullprivate.com) - 可定制化个人 DNS 解析服务

主要特性:

- 智能广告及追踪器过滤
- 内置DDNS 支持
- 防沉迷配置
- 支持按规则路由上游DNS

详情请访问 [Null Private](https://www.nullprivate.com) 了解更多。
