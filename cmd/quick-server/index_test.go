package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"io"
	"maps"
	"net/http"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/storage"
)

// countingStore counts the live-site reads (OpenFile, ListFiles) and, while a
// PutSite is in flight, makes ListFiles fail like a walk racing a directory
// swap does (ENOENT on a subtree that was just moved away).
type countingStore struct {
	storage.Backend
	mu      sync.Mutex
	opens   int
	lists   int
	putting bool
	hold    chan struct{} // if set, PutSite waits on it after starting
	started chan struct{}
}

func (c *countingStore) OpenFile(site, p string) (io.ReadSeekCloser, storage.FileInfo, error) {
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	return c.Backend.OpenFile(site, p)
}

func (c *countingStore) ListFiles(site string) ([]string, error) {
	c.mu.Lock()
	c.lists++
	putting := c.putting
	c.mu.Unlock()
	if putting {
		return nil, &walkErr{syscall.ENOENT}
	}
	return c.Backend.ListFiles(site)
}

type walkErr struct{ err error }

func (w *walkErr) Error() string { return "lstat sites/demo/sub: " + w.err.Error() }
func (w *walkErr) Unwrap() error { return w.err }

func (c *countingStore) PutSite(site string, tr *tar.Reader) error {
	c.mu.Lock()
	c.putting = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.putting = false
		c.mu.Unlock()
	}()
	if c.started != nil {
		close(c.started)
		c.started = nil
	}
	if c.hold != nil {
		<-c.hold
	}
	return c.Backend.PutSite(site, tr)
}

func (c *countingStore) reads() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens, c.lists
}

func (c *countingStore) reset() {
	c.mu.Lock()
	c.opens, c.lists = 0, 0
	c.mu.Unlock()
}

func newCountingServer(t *testing.T) (*server, *countingStore) {
	t.Helper()
	s := newDeltaTestServer(t)
	cs := &countingStore{Backend: s.store}
	s.store = cs
	return s, cs
}

// tarDeploy publishes files through the legacy tar endpoint.
func tarDeploy(t *testing.T, c *deltaClient, files map[string][]byte) {
	t.Helper()
	if code, body := tarDeployCode(c, files); code != http.StatusOK {
		t.Fatalf("tar deploy: %d %s", code, body)
	}
}

// tarDeployCode is tarDeploy for goroutines: it reports instead of failing.
func tarDeployCode(c *deltaClient, files map[string][]byte) (int, string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		tw.Write(files[n])
	}
	tw.Close()
	gz.Close()
	w := c.do(http.MethodPost, "/api/deploy", buf.Bytes(), nil)
	c.lease = "" // the tar upload ends the deploy it belonged to
	return w.Code, w.Body.String()
}

// storedIndexOf returns the live version's persisted index, failing if the
// metadata names no version or the blob is missing or invalid.
func storedIndexOf(t *testing.T, s *server, site string) (policy, []indexFile) {
	t.Helper()
	p, err := s.meta.load(site)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version == "" {
		t.Fatalf("no live version recorded: %+v", p)
	}
	b, ok, err := s.store.GetIndex(site, p.Version)
	if err != nil || !ok {
		t.Fatalf("index %s: ok=%v err=%v", p.Version, ok, err)
	}
	files, err := decodeIndex(b, p.Version)
	if err != nil {
		t.Fatalf("index %s: %v", p.Version, err)
	}
	return p, files
}

// checkIndexMatches verifies an index describes exactly these files, chunked
// like the CLI does, with contiguous offsets.
func checkIndexMatches(t *testing.T, got []indexFile, files map[string][]byte) {
	t.Helper()
	if len(got) != len(files) {
		t.Fatalf("index has %d files, want %d", len(got), len(files))
	}
	for _, f := range got {
		content, ok := files[f.Path]
		if !ok {
			t.Fatalf("index lists unexpected %q", f.Path)
		}
		want, _ := chunk.Index(bytes.NewReader(content))
		if f.Size != want.Size || f.SHA256 != hex.EncodeToString(want.Hash[:]) || len(f.Chunks) != len(want.Chunks) {
			t.Fatalf("%s: index entry %+v does not match the content", f.Path, f)
		}
		var off int64
		for i, c := range f.Chunks {
			if c.Hash != hex.EncodeToString(want.Chunks[i].Hash[:]) || c.Size != want.Chunks[i].Size || c.Off != off {
				t.Fatalf("%s chunk %d: %+v, want hash %x size %d off %d", f.Path, i, c, want.Chunks[i].Hash, want.Chunks[i].Size, off)
			}
			off += int64(c.Size)
		}
	}
}

