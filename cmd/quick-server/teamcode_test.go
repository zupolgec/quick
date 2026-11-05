package main

// Code sites and the team: people signed in with the company SSO open a code
// site without the code, unless the site asks the team for it too (code only),
// which only holds on a site the team can't manage (locked, or not in free
// ownership mode).

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

func codeOnlyPolicy() policy {
	return policy{CreatedBy: ann, Access: quick.AccessCode, Code: goodCode, CodeHash: "", CodeOnly: true, Locked: true, Owner: ann}
}

func TestTeamOpensCodeSiteWithSSO(t *testing.T) {
	e := newCodeEnv(t)
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<h1>app</h1>") {
		t.Errorf("signed-in colleague: %d %q, want the site", w.Code, w.Body.String())
	}
	w := e.visit("app.quick.test", "", "/", nil)
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "/__quick/code") {
		t.Errorf("visitor without SSO: %d %q, want the code page", w.Code, w.Header().Get("Location"))
	}
	if w := e.visit("app.quick.test", "someone@gmail.com", "/", nil); w.Code != http.StatusFound {
		t.Errorf("non-company account: %d, want the code page", w.Code)
	}
}

func TestCodePageOffersCompanySignIn(t *testing.T) {
	e := newCodeEnv(t)
	rd := "https://app.quick.test/page"
	w := e.visit("app.quick.test", "", "/__quick/code?rd="+url.QueryEscape(rd), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "https://quick.test/oauth2/sign_in?rd="+url.QueryEscape(rd)) {
		t.Errorf("code page without the company sign-in link: %d\n%s", w.Code, w.Body)
	}
	// Already signed in: straight to the page.
	w = e.visit("app.quick.test", bob, "/__quick/code?rd="+url.QueryEscape(rd), nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != rd {
		t.Errorf("signed-in colleague on the code page: %d %q, want %s", w.Code, w.Header().Get("Location"), rd)
	}
}

func TestCodePageOnCustomDomainHasNoSignIn(t *testing.T) {
	e := newCodeEnv(t)
	e.s.domains.set("www.example.com", "app", true)
	w := e.visit("www.example.com", "", "/__quick/code", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "oauth2/sign_in") {
		t.Errorf("custom domain code page: %d, must not offer the company sign-in\n%s", w.Code, w.Body)
	}
}

func TestCodeOnlySiteAsksTheTeamToo(t *testing.T) {
	e := newCodeEnv(t)
	p := codeOnlyPolicy()
	p.CodeHash = e.s.meta.hashCode(goodCode)
	e.setPolicy(t, "app", p)
	w := e.visit("app.quick.test", bob, "/", nil)
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "/__quick/code") {
		t.Fatalf("colleague on a code-only site: %d %q, want the code page", w.Code, w.Header().Get("Location"))
	}
	w = e.visit("app.quick.test", bob, "/__quick/code", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "oauth2/sign_in") {
		t.Errorf("code-only code page: %d, must show the form without the company sign-in\n%s", w.Code, w.Body)
	}
	if c := accessCookie(e.tryCode(goodCode, "10.0.0.9")); c == nil {
		t.Error("the code doesn't open a code-only site")
	}
}

func TestCodeOnlyLapsesOnSitesTheTeamCanManage(t *testing.T) {
	e := newCodeEnv(t)
	p := codeOnlyPolicy()
	p.CodeHash = e.s.meta.hashCode(goodCode)
	p.Locked, p.Owner = false, "" // free mode, unlocked: anyone could read the code
	e.setPolicy(t, "app", p)
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code != http.StatusOK {
		t.Errorf("unlocked code-only site in free mode: %d, want the colleague in", w.Code)
	}
	e.s.ownership = "shared"
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code != http.StatusFound {
		t.Errorf("code-only site in shared mode: %d, want the code page", w.Code)
	}
}

