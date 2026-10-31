package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The proxy must not hand the SSO session (cookie name set in the compose
// file) or an outside person's sign-in to the upstream.
func TestProxyStripsSSOAndGuestCookies(t *testing.T) {
	allowLoopback(t)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "cookie=%q", r.Header.Get("Cookie"))
	}))
	t.Cleanup(upstream.Close)
	s := newTestServer(t)
	trustCert(t, s, upstream)
	putSite(t, s.store, "demo", map[string]string{"_redirects": "/api/* " + upstream.URL + "/:splat 200"})

	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	r.AddCookie(&http.Cookie{Name: "_quick_sso", Value: "session"})
	r.AddCookie(&http.Cookie{Name: "_quick_sso_0", Value: "shard"})
	r.AddCookie(&http.Cookie{Name: "quick_guest_demo", Value: "guest"})
	r.AddCookie(&http.Cookie{Name: "other", Value: "1"})
	w := httptest.NewRecorder()
	s.serveSite(w, r, "demo")
	if got := w.Body.String(); got != `cookie="other=1"` {
		t.Errorf("upstream saw %s, want only other=1", got)
	}
}

// An upstream must not set cookies for the whole quick domain (other sites,
// the SSO session) nor overwrite quick's own cookies.
func TestProxyResponseCookiesStayOnSite(t *testing.T) {
	allowLoopback(t)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "app=1; Domain=.quick.example; Path=/; Secure")
		w.Header().Add("Set-Cookie", "_quick_sso=attacker; Domain=quick.example; Path=/")
		w.Header().Add("Set-Cookie", "quick_access_demo=x; Path=/")
	}))
	t.Cleanup(upstream.Close)
	s := newTestServer(t)
	trustCert(t, s, upstream)
	putSite(t, s.store, "demo", map[string]string{"_redirects": "/api/* " + upstream.URL + "/:splat 200"})

	w := get(t, s, "demo", "/api/x")
	got := w.Header().Values("Set-Cookie")
	if len(got) != 1 {
		t.Fatalf("Set-Cookie = %q, want only the app cookie", got)
	}
	if strings.Contains(strings.ToLower(got[0]), "domain") || !strings.HasPrefix(got[0], "app=1") {
		t.Errorf("Set-Cookie = %q, want app=1 without Domain", got[0])
	}
}
