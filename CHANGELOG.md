# 更新日志（CHANGELOG）

本项目遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/) 与 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 格式。

## 维护约定

- **版本号单一来源**：`package.json` 的 `version`；**只在发布时升版本**（普通提交不升）
- **升版规则**：MAJOR = 不兼容变更；MINOR = 向后兼容的新功能；PATCH = bug 修复 / 安全补丁 / 依赖升级；纯文档 / CI 改动不升版本
- **发布流程**：改 `version` + 在本文件顶部新增一节 → 推送 main → CI 自动打 tag `vX.Y.Z`、发 Release（二进制 + SHA256SUMS）、推版本化镜像（GHCR + CNB）
- **tag 约定**：`vX.Y.Z` 必须与 `package.json` 一致，发布后不可移动
- **未发布的改动**：先记在「未发布」一节，发版时把它并入新的版本号下

## [未发布]

### 修复

- 伪装 404 页对齐 Chrome 157 真实 DNS 错误页：图标是 72×72 位图，原先按 68×18 摆放被压成一条；桌面端「重新加载」由浮动到右侧改为与正文左对齐；窄屏改为固定在底部的通栏按钮并补齐全部断点；首行带上主机名（Chrome 实测不带端口）；深色正文色值改为 `#9aa0a6`
- 伪装页「尝试运行 Windows 网络诊断」原先是鼠标点不动、键盘 Tab 不到的 `<span>`，改为可聚焦的 `<a>`，点击 / 回车不改动地址栏
- Go 版：`FAILOVER_CHAIN` 在 `DOMAIN_MAPPINGS` 覆盖之前解析，同时配置两者时自定义映射里的上游名被误判「不在映射表」而丢弃，故障转移失效；现在基于覆盖后的映射表解析
- Go / 边缘版：转发时透传客户端的 `Accept-Encoding`，压缩应答会原样进缓存，未带该头的客户端命中后拿到压缩体无法解析；现在转发前剥离，缓存里恒为未压缩的应答
- Go 版：上游应答体中途断流直接返回 502；现在记该上游失败并继续候选链转移（与边缘版一致）
- Go 版：客户端先断开时发往上游的请求必然报错，原先被记成上游失败，几个没耐心的客户端就能把整条候选链逐家熔断；现在不计失败、不再转移
- Go / 边缘版：TC=1 的截断应答、事务 ID 或 question 对不上的应答、上游用 200 返回的非 DNS 内容都会进缓存；现在只缓存确认属于该查询的完整 DNS 应答
- Go / 边缘版：缓存键不含 EDNS 信息，做 DNSSEC 验证的客户端（DO=1）可能拿到不含 RRSIG/OPT 的缓存应答；现在带不带 OPT、带不带 DO 各自缓存
- Go 版：路径用解码后的 `r.URL.Path` 拼目标 URL，`%3F` / `%23` 会被再解析成查询串 / 片段；改为按转义形式转发（与边缘版一致）
- Go 版：只配了 `TLS_CERT` / `TLS_KEY` 其中一个时静默降级成明文 HTTP；现在拒绝启动并说明原因，证书加载失败同样启动即报错；服务端显式限定 TLS 1.2
- Go 版：`READ_TIMEOUT_SEC` / `WRITE_TIMEOUT_SEC` 为负值时所有连接当场断开；现在回退默认值
- Go 版：400 / 413 / 429 / 502 / 503 应答缺少 `Cache-Control: no-store`，可能被前置 CDN 缓存成粘性故障
- Go 版：缓存只有条目上限，最坏可被撑到约 640MB 并触发 GC 空转；新增 64 MiB 总字节预算，单条超预算不入缓存
- Go 版：上游建连没有独立超时，黑洞型故障要等满 15 秒才转移；新增 5 秒建连超时
- Go 版：`?dns=` 只接受 base64url，带填充或标准 base64 的客户端永远命中不了缓存
- Go 版：`FAILOVER_CHAIN` 里的重复项会让同一上游在一次请求里被试两遍
- Go 版：`rrNext` 不检查记录数据是否越界，末尾记录被截断的畸形应答被当成可解析（边缘版已检查）
- 边缘版：伪装页嵌在 `core.js` 的模板字符串里，页面中的反斜杠会被当成转义序列吃掉，内嵌脚本因此变成语法错误；同步脚本与冒烟测试现在拒绝并守住这一点
- 边缘版：语言选择大小写敏感，`en-GB` / `pt-PT` 这类带大写的条目精确匹配永远命不中
- 边缘版：熔断关闭（`CB_THRESHOLD=0`）时 `/metrics` 的 `providers` 恒显示不健康；修正为健康