func TestIndexWrittenAfterDeltaDeploy(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	files := map[string][]byte{"index.html": []byte("<h1>hi</h1>"), "big.bin": randData(40, 900<<10), "empty.txt": {}}
	c.deploy(files)
	_, idx := storedIndexOf(t, s, "demo")
	checkIndexMatches(t, idx, files)
}

func TestIndexWrittenAfterTarDeploy(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	files := map[string][]byte{"index.html": []byte("tar"), "a/b/big.bin": randData(41, 700<<10), "empty.txt": {}}
	tarDeploy(t, c, files)
	_, idx := storedIndexOf(t, s, "demo")
	checkIndexMatches(t, idx, files)
	// A tar deploy entirely replaces the version: a different index.
	files2 := map[string][]byte{"index.html": []byte("tar2")}
	tarDeploy(t, c, files2)
	_, idx = storedIndexOf(t, s, "demo")
	checkIndexMatches(t, idx, files2)
}

// With a valid index the plan never reads the live site.
func TestPlanDoesNotReadLiveSiteWithValidIndex(t *testing.T) {
	s, cs := newCountingServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(42, 2<<20)
	c.deploy(map[string][]byte{"big.bin": big, "index.html": []byte("v1")})

	cs.reset()
	m, _ := manifestOf(t, map[string][]byte{"big.bin": big, "index.html": []byte("v2")})
	code, pl := c.plan(m)
	if code != http.StatusOK {
		t.Fatalf("plan: %d", code)
	}
	if opens, lists := cs.reads(); opens != 0 || lists != 0 {
		t.Fatalf("plan read the live site: %d OpenFile, %d ListFiles", opens, lists)
	}
	if len(pl.Missing) != 1 || pl.MissingBytes != 2 {
		t.Fatalf("plan missing %v (%d bytes), want only index.html", pl.Missing, pl.MissingBytes)
	}
	// Same after a tar deploy.
	tarDeploy(t, c, map[string][]byte{"big.bin": big})
	cs.reset()
	if code, pl = c.plan(m); code != http.StatusOK || len(pl.Missing) != 1 {
		t.Fatalf("plan after tar deploy: %d %v", code, pl.Missing)
	}
	if opens, lists := cs.reads(); opens != 0 || lists != 0 {
		t.Fatalf("plan after tar deploy read the live site: %d OpenFile, %d ListFiles", opens, lists)
	}
}

// A plan racing a tar deploy (live tree being swapped) used to walk the live
// tree and fail with 500; with the index it never walks it unlocked.
func TestPlanDuringTarDeployIsNotAnError(t *testing.T) {
	s, cs := newCountingServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(43, 1<<20)
	c.deploy(map[string][]byte{"big.bin": big})

	cs.hold, cs.started = make(chan struct{}), make(chan struct{})
	started := cs.started
	deployed := make(chan int, 1)
	go func() {
		code, _ := tarDeployCode(c, map[string][]byte{"big.bin": big, "other.txt": []byte("x")})
		deployed <- code
	}()
	<-started

	m, _ := manifestOf(t, map[string][]byte{"big.bin": big, "index.html": []byte("v2")})
	planned := make(chan int, 1)
	// The tar holds the site's lease: only a forced plan can race it now.
	f := &deltaClient{t: t, s: s, site: "demo", takeover: true}
	go func() { code, _ := f.plan(m); planned <- code }()
	var code int
	select {
	case code = <-planned:
	case <-time.After(200 * time.Millisecond): // waiting for the deploy is fine
	}
	close(cs.hold)
	if code := <-deployed; code != http.StatusOK {
		t.Fatalf("tar deploy: %d", code)
	}
	if code == 0 {
		code = <-planned
	}
	if code != http.StatusOK {
		t.Fatalf("plan during a tar deploy: %d, want 200", code)
	}
	if sent := f.deploy(map[string][]byte{"big.bin": big, "index.html": []byte("v2")}); sent != 2 {
		t.Fatalf("deploy after the race sent %d bytes, want 2", sent)
	}
}

