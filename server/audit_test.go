package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// postQuery 以 POST wire 格式向测试服务发一条查询
func postQuery(s *server, path string, query []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com"+path, bytes.NewReader(query))
	req.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

// TestPathForwardedAsEscaped 路径必须按转义后的形式转发。
// 解码后的 r.URL.Path 拼进目标 URL 会被再解析一次：%3F 变成查询串分隔符、%23 变成片段、%2F 被当成路径分隔符
func TestPathForwardedAsEscaped(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/google/query-dns/a%3Fb%3Dc%23d", "https://dns.google/dns-query/a%3Fb%3Dc%23d"},
		{"/google/query-dns/x%2Fy", "https://dns.google/dns-query/x%2Fy"},
		{"/google/query-dns/%E4%B8%AD", "https://dns.google/dns-query/%E4%B8%AD"},
		{"/google/query-dns/plain", "https://dns.google/dns-query/plain"},
	}
	for _, c := range cases {
		st := &stubTransport{}
		s := testServer(Config{Mappings: defaultMappings}, st)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com"+c.path, nil))
		if w.Code != http.StatusOK || st.lastURL != c.want {
			t.Errorf("%s → code=%d url=%q，期望 %q", c.path, w.Code, st.lastURL, c.want)
		}
	}
}

// TestReservedPathsMatchExactly healthz / metrics 只认路径的书写形式，
// %68ealthz 这种转义写法不算命中（与边缘版 url.pathname 的行为一致）
func TestReservedPathsMatchExactly(t *testing.T) {
	s := testServer(Config{Mappings: defaultMappings, HealthPath: "/healthz", MetricsPath: "/metrics"}, &stubTransport{})
	for _, path := range []string{"/%68ealthz", "/healthz/", "/HEALTHZ"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com"+path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s 不应命中保留路径，实际 %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/healthz", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("/healthz 应返回 200 ok，实际 %d %q", w.Code, w.Body.String())
	}
}

// TestErrorResponsesAreNoStore 瞬时错误必须 no-store：否则前置 CDN 可能把一次 503 缓存成粘性故障
func TestErrorResponsesAreNoStore(t *testing.T) {
	assertNoStore := func(name string, w *httptest.ResponseRecorder, wantCode int) {
		t.Helper()
		if w.Code != wantCode {
			t.Errorf("%s：状态码 %d，期望 %d", name, w.Code, wantCode)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s：Cache-Control=%q，期望 no-store", name, cc)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s：缺少 X-Content-Type-Options: nosniff", name)
		}
	}

	// 503：在途槽位占满
	busy := testServer(Config{Mappings: defaultMappings, MaxInFlight: 1}, &stubTransport{})
	busy.inFlight <- struct{}{}
	busy.inFlightNum.Add(1)
	w := httptest.NewRecorder()
	busy.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns", nil))
	assertNoStore("503 busy", w, http.StatusServiceUnavailable)

	// 429：全局限速（首个请求占满每秒 1 次的窗口）
	limited := testServer(Config{Mappings: defaultMappings, MaxRPS: 1}, &stubTransport{})
	limited.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns", nil))
	w = httptest.NewRecorder()
	limited.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns", nil))
	assertNoStore("429 rate limited", w, http.StatusTooManyRequests)

	// 413：请求体超限
	s := testServer(Config{Mappings: defaultMappings}, &stubTransport{})
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(make([]byte, maxBodyBytes+1))))
	assertNoStore("413 too large", w, http.StatusRequestEntityTooLarge)

	// 400：请求体读取失败
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", errReader{}))
	assertNoStore("400 bad body", w, http.StatusBadRequest)

	// 502：上游不可达
	down := newServer(Config{Mappings: defaultMappings})
	down.client = &http.Client{Transport: &recordingTransport{fail: map[string]bool{"one.one.one.one": true}}}
	assertNoStore("502 upstream", postQuery(down, "/cloudflare/query-dns", makeQuery(0x0101)), http.StatusBadGateway)
}

// errReader 一读就失败的请求体
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("读取失败") }

// TestOversizedBodyRejectedWithoutReading Content-Length 已经超限时不必先读完再拒绝
func TestOversizedBodyRejectedWithoutReading(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings}, st)
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", errReader{})
	req.ContentLength = maxBodyBytes + 1
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge || st.lastURL != "" {
		t.Fatalf("期望 413 且不读体、不转发，实际 code=%d url=%q", w.Code, st.lastURL)
	}
}

