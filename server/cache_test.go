package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// makeQuery 构造一条最小的 qq.com A 查询报文
func makeQuery(id uint16) []byte {
	q := []byte{2, 'q', 'q', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	b := []byte{byte(id >> 8), byte(id), 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	return append(b, q...)
}

// makeResponse 构造带一条 A 记录（TTL 可控）的应答报文
func makeResponse(ttl uint32) []byte {
	q := []byte{2, 'q', 'q', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	b := []byte{0x33, 0x44, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0}
	b = append(b, q...)
	rr := []byte{0xC0, 0x0C, 0, 1, 0, 1,
		byte(ttl >> 24), byte(ttl >> 16), byte(ttl >> 8), byte(ttl),
		0, 4, 1, 2, 3, 4}
	return append(b, rr...)
}

func TestDNSMessageHelpers(t *testing.T) {
	resp := makeResponse(300)

	if got := dnsMinTTL(resp); got != 300 {
		t.Fatalf("dnsMinTTL = %d，期望 300", got)
	}
	out := decrementTTLs(resp, 100)
	if got := dnsMinTTL(out); got != 200 {
		t.Errorf("递减后 TTL = %d，期望 200", got)
	}
	if got := dnsMinTTL(resp); got != 300 {
		t.Errorf("原报文被就地修改，TTL = %d", got)
	}
	if got := dnsMinTTL(decrementTTLs(resp, 999)); got != 0 {
		t.Errorf("超额递减应归零，实际 %d", got)
	}

	id, key, ok := parseDNSQuery(makeQuery(0xBEEF))
	if !ok || id != 0xBEEF || len(key) == 0 {
		t.Fatalf("parseDNSQuery 失败: id=%x ok=%v", id, ok)
	}
	_, key2, _ := parseDNSQuery(makeQuery(0x1234))
	if string(key) != string(key2) {
		t.Error("不同事务 ID 的同一查询应产生相同缓存键")
	}
	if got := rewriteID(resp, 0xBEEF); got[0] != 0xBE || got[1] != 0xEF {
		t.Errorf("rewriteID 失败: %x", got[:2])
	}
	// 畸形报文不应 panic
	if _, _, ok := parseDNSQuery([]byte{1, 2, 3}); ok {
		t.Error("短报文不应解析成功")
	}
	if got := dnsMinTTL([]byte{0x33, 0x44, 0x81}); got != -1 {
		t.Errorf("畸形应答应返回 -1，实际 %d", got)
	}
}

func TestCacheHitServesLocally(t *testing.T) {
	calls := 0
	st := &stubTransport{resp: &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/dns-message"}},
		Body:       io.NopCloser(bytes.NewReader(makeResponse(300))),
	}}
	s := newServer(Config{Mappings: defaultMappings, CacheMaxTTL: 300, CacheEntries: 100})
	s.client = &http.Client{Transport: &countingTransport{inner: st, n: &calls}}

	first := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x1111)))
	req1.Header.Set("Content-Type", "application/dns-message")
	s.ServeHTTP(first, req1)
	if first.Code != 200 || first.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("首次应为 MISS/200，实际 %d/%s", first.Code, first.Header().Get("X-Cache"))
	}

	second := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x2222)))
	req2.Header.Set("Content-Type", "application/dns-message")
	s.ServeHTTP(second, req2)

	if calls != 1 {
		t.Errorf("第二次应命中缓存，上游调用次数 = %d", calls)
	}
	if second.Header().Get("X-Cache") != "HIT" {
		t.Errorf("第二次 X-Cache = %q", second.Header().Get("X-Cache"))
	}
	body := second.Body.Bytes()
	if len(body) < 2 || body[0] != 0x22 || body[1] != 0x22 {
		t.Errorf("缓存命中未按客户端 ID 改写: %x", body[:2])
	}
	if got := dnsMinTTL(body); got <= 0 || got > 300 {
		t.Errorf("缓存命中 TTL 异常: %d", got)
	}
	if second.Header().Get("Content-Type") != "application/dns-message" {
		t.Errorf("缓存命中 Content-Type = %q", second.Header().Get("Content-Type"))
	}
}

func TestCacheDisabled(t *testing.T) {
	calls := 0
	st := &stubTransport{resp: &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/dns-message"}},
		Body:       io.NopCloser(bytes.NewReader(makeResponse(300))),
	}}
	s := newServer(Config{Mappings: defaultMappings, CacheMaxTTL: 0})
	s.client = &http.Client{Transport: &countingTransport{inner: st, n: &calls}}

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x3333)))
		req.Header.Set("Content-Type", "application/dns-message")
		s.ServeHTTP(w, req)
	}
	if calls != 2 {
		t.Errorf("缓存关闭时上游应被调用 2 次，实际 %d", calls)
	}
}

