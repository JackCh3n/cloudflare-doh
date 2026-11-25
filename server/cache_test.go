package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeQuery 构造一条最小的 qq.com A 查询报文
func makeQuery(id uint16) []byte {
	q := []byte{2, 'q', 'q', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	b := []byte{byte(id >> 8), byte(id), 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	return append(b, q...)
}

// questionLen 返回查询报文 question 段（QNAME+QTYPE+QCLASS）的字节数
func questionLen(q []byte) int {
	i := 12
	for q[i] != 0 {
		i += 1 + int(q[i])
	}
	return i + 1 - 12 + 4
}

// withOPT 给查询追加一条 EDNS0 OPT（do 为 true 时置 DO 位），并更新 ARCOUNT
func withOPT(query []byte, do bool) []byte {
	// root 名 + TYPE(41) + CLASS(1232) + TTL(4) + RDLENGTH(0)；OPT 的 TTL 字段最高位即 DO
	opt := []byte{0, 0, 41, 0x04, 0xD0, 0, 0, 0, 0, 0, 0}
	if do {
		opt[5] = 0x80
	}
	out := append(append([]byte(nil), query...), opt...)
	out[11] = 1 // ARCOUNT
	return out
}

// buildResponse 按查询构造应答：原样回显事务 ID 与 question，附一条 A 记录（TTL 可控）
func buildResponse(query []byte, ttl uint32) []byte {
	ql := questionLen(query)
	b := make([]byte, 12, 12+ql+16)
	b[0], b[1] = query[0], query[1]
	b[2], b[3] = 0x81, 0x80 // QR|RD|RA
	b[5], b[7] = 1, 1       // QDCOUNT, ANCOUNT
	b = append(b, query[12:12+ql]...)
	return append(b, 0xC0, 0x0C, 0, 1, 0, 1,
		byte(ttl>>24), byte(ttl>>16), byte(ttl>>8), byte(ttl),
		0, 4, 1, 2, 3, 4)
}

// makeResponse 构造 ID=0x3344、带一条 A 记录（TTL 可控）的 qq.com 应答报文
func makeResponse(ttl uint32) []byte {
	return buildResponse(makeQuery(0x3344), ttl)
}

func dnsResponse(body []byte) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/dns-message"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// queryCacheServer 搭一个带缓存的测试服务：上游每次都按请求体构造应答，并统计调用次数
type queryCacheServer struct {
	s     *server
	calls int
	mut   func(query, resp []byte) []byte // 可选：改写返回给客户端的上游应答
}

func newQueryCacheServer(t *testing.T, mutate func(query, resp []byte) []byte) *queryCacheServer {
	t.Helper()
	q := &queryCacheServer{mut: mutate}
	st := &recordingTransport{body: nil}
	st.respond = func(req *http.Request) []byte {
		q.calls++
		var query []byte
		if req.Body != nil { // GET 请求没有请求体，查询在 ?dns= 参数里
			query, _ = io.ReadAll(req.Body)
		}
		if len(query) == 0 {
			query, _ = decodeDNSParam(req.URL.Query().Get("dns"))
		}
		resp := buildResponse(query, 300)
		if q.mut != nil {
			resp = q.mut(query, resp)
		}
		return resp
	}
	q.s = newServer(Config{Mappings: defaultMappings, CacheMaxTTL: 300, CacheEntries: 100})
	q.s.client = &http.Client{Transport: st}
	return q
}

func (q *queryCacheServer) ask(t *testing.T, query []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(query))
	req.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	q.s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("查询返回 %d：%s", w.Code, w.Body.String())
	}
	return w
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

	q, ok := parseDNSQuery(makeQuery(0xBEEF))
	if !ok || q.id != 0xBEEF || len(q.key) == 0 {
		t.Fatalf("parseDNSQuery 失败: id=%x ok=%v", q.id, ok)
	}
	q2, _ := parseDNSQuery(makeQuery(0x1234))
	if string(q.key) != string(q2.key) {
		t.Error("不同事务 ID 的同一查询应产生相同缓存键")
	}
	if got := rewriteID(resp, 0xBEEF); got[0] != 0xBE || got[1] != 0xEF {
		t.Errorf("rewriteID 失败: %x", got[:2])
	}
	// 畸形报文不应 panic
	if _, ok := parseDNSQuery([]byte{1, 2, 3}); ok {
		t.Error("短报文不应解析成功")
	}
	if got := dnsMinTTL([]byte{0x33, 0x44, 0x81}); got != -1 {
		t.Errorf("畸形应答应返回 -1，实际 %d", got)
	}
}

