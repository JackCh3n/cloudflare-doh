package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubTransport 记录发往上游的请求并返回固定响应
type stubTransport struct {
	lastURL    string
	lastMethod string
	lastBody   string
	lastAccept string
	lastLength int64
	resp       *http.Response
	err        error
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.lastURL = r.URL.String()
	s.lastMethod = r.Method
	s.lastAccept = r.Header.Get("Accept")
	s.lastLength = r.ContentLength
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		s.lastBody = string(b)
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.resp != nil {
		return s.resp, nil
	}
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/dns-message"}},
		Body:       io.NopCloser(strings.NewReader("dns-binary")),
	}, nil
}

func testServer(cfg Config, st *stubTransport) *server {
	s := newServer(cfg)
	s.client = &http.Client{Transport: st}
	return s
}

func TestResolveTarget(t *testing.T) {
	cfg := Config{Mappings: defaultMappings, DefaultUpstream: "cloudflare"}
	cases := []struct {
		path, query, want string
		ok                bool
	}{
		{"/google/query-dns", "name=example.com&type=A", "https://dns.google/dns-query?name=example.com&type=A", true},
		{"/cloudflare/query-dns", "", "https://one.one.one.one/dns-query", true},
		{"/quad9/query-dns", "name=a.com", "https://dns.quad9.net/dns-query?name=a.com", true},
		{"/adguard/query-dns", "", "https://dns.adguard-dns.com/dns-query", true},
		{"/dnspod/dns-query", "name=a.com", "https://doh.pub/dns-query?name=a.com", true},
		{"/alidns/query-dns", "", "https://dns.alidns.com/dns-query", true},
		{"/google", "name=a.com", "https://dns.google/?name=a.com", true},
		{"/dns-query", "dns=abc", "https://one.one.one.one/dns-query?dns=abc", true},
		{"/query-dns", "", "https://one.one.one.one/dns-query", true},
		{"/", "", "", false},
		{"/unknown/path", "", "", false},
		{"/index.html", "", "", false},
	}
	for _, c := range cases {
		_, got, ok := cfg.resolveTarget(c.path, c.query)
		if ok != c.ok || got != c.want {
			t.Errorf("resolveTarget(%q) = (%q,%v)，期望 (%q,%v)", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestLongestPrefixWins(t *testing.T) {
	cfg := Config{Mappings: map[string]Mapping{
		"/google":    {TargetDomain: "dns.google"},
		"/google-v2": {TargetDomain: "v2.example.com"},
	}}
	_, got, _ := cfg.resolveTarget("/google-v2/query", "")
	if got != "https://v2.example.com/query" {
		t.Errorf("最长前缀匹配失败: %s", got)
	}
}

func TestDefaultUpstreamOverride(t *testing.T) {
	cfg := Config{Mappings: defaultMappings, DefaultUpstream: "google"}
	_, got, _ := cfg.resolveTarget("/dns-query", "dns=abc")
	if got != "https://dns.google/dns-query?dns=abc" {
		t.Errorf("DEFAULT_UPSTREAM 未生效: %s", got)
	}
	cfg.DefaultUpstream = "nope"
	if _, _, ok := cfg.resolveTarget("/dns-query", ""); ok {
		t.Error("不存在的上游应当不转发")
	}
}

func TestHostAllowList(t *testing.T) {
	cfg := Config{Mappings: defaultMappings, AllowedHosts: []string{"doh.example.com"}}
	if !cfg.hostAllowed("doh.example.com") || !cfg.hostAllowed("DOH.Example.com:443") {
		t.Error("白名单内域名应放行")
	}
	if cfg.hostAllowed("evil.pages.dev") {
		t.Error("白名单外域名应拒绝")
	}
	cfg.AllowedHosts = nil
	if !cfg.hostAllowed("anything.workers.dev") {
		t.Error("未配置白名单时不应限制")
	}
}

func TestCamoForUnknownPath(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings, DefaultUpstream: "cloudflare"}, st)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/nope", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control=%q，期望 no-store", cc)
	}
	if !strings.Contains(w.Body.String(), "无法访问此网站") {
		t.Error("响应体不是伪装 404 页面")
	}
	if st.lastURL != "" {
		t.Error("未命中映射时不应发起上游请求")
	}
}

func TestHostRejected(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings, AllowedHosts: []string{"doh.example.com"}}, st)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://evil.pages.dev/google/query-dns", nil))

	if w.Code != http.StatusNotFound || st.lastURL != "" {
		t.Fatalf("白名单外应返回 404 且不转发，实际 code=%d url=%q", w.Code, st.lastURL)
	}
}

