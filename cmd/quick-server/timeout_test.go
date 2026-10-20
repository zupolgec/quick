package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

// stalledPost sends a request whose body stops after prefix bytes (the pipe
// stays open). It returns a channel with the status (0 on a transport error)
// and the writer, to close when the test is over.
func stalledPost(t *testing.T, url, lease string, prefix []byte) (<-chan int, *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, url, pr)
		if lease != "" {
			req.Header.Set(quick.DeployLeaseHeader, lease)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	go pw.Write(prefix)
	return done, pw
}

func within(t *testing.T, what string, d time.Duration, ch <-chan int) int {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("%s: still running after %v", what, d)
		return 0
	}
}

func (t *leaseTable) inflightOf(site string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if l := t.m[site]; l != nil {
		return l.inflight
	}
	return -1
}

// A client that starts an upload and stops sending can't keep the lease
// forever: the request times out, and then the lease idles out as usual.
func TestStalledUploadLetsTheLeaseExpire(t *testing.T) {
	s := newDeltaTestServer(t)
	s.timeouts.idle = 200 * time.Millisecond
	clk := withClock(s)
	srv := httptest.NewServer(s.apexMux)
	t.Cleanup(srv.Close)
	a := &deltaClient{t: t, s: s, site: "demo"}
	a.started(map[string][]byte{"index.html": []byte("a")})

	done, _ := stalledPost(t, srv.URL+"/api/deploy/chunks?name=demo", a.lease, gzipOf([]byte("partial"))[:10])
	for i := 0; s.leases.inflightOf("demo") != 1; i++ {
		if i > 200 {
			t.Fatal("upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	within(t, "stalled upload", 3*time.Second, done)
	if n := s.leases.inflightOf("demo"); n != 0 {
		t.Fatalf("in flight after the timeout: %d", n)
	}
	clk.add(leaseIdle + time.Second)
	(&deltaClient{t: t, s: s, site: "demo"}).leased()
}

// A stalled commit or tar upload holds the site lock only until it times out:
// delete (or anything else needing the lock) goes through.
func TestStalledPublishReleasesTheSiteLock(t *testing.T) {
	for _, path := range []string{"/api/deploy", "/api/deploy/commit"} {
		s := newDeltaTestServer(t)
		s.timeouts.idle = 200 * time.Millisecond
		srv := httptest.NewServer(s.apexMux)
		t.Cleanup(srv.Close)
		a := &deltaClient{t: t, s: s, site: "demo"}
		a.deploy(map[string][]byte{"index.html": []byte("v1")})
		lease := ""
		if path == "/api/deploy/commit" {
			a.started(map[string][]byte{"index.html": []byte("v2")})
			lease = a.lease
		}
		done, _ := stalledPost(t, srv.URL+path+"?name=demo", lease, []byte("{"))
		time.Sleep(50 * time.Millisecond) // let it take the lock
		deleted := make(chan int, 1)
		go func() { deleted <- a.do(http.MethodDelete, "/api/site/demo", nil, nil).Code }()
		if code := within(t, path+": delete behind a stalled publish", 3*time.Second, deleted); code != http.StatusOK {
			t.Fatalf("%s: delete: %d", path, code)
		}
		within(t, path, 3*time.Second, done)
	}
}

// A body trickling just fast enough to beat the idle timeout still ends at
// the overall cap.
func TestTricklingUploadHitsTheCap(t *testing.T) {
	s := newDeltaTestServer(t)
	s.timeouts.idle = 300 * time.Millisecond
	s.timeouts.upload = 800 * time.Millisecond
	srv := httptest.NewServer(s.apexMux)
	t.Cleanup(srv.Close)
	start := time.Now()
	done, pw := stalledPost(t, srv.URL+"/api/deploy?name=demo", "", nil)
	// A valid tar.gz that never ends: a file header, then a byte every 50 ms,
	// each flushed (well within the idle timeout).
	go func() {
		gz := gzip.NewWriter(pw)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: 1 << 20, Typeflag: tar.TypeReg})
		for range 200 {
			if _, err := tw.Write([]byte{'x'}); err != nil {
				return
			}
			if err := gz.Flush(); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	within(t, "trickling upload", 5*time.Second, done)
	if took := time.Since(start); took < 700*time.Millisecond || took > 3*time.Second {
		t.Fatalf("trickling upload ended after %v, want at the %v cap", took, s.timeouts.upload)
	}
	// It held the site lock and a lease: both are free again.
	deleted := make(chan int, 1)
	go func() {
		deleted <- (&deltaClient{t: t, s: s, site: "demo"}).do(http.MethodDelete, "/api/site/demo", nil, nil).Code
	}()
	within(t, "delete after the capped upload", time.Second, deleted)
	(&deltaClient{t: t, s: s, site: "demo"}).leased()
}

// stuckWriter is a client that never reads its response: Write blocks until
// the write deadline (none set: until the test ends).
type stuckWriter struct {
	hdr      http.Header
	mu       sync.Mutex
	deadline time.Time
	stop     chan struct{}
}

func (s *stuckWriter) Header() http.Header             { return s.hdr }
func (s *stuckWriter) WriteHeader(int)                 {}
func (s *stuckWriter) SetReadDeadline(time.Time) error { return nil }
func (s *stuckWriter) SetWriteDeadline(d time.Time) error {
	s.mu.Lock()
	s.deadline = d
	s.mu.Unlock()
	return nil
}

func (s *stuckWriter) Write(p []byte) (int, error) {
	for {
		s.mu.Lock()
		d := s.deadline
		s.mu.Unlock()
		if !d.IsZero() && time.Now().After(d) {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-s.stop:
			return 0, io.ErrClosedPipe
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A plan answer the client never reads can't pin the lease: the write times
// out, a lease this request opened is released (the client never learned its
// id), a renewed one idles out as usual.
func TestUnreadPlanResponseDoesNotPinTheLease(t *testing.T) {
	s := newDeltaTestServer(t)
	s.timeouts.write = 200 * time.Millisecond
	clk := withClock(s)
	plan := func(lease string) <-chan int {
		w := &stuckWriter{hdr: http.Header{}, stop: make(chan struct{})}
		t.Cleanup(func() { close(w.stop) })
		r := httptest.NewRequest(http.MethodPost, "/api/deploy/plan?name=demo", bytes.NewReader(mustJSON(quick.DeployManifest{})))
		if lease != "" {
			r.Header.Set(quick.DeployLeaseHeader, lease)
		}
		done := make(chan int, 1)
		go func() { s.apexMux.ServeHTTP(w, r); done <- 1 }()
		return done
	}
	// First plan, never read: the site is free right after the write timeout.
	within(t, "unread first plan", 3*time.Second, plan(""))
	b := &deltaClient{t: t, s: s, site: "demo"}
	b.leased()

	// A re-plan never read: in flight no more, so the lease idles out.
	within(t, "unread re-plan", 3*time.Second, plan(b.lease))
	if n := s.leases.inflightOf("demo"); n != 0 {
		t.Fatalf("in flight after the write timeout: %d", n)
	}
	clk.add(leaseIdle + time.Second)
	(&deltaClient{t: t, s: s, site: "demo"}).leased()
}

// Even the early answers of the deploy endpoints (wrong method, delta
// deploy off) are written under the deadline.
func TestEarlyDeployAnswersHaveAWriteDeadline(t *testing.T) {
	s := newDeltaTestServer(t)
	s.timeouts.write = 200 * time.Millisecond
	off := newDeltaTestServer(t)
	off.timeouts.write = 200 * time.Millisecond
	off.stage = nil
	off.apexMux = off.buildApexMux()
	cases := []struct {
		s            *server
		method, path string
	}{
		{s, http.MethodGet, "/api/deploy/plan"},
		{s, http.MethodGet, "/api/deploy/chunks"},
		{s, http.MethodGet, "/api/deploy/commit"},
		{s, http.MethodGet, "/api/deploy"},
		{s, http.MethodGet, "/api/deploy/lease"},
		{off, http.MethodPost, "/api/deploy/plan"},
	}
	for _, c := range cases {
		w := &stuckWriter{hdr: http.Header{}, stop: make(chan struct{})}
		r := httptest.NewRequest(c.method, c.path+"?name=demo", nil)
		done := make(chan int, 1)
		go func() { c.s.apexMux.ServeHTTP(w, r); done <- 1 }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			close(w.stop)
			t.Fatalf("%s %s: answer not bounded by the write deadline", c.method, c.path)
		}
		close(w.stop)
	}
}

// Refused requests read only a bounded part of their body, for a bounded
// time, before answering.
func TestRefusedDrainIsBounded(t *testing.T) {
	s := newDeltaTestServer(t)
	s.timeouts.drain = 200 * time.Millisecond
	srv := httptest.NewServer(s.apexMux)
	t.Cleanup(srv.Close)
	a := &deltaClient{t: t, s: s, site: "demo"}
	a.started(map[string][]byte{"index.html": []byte("a")})

	// Stalled body: answered once the drain time is up.
	done, _ := stalledPost(t, srv.URL+"/api/deploy?name=demo", "", make([]byte, 100<<10))
	if code := within(t, "refused stalled upload", 3*time.Second, done); code != http.StatusLocked {
		t.Fatalf("refused stalled upload: %d, want 423", code)
	}
	// Endless body: the server stops reading after the cap.
	done, pw := stalledPost(t, srv.URL+"/api/deploy?name=demo", "", nil)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, err := pw.Write(buf); err != nil {
				return
			}
		}
	}()
	within(t, "refused endless upload", 3*time.Second, done)
}

func TestServerHasTimeouts(t *testing.T) {
	hs := newHTTPServer(":0", http.NotFoundHandler())
	if hs.ReadHeaderTimeout <= 0 || hs.IdleTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout %v IdleTimeout %v", hs.ReadHeaderTimeout, hs.IdleTimeout)
	}
}