// TestEnvTimeoutRejectsNonPositive 负的超时等于「截止时间已过」，会让所有连接当场断掉，
// 所以 READ_TIMEOUT_SEC / WRITE_TIMEOUT_SEC 的 0 与负值一律回默认值
func TestEnvTimeoutRejectsNonPositive(t *testing.T) {
	for raw, want := range map[string]int{"": 20, "-5": 20, "0": 20, "abc": 20, "7": 7} {
		t.Setenv("READ_TIMEOUT_SEC", raw)
		if got := loadConfig().ReadTimeoutSec; got != want {
			t.Errorf("READ_TIMEOUT_SEC=%q → %d，期望 %d", raw, got, want)
		}
	}
	for raw, want := range map[string]int{"": 60, "-1": 60, "0": 60, "3": 3} {
		t.Setenv("WRITE_TIMEOUT_SEC", raw)
		if got := loadConfig().WriteTimeoutSec; got != want {
			t.Errorf("WRITE_TIMEOUT_SEC=%q → %d，期望 %d", raw, got, want)
		}
	}
	// 直接构造 Config 时同样兜底
	s := newServer(Config{Mappings: defaultMappings, ReadTimeoutSec: -3, WriteTimeoutSec: 0})
	if s.cfg.ReadTimeoutSec != 20 || s.cfg.WriteTimeoutSec != 60 {
		t.Errorf("newServer 应兜底超时，实际 read=%d write=%d", s.cfg.ReadTimeoutSec, s.cfg.WriteTimeoutSec)
	}
}

// TestFailoverChainDedupes 转移链里重复的名字会让同一家上游在一次请求里被试两遍
func TestFailoverChainDedupes(t *testing.T) {
	t.Setenv("DOMAIN_MAPPINGS", "")
	t.Setenv("FAILOVER_CHAIN", "google, Google ,/google,cloudflare,,")
	cfg := loadConfig()
	if len(cfg.Failover) != 2 || cfg.Failover[0] != "google" || cfg.Failover[1] != "cloudflare" {
		t.Errorf("转移链应去重为 [google cloudflare]，实际 %v", cfg.Failover)
	}
}

// TestBareAndFailoverTargetFollowPathMapping 裸路径与故障转移的目标路径写死成 /dns-query 时，
// 端点不在标准位置的上游（如 dns.nextdns.io/<profile>）会被打到不存在的路径上
func TestBareAndFailoverTargetFollowPathMapping(t *testing.T) {
	cfg := Config{
		Mappings: map[string]Mapping{
			"/nd":   {TargetDomain: "dns.nextdns.io", PathMapping: map[string]string{"/query-dns": "/abc123"}},
			"/home": {TargetDomain: "doh.lan", PathMapping: map[string]string{"/query-dns": "/dns-query", "/dns-query": "/"}},
		},
		DefaultUpstream: "nd",
	}
	if _, got, ok := cfg.resolveTarget("/dns-query", "dns=abc"); !ok || got != "https://dns.nextdns.io/abc123?dns=abc" {
		t.Errorf("裸路径应跟着 pathMapping 走，实际 ok=%v %q", ok, got)
	}
	if got, ok := cfg.providerTarget("nd", "dns=abc"); !ok || got != "https://dns.nextdns.io/abc123?dns=abc" {
		t.Errorf("故障转移目标应跟着 pathMapping 走，实际 ok=%v %q", ok, got)
	}
	if got, _ := cfg.providerTarget("home", ""); got != "https://doh.lan/" {
		t.Errorf("/dns-query 映射到根路径时应取它，实际 %q", got)
	}
	if _, got, _ := cfg.resolveTarget("/nd/query-dns", ""); got != "https://dns.nextdns.io/abc123" {
		t.Errorf("显式前缀路径仍按 pathMapping 重写，实际 %q", got)
	}
	// 默认映射的行为不变
	for name, want := range map[string]string{
		"cloudflare": "https://one.one.one.one/dns-query",
		"dnspod":     "https://doh.pub/dns-query",
		"google":     "https://dns.google/dns-query",
	} {
		if got, ok := (Config{Mappings: defaultMappings}).providerTarget(name, ""); !ok || got != want {
			t.Errorf("默认上游 %s 的端点应为 %q，实际 ok=%v %q", name, want, ok, got)
		}
	}
}

