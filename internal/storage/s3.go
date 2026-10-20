// S3-compatible backend (Hetzner Object Storage / MinIO / R2, ...) via minio-go.
// With this backend the container is stateless (no volume).
//
// Object layout:
//
//	<prefix>versions/<site>/<id>/<path>  one tree per publish (id: random hex)
//	<prefix>live/<site>.json             live pointer {"live":id,"prev":id}
//	<prefix>sites/<site>/<path>          legacy flat layout (before versioning)
//	<prefix>meta/<site>.json             policy metadata
//	<prefix>index/<site>/<version>       delta deploy chunk index
//
// Publishing is atomic. PutSite writes the new tree under a fresh id, never
// touching the live one, checks it is all there, then switches the pointer
// with a single small PUT: readers see the old or the new pointer, never a
// mix. Rollback swaps live and prev in the pointer the same way. Until the
// switch nothing a reader can see has changed, so a failed publish leaves the
// live site as it was (its partial tree is removed, best effort).
//
// The pointer is its own object rather than a field of the metadata: the
// metadata has its own cache with a longer TTL and other writers, while the
// pointer must flip promptly and only from the publish path. Reads resolve it
// through a short per-site cache (pointerTTL), updated in-process at once on
// publish, rollback and delete; other processes on the same bucket see a
// switch within the TTL.
//
// After a successful switch older trees are collected (best effort, failures
// only logged): every version other than live and prev, crash leftovers
// included (publishes of a site are serialized by the server's site lock), and
// the legacy flat objects. A site without a pointer is served from the legacy
// flat layout; its first publish moves it to the versioned one (no previous
// version to roll back to).
package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"archive/tar"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Config struct {
	Endpoint  string // host[:port], no scheme
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	Prefix    string // optional prefix within the bucket (e.g. "quick/")
	UseSSL    bool
}

type s3 struct {
	cli    *minio.Client
	bucket string
	prefix string

	mu   sync.Mutex
	ptrs map[string]ptrEntry
}

func newS3(c S3Config) (*s3, error) {
	cli, err := minio.New(c.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: c.UseSSL,
		Region: c.Region,
	})
	if err != nil {
		return nil, err
	}
	prefix := c.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &s3{cli: cli, bucket: c.Bucket, prefix: prefix, ptrs: map[string]ptrEntry{}}, nil
}

// sitePrefix is the legacy flat layout.
func (s *s3) sitePrefix(site string) string        { return s.prefix + "sites/" + site + "/" }
func (s *s3) metaKey(site string) string           { return s.prefix + "meta/" + site + ".json" }
func (s *s3) versionsPrefix(site string) string    { return s.prefix + "versions/" + site + "/" }
func (s *s3) versionPrefix(site, id string) string { return s.versionsPrefix(site) + id + "/" }
func (s *s3) pointerKey(site string) string        { return s.prefix + "live/" + site + ".json" }

// Index blobs live under their own prefix: ListSites doesn't read it.
func (s *s3) indexPrefix(site string) string { return s.prefix + "index/" + site + "/" }

// ---- live pointer ----

const pointerFormat = 1

type s3Pointer struct {
	Format int    `json:"format"`
	Live   string `json:"live"`
	Prev   string `json:"prev,omitempty"`
}

// pointerTTL bounds how long a process serves a pointer read from the bucket
// (var so tests can change it).
var pointerTTL = 2 * time.Second

// maxPointerEntries: past it, expired cache entries are dropped (requests for
// arbitrary site names must not grow the cache forever).
const maxPointerEntries = 4096

// ptrEntry caches a site's pointer. gen changes on every in-process update,
// so a read that started before a switch can't store its stale result.
type ptrEntry struct {
	p     s3Pointer
	ok    bool // a pointer exists (else: legacy flat layout or no site)
	valid bool
	at    time.Time
	gen   uint64
}

// readPointer reads the pointer from the bucket, bypassing the cache. A
// pointer that exists but can't be parsed is an error, never "no pointer"
// (that would silently serve the legacy layout).
func (s *s3) readPointer(site string) (s3Pointer, bool, error) {
	obj, err := s.cli.GetObject(context.Background(), s.bucket, s.pointerKey(site), minio.GetObjectOptions{})
	if err != nil {
		return s3Pointer{}, false, pointerErr(err)
	}
	defer obj.Close()
	b, err := io.ReadAll(io.LimitReader(obj, 4<<10))
	if err != nil {
		return s3Pointer{}, false, pointerErr(err)
	}
	var p s3Pointer
	if err := json.Unmarshal(b, &p); err != nil || p.Format != pointerFormat || checkVersion(p.Live) != nil ||
		(p.Prev != "" && checkVersion(p.Prev) != nil) {
		return s3Pointer{}, false, fmt.Errorf("storage: site %s: invalid live pointer", site)
	}
	return p, true, nil
}

