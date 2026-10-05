package main

// Local OIDC ID token verification (QUICK_OIDC_ISSUER): an in-process fake
// issuer serves discovery + JWKS and signs ID tokens with RSA.

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIssuer is a minimal OIDC IdP: discovery, JWKS and ID token signing.
type fakeIssuer struct {
	t      *testing.T
	server *httptest.Server
	issuer string
	priv   *rsa.PrivateKey
	kid    string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{t: t, priv: priv, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.issuer,
			"authorization_endpoint":                f.issuer + "/auth",
			"token_endpoint":                        f.issuer + "/token",
			"userinfo_endpoint":                     f.issuer + "/me",
			"jwks_uri":                              f.issuer + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       f.priv.Public(),
			KeyID:     f.kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}}
		_ = json.NewEncoder(w).Encode(ks)
	})
	f.server = httptest.NewServer(mux)
	f.issuer = f.server.URL
	t.Cleanup(f.server.Close)
	return f
}

type fakeTokenOpts struct {
	audience string
	email    string
	verified bool
	expiry   time.Time
	key      *rsa.PrivateKey
	kid      string
	omitMail bool
}

func (f *fakeIssuer) sign(opts fakeTokenOpts) string {
	f.t.Helper()
	key := opts.key
	if key == nil {
		key = f.priv
	}
	kid := opts.kid
	if kid == "" {
		kid = f.kid
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		f.t.Fatal(err)
	}
	exp := opts.expiry
	if exp.IsZero() {
		exp = time.Now().Add(time.Hour)
	}
	aud := opts.audience
	if aud == "" {
		aud = "test-client"
	}
	cl := jwt.Claims{
		Issuer:   f.issuer,
		Subject:  "user-123",
		Audience: jwt.Audience{aud},
		Expiry:   jwt.NewNumericDate(exp),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}
	b := jwt.Signed(signer).Claims(cl)
	if !opts.omitMail {
		b = b.Claims(map[string]any{"email": opts.email, "email_verified": opts.verified})
	}
	raw, err := b.Serialize()
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

// oidcServer builds a test server pointed at the fake issuer.
func (f *fakeIssuer) oidcServer(domain string) *server {
	return &server{domain: domain, clientID: "test-client", oidcIssuer: f.issuer}
}

func bearerRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/deploy?name=x", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestAuthenticateOIDCValid(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("example.com")
	email, err := s.authenticate(bearerRequest(f.sign(fakeTokenOpts{email: "alice@example.com", verified: true})))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if email != "alice@example.com" {
		t.Fatalf("email %q, want alice@example.com", email)
	}
}

func TestAuthenticateOIDCWrongAudience(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("example.com")
	tok := f.sign(fakeTokenOpts{audience: "other-client", email: "alice@example.com", verified: true})
	if _, err := s.authenticate(bearerRequest(tok)); err == nil {
		t.Fatal("expected audience error, got nil")
	}
}

func TestAuthenticateOIDCExpired(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("example.com")
	tok := f.sign(fakeTokenOpts{email: "alice@example.com", verified: true, expiry: time.Now().Add(-time.Hour)})
	if _, err := s.authenticate(bearerRequest(tok)); err == nil {
		t.Fatal("expected expiry error, got nil")
	}
}

func TestAuthenticateOIDCWrongSignature(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("example.com")
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok := f.sign(fakeTokenOpts{email: "alice@example.com", verified: true, key: other})
	if _, err := s.authenticate(bearerRequest(tok)); err == nil {
		t.Fatal("expected signature error, got nil")
	}
}

func TestAuthenticateOIDCUnverifiedEmail(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("example.com")
	tok := f.sign(fakeTokenOpts{email: "alice@example.com", verified: false})
	if _, err := s.authenticate(bearerRequest(tok)); err == nil {
		t.Fatal("expected email_verified error, got nil")
	}
}

func TestAuthenticateOIDCDisallowedDomain(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("example.com")
	tok := f.sign(fakeTokenOpts{email: "bob@other.com", verified: true})
	if _, err := s.authenticate(bearerRequest(tok)); err == nil {
		t.Fatal("expected domain error, got nil")
	}
}

func TestAuthenticateOIDCAllowAll(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("*")
	email, err := s.authenticate(bearerRequest(f.sign(fakeTokenOpts{email: "anyone@anywhere.org", verified: true})))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if email != "anyone@anywhere.org" {
		t.Fatalf("email %q, want anyone@anywhere.org", email)
	}
}

func TestAuthenticateOIDCMissingEmail(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("*")
	tok := f.sign(fakeTokenOpts{email: "", verified: true, omitMail: true})
	if _, err := s.authenticate(bearerRequest(tok)); err == nil {
		t.Fatal("expected missing-email error, got nil")
	}
}

