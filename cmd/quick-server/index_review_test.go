package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/storage"
)

// eofSeekStore behaves like minio-go on a truncated object: seeking past the
// end fails with io.EOF instead of succeeding.
type eofSeekStore struct{ storage.Backend }

type eofSeekFile struct {
	io.ReadSeekCloser
	size int64
}

func (f eofSeekFile) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekStart && off >= f.size {
		return 0, io.EOF
	}
	return f.ReadSeekCloser.Seek(off, whence)
}

func (e eofSeekStore) OpenFile(site, p string) (io.ReadSeekCloser, storage.FileInfo, error) {
	rc, fi, err := e.Backend.OpenFile(site, p)
	if err != nil {
		return rc, fi, err
	}
	size, err := rc.Seek(0, io.SeekEnd)
	if err != nil {
		rc.Close()
		return nil, fi, err
	}
	rc.Seek(0, io.SeekStart)
	return eofSeekFile{rc, size}, fi, nil
}

// newEOFSeekServer is a delta test server on eofSeekStore; it returns the
// directory holding the live sites.
func newEOFSeekServer(t *testing.T) (*server, string) {
	t.Helper()
	s := newDeltaTestServer(t)
	dir := t.TempDir()
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: dir, MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.store = eofSeekStore{st}
	s.meta = newMetaStore(st, []byte("secret"), 0)
	s.rules = newRulesStore(st, 5*time.Second)
	return s, dir
}

// expectStaleThenRetry runs one round expecting 409 (the index promised
// chunks that aren't there), then a full deploy that must succeed and publish
// files.
func expectStaleThenRetry(t *testing.T, s *server, c *deltaClient, files map[string][]byte) {
	t.Helper()
	m, data := manifestOf(t, files)
	_, pl := c.plan(m)
	c.upload(data, pl.Missing)
	if w := c.commit(m); w.Code != http.StatusConflict {
		t.Fatalf("commit over a stale index: %d %s, want 409", w.Code, w.Body.String())
	}
	c.deploy(files)
	for n, b := range files {
		if !bytes.Equal(readSite(t, s, "demo", n), b) {
			t.Fatalf("%s differs after the retry", n)
		}
	}
}

// Finding 1: an indexed file truncated so a chunk starts past its end. On s3
// Seek itself fails with io.EOF: still a stale index (409, dropped), not a 500.
func TestIndexedFileTruncatedSeekEOFIsStale(t *testing.T) {
	s, dir := newEOFSeekServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(60, 1<<20)
	files := map[string][]byte{"big.bin": big}
	c.deploy(files)
	// Keep the first chunk whole: the second one starts exactly at the end.
	m, _ := manifestOf(t, files)
	if err := os.Truncate(filepath.Join(dir, "demo", "big.bin"), int64(m.Files[0].Chunks[0].Size)); err != nil {
		t.Fatal(err)
	}
	files["index.html"] = []byte("v2")
	expectStaleThenRetry(t, s, c, files)
}

// Finding 1, short read variant: the chunk starts inside the truncated file.
func TestIndexedFileTruncatedShortReadIsStale(t *testing.T) {
	s, dir := newEOFSeekServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(61, 1<<20)
	files := map[string][]byte{"big.bin": big}
	c.deploy(files)
	if err := os.Truncate(filepath.Join(dir, "demo", "big.bin"), int64(len(big))-10); err != nil {
		t.Fatal(err)
	}
	files["index.html"] = []byte("v2")
	expectStaleThenRetry(t, s, c, files)
}

// Finding 2: a structurally valid index recording a real chunk hash with the
// wrong size. The bytes at that location don't hash right: stale (409 and
// rebuild), not a 400 that repeats on every retry.
func TestIndexWrongChunkSizeIsStale(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(62, 1<<20)
	files := map[string][]byte{"big.bin": big}
	c.deploy(files)
	p, idx := storedIndexOf(t, s, "demo")
	f := &idx[0]
	f.Chunks[0].Size--
	for i := 1; i < len(f.Chunks); i++ {
		f.Chunks[i].Off--
	}
	f.Size--
	b, err := encodeIndex(p.Version, idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.PutIndex("demo", p.Version, b); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeIndex(b, p.Version); err != nil {
		t.Fatalf("tampered index must still decode: %v", err)
	}
	files["index.html"] = []byte("v2")
	expectStaleThenRetry(t, s, c, files)
}

// A manifest lying about a live chunk's size (the index is right) stays a 400.
func TestManifestWrongSizeForLiveChunkIs400(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	small := randData(63, 40<<10) // one chunk
	c.deploy(map[string][]byte{"a.bin": small})
	m, _ := manifestOf(t, map[string][]byte{"a.bin": small})
	if len(m.Files[0].Chunks) != 1 {
		t.Fatalf("want a single chunk, got %d", len(m.Files[0].Chunks))
	}
	m.Files[0].Chunks[0].Size--
	m.Files[0].Size--
	if code, pl := c.plan(m); code != http.StatusOK || len(pl.Missing) != 0 {
		t.Fatalf("plan: %d %v", code, pl.Missing)
	}
	if w := c.commit(m); w.Code != http.StatusBadRequest {
		t.Fatalf("commit with a lying size: %d, want 400", w.Code)
	}
}
