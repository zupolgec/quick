// Per-site policy: lock, public access, code access. Persisted via
// storage.Backend (local file or S3) with a small TTL cache in front, since load
// is on the hot path of every served request.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const codeAccessTTL = 7 * 24 * time.Hour
const maxSiteTokens = 20
const maxViewers = 100 // per "people" site

// Write actions subject to ownership checks.
const (
	actDeploy = "deploy"
	actDelete = "delete"
	actPolicy = "policy"
	actToken  = "token"
)

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// canWrite decides whether email may run action on the site with metadata p,
// per the ownership mode (QUICK_OWNERSHIP) and any explicit lock. The lock
// always wins; then:
//
//	free   (default) anyone can do anything
//	shared           anyone can deploy; only the creator deletes/changes visibility
//	owned            only the creator can do anything
//
// Sites limited to chosen people (access "people") add, in every mode: only
// the creator deletes them, and only the creator changes who can see them
// (visibility or viewer list), or limits someone else's site to chosen
// people. handlePolicy enforces the visibility part (applyPolicy), since it
// depends on the request; the lock still applies on top. Deploys don't
// change: whoever may deploy here may deploy a people site, even one they
// can't open.
func (s *server) canWrite(p policy, email, action string) (bool, string) {
	if p.Locked && p.Owner != "" && !sameUser(p.Owner, email) && !s.isAdmin(email) {
		return false, "site locked by " + p.Owner
	}
	if action == actDelete && p.Access == quick.AccessPeople && !s.actsAsOwner(p, email) {
		return false, "only the owner (" + p.CreatedBy + ") can delete this site"
	}
	creatorOnly := false
	switch s.ownership {
	case "owned":
		creatorOnly = true
	case "shared":
		creatorOnly = action != actDeploy
	}
	if creatorOnly && p.CreatedBy != "" && !s.actsAsOwner(p, email) {
		return false, "site owned by " + p.CreatedBy + " (" + s.ownership + " mode)"
	}
	return true, ""
}

type policy struct {
	CreatedBy string       `json:"created_by,omitempty"`
	CreatedAt string       `json:"created_at,omitempty"`
	UpdatedBy string       `json:"updated_by,omitempty"`
	UpdatedAt string       `json:"updated_at,omitempty"`
	Owner     string       `json:"owner,omitempty"`     // lock owner
	Locked    bool         `json:"locked,omitempty"`    // only the owner can deploy/policy
	Access    string       `json:"access,omitempty"`    // "" = SSO | "public" | "code" | "people"
	CodeHash  string       `json:"code_hash,omitempty"` // HMAC of the code, never plaintext
	Viewers   []string     `json:"viewers,omitempty"`   // "people": lowercased emails, owner excluded
	Tokens    []siteToken  `json:"tokens,omitempty"`
	Domains   []siteDomain `json:"domains,omitempty"`
	SizeLimit int64        `json:"size_limit,omitempty"` // bytes, set by an administrator; 0 = default
	// Index ids of the live and the rollback tree ("" = not indexed), see index.go.
	Version     string `json:"version,omitempty"`
	PrevVersion string `json:"prev_version,omitempty"`
}

