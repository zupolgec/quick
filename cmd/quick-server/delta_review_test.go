package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/zupolgec/quick/internal/storage"
)

// Finding 1: chunks are accepted only if an outstanding plan asked for them,
// otherwise anyone with a deploy token could fill the disk with arbitrary
// tiny files.
func TestDeltaUploadRequiresPlan(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	b := []byte("x")
	h := sha256.Sum256(b)
	hx := hex.EncodeToString(h[:])
	c.leased()
	if code := c.upload(map[string][]byte{hx: b}, []string{hx}); code != http.StatusConflict {
		t.Fatalf("unplanned upload: %d, want 409", code)
	}
	if n := s.stage.used("demo"); n != 0 {
		t.Fatalf("unplanned chunk staged (%d bytes)", n)
	}
	// Planned for one site doesn't mean planned for another.
	m, data := manifestOf(t, map[string][]byte{"a": b})
	c.plan(m)
	other := &deltaClient{t: t, s: s, site: "other"}
	other.leased()
	if code := other.upload(data, []string{hx}); code != http.StatusConflict {
		t.Fatalf("upload planned for another site: %d, want 409", code)
	}
	if code := c.upload(data, []string{hx}); code != http.StatusOK {
		t.Fatalf("planned upload: %d, want 200", code)
	}
}

// Finding 3: the sweeper must not remove staged chunks a commit already
// verified and is about to publish.
func TestDeltaSweepDuringCommitDoesNotBreakIt(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	beforePublish = func(string) { s.stage.sweep(0) }
	defer func() { beforePublish = nil }()
	c.deploy(map[string][]byte{"index.html": []byte("v1"), "big.bin": randData(20, 300<<10)})
	if string(readSite(t, s, "demo", "index.html")) != "v1" {
		t.Fatal("content differs")
	}
}

// Finding 4: on the local backend reused live chunks are read straight from
// the live tree (PutSite reads the whole tar before swapping), so a commit
// stages nothing beyond what was uploaded and can't blow the per-site cap.
func TestDeltaCommitDoesNotCopyLiveChunksLocally(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(21, 1<<20)
	c.deploy(map[string][]byte{"big.bin": big})
	var during int64 = -1
	beforePublish = func(site string) { during = s.stage.used(site) }
	defer func() { beforePublish = nil }()
	c.deploy(map[string][]byte{"big.bin": big, "index.html": []byte("new")})
	if during < 0 || during > 4<<10 {
		t.Fatalf("staging held %d bytes during commit, want only the uploaded chunk", during)
	}
}

// Finding 2 (server): a commit reusing thousands of live files must not keep
// one descriptor per file open.
func TestDeltaCommitManyReusedFiles(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	files := map[string][]byte{}
	for i := range 2500 {
		files[fmt.Sprintf("f/%04d.txt", i)] = []byte(fmt.Sprintf("file %d", i))
	}
	c.deploy(files)
	withFDLimit(t, 512, func() {
		renamed := map[string][]byte{}
		for k, v := range files {
			renamed["g/"+k] = v
		}
		m, _ := manifestOf(t, renamed)
		c.leased()
		if w := c.commit(m); w.Code != http.StatusOK {
			t.Fatalf("commit reusing 2500 live files: %d %s", w.Code, w.Body.String())
		}
	})
}

func tinyFiles(prefix string, n int) map[string][]byte {
	files := map[string][]byte{}
	for i := range n {
		files[fmt.Sprintf("%s%03d", prefix, i)] = []byte(fmt.Sprintf("%s %d", prefix, i))
	}
	return files
}

