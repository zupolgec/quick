package main

// Instance settings: administrators choose defaults for the whole server on
// the dashboard's settings page (lock new sites, ask colleagues for the code).

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

const zedAdmin = "zed@example.com"

func newSettingsEnv(t *testing.T) *peopleEnv {
	t.Helper()
	e := newPeopleEnv(t)
	e.s.admins = map[string]bool{zedAdmin: true}
	return e
}

func mustSettings(t *testing.T, s *server) instanceSettings {
	t.Helper()
	st, err := s.settings()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *peopleEnv) settingsPage(t *testing.T, email string) (int, string, string) {
	t.Helper()
	w := e.visit("quick.test", email, "/dashboard/settings?lang=it", nil)
	csrf := ""
	if m := csrfRe.FindStringSubmatch(w.Body.String()); m != nil {
		csrf = m[1]
	}
	return w.Code, w.Body.String(), csrf
}

func TestSettingsPageOnlyForAdmins(t *testing.T) {
	e := newSettingsEnv(t)
	if code, _, _ := e.settingsPage(t, bob); code != http.StatusNotFound {
		t.Errorf("non-admin: %d, want 404", code)
	}
	code, page, csrf := e.settingsPage(t, zedAdmin)
	if code != http.StatusOK || csrf == "" || !strings.Contains(page, `name="lock_new_sites"`) || !strings.Contains(page, `name="code_only"`) {
		t.Fatalf("admin: %d\n%s", code, page)
	}
	// The dashboard links the page for admins only.
	if w := e.visit("quick.test", zedAdmin, "/dashboard?lang=it", nil); !strings.Contains(w.Body.String(), `href="/dashboard/settings"`) {
		t.Error("admin dashboard doesn't link the settings")
	}
	if w := e.visit("quick.test", bob, "/dashboard?lang=it", nil); strings.Contains(w.Body.String(), `href="/dashboard/settings"`) {
		t.Error("non-admin dashboard links the settings")
	}
	// Non-admins can't save, even with a token of their own.
	w := e.form(bob, "/dashboard/settings", url.Values{"csrf": {e.s.meta.signCSRF(bob, 1<<40)}, "lock_new_sites": {"1"}})
	if w.Code != http.StatusNotFound || mustSettings(t, e.s).LockNewSites {
		t.Errorf("non-admin save: %d, settings %+v", w.Code, mustSettings(t, e.s))
	}
}

func TestSettingsSaveAndShow(t *testing.T) {
	e := newSettingsEnv(t)
	_, _, csrf := e.settingsPage(t, zedAdmin)
	w := e.form(zedAdmin, "/dashboard/settings", url.Values{"csrf": {csrf}, "lock_new_sites": {"1"}, "code_only": {"1"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", w.Code, w.Body)
	}
	if st := mustSettings(t, e.s); !st.LockNewSites || !st.CodeOnly {
		t.Fatalf("saved %+v", st)
	}
	_, page, _ := e.settingsPage(t, zedAdmin)
	if !strings.Contains(page, `name="lock_new_sites" value="1" checked`) || !strings.Contains(page, `name="code_only" value="1" checked`) {
		t.Errorf("saved settings not shown:\n%s", page)
	}
	// A bad token changes nothing.
	if w := e.form(zedAdmin, "/dashboard/settings", url.Values{"csrf": {"nope"}}); w.Code != http.StatusForbidden || !mustSettings(t, e.s).LockNewSites {
		t.Errorf("bad csrf: %d %+v", w.Code, mustSettings(t, e.s))
	}
	// Settings are no site: the list doesn't show them.
	names, _ := e.s.store.ListSites()
	for _, n := range names {
		if !quick.ValidName(n) {
			t.Errorf("ListSites lists %q", n)
		}
	}
}

func TestNewSitesLockedByDefault(t *testing.T) {
	s := newDeltaTestServer(t)
	if err := s.saveSettings(instanceSettings{LockNewSites: true}); err != nil {
		t.Fatal(err)
	}
	(&deltaClient{t: t, s: s, site: "demo"}).deploy(map[string][]byte{"index.html": []byte("hi")})
	p, _ := s.meta.loadFresh("demo")
	if !p.Locked || p.Owner != p.CreatedBy || p.Owner == "" {
		t.Errorf("new site: %+v, want locked by its creator", p)
	}
	// Only new sites: an existing unlocked site stays unlocked on redeploy.
	p.Locked, p.Owner = false, ""
	s.meta.save("demo", p)
	(&deltaClient{t: t, s: s, site: "demo"}).deploy(map[string][]byte{"index.html": []byte("again")})
	if p, _ := s.meta.loadFresh("demo"); p.Locked {
		t.Errorf("redeploy locked an existing site: %+v", p)
	}
}

func TestCodeOnlyDefault(t *testing.T) {
	e := newSettingsEnv(t)
	if err := e.s.saveSettings(instanceSettings{CodeOnly: true}); err != nil {
		t.Fatal(err)
	}
	code, yes, no := "abcd1234", true, false
	// Unlocked in free mode: the default can't hold, and that's no error.
	if w, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code}); w.Code != http.StatusOK || res.CodeOnly {
		t.Errorf("unlocked: %d %+v", w.Code, res)
	}
	e.setAccess(t, ann, "open", quick.PolicyRequest{Locked: &yes})
	if w, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code}); w.Code != http.StatusOK || !res.CodeOnly {
		t.Errorf("locked, no choice: %d %+v, want the default", w.Code, res)
	}
	if _, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code, CodeOnly: &no}); res.CodeOnly {
		t.Errorf("explicit no: %+v", res)
	}
	// The dashboard ticks the box for a site that isn't a code site yet.
	e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessPublic)})
	page, _ := e.sitePage(t, ann, "open")
	if !strings.Contains(page, `name="code_only" value="1" checked`) {
		t.Errorf("dashboard doesn't propose the default:\n%s", page)
	}
}

// A failed first deploy doesn't leave behind the lock the settings added.
func TestFailedFirstDeployDropsTheDefaultLock(t *testing.T) {
	s := newDeltaTestServer(t)
	s.store = putSiteFails{s.store}
	s.meta = newMetaStore(s.store, []byte("secret"), 0)
	if err := s.saveSettings(instanceSettings{LockNewSites: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.meta.save("demo", policy{Access: "public"}); err != nil {
		t.Fatal(err)
	}
	c := &deltaClient{t: t, s: s, site: "demo"}
	if code, _ := tarDeployCode(c, map[string][]byte{"index.html": []byte("x")}); code == http.StatusOK {
		t.Fatal("publish failure answered 200")
	}
	if p, _ := s.meta.loadFresh("demo"); p.Locked || p.Owner != "" || p.Access != "public" {
		t.Errorf("after a failed first deploy: %+v, want the earlier settings without a lock", p)
	}
}
