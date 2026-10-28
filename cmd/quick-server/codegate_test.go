package main

// Sites with an access code: the code can travel in the link
// (?quick_code=...), changing it signs everyone out, wrong guesses are
// limited and codes have a minimum length.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

const goodCode = "k7m2xq9p"

func newCodeEnv(t *testing.T) *peopleEnv {
	t.Helper()
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessCode, CodeHash: e.s.meta.hashCode(goodCode)})
	return e
}

func accessCookie(w *httptest.ResponseRecorder) *http.Cookie { return siteCookie(w, "app") }

// tryCode posts the code form from the client ip.
func (e *peopleEnv) tryCode(code, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://app.quick.test/__quick/code",
		strings.NewReader(url.Values{"code": {code}, "rd": {"https://app.quick.test/"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Forwarded-For", ip)
	w := httptest.NewRecorder()
	e.s.route(w, r)
	return w
}

func TestCodeInLinkSignsIn(t *testing.T) {
	e := newCodeEnv(t)
	w := e.visitWith("app.quick.test", "/page?x=1&quick_code="+goodCode)
	if w.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://app.quick.test/page?x=1" {
		t.Errorf("Location %q: the code must leave the address, other parameters stay", loc)
	}
	c := accessCookie(w)
	if c == nil {
		t.Fatal("no access cookie")
	}
	if got := e.visitWith("app.quick.test", "/", c); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "<h1>app</h1>") {
		t.Errorf("with the cookie: %d %q", got.Code, got.Body.String())
	}
}

func TestWrongCodeInLinkShowsForm(t *testing.T) {
	e := newCodeEnv(t)
	w := e.visitWith("app.quick.test", "/?quick_code=nope1234")
	if w.Code != http.StatusUnauthorized || accessCookie(w) != nil {
		t.Fatalf("status %d, cookie %v: want the form with an error and no cookie", w.Code, accessCookie(w))
	}
	if b := w.Body.String(); strings.Contains(b, "nope1234") || !strings.Contains(b, `name="code"`) {
		t.Errorf("form must not echo the code:\n%s", b)
	}
}

func TestChangingCodeSignsEveryoneOut(t *testing.T) {
	e := newCodeEnv(t)
	c := accessCookie(e.tryCode(goodCode, "10.0.0.1"))
	if c == nil {
		t.Fatal("no access cookie")
	}
	p := e.policyOf(t, "app")
	p.CodeHash = e.s.meta.hashCode("newcode99")
	e.setPolicy(t, "app", p)
	if w := e.visitWith("app.quick.test", "/", c); w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "/__quick/code") {
		t.Errorf("old cookie after a code change: %d %q, want the code page", w.Code, w.Header().Get("Location"))
	}
}

func TestWrongCodesAreLimited(t *testing.T) {
	e := newCodeEnv(t)
	for i := range codeFailuresMax {
		if w := e.tryCode("wrong-guess", "10.0.0.1"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, w.Code)
		}
	}
	w := e.tryCode(goodCode, "10.0.0.1")
	if w.Code != http.StatusTooManyRequests || accessCookie(w) != nil {
		t.Errorf("after %d wrong codes: status %d, want 429 and no cookie", codeFailuresMax, w.Code)
	}
	if w := e.visit("app.quick.test", "", "/?quick_code="+goodCode, map[string]string{"X-Forwarded-For": "10.0.0.1"}); w.Code != http.StatusTooManyRequests {
		t.Error("the link must be limited too")
	}
	if w := e.tryCode(goodCode, "10.0.0.2"); accessCookie(w) == nil {
		t.Errorf("another visitor is blocked: status %d", w.Code)
	}
}

func TestCodeMinimumLength(t *testing.T) {
	e := newPeopleEnv(t)
	short := "abc1234"
	if w, _ := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &short}); w.Code != http.StatusBadRequest {
		t.Errorf("7-character code: status %d, want 400", w.Code)
	}
	ok := "abcd1234"
	if w, _ := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &ok}); w.Code != http.StatusOK {
		t.Errorf("8-character code: status %d %s", w.Code, w.Body.String())
	}
	_, csrf := e.sitePage(t, ann, "open")
	w := e.form(ann, "/api/site/open/policy", url.Values{"csrf": {csrf}, "op": {"access"}, "access": {"code"}, "code": {short}})
	if !strings.Contains(w.Body.String(), "almeno 8 caratteri") {
		t.Errorf("dashboard doesn't explain the minimum length:\n%s", w.Body.String())
	}
}

func TestWrongCodesWindow(t *testing.T) {
	now := time.Now()
	c := codeAttempts{now: func() time.Time { return now }}
	for range codeFailuresMax {
		c.fail("10.0.0.1")
	}
	if !c.blocked("10.0.0.1") {
		t.Fatal("not blocked after the maximum")
	}
	now = now.Add(codeFailuresWindow + time.Second)
	if c.blocked("10.0.0.1") {
		t.Error("still blocked after the window")
	}
}

func TestCodeShownToWhoManagesTheSite(t *testing.T) {
	e := newPeopleEnv(t)
	code := "abcd1234"
	if w, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code}); w.Code != http.StatusOK || res.Code != code {
		t.Fatalf("set code: %d, response code %q", w.Code, res.Code)
	}
	if w := e.visitWith("open.quick.test", "/?quick_code="+code); siteCookie(w, "open") == nil {
		t.Error("the stored code doesn't open the site")
	}
	page, _ := e.sitePage(t, ann, "open")
	if !strings.Contains(page, ">"+code+"<") || !strings.Contains(page, "https://open.quick.test/?quick_code="+code) {
		t.Errorf("dashboard doesn't show code and link:\n%s", page)
	}

	// Locked by ann: bob can't manage it, so the API doesn't give him the code
	// (the dashboard page is only for who can manage the site).
	p := e.policyOf(t, "open")
	p.Locked, p.Owner = true, ann
	e.setPolicy(t, "open", p)
	w := e.api(t, bob, http.MethodGet, "/api/site/open/policy", nil)
	if strings.Contains(w.Body.String(), code) {
		t.Errorf("bob sees the code: %s", w.Body)
	}

	// Leaving code access forgets the code.
	if _, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessPublic)}); res.Code != "" {
		t.Errorf("public site still has a code: %q", res.Code)
	}
	if p := e.policyOf(t, "open"); p.Code != "" || p.CodeHash != "" {
		t.Errorf("code kept after leaving code access: %+v", p)
	}
}

func TestOldCodeNotShown(t *testing.T) {
	e := newCodeEnv(t) // stored as a hash only
	page, _ := e.sitePage(t, ann, "app")
	if !strings.Contains(page, "Il codice non è visibile") {
		t.Errorf("dashboard doesn't explain the code can't be shown:\n%s", page)
	}
	if w := e.visitWith("app.quick.test", "/?quick_code="+goodCode); accessCookie(w) == nil {
		t.Error("an old code stops working")
	}
}

func siteCookie(w *httptest.ResponseRecorder, site string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == "quick_access_"+site {
			return c
		}
	}
	return nil
}
