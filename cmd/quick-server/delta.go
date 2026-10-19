// Delta deploy: only the chunks the server doesn't have travel (protocol in
// internal/quick, chunking in internal/chunk).
//
// Where chunks come from. There is no persistent chunk store to keep in sync or
// garbage-collect. The chunks the server "has" for a site are:
//
//   - the live version's, re-derived on demand by chunking the published files
//     (whatever deployed them: delta, tar, or a rollback). Always exact, no
//     index to go stale; the cost is reading the site once per plan and commit,
//     fine for local disk and the 500 MiB cap;
//   - staged ones: uploaded and hash-verified, waiting for a commit, under
//     QUICK_CHUNK_DIR/<site>/<sha256>. Per site, so a client can never learn
//     anything about another site's content.
//
// GC: a successful commit deletes the staged chunks it consumed; anything left
// behind by an abandoned deploy expires after stageTTL (hourly sweep), and
// deleting the site drops its staging dir. Staging per site is capped at the
// same byte budget as a whole site.
//
// Concurrency: plan and chunk upload don't take the site lock (uploads can be
// long; staged files are content-addressed and written via rename, so
// concurrent writers are harmless). Commit holds s.locks like handleDeploy and
// re-checks everything: if a chunk promised by the plan disappeared meanwhile
// (another deploy changed the live version, a sweep), it answers 409 and the
// CLI re-plans.
//
// Commit first copies every reused live chunk into staging and verifies every
// chunk and whole-file hash; only then streams the assembled files as a tar
// into storage PutSite, the same publish path (limits, atomic swap, rollback
// copy) as the tar upload. Copying first matters for backends that overwrite
// in place (s3): a file must never be read after PutSite replaced it.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const (
	maxManifest = 32 << 20 // manifest JSON, decompressed
	stageTTL    = 24 * time.Hour
)

// chunkStage holds uploaded chunks waiting for a commit, one dir per site.
type chunkStage struct {
	dir   string
	locks keyedMutex // per site: serializes uploads for the quota check
}

func newChunkStage(dir string) *chunkStage { return &chunkStage{dir: dir} }

func (c *chunkStage) siteDir(site string) string       { return filepath.Join(c.dir, site) }
func (c *chunkStage) path(site, hexHash string) string { return filepath.Join(c.dir, site, hexHash) }

func (c *chunkStage) has(site, hexHash string) bool {
	_, err := os.Stat(c.path(site, hexHash))
	return err == nil
}