// TestUpstreamRateLimitedTriggersFailover 上游 429 是在限「我们这个出口」的量，
// 换一家才能把这次查询做成功；全部 429 时返回 503
func TestUpstreamRateLimitedTriggersFailover(t *testing.T) {
	var hit []string
	st := &recordingTransport{hits: &hit, body: makeResponse(300), statusByHost: map[string]int{"one.one.one.one": 429}}
	s := newServer(Config{Mappings: defaultMappings, CBThreshold: 2, Failover: []string{"google"}})
	s.client = &http.Client{Transport: st}

	w := postQuery(s, "/cloudflare/query-dns", makeQuery(0x2222))
	if w.Code != http.StatusOK || len(hit) != 2 || hit[0] != "one.one.one.one" || hit[1] != "dns.google" {
		t.Fatalf("429 后应转移到 google 成功，实际 code=%d hits=%v", w.Code, hit)
	}
	if _, fails := s.healthFor("cloudflare").snapshot(); fails != 1 {
		t.Errorf("返回 429 的上游应记失败一次，实际 %d", fails)
	}

	st.statusByHost["dns.google"] = 429
	hit = nil
	w = postQuery(s, "/cloudflare/query-dns", makeQuery(0x2223))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("全部上游 429 时应返回 503 + Retry-After，实际 %d", w.Code)
	}
}

// TestOtherClientErrorsPassThrough 400 等其它 4xx 是查询本身的问题，换一家结果一样：
// 原样透传、不转移、不计熔断失败
func TestOtherClientErrorsPassThrough(t *testing.T) {
	var hit []string
	st := &recordingTransport{hits: &hit, body: []byte("bad request")}
	st.status = http.StatusBadRequest
	s := newServer(Config{Mappings: defaultMappings, CBThreshold: 1, Failover: []string{"google"}})
	s.client = &http.Client{Transport: st}

	w := postQuery(s, "/cloudflare/query-dns", makeQuery(0x3333))
	if w.Code != http.StatusBadRequest || w.Body.String() != "bad request" || len(hit) != 1 {
		t.Fatalf("400 应原样透传且不转移，实际 code=%d body=%q hits=%v", w.Code, w.Body.String(), hit)
	}
	if healthy, _ := s.healthFor("cloudflare").snapshot(); !healthy {
		t.Error("400 不应计入熔断失败")
	}
}

// TestBlackholedUpstreamFailsOverQuickly 上游 SYN 无应答（黑洞）时，建连超时必须单独生效：
// 否则要等满 15 秒的整体超时才失败，同一请求里的故障转移形同虚设
func TestBlackholedUpstreamFailsOverQuickly(t *testing.T) {
	// 192.0.2.0/24 是 RFC 5737 保留给文档用的网段，公网不可达
	s := newServer(Config{
		Mappings: map[string]Mapping{
			"/blackhole": {TargetDomain: "192.0.2.1:853"},
			"/stub":      {TargetDomain: "stub.invalid"},
		},
		Failover:    []string{"stub"},
		DialTimeout: 300 * time.Millisecond,
	})
	stub := &recordingTransport{body: makeResponse(300)}
	real := s.client.Transport
	s.client.Transport = &roundTripFunc{fn: func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "192.0.2.1:853" {
			return real.RoundTrip(r)
		}
		return stub.RoundTrip(r)
	}}

	start := time.Now()
	w := postQuery(s, "/blackhole/query-dns", makeQuery(0x4444))
	elapsed := time.Since(start)
	if w.Code != http.StatusOK {
		t.Fatalf("黑洞上游之后应由备用上游成功应答，实际 %d", w.Code)
	}
	if elapsed > 3*time.Second {
		t.Errorf("黑洞上游应在建连超时后迅速转移，实际耗时 %v", elapsed)
	}
}

// TestMetricsReportsVersionAndCacheBudget 运维需要知道在跑的是哪个版本、缓存占了多少内存
func TestMetricsReportsVersionAndCacheBudget(t *testing.T) {
	s := testServer(Config{Mappings: defaultMappings, MetricsPath: "/metrics", CacheMaxTTL: 300, CacheEntries: 100},
		&recordingTransport{respond: func(req *http.Request) []byte {
			query, _ := io.ReadAll(req.Body)
			return buildResponse(query, 300)
		}})
	postQuery(s, "/cloudflare/query-dns", makeQuery(0x5555))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/metrics", nil))
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("metrics 不是合法 JSON: %v", err)
	}
	if m["version"] != version {
		t.Errorf("metrics 应带 version=%q，实际 %v", version, m["version"])
	}
	if m["cache_bytes"].(float64) <= 0 || m["cache_max_bytes"] != float64(maxCacheBytes) {
		t.Errorf("metrics 应带 cache_bytes / cache_max_bytes，实际 %v / %v", m["cache_bytes"], m["cache_max_bytes"])
	}
}

