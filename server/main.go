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

const maxBodyBytes = 64 << 10 // DoH 报文很小，64KB 足够

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
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta http-equiv="X-UA-Compatible" content="IE=edge">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>404</title>
    <style>
        .main{
            width: 600px;
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
        <p>如果拼写无误，请<span>尝试运行 Windos 网络诊断</span>。</p>
        <p style="font-size:12px;">DNS_PROBE_FINSHED_NXDOMAIN</p>
        <button>重新加载</button>
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
		ReadTimeoutSec:  envInt("READ_TIMEOUT_SEC", 20),
		WriteTimeoutSec: envInt("WRITE_TIMEOUT_SEC", 60),
		CacheMaxTTL:     envInt("CACHE_MAX_TTL", 300),
		CacheEntries:    envInt("CACHE_ENTRIES", 10000),
		CBThreshold:     envInt("CB_THRESHOLD", 5),
		CBCooldown:      time.Duration(envInt("CB_COOLDOWN_SEC", 30)) * time.Second,
	}

	// 故障转移链：默认 cloudflare → google → adguard（全部海外可达且无过滤差异主嫌时可用）
	chain := strings.TrimSpace(os.Getenv("FAILOVER_CHAIN"))
	if chain == "" {
		chain = "cloudflare,google,adguard"
	}
	for _, name := range strings.Split(chain, ",") {
		if name = strings.Trim(strings.ToLower(name), "/ "); name != "" {
			if m, ok := cfg.Mappings["/"+name]; ok && m.TargetDomain != "" {
				cfg.Failover = append(cfg.Failover, name)
			} else {
				log.Printf("FAILOVER_CHAIN 里的 %q 不在映射表中，忽略", name)
			}
		}
	}

	if raw := strings.TrimSpace(os.Getenv("DOMAIN_MAPPINGS")); raw != "" {
		var parsed map[string]Mapping
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			log.Printf("DOMAIN_MAPPINGS 解析失败，回退默认映射: %v", err)
		} else if len(parsed) > 0 {
			cfg.Mappings = parsed
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

// providerTarget 构造指定上游的 DoH 目标 URL（裸路径与故障转移共用）
func (c Config) providerTarget(provider, rawQuery string) (string, bool) {
	m, ok := c.Mappings["/"+strings.TrimPrefix(provider, "/")]
	if !ok || m.TargetDomain == "" {
		return "", false
	}
	u := "https://" + m.TargetDomain + "/dns-query"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u, true
}

// resolveTarget 返回（上游名, 转发目标 URL）；ok=false 表示不转发
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

// parseDNSQuery 从查询报文提取（事务 ID, 缓存键）。
// 缓存键 = flags + question 段（QNAME+QTYPE+QCLASS），不含事务 ID——
// 不同客户端的 ID 不同但应答相同，命中后按客户端 ID 改写即可。
func parseDNSQuery(body []byte) (uint16, []byte, bool) {
	if len(body) < 17 { // 12 头 + 最短 question(1+4)
		return 0, nil, false
	}
	if int(body[4])<<8|int(body[5]) != 1 { // QDCOUNT 必须为 1
		return 0, nil, false
	}
	id := uint16(body[0])<<8 | uint16(body[1])
	i := 12
	for i < len(body) {
		l := int(body[i])
		if l == 0 {
			i++
			break
		}
		if l&0xC0 != 0 { // question 里不该有压缩指针
			return 0, nil, false
		}
		i += 1 + l
	}
	if i+4 > len(body) {
		return 0, nil, false
	}
	key := make([]byte, 0, 2+(i+4-12))
	key = append(key, body[2], body[3]) // flags
	key = append(key, body[12:i+4]...)  // question + qtype + qclass
	return id, key, true
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

type dnsCache struct {
	mu      sync.Mutex
	max     int
	ll      *list.List
	entries map[string]*list.Element
	hits    atomic.Uint64
	misses  atomic.Uint64
}

func newDNSCache(maxEntries int) *dnsCache {
	if maxEntries <= 0 {
		return nil
	}
	return &dnsCache{
		max:     maxEntries,
		ll:      list.New(),
		entries: make(map[string]*list.Element, maxEntries/4+8),
	}
}

func (c *dnsCache) get(key string) (cacheEntry, bool) {
	c.mu.Lock()
	el, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return cacheEntry{}, false
	}
	e := el.Value.(cacheEntry)
	c.ll.MoveToFront(el)
	c.mu.Unlock()
	if time.Since(e.fetchedAt) >= time.Duration(e.ttl)*time.Second {
		c.evict(key)
		c.misses.Add(1)
		return cacheEntry{}, false
	}
	c.hits.Add(1)
	return e, true
}

func (c *dnsCache) put(key string, e cacheEntry) {
	e.key = key
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value = e
		c.ll.MoveToFront(el)
		return
	}
	c.entries[key] = c.ll.PushFront(e)
	for c.ll.Len() > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		delete(c.entries, back.Value.(cacheEntry).key)
		c.ll.Remove(back)
	}
}

