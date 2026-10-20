// quick-server is the single front for *.<BASE_DOMAIN>: it serves sites from
// storage, enforces per-site policy (SSO / public / code), receives deploys and
// proxies SSO to oauth2-proxy. All config comes from env vars; nothing
// domain-specific is hardcoded.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const maxUpload = 200 << 20 // 200 MiB per deploy

// Set at image build time via -ldflags "-X main.version=vX.Y.Z"; exposed in
// /api/config so a running server can be told apart from the outside.
var version = "dev"

// Timeout so a hung dependency (IdP discovery/JWKS, oauth2-proxy) can't pin a
// request goroutine.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// googleIssuer is the default issuer when QUICK_OIDC_ISSUER is unset.
const googleIssuer = "https://accounts.google.com"

type server struct {
	store        storage.Backend
	meta         *metaStore
	baseDomain   string
	domain       string // allowed Google hosted domain; also exposed in /api/config
	clientID     string // ID token audience; also exposed in /api/config
	clientSecret string // optional CLI client secret (only for a Web client), served via /api/config
	oauth2URL    string
	ownership    string // free | shared | owned (QUICK_OWNERSHIP)
	oauthProxy   *httputil.ReverseProxy
	rules        *rulesStore            // per-site _redirects, TTL-cached
	siteProxy    *httputil.ReverseProxy // _redirects 200-to-URL proxying
	apexMux      *http.ServeMux
	noAuth       bool           // local development only
	locks        keyedMutex     // serializes per-site writes (single instance)
	leases       leaseTable     // one deploy at a time per site (lease.go)
	timeouts     deployTimeouts // deploy request bodies (timeouts.go)
	stage        *chunkStage    // delta deploy staging; nil = delta deploy off
	oidcIssuer   string         // QUICK_OIDC_ISSUER; empty = Google
	oidcMu       sync.Mutex     // guards oidcVerifier (lazy init)
	oidcVerifier *oidc.IDTokenVerifier
	domains      *domainIndex // custom domain -> site
	domainsMu    sync.Mutex   // serializes custom domain changes across sites
	resolver     resolver     // DNS checks for custom domains
}

type authIdentity struct {
	Email   string
	Actor   string
	Token   bool
	Site    string
	TokenID string
	Scopes  map[string]bool
}

// keyedMutex serializes operations per key (here: site name) so the
// load→modify→save cycle of deploy/policy/delete/rollback is atomic and two
// requests on the same site can't clobber each other. Zero value ready to use.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	mtx := k.m[key]
	if mtx == nil {
		mtx = &sync.Mutex{}
		k.m[key] = mtx
	}
	k.mu.Unlock()
	mtx.Lock()
	return mtx.Unlock
}

func main() {
	store, err := storage.New(storageConfigFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	metaSecret := os.Getenv("QUICK_META_SECRET")
	s := &server{
		store:        store,
		baseDomain:   os.Getenv("QUICK_BASE_DOMAIN"),
		domain:       os.Getenv("QUICK_ALLOWED_DOMAINS"),
		clientID:     os.Getenv("QUICK_OAUTH_CLIENT_ID"),
		clientSecret: os.Getenv("QUICK_OAUTH_CLIENT_SECRET"),
		oauth2URL:    quick.Env("QUICK_OAUTH2_URL", "http://oauth2-proxy:4180"),
		ownership:    parseOwnership(os.Getenv("QUICK_OWNERSHIP")),
		noAuth:       os.Getenv("QUICK_DEV_NOAUTH") == "1",
		oidcIssuer:   strings.TrimSpace(os.Getenv("QUICK_OIDC_ISSUER")),
	}
	if err := s.validateConfig(metaSecret); err != nil {
		log.Fatal(err)
	}
	s.meta = newMetaStore(store, []byte(metaSecret), 5*time.Second)
	s.rules = newRulesStore(store, 5*time.Second)
	if s.domains, err = loadDomainIndex(store, s.meta); err != nil {
		log.Fatal(err)
	}
	s.resolver = net.DefaultResolver
	s.stage = newChunkStage(quick.Env("QUICK_CHUNK_DIR", filepath.Join(os.TempDir(), "quick-chunks")))
	go s.stage.sweepLoop()
	go s.leases.sweepLoop()
	s.setupSiteProxy()
	if err := s.setupOAuthProxy(); err != nil {
		log.Fatal(err)
	}
	s.apexMux = s.buildApexMux()

	addr := quick.Env("QUICK_ADDR", ":8080")
	log.Printf("quick-server %s on %s (base=%s, storage=%s, ownership=%s, noauth=%v)", version, addr, s.baseDomain, quick.Env("QUICK_STORAGE", "local"), s.ownership, s.noAuth)
	log.Fatal(newHTTPServer(addr, http.HandlerFunc(s.route)).ListenAndServe())
}

// route dispatches by host: the apex (== baseDomain) is the control plane (API,
// auth, dashboard); every subdomain is just a site.
func (s *server) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/health" {
		fmt.Fprintln(w, "ok")
		return
	}
	// Asked by the proxy (on the internal host name) before issuing a
	// certificate for a custom domain.
	if r.URL.Path == "/api/domains/check" {
		s.handleDomainCheck(w, r)
		return
	}
	// Brand assets (font, logo, favicon) are served same-origin on every host so
	// their URLs never need CORS (pages live on the apex and site subdomains).
	if strings.HasPrefix(r.URL.Path, "/fonts/") || strings.HasPrefix(r.URL.Path, "/img/") {
		s.handleAsset(w, r)
		return
	}
	host := hostNoPort(fwdHost(r))
	if host == s.baseDomain {
		s.apexMux.ServeHTTP(w, r)
		return
	}
	sub := subOf(host, s.baseDomain)
	if sub == "" {
		if sub = s.customDomainSite(w, r, host); sub == "" {
			return
		}
	}
	if r.URL.Path == "/__quick/code" {
		s.handleCodePage(w, r, sub)
		return
	}
	if r.URL.Path == "/__quick/quick.js" || r.URL.Path == "/__quick/quick.d.ts" {
		s.handleClientLib(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/__quick/api/") {
		s.handleClientAPI(w, r, sub)
		return
	}
	s.handleSite(w, r, sub) // per-site gate + serve
}

