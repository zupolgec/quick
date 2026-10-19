package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

func newDeltaTestServer(t *testing.T) *server {
	t.Helper()
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: t.TempDir(), MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		store:      st,
		meta:       newMetaStore(st, []byte("secret"), 0),
		rules:      newRulesStore(st, 5*time.Second),
		stage:      newChunkStage(t.TempDir()),
		domain:     "example.com",
		baseDomain: "quick.example.com",
		ownership:  "free",
		noAuth:     true,
	}
	s.apexMux = s.buildApexMux()
	return s
}

func randData(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed+1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// deltaClient plays the CLI side of the protocol against the test server.
type deltaClient struct {
	t     *testing.T
	s     *server
	site  string
	token string
	lease string // from the last plan, sent back like the CLI does
	force bool   // ?force=1 (break another deploy's lease)
}

// manifestOf chunks the files like the CLI does and returns the manifest plus
// the chunk bytes by hex hash.
func manifestOf(t *testing.T, files map[string][]byte) (quick.DeployManifest, map[string][]byte) {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var m quick.DeployManifest
	data := map[string][]byte{}
	for _, n := range names {
		content := files[n]
		f, err := chunk.Index(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		mf := quick.ManifestFile{Path: n, Mode: 0o644, Size: f.Size, SHA256: hex.EncodeToString(f.Hash[:]), Chunks: []quick.ManifestChunk{}}
		off := 0
		for _, c := range f.Chunks {
			h := hex.EncodeToString(c.Hash[:])
			mf.Chunks = append(mf.Chunks, quick.ManifestChunk{Hash: h, Size: c.Size})
			data[h] = content[off : off+c.Size]
			off += c.Size
		}
		m.Files = append(m.Files, mf)
	}
	return m, data
}

func (c *deltaClient) do(method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	c.t.Helper()
	q := "?name=" + c.site
	if c.force {
		q += "&force=1"
	}
	r := httptest.NewRequest(method, path+q, bytes.NewReader(body))
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.lease != "" {
		r.Header.Set(quick.DeployLeaseHeader, c.lease)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	c.s.apexMux.ServeHTTP(w, r)
	return w
}

func (c *deltaClient) plan(m quick.DeployManifest) (int, quick.DeployPlanResponse) {
	c.t.Helper()
	b, _ := json.Marshal(m)
	w := c.do(http.MethodPost, "/api/deploy/plan", b, nil)
	var res quick.DeployPlanResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			c.t.Fatal(err)
		}
		c.lease = res.Lease
	}
	return w.Code, res
}

// leased opens a deploy (an empty plan) for tests of the requests that follow
// one.
func (c *deltaClient) leased() {
	c.t.Helper()
	c.lease = ""
	if code, _ := c.plan(quick.DeployManifest{}); code != http.StatusOK || c.lease == "" {
		c.t.Fatalf("empty plan: %d, lease %q", code, c.lease)
	}
}

// records encodes chunk records (gzip stream) for the given hashes.
func records(t *testing.T, data map[string][]byte, hashes []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, h := range hashes {
		raw, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		gz.Write(raw)
		_ = binary.Write(gz, binary.BigEndian, uint32(len(data[h])))
		gz.Write(data[h])
	}
	gz.Close()
	return buf.Bytes()
}

func (c *deltaClient) upload(data map[string][]byte, hashes []string) int {
	c.t.Helper()
	if len(hashes) == 0 {
		return http.StatusOK
	}
	return c.do(http.MethodPost, "/api/deploy/chunks", records(c.t, data, hashes), nil).Code
}

func (c *deltaClient) commit(m quick.DeployManifest) *httptest.ResponseRecorder {
	c.t.Helper()
	b, _ := json.Marshal(m)
	w := c.do(http.MethodPost, "/api/deploy/commit", b, nil)
	if w.Code != http.StatusConflict {
		c.lease = "" // the deploy is over, like the CLI's: the next plan starts a new one
	}
	return w
}

// deploy runs plan, upload, commit and returns the bytes of the uploaded chunks.
func (c *deltaClient) deploy(files map[string][]byte) int64 {
	c.t.Helper()
	m, data := manifestOf(c.t, files)
	code, pl := c.plan(m) // with c.lease if a deploy is in progress (re-plan)
	if code != http.StatusOK {
		c.t.Fatalf("plan: %d", code)
	}
	var sent int64
	for _, h := range pl.Missing {
		sent += int64(len(data[h]))
	}
	if code := c.upload(data, pl.Missing); code != http.StatusOK {
		c.t.Fatalf("upload: %d", code)
	}
	if w := c.commit(m); w.Code != http.StatusOK {
		c.t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	return sent
}

func readSite(t *testing.T, s *server, site, p string) []byte {
	t.Helper()
	rc, _, err := s.store.OpenFile(site, p)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b
}

func TestDeltaFirstDeployUploadsAllThenServes(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(1, 2<<20)
	sent := c.deploy(map[string][]byte{"index.html": []byte("<h1>hi</h1>"), "game/big.data": big})
	if want := int64(len(big) + len("<h1>hi</h1>")); sent != want {
		t.Fatalf("first deploy sent %d bytes, want %d", sent, want)
	}
	if !bytes.Equal(readSite(t, s, "demo", "game/big.data"), big) {
		t.Fatal("big.data content differs")
	}
	if string(readSite(t, s, "demo", "index.html")) != "<h1>hi</h1>" {
		t.Fatal("index.html content differs")
	}
	p, _ := s.meta.load("demo")
	if p.CreatedBy == "" || p.UpdatedBy == "" {
		t.Fatalf("metadata not updated: %+v", p)
	}
	// Consumed staged chunks are removed after the commit.
	if n := s.stage.used("demo"); n != 0 {
		t.Fatalf("staging still holds %d bytes after commit", n)
	}
}

// The live version is indexed from the published files, whatever uploaded
// them: a redeploy of identical content after a classic tar deploy sends nothing.
func TestDeltaIdenticalRedeployAfterTarDeploySendsNothing(t *testing.T) {
	s := newDeltaTestServer(t)
	big := randData(2, 3<<20)
	putSite(t, s.store, "demo", map[string]string{"index.html": "x", "big.bin": string(big)})
	c := &deltaClient{t: t, s: s, site: "demo"}
	if sent := c.deploy(map[string][]byte{"index.html": []byte("x"), "big.bin": big}); sent != 0 {
		t.Fatalf("identical redeploy sent %d bytes, want 0", sent)
	}
}

func TestDeltaEditInMiddleSendsFewChunks(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(3, 8<<20)
	c.deploy(map[string][]byte{"big.data": big})

	edited := append([]byte{}, big...)
	copy(edited[4<<20:], randData(4, 1000))
	sent := c.deploy(map[string][]byte{"big.data": edited})
	if sent == 0 || sent > 4*chunk.MaxSize {
		t.Fatalf("edit sent %d bytes, want a few chunks", sent)
	}
	if !bytes.Equal(readSite(t, s, "demo", "big.data"), edited) {
		t.Fatal("edited content differs")
	}
}

func TestDeltaRenameSendsNothing(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(5, 1<<20)
	c.deploy(map[string][]byte{"Build/game.v1.data": big})
	if sent := c.deploy(map[string][]byte{"Build/game.v2.data": big}); sent != 0 {
		t.Fatalf("rename sent %d bytes, want 0", sent)
	}
	if !bytes.Equal(readSite(t, s, "demo", "Build/game.v2.data"), big) {
		t.Fatal("renamed content differs")
	}
	if _, _, err := s.store.OpenFile("demo", "Build/game.v1.data"); err == nil {
		t.Fatal("old name still published (deploy must mirror)")
	}
}

func TestDeltaRejectsCorruptChunk(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"index.html": []byte("v1")})

	m, _ := manifestOf(t, map[string][]byte{"index.html": []byte("v2")})
	_, pl := c.plan(m)
	bad := map[string][]byte{pl.Missing[0]: []byte("not v2")}
	if code := c.upload(bad, pl.Missing); code != http.StatusBadRequest {
		t.Fatalf("corrupt chunk upload: %d, want 400", code)
	}
	if w := c.commit(m); w.Code != http.StatusConflict {
		t.Fatalf("commit without the chunk: %d, want 409", w.Code)
	}
	if string(readSite(t, s, "demo", "index.html")) != "v1" {
		t.Fatal("live site changed after a rejected deploy")
	}
}

