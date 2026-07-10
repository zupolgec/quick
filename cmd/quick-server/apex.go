// The apex (== BASE_DOMAIN) is the control plane: control API, auth, and a page
// that shows branding + SSO sign-in for guests and the sites dashboard for
// authenticated users. Subdomains are just sites.
package main

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

// renderSSOPage shows the sign-in page (no bare redirect): branding + SSO
// button. The button goes to sign_in on the apex, which after Google login
// returns to rd. TODO multi-provider: Google only for now.
func (s *server) renderSSOPage(w http.ResponseWriter, r *http.Request, host string) {
	rd := "https://" + host + r.URL.RequestURI()
	signIn := "https://" + s.baseDomain + "/oauth2/sign_in?rd=" + url.QueryEscape(rd)
	l := pickLang(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept-Language")
	_ = ssoPage.Execute(w, map[string]any{
		"Host": host, "SignIn": signIn, "Lang": string(l), "T": textsFor(l),
	})
}

// handleApexRoot serves the public landing (install + usage) at "/". The
// dashboard lives at /dashboard.
func (s *server) handleApexRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r) // the apex serves no sites
		return
	}
	l := pickLang(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept-Language")
	_ = landingPage.Execute(w, map[string]any{
		"Lang": string(l), "T": textsFor(l), "Base": s.baseDomain,
		"Install":    "curl -fsSL https://" + s.baseDomain + "/install.sh | sh",
		"InstallWin": "irm https://" + s.baseDomain + "/install.ps1 | iex",
		"Login":      "quick login --server https://" + s.baseDomain,
		"Deploy":     "quick deploy <name> ./folder",
	})
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	email, ok := s.checkSSO(r)
	if !ok {
		s.renderSSOPage(w, r, s.baseDomain)
		return
	}
	s.renderDashboard(w, pickLang(r), email)
}

func (s *server) handleDashboardSite(w http.ResponseWriter, r *http.Request) {
	email, ok := s.checkSSO(r)
	if !ok {
		s.renderSSOPage(w, r, s.baseDomain)
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/dashboard/site/"), "/")
	if !quick.ValidName(name) {
		http.NotFound(w, r)
		return
	}
	s.renderDashboardSitePage(w, r, name, email, "")
}

func (s *server) renderDashboardSitePage(w http.ResponseWriter, r *http.Request, name, email, reveal string) {
	p, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(p, email, actToken); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	exists, _ := s.store.SiteExists(name)
	access := p.Access
	if access == "" {
		access = "sso"
	}
	csrf := s.meta.signCSRF(email, time.Now().Add(2*time.Hour).Unix())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept-Language")
	_ = dashboardSitePage.Execute(w, map[string]any{
		"Email": email, "Name": name, "URL": "https://" + name + "." + s.baseDomain,
		"Base": s.baseDomain, "Access": badgeLabel(access, textsFor(pickLang(r))),
		"Locked": p.Locked, "Updated": updatedLabel(p), "Exists": exists,
		"Tokens": tokenInfos(p.Tokens), "Reveal": reveal, "CSRF": csrf,
		"Lang": string(pickLang(r)), "T": textsFor(pickLang(r)),
	})
}

func (s *server) handleSites(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.authenticate(r); err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	names, err := s.store.ListSites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]quick.SiteInfo, 0, len(names))
	for _, n := range names {
		p, _ := s.meta.load(n) // listing is best-effort; empty policy on error (display only)
		out = append(out, s.siteInfo(n, p))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(quick.SitesResponse{Sites: out})
}

func (s *server) siteInfo(name string, p policy) quick.SiteInfo {
	access := p.Access
	if access == "" {
		access = "sso"
	}
	return quick.SiteInfo{
		Site: name, URL: "https://" + name + "." + s.baseDomain,
		Access: access, Locked: p.Locked, Owner: p.Owner,
		CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt,
		UpdatedBy: p.UpdatedBy, UpdatedAt: p.UpdatedAt,
	}
}

type dashRow struct {
	Name    string
	URL     string
	Manage  string
	Badge   string // localized access label
	Locked  bool
	Updated string // "who · when", preformatted
	at      string // raw RFC3339, for sorting
	Mine    bool
}