func (s *server) buildApexMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	m.HandleFunc("/api/config", s.handleConfig)
	m.HandleFunc("/api/deploy", s.handleDeploy)
	m.HandleFunc("/api/deploy/plan", s.handleDeployPlan)
	m.HandleFunc("/api/deploy/chunks", s.handleDeployChunks)
	m.HandleFunc("/api/deploy/commit", s.handleDeployCommit)
	m.HandleFunc("/api/deploy/lease", s.handleDeployLease)
	m.HandleFunc("/api/sites", s.handleSites)
	m.HandleFunc("/api/site/", s.handleSiteAPI)
	m.HandleFunc("/oauth2/", s.handleOAuth2)
	m.HandleFunc("/install.sh", s.handleInstallSh)
	m.HandleFunc("/install.ps1", s.handleInstallPs1)
	m.HandleFunc("/dashboard/site/", s.handleDashboardSite)
	m.HandleFunc("/dashboard", s.handleDashboard) // sites dashboard (SSO page for guests)
	m.HandleFunc("/", s.handleApexRoot)           // public landing: install + usage
	return m
}

// validateConfig fails closed at startup: outside dev mode (QUICK_DEV_NOAUTH=1)
// the security-critical env vars must be set, otherwise auth would fail open
// (forgeable cookies for protected sites, any Google account allowed to deploy,
// unverified audience).
func (s *server) validateConfig(metaSecret string) error {
	if s.noAuth {
		log.Print("⚠ QUICK_DEV_NOAUTH=1: authentication disabled, local development only")
		return nil
	}
	var missing []string
	if metaSecret == "" {
		missing = append(missing, "QUICK_META_SECRET (signs cookies and codes for protected sites)")
	}
	if strings.TrimSpace(s.domain) == "" {
		missing = append(missing, `QUICK_ALLOWED_DOMAINS (allowed email domain; use "*" for any Google account)`)
	}
	if s.clientID == "" {
		missing = append(missing, "QUICK_OAUTH_CLIENT_ID (audience of the deploy ID token)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("insecure configuration, missing env vars:\n  - %s\n(set QUICK_DEV_NOAUTH=1 for local development only)", strings.Join(missing, "\n  - "))
	}
	if err := validateOIDCIssuer(s.oidcIssuer); err != nil {
		return err
	}
	if strings.TrimSpace(s.domain) == "*" {
		if s.oidcIssuer == "" {
			log.Print(`⚠ QUICK_ALLOWED_DOMAINS="*": any Google account can deploy`)
		} else {
			log.Print(`⚠ QUICK_ALLOWED_DOMAINS="*": any account can deploy`)
		}
	}
	return nil
}

// validateOIDCIssuer requires an https QUICK_OIDC_ISSUER (http only on
// loopback, for tests). Empty means Google.
func validateOIDCIssuer(iss string) error {
	if iss == "" {
		return nil
	}
	u, err := url.Parse(iss)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid QUICK_OIDC_ISSUER %q: must be an https URL", iss)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return fmt.Errorf("invalid QUICK_OIDC_ISSUER %q: must be an https URL (http only for localhost)", iss)
}

func parseOwnership(v string) string {
	switch v {
	case "shared", "owned":
		return v
	default:
		return "free"
	}
}

func hostNoPort(h string) string {
	host, _, _ := strings.Cut(h, ":")
	return host
}

func (s *server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	w = s.boundIO(w, r, s.timeouts.uploadOr()) // it holds the site lock while it reads
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	name, ok := deployName(w, r)
	if !ok {
		return
	}
	unlock := sync.OnceFunc(s.locks.lock(name))
	defer unlock()
	cur, ident, ok := s.authorizeDeploy(w, r, name, true)
	if !ok {
		return
	}
	// A deploy of its own: refused during another one, holds the lease while
	// it uploads (lease.go).
	lease, ok := s.beginLease(w, r, name, ident, unlock)
	if !ok {
		return
	}
	defer s.leases.release(name, lease)
	body := http.MaxBytesReader(w, r.Body, maxUpload)
	gz, err := gzip.NewReader(body)
	if err != nil {
		http.Error(w, "gzip: "+err.Error(), http.StatusBadRequest)
		return
	}
	old, err := s.clearVersions(name, &cur)
	if err != nil {
		http.Error(w, "could not update site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// The files are chunked as they stream into PutSite: the new version's
	// index without reading it back.
	pr, pw := io.Pipe()
	var files []indexFile
	done := make(chan struct{})
	go func() {
		defer close(done)
		var err error
		files, err = indexingTar(pw, tar.NewReader(gz))
		pw.CloseWithError(err)
	}()
	err = s.store.PutSite(name, tar.NewReader(pr))
	pr.Close() // unblocks the indexer if PutSite stopped early
	<-done
	if err != nil {
		s.publishFailed(name, &cur, old)
		http.Error(w, "deploy: "+err.Error(), http.StatusBadRequest)
		return
	}
	cur.PrevVersion = old.live
	if files != nil {
		cur.Version = s.saveIndex(name, files)
	}
	s.finishDeploy(w, name, cur, ident)
}

// deployName reads and validates the ?name= of a deploy request.
func deployName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.URL.Query().Get("name")
	if !quick.ValidName(name) {
		http.Error(w, "invalid site name (use a-z, 0-9, hyphen)", http.StatusBadRequest)
		return "", false
	}
	return name, true
}

// authorizeDeploy runs the checks every deploy endpoint shares: readable site
// state (fail closed), an identity with deploy scope (SSO user or site token),
// ownership/lock. On failure the response is already written. Writers call it
// under s.locks (locked=true: the policy is read fresh, since they save it
// back) so the policy can't change before they publish.
func (s *server) authorizeDeploy(w http.ResponseWriter, r *http.Request, name string, locked bool) (policy, authIdentity, bool) {
	load := s.meta.load
	if locked {
		load = s.meta.loadFresh
	}
	cur, err := load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return policy{}, authIdentity{}, false
	}
	ident, err := s.authenticateForSite(r, name, cur, quick.TokenScopeDeploy)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return policy{}, authIdentity{}, false
	}
	if ok, reason := s.canWrite(cur, ident.Email, actDeploy); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return policy{}, authIdentity{}, false
	}
	return cur, ident, true
}

