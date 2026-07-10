package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

func newTokenTestServer(t *testing.T) *server {
	t.Helper()
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: t.TempDir(), MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		store:      st,
		meta:       newMetaStore(st, []byte("secret"), 0),
		domain:     "wayexperience.it",
		baseDomain: "quick.example.com",
		ownership:  "owned",
	}
}

func TestDeployTokenAuthenticatesForSiteScope(t *testing.T) {
	s := newTokenTestServer(t)
	raw := "qk_test-token"
	p := policy{
		CreatedBy: "owner@wayexperience.it",
		Tokens: []siteToken{{
			ID: "tok1", Name: "ci", Hash: s.meta.hashToken("demo", raw),
			Scopes: []string{quick.TokenScopeDeploy}, CreatedBy: "owner@wayexperience.it",
			ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}},
	}
	r := httptest.NewRequest(http.MethodPost, "/api/deploy?name=demo", nil)
	r.Header.Set("Authorization", "Bearer "+raw)

	ident, err := s.authenticateForSite(r, "demo", p, quick.TokenScopeDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if ident.Email != "owner@wayexperience.it" || ident.Actor != "token:ci@demo" || !ident.Token {
		t.Fatalf("identity = %+v", ident)
	}
}

func TestDeployTokenRejectedByUserAuth(t *testing.T) {
	s := newTokenTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/api/site/demo/tokens", nil)
	r.Header.Set("Authorization", "Bearer qk_not-for-admin")
	if _, err := s.authenticate(r); err == nil {
		t.Fatal("quick deploy token accepted as a user token")
	}
}

func TestTokenExpiryChoices(t *testing.T) {
	if exp, err := tokenExpiry("90d"); err != nil || time.Until(exp) < 89*24*time.Hour {
		t.Fatalf("90d expiry = %v err=%v", exp, err)
	}
	if exp, err := tokenExpiry("never"); err != nil || !exp.IsZero() {
		t.Fatalf("never expiry = %v err=%v", exp, err)
	}
	if _, err := tokenExpiry("2y"); err == nil {
		t.Fatal("unsupported expiry accepted")
	}
}
