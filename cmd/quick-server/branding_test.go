package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFontServedSameOriginNoCDN(t *testing.T) {
	s := &server{baseDomain: "quick.example.test", noAuth: true} // signed in: the home is SSO-gated

	// Assets served on the apex and on a site subdomain (same-origin everywhere),
	// under the reserved /__quick/ path.
	wantCT := map[string]string{
		"/fonts/hanken-grotesk-latin.woff2": "font/woff2",
		"/img/logo.svg":                     "image/svg+xml",
		"/img/logo.png":                     "image/png",
		"/img/logo-dark.png":                "image/png",
		"/img/favicon.svg":                  "image/svg+xml",
		"/img/favicon.png":                  "image/png",
	}
	for _, host := range []string{"quick.example.test", "foo.quick.example.test"} {
		for p, ct := range wantCT {
			p = "/__quick" + p
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.Host = host
			s.route(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("%s on %s: code %d, want 200", p, host, w.Code)
			}
			if got := w.Header().Get("Content-Type"); got != ct {
				t.Errorf("%s on %s: content-type %q, want %q", p, host, got, ct)
			}
			if w.Body.Len() == 0 {
				t.Errorf("%s on %s: empty body", p, host)
			}
		}
	}

	// Pages reference the self-hosted font and pull in no external CDN.
	page := httptest.NewRecorder()
	s.handleApexRoot(page, httptest.NewRequest(http.MethodGet, "/", nil))
	b := page.Body.String()
	if !strings.Contains(b, `/fonts/hanken-grotesk-latin.woff2`) {
		t.Errorf("landing does not reference the self-hosted font")
	}
	if strings.Contains(b, "googleapis.com") || strings.Contains(b, "gstatic.com") {
		t.Errorf("landing still references an external font CDN")
	}
}

// The pages share quick-cli.com's flat style: the vector logo inline (so it takes
// each colour scheme), no shadows, no gradients other than the logo's own arrow.
func TestPagesUseFlatBrand(t *testing.T) {
	s := &server{baseDomain: "quick.example.test", noAuth: true}
	page := httptest.NewRecorder()
	s.handleApexRoot(page, httptest.NewRequest(http.MethodGet, "/", nil))
	b := page.Body.String()
	if !strings.Contains(b, `<svg class="logo"`) {
		t.Errorf("landing does not inline the vector logo")
	}
	if strings.Contains(brandCSS, "box-shadow:0") || strings.Contains(brandCSS, "linear-gradient") {
		t.Errorf("brand CSS still has shadows or gradients")
	}
	if strings.Contains(b, "logo.png") {
		t.Errorf("landing still uses the raster logo")
	}
}

func TestHealthDoesNotRequireHost(t *testing.T) {
	s := &server{baseDomain: "quick.example.test"}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/health", nil)

	s.route(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("code %d, want 200", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "ok" {
		t.Fatalf("body %q, want ok", w.Body.String())
	}
}

// A site's own img/ and fonts/ folders are the site's: quick's brand assets
// live under the reserved /__quick/ path on site hosts.
func TestSiteOwnsImgAndFontsFolders(t *testing.T) {
	s := newAPITestServer(t, true)
	putSite(t, s.store, "app", map[string]string{
		"index.html":       "<h1>app</h1>",
		"img/logo.png":     "site-logo",
		"fonts/body.woff2": "site-font",
	})
	for p, want := range map[string]string{"/img/logo.png": "site-logo", "/fonts/body.woff2": "site-font"} {
		if w := hostGet(s, "app.quick.test", p); w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s on a site: code %d body %q, want the site's file", p, w.Code, w.Body.String())
		}
	}
	for _, host := range []string{"app.quick.test", "quick.test"} {
		for _, p := range []string{"/__quick/img/favicon.svg", "/__quick/fonts/hanken-grotesk-latin.woff2"} {
			if w := hostGet(s, host, p); w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Errorf("%s on %s: code %d", p, host, w.Code)
			}
		}
	}
	// The apex keeps the old URLs (the PNG logo is linked from outside).
	if w := hostGet(s, "quick.test", "/img/logo.png"); w.Code != http.StatusOK {
		t.Errorf("/img/logo.png on the apex: code %d", w.Code)
	}
	// Server-rendered pages point at the reserved path.
	if strings.Contains(brandHead+brandCSS, `"/img/`) || strings.Contains(brandHead+brandCSS, `"/fonts/`) {
		t.Errorf("pages still reference /img or /fonts")
	}
}