func TestDeltaRejectsWrongFileHash(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"index.html": []byte("v1")})

	m, data := manifestOf(t, map[string][]byte{"index.html": []byte("v2"), "a.txt": []byte("aaa")})
	m.Files[1].SHA256 = strings.Repeat("0", 64)
	_, pl := c.plan(m)
	c.upload(data, pl.Missing)
	if w := c.commit(m); w.Code != http.StatusBadRequest {
		t.Fatalf("commit with wrong file hash: %d, want 400", w.Code)
	}
	if string(readSite(t, s, "demo", "index.html")) != "v1" {
		t.Fatal("live site changed after a rejected deploy")
	}
	if _, _, err := s.store.OpenFile("demo", "a.txt"); err == nil {
		t.Fatal("a.txt published by a rejected deploy")
	}
}

func TestDeltaCommitWithoutUploadIsConflict(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, _ := manifestOf(t, map[string][]byte{"index.html": []byte("v1")})
	c.plan(m)
	if w := c.commit(m); w.Code != http.StatusConflict {
		t.Fatalf("commit with missing chunks: %d, want 409", w.Code)
	}
	if ok, _ := s.store.SiteExists("demo"); ok {
		t.Fatal("site created by a failed commit")
	}
}

// Another deploy can replace the live version between plan and commit only by
// breaking the lease (--force): the first commit is refused (lease lost), the
// live site is the other deploy's, and a fresh run succeeds reusing what the
// first one staged.
func TestDeltaCommitAfterConcurrentDeployIsRefused(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(8, 1<<20)
	c.deploy(map[string][]byte{"big.bin": big})

	files := map[string][]byte{"big.bin": big, "index.html": []byte("mine")}
	m, data := manifestOf(t, files)
	_, pl := c.plan(m)
	c.upload(data, pl.Missing) // only index.html: big.bin is live
	o := &deltaClient{t: t, s: s, site: "demo", force: true}
	o.deploy(map[string][]byte{"index.html": []byte("theirs")})

	if w := c.commit(m); w.Code != http.StatusLocked {
		t.Fatalf("commit after a forced concurrent deploy: %d, want 423", w.Code)
	}
	if string(readSite(t, s, "demo", "index.html")) != "theirs" {
		t.Fatal("live site changed by the refused commit")
	}
	if sent := c.deploy(files); sent != int64(len(big)) {
		t.Fatalf("retry sent %d bytes, want %d (big.bin only)", sent, len(big))
	}
}

