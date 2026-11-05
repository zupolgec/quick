// Instance settings: defaults that administrators (QUICK_ADMINS) choose for
// the whole server on the dashboard's settings page. They apply when nobody
// chooses otherwise, and never change existing sites:
//
//	lock_new_sites  a site's first deploy locks it for its creator
//	code_only       setting an access code asks colleagues for it too
//	                (still only on sites the team can't manage)
package main

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"
)

type instanceSettings struct {
	LockNewSites bool `json:"lock_new_sites,omitempty"`
	CodeOnly     bool `json:"code_only,omitempty"`
}

// settingsCache holds the settings once read: this server is the only writer.
type settingsCache struct {
	mu  sync.Mutex
	val *instanceSettings
}

// settings returns the instance settings. If they can't be read, the
// built-in defaults apply (and the next call tries again).
func (s *server) settings() instanceSettings {
	s.settingsCache.mu.Lock()
	defer s.settingsCache.mu.Unlock()
	if v := s.settingsCache.val; v != nil {
		return *v
	}
	var st instanceSettings
	b, ok, err := s.store.GetSettings()
	if err == nil && ok {
		err = json.Unmarshal(b, &st)
	}
	if err != nil {
		log.Printf("WARNING: reading the instance settings: %v (using the defaults)", err)
		return instanceSettings{}
	}
	s.settingsCache.val = &st
	return st
}

func (s *server) saveSettings(st instanceSettings) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	s.settingsCache.mu.Lock()
	defer s.settingsCache.mu.Unlock()
	if err := s.store.PutSettings(b); err != nil {
		s.settingsCache.val = nil // the write may have landed anyway
		return err
	}
	s.settingsCache.val = &st
	return nil
}

// lockNewSite locks a site being created for its creator when the instance
// says so, and reports whether it did.
func (s *server) lockNewSite(p *policy, creator string) bool {
	if p.Locked || creator == "" || !s.settings().LockNewSites {
		return false
	}
	p.Locked, p.Owner = true, creator
	return true
}

// handleSettings shows (GET) and saves (POST) the settings page. It doesn't
// exist for anyone but the administrators.
func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	email, ok := s.checkSSO(r)
	if !ok {
		s.renderSSOPage(w, r, s.baseDomain, false)
		return
	}
	if !s.isAdmin(email) {
		renderNotFound(w, r, s.baseDomain, false)
		return
	}
	if r.Method == http.MethodPost {
		if !s.meta.validCSRF(email, r.FormValue("csrf")) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		st := instanceSettings{LockNewSites: r.FormValue("lock_new_sites") == "1", CodeOnly: r.FormValue("code_only") == "1"}
		if err := s.saveSettings(st); err != nil {
			http.Error(w, "could not save the settings: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("settings changed by %s: %+v", email, st)
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
		return
	}
	l := pickLang(r)
	t := textsFor(l)
	codeOnlyHint := t.SettingsCodeOnlyHint
	if s.ownership == "" || s.ownership == "free" {
		codeOnlyHint += " " + t.SettingsCodeOnlyLocked
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Accept-Language")
	_ = settingsPage.Execute(w, map[string]any{
		"Email": email, "Lang": string(l), "T": t, "S": s.settings(), "CodeOnlyHint": codeOnlyHint,
		"CSRF": s.meta.signCSRF(email, time.Now().Add(2*time.Hour).Unix()),
	})
}

var settingsPage = template.Must(template.New("settings").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.SettingsTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
.wrap{max-width:` + pageWidth + `;margin:0 auto;padding:2.5rem 1.25rem 4rem}
header{display:flex;justify-content:space-between;align-items:center;gap:1rem;margin-bottom:2.25rem}
.who{min-width:0;color:var(--muted);font-size:.95rem;overflow-wrap:anywhere;text-align:right}
.back{display:inline-flex;margin-bottom:1.25rem;font-size:.95rem;font-weight:500}
h1{font-size:2.5rem;line-height:1;letter-spacing:-.035em;margin:0 0 2rem;color:var(--ink)}
.panel{background:var(--card);padding:1.5rem 1.5rem 1.6rem;margin-top:4px}
.panel h2{font-size:1.35rem;margin:0 0 .4rem;color:var(--ink)}
.hint{color:var(--muted);font-size:.95rem;margin:0 0 1.1rem;max-width:64ch}
form{display:grid;gap:4px;max-width:560px}
.opt{display:flex;align-items:flex-start;gap:.7rem;padding:.75rem 1rem;background:var(--sunk);cursor:pointer;font-size:.95rem;color:var(--fg);transition:background-color .18s}
.opt:has(input:checked){background:var(--brand);color:#fff}
.opt:has(input:checked) small{color:rgba(255,255,255,.85)}
.opt input{width:auto;height:auto;margin:.25rem 0 0;accent-color:var(--ink)}
.opt span{display:grid;gap:.15rem}
.opt small{color:var(--muted);font-size:.86rem}
.btn{height:2.75rem;border:0;border-radius:0;font-family:var(--font-head);font-size:.95rem;font-weight:500;padding:0 1rem;cursor:pointer;justify-self:start;margin-top:.6rem}
@media(max-width:680px){.wrap{padding:1.5rem 1rem 3rem}header{display:block}.who{text-align:left;margin-top:.75rem}.btn{justify-self:stretch}h1{font-size:2rem}}
</style></head><body>
<div class="wrap">
  <header>` + brandLink + `<div class="who">{{.Email}}</div></header>
  <a class="back" href="/dashboard">{{.T.SiteBack}}</a>
  <h1>{{.T.SettingsTitle}}</h1>
  <section class="panel">
    <h2>{{.T.SettingsDefaults}}</h2>
    <p class="hint">{{.T.SettingsDefaultsHint}}</p>
    <form method="post" action="/dashboard/settings">
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <label class="opt"><input type="checkbox" name="lock_new_sites" value="1"{{if .S.LockNewSites}} checked{{end}}><span><strong>{{.T.SettingsLock}}</strong><small>{{.T.SettingsLockHint}}</small></span></label>
      <label class="opt"><input type="checkbox" name="code_only" value="1"{{if .S.CodeOnly}} checked{{end}}><span><strong>{{.T.SettingsCodeOnly}}</strong><small>{{.CodeOnlyHint}}</small></span></label>
      <button class="btn" type="submit">{{.T.AccessSave}}</button>
    </form>
  </section>
</div>
</body></html>`))