### 改进

- 伪装页按浏览器界面语言显示 Chrome 全部 55 种界面语言的官方文案（ar / fa / he / ur 从右到左），Windows 上随语言切换 Chrome 的字体栈，未收录的语言回退英文；高对比度模式下图标不再消失
- Go / 边缘版：上游返回 `429`（限流）视为不可用并转移；裸路径与故障转移的目标路径跟随 `pathMapping`（端点不在 `/dns-query` 的上游也能用）
- Go 版新增版本号：`doh-proxy -version`、启动日志、`/metrics` 的 `version` 字段；`/metrics` 新增 `cache_bytes` / `cache_max_bytes`
- 新增 `npm run camo:sync`（伪装页三处同步）与 `npm run test:camo`（无头浏览器设备模拟截图验证；原 `--window-size=375,667` 的做法在 Windows 上拍不到手机布局，压扁的图标也能通过）
- 新增 `npm run camo:chrome`：伪装页 ↔ 本机 Chrome 真实错误页对照（60 种视口的布局、7 张截图逐像素、多语言文案）；Chrome 改版后用它量出伪装页哪里要跟着改

### 构建与发布

- CI 把版本号编进二进制与镜像（`-X main.version`）
- CNB 只在 `main` 与 `v*` 标签上发布版本化镜像与 `latest`，其它分支只出 `<分支名>`（原先任何分支的推送都会覆盖已发布的镜像）

## [1.17.0] - 2026-11-11

### 变更

- **版本基线修正**：以首个提交 `b85783a`（2025-03-12 初始实现，`/<上游>/query-dns` 对外路由契约自此存在且从未变更）为基线重算全部 44 个主线提交 → `feat` 记 MINOR ×17、`fix` 记 PATCH ×4、`docs`/`chore`/`ci` ×15 不计 → 1.12.0 校正为 **1.17.0**（前值漏计共享核心确立之前的 5 个 feat）

### 修复

- JS 版 `DEFAULT_UPSTREAM` 未做小写归一（Go `loadConfig` 会 `ToLower`），大写配置会让裸路径 DoH 直接 404
- JS 版请求体读取失败（客户端中断）现在返回 400 `bad request body`，与 Go 对齐（原先未处理拒绝由平台兜底成 500）

### 改进

- 伪装 404 页更贴近 Chrome 真实行为：「重新加载」按钮可实际刷新页面（`location.reload`）、标签页标题显示当前域名（原固定 `404` 是伪装破绽）、补系统字体栈与 `body margin` 归零；三处副本（core.js / index.html / main.go）逐字同步

### 测试

- 冒烟新增：伪装页三处副本一致性守卫、压缩指针应答（`0xC00C`）TTL 计算与递减覆盖、`DEFAULT_UPSTREAM` 大小写回归、请求体读取失败 400 → 48 → 53 项

## [1.12.0] - 2026-11-10

### 变更

- **版本号按新 SemVer 规则从 git 历史重算**：基线 1.0.0（`8df1f89` 抽取共享核心 + EdgeOne 入口），其后 `feat` 记 MINOR ×12、`fix` 记 PATCH ×4、`docs`/`chore`/`ci` 不计 → 原 1.5.5 校正为 **1.12.0**（功能与 1.5.5 无差异）
- 新增本 `CHANGELOG.md`，回填 1.0.0 以来的全部版本历史，此后随每次发版更新
- README 的版本记录表收敛为指向本文件的入口，避免两处重复维护
- CI 修复：Release 下载产物限定 `doh-proxy-*`，避免 docker buildx 的 gha 缓存产物被误下载导致发版失败

## [1.5.5] - 2026-11-10

### 新增

- Worker 开启可观测性全家桶：`observability.traces`（自动分布式追踪，open beta）与 `observability.issues`（运行时异常聚合，需 wrangler ≥ 4.134）

### 变更

