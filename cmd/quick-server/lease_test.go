package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

// fakeClock drives lease expiry without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func withClock(s *server) *fakeClock {
	c := &fakeClock{t: time.Date(2026, 10, 20, 14, 2, 0, 0, time.UTC)}
	s.leases.now = c.now
	return c
}

// started plans files and uploads what is missing, without committing: a
// deploy in progress.
func (c *deltaClient) started(files map[string][]byte) quick.DeployManifest {
	c.t.Helper()
	m, data := manifestOf(c.t, files)
	c.lease = ""
	code, pl := c.plan(m)
	if code != http.StatusOK {
		c.t.Fatalf("plan: %d", code)
	}
	if pl.Lease == "" {
		c.t.Fatal("plan returned no lease")
	}
	if code := c.upload(data, pl.Missing); code != http.StatusOK {
		c.t.Fatalf("upload: %d", code)
	}
	return m
}

func lockedBody(t *testing.T, c *deltaClient, path string, body []byte) quick.DeployLocked {
	t.Helper()
	w := c.do(http.MethodPost, path, body, map[string]string{"Accept": "application/json"})
	if w.Code != http.StatusLocked {
		t.Fatalf("%s: %d %s, want 423", path, w.Code, w.Body.String())
	}
	var res quick.DeployLocked
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("%s: 423 body %q: %v", path, w.Body.String(), err)
	}
	return res
}

func manifestJSON(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	m, _ := manifestOf(t, files)
	b, _ := json.Marshal(m)
	return b
}