func (s *server) renderDashboard(w http.ResponseWriter, l lang, email string) {
	t := textsFor(l)
	names, _ := s.store.ListSites()
	rows := make([]dashRow, 0, len(names))
	for _, n := range names {
		p, _ := s.meta.load(n) // dashboard is best-effort; empty policy on error (display only)
		access := p.Access
		if access == "" {
			access = "sso"
		}
		rows = append(rows, dashRow{
			Name:    n,
			URL:     "https://" + n + "." + s.baseDomain,
			Manage:  "/dashboard/site/" + n,
			Badge:   badgeLabel(access, t),
			Locked:  p.Locked,
			Updated: updatedLabel(p),
			at:      p.UpdatedAt,
			Mine:    p.CreatedBy != "" && p.CreatedBy == email,
		})
	}
	// Most recent first; rows without a timestamp sink to the bottom, by name.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].at != rows[j].at {
			return rows[i].at > rows[j].at
		}
		return rows[i].Name < rows[j].Name
	})

	var mine, all []dashRow
	for _, r := range rows {
		if r.Mine {
			mine = append(mine, r)
		}
		all = append(all, r)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept-Language")
	_ = dashboardPage.Execute(w, map[string]any{
		"Email": email, "Mine": mine, "All": all, "Base": s.baseDomain,
		"Lang": string(l), "T": t,
	})
}

func updatedLabel(p policy) string {
	if p.UpdatedBy == "" && p.UpdatedAt == "" {
		return "—"
	}
	when := p.UpdatedAt
	if t, err := time.Parse(time.RFC3339, p.UpdatedAt); err == nil {
		when = t.Format("2006-01-02 15:04")
	}
	if p.UpdatedBy == "" {
		return when
	}
	return p.UpdatedBy + " · " + when
}

var ssoPage = template.Must(template.New("sso").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.SSOTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
body{display:grid;place-items:center;padding:1.5rem}
.card{width:min(360px,100%);background:var(--card);border:1px solid var(--border);border-radius:18px;padding:2.25rem 1.75rem;text-align:center;box-shadow:0 1px 2px rgba(13,24,50,.05),0 18px 48px rgba(13,24,50,.10)}
.card .brand{font-size:1.5rem;margin-bottom:.9rem}
h1{font-size:1.15rem;font-weight:700;margin:0 0 .35rem;color:var(--ink)}
p{margin:0 0 1.6rem;color:var(--muted);font-size:.9rem;overflow-wrap:anywhere}
.btn{width:100%;padding:.8rem;font-size:.95rem}
</style></head><body>
<div class="card">
  ` + brandWordmark + `
  <h1>{{.T.SSOHeading}}</h1>
  <p>{{.T.SSOIntro}} <b>{{.Host}}</b>.</p>
  <a class="btn" href="{{.SignIn}}">{{.T.SSOButton}}</a>
</div></body></html>`))

var dashboardPage = template.Must(template.New("dash").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.DashTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
.wrap{max-width:780px;margin:0 auto;padding:2rem 1.25rem}
header{display:flex;justify-content:space-between;align-items:center;margin-bottom:1.5rem}
.who{color:var(--muted);font-size:.85rem}
h2{font-size:.8rem;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);margin:1.8rem 0 .6rem}
.site{display:flex;justify-content:space-between;align-items:center;gap:1rem;padding:.7rem .9rem;background:var(--card);border:1px solid var(--border);border-radius:12px;margin-bottom:.5rem}
.site .name{font-weight:600;font-family:var(--font-head)}
.site .meta{color:var(--muted);font-size:.8rem}
.site .actions{display:flex;align-items:center;gap:.55rem;flex:none}
.manage{font-size:.78rem;color:var(--brand);font-weight:700}
.tag{font-size:.7rem;border:1px solid var(--border);border-radius:999px;padding:.1rem .5rem;color:var(--muted);margin-left:.4rem}
.empty{color:var(--muted);font-size:.9rem}
.help{margin-top:2.5rem;padding-top:1.5rem;border-top:1px solid var(--border);color:var(--muted);font-size:.85rem}
code{background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:.1rem .35rem;font-size:.85em;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
</style></head><body>
<div class="wrap">
  <header>` + brandLink + `<div class="who">{{.Email}}</div></header>

  <h2>{{.T.DashYourSites}}</h2>
  {{if .Mine}}{{range .Mine}}
  <div class="site">
    <div><span class="name"><a href="{{.URL}}">{{.Name}}</a></span><span class="tag">{{.Badge}}</span>{{if .Locked}}<span class="tag">{{$.T.DashLocked}}</span>{{end}}</div>
    <div class="actions"><div class="meta">{{.Updated}}</div><a class="manage" href="{{.Manage}}">Manage</a></div>
  </div>{{end}}{{else}}<p class="empty">{{.T.DashEmptyMine}} <code>quick deploy</code>.</p>{{end}}

  <h2>{{.T.DashAllSites}}</h2>
  {{if .All}}{{range .All}}
  <div class="site">
    <div><span class="name"><a href="{{.URL}}">{{.Name}}</a></span><span class="tag">{{.Badge}}</span>{{if .Locked}}<span class="tag">{{$.T.DashLocked}}</span>{{end}}</div>
    <div class="actions"><div class="meta">{{.Updated}}</div><a class="manage" href="{{.Manage}}">Manage</a></div>
  </div>{{end}}{{else}}<p class="empty">{{.T.DashEmptyAll}}</p>{{end}}

  <div class="help">
    {{.T.DashHelpPublish}} <code>quick deploy &lt;name&gt; ./folder</code> → <code>&lt;name&gt;.{{.Base}}</code>.
    {{.T.DashHelpInstall}} <code>go install github.com/zupolgec/quick/cmd/quick@latest</code>.
  </div>
</div></body></html>`))