- wrangler 升级 3.114 → 4.159，`deploy --dry-run` 验证通过

## [1.5.4] - 2026-11-02

### 变更

- 发布流程改为**版本号驱动**：每次推送 main 按 `package.json` 版本自动打 tag → 发 Releases → 推版本化镜像（GHCR + CNB），不再要求手动打 tag，小修也必出可部署产物；版本未变时仅跑验证并刷新 `edge` 镜像
- CI 前置增加边缘版冒烟测试（48 项）门禁

## [1.5.3] - 2026-11-02

### 修复

- JS 版 POST 空 body 时回退解析 `?dns=`（对齐 Go `dohQueryOf`，消除缓存命中面差异）
- server/README 补 64KB 体上限说明；全量验证矩阵通过（JS 48 项 / Go 全量 / YAML 与 bash 语法 / EdgeOne 三文件打包 / 三处伪装页逐字一致 / 残留 grep 干净）

## [1.5.2] - 2026-11-02

### 修复

- JS 记录名解析对齐 Go：非法标签长度字节（0x40/0x80）按解析失败处理，消除恶意报文下的语义分歧
- 伪装 404 与错误应答补 `X-Content-Type-Options: nosniff`（对齐 Go）
- 上游多条 `set-cookie` 透传不再互相覆盖
- `/metrics` 预登记全部映射上游（对齐 Go 的 providers 全量口径）

### 变更

- deploy-jp.bat 不再覆盖系统 `USER` 变量

## [1.5.1] - 2026-11-02

### 修复

- JS 版 `pathMapping` 改为最长键前缀重写（原 `String.replace` 会误替换路径中间出现的目标，与 Go 版语义分歧）
- Go 版请求体超 64KB 返回 413（原静默截断转发畸形报文）
- Go 版上游应答超 64KB 不再写截断体（消除客户端按 Content-Length 挂起风险），读体失败返回 502
- 伪装 404 页修正 `Windows` / `FINISHED` 拼写、适配窄屏（`max-width`）、`lang` 改 `zh-CN`，三处副本逐字同步
- `deploy-linux.sh` 自签证书 IPv4 被误判为 DNS SAN

### 测试

- 冒烟测试 46 → 47 项

## [1.5.0] - 2026-11-01

### 新增

- Worker/EdgeOne 版同步 Go 版能力：进程内应答缓存（DNS 报文级缓存键 / TTL 递减 / 跳过 OPT，POST 也命中）、上游熔断与 `FAILOVER_CHAIN` 故障转移、`HEALTH_PATH`/`METRICS_PATH`、hop-by-hop 头剥离、64KB 体上限（413）、上游 15s 超时、读体失败降级转移

### 测试

- 冒烟测试 26 → 46 项

## [1.4.1] - 2026-11-01

### 新增

- 新增 CNB 构建配置 `.cnb.yml`（分支/tag 推送自动构建 Docker 制品到 docker.cnb.cool）

### 变更

- 全库移除个人部署信息（域名/IP/SSH 端口改为 `doh.example.com` 等占位示例），并重写历史

## [1.4.0] - 2026-11-01

### 新增

- Linux 一键部署脚本 `scripts/deploy-linux.sh`（Releases/本地二进制 → systemd，幂等，已有单元不覆盖）
- GitHub Actions 自动发布：打 tag 构建多平台二进制到 Releases + 多架构 Docker 镜像到 GHCR

### 变更

- README 移除赞助商广告并同步部署文档

## [1.3.0] - 2026-11-01

### 新增

- Go 版应答缓存（LRU + ID 改写 + TTL 递减 + 跳过 OPT，实测 215ms→5.5ms）
- Go 版上游熔断/故障转移（连续失败熔断、冷却探测、`FAILOVER_CHAIN` 无缝切换）
- systemd cgroup 硬护栏（768M/640M/200% CPU）

## [1.2.0] - 2026-11-01

### 新增

- Go 版高并发加固：在途上限（503 + Retry-After）、单上游连接背压、TLS 会话复用、全局限速、软内存上限、读写超时族、panic 隔离、Content-Length 显式化、`/metrics` 指标端点
- 经 EdgeOne 压测 200 请求 / 50 并发零错误

## [1.1.2] - 2026-10-31

