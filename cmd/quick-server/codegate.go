// Sites with an access code. The visitor types the code on /__quick/code, or
// opens a link that carries it (?quick_code=...): either way they get a
// per-site cookie valid for codeAccessTTL, and the code leaves the address.
// The cookie is tied to the code, so changing it signs everyone out. The code
// is kept readable for those who manage the site, so they can share it again. Each
// visitor gets codeFailuresMax wrong codes per codeFailuresWindow, across
// sites, and codes are at least codeMinLength characters.
package main

import (
	"crypto/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

const (
	codeParam          = "quick_code"
	codeMinLength      = 8
	codeFailuresMax    = 10
	codeFailuresWindow = 15 * time.Minute
)

// visibleCode is the site's code for email, if they can change it ("" for
// anyone else, and on sites whose code was stored before it was kept).
func (s *server) visibleCode(p policy, email string) string {
	if ok, _ := s.canWrite(p, email, actPolicy); !ok || p.Access != quick.AccessCode {
		return ""
	}
	return p.Code
}

// teamManages reports whether anyone at the company may change the site's
// access (and read its code), as canWrite decides: unless someone locked it,
// in free mode, or on a site with no recorded creator in any mode.
func (s *server) teamManages(p policy) bool {
	if p.Locked && p.Owner != "" {
		return false
	}
	return s.ownership == "" || s.ownership == "free" || p.CreatedBy == ""
}

// teamNeedsCode reports whether colleagues signed in with the company SSO
// need the code too. By default they don't; a site can ask them for it only
// when they couldn't read or change it anyway.
func (s *server) teamNeedsCode(p policy) bool {
	return p.Access == quick.AccessCode && p.CodeOnly && !s.teamManages(p)
}

// openCodeSite reports whether a visitor without a valid code cookie gets
// into a code site anyway: colleagues signed in with the company SSO, or on a
// site that asks them for the code too, only the administrators (who can read
// the code anyway). email is the SSO account, if any.
func (s *server) openCodeSite(r *http.Request, p policy) (email string, signedIn, ok bool) {
	email, signedIn = s.checkSSO(r)
	return email, signedIn, signedIn && (!s.teamNeedsCode(p) || s.isAdmin(email))
}

// teamSignIn is the company sign-in link for the code page, "" where it
// doesn't apply: the site asks the team for the code, or it's on a custom
// domain (the SSO session lives on the base domain).
func (s *server) teamSignIn(p policy, host, rd string) string {
	if s.teamNeedsCode(p) || !strings.HasSuffix(host, "."+s.baseDomain) {
		return ""
	}
	return "https://" + s.baseDomain + "/oauth2/sign_in?rd=" + url.QueryEscape(rd)
}

// codeLink is the address that opens a code site without typing the code.
func codeLink(siteURL, code string) string {
	return siteURL + "/?" + codeParam + "=" + url.QueryEscape(code)
}

// codeFromLink handles a request that carries the code in its address. It
// reports false when there's nothing to do: no code, or not a code site.
func (s *server) codeFromLink(w http.ResponseWriter, r *http.Request, sub string) bool {
	if !r.URL.Query().Has(codeParam) {
		return false
	}
	p, err := s.meta.load(sub)
	if err != nil || p.Access != quick.AccessCode {
		return false
	}
	var code string
	var keep []string
	for part := range strings.SplitSeq(r.URL.RawQuery, "&") {
		k, v, _ := strings.Cut(part, "=")
		if k, _ := url.QueryUnescape(k); k == codeParam {
			code, _ = url.QueryUnescape(v)
			continue
		}
		keep = append(keep, part)
	}
	clean := *r.URL
	clean.RawQuery = strings.Join(keep, "&")
	s.enterCode(w, r, sub, p, code, "https://"+fwdHost(r)+clean.RequestURI())
	return true
}

// enterCode checks a code typed or carried by a link: if it's right it sets
// the access cookie and goes on to rd, otherwise it shows the form again.
func (s *server) enterCode(w http.ResponseWriter, r *http.Request, sub string, p policy, code, rd string) {
	host, l, ip := fwdHost(r), pickLang(r), clientIP(r)
	signIn := s.teamSignIn(p, host, rd)
	if s.codeTries.blocked(ip) {
		renderCodeForm(w, l, host, rd, signIn, http.StatusTooManyRequests)
		return
	}
	if !s.meta.checkCode(p, code) {
		s.codeTries.fail(ip)
		renderCodeForm(w, l, host, rd, signIn, http.StatusUnauthorized)
		return
	}
	exp := time.Now().Add(codeAccessTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     "quick_access_" + sub,
		Value:    s.meta.signAccess(sub, p.CodeHash, exp),
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(exp, 0),
	})
	http.Redirect(w, r, rd, http.StatusFound)
}

// clientIP is the visitor's address: the proxy in front of quick sets
// X-Forwarded-For, its last entry is the one it saw.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(xff[strings.LastIndex(xff, ",")+1:])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// codeAttempts counts wrong codes per visitor address.
type codeAttempts struct {
	mu    sync.Mutex
	fails map[string][]time.Time
	now   func() time.Time // tests
}

func (c *codeAttempts) recent(ip string) []time.Time {
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	for k, ts := range c.fails {
		if ts[len(ts)-1].Before(now.Add(-codeFailuresWindow)) {
			delete(c.fails, k)
		}
	}
	ts := c.fails[ip]
	for len(ts) > 0 && ts[0].Before(now.Add(-codeFailuresWindow)) {
		ts = ts[1:]
	}
	return ts
}

func (c *codeAttempts) blocked(ip string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recent(ip)) >= codeFailuresMax
}

func (c *codeAttempts) fail(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fails == nil {
		c.fails = map[string][]time.Time{}
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	c.fails[ip] = append(c.recent(ip), now)
}

// genCode creates a short, readable access code (no ambiguous characters),
// for the dashboard when the owner leaves the code empty.
func genCode() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, codeMinLength)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
