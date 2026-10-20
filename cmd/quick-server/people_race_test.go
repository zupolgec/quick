package main

// Revocation races on people sites, reproduced with deterministic barriers:
// a request paused mid-way (its body, a DNS lookup) while the owner removes
// the caller must not answer with what changed after the removal.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

func (e *peopleEnv) revoke(t *testing.T, who string) {
	t.Helper()
	if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{RemoveViewers: []string{who}}); w.Code != http.StatusOK {
		t.Fatalf("revoke %s: %d %s", who, w.Code, w.Body)
	}
}

// A viewer's plan paused in its body while the owner removes them and
// publishes new content (taking the site over) must not plan against the new
// version.
func TestPeoplePlanRevokedMidBody(t *testing.T) {
	e := newPeopleEnv(t)
	e.s.stage = newChunkStage(t.TempDir())
	e.setPolicy(t, "app", peoplePolicy(bob))
	owner := &deltaClient{t: t, s: e.s, site: "app", token: e.iss.sign(fakeTokenOpts{email: ann, verified: true})}
	owner.deploy(map[string][]byte{"index.html": randData(1, 200<<10)})

	fresh := map[string][]byte{"index.html": randData(2, 200<<10)} // published after the removal
	m, _ := manifestOf(t, fresh)
	body, _ := json.Marshal(m)
	pr, pw := io.Pipe()
	r := httptest.NewRequest(http.MethodPost, "/api/deploy/plan?name=app", pr)
	r.Header.Set("Authorization", "Bearer "+e.iss.sign(fakeTokenOpts{email: bob, verified: true}))
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { e.s.apexMux.ServeHTTP(w, r); close(done) }()
	// Barrier: the write returns once the handler (auth and lease done) is
	// reading the manifest.
	if _, err := pw.Write(body[:8]); err != nil {
		t.Fatal(err)
	}
	e.revoke(t, bob)
	owner.takeover = true
	owner.deploy(map[string][]byte{"index.html": randData(3, 200<<10)})
	owner.takeover = false
	owner.deploy(fresh) // the version bob's plan started on is pruned by now
	pw.Write(body[8:])
	pw.Close()
	<-done
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), `"missing"`) {
		t.Fatalf("revoked viewer's plan: %d %s", w.Code, w.Body)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("revoked viewer's plan: %d, want 404 like any non-viewer", w.Code)
	}
}
