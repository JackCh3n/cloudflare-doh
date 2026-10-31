# AdGuard Home 分流配置（配合本项目的 DoH 代理）

> 实测时间：2026-10-31 · 客户端在中国大陆 · 代理为 Cloudflare Worker `https://doh.example.com`

## 一、先看数据：谁快

用 HTTP/2 + keep-alive（贴近 AdGuard Home 实际行为）各发 15 次真实 DoH 查询：

### 国外域名（github / steamcommunity / google / youtube / cloudflare）

经 `https://doh.example.com`

| 上游 | p50 | p90 | 平均 | 成功 |
| --- | --- | --- | --- | --- |
| `/adguard` | **306ms** | 1095ms | **445ms** | 15/15 |
| `/cloudflare` | 345ms | 1274ms | 564ms | 15/15 |
| `/google` | 362ms | 1797ms | 761ms | 15/15 |
| `/quad9` | 705ms | 1395ms | 864ms | 15/15 |

**结论：Cloudflare 比 Google 更快也更稳**（p50 略胜 17ms，平均快 197ms，p90 快 523ms）。
`/adguard` 数字最好看，但上游自带广告过滤，本地 AGH 已经过滤过一遍，一般不建议叠加。

### 国内域名（qq / bilibili / baidu / aliyun / game.qq.com）

| 路径 | p50 | p90 | 平均 |
| --- | --- | --- | --- |
| 直连 `dns.alidns.com` | **26ms** | 27ms | **29ms** |
| 直连 `dns.pub` | 48ms | 72ms | 53ms |
| 经 Worker `/dnspod` | 375ms | 1087ms | 553ms |
| 经 Worker `/alidns` | 551ms | 1525ms | 746ms |

**结论：国内域名千万不要绕 Worker**，同一跳会让解析慢 8～20 倍（还拿不到国内就近 IP 的 ECS 优势）。
国内直连、国外走 Worker，是唯一合理的组合。

## 二、推荐配置（方案 A：速度优先，直接粘贴到「上游 DNS 服务器」）

> 地址写法：下面用的是显式路径 `https://doh.example.com/cloudflare/query-dns`。
> 2026-10-31 起，`https://doh.example.com`、`/dns-query`、`/query-dns` 也都能直接用（自动走 `DEFAULT_UPSTREAM`，默认 cloudflare），所以只想要一个上游时填 `https://doh.example.com/dns-query` 就够了。

