package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

// A team member who may deploy a people site but can't open it must not learn
// anything about its content through the delta protocol: plans for a chunk the
// live site has and for one it lacks look the same (they get no delta at all;
// the CLI falls back to the full upload), and commits can't reuse live chunks.
// The owner and the viewers keep their deltas.
func TestPeopleDeltaIsNoOracleForNonViewers(t *testing.T) {
	s := newDeltaTestServer(t) // QUICK_DEV_NOAUTH: every caller is dev@example.com
	const dev = "dev@example.com"
	secret := map[string][]byte{"index.html": randData(7, 300<<10)}
	owner := &deltaClient{t: t, s: s, site: "demo"}
	owner.deploy(secret) // dev creates it...
	p, _ := s.meta.loadFresh("demo")
	p.CreatedBy, p.Access = ann, quick.AccessPeople // ...but it belongs to ann now
	if err := s.meta.save("demo", p); err != nil {
		t.Fatal(err)
	}

	c := &deltaClient{t: t, s: s, site: "demo"}
	present, presentData := manifestOf(t, secret)
	absent, _ := manifestOf(t, map[string][]byte{"index.html": randData(8, 300<<10)})
	b1, _ := json.Marshal(present)
	b2, _ := json.Marshal(absent)
	w1 := c.do(http.MethodPost, "/api/deploy/plan", b1, nil)
	w2 := c.do(http.MethodPost, "/api/deploy/plan", b2, nil)
	if w1.Code != w2.Code || w1.Body.String() != w2.Body.String() {
		t.Fatalf("plans differ: present %d %q, absent %d %q", w1.Code, w1.Body, w2.Code, w2.Body)
	}
	if w1.Code != http.StatusNotFound {
		t.Errorf("plan for a non-viewer: %d, want 404 (full upload)", w1.Code)
	}
	var hashes []string
	for h := range presentData {
		hashes = append(hashes, h)
	}
	if code := c.upload(presentData, hashes); code != http.StatusNotFound {
		t.Errorf("chunks for a non-viewer: %d, want 404", code)
	}
	if w := c.commit(present); w.Code != http.StatusNotFound {
		t.Errorf("commit reusing live chunks for a non-viewer: %d, want 404", w.Code)
	}
	if s.leases.busy("demo") != nil {
		t.Error("a refused plan left the site leased")
	}

	// A listed viewer (and the owner) still get deltas.
	p.Viewers = []string{dev}
	if err := s.meta.save("demo", p); err != nil {
		t.Fatal(err)
	}
	if code, pl := c.plan(present); code != http.StatusOK || len(pl.Missing) != 0 {
		t.Errorf("viewer plan: %d missing %d", code, len(pl.Missing))
	}
	if w := c.commit(present); w.Code != http.StatusOK {
		t.Errorf("viewer commit: %d %s", w.Code, w.Body)
	}
}

// Identities match however the IdP or an old record cased them: creator, lock
// owner and viewers alike, in every ownership mode.
func TestIdentityMatchIgnoresCase(t *testing.T) {
	const stored, signedIn = "Ann@Example.com", "ann@example.com"
	for _, mode := range []string{"free", "shared", "owned"} {
		s := &server{ownership: mode}
		p := policy{CreatedBy: stored}
		for _, act := range []string{actDeploy, actDelete, actPolicy, actToken} {
			if ok, why := s.canWrite(p, signedIn, act); !ok {
				t.Errorf("%s %s by the creator: %s", mode, act, why)
			}
		}
		locked := policy{CreatedBy: stored, Locked: true, Owner: stored}
		if ok, why := s.canWrite(locked, signedIn, actDeploy); !ok {
			t.Errorf("%s deploy by the lock owner: %s", mode, why)
		}
		if ok, _ := s.canWrite(locked, "bob@example.com", actDeploy); ok {
			t.Errorf("%s: lock didn't stop someone else", mode)
		}
	}
	if !sameUser(" Ann@Example.com ", "ann@example.com") || sameUser("", "") || sameUser("ann@example.com", "bob@example.com") {
		t.Error("sameUser")
	}
}

// A mixed-case creator unlocks, changes visibility and deletes through the API.
func TestMixedCaseOwnerUnlocks(t *testing.T) {
	for _, mode := range []string{"free", "shared", "owned"} {
		e := newPeopleEnv(t)
		e.s.ownership = mode
		e.setPolicy(t, "app", policy{CreatedBy: "Ann@Example.com", Locked: true, Owner: "Ann@Example.com"})
		if w, res := e.setAccess(t, ann, "app", quick.PolicyRequest{Locked: new(false)}); w.Code != http.StatusOK || res.Locked {
			t.Errorf("%s unlock: %d %s", mode, w.Code, w.Body)
		}
		if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{Access: new(quick.AccessPublic)}); w.Code != http.StatusOK {
			t.Errorf("%s public: %d %s", mode, w.Code, w.Body)
		}
		if w := e.api(t, ann, http.MethodDelete, "/api/site/app", nil); w.Code != http.StatusOK {
			t.Errorf("%s delete: %d %s", mode, w.Code, w.Body)
		}
	}
}

// The dashboard counts a mixed-case creator's sites as theirs.
func TestDashboardMineIgnoresCase(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", policy{CreatedBy: "Ann@Example.com"})
	e.setPolicy(t, "open", policy{CreatedBy: bob})
	b := e.visit("quick.test", ann, "/dashboard", nil).Body.String()
	mine := b[:strings.Index(b, "All sites")]
	if !strings.Contains(mine, "app.quick.test") || strings.Contains(mine, "open.quick.test") {
		t.Errorf("your sites:\n%s", mine)
	}
}

// Deploy tokens never count as viewers of a people site, whoever made them.
func TestPeopleDeltaBlindForTokens(t *testing.T) {
	p := policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: []string{bob}}
	for _, c := range []struct {
		ident authIdentity
		want  bool
	}{
		{authIdentity{Email: ann}, false},
		{authIdentity{Email: "BOB@example.com"}, false},
		{authIdentity{Email: carl}, true},
		{authIdentity{Email: ann, Token: true}, true},
		{authIdentity{Email: bob, Token: true}, true},
	} {
		if got := blindDeploy(p, c.ident); got != c.want {
			t.Errorf("%+v: blind=%v, want %v", c.ident, got, c.want)
		}
	}
	if blindDeploy(policy{CreatedBy: ann}, authIdentity{Email: carl, Token: true}) {
		t.Error("team sites are never blind")
	}
}
