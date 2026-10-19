// Delta deploy: only the chunks the server doesn't have travel (protocol in
// internal/quick, chunking in internal/chunk).
//
// Where chunks come from. There is no persistent chunk store to keep in sync or
// garbage-collect. The chunks the server "has" for a site are:
//
//   - the live version's, from its persisted index (index.go), written by
//     every publish (delta, tar, rollback) and rebuilt from the published
//     files under the site lock if missing, stale or corrupt. Advisory: the
//     commit re-reads and re-checks every reused chunk;
//   - staged ones (stage.go): uploaded and hash-verified, waiting for a
//     commit. Per site, so a client can never learn anything about another
//     site's content; accepted only if a plan asked for them, with per-site
//     and aggregate caps.
//
// GC: a successful commit releases the staged chunks it consumed; anything
// left by an abandoned deploy expires after stageTTL (hourly sweep); deleting
// the site drops its staging.
//
// One deploy at a time per site: plan, uploads and commit belong to a deploy
// lease (lease.go), checked before any work.
//
// Concurrency: plan and chunk upload don't take the site lock (uploads can be
// long; staging has its own lock and accounting), except a plan that has to
// rebuild the live index, so it never walks a tree being swapped. Commit holds
// s.locks like handleDeploy and pins the site's staging so the sweeper can't
// remove verified chunks. If a chunk promised by the plan disappeared
// meanwhile (an expiry, a live tree out of sync with its index; another
// deploy can't change the live version without breaking the lease), it
// answers 409 and the CLI re-plans.
//
// Commit verifies every chunk and every whole-file hash before storage writes
// anything, then streams the assembled files as a tar into PutSite, the same
// publish path (limits, atomic swap, rollback copy) as the tar upload. Reused
// live chunks:
//
//   - atomic backends (local): read straight from the live tree in both
//     passes. PutSite extracts to a temp dir and swaps only at the end, and the
//     site lock keeps other writers out, so the live files are stable for the
//     whole commit. Nothing is copied: staging holds only uploads.
//   - backends that overwrite in place (s3): copied into staging during
//     verification (a file must never be read after PutSite replaced it). The
//     copies count toward the aggregate staging cap and are released at the
//     end of the commit, success or not.
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
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const (
	maxManifest = 32 << 20 // manifest JSON, decompressed
	stageTTL    = 24 * time.Hour
)

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
	sizes := map[[32]byte]int{} // a hash has one size across the manifest
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
			if n, ok := sizes[h]; ok && n != c.Size {
				return nil, fmt.Errorf("%s: chunk %s listed with sizes %d and %d", rel, c.Hash, n, c.Size)
			}
			sizes[h] = c.Size
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
	cur, ident, ok := s.authorizeDeploy(w, r, name, false)
	if !ok {
		return
	}
	lease, ok := s.beginLease(w, r, name, ident, maxManifest, nil)
	if !ok {
		return
	}
	// A failed first plan frees the site; a failed re-plan keeps the lease
	// (the CLI's tar fallback carries it, else it idles out).
	planned, opened := false, lease.id != r.Header.Get(quick.DeployLeaseHeader)
	defer func() {
		if planned || !opened {
			s.leases.end(lease)
		} else {
			s.leases.release(name, lease)
		}
	}()
	files, err := readManifest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	idx, err := s.liveIndexForPlan(name, cur)
	if err != nil {
		http.Error(w, "could not read the live version: "+err.Error(), http.StatusInternalServerError)
		return
	}
	res := quick.DeployPlanResponse{Missing: []string{}}
	want := map[[32]byte]int{}
	var order [][32]byte // manifest order, for the response
	for _, f := range files {
		res.TotalBytes += f.size
		for _, c := range f.chunks {
			if _, dup := want[c.Hash]; dup {
				continue
			}
			if _, live := idx[c.Hash]; live || s.stage.present(name, c.Hash) {
				continue
			}
			want[c.Hash] = c.Size
			order = append(order, c.Hash)
		}
	}
	// The staging lock decides what is really missing (an upload may have
	// landed meanwhile): answer with what was admitted.
	admitted, err := s.stage.want(name, want)
	if err != nil {
		code := http.StatusRequestEntityTooLarge
		if errors.Is(err, errStageBusy) {
			code = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), code)
		return
	}
	for _, h := range order {
		if size, ok := admitted[h]; ok {
			res.Missing = append(res.Missing, hex.EncodeToString(h[:]))
			res.MissingBytes += int64(size)
		}
	}
	res.Lease, planned = lease.id, true
	_ = json.NewEncoder(w).Encode(res)
}

