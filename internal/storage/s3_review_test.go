package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/s3fake"
)

func isPointerPut(method, key string) bool {
	return method == "PUT" && strings.HasPrefix(key, "q/live/")
}

// A pointer write that fails at transport level may still land later: it is
// ErrUncertain whatever the read-back shows now, and the new tree is kept, so
// the late write never points at a deleted tree.
func TestS3PointerTransportFailureIsUncertainAndKeepsTree(t *testing.T) {
	b, srv := newFakeS3(t)
	mustPut(t, b, "demo", map[string]string{"index.html": "one"})
	srv.Hook = func(m, k string) s3fake.Fault {
		if isPointerPut(m, k) {
			return s3fake.DeferAndDrop
		}
		return s3fake.Pass
	}
	err := b.PutSite("demo", tarOf(map[string]string{"index.html": "two"}))
	srv.Hook = nil
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("err = %v, want ErrUncertain", err)
	}
	srv.ApplyDeferred() // the write lands late
	wantSite(t, s3On(t, srv), "demo", map[string]string{"index.html": "two"})
}

// Retry overlap: an attempt dropped (applied later) then a refusal on the
// retry is still uncertain, not a refusal.
func TestS3PointerRefusalAfterDroppedAttemptIsUncertain(t *testing.T) {
	b, srv := newFakeS3(t)
	mustPut(t, b, "demo", map[string]string{"index.html": "one"})
	var n atomic.Int32
	srv.Hook = func(m, k string) s3fake.Fault {
		if isPointerPut(m, k) {
			if n.Add(1) == 1 {
				return s3fake.DeferAndDrop
			}
			return s3fake.Fail
		}
		return s3fake.Pass
	}
	err := b.PutSite("demo", tarOf(map[string]string{"index.html": "two"}))
	srv.Hook = nil
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("err = %v, want ErrUncertain", err)
	}
	srv.ApplyDeferred()
	wantSite(t, s3On(t, srv), "demo", map[string]string{"index.html": "two"})

	// A dropped attempt that never landed, then a success: published.
	n.Store(0)
	srv.Hook = func(m, k string) s3fake.Fault {
		if isPointerPut(m, k) && n.Add(1) == 1 {
			return s3fake.Drop
		}
		return s3fake.Pass
	}
	mustPut(t, b, "demo", map[string]string{"index.html": "three"})
	srv.Hook = nil
	wantSite(t, s3On(t, srv), "demo", map[string]string{"index.html": "three"})
}

// A handle opened on a version stays readable across two publishes (the
// version is pinned while open), even with no grace period.
func TestS3OpenHandleSurvivesTwoPublishes(t *testing.T) {
	b, srv := newFakeS3(t)
	content := strings.Repeat("A", 1000)
	mustPut(t, b, "demo", map[string]string{"index.html": content})
	rc, _, err := b.OpenFile("demo", "index.html")
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, b, "demo", map[string]string{"index.html": "B"})
	mustPut(t, b, "demo", map[string]string{"index.html": "C"})
	if _, err := rc.Seek(1, io.SeekStart); err != nil { // a new ranged GET
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != content[1:] {
		t.Fatalf("read after two publishes: %d bytes, %v", len(got), err)
	}
	rc.Close()
	mustPut(t, b, "demo", map[string]string{"index.html": "D"})
	if ids := versionIDs(srv, "demo"); len(ids) != 2 {
		t.Fatalf("closed version not collected: %v", ids)
	}
}

// Versions retired less than the grace period ago are kept (another process
// may still serve them); collected after it.
func TestS3GCRespectsGracePeriod(t *testing.T) {
	b, srv := newFakeS3(t)
	b.grace = time.Hour
	clock := time.Now()
	b.now = func() time.Time { return clock }
	mustPut(t, b, "demo", map[string]string{"index.html": "A"})
	mustPut(t, b, "demo", map[string]string{"index.html": "B"})
	mustPut(t, b, "demo", map[string]string{"index.html": "C"}) // A retired now
	if ids := versionIDs(srv, "demo"); len(ids) != 3 {
		t.Fatalf("versions within grace: %v, want A kept", ids)
	}
	clock = clock.Add(2 * time.Hour)
	srv.Age("q/", 2*time.Hour)
	mustPut(t, b, "demo", map[string]string{"index.html": "D"}) // A past grace, B retired now
	if ids := versionIDs(srv, "demo"); len(ids) != 3 {
		t.Fatalf("versions after grace: %v, want D, C and the just-retired B", ids)
	}
	clock = clock.Add(2 * time.Hour)
	srv.Age("q/", 2*time.Hour)
	mustPut(t, b, "demo", map[string]string{"index.html": "E"})
	if ids := versionIDs(srv, "demo"); len(ids) != 3 {
		t.Fatalf("versions: %v", ids)
	}
	wantSite(t, b, "demo", map[string]string{"index.html": "E"})
}

// Trees no pointer names (a crashed or uncertain publish) are collected only
// once older than the grace period.
func TestS3CrashLeftoverRespectsGrace(t *testing.T) {
	b, srv := newFakeS3(t)
	b.grace = time.Hour
	mustPut(t, b, "demo", map[string]string{"index.html": "A"})
	srv.Put("q/versions/demo/deadbeef/index.html", []byte("crashed"))
	mustPut(t, b, "demo", map[string]string{"index.html": "B"})
	if len(srv.Keys("q/versions/demo/deadbeef/")) == 0 {
		t.Fatal("recent leftover collected within the grace period")
	}
	srv.Age("q/versions/demo/deadbeef/", 2*time.Hour)
	mustPut(t, b, "demo", map[string]string{"index.html": "C"})
	if ks := srv.Keys("q/versions/demo/deadbeef/"); len(ks) != 0 {
		t.Fatalf("old leftover not collected: %v", ks)
	}
}