// put writes a chunk atomically (temp file + rename).
func (c *chunkStage) put(site string, h [32]byte, data []byte) error {
	dir := c.siteDir(site)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".up-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), c.path(site, hex.EncodeToString(h[:])))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// read returns a staged chunk, verified against its name.
func (c *chunkStage) read(site string, h [32]byte) ([]byte, error) {
	b, err := os.ReadFile(c.path(site, hex.EncodeToString(h[:])))
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(b) != h {
		os.Remove(c.path(site, hex.EncodeToString(h[:])))
		return nil, fmt.Errorf("staged chunk %x corrupted", h[:6])
	}
	return b, nil
}

// used is the bytes currently staged for site.
func (c *chunkStage) used(site string) int64 {
	ents, _ := os.ReadDir(c.siteDir(site))
	var n int64
	for _, e := range ents {
		if fi, err := e.Info(); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func (c *chunkStage) remove(site string, hashes map[[32]byte]bool) {
	for h := range hashes {
		os.Remove(c.path(site, hex.EncodeToString(h[:])))
	}
	os.Remove(c.siteDir(site)) // only if now empty
}

func (c *chunkStage) removeSite(site string) { os.RemoveAll(c.siteDir(site)) }

// sweep deletes staged chunks (and upload leftovers) older than maxAge.
func (c *chunkStage) sweep(maxAge time.Duration) {
	sites, _ := os.ReadDir(c.dir)
	cutoff := time.Now().Add(-maxAge)
	for _, sd := range sites {
		if !sd.IsDir() {
			continue
		}
		dir := filepath.Join(c.dir, sd.Name())
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
		os.Remove(dir) // only if now empty
	}
}

func (c *chunkStage) sweepLoop() {
	for {
		c.sweep(stageTTL)
		time.Sleep(time.Hour)
	}
}

// deltaFile is a validated manifest entry.
type deltaFile struct {
	rel    string
	mode   int64
	size   int64
	hash   [32]byte
	chunks []chunk.Chunk
}

func parseHash(s string) ([32]byte, error) {
	var h [32]byte
	if len(s) != 64 || strings.ToLower(s) != s {
		return h, fmt.Errorf("invalid hash %q", s)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, fmt.Errorf("invalid hash %q", s)
	}
	return h, nil
}

// parseManifest validates a manifest against the same rules as the tar path
// (CleanRel, file and byte caps) plus the chunker's size bounds: every chunk
// at most MaxSize, every chunk but a file's last at least MinSize (which also
// bounds the manifest's chunk count).
func parseManifest(m quick.DeployManifest) ([]deltaFile, error) {
	if len(m.Files) > storage.MaxExtractFiles {
		return nil, fmt.Errorf("too many files (limit %d)", storage.MaxExtractFiles)
	}
	seen := map[string]bool{}
	var total int64
	out := make([]deltaFile, 0, len(m.Files))
	for _, f := range m.Files {
		rel, err := storage.CleanRel(f.Path)
		if err != nil {
			return nil, err
		}
		if rel == "" {
			return nil, fmt.Errorf("invalid path %q", f.Path)
		}
		if seen[rel] {
			return nil, fmt.Errorf("duplicate path %q", rel)
		}
		seen[rel] = true
		if f.Size < 0 {
			return nil, fmt.Errorf("%s: invalid size", rel)
		}
		if total += f.Size; total > storage.MaxExtractBytes {
			return nil, fmt.Errorf("deploy too large (over %d MiB)", storage.MaxExtractBytes>>20)
		}
		df := deltaFile{rel: rel, mode: int64(f.Mode & 0o777), size: f.Size, chunks: make([]chunk.Chunk, 0, len(f.Chunks))}
		if df.mode == 0 {
			df.mode = 0o644
		}
		if df.hash, err = parseHash(f.SHA256); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		var sum int64
		for i, c := range f.Chunks {
			h, err := parseHash(c.Hash)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			last := i == len(f.Chunks)-1
			if c.Size < 1 || c.Size > chunk.MaxSize || (!last && c.Size < chunk.MinSize) {
				return nil, fmt.Errorf("%s: chunk %d has invalid size %d", rel, i, c.Size)
			}
			sum += int64(c.Size)
			df.chunks = append(df.chunks, chunk.Chunk{Hash: h, Size: c.Size})
		}
		if sum != f.Size {
			return nil, fmt.Errorf("%s: chunks add up to %d bytes, size is %d", rel, sum, f.Size)
		}
		out = append(out, df)
	}
	return out, nil
}

// readManifest decodes the request body (JSON, optionally gzip), capped.
func readManifest(w http.ResponseWriter, r *http.Request) ([]deltaFile, error) {
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxManifest)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		body = io.LimitReader(gz, maxManifest)
	}
	var m quick.DeployManifest
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	return parseManifest(m)
}

// chunkLoc is where a chunk lives in the live version.
type chunkLoc struct {
	path string
	off  int64
	size int
}

// liveIndex chunks the site's published files. Files vanishing mid-walk (an
// unlocked plan racing a deploy) are skipped: they just don't count as present.
func (s *server) liveIndex(site string) (map[[32]byte]chunkLoc, error) {
	files, err := s.store.ListFiles(site)
	if err != nil {
		return nil, err
	}
	idx := map[[32]byte]chunkLoc{}
	for _, p := range files {
		rc, _, err := s.store.OpenFile(site, p)
		if err != nil {
			continue
		}
		var off int64
		err = chunk.Split(rc, func(b []byte) error {
			h := sha256.Sum256(b)
			if _, ok := idx[h]; !ok {
				idx[h] = chunkLoc{path: p, off: off, size: len(b)}
			}
			off += int64(len(b))
			return nil
		})
		rc.Close()
		if err != nil {
			return nil, err
		}
	}
	return idx, nil
}

// deltaRequest runs the checks shared by the delta endpoints before auth:
// method, feature enabled, valid name.
func (s *server) deltaRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.stage == nil {
		http.NotFound(w, r)
		return "", false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return "", false
	}
	return deployName(w, r)
}

