package storage

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/s3fake"
)

// newFakeS3 runs the real s3 backend (minio client) against an in-memory S3
// server.
func newFakeS3(t *testing.T) (*s3, *s3fake.Server) {
	t.Helper()
	srv := s3fake.New("bucket")
	t.Cleanup(srv.Close)
	return s3On(t, srv), srv
}

// s3On is another backend instance (another server process) on the same bucket.
func s3On(t *testing.T, srv *s3fake.Server) *s3 {
	t.Helper()
	b, err := newS3(S3Config{Endpoint: srv.Endpoint(), Region: "us-east-1", Bucket: srv.Bucket, AccessKey: "k", SecretKey: "s", Prefix: "q"})
	if err != nil {
		t.Fatal(err)
	}
	b.grace = 0 // collect at once unless a test opts into the grace period
	return b
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }

func mustPut(t *testing.T, b Backend, site string, files map[string]string) {
	t.Helper()
	if err := b.PutSite(site, tarOf(files)); err != nil {
		t.Fatalf("PutSite: %v", err)
	}
}

func read(t *testing.T, b Backend, site, p string) string {
	t.Helper()
	rc, _, err := b.OpenFile(site, p)
	if err != nil {
		t.Fatalf("open %s/%s: %v", site, p, err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	return string(data)
}

func wantSite(t *testing.T, b Backend, site string, files map[string]string) {
	t.Helper()
	for p, want := range files {
		if got := read(t, b, site, p); got != want {
			t.Fatalf("%s/%s = %q, want %q", site, p, got, want)
		}
	}
	got, err := b.ListFiles(site)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(sortedKeys(files), ",") {
		t.Fatalf("ListFiles = %v, want %v", got, sortedKeys(files))
	}
}

func notFound(t *testing.T, b Backend, site, p string) {
	t.Helper()
	if _, _, err := b.OpenFile(site, p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open %s/%s: %v, want ErrNotFound", site, p, err)
	}
}

// versionIDs lists the version prefixes stored for site.
func versionIDs(srv *s3fake.Server, site string) []string {
	set := map[string]bool{}
	base := "q/versions/" + site + "/"
	for _, k := range srv.Keys(base) {
		id, _, _ := strings.Cut(strings.TrimPrefix(k, base), "/")
		set[id] = true
	}
	var out []string
	for id := range set {
		out = append(out, id)
	}
	return out
}

func TestS3AtomicPutSite(t *testing.T) {
	b, _ := newFakeS3(t)
	if !b.AtomicPutSite() {
		t.Fatal("s3 publishes into a new version and switches a pointer: AtomicPutSite must be true")
	}
}

func TestS3PublishServesNewVersion(t *testing.T) {
	b, srv := newFakeS3(t)
	v1 := map[string]string{"index.html": "one", "a/b.css": "css1", "gone.txt": "bye"}
	mustPut(t, b, "demo", v1)
	wantSite(t, b, "demo", v1)
	v2 := map[string]string{"index.html": "two", "a/b.css": "css2"}
	mustPut(t, b, "demo", v2)
	wantSite(t, b, "demo", v2)
	notFound(t, b, "demo", "gone.txt")
	if ok, err := b.SiteExists("demo"); !ok || err != nil {
		t.Fatalf("SiteExists = %v, %v", ok, err)
	}
	if sites, _ := b.ListSites(); strings.Join(sites, ",") != "demo" {
		t.Fatalf("ListSites = %v", sites)
	}
	// Nothing is written in the legacy flat layout.
	if ks := srv.Keys("q/sites/"); len(ks) != 0 {
		t.Fatalf("flat objects written: %v", ks)
	}
	// A fresh instance (another process, empty cache) sees the same.
	wantSite(t, s3On(t, srv), "demo", v2)
}

// A publish failing on the Nth object write changes nothing: the bucket is
// byte-for-byte as before (the partial version is removed) and the error is
// returned.
func TestS3FailedPublishLeavesLiveUntouched(t *testing.T) {
	b, srv := newFakeS3(t)
	v1 := map[string]string{"index.html": "one", "x.js": "x1"}
	mustPut(t, b, "demo", v1)
	mustPut(t, b, "demo", map[string]string{"index.html": "two", "x.js": "x2"}) // a prev version too
	live := map[string]string{"index.html": "two", "x.js": "x2"}
	before := srv.Snapshot()

	var n atomic.Int32
	srv.Hook = func(method, key string) s3fake.Fault {
		if method == "PUT" && strings.HasPrefix(key, "q/versions/") && n.Add(1) == 3 {
			return s3fake.Fail
		}
		return s3fake.Pass
	}
	files := map[string]string{}
	for i := range 6 {
		files[fmt.Sprintf("f%d.txt", i)] = "three"
	}
	if err := b.PutSite("demo", tarOf(files)); err == nil {
		t.Fatal("PutSite succeeded despite a failed object write")
	}
	srv.Hook = nil
	if after := srv.Snapshot(); !maps.Equal(before, after) {
		t.Fatalf("bucket changed by a failed publish:\nbefore %v\nafter  %v", before, after)
	}
	wantSite(t, b, "demo", live)
	wantSite(t, s3On(t, srv), "demo", live)

	// A tar refused midway (over the file cap) behaves the same.
	defer func(old int) { MaxExtractFiles = old }(MaxExtractFiles)
	MaxExtractFiles = 3
	if err := b.PutSite("demo", tarOf(files)); err == nil {
		t.Fatal("over-cap tar accepted")
	}
	if after := srv.Snapshot(); !maps.Equal(before, after) {
		t.Fatalf("bucket changed by a refused tar")
	}
}

// Readers during publishes never see a mix: the site flips from old to new at
// one instant, so reading the files in order, once a file of the new version
// has been seen no later read returns the old one.
func TestS3ConcurrentReadsNeverSeeAMix(t *testing.T) {
	b, _ := newFakeS3(t)
	const nfiles = 12
	version := func(tag string) map[string]string {
		m := map[string]string{}
		for i := range nfiles {
			m[fmt.Sprintf("f%02d.txt", i)] = tag
		}
		return m
	}
	mustPut(t, b, "demo", version("v0"))
	for round := 1; round <= 4; round++ {
		oldTag, newTag := fmt.Sprintf("v%d", round-1), fmt.Sprintf("v%d", round)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var bad atomic.Value
		var sawNew atomic.Bool
		wg.Go(func() {
			seenNew := false
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := range nfiles {
					rc, _, err := b.OpenFile("demo", fmt.Sprintf("f%02d.txt", i))
					if err != nil {
						bad.Store(err.Error())
						return
					}
					data, _ := io.ReadAll(rc)
					rc.Close()
					switch string(data) {
					case newTag:
						seenNew = true
						sawNew.Store(true)
					case oldTag:
						if seenNew {
							bad.Store(fmt.Sprintf("f%02d old after new", i))
							return
						}
					default:
						bad.Store(fmt.Sprintf("f%02d = %q", i, data))
						return
					}
				}
			}
		})
		mustPut(t, b, "demo", version(newTag))
		for !sawNew.Load() && bad.Load() == nil {
			time.Sleep(time.Millisecond)
		}
		close(stop)
		wg.Wait()
		if v := bad.Load(); v != nil {
			t.Fatalf("round %d: %v", round, v)
		}
	}
}

func TestS3RollbackTogglesAndGCKeepsTwoVersions(t *testing.T) {
	b, srv := newFakeS3(t)
	if ok, err := b.Rollback("demo"); ok || err != nil {
		t.Fatalf("rollback of a missing site: %v %v", ok, err)
	}
	v1 := map[string]string{"index.html": "one", "only1.txt": "1"}
	v2 := map[string]string{"index.html": "two"}
	v3 := map[string]string{"index.html": "three", "only3.txt": "3"}
	mustPut(t, b, "demo", v1)
	if ok, err := b.Rollback("demo"); ok || err != nil {
		t.Fatalf("rollback with no previous version: %v %v", ok, err)
	}
	mustPut(t, b, "demo", v2)
	mustPut(t, b, "demo", v3)
	if ids := versionIDs(srv, "demo"); len(ids) != 2 {
		t.Fatalf("versions kept: %v, want live and previous only", ids)
	}
	if ok, err := b.Rollback("demo"); !ok || err != nil {
		t.Fatalf("rollback: %v %v", ok, err)
	}
	wantSite(t, b, "demo", v2)
	notFound(t, b, "demo", "only3.txt")
	if ok, err := b.Rollback("demo"); !ok || err != nil {
		t.Fatalf("second rollback: %v %v", ok, err)
	}
	wantSite(t, b, "demo", v3)
	wantSite(t, s3On(t, srv), "demo", v3)
	if ids := versionIDs(srv, "demo"); len(ids) != 2 {
		t.Fatalf("rollback changed the stored versions: %v", ids)
	}
}

// Leftovers of a publish that crashed (a version no pointer names) are
// collected by the next successful publish.
func TestS3GCRemovesOrphanVersions(t *testing.T) {
	b, srv := newFakeS3(t)
	mustPut(t, b, "demo", map[string]string{"index.html": "one"})
	srv.Put("q/versions/demo/deadbeef/index.html", []byte("crashed"))
	mustPut(t, b, "demo", map[string]string{"index.html": "two"})
	for _, id := range versionIDs(srv, "demo") {
		if id == "deadbeef" {
			t.Fatal("orphan version not collected")
		}
	}
	if ids := versionIDs(srv, "demo"); len(ids) != 2 {
		t.Fatalf("versions: %v", ids)
	}
}

// GC failures are logged, never fail the publish nor touch the live site.
func TestS3GCFailureDoesNotFailPublish(t *testing.T) {
	b, srv := newFakeS3(t)
	mustPut(t, b, "demo", map[string]string{"index.html": "one"})
	mustPut(t, b, "demo", map[string]string{"index.html": "two"})
	srv.Hook = func(method, key string) s3fake.Fault {
		if method == "DELETE" {
			return s3fake.Fail
		}
		return s3fake.Pass
	}
	mustPut(t, b, "demo", map[string]string{"index.html": "three"})
	srv.Hook = nil
	wantSite(t, b, "demo", map[string]string{"index.html": "three"})
}

// Sites written before versioning (flat objects, no pointer) keep serving; the
// first publish moves them to the versioned layout and removes the flat
// objects.
func TestS3LegacyFlatLayoutServedThenMigrated(t *testing.T) {
	b, srv := newFakeS3(t)
	srv.Put("q/sites/old/index.html", []byte("legacy"))
	srv.Put("q/sites/old/img/a.png", []byte("png"))
	legacy := map[string]string{"index.html": "legacy", "img/a.png": "png"}
	wantSite(t, b, "old", legacy)
	if ok, err := b.SiteExists("old"); !ok || err != nil {
		t.Fatalf("SiteExists legacy = %v %v", ok, err)
	}
	if sites, _ := b.ListSites(); strings.Join(sites, ",") != "old" {
		t.Fatalf("ListSites = %v", sites)
	}
	if ok, err := b.Rollback("old"); ok || err != nil {
		t.Fatalf("rollback of a legacy site: %v %v", ok, err)
	}
	v2 := map[string]string{"index.html": "new"}
	mustPut(t, b, "old", v2)
	wantSite(t, b, "old", v2)
	notFound(t, b, "old", "img/a.png")
	if ks := srv.Keys("q/sites/"); len(ks) != 0 {
		t.Fatalf("flat objects left after migration: %v", ks)
	}
	if sites, _ := b.ListSites(); strings.Join(sites, ",") != "old" {
		t.Fatalf("ListSites after migration = %v", sites)
	}

	// A failed first publish of a legacy site leaves it as it was.
	srv.Put("q/sites/old2/index.html", []byte("legacy2"))
	srv.Hook = func(method, key string) s3fake.Fault {
		if method == "PUT" && strings.HasPrefix(key, "q/live/") {
			return s3fake.Fail
		}
		return s3fake.Pass
	}
	before := srv.Snapshot()
	if err := b.PutSite("old2", tarOf(v2)); err == nil {
		t.Fatal("publish with a failing pointer write succeeded")
	}
	srv.Hook = nil
	if !maps.Equal(before, srv.Snapshot()) {
		t.Fatal("failed migration changed the bucket")
	}
	wantSite(t, b, "old2", map[string]string{"index.html": "legacy2"})
}

func TestS3DeleteSiteRemovesEverything(t *testing.T) {
	b, srv := newFakeS3(t)
	srv.Put("q/sites/demo/flat.txt", []byte("legacy"))
	mustPut(t, b, "demo", map[string]string{"index.html": "one"})
	srv.Put("q/sites/demo/flat.txt", []byte("legacy")) // stray flat object
	mustPut(t, b, "demo", map[string]string{"index.html": "two"})
	if err := b.PutMeta("demo", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.PutIndex("demo", "abc", []byte("idx")); err != nil {
		t.Fatal(err)
	}
	mustPut(t, b, "other", map[string]string{"index.html": "keep"})
	existed, err := b.DeleteSite("demo")
	if !existed || err != nil {
		t.Fatalf("DeleteSite = %v %v", existed, err)
	}
	for _, k := range srv.Keys("q/") {
		if strings.Contains(k, "demo") {
			t.Fatalf("left after delete: %s", k)
		}
	}
	notFound(t, b, "demo", "index.html")
	if ok, _ := b.SiteExists("demo"); ok {
		t.Fatal("deleted site still exists")
	}
	if existed, err := b.DeleteSite("demo"); existed || err != nil {
		t.Fatalf("second DeleteSite = %v %v", existed, err)
	}
	wantSite(t, b, "other", map[string]string{"index.html": "keep"})
}

// The pointer is cached per site, but this process's publish, rollback and
// delete take effect at once; another process sees them within the TTL.
func TestS3PointerCacheInvalidation(t *testing.T) {
	defer func(old time.Duration) { pointerTTL = old }(pointerTTL)
	pointerTTL = time.Hour
	b, srv := newFakeS3(t)
	other := s3On(t, srv)
	mustPut(t, b, "demo", map[string]string{"index.html": "one"})
	if got := read(t, other, "demo", "index.html"); got != "one" { // other caches v1
		t.Fatal(got)
	}
	mustPut(t, b, "demo", map[string]string{"index.html": "two"})
	if got := read(t, b, "demo", "index.html"); got != "two" {
		t.Fatalf("after publish: %q", got)
	}
	if got := read(t, other, "demo", "index.html"); got != "one" {
		t.Fatalf("other process within TTL: %q, want the cached version", got)
	}
	if _, err := b.Rollback("demo"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, b, "demo", "index.html"); got != "one" {
		t.Fatalf("after rollback: %q", got)
	}
	if _, err := b.DeleteSite("demo"); err != nil {
		t.Fatal(err)
	}
	notFound(t, b, "demo", "index.html")

	pointerTTL = 20 * time.Millisecond
	mustPut(t, b, "demo", map[string]string{"index.html": "three"})
	time.Sleep(40 * time.Millisecond)
	if got := read(t, other, "demo", "index.html"); got != "three" {
		t.Fatalf("other process after TTL: %q", got)
	}
}

// A pointer write that S3 reports failed: if the pointer reads back as the new
// one it was applied (success); if it reads back as the old one nothing
// changed (plain error, partial version removed); if it can't be read back the
// outcome is unknown (ErrUncertain, the new version is kept).
func TestS3PointerWriteOutcomes(t *testing.T) {
	isPtr := func(method, key string) bool { return method == "PUT" && strings.HasPrefix(key, "q/live/") }

	t.Run("applied", func(t *testing.T) {
		b, srv := newFakeS3(t)
		mustPut(t, b, "demo", map[string]string{"index.html": "one"})
		srv.Hook = func(m, k string) s3fake.Fault {
			if isPtr(m, k) {
				return s3fake.ApplyAndFail
			}
			return s3fake.Pass
		}
		mustPut(t, b, "demo", map[string]string{"index.html": "two"})
		srv.Hook = nil
		wantSite(t, b, "demo", map[string]string{"index.html": "two"})
	})
	t.Run("refused", func(t *testing.T) {
		b, srv := newFakeS3(t)
		mustPut(t, b, "demo", map[string]string{"index.html": "one"})
		before := srv.Snapshot()
		srv.Hook = func(m, k string) s3fake.Fault {
			if isPtr(m, k) {
				return s3fake.Fail
			}
			return s3fake.Pass
		}
		err := b.PutSite("demo", tarOf(map[string]string{"index.html": "two"}))
		srv.Hook = nil
		if err == nil || errors.Is(err, ErrUncertain) {
			t.Fatalf("err = %v, want a plain failure", err)
		}
		if !maps.Equal(before, srv.Snapshot()) {
			t.Fatal("refused switch changed the bucket")
		}
		wantSite(t, b, "demo", map[string]string{"index.html": "one"})
	})
	t.Run("unknown", func(t *testing.T) {
		b, srv := newFakeS3(t)
		mustPut(t, b, "demo", map[string]string{"index.html": "one"})
		var written atomic.Bool
		srv.Hook = func(m, k string) s3fake.Fault {
			if isPtr(m, k) {
				written.Store(true)
				return s3fake.ApplyAndDrop
			}
			if written.Load() && m == "GET" && strings.HasPrefix(k, "q/live/") {
				return s3fake.Fail
			}
			return s3fake.Pass
		}
		err := b.PutSite("demo", tarOf(map[string]string{"index.html": "two"}))
		srv.Hook = nil
		if !errors.Is(err, ErrUncertain) {
			t.Fatalf("err = %v, want ErrUncertain", err)
		}
		// It was in fact applied: the new version must still be there.
		wantSite(t, b, "demo", map[string]string{"index.html": "two"})
	})
	t.Run("rollback refused", func(t *testing.T) {
		b, srv := newFakeS3(t)
		mustPut(t, b, "demo", map[string]string{"index.html": "one"})
		mustPut(t, b, "demo", map[string]string{"index.html": "two"})
		srv.Hook = func(m, k string) s3fake.Fault {
			if isPtr(m, k) {
				return s3fake.Fail
			}
			return s3fake.Pass
		}
		ok, err := b.Rollback("demo")
		srv.Hook = nil
		if ok || err == nil || errors.Is(err, ErrUncertain) {
			t.Fatalf("rollback = %v %v, want a plain failure", ok, err)
		}
		wantSite(t, b, "demo", map[string]string{"index.html": "two"})
	})
}

// A pointer that can't be read is an error, never a silent fallback to the
// flat layout (which would serve stale or missing content).
func TestS3PointerReadErrorIsNotAFallback(t *testing.T) {
	b, srv := newFakeS3(t)
	mustPut(t, b, "demo", map[string]string{"index.html": "one"})
	b2 := s3On(t, srv)
	srv.Hook = func(m, k string) s3fake.Fault {
		if strings.HasPrefix(k, "q/live/") {
			return s3fake.Fail
		}
		return s3fake.Pass
	}
	if _, _, err := b2.OpenFile("demo", "index.html"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("open with an unreadable pointer: %v, want an operational error", err)
	}
	srv.Hook = nil
	srv.Put("q/live/demo.json", []byte("garbage"))
	b3 := s3On(t, srv)
	if _, _, err := b3.OpenFile("demo", "index.html"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("open with a corrupt pointer: %v, want an error", err)
	}
}