func TestCodeOnlyNeedsALockInFreeMode(t *testing.T) {
	e := newPeopleEnv(t)
	code, yes, no := "abcd1234", true, false
	w, _ := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code, CodeOnly: &yes})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code-only on an unlocked site in free mode: %d, want 400", w.Code)
	}
	w, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code, CodeOnly: &yes, Locked: &yes})
	if w.Code != http.StatusOK || !res.CodeOnly || !res.Locked {
		t.Fatalf("code-only with a lock: %d %+v", w.Code, res)
	}
	// Unlocking gives the team its way in back.
	if w, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Locked: &no}); w.Code != http.StatusOK || res.CodeOnly {
		t.Errorf("after unlocking: %d %+v, want code_only off", w.Code, res)
	}
	if p := e.policyOf(t, "open"); p.CodeOnly || p.Code != code {
		t.Errorf("after unlocking: %+v, want code kept and code-only off", p)
	}

	e.s.ownership = "owned"
	if w, res := e.setAccess(t, ann, "open", quick.PolicyRequest{CodeOnly: &yes}); w.Code != http.StatusOK || !res.CodeOnly {
		t.Errorf("code-only alone in owned mode: %d %+v", w.Code, res)
	}
	if p := e.policyOf(t, "open"); p.Code != code {
		t.Errorf("code-only alone changed the code: %+v", p)
	}
	// Setting the access again starts from the default: the team gets in.
	if _, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code}); res.CodeOnly {
		t.Errorf("code set again without code_only: %+v", res)
	}
	if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{CodeOnly: &yes}); w.Code != http.StatusBadRequest {
		t.Errorf("code-only on a site without a code: %d, want 400", w.Code)
	}
}

func TestDashboardCodeOnlyOption(t *testing.T) {
	e := newPeopleEnv(t)
	code := "abcd1234"
	e.setPolicy(t, "open", policy{CreatedBy: ann, Access: quick.AccessCode, Code: code, CodeHash: e.s.meta.hashCode(code)})
	page, _ := e.sitePage(t, ann, "open")
	if !strings.Contains(page, `name="code_only"`) || !strings.Contains(page, "Disponibile solo per i siti bloccati") {
		t.Errorf("unlocked site: the option must be there, off, saying when it works:\n%s", page)
	}

	p := e.policyOf(t, "open")
	p.Locked, p.Owner = true, ann
	e.setPolicy(t, "open", p)
	_, csrf := e.sitePage(t, ann, "open")
	w := e.form(ann, "/api/site/open/policy", url.Values{"csrf": {csrf}, "op": {"access"}, "access": {"code"}, "code": {""}, "code_only": {"1"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("turning code-only on: %d\n%s", w.Code, w.Body)
	}
	if p := e.policyOf(t, "open"); !p.CodeOnly || p.Code != code {
		t.Fatalf("after the form: %+v, want code-only with the same code", p)
	}
	page, csrf = e.sitePage(t, ann, "open")
	if !strings.Contains(page, `name="code_only" value="1" checked`) {
		t.Errorf("the option doesn't show as on:\n%s", page)
	}
	e.form(ann, "/api/site/open/policy", url.Values{"csrf": {csrf}, "op": {"access"}, "access": {"code"}, "code": {""}})
	if p := e.policyOf(t, "open"); p.CodeOnly {
		t.Errorf("unticking the option left it on: %+v", p)
	}
}

// On a code site the client API names the colleague who came in with SSO;
// whoever came in with the code stays a guest (null).
func TestCodeSiteMeNamesColleagues(t *testing.T) {
	e := newCodeEnv(t)
	xq := map[string]string{"X-Quick": "1"}
	if w := e.visit("app.quick.test", bob, "/__quick/api/me", xq); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"email":"`+bob+`"`) {
		t.Errorf("colleague: %d %s", w.Code, w.Body)
	}
	c := accessCookie(e.tryCode(goodCode, "10.0.0.7"))
	r := httptest.NewRequest(http.MethodGet, "https://app.quick.test/__quick/api/me", nil)
	r.Header.Set("X-Quick", "1")
	r.AddCookie(c)
	w := httptest.NewRecorder()
	e.s.route(w, r)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"user":null}` {
		t.Errorf("guest with the code: %d %s", w.Code, w.Body)
	}
}
