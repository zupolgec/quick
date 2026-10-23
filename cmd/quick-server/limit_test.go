package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

// newLimitTestServer is a delta test server with a 1 MiB default site limit
// and a 4 MiB ceiling. The noAuth user (dev@example.com) is an admin only
// when admin is true.
func newLimitTestServer(t *testing.T, admin bool) *server {
	t.Helper()
	s := newDeltaTestServer(t)
	s.siteLimit, s.maxSiteLimit = 1<<20, 4<<20
	if admin {
		s.admins = map[string]bool{"dev@example.com": true}
	}
	return s
}

func setLimit(t *testing.T, s *server, site string, body string) (int, string) {
	c := &deltaClient{t: t, s: s}
	w := c.do(http.MethodPost, "/api/site/"+site+"/limit", []byte(body), nil)
	return w.Code, w.Body.String()
}

func getLimit(t *testing.T, s *server, site string) quick.LimitResponse {
	t.Helper()
	c := &deltaClient{t: t, s: s}
	w := c.do(http.MethodGet, "/api/site/"+site+"/limit", nil, nil)
	var res quick.LimitResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &res) != nil {
		t.Fatalf("get limit: %d %s", w.Code, w.Body)
	}
	return res
}

func TestSiteLimitRefusesBigDeltaDeploy(t *testing.T) {
	s := newLimitTestServer(t, false)
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, _ := manifestOf(t, map[string][]byte{"big.bin": randData(1, 2<<20)})
	code, _ := c.plan(m)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("plan over the limit: %d, want 413", code)
	}
	// A commit that skips the plan is refused the same way.
	c.leased()
	if w := c.commit(m); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "administrator") {
		t.Fatalf("commit over the limit: %d %s", w.Code, w.Body)
	}
	if ok, _ := s.store.SiteExists("demo"); ok {
		if files, _ := s.store.ListFiles("demo"); len(files) > 0 {
			t.Fatalf("files published over the limit: %v", files)
		}
	}
}

func TestSiteLimitRefusesBigTarDeploy(t *testing.T) {
	s := newLimitTestServer(t, false)
	c := &deltaClient{t: t, s: s, site: "demo"}
	code, body := tarDeployCode(c, map[string][]byte{"a.bin": randData(1, 600<<10), "b.bin": randData(2, 600<<10)})
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(body, "administrator") {
		t.Fatalf("tar over the limit: %d %s", code, body)
	}
	if files, _ := s.store.ListFiles("demo"); len(files) > 0 {
		t.Fatalf("files published over the limit: %v", files)
	}
	// Under the limit it still works.
	tarDeploy(t, c, map[string][]byte{"a.bin": randData(1, 600<<10)})
}

func TestAdminRaisesSiteLimit(t *testing.T) {
	s := newLimitTestServer(t, true)
	if got := getLimit(t, s, "demo"); got.SizeLimit != 1<<20 || got.Custom || got.Max != 4<<20 {
		t.Fatalf("default limit: %+v", got)
	}
	if code, body := setLimit(t, s, "demo", `{"size_limit":3145728}`); code != http.StatusOK {
		t.Fatalf("raise: %d %s", code, body)
	}
	if got := getLimit(t, s, "demo"); got.SizeLimit != 3<<20 || !got.Custom {
		t.Fatalf("raised limit: %+v", got)
	}
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"big.bin": randData(1, 2<<20)})

	// Above the ceiling: refused, the limit is unchanged.
	if code, _ := setLimit(t, s, "demo", `{"size_limit":5242880}`); code != http.StatusBadRequest {
		t.Fatalf("over the ceiling: %d, want 400", code)
	}
	// 0 goes back to the server default.
	if code, body := setLimit(t, s, "demo", `{"size_limit":0}`); code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, body)
	}
	if got := getLimit(t, s, "demo"); got.SizeLimit != 1<<20 || got.Custom {
		t.Fatalf("reset limit: %+v", got)
	}
}