```
# ============ 国内：直连国内 DoH ============
[/*.cn/]https://dns.pub/dns-query

# 腾讯系 / 腾讯游戏
[/qq.com/*.qq.com/tencent.com/*.tencent.com/tencentgames.com/*.tencentgames.com/]https://dns.pub/dns-query
[/dnspod.cn/*.dnspod.cn/dnspod.net/*.dnspod.net/]https://dns.pub/dns-query
[/myqcloud.com/*.myqcloud.com/qcloud.com/*.qcloud.com/tencent-cloud.com/*.tencent-cloud.com/]https://dns.pub/dns-query
[/gtimg.cn/*.gtimg.cn/qpic.cn/*.qpic.cn/weixin.qq.com/wechat.com/*.wechat.com/]https://dns.pub/dns-query
[/wetest.net/*.wetest.net/wetest.qq.com/ieg.qq.com/*.ieg.qq.com/]https://dns.pub/dns-query

# 阿里 / 百度 / 字节 / 网易
[/aliyun.com/*.aliyun.com/aliyuncs.com/*.aliyuncs.com/alicdn.com/*.alicdn.com/taobao.com/*.taobao.com/tmall.com/*.tmall.com/alipay.com/*.alipay.com/]https://dns.pub/dns-query
[/baidu.com/*.baidu.com/bdstatic.com/*.bdstatic.com/bdimg.com/*.bdimg.com/bcebos.com/*.bcebos.com/]https://dns.pub/dns-query
[/bytedance.com/*.bytedance.com/douyin.com/*.douyin.com/toutiao.com/*.toutiao.com/byteimg.com/*.byteimg.com/feishu.cn/*.feishu.cn/]https://dns.pub/dns-query
[/163.com/*.163.com/126.net/*.126.net/netease.com/*.netease.com/netease.im/*.netease.im/]https://dns.pub/dns-query

# 视频 / 电商 / 生活
[/bilibili.com/*.bilibili.com/hdslb.com/*.hdslb.com/bilivideo.com/*.bilivideo.com/]https://dns.pub/dns-query
[/iqiyi.com/*.iqiyi.com/youku.com/*.youku.com/qqvideo.com/*.qqvideo.com/]https://dns.pub/dns-query
[/jd.com/*.jd.com/360buyimg.com/*.360buyimg.com/meituan.com/*.meituan.com/pinduoduo.com/*.pinduoduo.com/]https://dns.pub/dns-query
[/zhihu.com/*.zhihu.com/weibo.com/*.weibo.com/kuaishou.com/*.kuaishou.com/xiaohongshu.com/*.xiaohongshu.com/]https://dns.pub/dns-query
[/12306.cn/*.12306.cn/ctrip.com/*.ctrip.com/ly.com/*.ly.com/]https://dns.pub/dns-query
[/xiaomi.com/*.xiaomi.com/mi.com/*.mi.com/huawei.com/*.huawei.com/honor.com/*.honor.com/]https://dns.pub/dns-query

# ============ 国外：走自己的 Worker ============
[/cloudfront.net/*.cloudfront.net/akamaihd.net/*.akamaihd.net/akamaized.net/*.akamaized.net/fastly.net/*.fastly.net/]https://doh.example.com/cloudflare/query-dns
[/github.com/*.github.com/githubusercontent.com/*.githubusercontent.com/]https://doh.example.com/cloudflare/query-dns
[/google.com/*.google.com/gstatic.com/*.gstatic.com/googleapis.com/*.googleapis.com/youtube.com/*.youtube.com/ytimg.com/*.ytimg.com/]https://doh.example.com/cloudflare/query-dns
[/steamcommunity.com/*.steamcommunity.com/steampowered.com/*.steampowered.com/steamstatic.com/*.steamstatic.com/steamcontent.com/*.steamcontent.com/]https://doh.example.com/cloudflare/query-dns

# ============ 默认（未命中上面任何规则）============
https://doh.example.com/cloudflare/query-dns
https://doh.example.com/google/query-dns
```

**Bootstrap DNS 服务器**（必须填明文，AGH 要靠它解析上面的域名）：

```
119.29.29.29
223.5.5.5
```

验证清单：

- 保存后点「测试上游」，每一行都应变成 OK
- 关掉 AGH 缓存（或调小到 60s 以内）测一次，避免命中旧缓存干扰判断
- 想确认走的是哪条规则：AGH 日志 → 查询日志里能看到「上游」列

## 三、备选方案 B（全部走自家域名，隐藏真实上游）

如果你的目的是「对外只暴露 `doh.example.com` 一个主机名」，可以把国内规则也指向自家 Worker 的国内上游路径：

```
# 国内域名 → 经自己的 Worker，再转到 doh.pub
[/qq.com/*.qq.com/*.cn/]https://doh.example.com/dnspod/query-dns

# 其它全部
https://doh.example.com/cloudflare/query-dns
```

代价很明确：国内解析从 **26–53ms 变成 375–550ms**（实测）。除非有统一的审计/隐藏需求，否则不建议。

## 四、规则写法要点

| 写法 | 含义 |
| --- | --- |
| `[/qq.com/]上游` | 只匹配 `qq.com` 本身 |
| `[/*.qq.com/]上游` | 匹配所有子域（`game.qq.com`、`pvp.qq.com`…） |
| `[/a.com/*.a.com/]上游` | 两条都要写，才能同时覆盖主域与子域（本文件都这么写） |
| `[/a.com/b.com/]上游` | 多条域名共用一条上游，域名之间用 `/` 分隔 |
| 不带中括号 | 默认上游，所有未命中规则的域名走这里 |

- 规则**越具体优先级越高**，所以把 `*.qq.com` 这类放到默认规则之前
- AGH 的 DoH 上游一律用 `POST + application/dns-message`，与路径无关
- 本项目 Worker 的 `CACHE_TTL` 只对 `GET` 生效，**对 AGH 的 POST 查询没有缓存收益**，AGH 侧请自行配置 `cache_size`