func TestAuthenticateRejectsDeployToken(t *testing.T) {
	f := newFakeIssuer(t)
	s := f.oidcServer("example.com")
	if _, err := s.authenticate(bearerRequest("qk_abcdef")); err == nil {
		t.Fatal("expected qk_ rejection, got nil")
	} else if !strings.Contains(err.Error(), "deploy token") {
		t.Fatalf("error %q should mention deploy tokens", err)
	}
}

// The Google (hd) branch as a pure function, without calling Google.
func TestCheckClaimsGoogleHD(t *testing.T) {
	s := &server{domain: "example.com", clientID: "test-client"}
	ok, err := s.checkIDClaims(idClaims{Email: "u@example.com", Hd: "example.com"})
	if err != nil || ok != "u@example.com" {
		t.Fatalf("hd allowed: got %q, %v", ok, err)
	}
	// No fallback to the email domain: a consumer Google account can use a
	// company address without a matching hd.
	for _, c := range []idClaims{
		{Email: "u@example.com", Hd: ""},
		{Email: "u@example.com", Hd: "gmail.com"},
		{Email: "", Hd: "example.com"},
	} {
		if _, err := s.checkIDClaims(c); err == nil {
			t.Fatalf("claims %+v: expected error, got nil", c)
		}
	}
}

func TestCheckClaimsGeneric(t *testing.T) {
	s := &server{domain: "example.com", clientID: "test-client", oidcIssuer: "https://idp.example.test"}
	ok, err := s.checkIDClaims(idClaims{Email: "Alice@Example.COM", EmailVerified: true})
	if err != nil || ok != "Alice@Example.COM" {
		t.Fatalf("verified allowed (case-insensitive): got %q, %v", ok, err)
	}
	for name, c := range map[string]idClaims{
		"unverified": {Email: "a@example.com", EmailVerified: false},
		"empty mail": {Email: "", EmailVerified: true},
		"bad domain": {Email: "b@other.com", EmailVerified: true},
		"no domain":  {Email: "nodomain", EmailVerified: true},
	} {
		if _, err := s.checkIDClaims(c); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
}

func TestSSOButtonLabel(t *testing.T) {
	google := &server{baseDomain: "quick.example.test"}
	w := httptest.NewRecorder()
	google.renderSSOPage(w, httptest.NewRequest(http.MethodGet, "/", nil), "foo.quick.example.test")
	if b := w.Body.String(); !strings.Contains(b, "Sign in with Google") {
		t.Fatalf("google button should mention Google:\n%s", b)
	}

	oidc := &server{baseDomain: "quick.example.test", oidcIssuer: "https://idp.example.test"}
	wo := httptest.NewRecorder()
	oidc.renderSSOPage(wo, httptest.NewRequest(http.MethodGet, "/", nil), "foo.quick.example.test")
	bo := wo.Body.String()
	if strings.Contains(bo, "Google") {
		t.Fatalf("oidc button must be provider-neutral:\n%s", bo)
	}
	if !strings.Contains(bo, "Sign in</a>") {
		t.Fatalf("oidc button should say generic Sign in:\n%s", bo)
	}

	ri := httptest.NewRequest(http.MethodGet, "/?lang=it", nil)
	wi := httptest.NewRecorder()
	oidc.renderSSOPage(wi, ri, "foo.quick.example.test")
	if bi := wi.Body.String(); !strings.Contains(bi, ">Accedi</a>") || strings.Contains(bi, "Google") {
		t.Fatalf("italian oidc button should say Accedi:\n%s", bi)
	}
}

func TestValidateConfigOIDCIssuer(t *testing.T) {
	good := &server{domain: "example.com", clientID: "c"}
	for _, iss := range []string{"", "https://idp.16bit.it", "http://localhost:8080", "http://127.0.0.1:5556/issuer"} {
		s := &server{domain: good.domain, clientID: good.clientID, oidcIssuer: iss}
		if err := s.validateConfig("secret"); err != nil {
			t.Errorf("issuer %q: unexpected error %v", iss, err)
		}
	}
	for _, iss := range []string{"http://idp.16bit.it", "ftp://idp.16bit.it/x", "://broken", "idp.16bit.it"} {
		s := &server{domain: good.domain, clientID: good.clientID, oidcIssuer: iss}
		if err := s.validateConfig("secret"); err == nil {
			t.Errorf("issuer %q: expected error, got nil", iss)
		}
	}
	// Unchanged requirements: no secret/domain/client id, no start.
	if err := (&server{}).validateConfig(""); err == nil {
		t.Fatal("empty config should still fail validation")
	}
}

func TestDomainAllowedIgnoresEmptyListItems(t *testing.T) {
	s := &server{domain: "wayexperience.it,,"}
	if s.domainAllowed("") {
		t.Fatal("empty hd allowed by an empty list item")
	}
}