func TestCacheSkipsZeroTTL(t *testing.T) {
	calls := 0
	st := &stubTransport{resp: &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/dns-message"}},
		Body:       io.NopCloser(bytes.NewReader(makeResponse(0))),
	}}
	s := newServer(Config{Mappings: defaultMappings, CacheMaxTTL: 300})
	s.client = &http.Client{Transport: &countingTransport{inner: st, n: &calls}}

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x6666)))
		req.Header.Set("Content-Type", "application/dns-message")
		s.ServeHTTP(w, req)
	}
	if calls != 2 {
		t.Errorf("TTL=0 的记录不应缓存，上游调用次数 = %d", calls)
	}
}

func TestFailoverWhenProviderFails(t *testing.T) {
	var hit []string
	st := &recordingTransport{
		fail: map[string]bool{"one.one.one.one": true},
		hits: &hit,
		body: makeResponse(300),
	}
	s := newServer(Config{
		Mappings:    defaultMappings,
		CBThreshold: 1,
		CBCooldown:  time.Minute,
		Failover:    []string{"cloudflare", "google"},
	})
	s.client = &http.Client{Transport: st}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x4444)))
	req.Header.Set("Content-Type", "application/dns-message")
	s.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("应转移到 google 成功，实际 %d", w.Code)
	}
	if len(hit) != 2 || hit[0] != "one.one.one.one" || hit[1] != "dns.google" {
		t.Errorf("转移顺序异常: %v", hit)
	}
	if healthy, fails := s.healthFor("cloudflare").snapshot(); healthy || fails < 1 {
		t.Errorf("cloudflare 应被熔断，healthy=%v fails=%d", healthy, fails)
	}

	hit = nil
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x5555)))
	req2.Header.Set("Content-Type", "application/dns-message")
	s.ServeHTTP(w2, req2)
	if len(hit) != 1 || hit[0] != "dns.google" {
		t.Errorf("熔断期间应只打 google，实际 %v", hit)
	}
}

func TestAllUpstreamsDown(t *testing.T) {
	var hit []string
	st := &recordingTransport{
		fail: map[string]bool{"one.one.one.one": true, "dns.google": true, "dns.adguard-dns.com": true},
		hits: &hit,
		body: makeResponse(300),
	}
	s := newServer(Config{Mappings: defaultMappings, CBThreshold: 2, CBCooldown: time.Minute})
	s.client = &http.Client{Transport: st}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x7777)))
	req.Header.Set("Content-Type", "application/dns-message")
	s.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway && w.Code != http.StatusServiceUnavailable {
		t.Errorf("全部上游不可用时应返回 502/503，实际 %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("上游失败响应应带 Retry-After")
	}
}

// countingTransport 统计上游调用次数
type countingTransport struct {
	inner http.RoundTripper
	n     *int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*c.n++
	return c.inner.RoundTrip(r)
}

// recordingTransport 记录命中的上游主机，并可指定某些主机失败
type recordingTransport struct {
	fail map[string]bool
	hits *[]string
	body []byte
}

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.hits = append(*t.hits, r.URL.Host)
	if t.fail[r.URL.Host] {
		return nil, errors.New("模拟上游不可用")
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/dns-message"}},
		Body:       io.NopCloser(bytes.NewReader(t.body)),
	}, nil
}

// TestOPTRecordIgnored 带 EDNS0 OPT（TTL 字段为 0）的应答仍应可缓存
func TestOPTRecordIgnored(t *testing.T) {
	base := makeResponse(300)
	// 追加一条 OPT 记录到 additional 段：name=0, type=41, class=1232, ttl=0, rdlen=0
	opt := []byte{0, 0, 41, 0x04, 0xD0, 0, 0, 0, 0, 0, 0}
	resp := append(append([]byte(nil), base...), opt...)
	resp[11] = 1 // ARCOUNT = 1

	if got := dnsMinTTL(resp); got != 300 {
		t.Fatalf("OPT 不应参与 TTL 计算，得到 %d，期望 300", got)
	}
	out := decrementTTLs(resp, 100)
	if got := dnsMinTTL(out); got != 200 {
		t.Errorf("递减后 TTL = %d，期望 200", got)
	}
	// OPT 的 TTL 字段必须保持 0
	if out[len(base)+5] != 0 || out[len(base)+6] != 0 {
		t.Errorf("OPT 的 TTL 字段被改写: %x", out[len(base)+4:len(base)+8])
	}
}