func (s *server) handleDeployPlan(w http.ResponseWriter, r *http.Request) {
	name, ok := s.deltaRequest(w, r)
	if !ok {
		return
	}
	if _, _, ok := s.authorizeDeploy(w, r, name); !ok {
		return
	}
	files, err := readManifest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	idx, err := s.liveIndex(name)
	if err != nil {
		http.Error(w, "could not read the live version: "+err.Error(), http.StatusInternalServerError)
		return
	}
	res := quick.DeployPlanResponse{Missing: []string{}}
	seen := map[[32]byte]bool{}
	for _, f := range files {
		res.TotalBytes += f.size
		for _, c := range f.chunks {
			if seen[c.Hash] {
				continue
			}
			seen[c.Hash] = true
			hx := hex.EncodeToString(c.Hash[:])
			if _, live := idx[c.Hash]; live || s.stage.has(name, hx) {
				continue
			}
			res.Missing = append(res.Missing, hx)
			res.MissingBytes += int64(c.Size)
		}
	}
	_ = json.NewEncoder(w).Encode(res)
}

func (s *server) handleDeployChunks(w http.ResponseWriter, r *http.Request) {
	name, ok := s.deltaRequest(w, r)
	if !ok {
		return
	}
	if _, _, ok := s.authorizeDeploy(w, r, name); !ok {
		return
	}
	unlock := s.stage.locks.lock(name)
	defer unlock()
	used := s.stage.used(name)

	fail := func(err error, code int) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			code = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "chunks: "+err.Error(), code)
	}
	gz, err := gzip.NewReader(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		fail(err, http.StatusBadRequest)
		return
	}
	// Decompressed cap too (gzip bomb): the same budget as the compressed body.
	lr := &io.LimitedReader{R: gz, N: maxUpload}
	hdr := make([]byte, quick.ChunkRecordHeader)
	buf := make([]byte, chunk.MaxSize)
	var res quick.ChunkUploadResponse
	for {
		if _, err := io.ReadFull(lr, hdr); err == io.EOF {
			if lr.N <= 0 {
				fail(fmt.Errorf("request over %d MiB once decompressed", maxUpload>>20), http.StatusRequestEntityTooLarge)
				return
			}
			break
		} else if err != nil {
			fail(err, http.StatusBadRequest)
			return
		}
		n := binary.BigEndian.Uint32(hdr[32:])
		if n == 0 || n > chunk.MaxSize {
			fail(fmt.Errorf("invalid chunk size %d", n), http.StatusBadRequest)
			return
		}
		data := buf[:n]
		if _, err := io.ReadFull(lr, data); err != nil {
			fail(err, http.StatusBadRequest)
			return
		}
		h := sha256.Sum256(data)
		if !bytes.Equal(h[:], hdr[:32]) {
			fail(fmt.Errorf("chunk %x: hash mismatch", hdr[:6]), http.StatusBadRequest)
			return
		}
		if s.stage.has(name, hex.EncodeToString(h[:])) {
			continue
		}
		if used+int64(n) > storage.MaxExtractBytes {
			fail(fmt.Errorf("too much pending data for this site (over %d MiB)", storage.MaxExtractBytes>>20), http.StatusRequestEntityTooLarge)
			return
		}
		if err := s.stage.put(name, h, data); err != nil {
			fail(err, http.StatusInternalServerError)
			return
		}
		used += int64(n)
		res.Stored++
		res.Bytes += int64(n)
	}
	_ = json.NewEncoder(w).Encode(res)
}