func TestOnlyAdminsChangeSiteLimit(t *testing.T) {
	s := newLimitTestServer(t, false)
	code, body := setLimit(t, s, "demo", `{"size_limit":3145728}`)
	if code != http.StatusForbidden || !strings.Contains(body, "administrator") {
		t.Fatalf("non-admin: %d %s", code, body)
	}
	if got := getLimit(t, s, "demo"); got.Custom {
		t.Fatalf("limit changed by a non-admin: %+v", got)
	}
}

func TestDeployRefusedWhenDiskLow(t *testing.T) {
	s := newLimitTestServer(t, false)
	free := int64(10 << 20)
	s.freeSpace = func() (int64, error) { return free, nil }
	s.minFree = 19 << 19 // 9.5 MiB
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, _ := manifestOf(t, map[string][]byte{"big.bin": randData(1, 900<<10)})
	if code, _ := c.plan(m); code != http.StatusInsufficientStorage {
		t.Fatalf("plan with the disk low: %d, want 507", code)
	}
	code, body := tarDeployCode(c, map[string][]byte{"a.bin": randData(1, 10<<10)})
	if code != http.StatusInsufficientStorage || !strings.Contains(body, "administrator") {
		t.Fatalf("tar with the disk low: %d %s", code, body)
	}
	free = 100 << 20
	c.deploy(map[string][]byte{"big.bin": randData(1, 900<<10)})
}

// The site page shows the limit; administrators change it there.
func TestDashboardSiteLimit(t *testing.T) {
	e := newPeopleEnv(t)
	e.s.siteLimit, e.s.maxSiteLimit = 100<<20, 2<<30
	e.s.admins = map[string]bool{bob: true}
	e.setPolicy(t, "app", policy{CreatedBy: ann})

	page, _ := e.sitePage(t, ann, "app")
	if !strings.Contains(page, "Dimensione massima") || !strings.Contains(page, "100 MB") || !strings.Contains(page, "chiedi a un amministratore") {
		t.Errorf("owner page misses the limit:\n%s", page)
	}
	if strings.Contains(page, `action="/api/site/app/limit"`) {
		t.Error("a non-admin sees the limit form")
	}

	page, csrf := e.sitePage(t, bob, "app")
	if !strings.Contains(page, `action="/api/site/app/limit"`) {
		t.Fatalf("admin page has no limit form:\n%s", page)
	}
	w := e.form(bob, "/api/site/app/limit", url.Values{"csrf": {csrf}, "size_limit": {"1.5GB"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("admin form: %d %s", w.Code, w.Body)
	}
	if p := e.policyOf(t, "app"); p.SizeLimit != 3<<29 {
		t.Fatalf("stored limit %d", p.SizeLimit)
	}
	if page, _ = e.sitePage(t, ann, "app"); !strings.Contains(page, "1.5 GB") {
		t.Error("page doesn't show the raised limit")
	}
	// Over the ceiling: the page explains it, nothing changes.
	w = e.form(bob, "/api/site/app/limit", url.Values{"csrf": {csrf}, "size_limit": {"3GB"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "2 GB") {
		t.Fatalf("over the ceiling: %d %s", w.Code, w.Body)
	}
	// A non-admin posting the form is refused.
	_, csrfAnn := e.sitePage(t, ann, "app")
	if w := e.form(ann, "/api/site/app/limit", url.Values{"csrf": {csrfAnn}, "size_limit": {"2GB"}}); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin form: %d", w.Code)
	}
	// Empty goes back to the default.
	if w := e.form(bob, "/api/site/app/limit", url.Values{"csrf": {csrf}, "size_limit": {""}}); w.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d", w.Code)
	}
	if p := e.policyOf(t, "app"); p.SizeLimit != 0 {
		t.Fatalf("reset stored %d", p.SizeLimit)
	}
}
