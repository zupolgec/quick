package main

// "Selected people" access: the site is visible only to its owner (CreatedBy)
// and a list of company accounts. Multi-user paths run through the real router
// with a fake oauth2-proxy (the _oauth2_proxy cookie value is the signed-in
// email) and the fake OIDC issuer for the bearer-token API.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

const (
	ann  = "ann@example.com" // owner
	bob  = "bob@example.com" // listed viewer
	carl = "carl@example.com"
)

type peopleEnv struct {
	s   *server
	iss *fakeIssuer
}

func newPeopleEnv(t *testing.T) *peopleEnv {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/auth" {
			if c, err := r.Cookie("_oauth2_proxy"); err == nil && c.Value != "" {
				w.Header().Set("X-Auth-Request-Email", c.Value)
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("oauth2-proxy " + r.URL.Path))
	}))
	t.Cleanup(fake.Close)
	iss := newFakeIssuer(t)
	s := newTestServer(t)
	s.baseDomain = "quick.test"
	s.domain = "example.com"
	s.clientID = "test-client"
	s.oidcIssuer = iss.issuer
	s.oauth2URL = fake.URL
	s.ownership = "free"
	s.meta = newMetaStore(s.store, []byte("secret"), time.Second)
	s.domains = newDomainIndex()
	if err := s.setupOAuthProxy(); err != nil {
		t.Fatal(err)
	}
	s.apexMux = s.buildApexMux()
	putSite(t, s.store, "app", map[string]string{"index.html": "<h1>app</h1>"})
	putSite(t, s.store, "open", map[string]string{"index.html": "<h1>open</h1>"})
	if err := s.meta.save("open", policy{CreatedBy: ann}); err != nil {
		t.Fatal(err)
	}
	return &peopleEnv{s: s, iss: iss}
}

func (e *peopleEnv) setPolicy(t *testing.T, site string, p policy) {
	t.Helper()
	if err := e.s.meta.save(site, p); err != nil {
		t.Fatal(err)
	}
}

func (e *peopleEnv) policyOf(t *testing.T, site string) policy {
	t.Helper()
	p, err := e.s.meta.loadFresh(site)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// visit fetches path on host as the signed-in email ("" = guest).
func (e *peopleEnv) visit(host, email, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	if email != "" {
		r.AddCookie(&http.Cookie{Name: "_oauth2_proxy", Value: email})
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.s.route(w, r)
	return w
}

// api calls an apex API endpoint with an ID token for email.
func (e *peopleEnv) api(t *testing.T, email, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "https://quick.test"+path, rd)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+e.iss.sign(fakeTokenOpts{email: email, verified: true}))
	w := httptest.NewRecorder()
	e.s.route(w, r)
	return w
}

func (e *peopleEnv) setAccess(t *testing.T, email, site string, req quick.PolicyRequest) (*httptest.ResponseRecorder, quick.PolicyResponse) {
	t.Helper()
	w := e.api(t, email, http.MethodPost, "/api/site/"+site+"/policy", req)
	var res quick.PolicyResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w, res
}

// form posts a dashboard form as the signed-in email.
func (e *peopleEnv) form(email, path string, v url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://quick.test"+path, strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept-Language", "it-IT")
	r.AddCookie(&http.Cookie{Name: "_oauth2_proxy", Value: email})
	w := httptest.NewRecorder()
	e.s.route(w, r)
	return w
}

func peoplePolicy(viewers ...string) policy {
	return policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: viewers}
}

func TestPeopleGate(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", peoplePolicy(bob))

	for _, who := range []string{ann, bob, "BOB@Example.com", "Ann@example.com"} {
		if w := e.visit("app.quick.test", who, "/", nil); w.Code != http.StatusOK || w.Body.String() != "<h1>app</h1>" {
			t.Errorf("%s: code %d body %q", who, w.Code, w.Body)
		}
	}

	w := e.visit("app.quick.test", carl, "/", map[string]string{"Accept-Language": "it-IT"})
	b := w.Body.String()
	if w.Code != http.StatusForbidden {
		t.Errorf("non-viewer: code %d, want 403", w.Code)
	}
	if !strings.Contains(b, "Questo sito è riservato") || !strings.Contains(b, ann) || !strings.Contains(b, carl) {
		t.Errorf("non-viewer page misses heading, owner or signed-in account:\n%s", b)
	}
	if strings.Contains(b, bob) || strings.Contains(b, "<h1>app</h1>") {
		t.Errorf("non-viewer page leaks the viewer list or the site:\n%s", b)
	}
	if !strings.Contains(b, "https://quick.test/oauth2/sign_out?rd="+url.QueryEscape("https://app.quick.test/")) {
		t.Errorf("non-viewer page has no link to switch account:\n%s", b)
	}
	if en := e.visit("app.quick.test", carl, "/", nil).Body.String(); !strings.Contains(en, "This site is restricted") {
		t.Errorf("english page not localized:\n%s", en)
	}

	g := e.visit("app.quick.test", "", "/page", nil)
	if g.Code != http.StatusOK || !strings.Contains(g.Body.String(), ssoMarker+url.QueryEscape("https://app.quick.test/page")) {
		t.Errorf("guest: code %d, want the SSO page:\n%s", g.Code, g.Body)
	}
}