func (s *server) handleDeployChunks(w http.ResponseWriter, r *http.Request) {
	name, ok := s.deltaRequest(w, r)
	if !ok {
		return
	}
	if _, _, ok := s.authorizeDeploy(w, r, name, false); !ok {
		return
	}
	lease, ok := s.joinLease(w, r, name, maxUpload, nil)
	if !ok {
		return
	}
	// A failed upload doesn't end the deploy here: the lease idles out if the
	// CLI gives up.
	defer s.leases.end(lease)
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
		switch err := s.stage.put(name, h, data, false); {
		case err == nil:
		case errors.Is(err, errNotPlanned):
			fail(err, http.StatusConflict)
			return
		case errors.Is(err, errStageFull):
			fail(err, http.StatusRequestEntityTooLarge)
			return
		default:
			fail(err, http.StatusInternalServerError)
			return
		}
		res.Stored++
		res.Bytes += int64(n)
	}
	_ = json.NewEncoder(w).Encode(res)
}

// beforePublish is a test seam: called by commit after verification, right
// before storage starts writing.
var beforePublish func(site string)

var (
	errChunkMissing = errors.New("chunk not available")
	errFileHash     = errors.New("file hash mismatch")
	errChunkSize    = errors.New("chunk or file size does not match its content")
)

// errIndexStale: the live index points at data that isn't there.
var errIndexStale = fmt.Errorf("%w: live files differ from their index", errChunkMissing)

// liveReader reads chunks of the live version keeping at most one file open
// (manifests are in file order, so consecutive chunks mostly share a file).
// Absent or changed data is errIndexStale; real I/O errors pass through.
type liveReader struct {
	s    *server
	site string
	path string
	rc   io.ReadSeekCloser
}

// read returns chunk c from its recorded live location, checked.
// The index can be wrong about a chunk's size too: if it records a size other
// than the manifest's, the recorded range decides. Bytes hashing to c.Hash
// there prove the manifest lies (errChunkSize, 400); anything else means the
// index is stale.
func (l *liveReader) read(c chunk.Chunk, loc chunkLoc) ([]byte, error) {
	b, err := l.readAt(loc)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(b) != c.Hash {
		return nil, errIndexStale
	}
	if loc.size != c.Size {
		return nil, errChunkSize
	}
	return b, nil
}

// readAt reads the recorded range. Every way of not finding it there (file
// gone, Seek or Read hitting the end: s3 fails Seek past the end with io.EOF)
// is errIndexStale.
func (l *liveReader) readAt(loc chunkLoc) ([]byte, error) {
	if l.rc == nil || l.path != loc.path {
		l.close()
		rc, _, err := l.s.store.OpenFile(l.site, loc.path)
		if errors.Is(err, storage.ErrNotFound) {
			return nil, errIndexStale
		}
		if err != nil {
			return nil, err
		}
		l.rc, l.path = rc, loc.path
	}
	eof := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errIndexStale
		}
		return err
	}
	if _, err := l.rc.Seek(loc.off, io.SeekStart); err != nil {
		return nil, eof(err)
	}
	b := make([]byte, loc.size)
	if _, err := io.ReadFull(l.rc, b); err != nil {
		return nil, eof(err)
	}
	return b, nil
}

func (l *liveReader) close() {
	if l.rc != nil {
		l.rc.Close()
		l.rc = nil
	}
}

// deltaSource tells where each chunk of a commit is read from in the publish
// pass: staging, or the live tree (atomic backends only).
type deltaSource struct {
	live   map[[32]byte]chunkLoc // read from the live version
	copied map[[32]byte]bool     // copied into staging by this commit
}

