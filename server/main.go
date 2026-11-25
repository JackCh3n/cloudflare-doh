// DoH 路径转发代理（Go 版，仅用标准库）
//
// 与仓库根目录的 Cloudflare Worker / EdgeOne 中间件实现路由逻辑一致：
//
//	/<上游>/query-dns  → 该上游的 DoH 端点，保留查询串与请求体
//	/<上游>/dns-query  → 同上（路径透传）
//	/dns-query、/query-dns、/（仅 DoH 请求）→ DEFAULT_UPSTREAM
//	其它路径 → 伪装 404 页面
//
// 环境变量：
//
//	LISTEN_ADDR       监听地址，默认 :8080
//	TLS_LISTEN        额外的 TLS 监听地址（如 :8444）；留空则 -tls-cert 生效时直接把 LISTEN_ADDR 变成 HTTPS
//	DOMAIN_MAPPINGS   覆盖整张映射表（JSON，字段同 Worker 版）
//	ALLOWED_HOSTS     允许的 Host，逗号分隔；为空表示不限制
//	DEFAULT_UPSTREAM  裸路径默认上游，默认 cloudflare
//	CACHE_TTL         GET+application/dns-message 的缓存秒数，默认 0（不缓存）
//	VERBOSE=1         打印每条请求日志
//	HEALTH_PATH       健康检查路径，默认空（关闭）
//	TLS_CERT/TLS_KEY  证书与私钥路径；配置后按上面的规则启用 HTTPS
package main

import (
	"bytes"
	"container/list"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxBodyBytes = 64 << 10 // DoH 报文很小，64KB 足够

	// 上游 TCP 建连（含域名解析）超时。不设的话只能等 http.Client 的整体超时（15 秒），
	// 黑洞型故障（SYN 无应答）会把整份预算耗在第一家上游上，故障转移形同虚设
	dialTimeout = 5 * time.Second

	// 上游单次请求的整体超时（建连 + 应答头 + 读体）
	upstreamTimeout = 15 * time.Second

	// 应答缓存的总字节预算。只限条目数不够：单条应答最大 64KB，默认 10000 条最坏能撑到约
	// 640MB——超过 MEM_LIMIT_MB 的软上限后会触发 GC 空转，等于被少量恶意查询打死
	maxCacheBytes = 64 << 20
)

// version 由构建时注入（go build -ldflags "-X main.version=X.Y.Z"），未注入时为 dev。
// 单一来源是仓库根目录 package.json 的 version，发布流程负责注入
var version = "dev"

// Mapping 一条路径前缀映射
type Mapping struct {
	TargetDomain string            `json:"targetDomain"`
	PathMapping  map[string]string `json:"pathMapping"`
}

// Config 运行时配置
type Config struct {
	Mappings        map[string]Mapping
	AllowedHosts    []string
	DefaultUpstream string
	CacheTTL        int
	Verbose         bool
	HealthPath      string
	MetricsPath     string

	// 并发与资源限制（对应 nginx 的 worker_connections / keepalive / 超时族）
	MaxInFlight     int // 全局在途请求上限，满则 503（worker_connections 的作用）
	MaxConnsPerHost int // 单上游连接上限，形成背压（keepalive 连接池上限）
	MaxRPS          int // 全局限速（请求/秒），0 = 不限
	MemLimitMB      int // 软内存上限（debug.SetMemoryLimit），0 = 不限
	ReadTimeoutSec  int // 读完整请求（含 body）超时，防 slow-loris
	WriteTimeoutSec int // 写响应超时，防慢客户端占连接

	// 内存缓存（热门域名命中走本地，微秒级）
	CacheMaxTTL  int // 单条缓存最长保留秒数（同时是记录 TTL 的上限），0 = 关闭
	CacheEntries int // LRU 条目数上限

	// 上游熔断与故障转移
	CBThreshold int           // 连续失败多少次熔断
	CBCooldown  time.Duration // 熔断冷却时长
	Failover    []string      // 请求的上游熔断后，按此顺序尝试其它上游

	// 上游建连超时，0 = dialTimeout（留给测试缩短黑洞上游的等待时间）
	DialTimeout time.Duration
}

