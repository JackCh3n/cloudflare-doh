// DoH 路径转发代理（Go 版，仅用标准库）
//
// 与仓库根目录的 Cloudflare Worker / EdgeOne 中间件实现路由逻辑一致：
//   /<上游>/query-dns  → 该上游的 DoH 端点，保留查询串与请求体
//   /<上游>/dns-query  → 同上（路径透传）
//   /dns-query、/query-dns、/（仅 DoH 请求）→ DEFAULT_UPSTREAM
//   其它路径 → 伪装 404 页面
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
	"context"
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
	"sort"
	"strconv"
	"strings"
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
	return cfg
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

// resolveTarget 返回转发目标 URL；ok=false 表示不转发
func (c Config) resolveTarget(path, rawQuery string) (string, bool) {
	// 最长前缀优先
	var prefix string
	for p := range c.Mappings {
		if strings.HasPrefix(path, p) && len(p) > len(prefix) {
			prefix = p
		}
	}

	if prefix == "" {
		bare := strings.TrimRight(path, "/")
		if bare == "/dns-query" || bare == "/query-dns" {
			return c.bareTarget(rawQuery)
		}
		return "", false
	}

	m := c.Mappings[prefix]
	if m.TargetDomain == "" {
		return "", false
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
	return u, true
}

// bareTarget 构造裸路径的默认上游目标
func (c Config) bareTarget(rawQuery string) (string, bool) {
	m, ok := c.Mappings["/"+c.DefaultUpstream]
	if !ok || m.TargetDomain == "" {
		return "", false
	}
	u := "https://" + m.TargetDomain + "/dns-query"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u, true
}

// isDoHRequest 判断请求是否确实是 DoH 协议流量
func isDoHRequest(r *http.Request) bool {
	a := strings.ToLower(r.Header.Get("Accept"))
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.Contains(a, "application/dns-message") || strings.Contains(ct, "application/dns-message")
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
}

func newServer(cfg Config) *server {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &server{cfg: cfg, client: &http.Client{Transport: transport, Timeout: 15 * time.Second}}
}

func (s *server) camo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, homepageHTML)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

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

	target, ok := s.cfg.resolveTarget(r.URL.Path, r.URL.RawQuery)
	if !ok && (r.URL.Path == "/" || r.URL.Path == "") && isDoHRequest(r) {
		target, ok = s.cfg.bareTarget(r.URL.RawQuery)
	}
	if !ok {
		s.camo(w)
		return
	}

	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		buf, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		body = strings.NewReader(string(buf))
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		http.Error(w, "bad target", http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	req.Header.Del("Host")
	req.Header.Del("Content-Length")
	if r.ContentLength > 0 {
		req.ContentLength = r.ContentLength
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if s.cfg.Verbose {
			log.Printf("upstream failed: %s %s -> %v", r.Method, target, err)
		}
		http.Error(w, "upstream error", http.StatusBadGateway)
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

	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)

	if s.cfg.Verbose {
		log.Printf("%s %s%s -> %s %d (%dB, %s)", r.Method, r.Host, r.URL.Path, target, resp.StatusCode, n, time.Since(started).Round(time.Millisecond))
	}
}

func main() {
	addr := flag.String("listen", envOr("LISTEN_ADDR", ":8080"), "监听地址，如 :8080")
	tlsAddr := flag.String("tls-listen", os.Getenv("TLS_LISTEN"), "额外的 TLS 监听地址，如 :8444（留空则把 -listen 变成 HTTPS）")
	certFile := flag.String("tls-cert", os.Getenv("TLS_CERT"), "TLS 证书路径（可选）")
	keyFile := flag.String("tls-key", os.Getenv("TLS_KEY"), "TLS 私钥路径（可选）")
	flag.Parse()

	cfg := loadConfig()
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
			Addr:              a,
			Handler:           srv,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
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
				log.Printf("关闭 %s 超时: %v", s.Addr, err)
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