type siteToken struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Hash       string   `json:"hash"`
	Scopes     []string `json:"scopes,omitempty"`
	CreatedBy  string   `json:"created_by,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	LastUsedBy string   `json:"last_used_by,omitempty"`
}

type cachedPolicy struct {
	p  policy
	at time.Time
}

type metaStore struct {
	be     storage.Backend
	secret []byte
	ttl    time.Duration
	mu     sync.Mutex
	cache  map[string]cachedPolicy
	// gen counts cache writes and drops per site: a load caches what it read
	// only if no save, fresh load or forget happened meanwhile, so a slow
	// read of old metadata can't overwrite a newer cached policy.
	gen map[string]uint64
}

func newMetaStore(be storage.Backend, secret []byte, ttl time.Duration) *metaStore {
	return &metaStore{be: be, secret: secret, ttl: ttl, cache: map[string]cachedPolicy{}, gen: map[string]uint64{}}
}

// load returns the site policy. It distinguishes three cases: missing metadata
// (legitimately empty policy, cached), present and valid (cached), storage error
// or corrupt JSON (error propagated and NOT cached). Callers on the
// write/serve path must treat the error as fail-closed: an empty policy from an
// error would drop lock and ownership (ownership bypass). Callers that save
// the policy back use loadFresh instead.
func (m *metaStore) load(site string) (policy, error) {
	if !quick.ValidName(site) {
		return policy{}, nil
	}
	m.mu.Lock()
	if c, ok := m.cache[site]; ok && time.Since(c.at) < m.ttl {
		m.mu.Unlock()
		return c.p.clone(), nil
	}
	g := m.gen[site]
	m.mu.Unlock()

	p, err := m.read(site)
	if err != nil {
		return policy{}, err
	}
	m.mu.Lock()
	if m.gen[site] == g {
		m.cache[site] = cachedPolicy{p.clone(), time.Now()}
	}
	m.mu.Unlock()
	return p, nil
}

// loadFresh reads the policy from storage, bypassing the cache. For
// read-modify-write callers (they hold the site lock, so what they read is the
// latest and stays so until they save) and for unlocked re-checks that must
// not trust the cache. It caches what it read only if no save, forget or
// other fresh load happened meanwhile: an unlocked read that stalled must not
// put back a policy a newer save replaced (say, a removed viewer).
func (m *metaStore) loadFresh(site string) (policy, error) {
	if !quick.ValidName(site) {
		return policy{}, nil
	}
	m.mu.Lock()
	g := m.gen[site]
	m.mu.Unlock()
	p, err := m.read(site)
	if err != nil {
		return policy{}, err
	}
	m.mu.Lock()
	if m.gen[site] == g {
		m.gen[site]++
		m.cache[site] = cachedPolicy{p.clone(), time.Now()}
	}
	m.mu.Unlock()
	return p, nil
}

func (m *metaStore) read(site string) (policy, error) {
	b, ok, err := m.be.GetMeta(site)
	if err != nil {
		return policy{}, err
	}
	var p policy
	if ok {
		if err := json.Unmarshal(b, &p); err != nil {
			return policy{}, fmt.Errorf("could not read metadata for %q: %w", site, err)
		}
	}
	return p, nil
}

// put caches p as the newest policy, invalidating loads in flight.
func (m *metaStore) put(site string, p policy) {
	m.mu.Lock()
	m.gen[site]++
	m.cache[site] = cachedPolicy{p.clone(), time.Now()}
	m.mu.Unlock()
}

// clone deep-copies the slices, so a cached policy is never shared with a
// caller that mutates its copy (writers don't hold readers' locks).
func (p policy) clone() policy {
	p.Tokens = slices.Clone(p.Tokens)
	for i := range p.Tokens {
		p.Tokens[i].Scopes = slices.Clone(p.Tokens[i].Scopes)
	}
	p.Domains = slices.Clone(p.Domains)
	p.Viewers = slices.Clone(p.Viewers)
	return p
}

func (m *metaStore) save(site string, p policy) error {
	if !quick.ValidName(site) {
		return errors.New("invalid site name")
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := m.be.PutMeta(site, b); err != nil {
		m.forget(site) // the write may have landed anyway
		return err
	}
	m.put(site, p)
	return nil
}

func (m *metaStore) forget(site string) {
	m.mu.Lock()
	m.gen[site]++
	delete(m.cache, site)
	m.mu.Unlock()
}

func (m *metaStore) hashCode(code string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *metaStore) hashToken(site, token string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte("quick-token|" + site + "|" + token))
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *metaStore) checkCode(p policy, code string) bool {
	if p.CodeHash == "" || code == "" {
		return false
	}
	return hmac.Equal([]byte(p.CodeHash), []byte(m.hashCode(code)))
}

func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// signAccess builds the access cookie value: "<expiry>.<signature>".
func (m *metaStore) signAccess(sub string, exp int64) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(sub + "|" + strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (m *metaStore) validAccessCookie(sub, val string) bool {
	expStr, _, ok := strings.Cut(val, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(m.signAccess(sub, exp)), []byte(val))
}

func (m *metaStore) signCSRF(email string, exp int64) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte("csrf|" + email + "|" + strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (m *metaStore) validCSRF(email, val string) bool {
	expStr, _, ok := strings.Cut(val, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(m.signCSRF(email, exp)), []byte(val))
}

// subOf extracts the first-level subdomain of a base-domain host.
// "foo.quick.example.com" + "quick.example.com" -> "foo".
func subOf(host, base string) string {
	host, _, _ = strings.Cut(host, ":")
	sub, ok := strings.CutSuffix(host, "."+base)
	if !ok || strings.Contains(sub, ".") {
		return ""
	}
	return sub
}

func fwdHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return h
	}
	return r.Host
}

// checkSSO asks oauth2-proxy /oauth2/auth (202 = valid session), forwarding the
// request cookie.
func (s *server) checkSSO(r *http.Request) (string, bool) {
	if s.noAuth {
		return "dev@" + def(s.domain, "example.com"), true
	}
	req, err := http.NewRequest(http.MethodGet, s.oauth2URL+"/oauth2/auth", nil)
	if err != nil {
		return "", false
	}
	if c := r.Header.Get("Cookie"); c != "" {
		req.Header.Set("Cookie", c)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return "", false
	}
	return resp.Header.Get("X-Auth-Request-Email"), true
}

// redirect sends the browser to path (sign_in or code page) with ?rd= set to the
// current URL.
func (s *server) redirect(w http.ResponseWriter, r *http.Request, host, path string) {
	rd := "https://" + host + r.URL.RequestURI()
	http.Redirect(w, r, path+"?rd="+url.QueryEscape(rd), http.StatusFound)
}

func (s *server) handleCodePage(w http.ResponseWriter, r *http.Request, sub string) {
	host := fwdHost(r)
	p, err := s.meta.load(sub)
	if err != nil {
		http.Error(w, "site temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if p.Access != "code" {
		http.Redirect(w, r, "https://"+host+"/", http.StatusFound)
		return
	}
	rd := r.FormValue("rd")
	if !safeRedirect(rd, host) {
		rd = "https://" + host + "/"
	}
	l := pickLang(r)
	if r.Method == http.MethodPost {
		if !s.meta.checkCode(p, r.FormValue("code")) {
			renderCodeForm(w, l, host, rd, true)
			return
		}
		exp := time.Now().Add(codeAccessTTL).Unix()
		http.SetCookie(w, &http.Cookie{
			Name:     "quick_access_" + sub,
			Value:    s.meta.signAccess(sub, exp),
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Unix(exp, 0),
		})
		http.Redirect(w, r, rd, http.StatusFound)
		return
	}
	renderCodeForm(w, l, host, rd, false)
}

// safeRedirect only allows same-host https URLs or relative paths.
func safeRedirect(rd, host string) bool {
	if rd == "" {
		return false
	}
	u, err := url.Parse(rd)
	if err != nil {
		return false
	}
	if u.Host == "" {
		return strings.HasPrefix(rd, "/")
	}
	return u.Scheme == "https" && u.Host == host
}

func (s *server) handleSiteAPI(w http.ResponseWriter, r *http.Request) {
	name, tail, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/site/"), "/")
	action, rest, _ := strings.Cut(tail, "/")
	if !quick.ValidName(name) {
		http.NotFound(w, r)
		return
	}
	switch {
	case action == "policy":
		s.handlePolicy(w, r, name)
	case action == "rollback" && r.Method == http.MethodPost:
		s.handleRollback(w, r, name)
	case action == "tokens":
		s.handleTokens(w, r, name, rest)
	case action == "domains":
		s.handleDomains(w, r, name, rest)
	case action == "limit":
		s.handleLimit(w, r, name)
	case action == "" && r.Method == http.MethodDelete:
		s.handleDelete(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleRollback(w http.ResponseWriter, r *http.Request, name string) {
	email, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	cur, err := s.meta.loadFresh(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(cur, email, actDeploy); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	// Not during a deploy (it would publish over the rollback anyway), unless
	// taken over: the deploy loses the site (lease.go).
	if takeoverRequested(r) {
		if broken := s.leases.clear(name); broken != nil {
			logBroken(name, broken, email)
		}
	} else if err := s.leases.busy(name); err != nil {
		leaseRefused(w, r, err)
		return
	}
	old, err := s.clearVersions(name, &cur, "") // rollback: the site exists, no creator to reserve
	if err != nil {
		http.Error(w, "could not update site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	ok, err := s.store.Rollback(name)
	if (err != nil || !ok) && !errors.Is(err, storage.ErrUncertain) {
		// Rollback leaves the trees as they were when it fails (unless the
		// outcome is unknown: then they stay unindexed until rebuilt).
		s.restoreVersions(name, &cur, old)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no previous version to restore", http.StatusNotFound)
		return
	}
	// The trees swapped places, their indexes with them.
	cur.Version, cur.PrevVersion = old.prev, old.live
	s.rules.forget(name)
	cur.UpdatedBy, cur.UpdatedAt = email, nowStamp()
	if err := s.meta.save(name, cur); err != nil {
		log.Printf("WARNING: rollback %q applied but saving metadata failed: %v", name, err)
		http.Error(w, "rollback applied but saving state failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("rollback %q by %s", name, email)
	_ = json.NewEncoder(w).Encode(quick.RollbackResponse{
		Site: name, RolledBack: true, URL: "https://" + name + "." + s.baseDomain,
	})
}

func (s *server) handlePolicy(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The dashboard posts plain forms with the SSO cookie (and a CSRF token);
	// the CLI sends JSON with an ID token.
	browserForm := r.Method == http.MethodPost && isFormPost(r) &&
		!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
	var email string
	var err error
	if browserForm {
		email, err = s.authenticateUser(r)
	} else {
		email, err = s.authenticate(r)
	}
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if browserForm && !s.meta.validCSRF(email, r.FormValue("csrf")) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	cur, err := s.meta.loadFresh(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		s.writePolicy(w, name, cur, email)
		return
	}
	var req quick.PolicyRequest
	if browserForm {
		var perr *policyErr
		if req, perr = policyFormRequest(r, cur); perr != nil {
			s.renderSitePageError(w, r, name, email, perr)
			return
		}
	} else if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if perr := s.applyPolicy(&cur, email, req); perr != nil {
		if browserForm {
			s.renderSitePageError(w, r, name, email, perr)
			return
		}
		http.Error(w, perr.msg, perr.status)
		return
	}
	if err := s.meta.save(name, cur); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if browserForm {
		http.Redirect(w, r, "/dashboard/site/"+name, http.StatusSeeOther)
		return
	}
	s.writePolicy(w, name, cur, email)
}

// applyPolicy applies req to cur on behalf of email, enforcing the write
// rules (canWrite, lock, chosen people: see canWrite).
func (s *server) applyPolicy(cur *policy, email string, req quick.PolicyRequest) *policyErr {
	viewerOps := req.Viewers != nil || len(req.AddViewers) > 0 || len(req.RemoveViewers) > 0
	access := cur.Access
	if req.Access != nil {
		switch *req.Access {
		case quick.AccessSSO, "sso":
			access = ""
		case quick.AccessPublic, quick.AccessCode, quick.AccessPeople:
			access = *req.Access
		default:
			return &policyErr{status: http.StatusBadRequest, msg: "invalid access"}
		}
	} else if viewerOps {
		access = quick.AccessPeople
	}
	if viewerOps && access != quick.AccessPeople {
		return &policyErr{status: http.StatusBadRequest, msg: "viewers apply only to access=people"}
	}
	changesAccess := req.Access != nil || viewerOps
	if changesAccess && (cur.Access == quick.AccessPeople || access == quick.AccessPeople) {
		if cur.CreatedBy == "" {
			return &policyErr{status: http.StatusConflict, kind: errNoOwner,
				msg: "this site has no recorded owner: it can't be limited to chosen people"}
		}
		if !s.actsAsOwner(*cur, email) {
			return &policyErr{status: http.StatusForbidden, kind: errNotOwner, arg: cur.CreatedBy,
				msg: "only the owner (" + cur.CreatedBy + ") can change who can see this site"}
		}
	}
	if ok, reason := s.canWrite(*cur, email, actPolicy); !ok {
		return &policyErr{status: http.StatusForbidden, msg: reason}
	}
	// Only the creator can change the lock: stops someone from "stealing" another
	// person's site by locking it under their own name (matters in free mode).
	if req.Locked != nil && cur.CreatedBy != "" && !s.actsAsOwner(*cur, email) {
		return &policyErr{status: http.StatusForbidden, msg: "only the creator (" + cur.CreatedBy + ") can lock the site"}
	}
	if changesAccess {
		switch access {
		case "", quick.AccessPublic:
			cur.Access, cur.CodeHash, cur.Viewers = access, "", nil
		case quick.AccessCode:
			if req.Code == nil || *req.Code == "" {
				return &policyErr{status: http.StatusBadRequest, kind: errCodeRequired, msg: "access=code requires a code"}
			}
			cur.Access, cur.CodeHash, cur.Viewers = quick.AccessCode, s.meta.hashCode(*req.Code), nil
		case quick.AccessPeople:
			viewers, perr := s.editViewers(*cur, req)
			if perr != nil {
				return perr
			}
			cur.Access, cur.CodeHash, cur.Viewers = quick.AccessPeople, "", viewers
		}
	}
	if req.Locked != nil {
		if cur.Locked = *req.Locked; cur.Locked {
			cur.Owner = email
		} else {
			cur.Owner = ""
		}
	}
	return nil
}

func (s *server) writePolicy(w http.ResponseWriter, name string, p policy, email string) {
	access := p.Access
	if access == "" {
		access = "sso"
	}
	var viewers []string
	visible := s.mayView(p, email)
	if p.Access == quick.AccessPeople && visible {
		viewers = p.Viewers
	}
	exists, _ := s.store.SiteExists(name)
	_ = json.NewEncoder(w).Encode(quick.PolicyResponse{
		Site: name, Access: access, Locked: p.Locked, Owner: p.Owner, Exists: exists,
		CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt, UpdatedBy: p.UpdatedBy, UpdatedAt: p.UpdatedAt,
		Viewers: viewers, ViewersHidden: !visible,
	})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request, name string) {
	email, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	cur, err := s.meta.loadFresh(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(cur, email, actDelete); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	existed, err := s.store.DeleteSite(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.meta.forget(name)
	s.rules.forget(name)
	if s.domains != nil {
		s.domains.dropSite(name)
	}
	if s.stage != nil {
		s.stage.removeSite(name)
	}
	// A deploy in progress ends here: its commit can't bring the site back.
	if broken := s.leases.clear(name); broken != nil {
		logBroken(name, broken, email)
	}
	if !existed {
		http.Error(w, "site not found", http.StatusNotFound)
		return
	}
	log.Printf("delete %q by %s", name, email)
	_ = json.NewEncoder(w).Encode(quick.DeleteResponse{Site: name, Deleted: true})
}

var codeForm = template.Must(template.New("code").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.CodeTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
  body{display:grid;place-items:center;padding:1.5rem}
  .card{width:min(420px,100%);background:var(--card);padding:2.5rem 2rem 2.25rem}
  .card .brand{margin-bottom:2rem}
  .card .logo{height:2rem}
  h1{font-size:1.75rem;line-height:1.1;margin:0 0 .5rem;color:var(--ink)}
  p{margin:0 0 1.5rem;color:var(--muted);font-size:1rem}
  p b{color:var(--ink);font-weight:500;overflow-wrap:anywhere}
  label{display:block;font-size:.88rem;color:var(--muted);margin:0 0 .4rem}
  input[type=password]{width:100%;height:3rem;padding:0 .9rem;font-size:1.05rem;color:var(--fg);
    background:var(--sunk);border:0;border-radius:0;font-family:var(--font-body)}
  .btn{width:100%;margin-top:1rem;min-height:3rem;font-size:1rem}
  .err{margin-top:1rem;font-size:.92rem;color:var(--err)}
</style></head><body>
<form class="card" method="post" action="/__quick/code">
  ` + brandWordmark + `
  <h1>{{.T.CodeHeading}}</h1>
  <p>{{.T.CodeIntro}} <b>{{.Host}}</b>.</p>
  <input type="hidden" name="rd" value="{{.RD}}">
  <label for="code">{{.T.CodeLabel}}</label>
  <input id="code" type="password" name="code" placeholder="••••••••" autofocus required autocomplete="off">
  <button class="btn" type="submit">{{.T.CodeButton}}</button>
  {{if .Error}}<div class="err">{{.T.CodeError}}</div>{{end}}
</form></body></html>`))

func renderCodeForm(w http.ResponseWriter, l lang, host, rd string, isErr bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept-Language")
	if isErr {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_ = codeForm.Execute(w, map[string]any{
		"Host": host, "RD": rd, "Error": isErr, "Lang": string(l), "T": textsFor(l),
	})
}