// pointerErr: absence is no error (ok=false), anything else is.
func pointerErr(err error) error {
	if errors.Is(s3OpenErr(err), ErrNotFound) {
		return nil
	}
	return err
}

// pointer is readPointer through the cache.
func (s *s3) pointer(site string) (s3Pointer, bool, error) {
	s.mu.Lock()
	e := s.ptrs[site]
	if e.valid && time.Since(e.at) < pointerTTL {
		s.mu.Unlock()
		return e.p, e.ok, nil
	}
	gen := e.gen
	s.mu.Unlock()
	p, ok, err := s.readPointer(site)
	if err != nil {
		return p, ok, err
	}
	s.mu.Lock()
	if s.ptrs[site].gen == gen {
		s.ptrs[site] = ptrEntry{p: p, ok: ok, valid: true, at: time.Now(), gen: gen}
		s.trimPointers()
	}
	s.mu.Unlock()
	return p, ok, nil
}

// cachePointer records an in-process change (valid=false: just forget it).
func (s *s3) cachePointer(site string, p s3Pointer, ok, valid bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ptrs[site] = ptrEntry{p: p, ok: ok, valid: valid, at: time.Now(), gen: s.ptrs[site].gen + 1}
}

// trimPointers drops expired entries once the cache is large. Caller holds mu.
func (s *s3) trimPointers() {
	if len(s.ptrs) <= maxPointerEntries {
		return
	}
	for site, e := range s.ptrs {
		if e.valid && time.Since(e.at) >= pointerTTL {
			delete(s.ptrs, site)
		}
	}
}

func (s *s3) writePointer(site string, p s3Pointer) error {
	b, _ := json.Marshal(p)
	_, err := s.cli.PutObject(context.Background(), s.bucket, s.pointerKey(site),
		bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{ContentType: "application/json"})
	return err
}

// switchPointer replaces old (had: it existed) with next. A write reported
// failed is checked by reading the pointer back: next means it was applied,
// old means nothing changed (plain error); anything else, or no answer, is
// ErrUncertain.
func (s *s3) switchPointer(site string, next, old s3Pointer, had bool) error {
	err := s.writePointer(site, next)
	if err == nil {
		s.cachePointer(site, next, true, true)
		return nil
	}
	s.cachePointer(site, s3Pointer{}, false, false)
	got, ok, rerr := s.readPointer(site)
	switch {
	case rerr == nil && ok && got == next:
		s.cachePointer(site, next, true, true)
		return nil
	case rerr == nil && ok == had && got == old:
		return fmt.Errorf("storage: switching the live version: %w", err)
	default:
		return fmt.Errorf("%w: switching the live version: %v", ErrUncertain, err)
	}
}

// root is where the live tree's objects are: the pointer's version, or the
// legacy flat layout when there is no pointer.
func (s *s3) root(site string) (string, error) {
	p, ok, err := s.pointer(site)
	if err != nil {
		return "", err
	}
	if !ok {
		return s.sitePrefix(site), nil
	}
	return s.versionPrefix(site, p.Live), nil
}

func newS3Version() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- publish ----

func (s *s3) PutSite(site string, tr *tar.Reader) error {
	ctx := context.Background()
	id := newS3Version()
	base := s.versionPrefix(site, id)
	written, err := s.writeTree(ctx, base, tr)
	if err == nil {
		err = s.checkTree(ctx, base, written)
	}
	var old s3Pointer
	var had bool
	if err == nil {
		old, had, err = s.readPointer(site)
	}
	if err == nil {
		next := s3Pointer{Format: pointerFormat, Live: id}
		if had {
			next.Prev = old.Live
		}
		if err = s.switchPointer(site, next, old, had); err == nil {
			s.collect(ctx, site, next)
			return nil
		}
	}
	if !errors.Is(err, ErrUncertain) { // else the new tree may be live
		if _, rerr := s.removePrefix(ctx, base); rerr != nil {
			log.Printf("storage: site %s: removing a failed publish: %v", site, rerr)
		}
	}
	return err
}

