package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

const zed = "zed@example.com" // administrator, not on any list

func newAdminEnv(t *testing.T, ownership string) *peopleEnv {
	t.Helper()
	e := newPeopleEnv(t)
	e.s.ownership = ownership
	e.s.admins = map[string]bool{zed: true}
	return e
}

// An administrator sees and opens a site limited to its owner.
func TestAdminSeesOnlyMeSite(t *testing.T) {
	e := newAdminEnv(t, "free")
	e.setPolicy(t, "app", peoplePolicy())

	if w := e.visit("app.quick.test", zed, "/", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<h1>app</h1>") {
		t.Fatalf("admin opening an only-me site: %d", w.Code)
	}
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code == http.StatusOK && strings.Contains(w.Body.String(), "<h1>app</h1>") {
		t.Fatal("a non-admin opens an only-me site")
	}
	w := e.api(t, zed, http.MethodGet, "/api/sites", nil)
	var list quick.SitesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, si := range list.Sites {
		found = found || si.Site == "app"
	}
	if !found {
		t.Errorf("admin's site list misses the only-me site: %s", w.Body)
	}
	page, _ := e.sitePage(t, zed, "app")
	if !strings.Contains(page, `action="/api/site/app/policy"`) {
		t.Error("admin's site page has no visibility form")
	}
}

// An administrator changes visibility, edits the people list, unlocks and
// deletes sites they don't own, in the strictest ownership mode.
func TestAdminActsAsOwner(t *testing.T) {
	e := newAdminEnv(t, "owned")
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessPeople, Locked: true, Owner: ann})

	if w, _ := e.setAccess(t, zed, "app", quick.PolicyRequest{AddViewers: []string{bob}}); w.Code != http.StatusOK {
		t.Fatalf("admin adding a viewer: %d %s", w.Code, w.Body)
	}
	if w, _ := e.setAccess(t, zed, "app", quick.PolicyRequest{Locked: new(false)}); w.Code != http.StatusOK {
		t.Fatalf("admin unlocking: %d %s", w.Code, w.Body)
	}
	if w, _ := e.setAccess(t, zed, "app", quick.PolicyRequest{Access: new(quick.AccessPublic)}); w.Code != http.StatusOK {
		t.Fatalf("admin making it public: %d %s", w.Code, w.Body)
	}
	if p := e.policyOf(t, "app"); p.Access != quick.AccessPublic || p.Locked || p.CreatedBy != ann {
		t.Fatalf("after admin changes: %+v", p)
	}
	// Someone else still can't.
	if w, _ := e.setAccess(t, bob, "app", quick.PolicyRequest{Access: new("sso")}); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin in owned mode: %d", w.Code)
	}
	e.setPolicy(t, "app", peoplePolicy())
	if w := e.api(t, zed, http.MethodDelete, "/api/site/app", nil); w.Code != http.StatusOK {
		t.Fatalf("admin deleting a people site: %d %s", w.Code, w.Body)
	}
}

// The site page tells everyone that administrators see every site.
func TestSitePageNamesAdmins(t *testing.T) {
	e := newAdminEnv(t, "free")
	e.setPolicy(t, "app", peoplePolicy())
	if page, _ := e.sitePage(t, ann, "app"); !strings.Contains(page, "Gli amministratori (zed@example.com) possono vedere e gestire tutti i siti") {
		t.Errorf("owner page doesn't name the admins:\n%s", page)
	}
}

// The dashboard shows access and lock as icons whose text explains them.
func TestDashboardBadgeIcons(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessCode, Locked: true, Owner: ann})
	w := e.visit("quick.test", ann, "/dashboard?lang=it", nil)
	page := w.Body.String()
	for _, want := range []string{
		`aria-label="Con codice: chiunque abbia il codice di accesso."`,
		`aria-label="Bloccato: solo chi l&#39;ha bloccato può sovrascriverlo."`,
		`<svg`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard misses %q", want)
		}
	}
	if strings.Contains(page, `<span class="tag">codice</span>`) {
		t.Error("dashboard still shows the text badge")
	}
}

// One list, each site once, with quick filters and their counts.
func TestDashboardSingleListWithFilters(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessCode, UpdatedAt: "2026-10-20T10:00:00Z"})
	// "open" (from newPeopleEnv) belongs to ann too; add someone else's public site.
	putSite(t, e.s.store, "pub", map[string]string{"index.html": "x"})
	e.setPolicy(t, "pub", policy{CreatedBy: bob, Access: quick.AccessPublic, Locked: true, Owner: bob})
	page := e.visit("quick.test", ann, "/dashboard?lang=it", nil).Body.String()
	if n := strings.Count(page, `data-name="app"`); n != 1 {
		t.Errorf("app listed %d times, want 1", n)
	}
	for _, want := range []string{
		`data-filter="all"`, `Tutti <span class="count">3</span>`,
		`data-filter="mine"`, `Miei <span class="count">2</span>`,
		`Pubblici <span class="count">1</span>`, `Con codice <span class="count">1</span>`,
		`Bloccati <span class="count">1</span>`,
		`data-access="code"`, `data-mine="1"`, `data-locked="1"`,
		"Tuo · aggiornato 2026-10-20",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard misses %q", want)
		}
	}
	if strings.Contains(page, "Riservati") {
		t.Error("a filter with no sites is shown")
	}
}
