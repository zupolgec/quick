package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oauth2-proxy signs in any account; quick decides who is in. A session for an
// account outside the company domains is no access anywhere, and the sign-in
// page says which account it is and how to switch.
func TestSSOOutsideDomainIsRefused(t *testing.T) {
	e := newPeopleEnv(t)
	const outsider = "mario@gmail.com"
	it := map[string]string{"Accept-Language": "it"}

	w := e.visit("app.quick.test", outsider, "/", it)
	body := w.Body.String()
	if w.Code != http.StatusForbidden || strings.Contains(body, "<h1>app</h1>") {
		t.Fatalf("site served to outsider: %d %s", w.Code, body)
	}
	for _, want := range []string{outsider, "@example.com", "/oauth2/sign_out?rd=", "Accedi con un altro account"} {
		if !strings.Contains(body, want) {
			t.Errorf("site page misses %q:\n%s", want, body)
		}
	}

	w = e.visit("quick.test", outsider, "/dashboard", nil)
	if body = w.Body.String(); w.Code != http.StatusForbidden || !strings.Contains(body, outsider) || strings.Contains(body, `id="search"`) {
		t.Fatalf("dashboard for outsider: %d %s", w.Code, body)
	}

	if w = e.visit("quick.test", outsider, "/api/sites", nil); w.Code == http.StatusOK {
		t.Fatalf("API with outsider session: %d %s", w.Code, w.Body.String())
	}

	// Company accounts are unaffected; signed-out visitors get the usual page.
	if w = e.visit("app.quick.test", ann, "/", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<h1>app</h1>") {
		t.Fatalf("company account: %d %s", w.Code, w.Body.String())
	}
	if w = e.visit("app.quick.test", "", "/", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/oauth2/sign_in?rd=") {
		t.Fatalf("signed out: %d %s", w.Code, w.Body.String())
	}

	// Any account allowed: the outsider is in.
	e.s.domain = "*"
	if w = e.visit("app.quick.test", outsider, "/", nil); w.Code != http.StatusOK {
		t.Fatalf("open domains: %d", w.Code)
	}
}

// With Google the account must belong to the company Workspace (hd, which
// oauth2-proxy passes as the session's groups), not merely use a company
// address: a personal Google account created with one is refused.
func TestSSOGoogleRequiresWorkspace(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("_oauth2_proxy")
		if r.URL.Path != "/oauth2/auth" || err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		email, hd, _ := strings.Cut(c.Value, "|")
		w.Header().Set("X-Auth-Request-Email", email)
		if hd != "" {
			w.Header().Set("X-Auth-Request-Groups", hd)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(fake.Close)
	s := &server{baseDomain: "quick.test", domain: "example.com", oauth2URL: fake.URL} // no issuer: Google
	req := func(cookie string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://quick.test/", nil)
		r.AddCookie(&http.Cookie{Name: "_oauth2_proxy", Value: cookie})
		return r
	}
	if _, ok := s.checkSSO(req("ann@example.com|example.com")); !ok {
		t.Fatal("Workspace account refused")
	}
	if _, ok := s.checkSSO(req("ann@example.com")); ok {
		t.Fatal("company address without Workspace accepted")
	}
	if _, ok := s.checkSSO(req("ann@example.com|other.com")); ok {
		t.Fatal("other Workspace accepted")
	}
	s.domain = "*"
	if _, ok := s.checkSSO(req("mario@gmail.com")); !ok {
		t.Fatal("any account refused with *")
	}
}