func TestPostBodyForwarded(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings, CacheTTL: 30}, st)
	body := "\x33\x44dns-message"
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if st.lastMethod != http.MethodPost || st.lastBody != body {
		t.Errorf("POST 未透传：method=%s body=%q", st.lastMethod, st.lastBody)
	}
	if w.Code != 200 || w.Body.String() != "dns-binary" {
		t.Errorf("响应异常：code=%d body=%q", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("POST 应为 no-store，实际 %q", cc)
	}
}

func TestGetCacheable(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings, CacheTTL: 30}, st)
	req := httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns?dns=abc", nil)
	req.Header.Set("Accept", "application/dns-message")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=30" {
		t.Errorf("GET+DoH 应为短缓存，实际 %q", cc)
	}
	if vary := w.Header().Get("Vary"); !strings.Contains(vary, "Accept") {
		t.Errorf("缺少 Vary: Accept，实际 %q", vary)
	}
	if st.lastURL != "https://one.one.one.one/dns-query?dns=abc" {
		t.Errorf("转发地址错误：%s", st.lastURL)
	}
}

func TestRootDohFallback(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings, DefaultUpstream: "cloudflare"}, st)

	// DoH 请求：转发到默认上游
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/", strings.NewReader("\x33\x44"))
	req.Header.Set("Content-Type", "application/dns-message")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 200 || st.lastURL != "https://one.one.one.one/dns-query" {
		t.Errorf("根路径 DoH 未转发：code=%d url=%q", w.Code, st.lastURL)
	}

	// 浏览器请求：伪装 404
	st2 := &stubTransport{}
	s2 := testServer(Config{Mappings: defaultMappings, DefaultUpstream: "cloudflare"}, st2)
	req2 := httptest.NewRequest(http.MethodGet, "https://doh.example.com/", nil)
	req2.Header.Set("Accept", "text/html")
	w2 := httptest.NewRecorder()
	s2.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound || st2.lastURL != "" {
		t.Errorf("浏览器访问根路径应为伪装 404，实际 code=%d url=%q", w2.Code, st2.lastURL)
	}
}

func TestHealthPath(t *testing.T) {
	s := testServer(Config{Mappings: defaultMappings, HealthPath: "/healthz"}, &stubTransport{})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/healthz", nil))
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("健康检查异常：code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestInFlightLimit(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings, MaxInFlight: 1}, st)

	// 先占满唯一槽位
	s.inFlight <- struct{}{}
	s.inFlightNum.Add(1)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("槽位占满时应返回 503，实际 %d", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("503 应带 Retry-After: 1，实际 %q", ra)
	}

	// 释放后恢复
	<-s.inFlight
	s.inFlightNum.Add(-1)
	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns", nil))
	if w2.Code != http.StatusOK {
		t.Errorf("释放槽位后应恢复 200，实际 %d", w2.Code)
	}
	if s.inFlightNum.Load() != 0 {
		t.Errorf("在途计数应归零，实际 %d", s.inFlightNum.Load())
	}
}

func TestContentLengthAlwaysSet(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings}, st)
	body := "\x33\x44\x01\x00"
	req := httptest.NewRequest(http.MethodPost, "https://doh.example.com/cloudflare/query-dns", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/dns-message")
	req.ContentLength = -1 // 模拟 chunked 进入
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if st.lastLength != int64(len(body)) {
		t.Errorf("上游 ContentLength 应为 %d，实际 %d（chunked 不应被透传）", len(body), st.lastLength)
	}
	if st.lastBody != body {
		t.Errorf("请求体未透传: %q", st.lastBody)
	}
}

func TestGlobalRateLimit(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings, MaxRPS: 1}, st)

	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns", nil))
		if w.Code != want {
			t.Errorf("第 %d 个请求状态码 %d，期望 %d", i+1, w.Code, want)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings, MetricsPath: "/internal-metrics"}, st)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/cloudflare/query-dns", nil))
	if w.Code != 200 {
		t.Fatalf("预热请求失败: %d", w.Code)
	}

	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "https://doh.example.com/internal-metrics", nil))
	if w2.Code != 200 {
		t.Fatalf("metrics 状态码 %d", w2.Code)
	}
	body := w2.Body.String()
	for _, k := range []string{"total_requests", "in_flight", "uptime_seconds"} {
		if !strings.Contains(body, k) {
			t.Errorf("metrics 缺少字段 %s: %s", k, body)
		}
	}
}

func TestNoMetricsByDefault(t *testing.T) {
	st := &stubTransport{}
	s := testServer(Config{Mappings: defaultMappings}, st)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://doh.example.com/metrics", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("未配置 METRICS_PATH 时应走伪装 404，实际 %d", w.Code)
	}
}
