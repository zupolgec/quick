package main

import (
	"net/http"
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
