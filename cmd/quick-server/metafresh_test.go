package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

// An unlocked loadFresh (plan re-checks, domain lists) that read old metadata
// must not overwrite the cache after a newer save.
func TestMetaCacheNotOverwrittenByStaleFreshLoad(t *testing.T) {
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: t.TempDir(), MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	slow := &slowMeta{Backend: st}
	ms := newMetaStore(slow, []byte("secret"), time.Minute)
	if err := ms.save("demo", policy{Access: "public"}); err != nil {
		t.Fatal(err)
	}
	started, release := slow.pauseNextGetMeta()
	done := make(chan policy)
	go func() { p, _ := ms.loadFresh("demo"); done <- p }()
	<-started
	if err := ms.save("demo", policy{Access: "code", CodeHash: "x"}); err != nil {
		t.Fatal(err)
	}
	release()
	if p := <-done; p.Access != "public" {
		t.Fatalf("the stalled read returns what it read, got %+v", p)
	}
	if p, _ := ms.load("demo"); p.Access != "code" {
		t.Fatalf("cache holds stale policy %+v after a newer save", p)
	}
}

// End to end: a stalled fresh read holding Bob's old permission resumes after
// the owner removed him and published new content; Bob still can't open it.
func TestPeopleRevokedViewerNotReadmittedByStaleRead(t *testing.T) {
	e := newPeopleEnv(t)
	slow := &slowMeta{Backend: e.s.store}
	e.s.meta = newMetaStore(slow, []byte("secret"), time.Minute)
	e.setPolicy(t, "app", peoplePolicy(bob))
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code != http.StatusOK {
		t.Fatalf("viewer before: %d", w.Code)
	}
	started, release := slow.pauseNextGetMeta()
	done := make(chan struct{})
	go func() { defer close(done); e.s.meta.loadFresh("app") }() // e.g. a plan's re-check
	<-started
	if w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{RemoveViewers: []string{bob}}); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d", w.Code)
	}
	putSite(t, e.s.store, "app", map[string]string{"index.html": "<h1>new private</h1>"})
	release()
	<-done
	if w := e.visit("app.quick.test", bob, "/", nil); w.Code != http.StatusForbidden {
		t.Fatalf("revoked viewer after the stale read: %d %q", w.Code, w.Body)
	}
}
