package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// interruptRig is a fake server around leaseServer whose endpoints can be
// held open, plus the CLI's interrupt state wired to a test signal channel.
type interruptRig struct {
	t        *testing.T
	l        *leaseServer
	srv      *httptest.Server
	in       *interrupts
	sigs     chan os.Signal
	exits    chan int
	mu       sync.Mutex
	events   []string
	hold     map[string]chan struct{} // path -> closed to let the request go on
	arrived  map[string]chan struct{} // path -> closed when it first arrives
	delDelay time.Duration
	wg       sync.WaitGroup  // the publish goroutine
	partial  map[string]bool // held paths answering 200 with half a body first
}

func newInterruptRig(t *testing.T, hold ...string) *interruptRig {
	t.Helper()
	g := &interruptRig{
		t: t, l: &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}}},
		sigs: make(chan os.Signal, 2), exits: make(chan int, 2),
		hold: map[string]chan struct{}{}, arrived: map[string]chan struct{}{},
		partial: map[string]bool{},
	}
	for _, p := range hold {
		g.hold[p], g.arrived[p] = make(chan struct{}), make(chan struct{})
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close) // runs last
	g.in = newInterrupts(g.sigs, func(code int) { g.exits <- code })
	intr = g.in
	t.Cleanup(func() { intr = nil; held.clear() })
	t.Cleanup(func() { // runs first: let held handlers and the deploy finish
		for _, c := range g.hold {
			select {
			case <-c:
			default:
				close(c)
			}
		}
		g.wg.Wait()
	})
	return g
}

func (g *interruptRig) event(s string) {
	g.mu.Lock()
	g.events = append(g.events, s)
	g.mu.Unlock()
}

func (g *interruptRig) log() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.events, " ")
}

func (g *interruptRig) serve(w http.ResponseWriter, r *http.Request) {
	g.event(r.URL.Path)
	if r.URL.Path == "/api/deploy/lease" {
		time.Sleep(g.delDelay)
		g.event("released")
	}
	if c, ok := g.hold[r.URL.Path]; ok {
		if g.partial[r.URL.Path] {
			// The server published: 200 and half the answer, then silence.
			io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"site":"de`))
			http.NewResponseController(w).Flush()
		}
		select {
		case <-g.arrived[r.URL.Path]:
		default:
			close(g.arrived[r.URL.Path])
		}
		<-c
		if g.partial[r.URL.Path] {
			return
		}
	}
	g.l.ServeHTTP(w, r)
}

func (g *interruptRig) let(path string) { close(g.hold[path]) }

// publish runs publishFolder in the background; the result comes on the
// channel, after which "returned" is logged.
func (g *interruptRig) publish() <-chan error {
	dir, p := deltaFixture(g.t, 2)
	done := make(chan error, 1)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		_, _, _, err := publishFolder(&cliConfig{Server: g.srv.URL, DeltaDeploy: true}, "demo", "tok", dir, p, false)
		g.event("returned")
		done <- err
	}()
	return done
}

func waitErr(t *testing.T, done <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("still running after %v", d)
		return nil
	}
}

// Codex's reproduction: Ctrl-C while a chunk upload is in flight, the release
// slow to arrive, then the upload answered OK. Nothing more is sent (no
// commit), the release reaches the server before the CLI is done, exit 130.
func TestInterruptDuringUploadStopsBeforeCommit(t *testing.T) {
	g := newInterruptRig(t, "/api/deploy/chunks")
	g.delDelay = 1300 * time.Millisecond
	done := g.publish()
	<-g.arrived["/api/deploy/chunks"]
	g.sigs <- os.Interrupt
	time.Sleep(100 * time.Millisecond)
	g.let("/api/deploy/chunks") // the upload would have succeeded
	err := waitErr(t, done, 5*time.Second)
	if !errors.Is(err, errInterrupted) || errors.Is(err, errPublishStarted) {
		t.Fatalf("err = %v, want interrupted before publishing", err)
	}
	log := g.log()
	if strings.Contains(log, "/api/deploy/commit") {
		t.Fatalf("commit sent after the interrupt: %s", log)
	}
	if !strings.Contains(log, "released returned") {
		t.Fatalf("the CLI was done before the release reached the server: %s", log)
	}
	if code := g.in.code(); code != 130 {
		t.Fatalf("exit code %d, want 130", code)
	}
	if !strings.Contains(interruptedReport(err), "nothing was published") {
		t.Fatalf("report: %q", interruptedReport(err))
	}
}