// The legacy flat objects of a migrated site are removed only once the grace
// period after the first pointer has passed.
func TestS3FlatLayoutRemovedAfterGrace(t *testing.T) {
	b, srv := newFakeS3(t)
	b.grace = time.Hour
	clock := time.Now()
	b.now = func() time.Time { return clock }
	srv.Put("q/sites/old/index.html", []byte("legacy"))
	mustPut(t, b, "old", map[string]string{"index.html": "new"})
	if len(srv.Keys("q/sites/old/")) == 0 {
		t.Fatal("flat objects removed within the grace period")
	}
	wantSite(t, b, "old", map[string]string{"index.html": "new"})
	clock = clock.Add(2 * time.Hour)
	mustPut(t, b, "old", map[string]string{"index.html": "newer"})
	if ks := srv.Keys("q/sites/old/"); len(ks) != 0 {
		t.Fatalf("flat objects left after the grace period: %v", ks)
	}
}

// A pointer read that started before an update must not be cached after it,
// even if the cache entry was evicted meanwhile.
func TestS3DelayedPointerReadNotCachedAfterEviction(t *testing.T) {
	defer func(ttl time.Duration, max int) { pointerTTL, maxPointerEntries = ttl, max }(pointerTTL, maxPointerEntries)
	b, _ := newFakeS3(t)
	mustPut(t, b, "demo", map[string]string{"index.html": "A"})
	evict := func() {
		pointerTTL, maxPointerEntries = time.Nanosecond, 0
		time.Sleep(time.Millisecond)
		b.mu.Lock()
		b.trimPointers()
		b.mu.Unlock()
	}
	evict()

	started, release := make(chan struct{}), make(chan struct{})
	pointerReadHook = func(site string) {
		close(started)
		<-release
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if rc, _, err := b.OpenFile("demo", "index.html"); err == nil {
			rc.Close()
		}
	}()
	<-started
	pointerReadHook = nil
	mustPut(t, b, "demo", map[string]string{"index.html": "B"})
	mustPut(t, b, "demo", map[string]string{"index.html": "C"})
	evict()
	pointerTTL = time.Hour
	close(release)
	<-done
	if got := read(t, b, "demo", "index.html"); got != "C" {
		t.Fatalf("served %q, want C: a stale pointer was cached", got)
	}
}

// A corrupt pointer: publishing refuses with a clear error and changes
// nothing; deleting the site still works and removes everything.
func TestS3CorruptPointer(t *testing.T) {
	b, srv := newFakeS3(t)
	mustPut(t, b, "demo", map[string]string{"index.html": "one"})
	srv.Put("q/live/demo.json", []byte("garbage"))
	b = s3On(t, srv)
	before := srv.Snapshot()
	err := b.PutSite("demo", tarOf(map[string]string{"index.html": "two"}))
	if err == nil || errors.Is(err, ErrUncertain) || !strings.Contains(err.Error(), "invalid live pointer") {
		t.Fatalf("publish over a corrupt pointer: %v", err)
	}
	if !maps.Equal(before, srv.Snapshot()) {
		t.Fatal("refused publish changed the bucket")
	}
	if err := b.PutMeta("demo", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	existed, err := b.DeleteSite("demo")
	if !existed || err != nil {
		t.Fatalf("DeleteSite = %v %v", existed, err)
	}
	if ks := srv.Keys(""); len(ks) != 0 {
		t.Fatalf("left after delete: %v", ks)
	}
	mustPut(t, b, "demo", map[string]string{"index.html": "fresh"})
	wantSite(t, b, "demo", map[string]string{"index.html": "fresh"})
}

// Files over minio's multipart threshold (16 MiB) publish and read back.
func TestS3LargeFileMultipart(t *testing.T) {
	b, _ := newFakeS3(t)
	big := make([]byte, 17<<20)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range big {
		big[i] = byte(r.Uint32())
	}
	mustPut(t, b, "demo", map[string]string{"big.bin": string(big), "index.html": "x"})
	rc, _, err := b.OpenFile("demo", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("big.bin read back: %d bytes, %v", len(got), err)
	}
}

// Listings are paginated and URL-encoded by S3: odd names and many pages
// still publish, list, collect and delete correctly.
func TestS3PaginatedEncodedListing(t *testing.T) {
	b, srv := newFakeS3(t)
	srv.MaxKeys = 2
	files := map[string]string{"a b+c.txt": "1", "ü/é.txt": "2", "%41.txt": "3", "x?y&z=1.txt": "4"}
	for i := range 5 {
		files[fmt.Sprintf("d/f%d.txt", i)] = "f"
	}
	mustPut(t, b, "demo", files)
	wantSite(t, b, "demo", files)
	mustPut(t, b, "demo", map[string]string{"index.html": "2"})
	mustPut(t, b, "demo", files)
	wantSite(t, b, "demo", files)
	if ids := versionIDs(srv, "demo"); len(ids) != 2 {
		t.Fatalf("versions: %v", ids)
	}
	if _, err := b.DeleteSite("demo"); err != nil {
		t.Fatal(err)
	}
	if ks := srv.Keys(""); len(ks) != 0 {
		t.Fatalf("left after delete: %v", ks)
	}
}