// Two people (or two terminals) deploying the same site: the second plan is
// refused with who is deploying and since when; the first goes through.
func TestLeaseSecondPlanRefused(t *testing.T) {
	s := newDeltaTestServer(t)
	withClock(s)
	a := &deltaClient{t: t, s: s, site: "demo"}
	m := a.started(map[string][]byte{"index.html": []byte("a")})

	b := &deltaClient{t: t, s: s, site: "demo"}
	res := lockedBody(t, b, "/api/deploy/plan", manifestJSON(t, map[string][]byte{"index.html": []byte("b")}))
	if res.Error != quick.LeaseLocked || res.Holder != "dev@example.com" || res.Since != "2026-10-20T14:02:00Z" {
		t.Fatalf("423 body: %+v", res)
	}
	// Without Accept: JSON, a readable sentence (older CLIs print the body).
	w := b.do(http.MethodPost, "/api/deploy/plan", manifestJSON(t, map[string][]byte{"index.html": []byte("b")}), nil)
	if w.Code != http.StatusLocked || !strings.Contains(w.Body.String(), "dev@example.com is publishing this site (since 14:02 UTC)") {
		t.Fatalf("text 423: %d %q", w.Code, w.Body.String())
	}
	if w := a.commit(m); w.Code != http.StatusOK {
		t.Fatalf("holder's commit: %d %s", w.Code, w.Body.String())
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "a" {
		t.Fatalf("live: %q, want a", got)
	}
}

// The holder is named by its identity actor: a deploy token shows as such.
func TestLeaseHolderIsTokenActor(t *testing.T) {
	s := newDeltaTestServer(t)
	s.noAuth = false
	raw := "qk_ci-token"
	p := policy{CreatedBy: "owner@example.com", Tokens: []siteToken{
		{ID: "t1", Name: "ci", Hash: s.meta.hashToken("demo", raw), Scopes: []string{quick.TokenScopeDeploy}, CreatedBy: "owner@example.com"},
	}}
	if err := s.meta.save("demo", p); err != nil {
		t.Fatal(err)
	}
	a := &deltaClient{t: t, s: s, site: "demo", token: raw}
	a.started(map[string][]byte{"index.html": []byte("a")})
	b := &deltaClient{t: t, s: s, site: "demo", token: raw}
	if res := lockedBody(t, b, "/api/deploy/plan", manifestJSON(t, map[string][]byte{"x": []byte("b")})); res.Holder != "token:ci@demo" {
		t.Fatalf("holder %q, want token:ci@demo", res.Holder)
	}
	// Unauthenticated requests learn nothing about the lease.
	anon := &deltaClient{t: t, s: s, site: "demo"}
	if w := anon.do(http.MethodPost, "/api/deploy/plan", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous plan: %d, want 401", w.Code)
	}
}

// The lease belongs to whoever opened it: another identity holding the id
// (leaked, copied) can't renew, upload, commit or release with it. Taking
// over stays the only way in for someone else.
func TestLeaseBoundToItsHolder(t *testing.T) {
	s := newDeltaTestServer(t)
	s.noAuth = false
	p := policy{CreatedBy: "owner@example.com", Tokens: []siteToken{
		{ID: "t1", Name: "ci", Hash: s.meta.hashToken("demo", "qk_ci"), Scopes: []string{quick.TokenScopeDeploy}, CreatedBy: "owner@example.com"},
		{ID: "t2", Name: "cd", Hash: s.meta.hashToken("demo", "qk_cd"), Scopes: []string{quick.TokenScopeDeploy}, CreatedBy: "owner@example.com"},
	}}
	if err := s.meta.save("demo", p); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"index.html": []byte("ci")}
	a := &deltaClient{t: t, s: s, site: "demo", token: "qk_ci"}
	m, data := manifestOf(t, files)
	_, pl := a.plan(m)

	b := &deltaClient{t: t, s: s, site: "demo", token: "qk_cd", lease: a.lease}
	for _, path := range []string{"/api/deploy/plan", "/api/deploy/chunks", "/api/deploy/commit"} {
		body := mustJSON(m)
		if path == "/api/deploy/chunks" {
			body = records(t, data, pl.Missing)
		}
		res := lockedBody(t, b, path, body)
		if res.Holder != "token:ci@demo" || !strings.Contains(res.Message, "deploy owned by token:ci@demo") {
			t.Fatalf("%s with someone else's lease: %+v", path, res)
		}
	}
	if w := b.do(http.MethodDelete, "/api/deploy/lease", nil, nil); w.Code != http.StatusLocked {
		t.Fatalf("release with someone else's lease: %d, want 423", w.Code)
	}
	if code := a.upload(data, pl.Missing); code != http.StatusOK {
		t.Fatalf("holder's upload: %d", code)
	}
	if w := a.commit(m); w.Code != http.StatusOK {
		t.Fatalf("holder's commit: %d %s", w.Code, w.Body.String())
	}

	// Takeover by the other identity works, with or without the old id.
	a.started(map[string][]byte{"index.html": []byte("ci2")})
	b.lease, b.takeover = a.lease, true
	b.deploy(map[string][]byte{"index.html": []byte("cd")})
	if got := string(readSite(t, s, "demo", "index.html")); got != "cd" {
		t.Fatalf("live: %q, want cd", got)
	}
}

// Chunks and commit belong to the lease: a missing or foreign id is refused
// before the body is even read.
func TestLeaseChunksAndCommitNeedTheLease(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	files := map[string][]byte{"index.html": []byte("a")}
	m := a.started(files)
	body := manifestJSON(t, files)

	for _, id := range []string{"", "bogus"} {
		b := &deltaClient{t: t, s: s, site: "demo", lease: id}
		res := lockedBody(t, b, "/api/deploy/chunks", []byte("not gzip"))
		if res.Holder != "dev@example.com" {
			t.Errorf("chunks with id %q: %+v", id, res)
		}
		if res := lockedBody(t, b, "/api/deploy/commit", body); res.Holder != "dev@example.com" {
			t.Errorf("commit with id %q: %+v", id, res)
		}
	}
	if ok, _ := s.store.SiteExists("demo"); ok {
		t.Fatal("published by a request without the lease")
	}
	if w := a.commit(m); w.Code != http.StatusOK {
		t.Fatalf("holder's commit: %d %s", w.Code, w.Body.String())
	}
	// No deploy in progress: chunks or commit without a plan are refused too.
	b := &deltaClient{t: t, s: s, site: "demo"}
	if res := lockedBody(t, b, "/api/deploy/commit", body); res.Error != quick.LeaseLost {
		t.Fatalf("commit without a lease: %+v", res)
	}
}