func TestCacheHitServesLocally(t *testing.T) {
	q := newQueryCacheServer(t, nil)

	first := q.ask(t, makeQuery(0x1111))
	if first.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("首次应为 MISS，实际 %q", first.Header().Get("X-Cache"))
	}

	second := q.ask(t, makeQuery(0x2222))

	if q.calls != 1 {
		t.Errorf("第二次应命中缓存，上游调用次数 = %d", q.calls)
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
	st := &countingTransport{inner: &recordingTransport{body: makeResponse(300)}, n: &calls}
	s := newServer(Config{Mappings: defaultMappings, CacheMaxTTL: 0})
	s.client = &http.Client{Transport: st}

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
	q := newQueryCacheServer(t, func(query, _ []byte) []byte { return buildResponse(query, 0) })

	q.ask(t, makeQuery(0x6666))
	q.ask(t, makeQuery(0x6666))
	if q.calls != 2 {
		t.Errorf("TTL=0 的记录不应缓存，上游调用次数 = %d", q.calls)
	}
}

// TestCacheSkipsTruncated 截断（TC=1）的应答不完整，不能进缓存：
// 否则带更大 EDNS 缓冲区的客户端也会一直拿到这份残缺应答
func TestCacheSkipsTruncated(t *testing.T) {
	q := newQueryCacheServer(t, func(_, resp []byte) []byte {
		resp[2] |= 0x02 // TC
		return resp
	})

	q.ask(t, makeQuery(0x7777))
	q.ask(t, makeQuery(0x7777))
	if q.calls != 2 {
		t.Errorf("TC=1 的应答不应缓存，上游调用次数 = %d", q.calls)
	}
}

// TestCacheRejectsForeignResponses 不是「这条查询的完整 DNS 应答」的内容一律不入缓存
func TestCacheRejectsForeignResponses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(query, resp []byte) []byte
	}{
		{"事务 ID 没有回显", func(_, resp []byte) []byte { resp[1] ^= 0xFF; return resp }},
		{"question 与查询不符", func(_, resp []byte) []byte { resp[13], resp[14] = 'w', 'w'; return resp }},
		{"是查询不是应答", func(_, resp []byte) []byte { resp[2] &^= 0x80; return resp }},
		{"末尾记录的数据被截断", func(_, resp []byte) []byte { return resp[:len(resp)-4] }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newQueryCacheServer(t, c.mutate)
			q.ask(t, makeQuery(0x8888))
			q.ask(t, makeQuery(0x8888))
			if q.calls != 2 {
				t.Errorf("该应答不应缓存，上游调用次数 = %d", q.calls)
			}
		})
	}
}

// TestCacheRejectsNonDNSBody 上游用 200 返回的非 DNS 内容（如门户认证页）不能进缓存
func TestCacheRejectsNonDNSBody(t *testing.T) {
	calls := 0
	st := &roundTripFunc{fn: func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader("<html><body>登录后上网</body></html>")),
		}, nil
	}}
	s := newServer(Config{Mappings: defaultMappings, CacheMaxTTL: 300})
	s.client = &http.Client{Transport: st}

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x9999)))
		req.Header.Set("Content-Type", "application/dns-message")
		s.ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "登录后上网") {
			t.Fatalf("上游的非 DNS 内容应原样透传，实际 %d %q", w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Errorf("非 DNS 内容不应缓存，上游调用次数 = %d", calls)
	}
}