// writeTemp 把内容写进临时目录，返回路径
func writeTemp(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestListenMode 只配了证书与私钥其中一个时必须拒绝启动：原先会悄悄按纯 HTTP 跑起来，
// 以为开了 TLS 的部署实际在明文监听
func TestListenMode(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t)
	_, otherKeyPEM := selfSignedCert(t)
	dir := t.TempDir()
	certFile := writeTemp(t, dir, "server.crt", certPEM)
	keyFile := writeTemp(t, dir, "server.key", keyPEM)

	if p, sec, _, err := listenMode(":8080", "", "", ""); err != nil || p != ":8080" || sec != "" {
		t.Errorf("没配证书应为纯 HTTP，实际 plain=%q secure=%q err=%v", p, sec, err)
	}
	if p, sec, cert, err := listenMode(":8080", "", certFile, keyFile); err != nil || p != "" || sec != ":8080" || len(cert.Certificate) != 1 {
		t.Errorf("配了证书且没给 tlsAddr 时 listen 应直接变 HTTPS，实际 plain=%q secure=%q err=%v", p, sec, err)
	}
	if p, sec, _, err := listenMode(":8080", ":8444", certFile, keyFile); err != nil || p != ":8080" || sec != ":8444" {
		t.Errorf("配了证书与 tlsAddr 应双监听，实际 plain=%q secure=%q err=%v", p, sec, err)
	}
	for _, pair := range [][2]string{{certFile, ""}, {"", keyFile}} {
		if _, _, _, err := listenMode(":8080", "", pair[0], pair[1]); err == nil {
			t.Errorf("证书与私钥只给其一时应拒绝启动（%q, %q）", pair[0], pair[1])
		}
	}
	if _, _, _, err := listenMode(":8080", "", certFile, filepath.Join(dir, "missing.key")); err == nil {
		t.Error("私钥文件不存在时应报错")
	}
	if _, _, _, err := listenMode(":8080", "", certFile, writeTemp(t, dir, "other.key", otherKeyPEM)); err == nil {
		t.Error("私钥与证书不匹配时应报错")
	}
}

// TestServerTLS 服务端至少要求 TLS 1.2，并且 listenMode 加载的证书确实被用于握手
func TestServerTLS(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := selfSignedCert(t)
	_, _, cert, err := listenMode(":0", "", writeTemp(t, dir, "server.crt", certPEM), writeTemp(t, dir, "server.key", keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Config{Mappings: defaultMappings, HealthPath: "/healthz"})
	hs := newHTTPServer(srv, "127.0.0.1:0", srv.cfg, true, cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = hs.ServeTLS(ln, "", "") }() // 与 main() 相同：证书来自 TLSConfig
	t.Cleanup(func() { _ = hs.Close() })

	get := func(maxVersion uint16) error {
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
			MinVersion:         tls.VersionTLS11,
			MaxVersion:         maxVersion,
			InsecureSkipVerify: true, //nolint:gosec // 自签证书 + 本地回环的测试
		})
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: doh.example.com\r\nConnection: close\r\n\r\n"); err != nil {
			return err
		}
		body, err := io.ReadAll(conn)
		if err != nil {
			return err
		}
		if !strings.Contains(string(body), "200 OK") || !strings.HasSuffix(string(body), "ok") {
			return errors.New("健康检查应返回 200 ok，实际 " + string(body))
		}
		return nil
	}
	if err := get(tls.VersionTLS12); err != nil {
		t.Errorf("TLS 1.2 应能握手并访问：%v", err)
	}
	if err := get(tls.VersionTLS11); err == nil {
		t.Error("服务端不应接受 TLS 1.1")
	}
}

// selfSignedCert 生成一张自签证书（含 127.0.0.1 的 SAN），返回 PEM 编码的证书与私钥
func selfSignedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "doh.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"doh.example.com"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
