package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/storage"
)

func TestParseRedirects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []redirectRule
	}{
		{"empty", "", nil},
		{"blank and comments", "\n  \n# full line\n   # indented\n", nil},
		{"default status 301", "/a /b", []redirectRule{{from: "/a", to: "/b", status: 301}}},
		{"trailing comment", "/a /b 302 # legacy page", []redirectRule{{from: "/a", to: "/b", status: 302}}},
		{"all redirect statuses", "/a /b 301\n/c /d 302\n/e /f 307\n/g /h 308", []redirectRule{
			{from: "/a", to: "/b", status: 301},
			{from: "/c", to: "/d", status: 302},
			{from: "/e", to: "/f", status: 307},
			{from: "/g", to: "/h", status: 308},
		}},
		{"local rewrite", "/app/* /index.html 200", []redirectRule{{from: "/app", wildcard: true, to: "/index.html", status: 200}}},
		{"spa catch-all", "/* /index.html 200", []redirectRule{{from: "", wildcard: true, to: "/index.html", status: 200}}},
		{"external redirect", "/x https://example.com/y 302", []redirectRule{{from: "/x", to: "https://example.com/y", status: 302}}},
		{"invalid status skipped", "/a /b 418", nil},
		{"force syntax skipped", "/a /b 301!", nil},
		{"mid-path wildcard skipped", "/a/*/b /c 301", nil},
		{"param placeholder skipped", "/a/:id /b 301", nil},
		{"splat without wildcard skipped", "/a /b/:splat 301", nil},
		{"relative from skipped", "a /b 301", nil},
		{"too many fields skipped", "/a /b 301 Country=it", nil},
		{"one field skipped", "/a", nil},
		{"http proxy target skipped", "/api/* http://api.example.com/:splat 200", nil},
		{"hostless proxy target skipped", "/api/* https:// 200", nil},
		{"ftp redirect target skipped", "/a ftp://example.com 301", nil},
		{"bare word target skipped", "/a b 301", nil},
		{"valid lines survive invalid ones", "/a /b 999\n/c /d 302\nnot a rule", []redirectRule{{from: "/c", to: "/d", status: 302}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := parseRedirects([]byte(tt.in))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d rules %+v, want %d", len(got), got, len(tt.want))
			}
			for i := range got {
				got[i].proxyURL = nil // pointer field; proxy rules are checked structurally below
				if got[i] != tt.want[i] {
					t.Errorf("rule %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseRedirectsProxyRule(t *testing.T) {
	rules, skipped := parseRedirects([]byte("/api/* https://api.example.com/v1/:splat?key=1 200"))
	if skipped != 0 || len(rules) != 1 {
		t.Fatalf("rules=%v skipped=%d", rules, skipped)
	}
	r := rules[0]
	if !r.proxy || r.proxyURL == nil || r.proxyURL.Host != "api.example.com" || r.proxyURL.Scheme != "https" {
		t.Fatalf("proxy rule = %+v", r)
	}
	if !r.wildcard || r.from != "/api" {
		t.Fatalf("from = %+v", r)
	}
}

func TestParseRedirectsSkippedCount(t *testing.T) {
	_, skipped := parseRedirects([]byte("/a /b 999\nbogus\n/c /d"))
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
}

func TestParseRedirectsRuleCap(t *testing.T) {
	old := maxRedirectRules
	maxRedirectRules = 3
	t.Cleanup(func() { maxRedirectRules = old })
	in := strings.Repeat("/a /b 301\n", 10)
	rules, _ := parseRedirects([]byte(in))
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}
}

func TestMatchRules(t *testing.T) {
	rules, _ := parseRedirects([]byte(strings.Join([]string{
		"/exact /target 301",
		"/exact /never 302", // first match wins
		"/api/* https://api.example.com/:splat 200",
		"/* /index.html 200",
	}, "\n")))

	tests := []struct {
		p         string
		wantTo    string
		wantSplat string
		wantOK    bool
	}{
		{"/exact", "/target", "", true},
		{"/api", "https://api.example.com/:splat", "", true},
		{"/api/", "https://api.example.com/:splat", "", true},
		{"/api/x/y", "https://api.example.com/:splat", "x/y", true},
		{"/api-x", "/index.html", "api-x", true}, // sibling: not the /api/* rule, falls to /*
		{"/anything/else", "/index.html", "anything/else", true},
		{"/", "/index.html", "", true},
	}
	for _, tt := range tests {
		m, ok := matchRules(rules, tt.p)
		if ok != tt.wantOK {
			t.Errorf("match(%q) ok = %v, want %v", tt.p, ok, tt.wantOK)
			continue
		}
		if m.rule.to != tt.wantTo || m.splat != tt.wantSplat {
			t.Errorf("match(%q) = to %q splat %q, want to %q splat %q", tt.p, m.rule.to, m.splat, tt.wantTo, tt.wantSplat)
		}
	}

	noCatchAll, _ := parseRedirects([]byte("/api/* /x 200"))
	if _, ok := matchRules(noCatchAll, "/api-x"); ok {
		t.Error("/api-x must not match /api/*")
	}
	if _, ok := matchRules(noCatchAll, "/other"); ok {
		t.Error("/other must not match")
	}
}

func TestTargetPath(t *testing.T) {
	rules, _ := parseRedirects([]byte("/docs/* /pages/:splat 200\n/old/* /new/:splat 301"))

	m, _ := matchRules(rules, "/docs/a/b")
	if got := m.targetPath(); got != "/pages/a/b" {
		t.Errorf("rewrite target = %q", got)
	}
	// A traversal splat can't climb out on local rewrites (cleaned).
	m, _ = matchRules(rules, "/docs/../../etc/passwd")
	if got := m.targetPath(); got != "/etc/passwd" {
		t.Errorf("cleaned rewrite target = %q", got)
	}
	// Redirect targets are echoed, not cleaned (they're Locations, not lookups).
	m, _ = matchRules(rules, "/old/x")
	if got := m.targetPath(); got != "/new/x" {
		t.Errorf("redirect target = %q", got)
	}
}

func TestProxyTarget(t *testing.T) {
	rules, _ := parseRedirects([]byte("/api/* https://api.example.com/v1/:splat?key=1 200"))
	m, ok := matchRules(rules, "/api/users/7")
	if !ok {
		t.Fatal("no match")
	}
	u := m.proxyTarget("page=2")
	if u.String() != "https://api.example.com/v1/users/7?key=1&page=2" {
		t.Errorf("proxy target = %q", u)
	}
	// The original rule URL must not be mutated by a match.
	if m.rule.proxyURL.Path != "/v1/:splat" || m.rule.proxyURL.RawQuery != "key=1" {
		t.Errorf("rule URL mutated: %q", m.rule.proxyURL)
	}
	// No inbound query: no trailing separator.
	if got := m.proxyTarget("").String(); got != "https://api.example.com/v1/users/7?key=1" {
		t.Errorf("proxy target no query = %q", got)
	}
}

func TestServeRedirectRules(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"index.html":    "<h1>home</h1>",
		"real.html":     "<h1>real</h1>",
		"pages/doc.txt": "doc",
		"_redirects": strings.Join([]string{
			"/moved /real 301",
			"/away https://example.com/x 302",
			"/real /elsewhere 302  # shadowed by real.html",
			"/docs/* /pages/:splat 200",
			"/ghost /nowhere.html 200",
		}, "\n"),
	})

	if w := get(t, s, "demo", "/moved?a=1"); w.Code != 301 || w.Header().Get("Location") != "/real?a=1" {
		t.Errorf("/moved: code %d location %q, want 301 /real?a=1", w.Code, w.Header().Get("Location"))
	}
	if w := get(t, s, "demo", "/away"); w.Code != 302 || w.Header().Get("Location") != "https://example.com/x" {
		t.Errorf("/away: code %d location %q", w.Code, w.Header().Get("Location"))
	}
	// Shadowing: /real resolves to real.html via clean URLs, so the rule never fires.
	if w := get(t, s, "demo", "/real"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "real") {
		t.Errorf("/real: code %d body %q, want the file", w.Code, w.Body.String())
	}
	// Local rewrite with splat.
	if w := get(t, s, "demo", "/docs/doc.txt"); w.Code != http.StatusOK || w.Body.String() != "doc" {
		t.Errorf("/docs/doc.txt: code %d body %q, want 200 doc", w.Code, w.Body.String())
	}
	// Broken rewrite target: falls through to 404, not an error.
	if w := get(t, s, "demo", "/ghost"); w.Code != http.StatusNotFound {
		t.Errorf("/ghost: code %d, want 404", w.Code)
	}
	// _redirects itself is hidden.
	if w := get(t, s, "demo", "/_redirects"); w.Code != http.StatusNotFound {
		t.Errorf("/_redirects: code %d, want 404", w.Code)
	}
	if w := get(t, s, "demo", "/nope"); w.Code != http.StatusNotFound {
		t.Errorf("/nope: code %d, want 404", w.Code)
	}
}

func TestServeRulesBeat200HTML(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"index.html": "shell-index",
		"200.html":   "shell-200",
		"_redirects": "/app/* /index.html 200",
	})

	if w := get(t, s, "demo", "/app/deep/route"); w.Code != http.StatusOK || w.Body.String() != "shell-index" {
		t.Errorf("/app/deep/route: code %d body %q, want the rule's shell", w.Code, w.Body.String())
	}
	// Outside the rule, 200.html still applies.
	if w := get(t, s, "demo", "/other"); w.Code != http.StatusOK || w.Body.String() != "shell-200" {
		t.Errorf("/other: code %d body %q, want 200.html", w.Code, w.Body.String())
	}
}