// finishDeploy runs after a successful PutSite: drops cached rules, records
// who deployed (and the token use) along with the index versions the caller
// set in cur, prunes older indexes, answers with the site URL.
func (s *server) finishDeploy(w http.ResponseWriter, name string, cur policy, ident authIdentity) {
	s.rules.forget(name)
	by := ident.Email
	if ident.Actor != "" {
		by = ident.Actor
	}
	now := nowStamp()
	if cur.CreatedBy == "" {
		cur.CreatedBy, cur.CreatedAt = ident.Email, now
	}
	cur.UpdatedBy, cur.UpdatedAt = by, now
	if ident.Token {
		s.markTokenUsed(&cur, ident.TokenID, by, now)
	}
	if err := s.meta.save(name, cur); err != nil {
		log.Printf("WARNING: deploy %q applied but saving metadata failed: %v", name, err)
		http.Error(w, "deploy applied but saving state failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.pruneIndexes(name, cur)
	log.Printf("deploy %q by %s", name, by)
	_ = json.NewEncoder(w).Encode(quick.DeployResponse{
		Site: name,
		URL:  "https://" + name + "." + s.baseDomain,
		By:   by,
	})
}

// authenticate verifies an OIDC ID token (Authorization: Bearer) locally
// against the issuer (Google unless QUICK_OIDC_ISSUER) and returns the email,
// checking audience and allowed domain. Quick deploy tokens are intentionally
// not accepted here; token management, delete and policy changes require a
// real SSO user.
func (s *server) authenticate(r *http.Request) (string, error) {
	if s.noAuth {
		return "dev@" + def(s.domain, "example.com"), nil
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return "", errors.New("missing token")
	}
	if strings.HasPrefix(tok, "qk_") {
		return "", errors.New("quick deploy tokens are not valid for this action")
	}
	v, err := s.verifier(r.Context())
	if err != nil {
		return "", err
	}
	idToken, err := v.Verify(oidc.ClientContext(r.Context(), httpClient), tok)
	if err != nil {
		return "", err
	}
	var claims idClaims
	if err := idToken.Claims(&claims); err != nil {
		return "", err
	}
	return s.checkIDClaims(claims)
}

// oidcIssuerURL is the expected issuer: QUICK_OIDC_ISSUER, or Google.
func (s *server) oidcIssuerURL() string {
	if s.oidcIssuer != "" {
		return s.oidcIssuer
	}
	return googleIssuer
}

// verifier builds the OIDC verifier on first use and caches it, so the server
// starts even if the IdP is briefly down.
func (s *server) verifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if s.oidcVerifier != nil {
		return s.oidcVerifier, nil
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, httpClient), s.oidcIssuerURL())
	if err != nil {
		return nil, err
	}
	cfg := &oidc.Config{ClientID: s.clientID}
	if s.clientID == "" {
		cfg.SkipClientIDCheck = true // no client id configured: no audience check
	}
	s.oidcVerifier = provider.Verifier(cfg)
	return s.oidcVerifier, nil
}

