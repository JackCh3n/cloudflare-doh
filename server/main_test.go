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
	resp       *http.Response
	err        error
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.lastURL = r.URL.String()
	s.lastMethod = r.Method
	s.lastAccept = r.Header.Get("Accept")
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
	return &server{cfg: cfg, client: &http.Client{Transport: st}}
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
		got, ok := cfg.resolveTarget(c.path, c.query)
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
	got, _ := cfg.resolveTarget("/google-v2/query", "")
	if got != "https://v2.example.com/query" {
		t.Errorf("最长前缀匹配失败: %s", got)
	}
}

func TestDefaultUpstreamOverride(t *testing.T) {
	cfg := Config{Mappings: defaultMappings, DefaultUpstream: "google"}
	got, _ := cfg.resolveTarget("/dns-query", "dns=abc")
	if got != "https://dns.google/dns-query?dns=abc" {
		t.Errorf("DEFAULT_UPSTREAM 未生效: %s", got)
	}
	cfg.DefaultUpstream = "nope"
	if _, ok := cfg.resolveTarget("/dns-query", ""); ok {
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
