package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

// inPlace is a backend that really overwrites in place, like s3: PutSite
// writes every tar entry straight into the live dir as it reads it, so a
// failure halfway leaves earlier files replaced.
type inPlace struct {
	storage.Backend
	dir string
}

func (inPlace) AtomicPutSite() bool { return false }

func (b inPlace) PutSite(site string, tr *tar.Reader) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		dst := filepath.Join(b.dir, site, filepath.FromSlash(hdr.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return err
		}
	}
}

func newInPlaceServer(t *testing.T) *server {
	t.Helper()
	s := newDeltaTestServer(t)
	dir := t.TempDir()
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: dir, MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.store = inPlace{Backend: st, dir: dir}
	s.meta = newMetaStore(st, []byte("secret"), 0)
	s.rules = newRulesStore(st, 5*time.Second)
	return s
}

// With in-place overwrites, a new file reusing chunks of an old file that is
// rewritten earlier in the same deploy must still get the old bytes: reused
// live chunks are copied before anything is written.
func TestDeltaInPlaceBackendSwapReusesSafely(t *testing.T) {
	s := newInPlaceServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	p, q := randData(30, 600<<10), randData(31, 600<<10)
	c.deploy(map[string][]byte{"1.bin": p, "2.bin": q})
	if sent := c.deploy(map[string][]byte{"1.bin": q, "2.bin": p}); sent != 0 {
		t.Fatalf("swap sent %d bytes, want 0", sent)
	}
	if !bytes.Equal(readSite(t, s, "demo", "1.bin"), q) || !bytes.Equal(readSite(t, s, "demo", "2.bin"), p) {
		t.Fatal("swapped files corrupted by in-place overwrite")
	}
}

// Finding 2: a staged chunk whose real length differs from the manifest size
// (hash and file hash consistent with the real bytes) must be rejected during
// verification, before an in-place backend replaced anything.
func TestDeltaStagedChunkSizeMismatchRejectedBeforeWrite(t *testing.T) {
	s := newInPlaceServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"a.txt": []byte("old")})

	// Stage "new" and "ab" through a legitimate plan, without committing.
	m1, data := manifestOf(t, map[string][]byte{"a.txt": []byte("new"), "b.txt": []byte("ab")})
	_, pl := c.plan(m1)
	if code := c.upload(data, pl.Missing); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	ab := sha256.Sum256([]byte("ab"))
	abHex := hex.EncodeToString(ab[:])
	m2, _ := manifestOf(t, map[string][]byte{"a.txt": []byte("new")})
	m2.Files = append(m2.Files, quick.ManifestFile{
		Path: "b.txt", Size: 1, SHA256: abHex,
		Chunks: []quick.ManifestChunk{{Hash: abHex, Size: 1}},
	})
	if w := c.commit(m2); w.Code != http.StatusBadRequest {
		t.Fatalf("commit with a lying chunk size: %d %s, want 400", w.Code, w.Body.String())
	}
	if got := string(readSite(t, s, "demo", "a.txt")); got != "old" {
		t.Fatalf("live a.txt = %q after a rejected commit, want old", got)
	}
}

// Finding 2: the same hash with two sizes in one manifest is inconsistent.
func TestDeltaManifestRejectsInconsistentChunkSizes(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, _ := manifestOf(t, map[string][]byte{"a": []byte("ab"), "b": []byte("x")})
	ab := m.Files[0].Chunks[0].Hash
	m.Files[1] = quick.ManifestFile{Path: "b", Size: 1, SHA256: ab, Chunks: []quick.ManifestChunk{{Hash: ab, Size: 1}}}
	if code, _ := c.plan(m); code != http.StatusBadRequest {
		t.Fatalf("plan with inconsistent sizes: %d, want 400", code)
	}
}

// Finding 1: plans are bounded server-wide: a plan that would exceed the
// aggregate number of outstanding chunk requests is refused without touching
// state, and existing plans keep working.
func TestDeltaPlanAggregateWantedCap(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxTotalWanted = 3
	a := &deltaClient{t: t, s: s, site: "a"}
	ma, da := manifestOf(t, tinyFiles("a", 2))
	if code, _ := a.plan(ma); code != http.StatusOK {
		t.Fatalf("plan a: %d", code)
	}
	b := &deltaClient{t: t, s: s, site: "b"}
	mb, _ := manifestOf(t, tinyFiles("b", 2))
	if code, _ := b.plan(mb); code != http.StatusServiceUnavailable {
		t.Fatalf("plan b over the aggregate cap: %d, want 503", code)
	}
	if _, ok := s.stage.sites["b"]; ok {
		t.Fatal("refused plan left a site state behind")
	}
	if code := a.upload(da, hashesOf(da)); code != http.StatusOK {
		t.Fatalf("upload for the admitted plan: %d", code)
	}
}

