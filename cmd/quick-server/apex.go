// The apex (== BASE_DOMAIN) is the control plane: control API, auth, and a page
// that shows branding + SSO sign-in for guests and the sites dashboard for
// authenticated users. Subdomains are just sites.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

// renderSSOPage shows the sign-in page (no bare redirect): branding + SSO
// button. The button goes to sign_in on the apex, which after sign-in
// returns to rd.
func (s *server) renderSSOPage(w http.ResponseWriter, r *http.Request, host string) {
	rd := "https://" + host + r.URL.RequestURI()
	signIn := "https://" + s.baseDomain + "/oauth2/sign_in?rd=" + url.QueryEscape(rd)
	l := pickLang(r)
	t := textsFor(l)
	if s.oidcIssuer != "" {
		// generic IdP: neutral label, no provider name.
		if l == langIT {
			t.SSOButton = "Accedi"
		} else {
			t.SSOButton = "Sign in"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept-Language")
	_ = ssoPage.Execute(w, map[string]any{
		"Host": host, "SignIn": signIn, "Lang": string(l), "T": t,
	})
}

// handleApexRoot serves the home (install + usage) at "/" to signed-in users;
// guests get the SSO page, as on /dashboard. The dashboard lives at /dashboard.
func (s *server) handleApexRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r) // the apex serves no sites
		return
	}
	if _, ok := s.checkSSO(r); !ok {
		s.renderSSOPage(w, r, s.baseDomain)
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
	s.renderSitePage(w, r, name, email, reveal, http.StatusOK, "")
}

// renderSitePageError shows the site page again with a refused change
// explained in the visitor's language.
func (s *server) renderSitePageError(w http.ResponseWriter, r *http.Request, name, email string, e *policyErr) {
	s.renderSitePage(w, r, name, email, "", e.status, policyErrText(e, textsFor(pickLang(r))))
}

// accessLevel is a choice in the site page's visibility form.
type accessLevel struct {
	Value, Label, Hint string
	Checked            bool
}

func (s *server) renderSitePage(w http.ResponseWriter, r *http.Request, name, email, reveal string, status int, errText string) {
	s.renderSitePageWith(w, r, name, email, reveal, status, errText, "")
}