// SIGTERM while the commit is on its way: the publish may complete on the
// server, the CLI says so, releases, and exits 143.
func TestInterruptDuringCommit(t *testing.T) {
	g := newInterruptRig(t, "/api/deploy/commit")
	done := g.publish()
	<-g.arrived["/api/deploy/commit"]
	g.sigs <- syscall.SIGTERM
	err := waitErr(t, done, 5*time.Second)
	if !errors.Is(err, errPublishStarted) {
		t.Fatalf("err = %v, want publish-started", err)
	}
	if !strings.Contains(interruptedReport(err), "the publish had already started and may have completed") {
		t.Fatalf("report: %q", interruptedReport(err))
	}
	if !strings.Contains(g.log(), "released returned") {
		t.Fatalf("no release before returning: %s", g.log())
	}
	if code := g.in.code(); code != 143 {
		t.Fatalf("exit code %d, want 143", code)
	}
}

// The server published and started answering 200 when the signal arrives:
// the answer is cut short, but the outcome is uncertain, never "nothing was
// published". For the commit and for the whole-folder upload.
func TestInterruptDuringPublishAnswer(t *testing.T) {
	for _, path := range []string{"/api/deploy/commit", "/api/deploy"} {
		g := newInterruptRig(t, path)
		g.partial[path] = true
		var done <-chan error
		if path == "/api/deploy" {
			dir, p := deltaFixture(t, 1)
			ch := make(chan error, 1)
			go func() {
				_, err := tarDeploy(&cliConfig{Server: g.srv.URL}, "demo", "tok", dir, p, false, "")
				ch <- err
			}()
			done = ch
		} else {
			done = g.publish()
		}
		<-g.arrived[path]
		time.Sleep(200 * time.Millisecond) // the CLI has the 200 headers, is reading the body
		g.sigs <- syscall.SIGTERM
		err := waitErr(t, done, 5*time.Second)
		if !errors.Is(err, errPublishStarted) {
			t.Fatalf("%s: err = %v, want publish-started", path, err)
		}
	}
}

// A second Ctrl-C while the release is slow exits at once.
func TestSecondInterruptExitsAtOnce(t *testing.T) {
	g := newInterruptRig(t, "/api/deploy/chunks")
	g.delDelay = 3 * time.Second
	g.publish()
	<-g.arrived["/api/deploy/chunks"]
	g.sigs <- os.Interrupt
	time.Sleep(200 * time.Millisecond) // releasing now
	start := time.Now()
	g.sigs <- os.Interrupt
	select {
	case code := <-g.exits:
		if code != 130 || time.Since(start) > time.Second {
			t.Fatalf("exit %d after %v", code, time.Since(start))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Ctrl-C didn't exit")
	}
}

// Ctrl-C while the first plan is on its way: the server may already have
// opened the lease, so the CLI waits (briefly) for the answer, releases the
// id it brings and stops there.
func TestInterruptDuringPlanReleasesTheLease(t *testing.T) {
	g := newInterruptRig(t, "/api/deploy/plan")
	done := g.publish()
	<-g.arrived["/api/deploy/plan"]
	g.sigs <- os.Interrupt
	time.Sleep(100 * time.Millisecond)
	g.let("/api/deploy/plan")
	if err := waitErr(t, done, 5*time.Second); !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v", err)
	}
	log := g.log()
	if !strings.Contains(g.l.requests(), "/api/deploy/lease lease=L1") || strings.Contains(log, "chunks") || strings.Contains(log, "commit") {
		t.Fatalf("after an interrupt during the plan: %s\n%s", log, g.l.requests())
	}
}

// The wait for a plan in flight is bounded.
func TestInterruptDuringSlowPlanStopsAnyway(t *testing.T) {
	defer func(d time.Duration) { planWait = d }(planWait)
	planWait = 100 * time.Millisecond
	g := newInterruptRig(t, "/api/deploy/plan")
	done := g.publish()
	<-g.arrived["/api/deploy/plan"]
	g.sigs <- os.Interrupt
	if err := waitErr(t, done, time.Second); !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v", err)
	}
}
