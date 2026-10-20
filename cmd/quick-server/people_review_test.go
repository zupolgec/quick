package main

import (
	"encoding/json"
	"net/http"
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
