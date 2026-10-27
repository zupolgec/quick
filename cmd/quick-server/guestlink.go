// Outside people on "people" sites: when the server can send email
// (mailer.go), a site's viewer list may hold addresses outside the company
// domains. They can't use the company SSO, so they ask for a sign-in link on
// the site's sign-in page (/__quick/email): the link works once, for
// guestLinkTTL, and leaves a per-site cookie valid for guestSessionTTL. The
// list is checked on every request, so removing someone locks them out at
// once. Company addresses always go through SSO.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

const (
	guestLinkTTL      = 15 * time.Minute
	guestSessionTTL   = 7 * 24 * time.Hour
	guestMailsPerHour = 3   // per site and address
	guestMailsTotal   = 200 // per hour, whole server
)

// isGuest reports whether email is an outside person listed on the site.
func (s *server) isGuest(p policy, email string) bool {
	if s.mail == nil || p.Access != quick.AccessPeople {
		return false
	}
	_, domain, _ := strings.Cut(email, "@")
	return domain != "" && !s.domainAllowed(domain) &&
		slices.ContainsFunc(p.Viewers, func(v string) bool { return sameUser(v, email) })
}

// hasGuests reports whether the sign-in page should offer the email link.
func (s *server) hasGuests(p policy) bool {
	return slices.ContainsFunc(p.Viewers, func(v string) bool { return s.isGuest(p, v) })
}

func guestCookieName(sub string) string { return "quick_guest_" + sub }

