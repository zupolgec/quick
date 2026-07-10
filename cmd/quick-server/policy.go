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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const codeAccessTTL = 7 * 24 * time.Hour
const maxSiteTokens = 20

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
func (s *server) canWrite(p policy, email, action string) (bool, string) {
	if p.Locked && p.Owner != "" && p.Owner != email {
		return false, "site locked by " + p.Owner
	}
	creatorOnly := false
	switch s.ownership {
	case "owned":
		creatorOnly = true
	case "shared":
		creatorOnly = action != actDeploy
	}
	if creatorOnly && p.CreatedBy != "" && p.CreatedBy != email {
		return false, "site owned by " + p.CreatedBy + " (" + s.ownership + " mode)"
	}
	return true, ""
}

type policy struct {
	CreatedBy string      `json:"created_by,omitempty"`
	CreatedAt string      `json:"created_at,omitempty"`
	UpdatedBy string      `json:"updated_by,omitempty"`
	UpdatedAt string      `json:"updated_at,omitempty"`
	Owner     string      `json:"owner,omitempty"`     // lock owner
	Locked    bool        `json:"locked,omitempty"`    // only the owner can deploy/policy
	Access    string      `json:"access,omitempty"`    // "" = SSO | "public" | "code"
	CodeHash  string      `json:"code_hash,omitempty"` // HMAC of the code, never plaintext
	Tokens    []siteToken `json:"tokens,omitempty"`
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
}

func newMetaStore(be storage.Backend, secret []byte, ttl time.Duration) *metaStore {
	return &metaStore{be: be, secret: secret, ttl: ttl, cache: map[string]cachedPolicy{}}
}

// load returns the site policy. It distinguishes three cases: missing metadata
// (legitimately empty policy, cached), present and valid (cached), storage error
// or corrupt JSON (error propagated and NOT cached). Callers on the
// write/serve path must treat the error as fail-closed: an empty policy from an
// error would drop lock and ownership (ownership bypass).
func (m *metaStore) load(site string) (policy, error) {
	if !quick.ValidName(site) {
		return policy{}, nil
	}
	m.mu.Lock()
	if c, ok := m.cache[site]; ok && time.Since(c.at) < m.ttl {
		m.mu.Unlock()
		return c.p, nil
	}
	m.mu.Unlock()

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
	m.mu.Lock()
	m.cache[site] = cachedPolicy{p, time.Now()}
	m.mu.Unlock()
	return p, nil
}

func (m *metaStore) save(site string, p policy) error {
	if !quick.ValidName(site) {
		return errors.New("invalid site name")
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := m.be.PutMeta(site, b); err != nil {
		return err
	}
	m.mu.Lock()
	m.cache[site] = cachedPolicy{p, time.Now()}
	m.mu.Unlock()
	return nil
}

func (m *metaStore) forget(site string) {
	m.mu.Lock()
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

func (s *server) handleCodePage(w http.ResponseWriter, r *http.Request) {
	host := fwdHost(r)
	sub := subOf(host, s.baseDomain)
	if sub == "" {
		http.NotFound(w, r)
		return
	}
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
	cur, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(cur, email, actDeploy); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	ok, err := s.store.Rollback(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no previous version to restore", http.StatusNotFound)
		return
	}
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
	email, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	cur, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		s.writePolicy(w, name, cur)
		return
	}
	if ok, reason := s.canWrite(cur, email, actPolicy); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	var req quick.PolicyRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// Only the creator can change the lock: stops someone from "stealing" another
	// person's site by locking it under their own name (matters in free mode).
	if req.Locked != nil && cur.CreatedBy != "" && cur.CreatedBy != email {
		http.Error(w, "only the creator ("+cur.CreatedBy+") can lock the site", http.StatusForbidden)
		return
	}
	if req.Access != nil {
		switch *req.Access {
		case quick.AccessSSO, "sso":
			cur.Access, cur.CodeHash = "", ""
		case quick.AccessPublic:
			cur.Access, cur.CodeHash = quick.AccessPublic, ""
		case quick.AccessCode:
			if req.Code == nil || *req.Code == "" {
				http.Error(w, "access=code requires a code", http.StatusBadRequest)
				return
			}
			cur.Access, cur.CodeHash = quick.AccessCode, s.meta.hashCode(*req.Code)
		default:
			http.Error(w, "invalid access", http.StatusBadRequest)
			return
		}
	}
	if req.Locked != nil {
		if cur.Locked = *req.Locked; cur.Locked {
			cur.Owner = email
		} else {
			cur.Owner = ""
		}
	}
	if err := s.meta.save(name, cur); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writePolicy(w, name, cur)
}

func (s *server) writePolicy(w http.ResponseWriter, name string, p policy) {
	access := p.Access
	if access == "" {
		access = "sso"
	}
	exists, _ := s.store.SiteExists(name)
	_ = json.NewEncoder(w).Encode(quick.PolicyResponse{
		Site: name, Access: access, Locked: p.Locked, Owner: p.Owner, Exists: exists,
		CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt, UpdatedBy: p.UpdatedBy, UpdatedAt: p.UpdatedAt,
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
	cur, err := s.meta.load(name)
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
  .card{width:min(360px,100%);background:var(--card);border:1px solid var(--border);
    border-radius:18px;padding:2.25rem 1.75rem;
    box-shadow:0 1px 2px rgba(13,24,50,.05),0 18px 48px rgba(13,24,50,.10)}
  .badge{width:46px;height:46px;border-radius:13px;display:grid;place-items:center;
    background:color-mix(in srgb,var(--brand) 10%,transparent);border:1px solid color-mix(in srgb,var(--brand) 22%,transparent);margin-bottom:1.2rem}
  .badge svg{width:22px;height:22px;stroke:var(--brand);fill:none;
    stroke-width:2;stroke-linecap:round;stroke-linejoin:round}
  h1{font-size:1.2rem;font-weight:700;margin:0 0 .35rem;letter-spacing:-.01em;color:var(--ink)}
  p{margin:0 0 1.4rem;color:var(--muted);font-size:.9rem}
  p b{color:var(--ink);font-weight:600;overflow-wrap:anywhere}
  label{display:block;font-size:.8rem;color:var(--muted);margin:0 0 .4rem}
  input[type=password]{width:100%;padding:.72rem .85rem;font-size:1rem;color:var(--fg);
    background:transparent;border:1px solid var(--border);border-radius:11px;outline:none;
    transition:border-color .15s,box-shadow .15s;font-family:var(--font-body)}
  input[type=password]:focus{border-color:var(--ring)}
  .btn{width:100%;margin-top:1.05rem;padding:.8rem;font-size:.95rem}
  .err{margin-top:.9rem;padding:.55rem .7rem;border-radius:9px;font-size:.83rem;
    color:var(--err);background:var(--err-bg)}
</style></head><body>
<form class="card" method="post" action="/__quick/code">
  <div class="badge" aria-hidden="true">
    <svg viewBox="0 0 24 24"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
  </div>
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
