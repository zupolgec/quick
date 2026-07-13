// quick-server is the single front for *.<BASE_DOMAIN>: it serves sites from
// storage, enforces per-site policy (SSO / public / code), receives deploys and
// proxies SSO to oauth2-proxy. All config comes from env vars; nothing
// domain-specific is hardcoded.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const maxUpload = 200 << 20 // 200 MiB per deploy

// Timeout so a hung dependency (Google tokeninfo, oauth2-proxy) can't pin a
// request goroutine.
var httpClient = &http.Client{Timeout: 10 * time.Second}

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
	noAuth       bool       // local development only
	locks        keyedMutex // serializes per-site writes (single instance)
}

type authIdentity struct {
	Email   string
	Actor   string
	Token   bool
	Site    string
	TokenID string
	Scopes  map[string]bool
}

func (a authIdentity) hasScope(scope string) bool {
	return a.Scopes != nil && a.Scopes[scope]
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
	}
	if err := s.validateConfig(metaSecret); err != nil {
		log.Fatal(err)
	}
	s.meta = newMetaStore(store, []byte(metaSecret), 5*time.Second)
	s.rules = newRulesStore(store, 5*time.Second)
	s.setupSiteProxy()
	if err := s.setupOAuthProxy(); err != nil {
		log.Fatal(err)
	}
	s.apexMux = s.buildApexMux()

	addr := quick.Env("QUICK_ADDR", ":8080")
	log.Printf("quick-server on %s (base=%s, storage=%s, ownership=%s, noauth=%v)", addr, s.baseDomain, quick.Env("QUICK_STORAGE", "local"), s.ownership, s.noAuth)
	log.Fatal(http.ListenAndServe(addr, http.HandlerFunc(s.route)))
}

// route dispatches by host: the apex (== baseDomain) is the control plane (API,
// auth, dashboard); every subdomain is just a site.
func (s *server) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/health" {
		fmt.Fprintln(w, "ok")
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
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/__quick/code" {
		s.handleCodePage(w, r)
		return
	}
	s.handleSite(w, r) // per-site gate + serve
}

func (s *server) buildApexMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	m.HandleFunc("/api/config", s.handleConfig)
	m.HandleFunc("/api/deploy", s.handleDeploy)
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
	if strings.TrimSpace(s.domain) == "*" {
		log.Print(`⚠ QUICK_ALLOWED_DOMAINS="*": any Google account can deploy`)
	}
	return nil
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
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("name")
	if !quick.ValidName(name) {
		http.Error(w, "invalid site name (use a-z, 0-9, hyphen)", http.StatusBadRequest)
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	cur, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	ident, err := s.authenticateForSite(r, name, cur, quick.TokenScopeDeploy)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if ok, reason := s.canWrite(cur, ident.Email, actDeploy); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxUpload)
	gz, err := gzip.NewReader(body)
	if err != nil {
		http.Error(w, "gzip: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.PutSite(name, tar.NewReader(gz)); err != nil {
		http.Error(w, "deploy: "+err.Error(), http.StatusBadRequest)
		return
	}
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
	log.Printf("deploy %q by %s", name, by)
	_ = json.NewEncoder(w).Encode(quick.DeployResponse{
		Site: name,
		URL:  "https://" + name + "." + s.baseDomain,
		By:   by,
	})
}

// authenticate validates a Google ID token (Authorization: Bearer) and returns
// the email, verifying hosted domain and (if set) audience. Quick deploy tokens
// are intentionally not accepted here; token management, delete and policy
// changes require a real SSO/Google user.
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
	resp, err := httpClient.Get("https://oauth2.googleapis.com/tokeninfo?id_token=" + tok)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("invalid token")
	}
	var info struct {
		Email string `json:"email"`
		Hd    string `json:"hd"`
		Aud   string `json:"aud"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	if !s.domainAllowed(info.Hd) {
		return "", fmt.Errorf("domain %q not allowed", info.Hd)
	}
	if s.clientID != "" && info.Aud != s.clientID {
		return "", errors.New("token audience does not match")
	}
	return info.Email, nil
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

// domainAllowed checks the Google hosted domain against QUICK_ALLOWED_DOMAINS:
// empty or "*" (any account), a single domain, or a comma-separated list.
// Mirrors OAUTH2_PROXY_EMAIL_DOMAINS.
func (s *server) domainAllowed(hd string) bool {
	d := strings.TrimSpace(s.domain)
	if d == "" || d == "*" {
		return true
	}
	for part := range strings.SplitSeq(d, ",") {
		if strings.TrimSpace(part) == hd {
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