## 五、腾讯游戏相关域名说明

游戏登录/更新服务器基本都挂在 `qq.com` 与 `tencentgames.com` 下，常见的：

| 域名 | 用途 |
| --- | --- |
| `game.qq.com` / `pvp.qq.com` | 游戏大厅 / 对战平台入口 |
| `smoba.qq.com` | 王者荣耀相关 |
| `ieg.qq.com` | 互动娱乐事业群（游戏业务） |
| `wetest.net` / `wetest.qq.com` | 腾讯 WeTest（游戏测试/加速相关） |
| `cdn.qq.com` / `*.gtimg.cn` | 游戏资源 CDN |

这些全部走国内 `dns.pub`，能拿到就近的国内节点；若走 Worker 的海外边缘，可能解析到香港/新加坡节点，延迟与登录失败率都会变高。

## 六、「测试上游」失败排查（10.24.36.108 实机记录）

排查环境：`orange-xq`（108 测试机，AdGuard Home 装在 `/vol1/@appcenter/AdGuardHome`，配置 `/vol1/@appdata/AdGuardHome/AdGuardHome.yaml`）。

### 排查结论

| 现象 | 实测 | 原因 / 处理 |
| --- | --- | --- |
| `https://doh.example.com` 测试失败 | `POST /` 返回 404（1588B HTML） | 早期版本只认 `/<上游>/query-dns`。**已修**：现在根路径、`/dns-query`、`/query-dns` 都可用（仅当请求头是 DoH 协议时才转发） |
| `https://doh.example.com/dns-query` 失败 | 同上 404 | 同上，已修 |
| `https://dns.cloudflare.com/dns-query` 不可用 | `code=000, 0.18s` 直接失败 | 该域名在国内被阻断，必须换成 `https://doh.example.com/cloudflare/query-dns` |
| `https://dns.google/dns-query` 不可用 | `code=000, 10s` 超时 | 同上，走 Worker 的 `/google/query-dns` |
| 上游解析偶发变慢 | 108 上 `ip -6 addr` 无全局地址、`ip -6 route` 无默认路由，`curl -6` **1.4ms 即失败** | bootstrap 里配了 3 台 IPv6 DNS，但本机没有 IPv6，建议删掉：`2400:3200::1`、`2402:4e00::`、`2001:dc7:1000::1`，只留 `223.5.5.5` / `119.29.29.29` |

### 修好之后在 108 上的实测（HTTP/2，POST `application/dns-message`）

```
POST /                     → 200, 72B, 0.96s   （带 DoH 请求头）
GET  /?dns=...             → 200, 72B          （带 accept: application/dns-message）
POST /dns-query            → 200, 72B, 1.53s
POST /query-dns            → 200, 72B, 1.88s
POST /cloudflare/query-dns → 200, 72B, 1.03s
POST /google/query-dns     → 200, 72B, 2.02s
POST /adguard/query-dns    → 200, 72B, 0.90s
浏览器 GET /               → 404 text/html（伪装页，按设计）
```

### 108 建议的上游写法

```
# 国内直连（108 实测 0.13s / 0.18s）
[/*.cn/]https://dns.alidns.com/dns-query
[/qq.com/*.qq.com/tencent.com/*.tencent.com/tencentgames.com/*.tencentgames.com/]https://dns.alidns.com/dns-query
[/bilibili.com/*.bilibili.com/hdslb.com/*.hdslb.com/]https://dns.alidns.com/dns-query
[/microsoft.com/*.microsoft.com/]https://dns.alidns.com/dns-query

# 国外走自己的 Worker
[/cloudfront.net/*.cloudfront.net/akamaihd.net/*.akamaihd.net/]https://doh.example.com/cloudflare/query-dns
[/github.com/*.github.com/steamcommunity.com/steampowered.com/*.steampowered.com/]https://doh.example.com/cloudflare/query-dns
[/google.com/*.google.com/youtube.com/*.youtube.com/]https://doh.example.com/cloudflare/query-dns

# 默认
https://doh.example.com/cloudflare/query-dns

# Bootstrap（只留 IPv4，108 没有 IPv6）
223.5.5.5
119.29.29.29
```