// TestCacheKeyIncludesEDNS 缓存键必须带 EDNS 信息：带 DO 位的客户端要的是含 RRSIG/OPT 的应答，
// 不带的客户端拿到的没有，两者不能共用一份缓存
func TestCacheKeyIncludesEDNS(t *testing.T) {
	q := newQueryCacheServer(t, func(query, resp []byte) []byte {
		// 模拟上游：查询带 OPT 才回 OPT，且原样回显 DO 位
		if len(query) > 12+questionLen(query) {
			resp[11] = 1 // ARCOUNT
			resp = append(resp, query[12+questionLen(query):]...)
		}
		return resp
	})

	plain := makeQuery(0x0001)
	edns := withOPT(makeQuery(0x0002), false)
	dnssec := withOPT(makeQuery(0x0003), true)
	// OPT 共 11 字节，DO 是其 TTL 字段（倒数第 6~3 字节）的最高位
	const doOffset = 6

	q.ask(t, plain).Body.Bytes()
	noOPT := q.ask(t, edns).Body.Bytes()
	withDO := q.ask(t, dnssec).Body.Bytes()
	if q.calls != 3 {
		t.Fatalf("三种 EDNS 形态应各打一次上游，实际 %d 次", q.calls)
	}
	if len(noOPT) != len(buildResponse(edns, 300))+11 || noOPT[len(noOPT)-doOffset]&0x80 != 0 {
		t.Errorf("带 OPT 无 DO 的客户端拿到的应答不对：%d 字节，DO 位 %x", len(noOPT), noOPT[len(noOPT)-doOffset])
	}
	if len(withDO) != len(noOPT) || withDO[len(withDO)-doOffset]&0x80 == 0 {
		t.Errorf("带 DO 的客户端拿到的应答里没有 DO 位：%d 字节", len(withDO))
	}

	// 再问一轮：全部命中缓存，且各自拿到的还是自己那一份
	again := q.ask(t, withOPT(makeQuery(0x0004), true)).Body.Bytes()
	q.ask(t, plain)
	if q.calls != 3 {
		t.Fatalf("第二轮应全部命中缓存，上游调用次数 = %d", q.calls)
	}
	if again[0] != 0x00 || again[1] != 0x04 || again[len(again)-doOffset]&0x80 == 0 {
		t.Errorf("带 DO 的客户端命中缓存后拿到的不是自己的应答")
	}
}