// The legacy tar upload (older CLIs) waits its turn too, and holds the lease
// while it uploads.
func TestLeaseTarDeployRefused(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	m := a.started(map[string][]byte{"index.html": []byte("delta")})

	b := &deltaClient{t: t, s: s, site: "demo"}
	if code, body := tarDeployCode(b, map[string][]byte{"index.html": []byte("tar")}); code != http.StatusLocked || !strings.Contains(body, "is publishing this site") {
		t.Fatalf("tar during a lease: %d %q, want 423", code, body)
	}
	if w := a.commit(m); w.Code != http.StatusOK {
		t.Fatalf("holder's commit: %d", w.Code)
	}
	tarDeploy(t, b, map[string][]byte{"index.html": []byte("tar")})
	if got := string(readSite(t, s, "demo", "index.html")); got != "tar" {
		t.Fatalf("live: %q, want tar", got)
	}
	// The tar released its lease: a delta deploy follows right away.
	a.deploy(map[string][]byte{"index.html": []byte("after")})
}

func TestLeaseRollbackRefusedUnlessForced(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	a.deploy(map[string][]byte{"index.html": []byte("v1")})
	a.deploy(map[string][]byte{"index.html": []byte("v2")})
	m := a.started(map[string][]byte{"index.html": []byte("v3")})

	b := &deltaClient{t: t, s: s, site: "demo"}
	if w := b.do(http.MethodPost, "/api/site/demo/rollback", nil, nil); w.Code != http.StatusLocked {
		t.Fatalf("rollback during a lease: %d, want 423", w.Code)
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "v2" {
		t.Fatalf("live: %q, want v2", got)
	}
	b.takeover = true
	if w := b.do(http.MethodPost, "/api/site/demo/rollback", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("forced rollback: %d %s", w.Code, w.Body.String())
	}
	if res := lockedBody(t, a, "/api/deploy/commit", mustJSON(m)); res.Error != quick.LeaseLost {
		t.Fatalf("broken holder's commit: %+v", res)
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "v1" {
		t.Fatalf("live: %q, want v1", got)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// An idle deploy loses its lease: another one goes through, and the idle
// one's commit is refused and never publishes.
func TestLeaseExpiresWhenIdle(t *testing.T) {
	s := newDeltaTestServer(t)
	clk := withClock(s)
	a := &deltaClient{t: t, s: s, site: "demo"}
	m := a.started(map[string][]byte{"index.html": []byte("a")})

	clk.add(leaseIdle + time.Second)
	b := &deltaClient{t: t, s: s, site: "demo"}
	b.deploy(map[string][]byte{"index.html": []byte("b")})

	if res := lockedBody(t, a, "/api/deploy/commit", mustJSON(m)); res.Error != quick.LeaseLost {
		t.Fatalf("expired holder's commit: %+v", res)
	}
	if res := lockedBody(t, a, "/api/deploy/plan", mustJSON(m)); res.Error != quick.LeaseLost {
		t.Fatalf("expired holder's re-plan: %+v", res)
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "b" {
		t.Fatalf("live: %q, want b", got)
	}
}

// Every request of the deploy renews the lease: a deploy longer than the idle
// timeout but never idle keeps it.
func TestLeaseRenewedByActivity(t *testing.T) {
	s := newDeltaTestServer(t)
	clk := withClock(s)
	a := &deltaClient{t: t, s: s, site: "demo"}
	files := tinyFiles("f", 3)
	m, data := manifestOf(t, files)
	_, pl := a.plan(m)
	b := &deltaClient{t: t, s: s, site: "demo"}
	for _, h := range pl.Missing {
		clk.add(leaseIdle * 3 / 4)
		if code := a.upload(data, []string{h}); code != http.StatusOK {
			t.Fatalf("upload: %d", code)
		}
		if res := lockedBody(t, b, "/api/deploy/plan", manifestJSON(t, map[string][]byte{"x": []byte("b")})); res.Error != quick.LeaseLocked {
			t.Fatalf("other plan: %+v", res)
		}
	}
	clk.add(leaseIdle * 3 / 4)
	if w := a.commit(m); w.Code != http.StatusOK {
		t.Fatalf("commit after %d renewals: %d %s", len(pl.Missing), w.Code, w.Body.String())
	}
}

// A request in flight (a long chunk upload) keeps the lease alive however
// long it takes; the idle timeout starts when it ends.
func TestLeaseInFlightRequestKeepsIt(t *testing.T) {
	var lt leaseTable
	clk := &fakeClock{t: time.Unix(1e9, 0)}
	lt.now = clk.now
	l, _, err := lt.begin("demo", "", "a", "a@x", false)
	if err != nil {
		t.Fatal(err)
	}
	lt.end(l)
	u, err := lt.join("demo", l.id, "a")
	if err != nil {
		t.Fatal(err)
	}
	clk.add(10 * leaseIdle)
	if _, _, err := lt.begin("demo", "", "b", "b@x", false); err == nil {
		t.Fatal("lease taken while its upload is in flight")
	}
	lt.end(u)
	clk.add(leaseIdle - time.Second)
	if _, _, err := lt.begin("demo", "", "b", "b@x", false); err == nil {
		t.Fatal("lease taken before the idle timeout")
	}
	clk.add(2 * time.Second)
	if _, _, err := lt.begin("demo", "", "b", "b@x", false); err != nil {
		t.Fatalf("idle lease not expired: %v", err)
	}
}

// --takeover breaks another deploy's lease (auth and ownership still apply); the
// broken holder's next requests are refused as lost, not retried.
func TestLeaseForceBreaksIt(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	files := map[string][]byte{"index.html": []byte("a"), "b.txt": []byte("bbb")}
	m, data := manifestOf(t, files)
	_, pl := a.plan(m)

	b := &deltaClient{t: t, s: s, site: "demo", takeover: true}
	b.deploy(map[string][]byte{"index.html": []byte("forced")})

	w := a.do(http.MethodPost, "/api/deploy/chunks", records(t, data, pl.Missing), map[string]string{"Accept": "application/json"})
	if w.Code != http.StatusLocked || !strings.Contains(w.Body.String(), quick.LeaseLost) {
		t.Fatalf("broken holder's upload: %d %s", w.Code, w.Body.String())
	}
	if res := lockedBody(t, a, "/api/deploy/commit", mustJSON(m)); res.Error != quick.LeaseLost {
		t.Fatalf("broken holder's commit: %+v", res)
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "forced" {
		t.Fatalf("live: %q, want forced", got)
	}

	// Forcing doesn't bypass ownership.
	s.ownership = "owned"
	if err := s.meta.save("other", policy{CreatedBy: "someone-else@example.com"}); err != nil {
		t.Fatal(err)
	}
	o := &deltaClient{t: t, s: s, site: "other", takeover: true}
	if code, _ := o.plan(m); code != http.StatusForbidden {
		t.Fatalf("forced plan on someone else's site: %d, want 403", code)
	}
}

// The CLI's tar fallback (busy server after a re-plan) carries the lease: it
// continues that deploy instead of being locked out by it, and ends it.
func TestLeaseTarFallbackWithTheLease(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	a.started(map[string][]byte{"index.html": []byte("delta")})
	tarDeploy(t, a, map[string][]byte{"index.html": []byte("tar")})
	b := &deltaClient{t: t, s: s, site: "demo"}
	b.deploy(map[string][]byte{"index.html": []byte("b")})
}

// A refused upload still being sent gets its 423, not a connection reset (the
// server reads the body before answering), and reading it doesn't hold the
// site lock: the holder's commit goes through meanwhile.
func TestLeaseRefusedUploadIsReadWithoutTheSiteLock(t *testing.T) {
	s := newDeltaTestServer(t)
	srv := httptest.NewServer(s.apexMux)
	defer srv.Close()
	a := &deltaClient{t: t, s: s, site: "demo"}
	m := a.started(map[string][]byte{"index.html": []byte("a")})

	pr, pw := io.Pipe()
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/api/deploy?name=demo", "application/gzip", pr)
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	if _, err := pw.Write(randData(70, 4<<20)); err != nil { // the server is reading it
		t.Fatal(err)
	}
	commit := make(chan int, 1)
	go func() { commit <- a.commit(m).Code }()
	select {
	case code := <-commit:
		if code != http.StatusOK {
			t.Fatalf("holder's commit: %d", code)
		}
	case <-time.After(5 * time.Second):
		pw.CloseWithError(io.ErrClosedPipe) // unblock the server before failing
		<-commit
		t.Fatal("holder's commit blocked by a refused upload")
	}
	pw.Write(randData(71, 4<<20))
	pw.Close()
	resp := <-done
	if resp == nil {
		t.Fatal("no response")
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusLocked || !strings.Contains(string(b), "is publishing this site") {
		t.Fatalf("refused upload: %d %q", resp.StatusCode, b)
	}
}

// DELETE /api/deploy/lease: an interrupted CLI gives the site back at once.
// Only with the current id, idempotent, never publishes.
func TestLeaseReleaseEndpoint(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	a.deploy(map[string][]byte{"index.html": []byte("v1")})
	m := a.started(map[string][]byte{"index.html": []byte("v2")})
	held := a.lease

	b := &deltaClient{t: t, s: s, site: "demo"}
	if w := b.do(http.MethodDelete, "/api/deploy/lease", nil, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("release without an id: %d, want 400", w.Code)
	}
	b.lease = "bogus"
	if w := b.do(http.MethodDelete, "/api/deploy/lease", nil, nil); w.Code != http.StatusLocked {
		t.Fatalf("release with another id: %d, want 423", w.Code)
	}
	b.lease = ""
	lockedBody(t, b, "/api/deploy/plan", manifestJSON(t, map[string][]byte{"x": []byte("b")})) // still held

	if w := a.do(http.MethodDelete, "/api/deploy/lease", nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("release: %d %s, want 204", w.Code, w.Body.String())
	}
	if w := a.do(http.MethodDelete, "/api/deploy/lease", nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("second release: %d, want 204 (idempotent)", w.Code)
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "v1" {
		t.Fatalf("live after release: %q, want v1", got)
	}
	if res := lockedBody(t, a, "/api/deploy/commit", mustJSON(m)); res.Error != quick.LeaseLost {
		t.Fatalf("commit after release: %+v", res)
	}
	b.deploy(map[string][]byte{"index.html": []byte("b")}) // free right away
	// A released id can't free someone else's lease.
	b.started(map[string][]byte{"index.html": []byte("b2")})
	a.lease = held
	if w := a.do(http.MethodDelete, "/api/deploy/lease", nil, nil); w.Code != http.StatusLocked {
		t.Fatalf("stale release over another deploy: %d, want 423", w.Code)
	}
	if w := a.do(http.MethodPost, "/api/deploy/lease", nil, nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d, want 405", w.Code)
	}
}

// Releasing needs the usual deploy auth.
func TestLeaseReleaseNeedsAuth(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	a.started(map[string][]byte{"index.html": []byte("a")})
	s.noAuth = false
	anon := &deltaClient{t: t, s: s, site: "demo", lease: a.lease}
	if w := anon.do(http.MethodDelete, "/api/deploy/lease", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous release: %d, want 401", w.Code)
	}
	s.noAuth = true
	if w := a.do(http.MethodDelete, "/api/deploy/lease", nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("release: %d", w.Code)
	}
}

// A forced tar upload breaks a lease too.
func TestLeaseForcedTarDeploy(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	m := a.started(map[string][]byte{"index.html": []byte("a")})
	b := &deltaClient{t: t, s: s, site: "demo", takeover: true}
	tarDeploy(t, b, map[string][]byte{"index.html": []byte("tar")})
	if res := lockedBody(t, a, "/api/deploy/commit", mustJSON(m)); res.Error != quick.LeaseLost {
		t.Fatalf("broken holder's commit: %+v", res)
	}
}

// A deploy that ends (published, or failed for good) frees the site at once.
func TestLeaseReleasedWhenTheDeployEnds(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	b := &deltaClient{t: t, s: s, site: "demo"}
	a.deploy(map[string][]byte{"index.html": []byte("a")})
	b.deploy(map[string][]byte{"index.html": []byte("b")})

	// Commit refused for good (bad file hash): released.
	m, data := manifestOf(t, map[string][]byte{"index.html": []byte("v2")})
	m.Files[0].SHA256 = strings.Repeat("0", 64)
	a.lease = ""
	_, pl := a.plan(m)
	a.upload(data, pl.Missing)
	if w := a.commit(m); w.Code != http.StatusBadRequest {
		t.Fatalf("bad commit: %d", w.Code)
	}
	b.deploy(map[string][]byte{"index.html": []byte("b2")})

	// Plan refused (invalid manifest): released.
	a.lease = ""
	if w := a.do(http.MethodPost, "/api/deploy/plan", []byte("{"), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad plan: %d", w.Code)
	}
	b.deploy(map[string][]byte{"index.html": []byte("b3")})
}

// A 409 (re-plan) doesn't end the deploy: the re-plan carries the lease and
// gets it back, nobody slips in between.
func TestLeaseReplanKeepsIt(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	files := map[string][]byte{"index.html": []byte("a")}
	m, data := manifestOf(t, files)
	_, pl := a.plan(m)
	first := a.lease
	if w := a.commit(m); w.Code != http.StatusConflict { // nothing uploaded yet
		t.Fatalf("commit before upload: %d, want 409", w.Code)
	}
	b := &deltaClient{t: t, s: s, site: "demo"}
	lockedBody(t, b, "/api/deploy/plan", manifestJSON(t, map[string][]byte{"x": []byte("b")}))

	code, pl := a.plan(m)
	if code != http.StatusOK || pl.Lease != first {
		t.Fatalf("re-plan: %d lease %q, want 200 and %q", code, pl.Lease, first)
	}
	a.upload(data, pl.Missing)
	if w := a.commit(m); w.Code != http.StatusOK {
		t.Fatalf("commit after re-plan: %d %s", w.Code, w.Body.String())
	}
}

// Deleting the site is allowed during a deploy and ends it: the holder's
// commit is refused and doesn't bring the site back.
func TestLeaseDeleteClearsIt(t *testing.T) {
	s := newDeltaTestServer(t)
	a := &deltaClient{t: t, s: s, site: "demo"}
	a.deploy(map[string][]byte{"index.html": []byte("v1")})
	m := a.started(map[string][]byte{"index.html": []byte("v2")})

	b := &deltaClient{t: t, s: s, site: "demo"}
	if w := b.do(http.MethodDelete, "/api/site/demo", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("delete during a deploy: %d %s", w.Code, w.Body.String())
	}
	if res := lockedBody(t, a, "/api/deploy/commit", mustJSON(m)); res.Error != quick.LeaseLost {
		t.Fatalf("commit after delete: %+v", res)
	}
	if ok, _ := s.store.SiteExists("demo"); ok {
		t.Fatal("site back after delete")
	}
	b.deploy(map[string][]byte{"index.html": []byte("new")})
}