func TestDeltaRollbackStillWorks(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	c.deploy(map[string][]byte{"index.html": []byte("v1")})
	c.deploy(map[string][]byte{"index.html": []byte("v2")})
	w := c.do(http.MethodPost, "/api/site/demo/rollback", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("rollback: %d %s", w.Code, w.Body.String())
	}
	if got := string(readSite(t, s, "demo", "index.html")); got != "v1" {
		t.Fatalf("after rollback: %q, want v1", got)
	}
	// And a delta deploy on top of the rolled-back version still works.
	if sent := c.deploy(map[string][]byte{"index.html": []byte("v1"), "b.txt": []byte("v1")}); sent != 0 {
		t.Fatalf("redeploy after rollback sent %d bytes, want 0 (v1 is live)", sent)
	}
}

func TestDeltaDeployToken(t *testing.T) {
	s := newDeltaTestServer(t)
	s.noAuth = false
	s.ownership = "owned"
	raw := "qk_ci-token"
	p := policy{CreatedBy: "owner@example.com", Tokens: []siteToken{
		{ID: "t1", Name: "ci", Hash: s.meta.hashToken("demo", raw), Scopes: []string{quick.TokenScopeDeploy}, CreatedBy: "owner@example.com"},
		{ID: "t2", Name: "noscope", Hash: s.meta.hashToken("demo", "qk_noscope"), CreatedBy: "owner@example.com"},
	}}
	if err := s.meta.save("demo", p); err != nil {
		t.Fatal(err)
	}
	m, _ := manifestOf(t, map[string][]byte{"index.html": []byte("hi")})

	for _, tok := range []string{"", "qk_noscope", "qk_wrong"} {
		c := &deltaClient{t: t, s: s, site: "demo", token: tok}
		if code, _ := c.plan(m); code != http.StatusUnauthorized {
			t.Errorf("plan with token %q: %d, want 401", tok, code)
		}
		if w := c.commit(m); w.Code != http.StatusUnauthorized {
			t.Errorf("commit with token %q: %d, want 401", tok, w.Code)
		}
	}
	// A token for another site doesn't work either.
	other := &deltaClient{t: t, s: s, site: "other", token: raw}
	if code, _ := other.plan(m); code != http.StatusUnauthorized {
		t.Errorf("plan on another site: %d, want 401", code)
	}

	c := &deltaClient{t: t, s: s, site: "demo", token: raw}
	c.deploy(map[string][]byte{"index.html": []byte("hi")})
	got, _ := s.meta.load("demo")
	if got.UpdatedBy != "token:ci@demo" || got.Tokens[0].LastUsedAt == "" {
		t.Fatalf("token deploy metadata: %+v", got)
	}
}