// renderSitePageWith is renderSitePage with an error for the size limit form.
func (s *server) renderSitePageWith(w http.ResponseWriter, r *http.Request, name, email, reveal string, status int, errText, sizeErr string) {
	p, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// A people site doesn't exist for whoever isn't on it.
	if !s.mayView(p, email) {
		renderNotFound(w, r, name+"."+s.baseDomain, false)
		return
	}
	if ok, reason := s.canWrite(p, email, actToken); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	exists, _ := s.store.SiteExists(name)
	l := pickLang(r)
	t := textsFor(l)
	current := accessKey(p)
	levels := []accessLevel{
		{Value: "team", Label: t.LevelTeam, Hint: t.LevelTeamHint},
		{Value: "only-me", Label: t.LevelOnlyMe, Hint: t.LevelOnlyMeHint},
		{Value: "people", Label: t.LevelPeople, Hint: t.LevelPeopleHint},
		{Value: "code", Label: t.LevelCode, Hint: t.LevelCodeHint},
		{Value: "public", Label: t.LevelPublic, Hint: t.LevelPublicHint},
	}
	var currentLabel string
	for i := range levels {
		if levels[i].Checked = levels[i].Value == current; levels[i].Checked {
			currentLabel = levels[i].Label
		}
	}
	csrf := s.meta.signCSRF(email, time.Now().Add(2*time.Hour).Unix())
	sizeAsk, adminsNote := t.SizeAskSys, ""
	if a := s.adminList(); len(a) > 0 {
		sizeAsk = fmt.Sprintf(t.SizeAsk, strings.Join(a, ", "))
		adminsNote = fmt.Sprintf(t.AccessAdmins, strings.Join(a, ", "))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept-Language")
	w.WriteHeader(status)
	_ = dashboardSitePage.Execute(w, map[string]any{
		"Email": email, "Name": name, "URL": "https://" + name + "." + s.baseDomain,
		"Base": s.baseDomain, "Access": policyBadge(p, t),
		"Locked": p.Locked, "Updated": updatedLabel(p), "Exists": exists,
		"Tokens": tokenInfos(p.Tokens), "Reveal": reveal, "CSRF": csrf,
		"Owner": p.CreatedBy, "IsOwner": s.actsAsOwner(p, email), "Levels": levels,
		"CurrentLevel": currentLabel, "People": p.Access == quick.AccessPeople,
		"Viewers": p.Viewers, "HasCode": p.Access == quick.AccessCode, "Error": errText,
		"Domain": s.companyDomain(), "Lang": string(l), "T": t,
		"Limit": quick.FormatSize(s.limitOf(p)), "Admin": s.isAdmin(email), "SizeError": sizeErr, "SizeAsk": sizeAsk, "AdminsNote": adminsNote,
		"SizeHint": fmt.Sprintf(t.SizeHint, quick.FormatSize(s.ceiling()), quick.FormatSize(s.defaultLimit())),
	})
}

// companyDomain is the first allowed email domain ("" when any is allowed),
// for the viewer email placeholder.
func (s *server) companyDomain() string {
	if d := quick.SplitList(s.domain); len(d) > 0 && d[0] != "*" {
		return d[0]
	}
	return ""
}

// accessKey names the site's visibility as the dashboard form does.
func accessKey(p policy) string {
	switch p.Access {
	case quick.AccessPublic, quick.AccessCode:
		return p.Access
	case quick.AccessPeople:
		if len(p.Viewers) == 0 {
			return "only-me"
		}
		return "people"
	default:
		return "team"
	}
}

func (s *server) handleSites(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	email, err := s.authenticate(r)
	if err != nil {
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
		p, err := s.meta.load(n)
		if err != nil {
			continue // unreadable: it could be a people site, don't reveal it
		}
		if !s.mayView(p, email) {
			continue // even the name of a people site is theirs only
		}
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
		p, err := s.meta.load(n)
		if err != nil || !s.mayView(p, email) {
			continue // a people site (or one that might be) is listed only to its people
		}
		rows = append(rows, dashRow{
			Name:    n,
			URL:     "https://" + n + "." + s.baseDomain,
			Manage:  "/dashboard/site/" + n,
			Badge:   policyBadge(p, t),
			Locked:  p.Locked,
			Updated: updatedLabel(p),
			at:      p.UpdatedAt,
			Mine:    isOwner(p, email),
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
.card{width:min(420px,100%);background:var(--card);padding:2.5rem 2rem 2.25rem}
.card .brand{margin-bottom:2rem}
.card .logo{height:2rem}
h1{font-size:1.75rem;line-height:1.1;margin:0 0 .5rem;color:var(--ink)}
p{margin:0 0 1.75rem;color:var(--muted);font-size:1rem;overflow-wrap:anywhere}
p b{color:var(--ink);font-weight:500}
.btn{width:100%;min-height:3rem;font-size:1rem}
</style></head><body>
<div class="card">
  ` + brandWordmark + `
  <h1>{{.T.SSOHeading}}</h1>
  <p>{{.T.SSOIntro}} <b>{{.Host}}</b>.</p>
  <a class="btn" href="{{.SignIn}}">{{.T.SSOButton}}</a>
</div></body></html>`))

var notFoundPage = template.Must(template.New("404").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.NotFoundTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
body{display:grid;place-items:center;padding:1.5rem}
.card{width:min(420px,100%);background:var(--card);padding:2.5rem 2rem 2.25rem}
.card .brand{margin-bottom:2rem}
.card .logo{height:2rem}
h1{font-size:1.75rem;line-height:1.1;margin:0 0 .5rem;color:var(--ink)}
p{margin:0;color:var(--muted);font-size:1rem;overflow-wrap:anywhere}
p b{color:var(--ink);font-weight:500}
.btn{width:100%;min-height:3rem;font-size:1rem}
</style></head><body>
<div class="card">
  ` + brandWordmark + `
  {{if .Page}}<h1>{{.T.NotFoundPageHeading}}</h1>
  <p>{{.T.NotFoundPageIntro}} <b>{{.Host}}</b>.</p>{{else}}<h1>{{.T.NotFoundSiteHeading}}</h1>
  <p>{{.T.NotFoundSiteIntro}} <b>{{.Host}}</b>.</p>{{end}}
</div></body></html>`))

// renderNotFound writes the branded 404: page=false for a site that doesn't
// exist, true for a missing page on a published site without its own 404.html.
func renderNotFound(w http.ResponseWriter, r *http.Request, host string, page bool) {
	l := pickLang(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Add("Vary", "Accept-Language")
	w.WriteHeader(http.StatusNotFound)
	_ = notFoundPage.Execute(w, map[string]any{
		"Host": host, "Page": page, "Lang": string(l), "T": textsFor(l),
	})
}

// pageWidth is the column of the home, the dashboard and a site's page, so
// moving between them doesn't shift the layout.
const pageWidth = "860px"

// docsURL is the public documentation.
const docsURL = "https://quick-cli.com/docs/"

var dashboardPage = template.Must(template.New("dash").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.DashTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
.wrap{max-width:` + pageWidth + `;margin:0 auto;padding:2.5rem 1.25rem 4rem}
header{display:flex;justify-content:space-between;align-items:center;gap:1rem;margin-bottom:2.5rem}
.who{min-width:0;color:var(--muted);font-size:.95rem;overflow-wrap:anywhere;text-align:right}
h2{font-size:1.5rem;color:var(--ink);margin:2.75rem 0 .9rem}
header+h2{margin-top:0}
.site{display:grid;grid-template-columns:minmax(0,1fr) auto;align-items:center;gap:1rem;padding:1rem 1.15rem;background:var(--card);margin-bottom:4px}
.site-main{min-width:0}
.titleline{display:flex;align-items:center;gap:.5rem;flex-wrap:wrap;min-width:0}
.site .name{font-size:1.15rem;font-weight:500;font-family:var(--font-head)}
.site .name a{color:var(--ink);overflow-wrap:anywhere}
.site .meta{min-width:0;color:var(--muted);font-size:.88rem;overflow-wrap:anywhere;margin-top:.2rem}
.manage{display:inline-flex;align-items:center;justify-content:center;min-height:2.5rem;padding:0 1rem;font-size:.92rem;font-weight:500;color:var(--ink);background:var(--sunk);white-space:nowrap;text-decoration:none;transition:background-color .18s,color .18s}
.manage:hover{background:var(--brand);color:#fff;text-decoration:none}
.tag{display:inline-flex;align-items:center;font-size:.8rem;line-height:1;padding:.3rem .5rem;background:var(--sunk);color:var(--muted)}
.empty{color:var(--muted);font-size:1rem}
.help{margin-top:3rem;color:var(--muted);font-size:.95rem}
code{background:var(--sunk);padding:.12rem .4rem;font-size:.88em;font-family:var(--font-code)}
.help code{background:var(--card)}
.search{width:100%;height:2.75rem;border:0;border-radius:0;background:var(--card);color:var(--fg);font:inherit;font-size:1rem;padding:0 .9rem;margin:0}
.site[hidden],.nomatch{display:none}
.help a{white-space:nowrap}
@media(max-width:640px){.wrap{padding:1.5rem 1rem 3rem}header{display:grid;grid-template-columns:minmax(0,1fr) auto}.who{text-align:left;grid-column:1/-1;margin-top:.75rem}.site{padding:.9rem}.help code{overflow-wrap:anywhere}}
</style></head><body>
<div class="wrap">
  <header>` + brandLink + `<div class="who">{{.Email}}</div></header>

  {{if .All}}<input class="search" id="search" type="search" placeholder="{{.T.DashSearch}}" aria-label="{{.T.DashSearch}}" autocomplete="off">{{end}}

  <h2>{{.T.DashYourSites}}</h2>
  <div class="list">
  {{if .Mine}}{{range .Mine}}
  <div class="site" data-name="{{.Name}}">
    <div class="site-main"><div class="titleline"><span class="name"><a href="{{.URL}}">{{.Name}}</a></span><span class="tag">{{.Badge}}</span>{{if .Locked}}<span class="tag">{{$.T.DashLocked}}</span>{{end}}</div><div class="meta">{{.Updated}}</div></div>
    <a class="manage" href="{{.Manage}}">{{$.T.DashManage}}</a>
  </div>{{end}}<p class="empty nomatch">{{.T.DashNoMatch}}</p>{{else}}<p class="empty">{{.T.DashEmptyMine}} <code>quick deploy</code>.</p>{{end}}
  </div>

  <h2>{{.T.DashAllSites}}</h2>
  <div class="list">
  {{if .All}}{{range .All}}
  <div class="site" data-name="{{.Name}}">
    <div class="site-main"><div class="titleline"><span class="name"><a href="{{.URL}}">{{.Name}}</a></span><span class="tag">{{.Badge}}</span>{{if .Locked}}<span class="tag">{{$.T.DashLocked}}</span>{{end}}</div><div class="meta">{{.Updated}}</div></div>
    <a class="manage" href="{{.Manage}}">{{$.T.DashManage}}</a>
  </div>{{end}}<p class="empty nomatch">{{.T.DashNoMatch}}</p>{{else}}<p class="empty">{{.T.DashEmptyAll}}</p>{{end}}
  </div>

  <div class="help">
    {{.T.DashHelpPublish}} <code>quick deploy &lt;name&gt; ./folder</code> → <code>&lt;name&gt;.{{.Base}}</code>.
    {{.T.DashHelpDocs}} <a href="` + docsURL + `">quick-cli.com/docs</a>.
  </div>
</div>
<script>
(function(){var q=document.getElementById("search");if(!q)return;q.addEventListener("input",function(){var v=q.value.trim().toLowerCase();document.querySelectorAll(".list").forEach(function(l){var n=0;l.querySelectorAll(".site").forEach(function(s){var ok=s.dataset.name.indexOf(v)>=0;s.hidden=!ok;if(ok)n++});var m=l.querySelector(".nomatch");if(m)m.style.display=n?"none":"block"})})})();
</script>
</body></html>`))

var dashboardSitePage = template.Must(template.New("dash-site").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Name}} · {{.T.SiteTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
.wrap{max-width:` + pageWidth + `;margin:0 auto;padding:2.5rem 1.25rem 4rem}
header{display:flex;justify-content:space-between;align-items:center;gap:1rem;margin-bottom:2.25rem}
.who{min-width:0;color:var(--muted);font-size:.95rem;overflow-wrap:anywhere;text-align:right}
.back{display:inline-flex;margin-bottom:1.25rem;font-size:.95rem;font-weight:500}
.top{display:flex;justify-content:space-between;align-items:flex-start;gap:1.5rem;margin-bottom:2rem}
h1{font-size:2.5rem;line-height:1;letter-spacing:-.035em;margin:0;color:var(--ink);overflow-wrap:anywhere}
.url{font-size:1rem;margin-top:.5rem;overflow-wrap:anywhere}
.url a{overflow-wrap:anywhere}
.tags{display:flex;gap:.4rem;flex-wrap:wrap;justify-content:flex-end}
.tag{font-size:.8rem;line-height:1;padding:.3rem .5rem;background:var(--card);color:var(--muted)}
.panel{background:var(--card);padding:1.5rem 1.5rem 1.6rem;margin-top:4px}
.panel h2{font-size:1.35rem;margin:0 0 .4rem;color:var(--ink)}
.hint{color:var(--muted);font-size:.95rem;margin:0 0 1.1rem;max-width:64ch}
.token{display:flex;justify-content:space-between;align-items:center;gap:1rem;padding:.8rem 1rem;background:var(--sunk);margin-top:4px}
.token-main{min-width:0}
.token-name{font-weight:500;color:var(--ink);overflow-wrap:anywhere}
.token-meta{color:var(--muted);font-size:.86rem;margin-top:.15rem;overflow-wrap:anywhere;word-break:break-word}
.empty{color:var(--muted);font-size:.95rem;margin:.5rem 0 0}
form.create{display:flex;flex-wrap:wrap;align-items:end;gap:.65rem;margin-top:.5rem}
.field{display:grid;gap:.35rem}
.field.name{flex:1 1 220px}
label{font-size:.88rem;color:var(--muted)}
input,select{width:100%;height:2.75rem;border:0;border-radius:0;background:var(--sunk);color:var(--fg);font:inherit;font-size:1rem;padding:0 .8rem}
select{appearance:none;-webkit-appearance:none;padding-right:2.4rem;cursor:pointer;background-image:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' fill='none' stroke='%23888' stroke-width='2.2' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpath d='m6 9 6 6 6-6'/%3E%3C/svg%3E");background-repeat:no-repeat;background-position:right .8rem center;background-size:1rem}
.btn,.btn-secondary{height:2.75rem;border:0;border-radius:0;font-family:var(--font-head);font-size:.95rem;font-weight:500;padding:0 1rem;cursor:pointer;transition:background-color .18s,color .18s,transform .14s}
.btn-secondary{background:var(--card);color:var(--ink)}
.token .btn-secondary{background:var(--card)}
.btn-secondary:hover{background:var(--brand);color:#fff}
.btn-secondary:active{transform:scale(.97)}
.reveal{background:var(--brand);color:#fff;padding:1.25rem 1.5rem;margin:0 0 4px}
.reveal p{margin:.3rem 0 .9rem;color:#fff;font-size:.95rem}
.reveal code.inline{background:rgba(255,255,255,.18);color:#fff}
.secret{display:flex;align-items:center;gap:.5rem}
.secret code{flex:1;min-width:0;background:#fff;color:#1a2238;padding:.7rem .8rem;overflow-x:auto;white-space:nowrap;font-family:var(--font-code);font-size:.88rem}
.secret .btn-secondary{background:#1a2238;color:#fff}
code.inline{background:var(--sunk);padding:.1rem .35rem;font-size:.88em;font-family:var(--font-code)}
.hint b{color:var(--ink);font-weight:500;overflow-wrap:anywhere}
.hint.small{font-size:.86rem;margin-top:.5rem}
.panel h3{font-size:1rem;margin:1.4rem 0 .4rem;color:var(--ink)}
.err{margin:0 0 1rem;padding:.7rem .9rem;font-size:.92rem;color:var(--err);background:var(--err-bg);max-width:64ch}
form.access{display:grid;gap:4px;max-width:560px}
.opt{display:flex;align-items:flex-start;gap:.7rem;padding:.75rem 1rem;background:var(--sunk);cursor:pointer;font-size:.95rem;color:var(--fg);transition:background-color .18s}
.opt:has(input:checked){background:var(--brand);color:#fff}
.opt:has(input:checked) small{color:rgba(255,255,255,.85)}
.opt input{width:auto;height:auto;margin:.25rem 0 0;accent-color:var(--ink)}
.opt span{display:grid;gap:.15rem}
.opt small{color:var(--muted);font-size:.86rem}
form.access .field.code{margin-top:.4rem}
form.access:not(:has(input[value=code]:checked)) .field.code{display:none}
form.access .btn{justify-self:start;margin-top:.6rem}
.person{display:flex;justify-content:space-between;align-items:center;gap:1rem;padding:.7rem 1rem;background:var(--sunk);margin-top:4px}
.person-email{min-width:0;overflow-wrap:anywhere;color:var(--ink)}
@media(max-width:680px){form.access .btn{justify-self:stretch}.person{display:block}.person form{margin-top:.55rem}.person .btn-secondary{width:100%}}
@media(max-width:680px){.wrap{padding:1.5rem 1rem 3rem}header{display:block}.top{display:block}.tags{justify-content:flex-start;margin-top:.9rem}.token{display:block}.token form{margin-top:.65rem}.token .btn-secondary{width:100%}.who{text-align:left;margin-top:.75rem}.secret{display:block}.secret .btn-secondary{width:100%;margin-top:.55rem}form.create{display:grid;grid-template-columns:minmax(0,1fr)}.field.name{min-width:0}.btn{width:100%}h1{font-size:2rem}}
</style></head><body>
<div class="wrap">
  <header>` + brandLink + `<div class="who">{{.Email}}</div></header>
  <a class="back" href="/dashboard">{{.T.SiteBack}}</a>
  <div class="top">
    <div><h1>{{.Name}}</h1><div class="url"><a href="{{.URL}}">{{.URL}}</a></div></div>
    <div class="tags"><span class="tag">{{.Access}}</span>{{if .Locked}}<span class="tag">{{.T.DashLocked}}</span>{{end}}{{if not .Exists}}<span class="tag">{{.T.SiteNoFiles}}</span>{{end}}</div>
  </div>

  {{if .Reveal}}<div class="reveal">
    <strong>{{.T.TokenCreated}}</strong>
    <p>{{.T.TokenStoreAs}} <code class="inline">QUICK_API_TOKEN</code>. {{.T.TokenNotShown}}</p>
    <div class="secret"><code id="new-token">{{.Reveal}}</code><button class="btn-secondary" type="button" onclick="navigator.clipboard.writeText(document.getElementById('new-token').textContent)">{{.T.Copy}}</button></div>
  </div>{{end}}

  <section class="panel" id="access">
    <h2>{{.T.AccessTitle}}</h2>
    {{if .Owner}}<p class="hint">{{.T.AccessOwner}} <b>{{.Owner}}</b></p>{{end}}
    {{if .Error}}<div class="err" role="alert">{{.Error}}</div>{{end}}
    {{if .IsOwner}}
    <form class="access" method="post" action="/api/site/{{.Name}}/policy">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <input type="hidden" name="op" value="access">
      {{range .Levels}}<label class="opt"><input type="radio" name="access" value="{{.Value}}"{{if .Checked}} checked{{end}}><span><strong>{{.Label}}</strong><small>{{.Hint}}</small></span></label>
      {{end}}<div class="field code"><label for="access-code">{{.T.AccessCodeLabel}}</label><input id="access-code" name="code" autocomplete="off"{{if .HasCode}} placeholder="{{.T.AccessCodeKeep}}"{{end}}></div>
      <button class="btn" type="submit">{{.T.AccessSave}}</button>
    </form>
    {{else}}<p class="hint">{{.T.AccessCurrent}} <b>{{.CurrentLevel}}</b>. {{.T.AccessOwnerOnly}}</p>{{end}}
    {{if .AdminsNote}}<p class="hint small">{{.AdminsNote}}</p>{{end}}

    {{if .People}}
    <h3>{{.T.PeopleTitle}}</h3>
    {{if .Viewers}}{{range .Viewers}}
    <div class="person">
      <span class="person-email">{{.}}</span>
      {{if $.IsOwner}}<form method="post" action="/api/site/{{$.Name}}/policy">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <input type="hidden" name="op" value="remove">
        <input type="hidden" name="email" value="{{.}}">
        <button class="btn-secondary" type="submit">{{$.T.PeopleRemove}}</button>
      </form>{{end}}
    </div>{{end}}{{else}}<p class="empty">{{.T.PeopleOnlyOwner}}</p>{{end}}
    {{if .IsOwner}}
    <form class="create" method="post" action="/api/site/{{.Name}}/policy">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <input type="hidden" name="op" value="add">
      <div class="field name"><label for="viewer-email">{{.T.PeopleEmail}}</label><input id="viewer-email" name="email" type="text" inputmode="email" autocomplete="off"{{if .Domain}} placeholder="{{.T.PeopleEmailUser}}@{{.Domain}}"{{end}} required></div>
      <button class="btn" type="submit">{{.T.PeopleAdd}}</button>
    </form>
    <p class="hint small">{{.T.PeopleEmailHint}}</p>
    {{end}}{{end}}
  </section>

  <section class="panel" id="size">
    <h2>{{.T.SizeTitle}}</h2>
    <p class="hint">{{.T.SizeIntro}} <b>{{.Limit}}</b>.{{if not .Admin}} {{.SizeAsk}}{{end}}</p>
    {{if .Admin}}
    {{if .SizeError}}<div class="err" role="alert">{{.SizeError}}</div>{{end}}
    <form class="create" method="post" action="/api/site/{{.Name}}/limit">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <div class="field name"><label for="size-limit">{{.T.SizeLabel}}</label><input id="size-limit" name="size_limit" autocomplete="off" placeholder="1.5GB"></div>
      <button class="btn" type="submit">{{.T.SizeSave}}</button>
    </form>
    <p class="hint small">{{.SizeHint}}</p>
    {{end}}
  </section>

  <section class="panel">
    <h2>{{.T.SiteTokens}}</h2>
    <p class="hint">{{.T.SiteTokensHint}}</p>
    {{if .Tokens}}{{range .Tokens}}
    <div class="token">
      <div class="token-main">
        <div class="token-name">{{.Name}}</div>
        <div class="token-meta">ID {{.ID}} · {{$.T.TokenScope}} {{range $i,$s := .Scopes}}{{if $i}}, {{end}}{{$s}}{{end}} · {{$.T.TokenExpires}} {{if .ExpiresAt}}{{.ExpiresAt}}{{else}}{{$.T.TokenNever}}{{end}} · {{if .LastUsedAt}}{{$.T.TokenLastUsed}} {{.LastUsedAt}}{{else}}{{$.T.TokenNeverUsed}}{{end}}</div>
      </div>
      <form method="post" action="/api/site/{{$.Name}}/tokens/{{.ID}}">
        <input type="hidden" name="_method" value="delete">
        <input type="hidden" name="csrf" value="{{$.CSRF}}">
        <button class="btn-secondary" type="submit">{{$.T.TokenRevoke}}</button>
      </form>
    </div>{{end}}{{else}}<p class="empty">{{.T.SiteTokensEmpty}}</p>{{end}}

    <h3>{{.T.TokenCreate}}</h3>
    <form class="create" method="post" action="/api/site/{{.Name}}/tokens">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <div class="field name"><label for="token-name">{{.T.TokenName}}</label><input id="token-name" name="name" placeholder="github-actions" maxlength="40"></div>
      <div class="field"><label for="expires-in">{{.T.TokenExpiry}}</label><select id="expires-in" name="expires_in"><option value="90d">90 {{.T.TokenDays}}</option><option value="30d">30 {{.T.TokenDays}}</option><option value="180d">180 {{.T.TokenDays}}</option><option value="365d">365 {{.T.TokenDays}}</option><option value="never">{{.T.TokenNoExpiry}}</option></select></div>
      <button class="btn" type="submit">{{.T.TokenCreate}}</button>
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
.wrap{max-width:` + pageWidth + `;margin:0 auto;padding:2.5rem 1.25rem 4rem}
header{display:flex;justify-content:space-between;align-items:center;margin-bottom:4rem}
.nav{font-family:var(--font-head);font-size:.95rem;font-weight:500;background:var(--card);padding:.6rem 1rem;color:var(--ink);text-decoration:none;transition:background-color .18s,color .18s}
.nav:hover{background:var(--brand);color:#fff;text-decoration:none}
h1{font-size:clamp(2.6rem,7vw,4.25rem);line-height:.98;letter-spacing:-.045em;margin:0 0 1rem;color:var(--ink);max-width:14ch;text-wrap:balance}
.tagline{color:var(--muted);font-size:1.25rem;line-height:1.4;margin:0 0 3.5rem;max-width:40ch;text-wrap:pretty}
h2{font-size:1.5rem;color:var(--ink);margin:0 0 1.25rem}
.step{margin-bottom:1.6rem}
.step .label{font-family:var(--font-head);font-size:1.1rem;font-weight:500;margin-bottom:.5rem;color:var(--ink)}
.cmd{display:flex;align-items:stretch;gap:4px}
.cmd code{flex:1;background:var(--card);padding:.85rem 1rem;font-size:.92rem;overflow-x:auto;white-space:nowrap;color:var(--fg);font-family:var(--font-code)}
.cmd .copy{flex:none;width:3rem;display:grid;place-items:center;border:0;border-radius:0;background:var(--card);color:var(--muted);cursor:pointer;transition:background-color .18s,color .18s}
.cmd .copy:hover{background:var(--brand);color:#fff}
.cmd .copy svg{width:18px;height:18px;display:block}
.t-icon-swap{position:relative;display:inline-grid}
.t-icon-swap .t-icon{grid-area:1/1;display:grid;place-items:center;transition:opacity .25s ease-in-out,transform .25s ease-in-out;will-change:opacity,transform}
.t-icon-swap[data-state="a"] .t-icon[data-icon="a"],.t-icon-swap[data-state="b"] .t-icon[data-icon="b"]{opacity:1;transform:scale(1)}
.t-icon-swap[data-state="a"] .t-icon[data-icon="b"],.t-icon-swap[data-state="b"] .t-icon[data-icon="a"]{opacity:0;transform:scale(.25)}
@media (prefers-reduced-motion:reduce){.t-icon-swap .t-icon{transition:none!important}}
.result{color:var(--muted);font-size:.95rem;margin:.6rem 0 0}
.result code{background:var(--card);padding:.12rem .4rem;font-size:.88em;font-family:var(--font-code)}
.guide{margin-top:3.5rem}
.guide .row{display:flex;gap:1rem;align-items:baseline;padding:.75rem 1rem;background:var(--card);margin-bottom:4px}
.guide code{flex:none;font-size:.9rem;color:var(--ink);font-family:var(--font-code)}
.guide .desc{color:var(--muted);font-size:.98rem}
.navs{display:flex;gap:4px}
.more{margin:1.1rem 0 0;font-size:.98rem}
@media(max-width:560px){.wrap{padding:1.5rem 1rem 3rem}header{gap:1rem;margin-bottom:2.75rem}.brand{min-width:0}.nav{margin-left:auto;white-space:nowrap}.guide .row{display:block}.guide code{display:block;margin-bottom:.25rem}.guide .desc{display:block}}
</style></head><body>
<div class="wrap">
  <header>
    ` + brandLink + `
    <nav class="navs"><a class="nav" href="` + docsURL + `">{{.T.NavDocs}}</a><a class="nav" href="/dashboard">{{.T.NavDashboard}}</a></nav>
  </header>

  <h1>{{.T.LandingHeadline}}</h1>
  <p class="tagline">{{.T.LandingTagline}}</p>

  <h2>{{.T.LandingGetStarted}}</h2>

  <div class="step">
    <div class="label">{{.T.LandingInstall}}</div>
    <div class="cmd"><code id="install" data-win="{{.InstallWin}}">{{.Install}}</code>` + copyBtn + `</div>
  </div>

  <div class="step">
    <div class="label">{{.T.LandingLogin}}</div>
    <div class="cmd"><code>{{.Login}}</code>` + copyBtn + `</div>
  </div>

  <div class="step">
    <div class="label">{{.T.LandingDeploy}}</div>
    <div class="cmd"><code>{{.Deploy}}</code>` + copyBtn + `</div>
    <p class="result">→ <code>&lt;name&gt;.{{.Base}}</code></p>
  </div>

  <div class="guide">
    <h2>{{.T.GuideTitle}}</h2>
    <div class="row"><code>quick deploy</code><span class="desc">{{.T.GuideUpdate}}</span></div>
    <div class="row"><code>quick access</code><span class="desc">{{.T.GuideVisibility}}</span></div>
    <div class="row"><code>quick status</code><span class="desc">{{.T.GuideStatus}}</span></div>
    <div class="row"><code>quick rollback</code><span class="desc">{{.T.GuideRollback}}</span></div>
    <p class="more"><a href="` + docsURL + `">{{.T.GuideMore}} →</a></p>
  </div>
</div>
<script>
(function(){var p=(navigator.userAgentData&&navigator.userAgentData.platform||navigator.platform||navigator.userAgent||"").toLowerCase();if(p.indexOf("win")>=0){var c=document.getElementById("install");if(c)c.textContent=c.dataset.win}})();
function cp(b){navigator.clipboard.writeText(b.previousElementSibling.textContent);b.dataset.state="b";clearTimeout(b._t);b._t=setTimeout(function(){b.dataset.state="a"},1200)}
</script>
</body></html>`))