func TestParseDNSQueryEDNS(t *testing.T) {
	plain, ok := parseDNSQuery(makeQuery(1))
	if !ok || plain.hasOPT || plain.doBit {
		t.Fatalf("无 OPT 的查询解析异常: ok=%v hasOPT=%v do=%v", ok, plain.hasOPT, plain.doBit)
	}
	edns, _ := parseDNSQuery(withOPT(makeQuery(2), false))
	dnssec, _ := parseDNSQuery(withOPT(makeQuery(3), true))
	if !edns.hasOPT || edns.doBit {
		t.Errorf("带 OPT 无 DO 的查询解析错误: hasOPT=%v do=%v", edns.hasOPT, edns.doBit)
	}
	if !dnssec.hasOPT || !dnssec.doBit {
		t.Errorf("带 OPT 与 DO 的查询解析错误: hasOPT=%v do=%v", dnssec.hasOPT, dnssec.doBit)
	}
	if string(plain.key) == string(edns.key) || string(edns.key) == string(dnssec.key) {
		t.Error("EDNS 形态不同的查询应产生不同的缓存键")
	}
	// OPT 的 TTL 字段里除 DO 以外的位（扩展 RCODE 等）不参与键
	other := withOPT(makeQuery(4), false)
	other[12+questionLen(other)+6] = 0x01
	if o, _ := parseDNSQuery(other); string(o.key) != string(edns.key) {
		t.Error("扩展 RCODE 等位不应影响缓存键")
	}
	// question 之后还有记录但解析不了：不可缓存
	bad := append(withOPT(makeQuery(5), false), 0x40, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	bad[11] = 2
	if _, ok := parseDNSQuery(bad); ok {
		t.Error("question 之后有解析不了的记录时应视为不可缓存")
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
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("上游失败响应应 no-store，实际 %q", w.Header().Get("Cache-Control"))
	}
}

// TestClientDisconnectNotCountedAsUpstreamFailure 客户端先断开时，请求上下文已结束，
// 上游请求必然报错。这不是上游的故障：不能记熔断失败，也不该再把候选链逐家试一遍
func TestClientDisconnectNotCountedAsUpstreamFailure(t *testing.T) {
	attempts := 0
	st := &roundTripFunc{fn: func(r *http.Request) (*http.Response, error) {
		attempts++
		cancel, _ := r.Context().Value(cancelKey{}).(context.CancelFunc)
		cancel() // 客户端在这一刻断开
		return nil, errors.New("context canceled")
	}}
	s := newServer(Config{Mappings: defaultMappings, CBThreshold: 1, Failover: []string{"google", "adguard"}})
	s.client = &http.Client{Transport: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", bytes.NewReader(makeQuery(0x1111))).WithContext(ctx)
	req = req.WithContext(context.WithValue(req.Context(), cancelKey{}, cancel))
	req.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if attempts != 1 {
		t.Errorf("客户端已断开时不应再转移，上游尝试次数 = %d", attempts)
	}
	for _, name := range []string{"cloudflare", "google", "adguard"} {
		if _, fails := s.healthFor(name).snapshot(); fails != 0 {
			t.Errorf("客户端断开不应记为 %s 的失败，实际 fails=%d", name, fails)
		}
	}
	if s.totalErr.Load() != 0 {
		t.Errorf("客户端断开不应计入 upstream_errors，实际 %d", s.totalErr.Load())
	}
}

type cancelKey struct{}

// roundTripFunc 把函数当作 RoundTripper
type roundTripFunc struct {
	fn func(*http.Request) (*http.Response, error)
}

func (f *roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f.fn(r) }

// TestCacheByteBudget 缓存除了条目数还要受总字节上限约束：
// 单条应答最大 64KB，10000 条最坏能撑到约 640MB
func TestCacheByteBudget(t *testing.T) {
	c := newDNSCache(1000, 3000)
	if c == nil {
		t.Fatal("预算为正时应创建缓存")
	}
	body := bytes.Repeat([]byte{1}, 1000)
	for _, key := range []string{"a", "b", "c", "d"} {
		c.put(key, cacheEntry{body: bytes.Clone(body), ttl: 300, fetchedAt: time.Now()})
	}
	// 每条 = 1000 字节应答体 + 1 字节的键
	if c.len() != 2 || c.sizeBytes() != 2*(1000+1) {
		t.Fatalf("应按字节预算淘汰到 2 条共 2002 字节，实际 %d 条 %d 字节", c.len(), c.sizeBytes())
	}
	if _, ok := c.get("a"); ok {
		t.Error("最久未用的 a 应最先被淘汰")
	}
	if _, ok := c.get("d"); !ok {
		t.Error("最新的 d 不应被淘汰")
	}

	// 覆盖同一条目时按新大小重新计账：c 变成 1501，加上 d 的 1001 仍在 3000 的预算内
	c.put("c", cacheEntry{body: bytes.Repeat([]byte{2}, 1500), ttl: 300, fetchedAt: time.Now()})
	if c.len() != 2 || c.sizeBytes() != 1501+1001 {
		t.Errorf("覆盖后应按新大小重新计账，实际 %d 条 %d 字节", c.len(), c.sizeBytes())
	}
	// 单条就超过总预算的应答直接不入缓存，也不为它把别的条目淘汰掉
	small := newDNSCache(10, 1000)
	small.put("b", cacheEntry{body: make([]byte, 100), ttl: 300, fetchedAt: time.Now()})
	small.put("huge", cacheEntry{body: make([]byte, 5000), ttl: 300, fetchedAt: time.Now()})
	if small.len() != 1 || small.sizeBytes() != 101 {
		t.Errorf("超过总预算的条目不应入缓存，实际 %d 条 %d 字节", small.len(), small.sizeBytes())
	}
	if _, ok := small.get("huge"); ok {
		t.Error("超过总预算的条目不应能取到")
	}
	if newDNSCache(10, 0) != nil || newDNSCache(0, 10) != nil {
		t.Error("预算为 0 时应关闭缓存")
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

// recordingTransport 记录命中的上游主机，并可指定某些主机失败；respond 优先于 body，
// statusByHost[host] 或 status 非 0 时覆盖应答状态码（用来模拟 429 / 5xx）
type recordingTransport struct {
	fail         map[string]bool
	hits         *[]string
	body         []byte
	status       int
	statusByHost map[string]int
	respond      func(req *http.Request) []byte
}

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.hits != nil {
		*t.hits = append(*t.hits, r.URL.Host)
	}
	if t.fail[r.URL.Host] {
		return nil, errors.New("模拟上游不可用")
	}
	body := t.body
	if t.respond != nil {
		body = t.respond(r)
	}
	resp := dnsResponse(body)
	if status := t.statusByHost[r.URL.Host]; status != 0 {
		resp.StatusCode = status
	} else if t.status != 0 {
		resp.StatusCode = t.status
	}
	return resp, nil
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

// TestDecodeDNSParamVariants RFC 8484 规定 ?dns= 是不带填充的 base64url，
// 但带填充、以及标准 base64（+ /）的写法也要能解，否则这类客户端永远命中不了缓存
func TestDecodeDNSParamVariants(t *testing.T) {
	// 查询报文 24 字节，正好是 3 的倍数；紧跟着的 FB EF FF 落在 base64 的 3 字节组边界上，
	// 标准 base64 里必然出现 + 和 /
	raw := append(makeQuery(0x1234), 0xFB, 0xEF, 0xFF)
	std := base64.StdEncoding.EncodeToString(raw)
	if !strings.Contains(std, "+//") {
		t.Fatalf("构造的数据应在标准 base64 里产生 +//，实际 %s", std)
	}
	for name, enc := range map[string]*base64.Encoding{
		"RawURL": base64.RawURLEncoding, "URL": base64.URLEncoding, "RawStd": base64.RawStdEncoding, "Std": base64.StdEncoding,
	} {
		if got, ok := decodeDNSParam(enc.EncodeToString(raw)); !ok || !bytes.Equal(got, raw) {
			t.Errorf("%s 编码解码失败 ok=%v", name, ok)
		}
	}
	if _, ok := decodeDNSParam("***"); ok {
		t.Error("非法 base64 不应解码成功")
	}

	q := newQueryCacheServer(t, nil)
	get := func(param string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		q.s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns?dns="+url.QueryEscape(param), nil))
		return w
	}
	if w := get(base64.RawURLEncoding.EncodeToString(raw)); w.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("首次应为 MISS，实际 %q", w.Header().Get("X-Cache"))
	}
	if w := get(std); w.Header().Get("X-Cache") != "HIT" || q.calls != 1 {
		t.Errorf("标准 base64 写法的同一查询应命中缓存，X-Cache=%q 上游调用=%d", w.Header().Get("X-Cache"), q.calls)
	}
}

// TestCamoBodyMatchesIndexHTML Go 服务实际返回的伪装页必须与仓库根目录的 index.html 一致
// （换行按 LF 归一化：Windows 检出是 CRLF，而 Go 原生字符串会丢弃 CR）
func TestCamoBodyMatchesIndexHTML(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "index.html"))
	if err != nil {
		t.Fatalf("读取 index.html: %v", err)
	}
	want := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if got := strings.TrimSpace(homepageHTML); got != want {
		t.Errorf("main.go 里的伪装页与 index.html 不一致（改了页面请执行 npm run camo:sync）")
	}
}
