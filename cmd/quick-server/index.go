// Live index: a persisted map of the published version's chunks, so delta
// plans and commits don't re-read and re-chunk the live site.
//
// Format: gzip JSON (indexFormat) with, per file, path, size, SHA-256 and the
// chunk list (hash, size, offset), stored by the backend as a blob keyed by a
// random version id (Backend.PutIndex: next to the metadata, local file or s3
// object).
//
// Versioning. The site metadata records which version is live (policy.Version)
// and which one is the rollback copy (policy.PrevVersion). The metadata is the
// only pointer: an index is used only if its blob exists, decodes, passes
// validation and embeds the same version. Every publish path runs under the
// site lock and:
//
//  1. clears both versions in the metadata before storage changes anything
//     (so a crash mid-publish can't leave a version naming the wrong tree);
//  2. publishes (PutSite or Rollback), restoring the versions if it failed;
//  3. stores the new version's index and records it (finishDeploy saves the
//     metadata), then prunes blobs other than live and previous.
//
// Sources of the index: a delta commit already has the manifest; a tar deploy
// chunks the files while they stream into PutSite (no re-read, also on s3); a
// rollback just swaps Version and PrevVersion, the blobs stay as they are.
//
// Fallback. A missing, stale or corrupt index is rebuilt by chunking the live
// files, always under the site lock: the plan takes the lock only in that case
// (and saves the result), the commit already holds it (in memory only, it is
// about to publish a new version anyway). The plan therefore never walks the
// live tree while a deploy may be swapping it.
//
// The index is advisory: the commit still reads every reused chunk from the
// live files at the recorded offset and checks its hash and size. If the tree
// no longer matches (files changed outside quick), the commit answers 409 and
// drops the recorded version, so the next plan rebuilds it. Sites published
// before the index existed have no version recorded: rebuilt on first plan.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const (
	indexFormat = 1
)

// maxIndexSize caps a decoded index (a full 500 MiB site is a few MiB of
// JSON); var so tests can lower it.
var maxIndexSize int64 = 64 << 20

type indexChunk struct {
	Hash string `json:"h"`
	Size int    `json:"s"`
	Off  int64  `json:"o"`
}

type indexFile struct {
	Path   string       `json:"p"`
	Size   int64        `json:"s"`
	SHA256 string       `json:"h"`
	Chunks []indexChunk `json:"c"`
}

type indexBlob struct {
	Format  int         `json:"format"`
	Version string      `json:"version"`
	Files   []indexFile `json:"files"`
}