var dashboardSitePage = template.Must(template.New("dash-site").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} deploy tokens</title>` + brandHead + `
<style>` + brandCSS + `
.wrap{max-width:860px;margin:0 auto;padding:2rem 1.25rem}
header{display:flex;justify-content:space-between;align-items:center;gap:1rem;margin-bottom:1.5rem}
.who{color:var(--muted);font-size:.85rem;overflow-wrap:anywhere;text-align:right}
.back{display:inline-flex;margin-bottom:1.1rem;font-size:.86rem;font-weight:700;color:var(--brand)}
.top{display:flex;justify-content:space-between;align-items:flex-start;gap:1.5rem;margin-bottom:1.75rem}
h1{font-size:1.75rem;font-weight:700;letter-spacing:-.02em;margin:0;color:var(--ink);overflow-wrap:anywhere}
.url{color:var(--muted);font-size:.92rem;margin-top:.3rem;overflow-wrap:anywhere}
.tags{display:flex;gap:.4rem;flex-wrap:wrap;justify-content:flex-end}
.tag{font-size:.72rem;border:1px solid var(--border);border-radius:999px;padding:.12rem .5rem;color:var(--muted)}
.panel{border-top:1px solid var(--border);padding-top:1.35rem;margin-top:1.35rem}
.panel h2{font-size:1rem;font-weight:700;margin:0 0 .35rem;color:var(--ink)}
.hint{color:var(--muted);font-size:.9rem;margin:.15rem 0 1rem;max-width:64ch}
.token{display:flex;justify-content:space-between;align-items:center;gap:1rem;padding:.75rem 0;border-top:1px solid var(--border)}
.token:first-of-type{border-top:0}
.token-main{min-width:0}
.token-name{font-weight:700;color:var(--ink);overflow-wrap:anywhere}
.token-meta{color:var(--muted);font-size:.82rem;margin-top:.18rem;overflow-wrap:anywhere}
.empty{color:var(--muted);font-size:.9rem;margin:.8rem 0 0}
form.create{display:flex;flex-wrap:wrap;align-items:end;gap:.65rem;margin-top:1rem}
.field{display:grid;gap:.32rem}
.field.name{flex:1 1 220px}
label{font-size:.78rem;color:var(--muted)}
input,select{height:36px;border:1px solid var(--border);border-radius:9px;background:var(--card);color:var(--fg);font:inherit;font-size:.9rem;padding:0 .7rem}
input:focus,select:focus{outline:2px solid var(--ring);outline-offset:0}
.btn,.btn-secondary{height:36px;border-radius:9px;border:1px solid var(--border);font-family:var(--font-head);font-size:.86rem;font-weight:700;padding:0 .75rem;cursor:pointer}
.btn{border-color:var(--btn);background:var(--btn);color:var(--btn-fg)}
.btn-secondary{background:transparent;color:var(--muted)}
.reveal{border:1px solid color-mix(in srgb,var(--ok) 28%,var(--border));background:color-mix(in srgb,var(--ok) 8%,transparent);border-radius:12px;padding:1rem;margin:0 0 1.35rem}
.reveal p{margin:.25rem 0 .75rem;color:var(--muted);font-size:.9rem}
.secret{display:flex;align-items:center;gap:.5rem}
.secret code{flex:1;min-width:0;background:var(--card);border:1px solid var(--border);border-radius:9px;padding:.65rem .75rem;overflow-x:auto;white-space:nowrap;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.84rem}
code.inline{background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:.1rem .35rem;font-size:.85em;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
@media(max-width:680px){.top{display:block}.tags{justify-content:flex-start;margin-top:.8rem}.token{align-items:flex-start}.who{text-align:left}.secret{display:block}.secret .btn-secondary{margin-top:.55rem}form.create{display:grid}.btn{width:100%}}
</style></head><body>
<div class="wrap">
  <header>` + brandLink + `<div class="who">{{.Email}}</div></header>
  <a class="back" href="/dashboard">Back to dashboard</a>
  <div class="top">
    <div><h1>{{.Name}}</h1><div class="url"><a href="{{.URL}}">{{.URL}}</a></div></div>
    <div class="tags"><span class="tag">{{.Access}}</span>{{if .Locked}}<span class="tag">Locked</span>{{end}}{{if not .Exists}}<span class="tag">No files</span>{{end}}</div>
  </div>

  {{if .Reveal}}<div class="reveal">
    <strong>Deploy token created.</strong>
    <p>Store it as <code class="inline">QUICK_API_TOKEN</code>. It will not be shown again.</p>
    <div class="secret"><code id="new-token">{{.Reveal}}</code><button class="btn-secondary" type="button" onclick="navigator.clipboard.writeText(document.getElementById('new-token').textContent)">Copy</button></div>
  </div>{{end}}

  <section class="panel">
    <h2>Deploy tokens</h2>
    <p class="hint">Tokens are limited to deploys for this site. They cannot delete, rollback, change visibility, or create other tokens.</p>
    {{if .Tokens}}{{range .Tokens}}
    <div class="token">
      <div class="token-main">
        <div class="token-name">{{.Name}}</div>
        <div class="token-meta">ID {{.ID}} · scope {{range $i,$s := .Scopes}}{{if $i}}, {{end}}{{$s}}{{end}} · expires {{if .ExpiresAt}}{{.ExpiresAt}}{{else}}never{{end}} · last {{if .LastUsedAt}}{{.LastUsedAt}}{{else}}never used{{end}}</div>
      </div>
      <form method="post" action="/api/site/{{$.Name}}/tokens/{{.ID}}">
        <input type="hidden" name="_method" value="delete">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <button class="btn-secondary" type="submit">Revoke</button>
      </form>
    </div>{{end}}{{else}}<p class="empty">No deploy tokens yet.</p>{{end}}
  </section>

  <section class="panel">
    <h2>Create token</h2>
    <form class="create" method="post" action="/api/site/{{.Name}}/tokens">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <div class="field name"><label for="token-name">Name</label><input id="token-name" name="name" value="github-actions" maxlength="40" required></div>
      <div class="field"><label for="expires-in">Expires</label><select id="expires-in" name="expires_in"><option value="90d">90 days</option><option value="30d">30 days</option><option value="180d">180 days</option><option value="365d">365 days</option><option value="never">Never</option></select></div>
      <button class="btn" type="submit">Create token</button>
    </form>
  </section>
</div></body></html>`))

// iconCopy/iconCheck stack in the same slot; cp() flips data-state to cross-fade
// copy → check (icon swap), so the button never resizes from a "Copied" label.
const iconCopy = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>`
const iconCheck = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"/></svg>`
const copyBtn = `<button class="copy t-icon-swap" data-state="a" type="button" aria-label="{{.T.Copy}}" onclick="cp(this)"><span class="t-icon" data-icon="a">` + iconCopy + `</span><span class="t-icon" data-icon="b">` + iconCheck + `</span></button>`

var landingPage = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.LandingTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
.wrap{max-width:640px;margin:0 auto;padding:2rem 1.25rem}
header{display:flex;justify-content:space-between;align-items:center;margin-bottom:3.5rem}
.nav{font-family:var(--font-head);font-size:.9rem;font-weight:700;border:1px solid var(--border);border-radius:10px;padding:.45rem .9rem;color:var(--ink)}
.nav:hover{text-decoration:none;border-color:var(--brand);color:var(--brand)}
h1{font-size:2.6rem;font-weight:800;letter-spacing:-.035em;line-height:1.07;margin:0 0 .7rem;color:var(--ink);max-width:22ch;text-wrap:balance}
.tagline{color:var(--muted);font-size:1.05rem;margin:0 0 3rem;max-width:46ch;text-wrap:pretty}
h2{font-size:.8rem;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);margin:0 0 1.2rem}
.step{margin-bottom:1.4rem}
.step .label{font-family:var(--font-head);font-size:.92rem;font-weight:700;margin-bottom:.5rem;display:flex;gap:.6rem;align-items:center}
.step .n{flex:none;display:grid;place-items:center;width:1.45rem;height:1.45rem;border-radius:999px;background:var(--btn);color:var(--btn-fg);font-size:.76rem;font-weight:700}
.cmd{display:flex;align-items:stretch;gap:.5rem}
.cmd code{flex:1;background:var(--card);border:1px solid var(--border);border-radius:11px;padding:.72rem .85rem;font-size:.88rem;overflow-x:auto;white-space:nowrap;color:var(--fg);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.cmd .copy{flex:none;width:2.7rem;display:grid;place-items:center;border:1px solid var(--border);border-radius:11px;background:var(--card);color:var(--muted);cursor:pointer;transition:border-color .15s,color .15s}
.cmd .copy:hover{border-color:var(--brand);color:var(--brand)}
.cmd .copy svg{width:17px;height:17px;display:block}
.t-icon-swap{position:relative;display:inline-grid}
.t-icon-swap .t-icon{grid-area:1/1;display:grid;place-items:center;transition:opacity .25s ease-in-out,filter .25s ease-in-out,transform .25s ease-in-out;will-change:opacity,filter,transform}
.t-icon-swap[data-state="a"] .t-icon[data-icon="a"],.t-icon-swap[data-state="b"] .t-icon[data-icon="b"]{opacity:1;filter:blur(0);transform:scale(1)}
.t-icon-swap[data-state="a"] .t-icon[data-icon="b"],.t-icon-swap[data-state="b"] .t-icon[data-icon="a"]{opacity:0;filter:blur(2px);transform:scale(.25)}
.t-icon-swap .t-icon[data-icon="b"]{color:var(--ok)}
@media (prefers-reduced-motion:reduce){.t-icon-swap .t-icon{transition:none!important}}
.result{color:var(--muted);font-size:.82rem;margin:.55rem 0 0}
.result code{background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:.1rem .35rem;font-size:.85em;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.guide{margin-top:3rem;padding-top:2rem;border-top:1px solid var(--border)}
.guide .row{display:flex;gap:.85rem;align-items:baseline;padding:.55rem 0}
.guide .row+.row{border-top:1px solid var(--border)}
.guide code{flex:none;background:var(--card);border:1px solid var(--border);border-radius:7px;padding:.18rem .5rem;font-size:.82rem;color:var(--ink);font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.guide .desc{color:var(--muted);font-size:.9rem}
</style></head><body>
<div class="wrap">
  <header>
    ` + brandLink + `
    <a class="nav" href="/dashboard">{{.T.NavDashboard}}</a>
  </header>

  <h1>{{.T.LandingHeadline}}</h1>
  <p class="tagline">{{.T.LandingTagline}}</p>

  <h2>{{.T.LandingGetStarted}}</h2>

  <div class="step">
    <div class="label"><span class="n">1</span>{{.T.LandingInstall}}</div>
    <div class="cmd"><code id="install" data-win="{{.InstallWin}}">{{.Install}}</code>` + copyBtn + `</div>
  </div>

  <div class="step">
    <div class="label"><span class="n">2</span>{{.T.LandingLogin}}</div>
    <div class="cmd"><code>{{.Login}}</code>` + copyBtn + `</div>
  </div>

  <div class="step">
    <div class="label"><span class="n">3</span>{{.T.LandingDeploy}}</div>
    <div class="cmd"><code>{{.Deploy}}</code>` + copyBtn + `</div>
    <p class="result">→ <code>&lt;name&gt;.{{.Base}}</code></p>
  </div>

  <div class="guide">
    <h2>{{.T.GuideTitle}}</h2>
    <div class="row"><code>quick deploy</code><span class="desc">{{.T.GuideUpdate}}</span></div>
    <div class="row"><code>quick private</code><span class="desc">{{.T.GuideVisibility}}</span></div>
    <div class="row"><code>quick status</code><span class="desc">{{.T.GuideStatus}}</span></div>
    <div class="row"><code>quick rollback</code><span class="desc">{{.T.GuideRollback}}</span></div>
  </div>
</div>
<script>
(function(){var p=(navigator.userAgentData&&navigator.userAgentData.platform||navigator.platform||navigator.userAgent||"").toLowerCase();if(p.indexOf("win")>=0){var c=document.getElementById("install");if(c)c.textContent=c.dataset.win}})();
function cp(b){navigator.clipboard.writeText(b.previousElementSibling.textContent);b.dataset.state="b";clearTimeout(b._t);b._t=setTimeout(function(){b.dataset.state="a"},1200)}
</script>
</body></html>`))