func TestDeltaOwnershipEnforced(t *testing.T) {
	s := newDeltaTestServer(t)
	s.ownership = "owned"
	if err := s.meta.save("demo", policy{CreatedBy: "someone-else@example.com"}); err != nil {
		t.Fatal(err)
	}
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, data := manifestOf(t, map[string][]byte{"index.html": []byte("hi")})
	if code, _ := c.plan(m); code != http.StatusForbidden {
		t.Errorf("plan: %d, want 403", code)
	}
	var hashes []string
	for h := range data {
		hashes = append(hashes, h)
	}
	if code := c.upload(data, hashes); code != http.StatusForbidden {
		t.Errorf("upload: %d, want 403", code)
	}
	if w := c.commit(m); w.Code != http.StatusForbidden {
		t.Errorf("commit: %d, want 403", w.Code)
	}
}

func TestLegacyTarDeployStillWorks(t *testing.T) {
	s := newDeltaTestServer(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("ok"))
	tw.Close()
	gz.Close()
	c := &deltaClient{t: t, s: s, site: "demo"}
	if w := c.do(http.MethodPost, "/api/deploy", buf.Bytes(), nil); w.Code != http.StatusOK {
		t.Fatalf("legacy deploy: %d %s", w.Code, w.Body.String())
	}
	if string(readSite(t, s, "demo", "index.html")) != "ok" {
		t.Fatal("legacy deploy content differs")
	}
}

func TestDeltaGzipManifest(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, _ := manifestOf(t, map[string][]byte{"index.html": []byte("hi")})
	b, _ := json.Marshal(m)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(b)
	gz.Close()
	w := c.do(http.MethodPost, "/api/deploy/plan", buf.Bytes(), map[string]string{"Content-Encoding": "gzip"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "missing") {
		t.Fatalf("gzip plan: %d %s", w.Code, w.Body.String())
	}
}

func TestDeltaManifestValidation(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	big := randData(7, 600<<10)
	base := func() quick.DeployManifest {
		m, _ := manifestOf(t, map[string][]byte{"a.bin": big, "index.html": []byte("hi")})
		return m
	}
	cases := map[string]func(m *quick.DeployManifest){
		"empty path":     func(m *quick.DeployManifest) { m.Files[1].Path = "/" },
		"duplicate path": func(m *quick.DeployManifest) { m.Files[1].Path = "./a.bin" },
		"size mismatch":  func(m *quick.DeployManifest) { m.Files[1].Size++ },
		"bad hash":       func(m *quick.DeployManifest) { m.Files[1].Chunks[0].Hash = "zz" },
		"bad file hash":  func(m *quick.DeployManifest) { m.Files[1].SHA256 = "abc" },
		"chunk too big": func(m *quick.DeployManifest) {
			f := &m.Files[0]
			f.Chunks = []quick.ManifestChunk{{Hash: f.Chunks[0].Hash, Size: int(f.Size)}}
		},
		"chunk too small": func(m *quick.DeployManifest) {
			f := &m.Files[0]
			c0 := f.Chunks[0]
			f.Chunks = append([]quick.ManifestChunk{{Hash: c0.Hash, Size: 1}, {Hash: c0.Hash, Size: c0.Size - 1}}, f.Chunks[1:]...)
		},
		"zero chunk": func(m *quick.DeployManifest) {
			m.Files[1].Chunks = append(m.Files[1].Chunks, quick.ManifestChunk{Hash: m.Files[1].Chunks[0].Hash, Size: 0})
		},
	}
	for name, mutate := range cases {
		m := base()
		mutate(&m)
		if code, _ := c.plan(m); code != http.StatusBadRequest {
			t.Errorf("%s: plan %d, want 400", name, code)
		}
		c.leased()
		if w := c.commit(m); w.Code != http.StatusBadRequest {
			t.Errorf("%s: commit %d, want 400", name, w.Code)
		}
	}
}

// Paths go through the same CleanRel as tar entries: "../" can't climb out of
// the site, it is clamped to the site root.
func TestDeltaPathTraversalStaysInsideSite(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	m, data := manifestOf(t, map[string][]byte{"x": []byte("escape")})
	m.Files[0].Path = "../../escape.txt"
	_, pl := c.plan(m)
	c.upload(data, pl.Missing)
	if w := c.commit(m); w.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", w.Code, w.Body.String())
	}
	if string(readSite(t, s, "demo", "escape.txt")) != "escape" {
		t.Fatal("clamped path not published inside the site")
	}
	files, _ := s.store.ListFiles("demo")
	if len(files) != 1 || files[0] != "escape.txt" {
		t.Fatalf("site files = %v", files)
	}
}