// verifyAll checks, before anything is published, that every chunk is
// available and intact and that every file hashes to its manifest value. On
// non-atomic backends it copies reused live chunks into staging.
func (s *server) verifyAll(site string, files []deltaFile, idx map[[32]byte]chunkLoc) (deltaSource, error) {
	src := deltaSource{live: map[[32]byte]chunkLoc{}, copied: map[[32]byte]bool{}}
	copyLive := !s.store.AtomicPutSite()
	lr := &liveReader{s: s, site: site}
	defer lr.close()
	get := func(c chunk.Chunk) ([]byte, error) {
		b, err := s.stage.read(site, c.Hash)
		if err == nil && len(b) != c.Size {
			return nil, errChunkSize // the hash only proves the bytes, not the declared size
		}
		if !errors.Is(err, errChunkMissing) {
			return b, err
		}
		loc, ok := idx[c.Hash]
		if !ok {
			return nil, errChunkMissing
		}
		if b, err = lr.read(c, loc); err != nil {
			return nil, err
		}
		if copyLive {
			if err := s.stage.put(site, c.Hash, b, true); err != nil {
				return nil, err
			}
			src.copied[c.Hash] = true
		} else {
			src.live[c.Hash] = loc
		}
		return b, nil
	}
	for _, f := range files {
		whole := sha256.New()
		var n int64
		for _, c := range f.chunks {
			b, err := get(c)
			if err != nil {
				return src, err
			}
			whole.Write(b)
			n += int64(len(b))
		}
		if n != f.size {
			return src, fmt.Errorf("%s: %w", f.rel, errChunkSize)
		}
		if !bytes.Equal(whole.Sum(nil), f.hash[:]) {
			return src, fmt.Errorf("%s: %w", f.rel, errFileHash)
		}
	}
	return src, nil
}

// writeDeltaTar streams the manifest's files as a tar, each chunk read from
// where verifyAll found it (and verified again).
func (s *server) writeDeltaTar(w io.Writer, site string, files []deltaFile, src deltaSource) error {
	lr := &liveReader{s: s, site: site}
	defer lr.close()
	tw := tar.NewWriter(w)
	now := time.Now()
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.rel, Mode: f.mode, Size: f.size, Typeflag: tar.TypeReg, ModTime: now}); err != nil {
			return err
		}
		for _, c := range f.chunks {
			var b []byte
			var err error
			if loc, ok := src.live[c.Hash]; ok {
				b, err = lr.read(c, loc)
			} else {
				b, err = s.stage.read(site, c.Hash)
			}
			if err != nil {
				return err
			}
			if len(b) != c.Size {
				return errChunkSize
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
	unlock := sync.OnceFunc(s.locks.lock(name))
	defer unlock()
	cur, ident, ok := s.authorizeDeploy(w, r, name, true)
	if !ok {
		return
	}
	lease, ok := s.joinLease(w, r, name, maxManifest, unlock)
	if !ok {
		return
	}
	replan := false // 409: the deploy goes on with a new plan; anything else ends it
	defer func() {
		if replan {
			s.leases.end(lease)
		} else {
			s.leases.release(name, lease)
		}
	}()
	files, err := readManifest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	unpin := s.stage.pin(name)
	defer unpin()
	idx, stored, err := s.liveIndexLocked(name, cur)
	if err != nil {
		http.Error(w, "could not read the live version: "+err.Error(), http.StatusInternalServerError)
		return
	}
	src, err := s.verifyAll(name, files, idx)
	defer s.stage.release(name, src.copied) // commit copies never outlive the commit
	if err != nil {
		if stored && errors.Is(err, errIndexStale) {
			s.dropLiveVersion(name, &cur) // the next plan rebuilds it
		}
		switch {
		case errors.Is(err, errChunkMissing):
			replan = true
			http.Error(w, "some chunks are no longer available: plan again", http.StatusConflict)
		case errors.Is(err, errFileHash), errors.Is(err, errChunkSize):
			http.Error(w, "deploy: "+err.Error(), http.StatusBadRequest)
		case errors.Is(err, errStageFull):
			http.Error(w, "deploy: "+err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, "deploy: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if beforePublish != nil {
		beforePublish(name)
	}
	old, err := s.clearVersions(name, &cur)
	if err != nil {
		http.Error(w, "could not update site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(s.writeDeltaTar(pw, name, files, src))
	}()
	err = s.store.PutSite(name, tar.NewReader(pr))
	pr.Close() // unblocks the writer if PutSite stopped early
	<-done
	if err != nil {
		s.publishFailed(name, &cur, old)
		http.Error(w, "deploy: "+err.Error(), http.StatusBadRequest) // as handleDeploy
		return
	}
	cur.Version, cur.PrevVersion = s.saveIndex(name, indexOfManifest(files)), old.live
	used := map[[32]byte]bool{}
	for _, f := range files {
		for _, c := range f.chunks {
			used[c.Hash] = true
		}
	}
	s.stage.release(name, used)
	log.Printf("delta deploy %q: %d files", name, len(files))
	s.finishDeploy(w, name, cur, ident)
}
