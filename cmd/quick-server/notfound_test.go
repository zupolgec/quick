package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/storage"
)

func newGateServer(t *testing.T) *server {
	t.Helper()
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: t.TempDir(), MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// oauth2URL points nowhere: any SSO check fails, as for a signed-out visitor.
	s := &server{store: st, baseDomain: "example.test", oauth2URL: "http://127.0.0.1:1",
		meta: newMetaStore(st, []byte("secret"), 0), rules: newRulesStore(st, 5*time.Second)}
	s.setupSiteProxy()
	return s
}

func requestSite(s *server, host, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = host
	r.Header.Set("Accept-Language", "it")
	w := httptest.NewRecorder()
	s.route(w, r)
	return w
}

// A site that was never published still asks for sign-in first: whether a
// name exists must not be visible from outside.
func TestUnknownSiteAsksSignInFirst(t *testing.T) {
	s := newGateServer(t)
	w := requestSite(s, "ghost.example.test", "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/oauth2/sign_in") {
		t.Fatalf("code %d, want the sign-in page", w.Code)
	}
}

// Once signed in, an unknown site gets a real HTML 404 page (an empty body
// makes mobile browsers download a file).
func TestUnknownSiteAfterSignInIsHTML404(t *testing.T) {
	s := newGateServer(t)
	s.noAuth = true // signed in
	w := requestSite(s, "ghost.example.test", "/")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "ghost.example.test") {
		t.Errorf("404 page does not name the site: %q", w.Body.String())
	}
}

// A published SSO site still asks a signed-out visitor to sign in.
func TestExistingSSOSiteStillAsksSignIn(t *testing.T) {
	s := newGateServer(t)
	putSite(t, s.store, "real", map[string]string{"index.html": "<h1>hi</h1>"})
	w := requestSite(s, "real.example.test", "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/oauth2/sign_in") {
		t.Fatalf("code %d, want the sign-in page", w.Code)
	}
}

// A missing page on a site without its own 404.html gets the HTML 404 page.
func TestMissingPageWithoutCustom404IsHTML(t *testing.T) {
	s := newTestServer(t)
	s.baseDomain = "example.test"
	putSite(t, s.store, "demo", map[string]string{"index.html": "<h1>home</h1>"})
	w := get(t, s, "demo", "/nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type %q, want text/html", ct)
	}
	if w.Body.Len() == 0 {
		t.Error("empty 404 body")
	}
}