func hashesOf(data map[string][]byte) []string {
	var hs []string
	for h := range data {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	return hs
}

// Finding 1: every staged chunk is charged at least a disk block, so many tiny
// chunks hit the per-site byte cap.
func TestDeltaStagingChargesPerChunkOverhead(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxSiteBytes = 2 * stageBlock
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, data := manifestOf(t, tinyFiles("f", 3))
	c.plan(m)
	if code := c.upload(data, hashesOf(data)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("3 one-block chunks over a 2-block cap: %d, want 413", code)
	}
	if n := s.stage.used("demo"); n != 2*stageBlock {
		t.Fatalf("charged %d, want %d", n, 2*stageBlock)
	}
}

// Finding 1: the number of planned + staged chunks per site is bounded.
func TestDeltaStagingChunkCountCap(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxSiteChunks = 2
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, _ := manifestOf(t, tinyFiles("f", 3))
	if code, _ := c.plan(m); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("plan over the chunk cap: %d, want 413", code)
	}
	m, data := manifestOf(t, tinyFiles("g", 2))
	if code, _ := c.plan(m); code != http.StatusOK {
		t.Fatalf("plan within the cap: %d", code)
	}
	if code := c.upload(data, hashesOf(data)); code != http.StatusOK {
		t.Fatalf("upload within the cap: %d", code)
	}
}

// Finding 1: an aggregate cap across sites bounds the whole staging dir.
func TestDeltaStagingAggregateCap(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxTotalBytes = 2 * stageBlock
	a := &deltaClient{t: t, s: s, site: "a"}
	m, data := manifestOf(t, tinyFiles("a", 2))
	a.plan(m)
	if code := a.upload(data, hashesOf(data)); code != http.StatusOK {
		t.Fatalf("site a: %d", code)
	}
	b := &deltaClient{t: t, s: s, site: "b"}
	m, data = manifestOf(t, tinyFiles("b", 1))
	b.plan(m)
	if code := b.upload(data, hashesOf(data)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("site b over the aggregate cap: %d, want 413", code)
	}
}

// Finding 4: concurrent uploads keep the accounting exact.
func TestDeltaConcurrentUploadsAccounting(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, data := manifestOf(t, tinyFiles("f", 40))
	c.plan(m)
	hs := hashesOf(data)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Overlapping slices: the same chunk uploaded twice is charged once.
			c.upload(data, hs[i*5:min(len(hs), i*5+10)])
		}()
	}
	wg.Wait()
	if n := s.stage.used("demo"); n != 40*stageBlock || s.stage.total != 40*stageBlock {
		t.Fatalf("charged %d (total %d), want %d", n, s.stage.total, 40*stageBlock)
	}
	ents, _ := os.ReadDir(filepath.Join(s.stage.dir, "demo"))
	if len(ents) != 40 {
		t.Fatalf("%d files staged, want 40", len(ents))
	}
}

// nonAtomic makes the local backend look like s3: PutSite overwrites in place.
type nonAtomic struct{ storage.Backend }

func (nonAtomic) AtomicPutSite() bool { return false }

// Finding 4: on backends that overwrite in place, reused live chunks are
// copied into staging under the aggregate cap and released after the commit.
func TestDeltaNonAtomicBackendCopiesWithinCap(t *testing.T) {
	s := newDeltaTestServer(t)
	s.store = nonAtomic{s.store}
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(22, 1<<20)
	c.deploy(map[string][]byte{"big.bin": big, "index.html": []byte("v1")})

	var during int64
	beforePublish = func(site string) { during = s.stage.used(site) }
	defer func() { beforePublish = nil }()
	c.deploy(map[string][]byte{"big.bin": big, "index.html": []byte("v2")})
	if during < int64(len(big)) {
		t.Fatalf("staging held %d bytes during commit, want the copied live chunks", during)
	}
	if n := s.stage.used("demo"); n != 0 || s.stage.total != 0 {
		t.Fatalf("copies not released: site %d total %d", n, s.stage.total)
	}

	s.stage.maxTotalBytes = 64 << 10
	m, data := manifestOf(t, map[string][]byte{"big.bin": big, "index.html": []byte("v3")})
	_, pl := c.plan(m)
	c.upload(data, pl.Missing)
	if w := c.commit(m); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("commit over the aggregate cap: %d, want 503", w.Code)
	}
	if string(readSite(t, s, "demo", "index.html")) != "v2" {
		t.Fatal("live site changed by a refused commit")
	}
	if s.stage.total != stageBlock {
		t.Fatalf("after refused commit staging holds %d, want only the uploaded chunk", s.stage.total)
	}
}
