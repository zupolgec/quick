package main

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestParseHeaders(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []headerRule
	}{
		{"empty", "", nil},
		{"catch-all", "/*\n  X-Frame-Options: DENY", []headerRule{
			{from: "", wildcard: true, headers: []headerField{{"X-Frame-Options", "DENY"}}},
		}},
		{"several paths and headers", "# fonts are immutable\n/fonts/*\n  Cache-Control: public, max-age=31536000\n  Access-Control-Allow-Origin: *\n\n/about\n  X-Custom: yes\r\n", []headerRule{
			{from: "/fonts", wildcard: true, headers: []headerField{{"Cache-Control", "public, max-age=31536000"}, {"Access-Control-Allow-Origin", "*"}}},
			{from: "/about", headers: []headerField{{"X-Custom", "yes"}}},
		}},
		{"name canonicalized, value keeps colons and hashes", "/a\n  content-security-policy: default-src 'self' https://x.test#y", []headerRule{
			{from: "/a", headers: []headerField{{"Content-Security-Policy", "default-src 'self' https://x.test#y"}}},
		}},
		{"robots tag allowed", "/*\n  X-Robots-Tag: noarchive", []headerRule{
			{from: "", wildcard: true, headers: []headerField{{"X-Robots-Tag", "noarchive"}}},
		}},
		{"forbidden headers skipped", "/*\n  Set-Cookie: a=b\n  content-type: text/html\n  X-Content-Type-Options: none\n  Strict-Transport-Security: max-age=1\n  X-Quick-Proxy: 0\n  X-Forwarded-Host: evil.test\n  Access-Control-Allow-Credentials: true\n  WWW-Authenticate: Basic\n  Proxy-Authenticate: Basic\n  Location: /x\n  X-Ok: 1", []headerRule{
			{from: "", wildcard: true, headers: []headerField{{"X-Ok", "1"}}},
		}},
		{"header before any path skipped", "X-Early: 1\n/a\n  X-Ok: 1", []headerRule{
			{from: "/a", headers: []headerField{{"X-Ok", "1"}}},
		}},
		{"invalid path drops its headers", "/a/*/b\n  X-Lost: 1\n/c\n  X-Ok: 1", []headerRule{
			{from: "/c", headers: []headerField{{"X-Ok", "1"}}},
		}},
		{"malformed header lines skipped", "/a\n  no colon\n  Bad Name: 1\n  X-Empty:\n  X-Ok: 1", []headerRule{
			{from: "/a", headers: []headerField{{"X-Ok", "1"}}},
		}},
		{"path without headers dropped", "/a\n/b\n  X-Ok: 1", []headerRule{
			{from: "/b", headers: []headerField{{"X-Ok", "1"}}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := parseHeaders([]byte(tt.in))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseHeadersSkippedCount(t *testing.T) {
	_, skipped := parseHeaders([]byte("X-Early: 1\n/a\n  Set-Cookie: a=b\n  X-Ok: 1\n  junk"))
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
}

func TestServeHeadersRules(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"index.html":      "home",
		"notes.txt":       "plain",
		"fonts/a.woff2":   "font",
		"404.html":        "missing",
		"_headers":        "/*\n  X-Frame-Options: DENY\n  Cache-Control: no-cache\n/fonts/*\n  Cache-Control: public, max-age=31536000\n  Access-Control-Allow-Origin: *\n/notes.txt\n  Content-Type: text/html\n  X-Content-Type-Options: none",
		"_redirects":      "/old /fonts/a.woff2 301",
		"docs/index.html": "docs",
	})

	w := get(t, s, "demo", "/")
	if w.Code != http.StatusOK || w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("/: code %d headers %v", w.Code, w.Header())
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("/: fonts rule leaked: %v", w.Header())
	}

	// All matching rules apply; the same header from several rules is joined.
	w = get(t, s, "demo", "/fonts/a.woff2")
	if got := w.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-cache, public, max-age=31536000" {
		t.Errorf("/fonts/a.woff2 Cache-Control = %q", got)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("/fonts/a.woff2 headers %v", w.Header())
	}

	// Forbidden headers never override quick's own.
	w = get(t, s, "demo", "/notes.txt")
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("/notes.txt Content-Type = %q", ct)
	}
	if v := w.Header().Values("X-Content-Type-Options"); len(v) != 1 || v[0] != "nosniff" {
		t.Errorf("/notes.txt X-Content-Type-Options = %q", v)
	}

	// Error pages carry the headers of the requested path.
	if w = get(t, s, "demo", "/missing"); w.Code != http.StatusNotFound || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("/missing: code %d headers %v", w.Code, w.Header())
	}

	// Redirects don't.
	if w = get(t, s, "demo", "/old"); w.Code != http.StatusMovedPermanently || w.Header().Get("X-Frame-Options") != "" {
		t.Errorf("/old: code %d headers %v", w.Code, w.Header())
	}

	// _headers itself is never served.
	if w = get(t, s, "demo", "/_headers"); w.Code != http.StatusNotFound || w.Body.String() != "missing" {
		t.Errorf("/_headers: code %d body %q, want the site's 404", w.Code, w.Body.String())
	}
}