// Finding 1: the number of site states is bounded too.
func TestDeltaPlanSiteStatesCap(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxSites = 1
	a := &deltaClient{t: t, s: s, site: "a"}
	m, _ := manifestOf(t, tinyFiles("a", 1))
	a.plan(m)
	b := &deltaClient{t: t, s: s, site: "b"}
	if code, _ := b.plan(m); code != http.StatusServiceUnavailable {
		t.Fatalf("plan for a new site over the cap: %d, want 503", code)
	}
	if len(s.stage.sites) != 1 {
		t.Fatalf("%d site states, want 1", len(s.stage.sites))
	}
}

// Finding 1: a plan with nothing missing leaves no state; plans expire within
// planTTL (an upload session), well before staged chunks do.
func TestDeltaPlanStateIsShortLived(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"index.html": []byte("x")})
	m, _ := manifestOf(t, map[string][]byte{"index.html": []byte("x")})
	c.plan(m)
	if len(s.stage.sites) != 0 {
		t.Fatalf("plan with nothing missing left %d site states", len(s.stage.sites))
	}
	m, _ = manifestOf(t, map[string][]byte{"index.html": []byte("y")})
	c.plan(m)
	for _, w := range s.stage.sites["demo"].wanted {
		if time.Until(w.exp) > planTTL || planTTL > 2*time.Hour {
			t.Fatalf("plan expires in %v, want within %v (<= 2h)", time.Until(w.exp), planTTL)
		}
	}
}

// Finding 3: chunks requested by two plans count once.
func TestDeltaPlanOverlapCountedOnce(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxSiteChunks = 3
	c := &deltaClient{t: t, s: s, site: "demo"}
	files := tinyFiles("f", 3)
	m1, data := manifestOf(t, map[string][]byte{"x": files["f000"], "y": files["f001"]})
	m2, _ := manifestOf(t, map[string][]byte{"y": files["f001"], "z": files["f002"]})
	c.plan(m1)
	if code, _ := c.plan(m2); code != http.StatusOK {
		t.Fatalf("overlapping plan within the cap: %d", code)
	}
	x := m1.Files[0].Chunks[0].Hash
	if code := c.upload(data, []string{x}); code != http.StatusOK {
		t.Fatalf("chunk of the first plan forgotten: upload %d", code)
	}
}

// Finding 3: a refused plan doesn't drop the outstanding ones.
func TestDeltaRefusedPlanKeepsExistingOnes(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxSiteChunks = 3
	c := &deltaClient{t: t, s: s, site: "demo"}
	m1, data := manifestOf(t, tinyFiles("a", 2))
	c.plan(m1)
	big, _ := manifestOf(t, tinyFiles("b", 4))
	if code, _ := c.plan(big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("plan over the site cap: %d, want 413", code)
	}
	if code := c.upload(data, hashesOf(data)); code != http.StatusOK {
		t.Fatalf("existing plan dropped by a refused one: upload %d", code)
	}
}

// Finding 3: making room evicts only the oldest requests, as many as needed.
func TestDeltaPlanEvictsOldestOnly(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxSiteChunks = 3
	c := &deltaClient{t: t, s: s, site: "demo"}
	files := tinyFiles("f", 4)
	ma, da := manifestOf(t, map[string][]byte{"a": files["f000"]})
	mb, db := manifestOf(t, map[string][]byte{"b": files["f001"]})
	mc, _ := manifestOf(t, map[string][]byte{"c": files["f002"], "d": files["f003"]})
	c.plan(ma)
	c.plan(mb)
	if code, _ := c.plan(mc); code != http.StatusOK {
		t.Fatalf("plan needing one eviction: %d", code)
	}
	if code := c.upload(db, hashesOf(db)); code != http.StatusOK {
		t.Fatalf("second-oldest request evicted: upload %d", code)
	}
	if code := c.upload(da, hashesOf(da)); code != http.StatusConflict {
		t.Fatalf("oldest request should be evicted: upload %d, want 409", code)
	}
}