// defaultMappings 与 Worker 版保持一致
var defaultMappings = map[string]Mapping{
	"/google":     {TargetDomain: "dns.google", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
	"/cloudflare": {TargetDomain: "one.one.one.one", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
	"/quad9":      {TargetDomain: "dns.quad9.net", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
	"/adguard":    {TargetDomain: "dns.adguard-dns.com", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
	"/dnspod":     {TargetDomain: "doh.pub", PathMapping: map[string]string{"/query-dns": "/dns-query", "/dns-query": "/dns-query"}},
	"/alidns":     {TargetDomain: "dns.alidns.com", PathMapping: map[string]string{"/query-dns": "/dns-query"}},
}

// homepageHTML 未命中时返回的伪装 404 页面（与 Worker 版逐字一致）
const homepageHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta http-equiv="X-UA-Compatible" content="IE=edge">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>404</title>
    <script>document.title = location.host || '404';</script>
    <style>
        body{
            font-family: system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif;
            margin: 0;
        }
        .main{
            width: 600px;
            max-width: 92%;
            height:417px;
            margin: 0 auto;
            margin-top: 125px;
        }
        img{
            margin-bottom: 20px;
        }
        p{
            font-size: 14px;
            color: rgb(32, 33, 36);
        }
        span{
            color:rgb(26, 115, 232);
            cursor: pointer;
        }
        span:hover{
            border-bottom: 1px solid rgb(26, 115, 232);
        }
        button{
            float: right;
            margin-top: 60px;
            background-color: #1970e5;
            color: #fff;
            border-width: 0px;
            padding:10px;
            border-radius: 10%;
            cursor: pointer;
        }
    </style>
</head>
<body>
    <div class="main">
        <img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEgAAABIAQMAAABvIyEEAAAABlBMVEUAAABTU1OoaSf/AAAAAXRSTlMAQObYZgAAAENJREFUeF7tzbEJACEQRNGBLeAasBCza2lLEGx0CxFGG9hBMDDxRy/72O9FMnIFapGylsu1fgoBdkXfUHLrQgdfrlJN1BdYBjQQm3UAAAAASUVORK5CYII=" alt="">
        <p style="font-size:24px;">无法访问此网站</p>
        <p>检查是否有拼写错误。</p>
        <p>如果拼写无误，请<span>尝试运行 Windows 网络诊断</span>。</p>
        <p style="font-size:12px;">DNS_PROBE_FINISHED_NXDOMAIN</p>
        <button onclick="location.reload()">重新加载</button>
    </div>

</body>
</html>`

// loadConfig 从环境变量读取配置
func loadConfig() Config {
	cfg := Config{
		Mappings:        defaultMappings,
		DefaultUpstream: "cloudflare",
		MaxInFlight:     envInt("MAX_INFLIGHT", 512),
		MaxConnsPerHost: envInt("MAX_CONNS_PER_HOST", 64),
		MaxRPS:          envInt("MAX_RPS", 0),
		MemLimitMB:      envInt("MEM_LIMIT_MB", 512),
		ReadTimeoutSec:  envTimeout("READ_TIMEOUT_SEC", 20),
		WriteTimeoutSec: envTimeout("WRITE_TIMEOUT_SEC", 60),
		CacheMaxTTL:     envInt("CACHE_MAX_TTL", 300),
		CacheEntries:    envInt("CACHE_ENTRIES", 10000),
		CBThreshold:     envInt("CB_THRESHOLD", 5),
		CBCooldown:      time.Duration(envInt("CB_COOLDOWN_SEC", 30)) * time.Second,
	}

	if raw := strings.TrimSpace(os.Getenv("DOMAIN_MAPPINGS")); raw != "" {
		var parsed map[string]Mapping
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			log.Printf("DOMAIN_MAPPINGS 解析失败，回退默认映射: %v", err)
		} else if len(parsed) > 0 {
			cfg.Mappings = parsed
		}
	}

	// 故障转移链：默认 cloudflare → google → adguard（全部海外可达且无过滤差异主嫌时可用）。
	// 必须在 DOMAIN_MAPPINGS 覆盖之后再校验，否则自定义映射里的上游名会被误判不在映射表而丢弃
	chain := strings.TrimSpace(os.Getenv("FAILOVER_CHAIN"))
	if chain == "" {
		chain = "cloudflare,google,adguard"
	}
	seen := map[string]bool{}
	for _, name := range strings.Split(chain, ",") {
		if name = strings.Trim(strings.ToLower(name), "/ "); name != "" {
			if m, ok := cfg.Mappings["/"+name]; !ok || m.TargetDomain == "" {
				log.Printf("FAILOVER_CHAIN 里的 %q 不在映射表中，忽略", name)
			} else if !seen[name] {
				// 重复的名字会让同一家上游在一次请求里被试两遍
				seen[name] = true
				cfg.Failover = append(cfg.Failover, name)
			}
		}
	}

	if raw := strings.TrimSpace(os.Getenv("ALLOWED_HOSTS")); raw != "" {
		for _, h := range strings.Split(raw, ",") {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				cfg.AllowedHosts = append(cfg.AllowedHosts, h)
			}
		}
	}

	if v := strings.TrimSpace(os.Getenv("DEFAULT_UPSTREAM")); v != "" {
		cfg.DefaultUpstream = strings.TrimPrefix(strings.ToLower(v), "/")
	}

	if v := strings.TrimSpace(os.Getenv("CACHE_TTL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.CacheTTL = n
		}
	}

	cfg.Verbose = os.Getenv("VERBOSE") == "1"
	cfg.HealthPath = strings.TrimSpace(os.Getenv("HEALTH_PATH"))
	cfg.MetricsPath = strings.TrimSpace(os.Getenv("METRICS_PATH"))
	return cfg
}

// envInt 读取整数环境变量，非法或缺省时用默认值；<=0 时按调用方语义处理
func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("%s=%q 非法，使用默认值 %d", key, v, def)
		return def
	}
	return n
}

// envTimeout 读取秒数形式的超时。负值或 0 会变成「立即超时」（负的 time.Duration 等于截止时间
// 已过），所有连接当场断掉，所以一律按非法值处理
func envTimeout(key string, def int) int {
	n := envInt(key, def)
	if n <= 0 {
		log.Printf("%s=%d 无效（必须大于 0），使用默认值 %d", key, n, def)
		return def
	}
	return n
}

// hostAllowed 判断 Host 是否在白名单内
func (c Config) hostAllowed(host string) bool {
	if len(c.AllowedHosts) == 0 {
		return true
	}
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, allowed := range c.AllowedHosts {
		if host == allowed {
			return true
		}
	}
	return false
}

// dohEndpointPath 返回某上游的 DoH 端点路径，裸路径与故障转移都打这里。
// pathMapping 把 /dns-query 或 /query-dns 映射到别处，说明端点不在标准位置
// （例如 dns.nextdns.io/<profile>），必须跟着映射走，否则会打到上游不存在的 /dns-query
func dohEndpointPath(m Mapping) string {
	for _, k := range []string{"/dns-query", "/query-dns"} {
		if v := m.PathMapping[k]; v != "" {
			return "/" + strings.TrimLeft(v, "/")
		}
	}
	return "/dns-query"
}

// providerTarget 构造指定上游的 DoH 目标 URL（裸路径与故障转移共用）
func (c Config) providerTarget(provider, rawQuery string) (string, bool) {
	m, ok := c.Mappings["/"+strings.TrimPrefix(provider, "/")]
	if !ok || m.TargetDomain == "" {
		return "", false
	}
	u := "https://" + m.TargetDomain + dohEndpointPath(m)
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u, true
}

// resolveTarget 返回（上游名, 转发目标 URL）；ok=false 表示不转发。
//
// path 必须是「转义后」的形式（r.URL.EscapedPath()），与边缘版的 url.pathname 一致：
// 若传入解码后的 r.URL.Path，路径里的 %3F / %23 拼进目标 URL 后会被再解析一次，
// 变成查询串分隔符或片段
func (c Config) resolveTarget(path, rawQuery string) (string, string, bool) {
	// 最长前缀优先
	var prefix string
	for p := range c.Mappings {
		if strings.HasPrefix(path, p) && len(p) > len(prefix) {
			prefix = p
		}
	}

	if prefix == "" {
		// 兼容 https://域名/dns-query 这种「无前缀」的标准 DoH 写法
		bare := strings.TrimRight(path, "/")
		if bare == "/dns-query" || bare == "/query-dns" {
			if u, ok := c.providerTarget(c.DefaultUpstream, rawQuery); ok {
				return c.DefaultUpstream, u, true
			}
		}
		return "", "", false
	}

	m := c.Mappings[prefix]
	if m.TargetDomain == "" {
		return "", "", false
	}

	rest := path[len(prefix):]
	// 按路径映射表重写（键按长度倒序，保证最长匹配）
	keys := make([]string, 0, len(m.PathMapping))
	for k := range m.PathMapping {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	targetPath := rest
	for _, k := range keys {
		if strings.HasPrefix(rest, k) {
			targetPath = m.PathMapping[k] + rest[len(k):]
			break
		}
	}
	if targetPath == "" {
		targetPath = "/"
	}
	if !strings.HasPrefix(targetPath, "/") {
		targetPath = "/" + targetPath
	}

	u := "https://" + m.TargetDomain + targetPath
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return strings.TrimPrefix(prefix, "/"), u, true
}

// isDoHRequest 判断请求是否确实是 DoH 协议流量
func isDoHRequest(r *http.Request) bool {
	a := strings.ToLower(r.Header.Get("Accept"))
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.Contains(a, "application/dns-message") || strings.Contains(ct, "application/dns-message")
}

// ---------- DNS 报文解析（用于缓存键与 TTL 处理） ----------

// dnsQuery 从查询报文里提取的信息：改写应答的事务 ID 用 id，缓存键与入缓存前的校验用其余字段
type dnsQuery struct {
	id       uint16
	question []byte // QNAME+QTYPE+QCLASS（不含事务 ID），指向请求体
	key      []byte // 缓存键 = flags + question + EDNS 信息
	hasOPT   bool   // 查询带 EDNS0 OPT
	doBit    bool   // 查询置了 DNSSEC OK（DO）位
}

// parseDNSQuery 解析查询报文。
// 缓存键 = flags + question 段（QNAME+QTYPE+QCLASS）+ EDNS 信息，不含事务 ID——
// 不同客户端的 ID 不同但应答相同，命中后按客户端 ID 改写即可。
//
// EDNS 信息必须进键：带 DO 位的客户端（自己做 DNSSEC 验证）要的应答里有 RRSIG 与 OPT，
// 不带的客户端拿到的应答里没有。两者共用一个键的话，先到的那份应答会被错发给另一种客户端。
// question 之后还有记录但解析不了（畸形报文）时视为不可缓存。
func parseDNSQuery(body []byte) (dnsQuery, bool) {
	var q dnsQuery
	if len(body) < 17 { // 12 头 + 最短 question(1+4)
		return q, false
	}
	if int(body[4])<<8|int(body[5]) != 1 { // QDCOUNT 必须为 1
		return q, false
	}
	q.id = uint16(body[0])<<8 | uint16(body[1])
	i := 12
	for i < len(body) {
		l := int(body[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 { // question 里不该有压缩指针
			return q, false
		}
		i += 1 + l
	}
	if i+4 > len(body) {
		return q, false
	}
	q.question = body[12 : i+4]
	q.key = append(q.key, body[2], body[3]) // flags
	q.key = append(q.key, q.question...)

	i += 4 // 跳过 QTYPE + QCLASS，从这里开始是记录区
	// 跳过 answer / authority 段，在 additional 段里找 OPT
	an, ns, ar := int(body[6])<<8|int(body[7]), int(body[8])<<8|int(body[9]), int(body[10])<<8|int(body[11])
	for c := 0; c < an+ns+ar; c++ {
		ttlOff, typ, next, ok := rrNext(body, i)
		if !ok {
			return dnsQuery{}, false
		}
		if c >= an+ns && typ == 41 { // OPT 的 TTL 字段最高位就是 DO
			q.hasOPT = true
			q.doBit = body[ttlOff]&0x80 != 0
		}
		i = next
	}
	var edns byte
	if q.hasOPT {
		edns |= 1
	}
	if q.doBit {
		edns |= 2
	}
	q.key = append(q.key, edns)
	return q, true
}

// rrNext 从 i 处解析一条 RR，返回（TTL 字段偏移, 记录类型, 下一记录偏移）
func rrNext(b []byte, i int) (ttlOff, typ, next int, ok bool) {
	if i >= len(b) {
		return 0, 0, 0, false
	}
	if b[i]&0xC0 == 0xC0 { // 压缩指针名
		i += 2
	} else {
		for i < len(b) {
			l := int(b[i])
			if l == 0 {
				i++
				break
			}
			if l&0xC0 != 0 {
				return 0, 0, 0, false
			}
			i += 1 + l
		}
	}
	if i+10 > len(b) {
		return 0, 0, 0, false
	}
	typ = (int(b[i]) << 8) | int(b[i+1])
	rdlen := (int(b[i+8]) << 8) | int(b[i+9])
	if next := i + 10 + rdlen; next > len(b) {
		return 0, 0, 0, false // 记录数据超出报文长度：报文不完整（对齐边缘版 rrNext）
	}
	return i + 4, typ, i + 10 + rdlen, true
}

// isPseudoRR EDNS0 OPT / TSIG / TKEY 不是真实资源记录，其 TTL 字段含义不同（OPT 恒为 0），
// 参与 TTL 计算会把整条应答误判成不可缓存，必须跳过
func isPseudoRR(typ int) bool {
	return typ == 41 /* OPT */ || typ == 249 /* TKEY */ || typ == 250 /* TSIG */
}

// dnsSectionStart 返回应答记录区起始位置与 AN+NS+AR 记录总数
func dnsSectionStart(b []byte) (int, int, bool) {
	if len(b) < 12 {
		return 0, 0, false
	}
	qd := (int(b[4]) << 8) | int(b[5])
	i := 12
	for q := 0; q < qd; q++ {
		for i < len(b) {
			l := int(b[i])
			if l == 0 {
				i++
				break
			}
			if l&0xC0 != 0 { // 指针只可能出现在记录区，question 里遇到就按 2 字节跳过
				i += 2
				break
			}
			i += 1 + l
		}
		i += 4 // QTYPE + QCLASS
	}
	if i > len(b) {
		return 0, 0, false
	}
	an := (int(b[6]) << 8) | int(b[7])
	ns := (int(b[8]) << 8) | int(b[9])
	ar := (int(b[10]) << 8) | int(b[11])
	return i, an + ns + ar, true
}

// dnsMinTTL 返回应答里最小的记录 TTL；解析失败返回 -1
func dnsMinTTL(b []byte) int {
	start, count, ok := dnsSectionStart(b)
	if !ok {
		return -1
	}
	min := -1
	i := start
	for c := 0; c < count; c++ {
		ttlOff, typ, next, ok2 := rrNext(b, i)
		if !ok2 || ttlOff+4 > len(b) {
			return -1
		}
		if !isPseudoRR(typ) {
			ttl := int(b[ttlOff])<<24 | int(b[ttlOff+1])<<16 | int(b[ttlOff+2])<<8 | int(b[ttlOff+3])
			if min < 0 || ttl < min {
				min = ttl
			}
		}
		i = next
	}
	return min
}

// decrementTTLs 把应答中所有记录 TTL 减去 sec（缓存命中时使用），返回新切片；
// 解析失败时原样返回，宁可 TTL 偏大也不破坏报文
func decrementTTLs(b []byte, sec int) []byte {
	out := append([]byte(nil), b...)
	start, count, ok := dnsSectionStart(out)
	if !ok {
		return b
	}
	i := start
	for c := 0; c < count; c++ {
		ttlOff, typ, next, ok2 := rrNext(out, i)
		if !ok2 || ttlOff+4 > len(out) {
			return b
		}
		if !isPseudoRR(typ) { // OPT 的 TTL 字段是扩展 RCODE/标志位，绝不能递减
			ttl := int(out[ttlOff])<<24 | int(out[ttlOff+1])<<16 | int(out[ttlOff+2])<<8 | int(out[ttlOff+3])
			if ttl > sec {
				ttl -= sec
			} else {
				ttl = 0
			}
			out[ttlOff], out[ttlOff+1], out[ttlOff+2], out[ttlOff+3] = byte(ttl>>24), byte(ttl>>16), byte(ttl>>8), byte(ttl)
		}
		i = next
	}
	return out
}

// rewriteID 把应答的事务 ID 改成客户端的（缓存命中时使用）
func rewriteID(b []byte, id uint16) []byte {
	out := append([]byte(nil), b...)
	out[0] = byte(id >> 8)
	out[1] = byte(id)
	return out
}

// ---------- LRU 应答缓存 ----------

type cacheEntry struct {
	key         string
	body        []byte
	contentType string
	fetchedAt   time.Time
	ttl         int // 有效秒数 = min(记录最小 TTL, CacheMaxTTL)
}

// size 一条缓存占用的内存（键 + 应答体；结构体本身的几十字节忽略不计）
func (e cacheEntry) size() int { return len(e.key) + len(e.body) }

// dnsCache 按条目数与总字节数双重上限淘汰的 LRU
type dnsCache struct {
	mu       sync.Mutex
	max      int // 条目数上限
	maxBytes int // 总字节上限
	bytes    int // 当前占用
	ll       *list.List
	entries  map[string]*list.Element
	hits     atomic.Uint64
	misses   atomic.Uint64
}

func newDNSCache(maxEntries, maxBytes int) *dnsCache {
	if maxEntries <= 0 || maxBytes <= 0 {
		return nil
	}
	return &dnsCache{
		max:      maxEntries,
		maxBytes: maxBytes,
		ll:       list.New(),
		entries:  make(map[string]*list.Element, min(maxEntries, 4096)/4+8),
	}
}

// removeLocked 摘掉一个条目（调用方持锁）
func (c *dnsCache) removeLocked(el *list.Element) {
	e := el.Value.(cacheEntry)
	c.bytes -= e.size()
	delete(c.entries, e.key)
	c.ll.Remove(el)
}

// get 取缓存；过期条目就地摘除并按未命中处理
func (c *dnsCache) get(key string) (cacheEntry, bool) {
	// 过期判断必须持锁：解锁后再摘除，可能误删这期间刚被刷新写入的新条目
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		c.misses.Add(1)
		return cacheEntry{}, false
	}
	e := el.Value.(cacheEntry)
	if time.Since(e.fetchedAt) >= time.Duration(e.ttl)*time.Second {
		c.removeLocked(el)
		c.misses.Add(1)
		return cacheEntry{}, false
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	return e, true
}

// put 写入缓存；超过条目数或总字节上限时，从最久未用的一端开始淘汰
func (c *dnsCache) put(key string, e cacheEntry) {
	e.key = key
	c.mu.Lock()
	defer c.mu.Unlock()
	// 单条就超过总预算：不入缓存，也不为它把别的条目淘汰掉
	if e.size() > c.maxBytes {
		return
	}
	if el, ok := c.entries[key]; ok {
		c.bytes -= el.Value.(cacheEntry).size()
		el.Value = e
		c.ll.MoveToFront(el)
	} else {
		c.entries[key] = c.ll.PushFront(e)
	}
	c.bytes += e.size()
	for c.ll.Len() > 0 && (c.ll.Len() > c.max || c.bytes > c.maxBytes) {
		c.removeLocked(c.ll.Back())
	}
}

func (c *dnsCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *dnsCache) sizeBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// ---------- 上游熔断器 ----------

type providerHealth struct {
	mu        sync.Mutex
	fails     int
	until     time.Time
	threshold int
	cooldown  time.Duration
}

// allow 是否允许尝试该上游（熔断冷却结束后放行一次探测请求）
func (h *providerHealth) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.threshold <= 0 || h.fails < h.threshold {
		return true
	}
	return time.Now().After(h.until)
}

func (h *providerHealth) record(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		h.fails = 0
		return
	}
	h.fails++
	if h.fails >= h.threshold {
		h.until = time.Now().Add(h.cooldown)
	}
}

func (h *providerHealth) snapshot() (healthy bool, fails int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fails < h.threshold, h.fails
}

// cacheControl 计算响应缓存策略；第二个返回值表示是否需要 Vary: Accept
func (c Config) cacheControl(r *http.Request) (string, bool) {
	if c.CacheTTL > 0 && r.Method == http.MethodGet &&
		strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/dns-message") {
		return fmt.Sprintf("public, max-age=%d", c.CacheTTL), true
	}
	return "no-store", false
}

// hop-by-hop 头，转发时需要剥掉
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type server struct {
	cfg    Config
	client *http.Client

	// 并发控制与运行指标
	inFlight    chan struct{}
	inFlightNum atomic.Int64
	totalReq    atomic.Uint64
	totalErr    atomic.Uint64
	startedAt   time.Time

	rpsMu       sync.Mutex
	windowStart time.Time
	windowCount int

	// 应答缓存与上游熔断
	cache    *dnsCache
	healthMu sync.Mutex
	health   map[string]*providerHealth
}

func newServer(cfg Config) *server {
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 512
	}
	if cfg.MaxConnsPerHost <= 0 {
		cfg.MaxConnsPerHost = 64
	}
	if cfg.CBThreshold <= 0 {
		cfg.CBThreshold = 5
	}
	if cfg.CBCooldown <= 0 {
		cfg.CBCooldown = 30 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = dialTimeout
	}
	// 负的超时等于「截止时间已过」，所有连接会当场断掉；<=0 一律回默认值
	if cfg.ReadTimeoutSec <= 0 {
		cfg.ReadTimeoutSec = 20
	}
	if cfg.WriteTimeoutSec <= 0 {
		cfg.WriteTimeoutSec = 60
	}

	// 传输层按 nginx 的连接复用思路调优：
	//   MaxConnsPerHost 形成背压（多余的请求排队等空闲连接，而不是无限新建）
	//   ClientSessionCache 复用 TLS 会话，省掉重复握手（高并发下 CPU 的大头）
	//   DialContext 给建连阶段单独设超时：默认拨号器没有超时，黑洞上游要等满整体超时才失败
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.MaxConnsPerHost * 4,
		MaxIdleConnsPerHost:   cfg.MaxConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
		},
	}

	s := &server{
		cfg:       cfg,
		client:    &http.Client{Transport: transport, Timeout: upstreamTimeout},
		inFlight:  make(chan struct{}, cfg.MaxInFlight),
		startedAt: time.Now(),
		health:    make(map[string]*providerHealth),
	}
	if cfg.CacheMaxTTL > 0 {
		s.cache = newDNSCache(cfg.CacheEntries, maxCacheBytes)
	}
	for name := range cfg.Mappings {
		s.health[strings.TrimPrefix(name, "/")] = &providerHealth{
			threshold: cfg.CBThreshold,
			cooldown:  cfg.CBCooldown,
		}
	}
	return s
}

// healthFor 取上游健康状态（未知上游按需创建）
func (s *server) healthFor(name string) *providerHealth {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h, ok := s.health[name]
	if !ok {
		h = &providerHealth{threshold: s.cfg.CBThreshold, cooldown: s.cfg.CBCooldown}
		s.health[name] = h
	}
	return h
}

// candidates 返回本次请求可尝试的上游顺序：首选 + 健康检查通过的转移链
func (s *server) candidates(primary string) []string {
	out := make([]string, 0, 1+len(s.cfg.Failover))
	if s.healthFor(primary).allow() {
		out = append(out, primary)
	}
	for _, f := range s.cfg.Failover {
		if f == primary {
			continue
		}
		if s.healthFor(f).allow() {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		// 全部处于熔断冷却中：仍然尝试首选，避免把所有请求直接判死
		out = append(out, primary)
	}
	return out
}

// tooManyRequests 全局限速（按秒窗口计数）
func (s *server) tooManyRequests() bool {
	if s.cfg.MaxRPS <= 0 {
		return false
	}
	s.rpsMu.Lock()
	defer s.rpsMu.Unlock()
	now := time.Now()
	if now.Sub(s.windowStart) >= time.Second {
		s.windowStart = now
		s.windowCount = 0
	}
	s.windowCount++
	return s.windowCount > s.cfg.MaxRPS
}

func (s *server) writeMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	providers := make(map[string]any, len(s.health))
	s.healthMu.Lock()
	for name, h := range s.health {
		healthy, fails := h.snapshot()
		providers[name] = map[string]any{"healthy": healthy, "consecutive_failures": fails}
	}
	s.healthMu.Unlock()

	m := map[string]any{
		"version":         version,
		"in_flight":       s.inFlightNum.Load(),
		"total_requests":  s.totalReq.Load(),
		"upstream_errors": s.totalErr.Load(),
		"uptime_seconds":  int64(time.Since(s.startedAt).Seconds()),
		"max_in_flight":   s.cfg.MaxInFlight,
		"max_rps":         s.cfg.MaxRPS,
		"providers":       providers,
	}
	if s.cache != nil {
		m["cache_hits"] = s.cache.hits.Load()
		m["cache_misses"] = s.cache.misses.Load()
		m["cache_entries"] = s.cache.len()
		m["cache_bytes"] = s.cache.sizeBytes()
		m["cache_max_ttl"] = s.cfg.CacheMaxTTL
		m["cache_max_bytes"] = s.cache.maxBytes
	} else {
		m["cache_entries"] = 0
	}
	_ = json.NewEncoder(w).Encode(m)
}

// plainError 统一的纯文本错误应答（与边缘版 plainResponse 一致）。
// 瞬时错误（繁忙 / 限速 / 上游故障）必须 no-store：否则前置 CDN 可能把一次 503 缓存成粘性故障
func plainError(w http.ResponseWriter, code int, text, retryAfter string) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	http.Error(w, text, code)
}

// clientGone 请求上下文已结束（客户端断开 / 连接被服务端超时关闭）
func clientGone(r *http.Request) bool { return r.Context().Err() != nil }

func (s *server) camo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, homepageHTML)
}

// ServeHTTP 处理顺序：在途上限 → 限速 → Host 白名单 → healthz/metrics → 目标解析 →
// 读请求体 → 缓存命中即返回 → 上游候选链（熔断 + 故障转移）→ 存缓存 → 响应
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	// 任何 panic 都不能带垮连接之外的状态
	defer func() {
		if err := recover(); err != nil {
			log.Printf("panic: %v\n%s", err, debug.Stack())
			// 响应头一旦发出就改不了状态码，此时 http.Error 只会留下一行 superfluous 警告
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}()

	// 在途请求上限：满则快速失败，避免 goroutine 无界堆积
	select {
	case s.inFlight <- struct{}{}:
		s.inFlightNum.Add(1)
		s.totalReq.Add(1)
		defer func() {
			<-s.inFlight
			s.inFlightNum.Add(-1)
		}()
	default:
		plainError(w, http.StatusServiceUnavailable, "server busy", "1")
		return
	}

	if s.tooManyRequests() {
		plainError(w, http.StatusTooManyRequests, "rate limited", "1")
		return
	}

	if !s.cfg.hostAllowed(r.Host) {
		s.camo(w)
		return
	}

	// 路径一律用转义后的形式比较与转发（原因见 resolveTarget）
	path := r.URL.EscapedPath()
	if s.cfg.HealthPath != "" && path == s.cfg.HealthPath {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok")
		return
	}

	if s.cfg.MetricsPath != "" && path == s.cfg.MetricsPath {
		s.writeMetrics(w)
		return
	}

	provider, target, ok := s.cfg.resolveTarget(path, r.URL.RawQuery)
	if !ok && (path == "/" || path == "") && isDoHRequest(r) {
		provider, target, ok = s.cfg.resolveTarget("/dns-query", r.URL.RawQuery)
	}
	if !ok {
		s.camo(w)
		return
	}

	// 读请求体（DoH 报文很小，64KB 上限；超限直接拒绝，
	// 避免把静默截断后的畸形报文发给上游浪费一次查询）
	var bodyBuf []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// Content-Length 已经超限时不必先读完再拒绝
		if r.ContentLength > maxBodyBytes {
			plainError(w, http.StatusRequestEntityTooLarge, "request body too large", "")
			return
		}
		buf, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			if clientGone(r) {
				return // 客户端中途断开，这个 400 没人收
			}
			plainError(w, http.StatusBadRequest, "bad request body", "")
			return
		}
		if len(buf) > maxBodyBytes {
			plainError(w, http.StatusRequestEntityTooLarge, "request body too large", "")
			return
		}
		bodyBuf = buf
	}

	// 组装缓存键（只对 wire 格式 DoH 查询缓存，JSON 查询不缓存）
	cacheKey := ""
	var query dnsQuery
	if s.cache != nil {
		if q, ok := dohQueryOf(r, bodyBuf); ok {
			query = q
			cacheKey = provider + "|" + string(q.key)
		}
	}

	// 命中内存缓存：改写 ID、递减 TTL 后直接返回
	if cacheKey != "" {
		if e, hit := s.cache.get(cacheKey); hit {
			elapsed := int(time.Since(e.fetchedAt).Seconds())
			out := rewriteID(decrementTTLs(e.body, elapsed), query.id)
			s.writeViaHeaders(w, r, e.contentType)
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			if s.cfg.Verbose {
				log.Printf("%s %s%s -> cache HIT (%dB, %s)", r.Method, r.Host, path, len(out), time.Since(started).Round(time.Millisecond))
			}
			return
		}
	}

	// 依次尝试：首选上游 → 故障转移链（熔断中的上游会被跳过）
	var (
		resp      *http.Response
		usedURL   = target
		lastErr   error
		gotStatus int
		cacheable []byte
		readBuf   []byte
	)
	for _, cand := range s.candidates(provider) {
		attemptURL := target
		if cand != provider {
			u, ok2 := s.cfg.providerTarget(cand, r.URL.RawQuery)
			if !ok2 {
				continue
			}
			attemptURL = u
		}

		var body io.Reader
		if bodyBuf != nil {
			body = bytes.NewReader(bodyBuf)
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, attemptURL, body)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header = r.Header.Clone()
		for _, h := range hopHeaders {
			req.Header.Del(h)
		}
		req.Header.Del("Host")
		req.Header.Del("Content-Length")
		// Accept-Encoding 也不能透传：transport 只对「自己加的」gzip 做透明解压，
		// 克隆客户端的会导致压缩应答原样进缓存，未带该头的客户端命中后拿到压缩体无法解析。
		// 剥掉后由 transport 自行协商，缓存恒存 identity 字节（与边缘版语义一致）。
		req.Header.Del("Accept-Encoding")
		// 始终显式设置长度，避免 chunked 转发被部分 DoH 上游拒绝
		req.ContentLength = int64(len(bodyBuf))

		resp, err = s.client.Do(req)
		if err != nil {
			// 请求上下文已结束，说明是客户端先断开（或本端超时关了连接）：这不是上游的故障。
			// 记成失败的话，几个没耐心的客户端就能把整条候选链逐家熔断；
			// 也没必要再转移——没有人会收到应答
			if clientGone(r) {
				return
			}
			s.healthFor(cand).record(false)
			lastErr = err
			continue
		}
		// 上游 5xx 或限流（429）视为不可用：读完响应体以复用连接后转移。
		// 429 是上游在限「我们这个出口」的量，换一家才能把这次查询做成功
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
			resp.Body.Close()
			s.healthFor(cand).record(false)
			lastErr = fmt.Errorf("%s 返回 %d", cand, resp.StatusCode)
			gotStatus = resp.StatusCode
			resp = nil
			continue
		}
		s.healthFor(cand).record(true)

		// 可缓存响应先整段读出（DoH 应答极小），顺便落缓存。
		// 多读 1 字节用于检测截断：超出上限时不能只写截断体——客户端会按上游
		// Content-Length 挂起等剩余字节——改为把已读部分连同剩余一起写完。
		// 读体失败（上游中途断流）记该上游失败并继续候选链（对齐边缘版行为）
		if cacheKey != "" && resp.StatusCode == http.StatusOK {
			buf, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
			if readErr != nil {
				resp.Body.Close()
				if clientGone(r) {
					return
				}
				s.healthFor(cand).record(false)
				lastErr = readErr
				resp = nil
				continue
			}
			readBuf = buf
			if len(readBuf) <= maxBodyBytes {
				cacheable = readBuf
				s.storeCache(query, cacheKey, cacheable, resp.Header.Get("Content-Type"))
			}
		}
		usedURL = attemptURL
		break
	}

	if resp == nil {
		s.totalErr.Add(1)
		if s.cfg.Verbose {
			log.Printf("upstream failed: %s %s -> %v", r.Method, target, lastErr)
		}
		code := http.StatusBadGateway
		if gotStatus != 0 {
			code = http.StatusServiceUnavailable
		}
		plainError(w, code, "upstream error", "2")
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	cc, needVary := s.cfg.cacheControl(r)
	w.Header().Set("Cache-Control", cc)
	if needVary {
		if v := w.Header().Get("Vary"); v != "" {
			w.Header().Set("Vary", v+", Accept")
		} else {
			w.Header().Set("Vary", "Accept")
		}
	}
	if cacheKey != "" {
		w.Header().Set("X-Cache", "MISS")
	}

	w.WriteHeader(resp.StatusCode)
	var n int64
	if cacheable != nil {
		m, _ := w.Write(cacheable)
		n = int64(m)
	} else {
		if readBuf != nil {
			// 截断检测已消费了前 maxBodyBytes+1 字节，先补写已读部分再续写剩余
			m, _ := w.Write(readBuf)
			n = int64(m)
		}
		m, _ := io.Copy(w, resp.Body)
		n += m
	}

	if s.cfg.Verbose {
		log.Printf("%s %s%s -> %s %d (%dB, %s)", r.Method, r.Host, path, usedURL, resp.StatusCode, n, time.Since(started).Round(time.Millisecond))
	}
}

// writeViaHeaders 设置缓存命中响应的基础响应头
func (s *server) writeViaHeaders(w http.ResponseWriter, r *http.Request, contentType string) {
	h := w.Header()
	if contentType != "" {
		h.Set("Content-Type", contentType)
	} else {
		h.Set("Content-Type", "application/dns-message")
	}
	cc, needVary := s.cfg.cacheControl(r)
	h.Set("Cache-Control", cc)
	if needVary {
		if v := h.Get("Vary"); v != "" {
			h.Set("Vary", v+", Accept")
		} else {
			h.Set("Vary", "Accept")
		}
	}
}

// storeCache 判定并写入缓存：只缓存「确实是这条查询的完整 DNS 应答」——
// 媒体类型是 application/dns-message、QR=1、事务 ID 与 question 原样回显、未截断（TC=0）、
// rcode=0/3 且最小记录 TTL>0（rcode=3 且无 SOA 的应答没有 TTL，自然不缓存）。
func (s *server) storeCache(q dnsQuery, key string, body []byte, contentType string) {
	if s.cache == nil || s.cfg.CacheMaxTTL <= 0 || len(body) < 12+len(q.question) {
		return
	}
	// 上游用 200 返回的非 DNS 内容（例如门户认证页）不能进缓存
	if mt, _, err := mime.ParseMediaType(contentType); err != nil || mt != "application/dns-message" {
		return
	}
	if body[2]&0x80 == 0 {
		return // QR=0：这是查询不是应答
	}
	// 截断的应答只是完整应答的一部分：缓存之后，带了更大 EDNS 缓冲区的客户端也会一直拿到它，
	// 而它自己重新问一次又会命中同一份，等于这次查询在 TTL 内永远解析不完
	if body[2]&0x02 != 0 {
		return // TC=1
	}
	// 应答的 question 必须与查询逐字节相同：否则是别的查询的应答，不能挂到这个键下
	if int(body[4])<<8|int(body[5]) != 1 || !bytes.Equal(body[12:12+len(q.question)], q.question) {
		return
	}
	if uint16(body[0])<<8|uint16(body[1]) != q.id {
		return // 事务 ID 没有原样回显
	}
	rcode := int(body[3] & 0x0F)
	if rcode != 0 && rcode != 3 {
		return
	}
	ttl := dnsMinTTL(body)
	if ttl <= 0 {
		return
	}
	if ttl > s.cfg.CacheMaxTTL {
		ttl = s.cfg.CacheMaxTTL
	}
	s.cache.put(key, cacheEntry{
		body:        append([]byte(nil), body...),
		contentType: contentType,
		fetchedAt:   time.Now(),
		ttl:         ttl,
	})
}

// dohQueryOf 提取查询信息：POST 报文优先，其次 GET 的 ?dns= 参数
func dohQueryOf(r *http.Request, body []byte) (dnsQuery, bool) {
	if len(body) > 0 {
		return parseDNSQuery(body) // 体非空但解析失败时不回退 ?dns=（对齐边缘版）
	}
	v := r.URL.Query().Get("dns")
	if v == "" {
		return dnsQuery{}, false
	}
	raw, ok := decodeDNSParam(v)
	if !ok {
		return dnsQuery{}, false
	}
	return parseDNSQuery(raw)
}

// decodeDNSParam 解码 ?dns= 参数。RFC 8484 规定是不带填充的 base64url，
// 实际有客户端会带填充、或者干脆用标准 base64（+ /）：四种写法都接受
func decodeDNSParam(v string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if raw, err := enc.DecodeString(v); err == nil {
			return raw, true
		}
	}
	return nil, false
}

// listenMode 决定明文 / TLS 监听地址，并加载证书。规则：
//   - 证书与私钥都没给：纯 HTTP
//   - 都给了且没给 tlsAddr：listen 指定的端口直接变 HTTPS
//   - 都给了且给了 tlsAddr：两个一起跑（HTTP + HTTPS）
//
// 只给其中一个属于配置错误，直接拒绝启动。原先会悄悄按纯 HTTP 跑起来，
// 以为开了 TLS 的部署实际在明文监听，而日志里只有一行「HTTP 监听」。
func listenMode(addr, tlsAddr, certFile, keyFile string) (plainAddr, secureAddr string, cert tls.Certificate, err error) {
	switch {
	case certFile == "" && keyFile == "":
		return addr, "", tls.Certificate{}, nil
	case certFile == "" || keyFile == "":
		return "", "", tls.Certificate{}, fmt.Errorf("TLS_CERT（%q）与 TLS_KEY（%q）必须同时配置，只给其中一个会导致以明文 HTTP 启动", certFile, keyFile)
	}
	// 启动时就把证书读进来：路径写错、私钥不匹配都在这里报清楚，而不是等监听协程失败后只留一句「服务退出」
	if cert, err = tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return "", "", tls.Certificate{}, fmt.Errorf("加载 TLS 证书失败: %w", err)
	}
	if tlsAddr != "" {
		return addr, tlsAddr, cert, nil
	}
	return "", addr, cert, nil
}

// newHTTPServer 构造一个监听实例；secure 为 true 时用 listenMode 已加载的证书提供 TLS
func newHTTPServer(h http.Handler, addr string, cfg Config, secure bool, cert tls.Certificate) *http.Server {
	s := &http.Server{
		Addr:    addr,
		Handler: h,
		// 超时族（对应 nginx client_header_timeout / client_body_timeout / send_timeout）
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Duration(cfg.ReadTimeoutSec) * time.Second,
		WriteTimeout:      time.Duration(cfg.WriteTimeoutSec) * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10, // DoH 头很小，限制头部大小防慢速大包
	}
	if secure {
		// 证书已在 listenMode 里加载好；显式限定最低 TLS 1.2（与上游连接一致，不依赖 Go 版本的默认值）
		s.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	return s
}

func main() {
	addr := flag.String("listen", envOr("LISTEN_ADDR", ":8080"), "监听地址，如 :8080")
	tlsAddr := flag.String("tls-listen", strings.TrimSpace(os.Getenv("TLS_LISTEN")), "额外的 TLS 监听地址，如 :8444（留空则把 -listen 变成 HTTPS）")
	certFile := flag.String("tls-cert", strings.TrimSpace(os.Getenv("TLS_CERT")), "TLS 证书路径（可选）")
	keyFile := flag.String("tls-key", strings.TrimSpace(os.Getenv("TLS_KEY")), "TLS 私钥路径（可选）")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg := loadConfig()
	if cfg.MemLimitMB > 0 {
		// 软内存上限：到顶先触发 GC 降速，而不是把整机拖进 OOM
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)
	}
	srv := newServer(cfg)

	plainAddr, secureAddr, cert, err := listenMode(*addr, *tlsAddr, *certFile, *keyFile)
	if err != nil {
		log.Fatalf("%v", err)
	}

	serve := func(a string, secure bool) *http.Server {
		return newHTTPServer(srv, a, srv.cfg, secure, cert)
	}
	servers := make([]*http.Server, 0, 2)
	errCh := make(chan error, 2)

	if plainAddr != "" {
		s := serve(plainAddr, false)
		servers = append(servers, s)
		go func() {
			log.Printf("HTTP  监听 %s", plainAddr)
			errCh <- s.ListenAndServe()
		}()
	}
	if secureAddr != "" {
		s := serve(secureAddr, true)
		servers = append(servers, s)
		go func() {
			log.Printf("HTTPS 监听 %s（cert=%s）", secureAddr, *certFile)
			errCh <- s.ListenAndServeTLS("", "") // 证书已在 TLSConfig 里
		}()
	}
	if len(servers) == 0 {
		log.Fatalf("没有可用的监听地址")
	}

	log.Printf("doh-proxy %s 启动：upstreams=%d allowed_hosts=%d default=%s cache_ttl=%d",
		version, len(cfg.Mappings), len(cfg.AllowedHosts), cfg.DefaultUpstream, cfg.CacheTTL)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务退出: %v", err)
		}
	case sig := <-stop:
		log.Printf("收到信号 %s，开始优雅退出", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range servers {
			if err := s.Shutdown(ctx); err != nil {
				log.Printf("关闭 %s 超时，强制断开剩余连接: %v", s.Addr, err)
				_ = s.Close()
			}
		}
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