// writeTree uploads the tar's regular files under base and returns the keys
// written with their sizes (the last entry for a path wins, as on local).
func (s *s3) writeTree(ctx context.Context, base string, tr *tar.Reader) (map[string]int64, error) {
	written := map[string]int64{}
	var extracted int64
	var files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel, err := CleanRel(hdr.Name)
		if err != nil {
			return written, err
		}
		if rel == "" {
			continue
		}
		// Anti-bomb caps: the declared size is authoritative (tar.Reader reads
		// exactly hdr.Size per entry, and PutObject consumes the same).
		if files++; files > MaxExtractFiles {
			return written, errTooManyFiles
		}
		if hdr.Size < 0 || extracted+hdr.Size > MaxExtractBytes {
			return written, errArchiveTooBig
		}
		extracted += hdr.Size
		ct := mime.TypeByExtension(filepath.Ext(rel))
		if ct == "" {
			ct = "application/octet-stream"
		}
		if _, err := s.cli.PutObject(ctx, s.bucket, base+rel, tr, hdr.Size,
			minio.PutObjectOptions{ContentType: ct}); err != nil {
			return written, err
		}
		written[base+rel] = hdr.Size
	}
}

// checkTree verifies every written object is listed under base with its size
// before the pointer may name it.
func (s *s3) checkTree(ctx context.Context, base string, written map[string]int64) error {
	found := 0
	for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: base, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if size, ok := written[obj.Key]; ok {
			if obj.Size != size {
				return fmt.Errorf("storage: %s: stored %d bytes, wrote %d", obj.Key, obj.Size, size)
			}
			found++
		}
	}
	if found != len(written) {
		return fmt.Errorf("storage: %d of %d objects missing after upload", len(written)-found, len(written))
	}
	return nil
}

// collect removes the trees p doesn't name and the legacy flat objects (best
// effort, after a switch: nothing here is live).
func (s *s3) collect(ctx context.Context, site string, p s3Pointer) {
	base := s.versionsPrefix(site)
	err := s.removeWhere(ctx, base, func(key string) bool {
		id, _, _ := strings.Cut(strings.TrimPrefix(key, base), "/")
		return id != p.Live && id != p.Prev
	})
	if err == nil {
		_, err = s.removePrefix(ctx, s.sitePrefix(site))
	}
	if err != nil {
		log.Printf("storage: site %s: collecting old versions: %v", site, err)
	}
}

// removePrefix deletes every object under prefix; n is how many were found.
func (s *s3) removePrefix(ctx context.Context, prefix string) (n int, err error) {
	err = s.removeWhere(ctx, prefix, func(string) bool { n++; return true })
	return n, err
}

