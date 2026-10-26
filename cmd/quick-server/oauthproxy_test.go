package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newOAuthErrServer fronts a fake oauth2-proxy that answers /oauth2/callback
// with its own HTML error page (status from ?fail=), as on a refused sign-in.
func newOAuthErrServer(t *testing.T) *server {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("fail") {
		case "403":
			proxyErrorPage(w, http.StatusForbidden, "You do not have permission to access this resource.")
		case "csrf":
			proxyErrorPage(w, http.StatusForbidden, "Login Failed: Unable to find a valid CSRF token. Please try again.")
		case "500":
			proxyErrorPage(w, http.StatusInternalServerError, "Oops! Something went wrong. For more information contact your server administrator.")
		case "idp":
			proxyErrorPage(w, http.StatusForbidden, "access_denied &amp; more")
		case "json":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"x"}`))
		default:
			http.Redirect(w, r, "https://accounts.example/auth", http.StatusFound)
		}
	}))
	t.Cleanup(fake.Close)
	s := &server{baseDomain: "quick.test", domain: "example.com", oauth2URL: fake.URL}
	if err := s.setupOAuthProxy(); err != nil {
		t.Fatal(err)
	}
	return s
}

// proxyErrorPage writes the "More Info" part of oauth2-proxy's error template.
func proxyErrorPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(`<html><body><div class="status-code">` + http.StatusText(status) + `</div>
      <div class="card-content has-text-left is-hidden">
        <div class="content">
          ` + msg + `
        </div>
        <div class="content">
          Request ID: 2732b6d6-79db
        </div>
      </div>
<p>Secured with OAuth2 Proxy</p></body></html>`))
}

func oauthReq(s *server, path, acceptLang string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://quick.test"+path, nil)
	r.Header.Set("Accept-Language", acceptLang)
	w := httptest.NewRecorder()
	s.handleOAuth2(w, r)
	return w
}

// A refused sign-in shows quick's own page, not oauth2-proxy's: same status,
// branded, in the visitor's language, with a way to sign in again.
func TestOAuthErrorPageIsBranded(t *testing.T) {
	s := newOAuthErrServer(t)

	w := oauthReq(s, "/oauth2/callback?fail=403", "it-IT,it;q=0.9")
	body := w.Body.String()
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
	if strings.Contains(body, "OAuth2 Proxy") {
		t.Fatalf("oauth2-proxy page leaked through: %s", body)
	}
	for _, want := range []string{`class="brand"`, "Accesso non consentito", "example.com", `href="/oauth2/sign_in"`, "<details", "2732b6d6-79db"} {
		if !strings.Contains(body, want) {
			t.Errorf("403 page misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "You do not have permission") {
		t.Errorf("generic oauth2-proxy message shown: %s", body)
	}

	// An expired sign-in is not a refused account.
	w = oauthReq(s, "/oauth2/callback?fail=csrf", "it")
	body = w.Body.String()
	if !strings.Contains(body, "Accesso scaduto") || strings.Contains(body, "Accesso non consentito") || !strings.Contains(body, "CSRF") {
		t.Errorf("csrf page: %s", body)
	}

	// The IdP's own reason is kept, unescaped once and escaped again.
	w = oauthReq(s, "/oauth2/callback?fail=idp", "en")
	if body = w.Body.String(); !strings.Contains(body, "access_denied &amp; more") {
		t.Errorf("idp message lost: %s", body)
	}

	w = oauthReq(s, "/oauth2/callback?fail=500", "en")
	body = w.Body.String()
	if w.Code != http.StatusInternalServerError || strings.Contains(body, "OAuth2 Proxy") || !strings.Contains(body, "Sign-in failed") {
		t.Fatalf("500 page: %d %s", w.Code, body)
	}
}

// Redirects and non-HTML answers pass through untouched.
func TestOAuthPassThrough(t *testing.T) {
	s := newOAuthErrServer(t)
	if w := oauthReq(s, "/oauth2/sign_in", "en"); w.Code != http.StatusFound || w.Header().Get("Location") != "https://accounts.example/auth" {
		t.Fatalf("redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := oauthReq(s, "/oauth2/callback?fail=json", "en"); w.Code != http.StatusForbidden || w.Body.String() != `{"error":"x"}` {
		t.Fatalf("json: %d %s", w.Code, w.Body.String())
	}
}