func TestHeadersRulesCachedAndForgotten(t *testing.T) {
	s := newTestServer(t)
	s.rules = newRulesStore(s.store, time.Hour)
	putSite(t, s.store, "demo", map[string]string{"index.html": "home"})
	if w := get(t, s, "demo", "/"); w.Header().Get("X-New") != "" {
		t.Fatalf("unexpected header %v", w.Header())
	}
	putSite(t, s.store, "demo", map[string]string{"index.html": "home", "_headers": "/*\n  X-New: 1"})
	s.rules.forget("demo")
	if w := get(t, s, "demo", "/"); w.Header().Get("X-New") != "1" {
		t.Errorf("after forget: headers %v", w.Header())
	}
}

// Every response on a <site>.<base> address is noindex; custom domains are
// left alone, so a site that must be indexed lives on its own domain.
func TestNoindexOnQuickAddresses(t *testing.T) {
	s, _ := newDomainTestServer(t)
	putSite(t, s.store, "other", map[string]string{
		"index.html": "<h1>other</h1>",
		"_headers":   "/*\n  X-Robots-Tag: noarchive",
	})
	s.meta.save("other", policy{Access: "public", Domains: []siteDomain{{Name: "www.example.com", VerifiedAt: nowStamp()}}})
	s.domains.set("www.example.com", "other", true)

	w := hostGet(s, "other.quick.test", "/")
	if w.Code != http.StatusOK {
		t.Fatalf("quick address: code %d", w.Code)
	}
	if got := w.Header().Values("X-Robots-Tag"); !reflect.DeepEqual(got, []string{"noindex, nofollow", "noarchive"}) {
		t.Errorf("quick address X-Robots-Tag = %q, want the default plus the site's", got)
	}
	for _, p := range []string{"/missing.css", "/__quick/fonts/hanken-grotesk-latin.woff2", "/__quick/code", "/robots.txt"} {
		if got := hostGet(s, "other.quick.test", p).Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
			t.Errorf("%s: X-Robots-Tag = %q", p, got)
		}
	}
	// Sites without a policy are SSO sites.
	if got := hostGet(s, "app.quick.test", "/").Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Errorf("SSO site: X-Robots-Tag = %q", got)
	}

	w = hostGet(s, "www.example.com", "/")
	if w.Code != http.StatusOK {
		t.Fatalf("custom domain: code %d", w.Code)
	}
	if got := w.Header().Values("X-Robots-Tag"); !reflect.DeepEqual(got, []string{"noarchive"}) {
		t.Errorf("custom domain X-Robots-Tag = %q, want only the site's", got)
	}
	if got := hostGet(s, "quick.test", "/api/config").Header().Get("X-Robots-Tag"); got != "" {
		t.Errorf("apex: X-Robots-Tag = %q", got)
	}
}