func newIndexVersion() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func encodeIndex(version string, files []indexFile) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(indexBlob{Format: indexFormat, Version: version, Files: files}); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeIndex parses and validates a blob: right format and version, clean
// unique paths, valid hashes, chunk sizes within the chunker's bounds,
// contiguous offsets adding up to the file size. The gzip stream is read to
// its end (checksum verified) within maxIndexSize, and must hold exactly one
// JSON document.
func decodeIndex(b []byte, version string) ([]indexFile, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(gz, maxIndexSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxIndexSize {
		return nil, fmt.Errorf("index over %d bytes", maxIndexSize)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var ix indexBlob
	if err := dec.Decode(&ix); err != nil {
		return nil, err
	}
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		return nil, errors.New("index: trailing data")
	}
	if ix.Format != indexFormat || ix.Version != version {
		return nil, fmt.Errorf("index format %d version %q, want %d %q", ix.Format, ix.Version, indexFormat, version)
	}
	seen := map[string]bool{}
	for _, f := range ix.Files {
		if rel, err := storage.CleanRel(f.Path); err != nil || rel == "" || rel != f.Path || seen[rel] {
			return nil, fmt.Errorf("index: bad path %q", f.Path)
		}
		seen[f.Path] = true
		if _, err := parseHash(f.SHA256); err != nil {
			return nil, err
		}
		var off int64
		for _, c := range f.Chunks {
			if _, err := parseHash(c.Hash); err != nil {
				return nil, err
			}
			if c.Size < 1 || c.Size > chunk.MaxSize || c.Off != off {
				return nil, fmt.Errorf("index: %s: bad chunk at %d", f.Path, c.Off)
			}
			off += int64(c.Size)
		}
		if off != f.Size {
			return nil, fmt.Errorf("index: %s: chunks add up to %d, size %d", f.Path, off, f.Size)
		}
	}
	return ix.Files, nil
}

// locsOf maps each chunk hash to its first location.
func locsOf(files []indexFile) map[[32]byte]chunkLoc {
	idx := map[[32]byte]chunkLoc{}
	for _, f := range files {
		for _, c := range f.Chunks {
			h, err := parseHash(c.Hash)
			if err != nil {
				continue // validated by decodeIndex; built locally otherwise
			}
			if _, ok := idx[h]; !ok {
				idx[h] = chunkLoc{path: f.Path, off: c.Off, size: c.Size}
			}
		}
	}
	return idx
}

// indexOf chunks one file's content.
func indexOf(path string, r io.Reader) (indexFile, error) {
	f := indexFile{Path: path, Chunks: []indexChunk{}}
	whole := sha256.New()
	err := chunk.Split(r, func(b []byte) error {
		whole.Write(b)
		h := sha256.Sum256(b)
		f.Chunks = append(f.Chunks, indexChunk{Hash: hex.EncodeToString(h[:]), Size: len(b), Off: f.Size})
		f.Size += int64(len(b))
		return nil
	})
	f.SHA256 = hex.EncodeToString(whole.Sum(nil))
	return f, err
}

// indexingTar copies the regular files and directories of src to w as a tar
// for PutSite (other entry types are ignored by PutSite anyway) and returns
// the index of the regular files as PutSite publishes them: cleaned paths, the
// last entry for a path wins. Unsafe paths are passed through untouched for
// PutSite to reject, as before.
func indexingTar(w io.Writer, src *tar.Reader, limit int64) ([]indexFile, error) {
	tw := tar.NewWriter(w)
	now := time.Now()
	pos := map[string]int{}
	files := []indexFile{}
	var total int64
	for {
		hdr, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := tw.WriteHeader(&tar.Header{Name: hdr.Name, Mode: hdr.Mode & 0o7777, Typeflag: tar.TypeDir, ModTime: now}); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if rel, err := storage.CleanRel(hdr.Name); err == nil && quick.IsReservedPath(rel) {
				return nil, errReserved{rel}
			}
			if total += hdr.Size; total > limit {
				return nil, errTooBig{limit: limit}
			}
			if err := tw.WriteHeader(&tar.Header{Name: hdr.Name, Mode: hdr.Mode & 0o7777, Size: hdr.Size, Typeflag: tar.TypeReg, ModTime: now}); err != nil {
				return nil, err
			}
			f, err := indexOf("", io.TeeReader(src, tw))
			if err != nil {
				return nil, err
			}
			rel, err := storage.CleanRel(hdr.Name)
			if err != nil || rel == "" {
				continue
			}
			f.Path = rel
			if i, ok := pos[rel]; ok {
				files[i] = f
			} else {
				pos[rel] = len(files)
				files = append(files, f)
			}
		}
	}
	// The index is complete here: the caller may use it even if the trailer
	// can't be written because PutSite already stopped reading.
	return files, tw.Close()
}

// errReserved stops a tar deploy carrying files under the reserved folder.
type errReserved struct{ rel string }

func (e errReserved) Error() string { return quick.ReservedPathError(e.rel) }

// indexOfManifest is the index of what a delta commit publishes.
func indexOfManifest(files []deltaFile) []indexFile {
	out := make([]indexFile, 0, len(files))
	for _, df := range files {
		f := indexFile{Path: df.rel, Size: df.size, SHA256: hex.EncodeToString(df.hash[:]), Chunks: make([]indexChunk, 0, len(df.chunks))}
		var off int64
		for _, c := range df.chunks {
			f.Chunks = append(f.Chunks, indexChunk{Hash: hex.EncodeToString(c.Hash[:]), Size: c.Size, Off: off})
			off += int64(c.Size)
		}
		out = append(out, f)
	}
	return out
}