// removeWhere deletes the objects under prefix that match (listed first, so
// the listing never races its own deletions).
func (s *s3) removeWhere(ctx context.Context, prefix string, match func(key string) bool) error {
	var keys []string
	for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if match(obj.Key) {
			keys = append(keys, obj.Key)
		}
	}
	for _, k := range keys {
		if err := s.cli.RemoveObject(ctx, s.bucket, k, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// AtomicPutSite: the new tree is written apart and switched in at the end.
func (s *s3) AtomicPutSite() bool { return true }

func (s *s3) OpenFile(site, p string) (io.ReadSeekCloser, FileInfo, error) {
	rel, err := CleanRel(p)
	if err != nil {
		return nil, FileInfo{}, err
	}
	root, err := s.root(site)
	if err != nil {
		return nil, FileInfo{}, err
	}
	ctx := context.Background()
	obj, err := s.cli.GetObject(ctx, s.bucket, root+rel, minio.GetObjectOptions{})
	if err != nil {
		return nil, FileInfo{}, s3OpenErr(err)
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, FileInfo{}, s3OpenErr(err)
	}
	return obj, FileInfo{Name: path.Base(rel), ModTime: st.LastModified, ETag: st.ETag}, nil
}

// s3OpenErr maps only genuine absence (NoSuchKey, 404) to ErrNotFound; any
// other error (timeout, access denied) is operational and passes through.
func s3OpenErr(err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return err
}

// DeleteSite removes the legacy flat objects first (never read while a
// pointer exists), then the pointer (the site stops serving at once), then
// every version, the indexes and the metadata.
func (s *s3) DeleteSite(site string) (bool, error) {
	ctx := context.Background()
	n, err := s.removePrefix(ctx, s.sitePrefix(site))
	existed := n > 0
	if err != nil {
		return existed, err
	}
	_, had, err := s.readPointer(site)
	if err != nil {
		return existed, err
	}
	existed = existed || had
	err = s.cli.RemoveObject(ctx, s.bucket, s.pointerKey(site), minio.RemoveObjectOptions{})
	s.cachePointer(site, s3Pointer{}, false, err == nil)
	if err != nil {
		return existed, err
	}
	if n, err := s.removePrefix(ctx, s.versionsPrefix(site)); n > 0 || err != nil {
		existed = true
		if err != nil {
			return existed, err
		}
	}
	if err := s.PruneIndexes(site); err != nil {
		return existed, err
	}
	if _, ok, _ := s.GetMeta(site); ok {
		existed = true
	}
	if err := s.cli.RemoveObject(ctx, s.bucket, s.metaKey(site), minio.RemoveObjectOptions{}); err != nil {
		return existed, err
	}
	return existed, nil
}

func (s *s3) SiteExists(site string) (bool, error) {
	if _, ok, err := s.pointer(site); err != nil || ok {
		return ok, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix: s.sitePrefix(site), Recursive: true, MaxKeys: 1,
	})
	if obj, ok := <-ch; ok {
		if obj.Err != nil {
			return false, obj.Err
		}
		return true, nil
	}
	_, ok, err := s.GetMeta(site)
	return ok, err
}

// ListSites: sites with a pointer, a legacy flat tree or metadata.
func (s *s3) ListSites() ([]string, error) {
	ctx := context.Background()
	set := map[string]bool{}
	sitesBase := s.prefix + "sites/"
	for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: sitesBase, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		rest := strings.TrimPrefix(obj.Key, sitesBase)
		if i := strings.IndexByte(rest, '/'); i > 0 {
			set[rest[:i]] = true
		}
	}
	for _, base := range []string{s.prefix + "live/", s.prefix + "meta/"} {
		for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: base, Recursive: true}) {
			if obj.Err != nil {
				return nil, obj.Err
			}
			if n := strings.TrimSuffix(strings.TrimPrefix(obj.Key, base), ".json"); n != "" && !strings.Contains(n, "/") {
				set[n] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func (s *s3) ListFiles(site string) ([]string, error) {
	base, err := s.root(site)
	if err != nil {
		return nil, err
	}
	var out []string
	for obj := range s.cli.ListObjects(context.Background(), s.bucket, minio.ListObjectsOptions{Prefix: base, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if rel := strings.TrimPrefix(obj.Key, base); rel != "" && !strings.HasSuffix(rel, "/") {
			out = append(out, rel)
		}
	}
	return out, nil
}

// Rollback swaps live and prev in the pointer (one atomic PUT). Legacy flat
// sites and sites published once have no previous version.
func (s *s3) Rollback(site string) (bool, error) {
	cur, ok, err := s.readPointer(site)
	if err != nil || !ok || cur.Prev == "" {
		return false, err
	}
	next := s3Pointer{Format: pointerFormat, Live: cur.Prev, Prev: cur.Live}
	if err := s.switchPointer(site, next, cur, true); err != nil {
		return false, err
	}
	return true, nil
}

func (s *s3) GetMeta(site string) ([]byte, bool, error) {
	ctx := context.Background()
	obj, err := s.cli.GetObject(ctx, s.bucket, s.metaKey(site), minio.GetObjectOptions{})
	if err != nil {
		return nil, false, err
	}
	defer obj.Close()
	b, err := io.ReadAll(obj)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

func (s *s3) PutMeta(site string, data []byte) error {
	_, err := s.cli.PutObject(context.Background(), s.bucket, s.metaKey(site),
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json"})
	return err
}

func (s *s3) GetIndex(site, version string) ([]byte, bool, error) {
	if err := checkVersion(version); err != nil {
		return nil, false, err
	}
	obj, err := s.cli.GetObject(context.Background(), s.bucket, s.indexPrefix(site)+version, minio.GetObjectOptions{})
	if err != nil {
		if errors.Is(s3OpenErr(err), ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer obj.Close()
	b, err := readIndexBlob(obj)
	if err != nil {
		if errors.Is(s3OpenErr(err), ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

func (s *s3) PutIndex(site, version string, data []byte) error {
	if err := checkVersion(version); err != nil {
		return err
	}
	_, err := s.cli.PutObject(context.Background(), s.bucket, s.indexPrefix(site)+version,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

func (s *s3) PruneIndexes(site string, keep ...string) error {
	ctx := context.Background()
	base := s.indexPrefix(site)
	for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: base, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if slices.Contains(keep, strings.TrimPrefix(obj.Key, base)) {
			continue
		}
		if err := s.cli.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return nil
}
