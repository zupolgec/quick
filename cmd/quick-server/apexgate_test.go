package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newApexGateServer serves the apex "quick.test" through the real router, with
// a fake oauth2-proxy: the "_oauth2_proxy=ok" cookie is a signed-in user.
func newApexGateServer(t *testing.T) *server {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/auth":
			if c, err := r.Cookie("_oauth2_proxy"); err == nil && c.Value == "ok" {
				w.Header().Set("X-Auth-Request-Email", "ann@example.com")
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.Write([]byte("oauth2-proxy " + r.URL.Path))
		}
	}))
	t.Cleanup(fake.Close)
	s := newTestServer(t)
	s.baseDomain = "quick.test"
	s.domain = "example.com"
	s.oauth2URL = fake.URL
	s.meta = newMetaStore(s.store, []byte("secret"), time.Second)
	s.domains = newDomainIndex()
	if err := s.setupOAuthProxy(); err != nil {
		t.Fatal(err)
	}
	s.apexMux = s.buildApexMux()
	putSite(t, s.store, "app", map[string]string{"index.html": "<h1>app</h1>"})
	return s
}

func apexReq(s *server, method, path string, signedIn bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://quick.test"+path, nil)
	if signedIn {
		r.AddCookie(&http.Cookie{Name: "_oauth2_proxy", Value: "ok"})
	}
	w := httptest.NewRecorder()
	s.route(w, r)
	return w
}

const ssoMarker = "/oauth2/sign_in?rd="

// Guests on the apex home get the same SSO page as on /dashboard, and nothing
// from the install/usage content.
func TestApexHomeRequiresSSO(t *testing.T) {
	s := newApexGateServer(t)
	dash := apexReq(s, http.MethodGet, "/dashboard", false)
	for _, p := range []string{"/", "/?lang=it", "/dashboard", "/dashboard/site/app"} {
		w := apexReq(s, http.MethodGet, p, false)
		b := w.Body.String()
		if w.Code != dash.Code {
			t.Errorf("%s: guest code %d, dashboard guest code %d", p, w.Code, dash.Code)
		}
		if !strings.Contains(b, ssoMarker+url.QueryEscape("https://quick.test"+p)) {
			t.Errorf("%s: guest does not get the SSO page:\n%s", p, b)
		}
		for _, leak := range []string{"install.sh", "install.ps1", "quick login", "quick deploy", "Get started", "Come iniziare"} {
			if strings.Contains(b, leak) {
				t.Errorf("%s: guest page leaks %q", p, leak)
			}
		}
	}
}

func TestApexHomeSignedIn(t *testing.T) {
	s := newApexGateServer(t)
	w := apexReq(s, http.MethodGet, "/", true)
	b := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(b, "curl -fsSL https://quick.test/install.sh | sh") || !strings.Contains(b, "Get started") {
		t.Errorf("signed-in home: code %d\n%s", w.Code, b)
	}
	if strings.Contains(b, ssoMarker) {
		t.Errorf("signed-in home shows the SSO page")
	}
}

func TestApexHomeDevNoAuth(t *testing.T) {
	s := newApexGateServer(t)
	s.noAuth = true
	w := apexReq(s, http.MethodGet, "/", false)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Get started") {
		t.Errorf("QUICK_DEV_NOAUTH home: code %d\n%s", w.Code, w.Body)
	}
}

// Machine and CLI endpoints stay reachable without an SSO cookie: they answer
// themselves (or authenticate with bearer tokens), never with the SSO page.
func TestApexPublicEndpointsSkipSSO(t *testing.T) {
	s := newApexGateServer(t)
	cases := []struct {
		method, path string
		code         int // 0: any status, as long as it is not the SSO page
		body         string
	}{
		{http.MethodGet, "/api/health", http.StatusOK, "ok"},
		{http.MethodGet, "/api/config", http.StatusOK, `"base_domain"`},
		{http.MethodGet, "/api/domains/check?domain=nope.example.org", http.StatusNotFound, ""},
		{http.MethodGet, "/api/sites", http.StatusUnauthorized, "missing token"},
		{http.MethodGet, "/api/site/app", 0, ""},
		{http.MethodPost, "/api/deploy", 0, ""},
		{http.MethodPost, "/api/deploy/plan", 0, ""},
		{http.MethodGet, "/oauth2/sign_in?rd=https%3A%2F%2Fquick.test%2F", http.StatusOK, "oauth2-proxy /oauth2/sign_in"},
		{http.MethodGet, "/oauth2/callback", http.StatusOK, "oauth2-proxy /oauth2/callback"},
		{http.MethodGet, "/install.sh", http.StatusOK, "#!"},
		{http.MethodGet, "/install.ps1", http.StatusOK, ""},
		{http.MethodGet, "/fonts/hanken-grotesk-latin.woff2", http.StatusOK, ""},
		{http.MethodGet, "/img/logo.png", http.StatusOK, ""},
		{http.MethodGet, "/img/favicon.png", http.StatusOK, ""},
	}
	for _, c := range cases {
		w := apexReq(s, c.method, c.path, false)
		b := w.Body.String()
		if c.code != 0 && w.Code != c.code {
			t.Errorf("%s %s: code %d, want %d (%s)", c.method, c.path, w.Code, c.code, b)
		}
		if strings.Contains(b, ssoMarker) {
			t.Errorf("%s %s: got the SSO page", c.method, c.path)
		}
		if c.body != "" && !strings.Contains(b, c.body) {
			t.Errorf("%s %s: body missing %q:\n%s", c.method, c.path, c.body, b)
		}
	}
}