// A real (unsimulated) race: plans and tar deploys hammering the same site
// never produce a server error.
func TestPlanConcurrentWithTarDeploys(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	files := map[string][]byte{}
	for i := range 30 {
		files[string(rune('a'+i%26))+"/"+string(rune('a'+i/26))+"/f.txt"] = randData(uint64(100+i), 2000)
	}
	tarDeploy(t, c, files)
	m, _ := manifestOf(t, files)
	// Each side breaks the other's lease: the only way they can still race.
	ct := &deltaClient{t: t, s: s, site: "demo", takeover: true}
	c.takeover = true
	var wg sync.WaitGroup
	stop := make(chan struct{})
	deployCode := http.StatusOK
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			next := maps.Clone(files)
			next["x.txt"] = []byte{byte(i)}
			if code, _ := tarDeployCode(ct, next); code != http.StatusOK {
				deployCode = code
				return
			}
		}
	}()
	planCode := http.StatusOK
	// A plan whose lease a tar deploy broke meanwhile answers 423 (lease lost):
	// that is the protocol working, not an error.
	for range 100 {
		if code, _ := c.plan(m); code >= 500 {
			planCode = code
			break
		}
	}
	close(stop)
	wg.Wait()
	if planCode != http.StatusOK || deployCode != http.StatusOK {
		t.Fatalf("plan %d, tar deploy %d during concurrent runs", planCode, deployCode)
	}
}

// After a rollback the restored version's own index is live again: a delta
// deploy on top reuses its chunks, without reading the live site to plan.
func TestRollbackSwapsIndexes(t *testing.T) {
	s, cs := newCountingServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	v1 := map[string][]byte{"index.html": []byte("v1"), "one.bin": randData(44, 1<<20)}
	v2 := map[string][]byte{"index.html": []byte("v2"), "two.bin": randData(45, 1<<20)}
	c.deploy(v1)
	p1, _ := storedIndexOf(t, s, "demo")
	tarDeploy(t, c, v2)
	p2, _ := storedIndexOf(t, s, "demo")
	if p2.PrevVersion != p1.Version {
		t.Fatalf("after deploy prev=%q, want %q", p2.PrevVersion, p1.Version)
	}
	if w := c.do(http.MethodPost, "/api/site/demo/rollback", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", w.Code, w.Body.String())
	}
	p, idx := storedIndexOf(t, s, "demo")
	if p.Version != p1.Version || p.PrevVersion != p2.Version {
		t.Fatalf("after rollback live=%q prev=%q, want %q %q", p.Version, p.PrevVersion, p1.Version, p2.Version)
	}
	checkIndexMatches(t, idx, v1)

	cs.reset()
	m, _ := manifestOf(t, map[string][]byte{"index.html": []byte("v1"), "one.bin": v1["one.bin"], "two.bin": v2["two.bin"]})
	code, pl := c.plan(m)
	if code != http.StatusOK {
		t.Fatalf("plan: %d", code)
	}
	if opens, lists := cs.reads(); opens != 0 || lists != 0 {
		t.Fatalf("plan after rollback read the live site: %d OpenFile, %d ListFiles", opens, lists)
	}
	if pl.MissingBytes != int64(len(v2["two.bin"])) {
		t.Fatalf("plan after rollback misses %d bytes, want two.bin only (%d)", pl.MissingBytes, len(v2["two.bin"]))
	}
	v3 := map[string][]byte{"index.html": []byte("v1"), "one.bin": v1["one.bin"], "copy.bin": v1["one.bin"]}
	if sent := c.deploy(v3); sent != 0 {
		t.Fatalf("deploy reusing the rolled-back version sent %d bytes, want 0", sent)
	}
	for n, b := range v3 {
		if !bytes.Equal(readSite(t, s, "demo", n), b) {
			t.Fatalf("%s differs after deploy on top of a rollback", n)
		}
	}
	// A second rollback goes back to v1 (the version before v3's deploy).
	if w := c.do(http.MethodPost, "/api/site/demo/rollback", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("rollback: %d", w.Code)
	}
	_, idx = storedIndexOf(t, s, "demo")
	checkIndexMatches(t, idx, v1)
}