### 变更

- AGH 上游切换为「dns.pub 国内 + 自建 EdgeOne 加速入口国外」并重启生效；实测自建入口 0.23–0.33s（Worker 入口 0.60–3.09s）；文档同步推荐配置

## [1.1.1] - 2026-10-31

### 新增

- Go 版双监听（HTTP + TLS，`-tls-listen`）；源站启用自签 10 年证书并前置 EdgeOne 加速

### 修复

- 修复 EdgeOne 回源 525；Host 白名单收紧至加速域名 + 源站 IP

## [1.1.0] - 2026-10-31

### 新增

- Go 版实现 `server/`（单文件、仅标准库、含单元测试），部署到腾讯云日本节点（systemd 托管）
- `scripts/deploy-jp.bat` 与 `scripts/doh-proxy.service`

### 性能

- 源站直连实测 0.106–0.120s，远优于 Cloudflare Worker 的 0.885–3.706s

## [1.0.11] - 2026-10-31

### 新增

- 备用入口域名（同一 Worker 双域名 + 双证书），`ALLOWED_HOSTS` 同步

### 文档

- 补双入口说明与「新增域名后本地解析不了多为 AGH 负缓存」的排查记录

## [1.0.10] - 2026-10-31

### 变更

- AGH 国内上游统一改为腾讯 `doh.pub`（去掉阿里）；实测 `qq.com` / `game.qq.com` / `pvp.qq.com` 均返回腾讯云 CDN 就近节点

## [1.0.9] - 2026-10-31

### 新增

- 兼容裸路径：根路径 `/` 与 `/dns-query`、`/query-dns` 均可作为 DoH 端点（仅当请求头为 DoH 协议时转发，浏览器访问仍是伪装 404）
- 新增 `DEFAULT_UPSTREAM`

### 文档

- `docs/adguard-home.md` 补实机排查记录

## [1.0.8] - 2026-10-31

### 文档

- 新增 `docs/adguard-home.md`：google/cloudflare 延迟实测（cloudflare 更快）、国内直连与绕 Worker 对比、含腾讯游戏的完整分流清单；README 精简为结论 + 链接

## [1.0.7] - 2026-10-31

### 变更

- 迁移完成：删除 Pages 项目与旧域名，Worker 以 `custom_domain` 绑定唯一自定义入口域名，`workers_dev` 与版本预览关闭
- Workers Cache 实测 MISS→HIT，六家上游 12/12 全通

## [1.0.6] - 2026-10-31

### 新增

- `ALLOWED_HOSTS`（只允许绑定域名访问）与 `CACHE_TTL`（Workers Cache 显式缓存策略）

### 变更

- wrangler 配置关闭 `workers_dev` 与 `preview_urls` 并启用 `cache.enabled`；README 补充生产配置与缓存两个坑

## [1.0.5] - 2026-10-31

### 新增

- `scripts/check-doh.mjs` 线上自检脚本（真实 DoH 查询 + 解析 A 记录）

### 变更

- 用真实测试域名复测三个站点六家上游，README 实测表改为真实结果

## [1.0.4] - 2026-10-31

### 文档

- 补记国际版手动上传站点实测结果：中间件被正确识别，Google / AdGuard / doh.pub / AliDNS 可达，Quad9 不可达、Cloudflare 不稳定

## [1.0.3] - 2026-10-31

### 新增

- `npm run pack:edgeone` 生成 3 文件手动上传目录 `dist/edgeone`（已在 EdgeOne 实测部署通过）

### 文档

- README 补充控制台直接上传与国际版说明

## [1.0.2] - 2026-10-31

### 新增

- `-a overseas` 海外区域站点（`cloudflare-doh-overseas`）并实测各上游可达性

### 文档

- README 补充双区域部署与项目 ID 对照

## [1.0.1] - 2026-10-31

### 新增

- `/dnspod`（doh.pub）、`/alidns`（dns.alidns.com）国内可达映射

### 文档

- 补充 EdgeOne 大陆边缘的出网实测结论与 AdGuard Home 配置说明

## [1.0.0] - 2026-10-31

### 新增

- 抽取 `src/core.js` 共享核心，新增腾讯 EdgeOne Pages 中间件入口
- 冒烟测试替代原 vitest 模板测试