var (
	errChunkMissing = errors.New("chunk not available")
	errFileHash     = errors.New("file hash mismatch")
)

// stageAll makes every chunk of the manifest available in staging (copying
// reused ones from the live version) and verifies each chunk and each file's
// hash. Returns the chunks it copied, so a failed commit can drop them.
func (s *server) stageAll(site string, files []deltaFile, idx map[[32]byte]chunkLoc) (map[[32]byte]bool, error) {
	copied := map[[32]byte]bool{}
	open := map[string]io.ReadSeekCloser{}
	defer func() {
		for _, rc := range open {
			rc.Close()
		}
	}()
	get := func(c chunk.Chunk) ([]byte, error) {
		if b, err := s.stage.read(site, c.Hash); err == nil {
			return b, nil
		}
		loc, ok := idx[c.Hash]
		if !ok || loc.size != c.Size {
			return nil, errChunkMissing
		}
		rc := open[loc.path]
		if rc == nil {
			var err error
			if rc, _, err = s.store.OpenFile(site, loc.path); err != nil {
				return nil, errChunkMissing
			}
			open[loc.path] = rc
		}
		b := make([]byte, loc.size)
		if _, err := rc.Seek(loc.off, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(rc, b); err != nil || sha256.Sum256(b) != c.Hash {
			return nil, errChunkMissing
		}
		if err := s.stage.put(site, c.Hash, b); err != nil {
			return nil, err
		}
		copied[c.Hash] = true
		return b, nil
	}
	for _, f := range files {
		whole := sha256.New()
		for _, c := range f.chunks {
			b, err := get(c)
			if err != nil {
				return copied, err
			}
			whole.Write(b)
		}
		if !bytes.Equal(whole.Sum(nil), f.hash[:]) {
			return copied, fmt.Errorf("%s: %w", f.rel, errFileHash)
		}
	}
	return copied, nil
}

// writeDeltaTar streams the manifest's files, assembled from staging, as a tar.
func (s *server) writeDeltaTar(w io.Writer, site string, files []deltaFile) error {
	tw := tar.NewWriter(w)
	now := time.Now()
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.rel, Mode: f.mode, Size: f.size, Typeflag: tar.TypeReg, ModTime: now}); err != nil {
			return err
		}
		for _, c := range f.chunks {
			b, err := s.stage.read(site, c.Hash)
			if err != nil {
				return err
			}
			if _, err := tw.Write(b); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

func (s *server) handleDeployCommit(w http.ResponseWriter, r *http.Request) {
	name, ok := s.deltaRequest(w, r)
	if !ok {
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	cur, ident, ok := s.authorizeDeploy(w, r, name)
	if !ok {
		return
	}
	files, err := readManifest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	idx, err := s.liveIndex(name)
	if err != nil {
		http.Error(w, "could not read the live version: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copied, err := s.stageAll(name, files, idx)
	if err != nil {
		s.stage.remove(name, copied)
		switch {
		case errors.Is(err, errChunkMissing):
			http.Error(w, "some chunks are no longer available: plan again", http.StatusConflict)
		case errors.Is(err, errFileHash):
			http.Error(w, "deploy: "+err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, "deploy: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(s.writeDeltaTar(pw, name, files))
	}()
	err = s.store.PutSite(name, tar.NewReader(pr))
	pr.Close() // unblocks the writer if PutSite stopped early
	<-done
	used := map[[32]byte]bool{}
	for _, f := range files {
		for _, c := range f.chunks {
			used[c.Hash] = true
		}
	}
	if err != nil {
		s.stage.remove(name, copied)
		http.Error(w, "deploy: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.stage.remove(name, used)
	log.Printf("delta deploy %q: %d files", name, len(files))
	s.finishDeploy(w, name, cur, ident)
}
