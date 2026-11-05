package main

// Review fixes for v0.19.0: unreadable settings refuse what depends on them,
// code-only needs a site the team really can't manage, administrators open
// code-only sites, and ownership modes keep code-only across an unlock.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

// settingsUnreadable fails every settings read.
type settingsUnreadable struct{ storage.Backend }

func (settingsUnreadable) GetSettings() ([]byte, bool, error) {
	return nil, false, errors.New("bucket unreachable")
}

func TestUnreadableSettingsRefuseFirstDeploy(t *testing.T) {
	s := newDeltaTestServer(t)
	s.store = settingsUnreadable{s.store}
	s.meta = newMetaStore(s.store, []byte("secret"), 0)
	c := &deltaClient{t: t, s: s, site: "demo"}
	if code, _ := tarDeployCode(c, map[string][]byte{"index.html": []byte("x")}); code == http.StatusOK {
		t.Fatal("first deploy went ahead without knowing whether to lock the site")
	}
	if _, ok, _ := s.store.GetMeta("demo"); ok {
		t.Error("refused first deploy left metadata behind")
	}
}

func TestUnreadableSettingsRefuseTheCodeDefault(t *testing.T) {
	e := newSettingsEnv(t)
	e.s.store = settingsUnreadable{e.s.store}
	e.s.meta = newMetaStore(e.s.store, []byte("secret"), 0)
	e.setPolicy(t, "open", policy{CreatedBy: ann, Locked: true, Owner: ann})
	code, no := "abcd1234", false
	if w, _ := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code}); w.Code != http.StatusServiceUnavailable {
		t.Errorf("code without a choice: %d, want 503", w.Code)
	}
	if w, _ := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code, CodeOnly: &no}); w.Code != http.StatusOK {
		t.Errorf("code with an explicit choice: %d, want 200", w.Code)
	}
	if w := e.visit("quick.test", zedAdmin, "/dashboard/settings", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("settings page: %d, want 503 (an empty form would overwrite them)", w.Code)
	}
}

func TestCodeOnlyNeedsAnOwnerOutsideFreeMode(t *testing.T) {
	e := newPeopleEnv(t)
	e.s.ownership = "shared"
	code, yes := "abcd1234", true
	e.setPolicy(t, "app", policy{}) // no recorded creator: anyone manages it
	if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code, CodeOnly: &yes}); w.Code != http.StatusBadRequest {
		t.Errorf("code-only on a site without an owner: %d, want 400", w.Code)
	}
	e.setPolicy(t, "app", policy{Access: quick.AccessCode, Code: code, CodeHash: e.s.meta.hashCode(code), CodeOnly: true})
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code != http.StatusOK {
		t.Errorf("stored code-only on a site without an owner: %d, want the colleague in", w.Code)
	}
}

func TestUnlockKeepsCodeOnlyOutsideFreeMode(t *testing.T) {
	e := newPeopleEnv(t)
	e.s.ownership = "shared"
	code, yes, no := "abcd1234", true, false
	if w, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code, CodeOnly: &yes}); w.Code != http.StatusOK || !res.CodeOnly {
		t.Fatalf("code-only in shared mode: %d %+v", w.Code, res)
	}
	e.setAccess(t, ann, "open", quick.PolicyRequest{Locked: &yes})
	if _, res := e.setAccess(t, ann, "open", quick.PolicyRequest{Locked: &no}); !res.CodeOnly {
		t.Errorf("unlocking in shared mode dropped code-only: %+v (only the creator manages it anyway)", res)
	}
}

func TestAdminsOpenCodeOnlySites(t *testing.T) {
	e := newCodeEnv(t)
	e.s.admins = map[string]bool{zedAdmin: true}
	p := codeOnlyPolicy()
	p.CodeHash = e.s.meta.hashCode(goodCode)
	e.setPolicy(t, "app", p)
	if w := e.visit("app.quick.test", zedAdmin, "/", nil); w.Code != http.StatusOK {
		t.Errorf("admin on a code-only site: %d, want the site", w.Code)
	}
	if w := e.visit("app.quick.test", zedAdmin, "/__quick/code?rd=https%3A%2F%2Fapp.quick.test%2F", nil); w.Code != http.StatusFound {
		t.Errorf("admin on the code page: %d, want a redirect to the site", w.Code)
	}
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "/__quick/code") {
		t.Errorf("colleague on a code-only site: %d, want the code page", w.Code)
	}
}
