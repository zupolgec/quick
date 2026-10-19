package main

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	s.timeouts.upload = 500 * time.Millisecond
	srv := httptest.NewServer(s.apexMux)
	t.Cleanup(srv.Close)
	done, pw := stalledPost(t, srv.URL+"/api/deploy?name=demo", "", nil)
	go func() {
		for range 100 {
			if _, err := pw.Write([]byte{0}); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	within(t, "trickling upload", 3*time.Second, done)
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
