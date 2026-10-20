package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// The home, the dashboard and a site's page share one column width, so the
// logo doesn't jump when moving between them; home and dashboard point to the
// public docs; the dashboard can filter sites by name.
func TestApexPagesLayout(t *testing.T) {
	s := newAPITestServer(t, true)
	s.apexMux = s.buildApexMux()
	home := apexCall(s, http.MethodGet, "/", "").Body.String()
	dash := httptest.NewRecorder()
	s.renderDashboard(dash, langIT, "dev@example.com")
	site := httptest.NewRecorder()
	s.renderDashboardSitePage(site, httptest.NewRequest(http.MethodGet, "/", nil), "app", "dev@example.com", "")

	width := regexp.MustCompile(`\.wrap\{max-width:([^;]+);`)
	widths := map[string]string{}
	for name, body := range map[string]string{"home": home, "dashboard": dash.Body.String(), "site page": site.Body.String()} {
		m := width.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no .wrap width", name)
		}
		widths[name] = m[1]
	}
	if widths["home"] != widths["dashboard"] || widths["dashboard"] != widths["site page"] {
		t.Errorf("widths differ: %v", widths)
	}
	for name, body := range map[string]string{"home": home, "dashboard": dash.Body.String()} {
		if !regexp.MustCompile(`href="https://quick-cli\.com/docs/"`).MatchString(body) {
			t.Errorf("%s: no link to the docs", name)
		}
	}
	if !regexp.MustCompile(`<input[^>]+type="search"`).MatchString(dash.Body.String()) {
		t.Error("dashboard: no search field")
	}
}