func TestDeltaLimits(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}

	defer func(v int) { storage.MaxExtractFiles = v }(storage.MaxExtractFiles)
	storage.MaxExtractFiles = 2
	m, _ := manifestOf(t, map[string][]byte{"a": []byte("1"), "b": []byte("2"), "c": []byte("3")})
	if code, _ := c.plan(m); code != http.StatusBadRequest {
		t.Errorf("too many files: plan %d, want 400", code)
	}
	storage.MaxExtractFiles = 20000

	defer func(v int64) { storage.MaxExtractBytes = v }(storage.MaxExtractBytes)
	storage.MaxExtractBytes = 1000
	m, _ = manifestOf(t, map[string][]byte{"a": make([]byte, 1001)})
	if code, _ := c.plan(m); code != http.StatusBadRequest {
		t.Errorf("too many bytes: plan %d, want 400", code)
	}
}

func TestDeltaUploadRejectsOversizedRecord(t *testing.T) {
	s := newDeltaTestServer(t)
	c := &deltaClient{t: t, s: s, site: "demo"}
	b := make([]byte, chunk.MaxSize+1)
	h := sha256.Sum256(b)
	hx := hex.EncodeToString(h[:])
	c.leased()
	if code := c.upload(map[string][]byte{hx: b}, []string{hx}); code != http.StatusBadRequest {
		t.Fatalf("oversized chunk: %d, want 400", code)
	}
}

func TestStageSweepRemovesOldChunks(t *testing.T) {
	s := newDeltaTestServer(t)
	b := []byte("abandoned")
	h := sha256.Sum256(b)
	if _, err := s.stage.want("demo", map[[32]byte]int{h: len(b)}); err != nil {
		t.Fatal(err)
	}
	if err := s.stage.put("demo", h, b, false); err != nil {
		t.Fatal(err)
	}
	p := s.stage.path("demo", h)
	s.stage.sweep(24 * time.Hour)
	if _, err := os.Stat(p); err != nil {
		t.Fatal("fresh staged chunk swept")
	}
	s.stage.mu.Lock()
	sc := s.stage.sites["demo"].staged[h]
	sc.at = time.Now().Add(-48 * time.Hour)
	s.stage.sites["demo"].staged[h] = sc
	s.stage.mu.Unlock()
	s.stage.sweep(24 * time.Hour)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("old staged chunk not swept")
	}
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Fatal("empty site staging dir not removed")
	}
	if s.stage.total != 0 || s.stage.used("demo") != 0 {
		t.Fatalf("accounting after sweep: total %d", s.stage.total)
	}
}

// Accounting survives a restart: it is rebuilt from the staging dir, and
// upload leftovers are cleaned.
func TestStageAccountingRebuiltAtStartup(t *testing.T) {
	dir := t.TempDir()
	st := newChunkStage(dir)
	b := []byte("pending")
	h := sha256.Sum256(b)
	st.want("demo", map[[32]byte]int{h: len(b)})
	if err := st.put("demo", h, b, false); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "demo", ".up-123"), []byte("partial"), 0o600)
	st2 := newChunkStage(dir)
	if st2.used("demo") != stageBlock || st2.total != stageBlock || !st2.present("demo", h) {
		t.Fatalf("rebuilt accounting: used %d total %d", st2.used("demo"), st2.total)
	}
	if _, err := os.Stat(filepath.Join(dir, "demo", ".up-123")); !os.IsNotExist(err) {
		t.Fatal("upload leftover not removed")
	}
}

func TestConfigAdvertisesDeltaDeploy(t *testing.T) {
	s := newDeltaTestServer(t)
	w := httptest.NewRecorder()
	s.handleConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var cfg quick.ConfigResponse
	json.Unmarshal(w.Body.Bytes(), &cfg)
	if !cfg.DeltaDeploy {
		t.Fatal("delta_deploy not advertised")
	}
}
