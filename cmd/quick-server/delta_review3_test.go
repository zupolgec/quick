package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

// Finding 1: the per-site staging allowance must fit every legal manifest.
// Worst case for the 4 KiB rounding is many chunks just over a block
// boundary: here, with the limits scaled down, maxFiles-1 tiny files plus one
// file of chunks of MinSize+1 bytes, all distinct, exactly at the byte cap.
func TestDeltaStagingAllowanceFitsWorstLegalManifest(t *testing.T) {
	const files, bigChunks = 4, 4
	content := int64((files-1)*8 + bigChunks*(chunk.MinSize+1))
	defer func(b int64, f int) { storage.MaxExtractBytes, storage.MaxExtractFiles = b, f }(storage.MaxExtractBytes, storage.MaxExtractFiles)
	storage.MaxExtractBytes, storage.MaxExtractFiles = content, files
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}

	var m quick.DeployManifest
	data := map[string][]byte{}
	for i := range files - 1 {
		b := []byte(fmt.Sprintf("tiny-%03d", i))
		h := sha256.Sum256(b)
		hx := hex.EncodeToString(h[:])
		data[hx] = b
		m.Files = append(m.Files, quick.ManifestFile{Path: fmt.Sprintf("t%d", i), Size: 8, SHA256: hx,
			Chunks: []quick.ManifestChunk{{Hash: hx, Size: 8}}})
	}
	var whole bytes.Buffer
	big := quick.ManifestFile{Path: "big.bin", Size: int64(bigChunks * (chunk.MinSize + 1))}
	for i := range bigChunks {
		b := randData(uint64(40+i), chunk.MinSize+1)
		h := sha256.Sum256(b)
		hx := hex.EncodeToString(h[:])
		data[hx] = b
		whole.Write(b)
		big.Chunks = append(big.Chunks, quick.ManifestChunk{Hash: hx, Size: len(b)})
	}
	wh := sha256.Sum256(whole.Bytes())
	big.SHA256 = hex.EncodeToString(wh[:])
	m.Files = append(m.Files, big)

	code, pl := c.plan(m)
	if code != http.StatusOK {
		t.Fatalf("plan: %d", code)
	}
	if code := c.upload(data, pl.Missing); code != http.StatusOK {
		t.Fatalf("upload of a legal manifest: %d, want 200", code)
	}
	if w := c.commit(m); w.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
}

// Finding 2: a commit in progress must not let a plan for its site skip the
// site-state cap.
func TestDeltaCommitPinDoesNotBypassSiteCap(t *testing.T) {
	s := newDeltaTestServer(t)
	b := &deltaClient{t: t, s: s, site: "b"}
	b.deploy(map[string][]byte{"index.html": []byte("b")})
	a := &deltaClient{t: t, s: s, site: "a"}
	ma, _ := manifestOf(t, tinyFiles("a", 1))
	if code, _ := a.plan(ma); code != http.StatusOK {
		t.Fatalf("plan a: %d", code)
	}
	s.stage.maxSites = 1

	mb, _ := manifestOf(t, map[string][]byte{"index.html": []byte("b")})
	planDuringCommit := -1
	beforePublish = func(string) {
		m, _ := manifestOf(t, tinyFiles("b", 3))
		planDuringCommit, _ = b.plan(m)
	}
	defer func() { beforePublish = nil }()
	if w := b.commit(mb); w.Code != http.StatusOK {
		t.Fatalf("commit b: %d", w.Code)
	}
	if planDuringCommit != http.StatusServiceUnavailable {
		t.Fatalf("plan for a pinned site over the cap: %d, want 503", planDuringCommit)
	}
	if ss := s.stage.sites["b"]; ss != nil && len(ss.wanted) > 0 {
		t.Fatal("refused plan left requests behind")
	}
}

// Finding 3: admission reconciles the plan against what is already staged
// (a concurrent upload may have landed after the plan was computed).
func TestStageWantReconcilesAgainstStaged(t *testing.T) {
	st := newChunkStage(t.TempDir())
	st.maxSiteChunks = 1
	b := []byte("x")
	h := sha256.Sum256(b)
	if _, err := st.want("demo", map[[32]byte]int{h: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.put("demo", h, b, false); err != nil {
		t.Fatal(err)
	}
	got, err := st.want("demo", map[[32]byte]int{h: 1})
	if err != nil {
		t.Fatalf("plan for an already staged chunk refused: %v", err)
	}
	if len(got) != 0 || len(st.sites["demo"].wanted) != 0 || st.wantedTotal != 0 {
		t.Fatalf("already staged chunk still requested: %v", got)
	}
}

// Finding 4: expired plans don't hold site slots until the next sweep.
func TestDeltaExpiredPlansFreeSiteSlots(t *testing.T) {
	s := newDeltaTestServer(t)
	s.stage.maxSites = 1
	a := &deltaClient{t: t, s: s, site: "a"}
	m, _ := manifestOf(t, tinyFiles("a", 2))
	a.plan(m)
	s.stage.mu.Lock()
	for h, w := range s.stage.sites["a"].wanted {
		w.exp = time.Now().Add(-time.Minute)
		s.stage.sites["a"].wanted[h] = w
	}
	s.stage.mu.Unlock()
	b := &deltaClient{t: t, s: s, site: "b"}
	if code, _ := b.plan(m); code != http.StatusOK {
		t.Fatalf("plan for a new site after the other plan expired: %d, want 200", code)
	}
	if _, ok := s.stage.sites["a"]; ok {
		t.Fatal("idle expired state not dropped")
	}
}