// signGuest builds the guest cookie value: "<expiry>.<email b64>.<signature>".
func (m *metaStore) signGuest(sub, email string, exp int64) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte("guest|" + sub + "|" + email + "|" + strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString([]byte(email)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// guestFromCookie returns the outside person signed in on the site, if the
// cookie is valid and they're still on its list.
func (s *server) guestFromCookie(r *http.Request, sub string, p policy) (string, bool) {
	if s.mail == nil || p.Access != quick.AccessPeople {
		return "", false
	}
	c, err := r.Cookie(guestCookieName(sub))
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	raw, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || err2 != nil || time.Now().Unix() > exp {
		return "", false
	}
	email := string(raw)
	if !hmac.Equal([]byte(s.meta.signGuest(sub, email, exp)), []byte(c.Value)) || !s.isGuest(p, email) {
		return "", false
	}
	return email, true
}

// guestLinks holds the links sent and not used yet, and the sending limits.
// In memory: a restart only voids links younger than guestLinkTTL. Zero
// value ready to use.
type guestLinks struct {
	mu      sync.Mutex
	now     func() time.Time // nil = time.Now (tests)
	pending map[string]guestLink
	sent    map[string][]time.Time // site|email -> sends in the last hour
	total   []time.Time
}

type guestLink struct {
	site, email, rd string
	exp             time.Time
}

func (g *guestLinks) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func lastHour(ts []time.Time, now time.Time) []time.Time {
	return slices.DeleteFunc(ts, func(t time.Time) bool { return now.Sub(t) >= time.Hour })
}

// issue creates a link for email on site, unless a sending limit is reached.
func (g *guestLinks) issue(site, email, rd string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if g.pending == nil {
		g.pending, g.sent = map[string]guestLink{}, map[string][]time.Time{}
	}
	for t, l := range g.pending {
		if now.After(l.exp) {
			delete(g.pending, t)
		}
	}
	key := site + "|" + email
	g.total = lastHour(g.total, now)
	g.sent[key] = lastHour(g.sent[key], now)
	if len(g.sent[key]) >= guestMailsPerHour || len(g.total) >= guestMailsTotal {
		return "", false
	}
	for k, ts := range g.sent {
		if len(lastHour(ts, now)) == 0 {
			delete(g.sent, k)
		}
	}
	g.sent[key] = append(g.sent[key], now)
	g.total = append(g.total, now)
	tok := randomID(32)
	g.pending[tok] = guestLink{site: site, email: email, rd: rd, exp: now.Add(guestLinkTTL)}
	return tok, true
}

// use spends a link of site: it works once, before it expires.
func (g *guestLinks) use(tok, site string) (guestLink, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.pending[tok]
	if !ok || l.site != site {
		return guestLink{}, false
	}
	delete(g.pending, tok)
	return l, !g.clock().After(l.exp)
}

// handleGuestLink serves /__quick/email on a site host:
//
//	POST email=...  asks for a link (same answer whoever asks)
//	GET  t=...      the link from the email: a page with a button, since mail
//	                scanners open links and would spend it
//	POST t=...      spends the link and signs the visitor in
func (s *server) handleGuestLink(w http.ResponseWriter, r *http.Request, sub string) {
	host := fwdHost(r)
	p, err := s.meta.load(sub)
	if err != nil {
		http.Error(w, "site temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	l := pickLang(r)
	t := textsFor(l)
	page := map[string]any{"Host": host, "Lang": string(l), "T": t, "Site": "https://" + host + "/"}
	render := func(status int, mode string) {
		page["Mode"] = mode
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Add("Vary", "Accept-Language")
		w.WriteHeader(status)
		_ = guestPage.Execute(w, page)
	}
	tok := r.FormValue("t")
	switch {
	case r.Method == http.MethodGet && tok != "":
		page["Token"] = tok
		render(http.StatusOK, "confirm")
	case r.Method == http.MethodPost && tok != "":
		link, ok := s.guests.use(tok, sub)
		if !ok || !s.isGuest(p, link.email) {
			render(http.StatusGone, "invalid")
			return
		}
		exp := time.Now().Add(guestSessionTTL).Unix()
		http.SetCookie(w, &http.Cookie{
			Name: guestCookieName(sub), Value: s.meta.signGuest(sub, link.email, exp),
			Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: time.Unix(exp, 0),
		})
		log.Printf("guest %s signed in on %q", link.email, sub)
		http.Redirect(w, r, link.rd, http.StatusFound)
	case r.Method == http.MethodPost:
		rd := r.FormValue("rd")
		if !safeRedirect(rd, host) {
			rd = "https://" + host + "/"
		}
		email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
		if s.isGuest(p, email) {
			if tok, ok := s.guests.issue(sub, email, rd); ok {
				link := "https://" + host + "/__quick/email?t=" + tok
				msg, err := guestMail(t, l, email, host, link)
				go func() {
					if err == nil {
						err = s.mail.Send(msg)
					}
					if err != nil {
						log.Printf("guest link for %s on %q: %v", email, sub, err)
					}
				}()
			}
		}
		render(http.StatusOK, "sent")
	default:
		http.Redirect(w, r, "https://"+host+"/", http.StatusFound)
	}
}

// guestMail is the email with the sign-in link, as text and HTML.
func guestMail(t uiText, l lang, to, host, link string) (mailMessage, error) {
	intro := fmt.Sprintf(t.GuestMailIntro, host)
	text := t.GuestMailHello + "\n\n" + intro + "\n\n" + link + "\n\n" + t.GuestMailValid + "\n\n" + t.GuestMailIgnore + "\n\n-- \nquick · " + host + "\n"
	var html strings.Builder
	err := guestMailHTML.Execute(&html, map[string]any{
		"Lang": string(l), "T": t, "Intro": intro, "Link": template.URL(link), "Host": host,
	})
	return mailMessage{To: to, Subject: fmt.Sprintf(t.GuestMailSubject, host), Text: text, HTML: html.String()}, err
}

// Email clients ignore most CSS: tables and inline styles only.
var guestMailHTML = template.Must(template.New("guestmail").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.T.GuestMailButton}}</title></head>
<body style="margin:0;padding:0;background:#e8e6fd">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#e8e6fd">
<tr><td align="center" style="padding:32px 16px">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:480px;background:#ffffff">
<tr><td style="padding:36px 32px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;color:#1a2238;font-size:16px;line-height:1.5">
<p style="margin:0 0 24px;font-size:22px;font-weight:700;color:#1a2238">quick</p>
<p style="margin:0 0 8px">{{.T.GuestMailHello}}</p>
<p style="margin:0 0 24px">{{.Intro}}</p>
<table role="presentation" cellpadding="0" cellspacing="0"><tr><td style="background:#4a3ddb">
<a href="{{.Link}}" style="display:inline-block;padding:14px 28px;color:#ffffff;text-decoration:none;font-weight:600">{{.T.GuestMailButton}}</a>
</td></tr></table>
<p style="margin:24px 0 8px;font-size:14px;color:#5b5878">{{.T.GuestMailValid}}</p>
<p style="margin:0 0 24px;font-size:14px;color:#5b5878">{{.T.GuestMailIgnore}}</p>
<p style="margin:0 0 4px;font-size:13px;color:#5b5878">{{.T.GuestMailFallback}}</p>
<p style="margin:0;font-size:13px;word-break:break-all"><a href="{{.Link}}" style="color:#4a3ddb">{{.Link}}</a></p>
</td></tr></table>
<p style="margin:16px 0 0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:12px;color:#5b5878">quick · {{.Host}}</p>
</td></tr></table>
</body></html>`))

// guestForm is the email link form shown under the SSO button on sites with
// outside people.
const guestForm = `{{if .Guests}}
  <form class="guest" method="post" action="/__quick/email">
    <h2>{{.T.GuestOr}}</h2>
    <p>{{.T.GuestIntro}}</p>
    <input type="hidden" name="rd" value="{{.RD}}">
    <label for="guest-email">{{.T.GuestEmail}}</label>
    <input id="guest-email" type="email" name="email" autocomplete="email" required>
    <button class="btn" type="submit">{{.T.GuestSend}}</button>
  </form>{{end}}`

const guestFormCSS = `
.guest{margin-top:2.75rem}
.guest h2{font-size:1.05rem;margin:0 0 .35rem;color:var(--ink)}
.guest p{margin:0 0 1rem;font-size:.92rem}
.guest label{display:block;font-size:.88rem;color:var(--muted);margin:0 0 .4rem}
.guest input[type=email]{width:100%;height:3rem;padding:0 .9rem;font-size:1rem;color:var(--fg);
  background:var(--sunk);border:0;border-radius:0;font-family:var(--font-body)}
.guest .btn{margin-top:.75rem}`

var guestPage = template.Must(template.New("guest").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.GuestTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
body{display:grid;place-items:center;padding:1.5rem}
.card{width:min(420px,100%);background:var(--card);padding:2.5rem 2rem 2.25rem}
.card .brand{margin-bottom:2rem}
.card .logo{height:2rem}
h1{font-size:1.75rem;line-height:1.1;margin:0 0 .5rem;color:var(--ink)}
p{margin:0 0 1.75rem;color:var(--muted);font-size:1rem;overflow-wrap:anywhere}
p b{color:var(--ink);font-weight:500}
p:last-child{margin-bottom:0}
.btn{width:100%;min-height:3rem;font-size:1rem}
</style></head><body>
<div class="card">
  ` + brandWordmark + `
  {{if eq .Mode "confirm"}}<h1>{{.T.GuestConfirmHeading}}</h1>
  <p>{{.T.GuestConfirmIntro}} <b>{{.Host}}</b>. {{.T.GuestConfirmStay}}</p>
  <form method="post" action="/__quick/email"><input type="hidden" name="t" value="{{.Token}}">
  <button class="btn" type="submit">{{.T.GuestConfirmButton}}</button></form>
  {{else if eq .Mode "invalid"}}<h1>{{.T.GuestInvalidHeading}}</h1>
  <p>{{.T.GuestInvalidIntro}}</p>
  <a class="btn" href="{{.Site}}">{{.T.GuestInvalidButton}}</a>
  {{else}}<h1>{{.T.GuestSentHeading}}</h1>
  <p>{{.T.GuestSentA}} <b>{{.Host}}</b>{{.T.GuestSentB}}</p>{{end}}
</div></body></html>`))
