package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	failOn   string   // endpoint answering 500
	locked   quick.DeployLocked
}

func (l *leaseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.seen = append(l.seen, fmt.Sprintf("%s lease=%s takeover=%v json=%v", r.URL.Path, r.Header.Get(quick.DeployLeaseHeader),
		r.URL.Query().Get("takeover") == "1", strings.Contains(r.Header.Get("Accept"), "application/json")))
	locked, failed := r.URL.Path == l.lockedOn, r.URL.Path == l.failOn
	l.mu.Unlock()
	if r.URL.Path == "/api/deploy/lease" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if failed {
		http.Error(w, "disk on fire", http.StatusInternalServerError)
		return
	}
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

func (l *leaseServer) requests() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.seen, "\n")
}

// A deploy that gives up after getting the site gives it back right away;
// one that succeeds doesn't need to (the commit released it).
func TestDeltaDeployReleasesOnFailure(t *testing.T) {
	for path, wantRelease := range map[string]bool{"/api/deploy/chunks": true, "/api/deploy/commit": true, "": false} {
		l := &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}}, failOn: path}
		err := runLease(t, l, false)
		if (err != nil) != (path != "") {
			t.Fatalf("fail on %q: err = %v", path, err)
		}
		got := strings.Contains(l.requests(), "/api/deploy/lease lease=L1")
		if got != wantRelease {
			t.Fatalf("fail on %q: released %v, want %v\n%s", path, got, wantRelease, l.requests())
		}
		if held.held() {
			t.Fatalf("fail on %q: lease still recorded as held", path)
		}
	}
}

// Ctrl-C (or SIGTERM) while holding a lease: the CLI releases it before
// exiting with the interrupted status, so a rerun doesn't have to wait.
func TestInterruptReleasesTheLease(t *testing.T) {
	l := &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}}}
	srv := httptest.NewServer(l)
	defer srv.Close()
	for sig, wantCode := range map[os.Signal]int{os.Interrupt: 130, syscall.SIGTERM: 143} {
		var a activeLease
		a.set(&cliConfig{Server: srv.URL}, "tok", "demo", "L7")
		sigs := make(chan os.Signal, 1)
		exited := make(chan int, 1)
		go watchInterrupts(sigs, &a, func(code int) { exited <- code })
		sigs <- sig
		select {
		case code := <-exited:
			if code != wantCode {
				t.Fatalf("%v: exit %d, want %d", sig, code, wantCode)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%v: no exit", sig)
		}
		if !strings.Contains(l.requests(), "/api/deploy/lease lease=L7") {
			t.Fatalf("%v: no release:\n%s", sig, l.requests())
		}
		l.mu.Lock()
		l.seen = nil
		l.mu.Unlock()
	}
	// Nothing held: just exit.
	var a activeLease
	sigs := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	go watchInterrupts(sigs, &a, func(code int) { exited <- code })
	sigs <- os.Interrupt
	if code := <-exited; code != 130 || l.requests() != "" {
		t.Fatalf("exit %d, requests %q", code, l.requests())
	}
}

// Busy server after the lease was acquired: the whole-folder fallback carries
// the lease; if it fails before the server takes it over (here a file vanished
// while building the tar), the lease is released, not just forgotten.
func TestTarFallbackFailureReleasesTheLease(t *testing.T) {
	dir, p := deltaFixture(t, 2)
	plans := 0
	l := &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}, commitConflicts: 1}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/deploy/plan" {
			if plans++; plans == 2 { // re-plan: busy, and the folder changes meanwhile
				os.Remove(filepath.Join(dir, p.files[0].rel))
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
		}
		l.ServeHTTP(w, r)
	}))
	defer srv.Close()
	_, _, delta, err := publishFolder(&cliConfig{Server: srv.URL, DeltaDeploy: true}, "demo", "tok", dir, p, false)
	if err == nil || delta {
		t.Fatalf("publish: delta %v err %v, want a failed tar fallback", delta, err)
	}
	if !strings.Contains(l.requests(), "/api/deploy/lease lease=L1") {
		t.Fatalf("lease not released after the failed fallback (%v):\n%s", err, l.requests())
	}
	if held.held() {
		t.Fatal("lease still recorded as held")
	}
}

// A big refused upload may see the connection closed instead of the 423 (the
// server reads only part of a refused body): the CLI asks again with an empty
// batch to learn why, and shows the lease message.
func TestUploadResetShowsTheLeaseMessage(t *testing.T) {
	dir, p := deltaFixture(t, 2)
	l := &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/deploy/chunks" {
			if r.ContentLength > 40 { // a real batch: drop the connection
				conn, _, _ := http.NewResponseController(w).Hijack()
				conn.Close()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusLocked)
			json.NewEncoder(w).Encode(quick.DeployLocked{Error: quick.LeaseLost, Holder: "luigi@example.com"})
			return
		}
		l.ServeHTTP(w, r)
	}))
	defer srv.Close()
	_, _, err := deltaDeploy(&cliConfig{Server: srv.URL}, "demo", "tok", dir, p, false)
	if err == nil || !strings.Contains(err.Error(), "lost the site to luigi@example.com") {
		t.Fatalf("err = %v, want the lease message", err)
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
