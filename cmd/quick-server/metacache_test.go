package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/storage"
)

// slowMeta delays GetMeta after reading: an unlocked cache-miss load that
// read old metadata and resumes after a locked writer saved newer metadata.
type slowMeta struct {
	storage.Backend
	mu      sync.Mutex
	gate    chan struct{}
	reading chan struct{}
}

func (m *slowMeta) GetMeta(site string) ([]byte, bool, error) {
	b, ok, err := m.Backend.GetMeta(site)
	m.mu.Lock()
	gate, reading := m.gate, m.reading
	m.gate, m.reading = nil, nil
	m.mu.Unlock()
	if gate != nil {
		close(reading)
		<-gate
	}
	return b, ok, err
}

// pauseNextGetMeta makes the next GetMeta block after reading until the
// returned release is called; started is closed once it has read.
func (m *slowMeta) pauseNextGetMeta() (started <-chan struct{}, release func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gate, m.reading = make(chan struct{}), make(chan struct{})
	gate := m.gate
	return m.reading, func() { close(gate) }
}

// Finding 3: a load that read old metadata must not overwrite the cache after
// a newer save.
func TestMetaCacheNotOverwrittenByStaleLoad(t *testing.T) {
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: t.TempDir(), MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	slow := &slowMeta{Backend: st}
	ms := newMetaStore(slow, []byte("secret"), time.Minute)
	if err := ms.save("demo", policy{Access: "public"}); err != nil {
		t.Fatal(err)
	}
	ms.forget("demo")
	started, release := slow.pauseNextGetMeta()
	done := make(chan struct{})
	go func() { defer close(done); ms.load("demo") }()
	<-started
	if err := ms.save("demo", policy{Access: "code", CodeHash: "x"}); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	if p, _ := ms.load("demo"); p.Access != "code" {
		t.Fatalf("cache holds stale policy %+v after a newer save", p)
	}
}

// Finding 3: rebuilding the index must never resurrect a revoked token, even
// with a stale cached policy (or metadata changed behind the cache).
func TestIndexRebuildDoesNotResurrectRevokedToken(t *testing.T) {
	for _, mode := range []string{"stale load", "changed behind cache"} {
		t.Run(mode, func(t *testing.T) {
			s := newDeltaTestServer(t)
			slow := &slowMeta{Backend: s.store}
			s.meta = newMetaStore(slow, []byte("secret"), time.Minute)
			c := &deltaClient{t: t, s: s, site: "demo"}
			files := map[string][]byte{"index.html": []byte("v1")}
			c.deploy(files)
			p, _ := s.meta.load("demo")
			p.Tokens = []siteToken{{ID: "t1", Name: "ci", Hash: "h"}}
			if err := s.meta.save("demo", p); err != nil {
				t.Fatal(err)
			}
			if err := s.store.PruneIndexes("demo"); err != nil { // force a rebuild
				t.Fatal(err)
			}
			revoked := p
			revoked.Tokens = nil
			switch mode {
			case "stale load":
				s.meta.forget("demo")
				started, release := slow.pauseNextGetMeta()
				done := make(chan struct{})
				go func() { defer close(done); s.meta.load("demo") }()
				<-started
				if err := s.meta.save("demo", revoked); err != nil {
					t.Fatal(err)
				}
				release()
				<-done
			case "changed behind cache":
				b, _ := json.Marshal(revoked)
				if err := s.store.PutMeta("demo", b); err != nil {
					t.Fatal(err)
				}
			}
			m, _ := manifestOf(t, files)
			if code, _ := c.plan(m); code != http.StatusOK {
				t.Fatalf("plan: %d", code)
			}
			b, _, err := s.store.GetMeta("demo")
			if err != nil {
				t.Fatal(err)
			}
			var got policy
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Tokens) != 0 {
				t.Fatalf("revoked token resurrected by the index rebuild: %+v", got.Tokens)
			}
			if got.Version == "" {
				t.Fatal("rebuilt index not recorded")
			}
		})
	}
}