func TestServeOversizeRedirectsIgnored(t *testing.T) {
	old := maxRedirectsBytes
	maxRedirectsBytes = 16
	t.Cleanup(func() { maxRedirectsBytes = old })

	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"_redirects": "/moved /real 301\n# padding padding padding",
	})
	if w := get(t, s, "demo", "/moved"); w.Code != http.StatusNotFound {
		t.Errorf("/moved: code %d, want 404 (oversize _redirects ignored)", w.Code)
	}
}

// countingBackend counts OpenFile probes for a single path.
type countingBackend struct {
	storage.Backend
	path  string
	count int
}

func (c *countingBackend) OpenFile(site, p string) (io.ReadSeekCloser, storage.FileInfo, error) {
	if p == c.path {
		c.count++
	}
	return c.Backend.OpenFile(site, p)
}

func TestRulesStoreCaching(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{"index.html": "home"})

	cb := &countingBackend{Backend: s.store, path: "/_redirects"}
	s.rules = newRulesStore(cb, time.Hour)

	for range 5 {
		get(t, s, "demo", "/whatever")
	}
	if cb.count != 1 {
		t.Errorf("OpenFile(_redirects) probes = %d, want 1 (negative result cached)", cb.count)
	}

	// A new deploy + forget picks the rules up immediately.
	putSite(t, s.store, "demo", map[string]string{
		"index.html": "home",
		"_redirects": "/whatever / 302",
	})
	s.rules.forget("demo")
	if w := get(t, s, "demo", "/whatever"); w.Code != 302 {
		t.Errorf("after forget: code %d, want 302", w.Code)
	}
	if cb.count != 2 {
		t.Errorf("OpenFile(_redirects) probes = %d, want 2 after forget", cb.count)
	}
}

func TestAppendQuery(t *testing.T) {
	tests := []struct{ loc, q, want string }{
		{"/a", "", "/a"},
		{"/a", "x=1", "/a?x=1"},
		{"/a?y=2", "x=1", "/a?y=2&x=1"},
		{"https://e.com/a?y=2", "x=1", "https://e.com/a?y=2&x=1"},
	}
	for _, tt := range tests {
		if got := appendQuery(tt.loc, tt.q); got != tt.want {
			t.Errorf("appendQuery(%q, %q) = %q, want %q", tt.loc, tt.q, got, tt.want)
		}
	}
}