func TestPeopleGateOnlyMe(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", peoplePolicy())
	if w := e.visit("app.quick.test", ann, "/", nil); w.Code != http.StatusOK {
		t.Errorf("owner: code %d", w.Code)
	}
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code != http.StatusForbidden {
		t.Errorf("anyone else: code %d, want 403", w.Code)
	}
}

func TestPeopleClientAPI(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", peoplePolicy(bob))
	w := e.visit("app.quick.test", bob, "/__quick/api/me", sameOrigin)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"email":"`+bob+`"`) {
		t.Errorf("viewer me: code %d body %s", w.Code, w.Body)
	}
	w = e.visit("app.quick.test", carl, "/__quick/api/me", sameOrigin)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), carl) {
		t.Errorf("non-viewer me: code %d body %s", w.Code, w.Body)
	}
	if w := e.visit("app.quick.test", "", "/__quick/api/me", sameOrigin); w.Code != http.StatusUnauthorized {
		t.Errorf("guest me: code %d", w.Code)
	}
}

func TestPeopleCustomDomainRedirects(t *testing.T) {
	s, dns := newDomainTestServer(t)
	dns.point("www.example.com", "app")
	addDomain(t, s, "app", "www.example.com")
	p, _ := s.meta.loadFresh("app")
	p.CreatedBy, p.Access = "dev@example.com", quick.AccessPeople
	if err := s.meta.save("app", p); err != nil {
		t.Fatal(err)
	}
	w := hostGet(s, "www.example.com", "/page?x=1")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://app.quick.test/page?x=1" {
		t.Errorf("code %d location %q", w.Code, w.Header().Get("Location"))
	}
}

func TestPeoplePolicyOwnerChanges(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", policy{CreatedBy: ann})

	w, res := e.setAccess(t, ann, "app", quick.PolicyRequest{Access: new(quick.AccessPeople)})
	if w.Code != http.StatusOK || res.Access != quick.AccessPeople || len(res.Viewers) != 0 {
		t.Fatalf("switch to people: code %d res %+v body %s", w.Code, res, w.Body)
	}
	w, res = e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: []string{"Bob@Example.com", bob, " carl@example.com ", ann}})
	if w.Code != http.StatusOK || !slices.Equal(res.Viewers, []string{bob, carl}) {
		t.Fatalf("add: code %d viewers %v body %s", w.Code, res.Viewers, w.Body)
	}
	if got := e.policyOf(t, "app").Viewers; !slices.Equal(got, []string{bob, carl}) {
		t.Errorf("stored viewers %v", got)
	}
	w, res = e.setAccess(t, ann, "app", quick.PolicyRequest{RemoveViewers: []string{"BOB@example.com", "nobody@example.com"}})
	if w.Code != http.StatusOK || !slices.Equal(res.Viewers, []string{carl}) {
		t.Fatalf("remove: code %d viewers %v", w.Code, res.Viewers)
	}
	if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{RemoveViewers: []string{ann}}); w.Code != http.StatusBadRequest {
		t.Errorf("remove owner: code %d, want 400", w.Code)
	}
	// only me: people with an empty list
	w, res = e.setAccess(t, ann, "app", quick.PolicyRequest{Access: new(quick.AccessPeople), Viewers: &[]string{}})
	if w.Code != http.StatusOK || res.Access != quick.AccessPeople || len(res.Viewers) != 0 {
		t.Fatalf("only me: code %d res %+v", w.Code, res)
	}
	// back to the whole team drops the list
	w, res = e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: []string{bob}})
	if w.Code != http.StatusOK || !slices.Equal(res.Viewers, []string{bob}) {
		t.Fatalf("add again: code %d res %+v", w.Code, res)
	}
	w, res = e.setAccess(t, ann, "app", quick.PolicyRequest{Access: new("sso")})
	if w.Code != http.StatusOK || res.Access != "sso" || len(res.Viewers) != 0 || len(e.policyOf(t, "app").Viewers) != 0 {
		t.Fatalf("back to team: code %d res %+v", w.Code, res)
	}
	// add on a team site switches it to people
	w, res = e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: []string{carl}})
	if w.Code != http.StatusOK || res.Access != quick.AccessPeople || !slices.Equal(res.Viewers, []string{carl}) {
		t.Fatalf("add switches: code %d res %+v", w.Code, res)
	}
}

func TestPeoplePolicyNonOwner(t *testing.T) {
	for _, mode := range []string{"free", "shared", "owned"} {
		t.Run(mode, func(t *testing.T) {
			e := newPeopleEnv(t)
			e.s.ownership = mode
			e.setPolicy(t, "app", peoplePolicy(bob))
			for name, req := range map[string]quick.PolicyRequest{
				"team":   {Access: new("sso")},
				"public": {Access: new(quick.AccessPublic)},
				"add":    {AddViewers: []string{carl}},
				"remove": {RemoveViewers: []string{bob}},
				"only":   {Access: new(quick.AccessPeople), Viewers: &[]string{}},
			} {
				w, _ := e.setAccess(t, bob, "app", req)
				if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "only the owner ("+ann+") can change who can see this site") {
					t.Errorf("%s by a viewer: code %d body %s", name, w.Code, w.Body)
				}
			}
			if p := e.policyOf(t, "app"); p.Access != quick.AccessPeople || !slices.Equal(p.Viewers, []string{bob}) {
				t.Errorf("policy changed: %+v", p)
			}
			if w := e.api(t, bob, http.MethodDelete, "/api/site/app", nil); w.Code != http.StatusForbidden {
				t.Errorf("delete by a viewer: code %d", w.Code)
			}
		})
	}
}

func TestPeopleSwitchIntoPeopleIsOwnerOnly(t *testing.T) {
	e := newPeopleEnv(t)
	// free mode: anyone may change a team site's visibility, but not limit it
	// to chosen people.
	if w, _ := e.setAccess(t, bob, "open", quick.PolicyRequest{Access: new(quick.AccessPeople)}); w.Code != http.StatusForbidden {
		t.Errorf("switch into people: code %d", w.Code)
	}
	if w, _ := e.setAccess(t, bob, "open", quick.PolicyRequest{AddViewers: []string{bob}}); w.Code != http.StatusForbidden {
		t.Errorf("add into people: code %d", w.Code)
	}
	if w, res := e.setAccess(t, bob, "open", quick.PolicyRequest{Access: new(quick.AccessPublic)}); w.Code != http.StatusOK || res.Access != quick.AccessPublic {
		t.Errorf("public stays free: code %d res %+v", w.Code, res)
	}
	// A site with no recorded owner can't be limited to people.
	e.setPolicy(t, "app", policy{})
	if w, _ := e.setAccess(t, bob, "app", quick.PolicyRequest{Access: new(quick.AccessPeople)}); w.Code != http.StatusConflict {
		t.Errorf("no owner: code %d body %s", w.Code, w.Body)
	}
}

func TestPeoplePolicyLock(t *testing.T) {
	e := newPeopleEnv(t)
	// Owner locks the people site and can still manage it.
	e.setPolicy(t, "app", peoplePolicy(bob))
	if w, res := e.setAccess(t, ann, "app", quick.PolicyRequest{Locked: new(true)}); w.Code != http.StatusOK || !res.Locked {
		t.Fatalf("lock: code %d", w.Code)
	}
	if w, res := e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: []string{carl}}); w.Code != http.StatusOK || len(res.Viewers) != 2 {
		t.Errorf("owner on locked site: code %d res %+v", w.Code, res)
	}
	// A lock held by someone else blocks even the owner.
	p := peoplePolicy(bob)
	p.Locked, p.Owner = true, carl
	e.setPolicy(t, "app", p)
	if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: []string{carl}}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "locked") {
		t.Errorf("foreign lock: code %d body %s", w.Code, w.Body)
	}
}

func TestPeopleViewerValidation(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", peoplePolicy())
	for _, bad := range []string{"nope", "a@", "@example.com", "Bob <bob@example.com>", "x@other.org", "a b@example.com", ""} {
		if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: []string{bad}}); w.Code != http.StatusBadRequest {
			t.Errorf("add %q: code %d, want 400", bad, w.Code)
		}
	}
	if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{Access: new(quick.AccessPeople), Viewers: &[]string{"x@other.org"}}); w.Code != http.StatusBadRequest {
		t.Errorf("replace with other domain: code %d", w.Code)
	}
	many := make([]string, maxViewers+1)
	for i := range many {
		many[i] = fmt.Sprintf("u%d@example.com", i)
	}
	if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: many}); w.Code != http.StatusBadRequest {
		t.Errorf("too many: code %d", w.Code)
	}
	if w, res := e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: many[:maxViewers]}); w.Code != http.StatusOK || len(res.Viewers) != maxViewers {
		t.Errorf("at the cap: code %d n %d", w.Code, len(res.Viewers))
	}
	if p := e.policyOf(t, "app"); len(p.Viewers) != maxViewers {
		t.Errorf("stored %d viewers", len(p.Viewers))
	}

	// "*" accepts any well-formed address; several allowed domains each work.
	e.s.domain = "*"
	if w, _ := e.setAccess(t, ann, "open", quick.PolicyRequest{AddViewers: []string{"x@other.org"}}); w.Code != http.StatusOK {
		t.Errorf("any domain: code %d body %s", w.Code, w.Body)
	}
	e.s.domain = "example.com, other.org"
	if w, _ := e.setAccess(t, ann, "open", quick.PolicyRequest{AddViewers: []string{"y@OTHER.org"}}); w.Code != http.StatusOK {
		t.Errorf("second allowed domain: code %d body %s", w.Code, w.Body)
	}
}

func TestPeoplePolicyRead(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", peoplePolicy(bob))
	for who, want := range map[string][]string{ann: {bob}, bob: {bob}, carl: nil} {
		w := e.api(t, who, http.MethodGet, "/api/site/app/policy", nil)
		var res quick.PolicyResponse
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if w.Code != http.StatusOK || res.Access != quick.AccessPeople || res.CreatedBy != ann || !slices.Equal(res.Viewers, want) || res.ViewersHidden != (who == carl) {
			t.Errorf("%s: code %d res %+v", who, w.Code, res)
		}
		if who == carl && strings.Contains(w.Body.String(), bob) {
			t.Errorf("non-viewer sees the list: %s", w.Body)
		}
	}
}

func TestPeopleDeleteOwnerOnly(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", peoplePolicy(bob))
	if w := e.api(t, carl, http.MethodDelete, "/api/site/app", nil); w.Code != http.StatusForbidden {
		t.Errorf("delete by someone else: code %d", w.Code)
	}
	if w := e.api(t, ann, http.MethodDelete, "/api/site/app", nil); w.Code != http.StatusOK {
		t.Errorf("delete by owner: code %d body %s", w.Code, w.Body)
	}
}

// Deploy permission doesn't change: a team member may still publish a people
// site they can't open (free mode), as long as it isn't locked.
func TestPeopleDeployUnchanged(t *testing.T) {
	s := &server{ownership: "free"}
	if ok, _ := s.canWrite(peoplePolicy(), carl, actDeploy); !ok {
		t.Error("deploy refused to a non-viewer in free mode")
	}
	if ok, _ := s.canWrite(peoplePolicy(), carl, actDelete); ok {
		t.Error("delete allowed to a non-owner")
	}
	if ok, _ := s.canWrite(peoplePolicy(), "ANN@example.com", actDelete); !ok {
		t.Error("delete refused to the owner")
	}
}

func TestPeopleListings(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", peoplePolicy(bob))
	for who, want := range map[string]bool{ann: true, bob: true, carl: false} {
		w := e.api(t, who, http.MethodGet, "/api/sites", nil)
		var res quick.SitesResponse
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		var names []string
		for _, si := range res.Sites {
			names = append(names, si.Site)
		}
		if w.Code != http.StatusOK || slices.Contains(names, "app") != want || !slices.Contains(names, "open") {
			t.Errorf("/api/sites as %s: code %d names %v", who, w.Code, names)
		}
		d := e.visit("quick.test", who, "/dashboard", nil).Body.String()
		if strings.Contains(d, "app.quick.test") != want || !strings.Contains(d, "open.quick.test") {
			t.Errorf("dashboard as %s: shows app=%v, want %v", who, strings.Contains(d, "app.quick.test"), want)
		}
	}
	// The site page of a people site doesn't exist for a non-viewer.
	w := e.visit("quick.test", carl, "/dashboard/site/app", nil)
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), bob) || strings.Contains(w.Body.String(), ann) {
		t.Errorf("site page as non-viewer: code %d\n%s", w.Code, w.Body)
	}
	if w := e.visit("quick.test", bob, "/dashboard/site/app", nil); w.Code != http.StatusOK {
		t.Errorf("site page as viewer: code %d", w.Code)
	}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (e *peopleEnv) sitePage(t *testing.T, email, site string) (string, string) {
	t.Helper()
	w := e.visit("quick.test", email, "/dashboard/site/"+site+"?lang=it", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("site page as %s: code %d", email, w.Code)
	}
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("no csrf token on the page:\n%s", w.Body)
	}
	return w.Body.String(), m[1]
}

func TestPeopleDashboardForm(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", policy{CreatedBy: ann})
	page, csrf := e.sitePage(t, ann, "app")
	for _, want := range []string{"Chi può vedere il sito", "Tutta l&#39;azienda", "Solo io", "Persone scelte", `action="/api/site/app/policy"`} {
		if !strings.Contains(page, want) {
			t.Errorf("owner page misses %q", want)
		}
	}

	post := func(email, csrf string, v url.Values) *httptest.ResponseRecorder {
		v.Set("csrf", csrf)
		return e.form(email, "/api/site/app/policy", v)
	}
	if w := post(ann, "bogus", url.Values{"op": {"access"}, "access": {"people"}}); w.Code != http.StatusForbidden {
		t.Errorf("bad csrf: code %d", w.Code)
	}
	if p := e.policyOf(t, "app"); p.Access != "" {
		t.Fatalf("bad csrf changed the policy: %+v", p)
	}
	w := post(ann, csrf, url.Values{"op": {"access"}, "access": {"only-me"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/dashboard/site/app" {
		t.Fatalf("only me: code %d location %q body %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if p := e.policyOf(t, "app"); p.Access != quick.AccessPeople || len(p.Viewers) != 0 {
		t.Fatalf("only me stored %+v", p)
	}
	if w := post(ann, csrf, url.Values{"op": {"add"}, "email": {"Bob@example.com, carl@example.com"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("add: code %d body %s", w.Code, w.Body)
	}
	if p := e.policyOf(t, "app"); !slices.Equal(p.Viewers, []string{bob, carl}) {
		t.Fatalf("add stored %v", p.Viewers)
	}
	page, _ = e.sitePage(t, ann, "app")
	if !strings.Contains(page, bob) || !strings.Contains(page, carl) || !strings.Contains(page, "Rimuovi") {
		t.Errorf("owner page doesn't list the viewers:\n%s", page)
	}
	if w := post(ann, csrf, url.Values{"op": {"remove"}, "email": {carl}}); w.Code != http.StatusSeeOther {
		t.Fatalf("remove: code %d", w.Code)
	}
	if p := e.policyOf(t, "app"); !slices.Equal(p.Viewers, []string{bob}) {
		t.Fatalf("remove stored %v", p.Viewers)
	}
	w = post(ann, csrf, url.Values{"op": {"add"}, "email": {"x@other.org"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "x@other.org") || !strings.Contains(w.Body.String(), "Chi può vedere il sito") {
		t.Errorf("invalid email: code %d, want the page with an error\n%s", w.Code, w.Body)
	}

	// A viewer sees the page without the controls, and can't post them.
	vpage, vcsrf := e.sitePage(t, bob, "app")
	if strings.Contains(vpage, `action="/api/site/app/policy"`) {
		t.Errorf("viewer page has access controls")
	}
	if !strings.Contains(vpage, ann) {
		t.Errorf("viewer page doesn't say who the owner is")
	}
	w = post(bob, vcsrf, url.Values{"op": {"add"}, "email": {"dave@example.com"}})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), ann) {
		t.Errorf("viewer post: code %d\n%s", w.Code, w.Body)
	}
	if p := e.policyOf(t, "app"); !slices.Equal(p.Viewers, []string{bob}) {
		t.Errorf("viewer post changed the list: %v", p.Viewers)
	}

	// Back to the whole company.
	if w := post(ann, csrf, url.Values{"op": {"access"}, "access": {"team"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("team: code %d", w.Code)
	}
	if p := e.policyOf(t, "app"); p.Access != "" || len(p.Viewers) != 0 {
		t.Errorf("team stored %+v", p)
	}
}
