package main

import (
	"strings"
	"testing"
)

// _redirects and _headers stay private whatever spelling reaches the file.
func TestRulesFilesNeverServedAnySpelling(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"_redirects": "/api/* https://secret-upstream.example/:splat 200",
		"_headers":   "/*\n  X-Test: 1",
		"index.html": "home",
	})
	for _, p := range []string{"/_redirects/", "/_headers/", "//_redirects", "/x/../_redirects", "/./_headers"} {
		if w := get(t, s, "demo", p); strings.Contains(w.Body.String(), "secret-upstream") || strings.Contains(w.Body.String(), "X-Test") {
			t.Errorf("%s served the rules file (code %d)", p, w.Code)
		}
	}
}
