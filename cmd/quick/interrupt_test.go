package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
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
	wg       sync.WaitGroup              // the publish goroutine
	partial  map[string]bool             // held paths answering 200 with half a body first
	respond  map[string]http.HandlerFunc // paths answered by these instead
}

func newInterruptRig(t *testing.T, hold ...string) *interruptRig {
	t.Helper()
	g := &interruptRig{
		t: t, l: &leaseServer{fakeDeltaServer: &fakeDeltaServer{have: map[string]bool{}}},
		sigs: make(chan os.Signal, 2), exits: make(chan int, 2),
		hold: map[string]chan struct{}{}, arrived: map[string]chan struct{}{},
		partial: map[string]bool{}, respond: map[string]http.HandlerFunc{},
	}
	for _, p := range hold {
		g.hold[p], g.arrived[p] = make(chan struct{}), make(chan struct{})
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close) // runs last
	g.in = newInterrupts(g.sigs, func(code int) { g.exits <- code })
	intr = g.in
	t.Cleanup(func() { g.in.stop(); intr = nil; held.clear() })
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
	if h := g.respond[r.URL.Path]; h != nil {
		io.Copy(io.Discard, r.Body)
		h(w, r)
		return
	}
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
	if !strings.Contains(interruptedReport(pub.get()), "nothing was published") {
		t.Fatalf("report: %q", interruptedReport(pub.get()))
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
	if !strings.Contains(interruptedReport(pub.get()), "the publish had already started and may have completed") {
		t.Fatalf("report: %q", interruptedReport(pub.get()))
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

type slowFile struct{ io.ReadCloser }

func (s slowFile) Read(p []byte) (int, error) {
	time.Sleep(20 * time.Millisecond)
	return s.ReadCloser.Read(p[:min(len(p), 1)])
}

// Ctrl-C while the whole-folder tar is still being built: packaging stops,
// nothing was sent, so nothing was published.
func TestInterruptWhilePackagingTheFolder(t *testing.T) {
	g := newInterruptRig(t)
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/big.bin", make([]byte, 1000), 0o644); err != nil { // ~20 s at this pace
		t.Fatal(err)
	}
	p, err := buildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func(f func(string) (io.ReadCloser, error)) { openForTar = f }(openForTar)
	openForTar = func(path string) (io.ReadCloser, error) {
		f, err := os.Open(path)
		return slowFile{f}, err
	}
	done := make(chan error, 1)
	go func() {
		_, err := tarDeploy(&cliConfig{Server: g.srv.URL}, "demo", "tok", dir, p, false, "")
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	g.sigs <- os.Interrupt
	err = waitErr(t, done, time.Second)
	if !errors.Is(err, errInterrupted) || errors.Is(err, errPublishStarted) {
		t.Fatalf("err = %v, want interrupted before publishing", err)
	}
	if g.log() != "" {
		t.Fatalf("requests sent: %s", g.log())
	}
}

// SIGTERM during the visibility change that follows a deploy (--public): the
// site is published, the change interrupted; the command exits 143 and says
// exactly that.
func TestInterruptDuringVisibilityChange(t *testing.T) {
	g := newInterruptRig(t, "/api/site/demo/policy")
	dir, p := deltaFixture(t, 1)
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runPublish(g.in, &cliConfig{Server: g.srv.URL, DeltaDeploy: true}, "demo", "tok", dir, p,
			deployOpts{public: true, save: func() {}}, &out, &errOut)
	}()
	<-g.arrived["/api/site/demo/policy"]
	g.sigs <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 143 {
			t.Fatalf("exit %d, want 143", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still running")
	}
	if !strings.Contains(out.String(), "published") || !strings.Contains(errOut.String(), "published; visibility change interrupted") {
		t.Fatalf("out %q\nerr %q", out.String(), errOut.String())
	}
}

// A signal after everything is done is still honored at the end.
func TestSignalAfterPublishIsHonored(t *testing.T) {
	g := newInterruptRig(t)
	dir, p := deltaFixture(t, 1)
	var out, errOut bytes.Buffer
	saved := func() { g.sigs <- os.Interrupt; time.Sleep(50 * time.Millisecond) }
	code := runPublish(g.in, &cliConfig{Server: g.srv.URL, DeltaDeploy: true}, "demo", "tok", dir, p,
		deployOpts{save: saved}, &out, &errOut)
	if code != 130 || !strings.Contains(out.String(), "published") {
		t.Fatalf("exit %d, out %q", code, out.String())
	}
}

// The handler lives as long as the command: stop ends the watcher (fired or
// not) and gives signals back to their default handling.
func TestInterruptsStop(t *testing.T) {
	for _, fire := range []bool{false, true} {
		sigs := make(chan os.Signal, 2)
		in := newInterrupts(sigs, func(int) { t.Error("exit called") })
		if fire {
			sigs <- os.Interrupt
			for in.code() == 0 {
				time.Sleep(time.Millisecond)
			}
		}
		in.stop()
		select {
		case <-in.finished:
		case <-time.After(time.Second):
			t.Fatalf("fired %v: watcher still running", fire)
		}
	}
	defer func(f func(chan<- os.Signal)) { signalStop = f }(signalStop)
	var stopped bool
	signalStop = func(c chan<- os.Signal) { stopped = true; signal.Stop(c) }
	in := catchInterrupts()
	in.stop()
	if !stopped || intr != nil {
		t.Fatalf("signal.Stop called %v, intr %v", stopped, intr)
	}
}

// runRig runs the whole deploy command body against the rig.
func (g *interruptRig) run() (int, string, string) {
	dir, p := deltaFixture(g.t, 1)
	var out, errOut bytes.Buffer
	code := runPublish(g.in, &cliConfig{Server: g.srv.URL, DeltaDeploy: true}, "demo", "tok", dir, p,
		deployOpts{save: func() {}}, &out, &errOut)
	return code, out.String(), errOut.String()
}

// Codex's reproduction: the commit was applied but its answer is lost (cut
// short, no signal yet); SIGTERM arrives while the CLI gives the lease back.
// The publish may have happened: never "nothing was published".
func TestLostCommitAnswerThenSignalDuringRelease(t *testing.T) {
	g := newInterruptRig(t, "/api/deploy/lease")
	g.respond["/api/deploy/commit"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"site":"de`)) // and the connection closes
	}
	go func() {
		<-g.arrived["/api/deploy/lease"]
		g.sigs <- syscall.SIGTERM
		time.Sleep(50 * time.Millisecond)
		g.let("/api/deploy/lease")
	}()
	code, _, errOut := g.run()
	if code != 143 || !strings.Contains(errOut, "may have completed") || strings.Contains(errOut, "nothing was published") {
		t.Fatalf("exit %d, err %q", code, errOut)
	}
}

// Without any signal, the outcome still decides the wording: a refusal
// published nothing, anything else after the commit was sent may have
// published.
func TestPublishOutcomeWording(t *testing.T) {
	answer := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(body))
		}
	}
	uncertain := map[string]http.HandlerFunc{
		"malformed 200": answer(http.StatusOK, "not json"),
		"500":           answer(http.StatusInternalServerError, "boom"),
		"502":           answer(http.StatusBadGateway, "proxy"),
	}
	refused := map[string]http.HandlerFunc{}
	for _, c := range []int{400, 401, 403, 409, 413, 423} {
		refused[http.StatusText(c)] = answer(c, "no")
	}
	for name, h := range uncertain {
		g := newInterruptRig(t)
		g.respond["/api/deploy/commit"] = h
		code, _, errOut := g.run()
		if code != 1 || !strings.Contains(errOut, "the deploy may have been published; check the site") {
			t.Errorf("%s: exit %d, err %q", name, code, errOut)
		}
	}
	for name, h := range refused {
		g := newInterruptRig(t)
		g.respond["/api/deploy/commit"] = h
		code, _, errOut := g.run()
		if code != 1 || strings.Contains(errOut, "may have") {
			t.Errorf("%s: exit %d, err %q", name, code, errOut)
		}
		// Interrupted after the refusal (while giving the lease back): still
		// nothing published.
		g = newInterruptRig(t, "/api/deploy/lease")
		g.respond["/api/deploy/commit"] = h
		go func() {
			<-g.arrived["/api/deploy/lease"]
			g.sigs <- os.Interrupt
			time.Sleep(50 * time.Millisecond)
			g.let("/api/deploy/lease")
		}()
		if code, _, errOut := g.run(); code != 130 || !strings.Contains(errOut, "nothing was published") {
			t.Errorf("%s then Ctrl-C: exit %d, err %q", name, code, errOut)
		}
	}
}

// Published but not recorded (200 with a warning): a success that says so.
func TestPublishedWithWarning(t *testing.T) {
	g := newInterruptRig(t)
	g.respond["/api/deploy/commit"] = func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"site":"demo","url":"https://demo.example","warning":"details not saved"}`))
	}
	code, out, errOut := g.run()
	if code != 0 || !strings.Contains(out, "published") || !strings.Contains(errOut, "warning: details not saved") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
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