// The metadata names a version whose blob isn't there (or names another
// version): the index is rebuilt from the live files and saved.
func TestStaleIndexRebuilt(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(46, 1<<20)
	files := map[string][]byte{"big.bin": big}
	c.deploy(files)
	p, idx := storedIndexOf(t, s, "demo")

	// Blob of another version stored under the live one's name.
	other, err := encodeIndex("abcdef", idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutIndex("demo", p.Version, other); err != nil {
		t.Fatal(err)
	}
	files["index.html"] = []byte("v2")
	if sent := c.deploy(files); sent != 2 {
		t.Fatalf("deploy over a mismatched index sent %d bytes, want 2", sent)
	}

	// Metadata names a version that has no blob.
	p, _ = storedIndexOf(t, s, "demo")
	p.Version = "0123abcd"
	if err := s.meta.save("demo", p); err != nil {
		t.Fatal(err)
	}
	m, _ := manifestOf(t, files)
	if code, pl := c.plan(m); code != http.StatusOK || len(pl.Missing) != 0 {
		t.Fatalf("plan over a missing index: %d %v", code, pl.Missing)
	}
	p2, idx2 := storedIndexOf(t, s, "demo")
	if p2.Version == "0123abcd" {
		t.Fatal("rebuilt index not recorded")
	}
	checkIndexMatches(t, idx2, files)
}

func TestCorruptIndexRebuilt(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(47, 1<<20)
	c.deploy(map[string][]byte{"big.bin": big})
	p, _ := storedIndexOf(t, s, "demo")
	for _, garbage := range [][]byte{[]byte("not gzip"), gzipOf([]byte(`{"format":1,"version":"` + p.Version + `","files":[{"p":"big.bin","s":5,"h":"zz","c":[]}]}`))} {
		if err := s.store.PutIndex("demo", p.Version, garbage); err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{"big.bin": big, "n.txt": garbage}
		if sent := c.deploy(files); sent != int64(len(garbage)) {
			t.Fatalf("deploy over a corrupt index sent %d bytes, want %d", sent, len(garbage))
		}
		var idx []indexFile
		p, idx = storedIndexOf(t, s, "demo")
		checkIndexMatches(t, idx, files)
	}
}

func gzipOf(b []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(b)
	gz.Close()
	return buf.Bytes()
}

// The live tree changed behind the index's back (e.g. a crash between publish
// and metadata update, or files edited by hand): the plan believes chunks are
// there, the commit finds they aren't, answers 409 and drops the index, and
// the next round rebuilds it and succeeds.
func TestIndexOutOfSyncWithTreeHealsOnCommit(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(48, 1<<20)
	files := map[string][]byte{"big.bin": big, "index.html": []byte("v1")}
	c.deploy(files)
	putSite(t, s.store, "demo", map[string]string{"index.html": "hand edited"})

	m, data := manifestOf(t, files)
	_, pl := c.plan(m)
	c.upload(data, pl.Missing)
	if w := c.commit(m); w.Code != http.StatusConflict {
		t.Fatalf("commit with an out-of-sync index: %d, want 409", w.Code)
	}
	if sent := c.deploy(files); sent != int64(len(big)+2) {
		t.Fatalf("retry sent %d bytes, want %d", sent, len(big)+2)
	}
	_, idx := storedIndexOf(t, s, "demo")
	checkIndexMatches(t, idx, files)
}

// Deleting a site removes its indexes (live and previous).
func TestDeleteRemovesIndexes(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"index.html": []byte("v1")})
	c.deploy(map[string][]byte{"index.html": []byte("v2")})
	p, _ := storedIndexOf(t, s, "demo")
	if w := c.do(http.MethodDelete, "/api/site/demo", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	for _, v := range []string{p.Version, p.PrevVersion} {
		if _, ok, _ := s.store.GetIndex("demo", v); ok {
			t.Fatalf("index %s survived the delete", v)
		}
	}
	// A new site under the same name starts from scratch.
	if sent := c.deploy(map[string][]byte{"index.html": []byte("v2")}); sent != 2 {
		t.Fatalf("deploy after delete sent %d bytes, want 2", sent)
	}
}

// Old versions' blobs don't pile up: only the live and previous ones stay.
func TestOldIndexesPruned(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	var versions []string
	for i := range 4 {
		c.deploy(map[string][]byte{"index.html": {byte('a' + i)}})
		p, _ := storedIndexOf(t, s, "demo")
		versions = append(versions, p.Version)
	}
	for i, v := range versions {
		_, ok, err := s.store.GetIndex("demo", v)
		if err != nil {
			t.Fatal(err)
		}
		if want := i >= 2; ok != want {
			t.Fatalf("index of deploy %d present=%v, want %v", i, ok, want)
		}
	}
}

// A failed publish leaves the recorded version as it was.
func TestFailedDeployKeepsIndex(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"index.html": []byte("v1")})
	before, _ := storedIndexOf(t, s, "demo")
	if w := c.do(http.MethodPost, "/api/deploy", []byte("not gzip"), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad tar deploy: %d", w.Code)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("not a tar"))
	gz.Close()
	if w := c.do(http.MethodPost, "/api/deploy", buf.Bytes(), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad tar deploy: %d", w.Code)
	}
	after, _ := storedIndexOf(t, s, "demo")
	if after.Version != before.Version || after.PrevVersion != before.PrevVersion {
		t.Fatalf("failed deploy changed versions: %q/%q -> %q/%q", before.Version, before.PrevVersion, after.Version, after.PrevVersion)
	}
}