func (c *dnsCache) evict(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.ll.Remove(el)
		delete(c.entries, key)
	}
}

func (c *dnsCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
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

	// 传输层按 nginx 的连接复用思路调优：
	//   MaxConnsPerHost 形成背压（多余的请求排队等空闲连接，而不是无限新建）
	//   ClientSessionCache 复用 TLS 会话，省掉重复握手（高并发下 CPU 的大头）
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
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
		client:    &http.Client{Transport: transport, Timeout: 15 * time.Second},
		inFlight:  make(chan struct{}, cfg.MaxInFlight),
		startedAt: time.Now(),
		health:    make(map[string]*providerHealth),
	}
	if cfg.CacheMaxTTL > 0 {
		s.cache = newDNSCache(cfg.CacheEntries)
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
		m["cache_max_ttl"] = s.cfg.CacheMaxTTL
	} else {
		m["cache_entries"] = 0
	}
	_ = json.NewEncoder(w).Encode(m)
}

func (s *server) camo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, homepageHTML)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	// 任何 panic 都不能带垮连接之外的状态
	defer func() {
		if err := recover(); err != nil {
			log.Printf("panic: %v", err)
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
		w.Header().Set("Retry-After", "1")
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return
	}

	if s.tooManyRequests() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	if !s.cfg.hostAllowed(r.Host) {
		s.camo(w)
		return
	}

	if s.cfg.HealthPath != "" && r.URL.Path == s.cfg.HealthPath {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok")
		return
	}

	if s.cfg.MetricsPath != "" && r.URL.Path == s.cfg.MetricsPath {
		s.writeMetrics(w)
		return
	}

	provider, target, ok := s.cfg.resolveTarget(r.URL.Path, r.URL.RawQuery)
	if !ok && (r.URL.Path == "/" || r.URL.Path == "") && isDoHRequest(r) {
		provider, target, ok = s.cfg.resolveTarget("/dns-query", r.URL.RawQuery)
	}
	if !ok {
		s.camo(w)
		return
	}

	// 读请求体（DoH 报文很小，64KB 上限）
	var bodyBuf []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		buf, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		bodyBuf = buf
	}

	// 组装缓存键（只对 wire 格式 DoH 查询缓存，JSON 查询不缓存）
	cacheKey := ""
	var reqID uint16
	if s.cache != nil {
		if id, qkey, ok := dohQueryOf(r, bodyBuf); ok {
			reqID = id
			cacheKey = provider + "|" + string(qkey)
		}
	}

	// 命中内存缓存：改写 ID、递减 TTL 后直接返回
	if cacheKey != "" {
		if e, hit := s.cache.get(cacheKey); hit {
			elapsed := int(time.Since(e.fetchedAt).Seconds())
			out := rewriteID(decrementTTLs(e.body, elapsed), reqID)
			s.writeViaHeaders(w, r, e.contentType)
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			if s.cfg.Verbose {
				log.Printf("%s %s%s -> cache HIT (%dB, %s)", r.Method, r.Host, r.URL.Path, len(out), time.Since(started).Round(time.Millisecond))
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
		// 始终显式设置长度，避免 chunked 转发被部分 DoH 上游拒绝
		req.ContentLength = int64(len(bodyBuf))

		resp, err = s.client.Do(req)
		if err != nil {
			s.healthFor(cand).record(false)
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			// 上游 5xx 视为不可用，读完响应体以复用连接后转移
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
			resp.Body.Close()
			s.healthFor(cand).record(false)
			lastErr = fmt.Errorf("%s 返回 %d", cand, resp.StatusCode)
			gotStatus = resp.StatusCode
			resp = nil
			continue
		}
		s.healthFor(cand).record(true)
		usedURL = attemptURL
		break
	}

	if resp == nil {
		s.totalErr.Add(1)
		if s.cfg.Verbose {
			log.Printf("upstream failed: %s %s -> %v", r.Method, target, lastErr)
		}
		w.Header().Set("Retry-After", "2")
		code := http.StatusBadGateway
		if gotStatus != 0 {
			code = http.StatusServiceUnavailable
		}
		http.Error(w, "upstream error", code)
		return
	}
	defer resp.Body.Close()

	// 可缓存响应先整段读出（DoH 应答极小），顺便落缓存
	var cacheable []byte
	if cacheKey != "" && resp.StatusCode == http.StatusOK {
		if buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes)); err == nil {
			cacheable = buf
			if len(cacheable) <= maxBodyBytes {
				s.storeCache(cacheKey, cacheable, resp.Header.Get("Content-Type"))
			}
		}
	}

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
		n, _ = io.Copy(w, resp.Body)
	}

	if s.cfg.Verbose {
		log.Printf("%s %s%s -> %s %d (%dB, %s)", r.Method, r.Host, r.URL.Path, usedURL, resp.StatusCode, n, time.Since(started).Round(time.Millisecond))
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

// storeCache 判定并写入缓存：仅缓存 rcode=0/3 且有有效 TTL 的应答
func (s *server) storeCache(key string, body []byte, contentType string) {
	if s.cache == nil || len(body) < 12 || s.cfg.CacheMaxTTL <= 0 {
		return
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
	if contentType == "" {
		contentType = "application/dns-message"
	}
	s.cache.put(key, cacheEntry{
		body:        append([]byte(nil), body...),
		contentType: contentType,
		fetchedAt:   time.Now(),
		ttl:         ttl,
	})
}

// dohQueryOf 提取（事务 ID, 缓存键）：POST 报文优先，其次 GET 的 ?dns= 参数
func dohQueryOf(r *http.Request, body []byte) (uint16, []byte, bool) {
	if len(body) > 0 {
		if id, k, ok := parseDNSQuery(body); ok {
			return id, k, true
		}
		return 0, nil, false
	}
	if v := r.URL.Query().Get("dns"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			raw, err = base64.URLEncoding.DecodeString(v)
			if err != nil {
				return 0, nil, false
			}
		}
		return parseDNSQuery(raw)
	}
	return 0, nil, false
}

func main() {
	addr := flag.String("listen", envOr("LISTEN_ADDR", ":8080"), "监听地址，如 :8080")
	tlsAddr := flag.String("tls-listen", os.Getenv("TLS_LISTEN"), "额外的 TLS 监听地址，如 :8444（留空则把 -listen 变成 HTTPS）")
	certFile := flag.String("tls-cert", os.Getenv("TLS_CERT"), "TLS 证书路径（可选）")
	keyFile := flag.String("tls-key", os.Getenv("TLS_KEY"), "TLS 私钥路径（可选）")
	flag.Parse()

	cfg := loadConfig()
	if cfg.MemLimitMB > 0 {
		// 软内存上限：到顶先触发 GC 降速，而不是把整机拖进 OOM
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)
	}
	srv := newServer(cfg)

	hasTLS := *certFile != "" && *keyFile != ""
	plainAddr, secureAddr := *addr, ""
	switch {
	case hasTLS && *tlsAddr != "":
		secureAddr = *tlsAddr // 双监听：HTTP + HTTPS
	case hasTLS:
		plainAddr, secureAddr = "", *addr // 单监听：-listen 直接变 HTTPS
	}

	newHTTPServer := func(a string) *http.Server {
		return &http.Server{
			Addr:    a,
			Handler: srv,
			// 超时族（对应 nginx client_header_timeout / client_body_timeout / send_timeout）
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       time.Duration(cfg.ReadTimeoutSec) * time.Second,
			WriteTimeout:      time.Duration(cfg.WriteTimeoutSec) * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    16 << 10, // DoH 头很小，限制头部大小防慢速大包
		}
	}

	servers := make([]*http.Server, 0, 2)
	errCh := make(chan error, 2)

	if plainAddr != "" {
		s := newHTTPServer(plainAddr)
		servers = append(servers, s)
		go func() {
			log.Printf("HTTP  监听 %s", plainAddr)
			errCh <- s.ListenAndServe()
		}()
	}
	if secureAddr != "" {
		s := newHTTPServer(secureAddr)
		servers = append(servers, s)
		go func() {
			log.Printf("HTTPS 监听 %s（cert=%s）", secureAddr, *certFile)
			errCh <- s.ListenAndServeTLS(*certFile, *keyFile)
		}()
	}
	if len(servers) == 0 {
		log.Fatalf("没有可用的监听地址")
	}

	log.Printf("doh-proxy 启动：upstreams=%d allowed_hosts=%d default=%s cache_ttl=%d",
		len(cfg.Mappings), len(cfg.AllowedHosts), cfg.DefaultUpstream, cfg.CacheTTL)

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
