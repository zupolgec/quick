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
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("<html>Secured with OAuth2 Proxy 403 Forbidden</html>"))
		case "500":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("<html>Secured with OAuth2 Proxy 500</html>"))
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
	for _, want := range []string{`class="brand"`, "Accesso non consentito", "example.com", `href="/oauth2/sign_in"`} {
		if !strings.Contains(body, want) {
			t.Errorf("403 page misses %q:\n%s", want, body)
		}
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