// idClaims are the verified claims that decide access.
type idClaims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Hd            string `json:"hd"`
}

// checkIDClaims applies the domain check and returns the email. Google: the
// hosted domain (hd), never the email domain, since consumer Google accounts
// can use company addresses. Other issuers: a verified email in an allowed
// domain.
func (s *server) checkIDClaims(c idClaims) (string, error) {
	if strings.TrimSpace(c.Email) == "" {
		return "", errors.New("missing email in token")
	}
	if s.oidcIssuerURL() == googleIssuer {
		if !s.domainAllowed(c.Hd) {
			return "", fmt.Errorf("domain %q not allowed", c.Hd)
		}
		return c.Email, nil
	}
	if !c.EmailVerified {
		return "", errors.New("email not verified")
	}
	_, domain, _ := strings.Cut(c.Email, "@")
	if domain == "" || !s.domainAllowed(domain) {
		return "", fmt.Errorf("domain %q not allowed", domain)
	}
	return c.Email, nil
}

func (s *server) authenticateUser(r *http.Request) (string, error) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return s.authenticate(r)
	}
	if email, ok := s.checkSSO(r); ok && email != "" {
		return email, nil
	}
	return "", errors.New("missing user session")
}

func (s *server) authenticateForSite(r *http.Request, site string, p policy, scope string) (authIdentity, error) {
	if s.noAuth {
		email := "dev@" + def(s.domain, "example.com")
		return authIdentity{Email: email, Actor: email}, nil
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if strings.HasPrefix(tok, "qk_") {
		for _, st := range p.Tokens {
			if st.Hash == "" || !hmac.Equal([]byte(st.Hash), []byte(s.meta.hashToken(site, tok))) {
				continue
			}
			if st.ExpiresAt != "" {
				exp, err := time.Parse(time.RFC3339, st.ExpiresAt)
				if err != nil || time.Now().After(exp) {
					return authIdentity{}, errors.New("token expired")
				}
			}
			scopes := map[string]bool{}
			for _, sc := range st.Scopes {
				scopes[sc] = true
			}
			if !scopes[scope] {
				return authIdentity{}, errors.New("token missing scope " + scope)
			}
			actor := "token:" + st.Name + "@" + site
			return authIdentity{Email: st.CreatedBy, Actor: actor, Token: true, Site: site, TokenID: st.ID, Scopes: scopes}, nil
		}
		return authIdentity{}, errors.New("invalid token")
	}
	email, err := s.authenticate(r)
	if err != nil {
		return authIdentity{}, err
	}
	return authIdentity{Email: email, Actor: email}, nil
}

func (s *server) markTokenUsed(p *policy, tokenID, by, now string) {
	for i := range p.Tokens {
		if p.Tokens[i].ID == tokenID {
			p.Tokens[i].LastUsedAt = now
			p.Tokens[i].LastUsedBy = by
			return
		}
	}
}

// domainAllowed checks a domain (Google hd or email domain) against QUICK_ALLOWED_DOMAINS:
// empty or "*" (any account), a single domain, or a comma-separated list.
// Mirrors OAUTH2_PROXY_EMAIL_DOMAINS.
func (s *server) domainAllowed(hd string) bool {
	d := strings.TrimSpace(s.domain)
	if d == "" || d == "*" {
		return true
	}
	for part := range strings.SplitSeq(d, ",") {
		if p := strings.TrimSpace(part); p != "" && strings.EqualFold(p, hd) {
			return true
		}
	}
	return false
}

func def(v, d string) string {
	if v != "" {
		return v
	}
	return d
}