// storedIndex returns the live version's chunk map from its persisted index;
// ok=false if p records no version or the blob is missing or invalid.
func (s *server) storedIndex(site string, p policy) (map[[32]byte]chunkLoc, bool) {
	if p.Version == "" {
		return nil, false
	}
	b, ok, err := s.store.GetIndex(site, p.Version)
	if err != nil || !ok {
		if err != nil {
			log.Printf("site %s: index %s unreadable: %v", site, p.Version, err)
		}
		return nil, false
	}
	files, err := decodeIndex(b, p.Version)
	if err != nil {
		log.Printf("site %s: index %s invalid, rebuilding: %v", site, p.Version, err)
		return nil, false
	}
	return locsOf(files), true
}

// walkIndex chunks the site's published files. Callers hold the site lock, so
// the tree is stable; a file vanishing anyway (edited by hand) is skipped.
func (s *server) walkIndex(site string) ([]indexFile, error) {
	paths, err := s.store.ListFiles(site)
	if err != nil {
		return nil, err
	}
	out := make([]indexFile, 0, len(paths))
	for _, p := range paths {
		rc, _, err := s.store.OpenFile(site, p)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		f, err := indexOf(p, rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// saveIndex stores files as a new version's index and returns its id, or ""
// if storing failed (the next plan rebuilds it).
func (s *server) saveIndex(site string, files []indexFile) string {
	v := newIndexVersion()
	b, err := encodeIndex(v, files)
	if err == nil {
		err = s.store.PutIndex(site, v, b)
	}
	if err != nil {
		log.Printf("site %s: saving the live index failed: %v", site, err)
		return ""
	}
	return v
}

// pruneIndexes drops the blobs p no longer points to (best effort).
func (s *server) pruneIndexes(site string, p policy) {
	var keep []string
	for _, v := range []string{p.Version, p.PrevVersion} {
		if v != "" {
			keep = append(keep, v)
		}
	}
	if err := s.store.PruneIndexes(site, keep...); err != nil {
		log.Printf("site %s: pruning old indexes failed: %v", site, err)
	}
}

// liveIndexForPlan returns the live chunk map for an unlocked plan: the stored
// index if valid, else a rebuild under the site lock (saved for next time).
// version is the metadata Version the map belongs to, so the plan can check
// it is still the live one when it answers.
func (s *server) liveIndexForPlan(site string, cur policy) (idx map[[32]byte]chunkLoc, version string, err error) {
	if idx, ok := s.storedIndex(site, cur); ok {
		return idx, cur.Version, nil
	}
	unlock := s.locks.lock(site)
	defer unlock()
	p, err := s.meta.loadFresh(site)
	if err != nil {
		return nil, "", err
	}
	if idx, ok := s.storedIndex(site, p); ok { // rebuilt or republished meanwhile
		return idx, p.Version, nil
	}
	files, err := s.walkIndex(site)
	if err != nil {
		return nil, "", err
	}
	// An empty site has nothing to remember, and saving would create metadata
	// for a site that doesn't exist.
	version = p.Version // what the metadata says, if recording the rebuild fails
	if len(files) > 0 {
		if v := s.saveIndex(site, files); v != "" {
			p.Version = v
			if err := s.meta.save(site, p); err != nil {
				log.Printf("site %s: recording the rebuilt index failed: %v", site, err)
			} else {
				version = v
				s.pruneIndexes(site, p)
			}
		}
	}
	return locsOf(files), version, nil
}

// liveIndexLocked is the commit's view of the live chunks (site lock held):
// the stored index if valid, else a walk (not saved: a new version follows).
func (s *server) liveIndexLocked(site string, cur policy) (idx map[[32]byte]chunkLoc, stored bool, err error) {
	if idx, ok := s.storedIndex(site, cur); ok {
		return idx, true, nil
	}
	files, err := s.walkIndex(site)
	if err != nil {
		return nil, false, err
	}
	return locsOf(files), false, nil
}

// versions are the index ids recorded for the live and previous trees, plus
// whether this publish reserved the site's creator (first deploy).
type versions struct {
	live, prev string
	reserved   bool // CreatedBy was set by this publish, before writing files
	hadMeta    bool // the site had metadata before the reservation
}

// clearVersions runs before storage changes the site (site lock held): it
// marks both trees as unindexed and, on a first deploy, records the creator,
// so a live site can never end up without its owner even if saving the
// metadata after the publish fails. Returns what was there, for
// publishFailed. Nothing is written when there is nothing to change.
func (s *server) clearVersions(site string, cur *policy, creator string) (versions, error) {
	v := versions{live: cur.Version, prev: cur.PrevVersion}
	if cur.CreatedBy == "" && creator != "" {
		_, had, err := s.store.GetMeta(site)
		if err != nil {
			return v, err
		}
		v.reserved, v.hadMeta = true, had
		cur.CreatedBy, cur.CreatedAt = creator, nowStamp()
	}
	if v.live == "" && v.prev == "" && !v.reserved {
		return v, nil
	}
	cur.Version, cur.PrevVersion = "", ""
	if err := s.meta.save(site, *cur); err != nil {
		cur.Version, cur.PrevVersion = v.live, v.prev
		if v.reserved {
			cur.CreatedBy, cur.CreatedAt = "", ""
		}
		return versions{live: v.live, prev: v.prev}, err
	}
	return v, nil
}

// restoreVersions puts back what clearVersions removed after a publish that
// changed nothing (best effort: if it fails the next plan rebuilds).
func (s *server) restoreVersions(site string, cur *policy, v versions) {
	if v.live == "" && v.prev == "" {
		return
	}
	cur.Version, cur.PrevVersion = v.live, v.prev
	if err := s.meta.save(site, *cur); err != nil {
		log.Printf("site %s: restoring the index versions failed: %v", site, err)
	}
}

// publishFailed handles a PutSite error and returns the status to answer: on
// atomic backends the trees are as they were, so the versions go back and the
// answer is a refusal (400: nothing published). A backend writing in place may
// have changed part of the live tree, which stays unindexed until rebuilt.
// With storage.ErrUncertain the new version may be live: nothing is undone
// (the trees stay unindexed, the creator stays) and the answer is 500, so the
// CLI reports an unknown outcome rather than "nothing published".
func (s *server) publishFailed(site string, cur *policy, v versions, err error) int {
	if errors.Is(err, storage.ErrUncertain) {
		return http.StatusInternalServerError
	}
	if v.reserved {
		s.unreserveCreator(site, cur, v)
	}
	if s.store.AtomicPutSite() {
		s.restoreVersions(site, cur, v)
	}
	return http.StatusBadRequest
}

// unreserveCreator undoes a first deploy's creator reservation when nothing
// was published (best effort). A site that had no metadata goes back to having
// none, so a failed first deploy isn't listed.
func (s *server) unreserveCreator(site string, cur *policy, v versions) {
	// SiteExists counts metadata too (which the reservation just wrote): look
	// at the files.
	if live, err := s.store.ListFiles(site); err != nil || len(live) > 0 {
		return // something is live (in-place backend): it keeps its creator
	}
	cur.CreatedBy, cur.CreatedAt = "", ""
	if !v.hadMeta {
		if _, err := s.store.DeleteSite(site); err != nil {
			log.Printf("site %s: removing a failed first deploy's metadata failed: %v", site, err)
		}
		s.meta.forget(site)
		return
	}
	if err := s.meta.save(site, *cur); err != nil {
		log.Printf("site %s: undoing the creator reservation failed: %v", site, err)
	}
}

// dropLiveVersion forgets the live index after the commit found it out of
// sync with the tree (site lock held; best effort).
func (s *server) dropLiveVersion(site string, cur *policy) {
	cur.Version = ""
	if err := s.meta.save(site, *cur); err != nil {
		log.Printf("site %s: dropping the stale index failed: %v", site, err)
	}
}
