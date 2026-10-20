package main

import (
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zupolgec/quick/internal/s3fake"
	"github.com/zupolgec/quick/internal/storage"
)

// newS3TestServer is newDeltaTestServer on the real s3 backend, talking to an
// in-memory S3 server.
func newS3TestServer(t *testing.T) (*server, *s3fake.Server) {
	t.Helper()
	srv := s3fake.New("bucket")
	t.Cleanup(srv.Close)
	st, err := storage.New(storage.Config{Kind: "s3", S3: storage.S3Config{
		Endpoint: srv.Endpoint(), Region: "us-east-1", Bucket: "bucket", AccessKey: "k", SecretKey: "s",
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := newDeltaTestServer(t)
	s.store = st
	s.meta = newMetaStore(st, []byte("secret"), 0)
	s.rules = newRulesStore(st, 0)
	return s, srv
}

// liveObjects is the part of the bucket readers can see: pointers, trees,
// legacy objects (metadata is rewritten by every publish attempt).
func liveObjects(srv *s3fake.Server) map[string]string {
	out := map[string]string{}
	for k, v := range srv.Snapshot() {
		if !strings.HasPrefix(k, "meta/") {
			out[k] = v
		}
	}
	return out
}

func failNthTreePut(srv *s3fake.Server, n int32) {
	var count atomic.Int32
	srv.Hook = func(method, key string) s3fake.Fault {
		if method == http.MethodPut && strings.HasPrefix(key, "versions/") && count.Add(1) == n {
			return s3fake.Fail
		}
		return s3fake.Pass
	}
}

// Delta deploys on s3 publish atomically: reused chunks are read from the live
// version while the new one is written apart (nothing copied into staging),
// and the site serves the new version.
func TestS3DeltaDeployEndToEnd(t *testing.T) {
	s, srv := newS3TestServer(t)
	if !s.store.AtomicPutSite() {
		t.Fatal("s3 backend must be atomic")
	}
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(7, 1<<20)
	c.deploy(map[string][]byte{"index.html": []byte("one"), "big.bin": big})

	var during int64 = -1
	beforePublish = func(site string) { during = s.stage.used(site) }
	defer func() { beforePublish = nil }()
	sent := c.deploy(map[string][]byte{"index.html": []byte("two"), "big.bin": big})
	if sent != int64(len("two")) {
		t.Fatalf("second deploy sent %d bytes, want only the changed file", sent)
	}
	if during != charge(len("two")) {
		t.Fatalf("staging during the commit: %d bytes, want only the upload (%d): live chunks must not be copied", during, charge(len("two")))
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "two" {
		t.Fatalf("index.html = %q", got)
	}
	if got := readSite(t, s, "demo", "big.bin"); string(got) != string(big) {
		t.Fatal("big.bin corrupted")
	}
	if ks := srv.Keys("sites/"); len(ks) != 0 {
		t.Fatalf("flat objects written: %v", ks)
	}
	// Rollback through the API, and a delta deploy after it.
	if w := c.do(http.MethodPost, "/api/site/demo/rollback", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", w.Code, w.Body.String())
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "one" {
		t.Fatalf("after rollback index.html = %q", got)
	}
	if sent := c.deploy(map[string][]byte{"index.html": []byte("one"), "big.bin": big, "new.txt": []byte("n")}); sent != 1 {
		t.Fatalf("deploy after rollback sent %d bytes, want 1 (the index follows the rollback)", sent)
	}
	if w := c.do(http.MethodPost, "/api/site/demo/rollback", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("second rollback: %d", w.Code)
	}
	if _, _, err := s.store.OpenFile("demo", "new.txt"); err == nil {
		t.Fatal("rollback did not restore the previous version")
	}
}

// A publish failing midway on s3 is a refusal (4xx, what the CLI reports as
// "nothing published") and the live site is untouched; the recorded index
// versions come back, so the next plan needs no rebuild.
func TestS3PublishFailureIsARefusalAndLiveUnchanged(t *testing.T) {
	for _, tarUpload := range []bool{false, true} {
		s, srv := newS3TestServer(t)
		c := &deltaClient{t: t, s: s, site: "demo"}
		v1 := map[string][]byte{"index.html": []byte("one"), "a.css": []byte("css")}
		c.deploy(v1)
		before := liveObjects(srv)
		p1, _ := s.meta.loadFresh("demo")

		failNthTreePut(srv, 2)
		v2 := map[string][]byte{"index.html": []byte("two"), "a.css": []byte("css2"), "b.js": []byte("js")}
		var code int
		var body string
		if tarUpload {
			code, body = tarDeployCode(c, v2)
		} else {
			m, data := manifestOf(t, v2)
			_, pl := c.plan(m)
			c.upload(data, pl.Missing)
			w := c.commit(m)
			code, body = w.Code, w.Body.String()
		}
		srv.Hook = nil
		if code < 400 || code >= 500 {
			t.Fatalf("tar %v: failed publish answered %d %q, want a 4xx refusal", tarUpload, code, body)
		}
		if after := liveObjects(srv); !maps.Equal(before, after) {
			t.Fatalf("tar %v: bucket changed:\nbefore %v\nafter  %v", tarUpload, before, after)
		}
		for p, want := range v1 {
			if got := string(readSite(t, s, "demo", p)); got != string(want) {
				t.Fatalf("tar %v: %s = %q", tarUpload, p, got)
			}
		}
		if p, _ := s.meta.loadFresh("demo"); p.Version != p1.Version || p.Version == "" {
			t.Fatalf("tar %v: index version %q, want %q restored", tarUpload, p.Version, p1.Version)
		}
		c.leased() // the failed deploy ended its lease
		c.lease = ""
	}
}

// A first deploy failing on s3 leaves nothing: no metadata, not listed, no
// objects.
func TestS3FailedFirstDeployLeavesNothing(t *testing.T) {
	s, srv := newS3TestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	failNthTreePut(srv, 2)
	m, data := manifestOf(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	_, pl := c.plan(m)
	c.upload(data, pl.Missing)
	if w := c.commit(m); w.Code != http.StatusBadRequest {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	srv.Hook = nil
	if ks := srv.Keys(""); len(ks) != 0 {
		t.Fatalf("left behind: %v", ks)
	}
}

// When the switch's outcome can't be known the answer is 5xx (the CLI says
// the outcome is uncertain), never a refusal.
func TestS3UncertainSwitchIsNotARefusal(t *testing.T) {
	s, srv := newS3TestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"index.html": []byte("one")})
	var switched atomic.Bool
	srv.Hook = func(method, key string) s3fake.Fault {
		if !strings.HasPrefix(key, "live/") {
			return s3fake.Pass
		}
		if method == http.MethodPut {
			switched.Store(true)
			return s3fake.ApplyAndDrop // applied, the answer lost
		}
		if switched.Load() {
			return s3fake.Fail
		}
		return s3fake.Pass
	}
	m, data := manifestOf(t, map[string][]byte{"index.html": []byte("two")})
	_, pl := c.plan(m)
	c.upload(data, pl.Missing)
	w := c.commit(m)
	srv.Hook = nil
	if w.Code < 500 {
		t.Fatalf("uncertain publish answered %d %q, want 5xx", w.Code, w.Body.String())
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "two" {
		t.Fatalf("index.html = %q (the switch was applied)", got)
	}
	// The next deploy works and doesn't trust a stale index.
	c.lease = ""
	c.deploy(map[string][]byte{"index.html": []byte("three")})
	if got := string(readSite(t, s, "demo", "index.html")); got != "three" {
		t.Fatalf("index.html = %q", got)
	}
}

// Legacy flat sites keep serving through the handlers and move to the
// versioned layout on their first deploy.
func TestS3LegacySiteServedAndMigratedByDeploy(t *testing.T) {
	s, srv := newS3TestServer(t)
	srv.Put("sites/old/index.html", []byte("legacy"))
	if err := s.meta.save("old", policy{Access: "public"}); err != nil {
		t.Fatal(err)
	}
	if w := hostGet(s, "old.quick.example.com", "/"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "legacy") {
		t.Fatalf("legacy site: %d %q", w.Code, w.Body.String())
	}
	c := &deltaClient{t: t, s: s, site: "old"}
	if sent := c.deploy(map[string][]byte{"index.html": []byte("legacy"), "new.html": []byte("new")}); sent != 3 {
		t.Fatalf("first deploy of a legacy site sent %d bytes, want 3 (index.html reused)", sent)
	}
	// The flat objects stay for the grace period (a request may still read
	// them) but are no longer served.
	srv.Put("sites/old/flat-only.html", []byte("flat"))
	if w := hostGet(s, "old.quick.example.com", "/new"); w.Code != http.StatusOK || w.Body.String() != "new" {
		t.Fatalf("after migration: %d %q", w.Code, w.Body.String())
	}
	if w := hostGet(s, "old.quick.example.com", "/flat-only"); w.Code == http.StatusOK {
		t.Fatal("flat layout still served after migration")
	}
	// Delete through the API removes every object of the site.
	if w := c.do(http.MethodDelete, "/api/site/old", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if ks := srv.Keys(""); len(ks) != 0 {
		t.Fatalf("left after delete: %v", ks)
	}
	if w := hostGet(s, "old.quick.example.com", "/new.html"); w.Code == http.StatusOK {
		t.Fatal("deleted site still served")
	}
}
