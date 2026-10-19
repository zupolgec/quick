package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

// leaseServer wraps fakeDeltaServer: hands out a lease, records what each
// request carried, and can answer 423 on one endpoint.
type leaseServer struct {
	*fakeDeltaServer
	mu       sync.Mutex
	seen     []string // "<path> lease=<id> takeover=<bool> accept=<json?>"
	lockedOn string   // endpoint answering 423
	locked   quick.DeployLocked
}

func (l *leaseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.seen = append(l.seen, fmt.Sprintf("%s lease=%s takeover=%v json=%v", r.URL.Path, r.Header.Get(quick.DeployLeaseHeader),
		r.URL.Query().Get("takeover") == "1", strings.Contains(r.Header.Get("Accept"), "application/json")))
	locked := r.URL.Path == l.lockedOn
	l.mu.Unlock()
	if locked {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		json.NewEncoder(w).Encode(l.locked)
		return
	}
	if r.URL.Path == "/api/deploy/plan" {
		rec := httptest.NewRecorder()
		l.fakeDeltaServer.ServeHTTP(rec, r)
		var res quick.DeployPlanResponse
		json.Unmarshal(rec.Body.Bytes(), &res)
		res.Lease = "L1"
		json.NewEncoder(w).Encode(res)
		return
	}
	l.fakeDeltaServer.ServeHTTP(w, r)
}

func runLease(t *testing.T, l *leaseServer, takeover bool) error {
	t.Helper()
	dir, p := deltaFixture(t, 3)
	srv := httptest.NewServer(l)
	defer srv.Close()
	_, _, err := deltaDeploy(&cliConfig{Server: srv.URL}, "demo", "tok", dir, p, takeover)
	return err
}

// The plan's lease travels on every later request of the deploy, re-plans
// included; --takeover only on the first plan (a re-plan must not break a lease
// someone else may have forced meanwhile).
func TestDeltaDeployCarriesTheLease(t *testing.T) {
	l := &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}, commitConflicts: 1}}
	if err := runLease(t, l, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/api/deploy/plan lease= takeover=true json=true",
		"/api/deploy/chunks lease=L1 takeover=false json=true",
		"/api/deploy/commit lease=L1 takeover=false json=true",
		"/api/deploy/plan lease=L1 takeover=false json=true",
		"/api/deploy/chunks lease=L1 takeover=false json=true",
		"/api/deploy/commit lease=L1 takeover=false json=true",
	}
	if strings.Join(l.seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(l.seen, "\n"), strings.Join(want, "\n"))
	}
}

// Someone else deploying: a clear message naming them, no retry.
func TestDeltaDeployLockedMessage(t *testing.T) {
	since := time.Now().UTC().Truncate(time.Minute) // today: just the local time
	l := &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}}, lockedOn: "/api/deploy/plan",
		locked: quick.DeployLocked{Error: quick.LeaseLocked, Holder: "mario@example.com", Since: since.Format(time.RFC3339), Message: "server text"}}
	err := runLease(t, l, false)
	want := "mario@example.com is publishing this site (since " + since.Local().Format("15:04") + "); retry in a moment or use --takeover"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if len(l.seen) != 1 {
		t.Fatalf("%d requests, want the plan only", len(l.seen))
	}
}

// The lease was lost mid-deploy (forced by someone else, expired): say so, no
// retry, nothing published.
func TestDeltaDeployLeaseLost(t *testing.T) {
	for _, path := range []string{"/api/deploy/chunks", "/api/deploy/commit"} {
		l := &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}}, lockedOn: path,
			locked: quick.DeployLocked{Error: quick.LeaseLost, Holder: "luigi@example.com", Since: time.Now().Format(time.RFC3339)}}
		err := runLease(t, l, false)
		if err == nil || !strings.Contains(err.Error(), "lost the site to luigi@example.com") || !strings.Contains(err.Error(), "nothing was published") {
			t.Fatalf("%s: err = %v", path, err)
		}
		if l.commits != 0 && path == "/api/deploy/chunks" {
			t.Fatalf("%s: committed after losing the lease", path)
		}
		if strings.Count(strings.Join(l.seen, "\n"), "/api/deploy/plan") != 1 {
			t.Fatalf("%s: re-planned after losing the lease:\n%s", path, strings.Join(l.seen, "\n"))
		}
	}
}

// A lease older than today shows the date too.
func TestLockedMessageOlderThanToday(t *testing.T) {
	since := time.Now().AddDate(0, 0, -3)
	e := &lockedError{quick.DeployLocked{Error: quick.LeaseLocked, Holder: "a@x", Since: since.UTC().Format(time.RFC3339)}}
	if want := "(since " + since.Format("Jan 2 15:04") + ")"; !strings.Contains(e.Error(), want) {
		t.Fatalf("%q, want %q", e.Error(), want)
	}
}

// The whole-folder upload and rollback get the same message, and send
// --force and the lease of a delta attempt (fallback on a busy server).
func TestTarDeployAndRollbackLocked(t *testing.T) {
	l := &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}}, lockedOn: "/api/deploy",
		locked: quick.DeployLocked{Error: quick.LeaseLocked, Holder: "token:ci@demo", Since: time.Now().Format(time.RFC3339)}}
	srv := httptest.NewServer(l)
	defer srv.Close()
	dir, p := deltaFixture(t, 1)
	_, err := tarDeploy(&cliConfig{Server: srv.URL}, "demo", "tok", dir, p, true, "L9")
	if err == nil || !strings.Contains(err.Error(), "token:ci@demo is publishing this site") {
		t.Fatalf("tar: err = %v", err)
	}
	if l.seen[0] != "/api/deploy lease=L9 takeover=true json=true" {
		t.Fatalf("tar request: %s", l.seen[0])
	}

	l.lockedOn = "/api/site/demo/rollback"
	_, err = postRollback(&cliConfig{Server: srv.URL}, "demo", "tok", false)
	if err == nil || !strings.Contains(err.Error(), "token:ci@demo is publishing this site") {
		t.Fatalf("rollback: err = %v", err)
	}
	if l.seen[1] != "/api/site/demo/rollback lease= takeover=false json=true" {
		t.Fatalf("rollback request: %s", l.seen[1])
	}
}
