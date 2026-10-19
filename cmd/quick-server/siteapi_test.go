package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newAPITestServer serves "app.quick.test" through the real router. sso=false
// points the SSO check at a dead oauth2-proxy, i.e. nobody is signed in.
func newAPITestServer(t *testing.T, sso bool) *server {
	t.Helper()
	s := newTestServer(t)
	s.baseDomain = "quick.test"
	s.domain = "example.com"
	s.meta = newMetaStore(s.store, []byte("secret"), time.Second)
	if sso {
		s.noAuth = true
	} else {
		s.oauth2URL = "http://127.0.0.1:1"
	}
	putSite(t, s.store, "app", map[string]string{"index.html": "<h1>app</h1>"})
	return s
}

func apiReq(s *server, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://app.quick.test"+path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.route(w, r)
	return w
}

var sameOrigin = map[string]string{"X-Quick": "1", "Sec-Fetch-Site": "same-origin"}

func TestClientAPIMeReturnsSSOUser(t *testing.T) {
	s := newAPITestServer(t, true)
	w := apiReq(s, http.MethodGet, "/__quick/api/me", sameOrigin)
	if w.Code != http.StatusOK {
		t.Fatalf("code %d body %s", w.Code, w.Body)
	}
	var got struct {
		User *struct{ Email string } `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.User == nil || got.User.Email != "dev@example.com" {
		t.Errorf("body %s", w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
}

// A public site exposes no identity, even to a signed-in visitor.
func TestClientAPIMePublicSiteIsAnonymous(t *testing.T) {
	s := newAPITestServer(t, true)
	if err := s.meta.save("app", policy{Access: "public"}); err != nil {
		t.Fatal(err)
	}
	w := apiReq(s, http.MethodGet, "/__quick/api/me", sameOrigin)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"user":null}` {
		t.Errorf("code %d body %s", w.Code, w.Body)
	}
}

func TestClientAPIRequiresSiteAccess(t *testing.T) {
	s := newAPITestServer(t, false)
	w := apiReq(s, http.MethodGet, "/__quick/api/me", sameOrigin)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("code %d body %s", w.Code, w.Body)
	}
}

// Sibling sites share the SSO cookie (same site): the API must refuse calls
// that don't come from the site's own origin.
func TestClientAPIRejectsCrossOrigin(t *testing.T) {
	s := newAPITestServer(t, true)
	for name, hdr := range map[string]map[string]string{
		"no header":  {"Sec-Fetch-Site": "same-origin"},
		"same-site":  {"X-Quick": "1", "Sec-Fetch-Site": "same-site"},
		"cross-site": {"X-Quick": "1", "Sec-Fetch-Site": "cross-site"},
	} {
		if w := apiReq(s, http.MethodGet, "/__quick/api/me", hdr); w.Code != http.StatusForbidden {
			t.Errorf("%s: code %d, want 403", name, w.Code)
		}
	}
}

func TestClientLibraryIsServed(t *testing.T) {
	s := newAPITestServer(t, false)
	w := apiReq(s, http.MethodGet, "/__quick/quick.js", nil)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || !strings.Contains(w.Body.String(), "/__quick/api/") {
		t.Errorf("quick.js: code %d type %q", w.Code, w.Header().Get("Content-Type"))
	}
	w = apiReq(s, http.MethodGet, "/__quick/quick.d.ts", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "me(") {
		t.Errorf("quick.d.ts: code %d", w.Code)
	}
}
