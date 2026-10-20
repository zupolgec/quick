// S3-compatible backend (Hetzner Object Storage / MinIO / R2, ...) via minio-go.
// With this backend the container is stateless (no volume).
//
// Object layout:
//
//	<prefix>versions/<site>/<id>/<path>  one tree per publish (id: random hex)
//	<prefix>live/<site>.json             live pointer (live, prev, retired versions)
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
// A pointer write is trusted as refused only on a definitive S3 error answer
// to its only attempt (4xx). A transport error or 5xx may still be applied
// later, so it is ErrUncertain and the new tree is kept; a pointer that lands
// late then names a tree that still exists (see the grace period below).
//
// The pointer is its own object rather than a field of the metadata: the
// metadata has its own cache with a longer TTL and other writers, while the
// pointer must flip promptly and only from the publish path. Reads resolve it
// through a short per-site cache (pointerTTL), updated in-process at once on
// publish, rollback and delete; other processes on the same bucket see a
// switch within the TTL.
//
// Garbage collection runs after each successful publish (best effort,
// failures only logged) and never deletes a tree someone may still read:
//
//   - live and prev are always kept;
//   - a version leaving prev is recorded in the pointer as retired, with the
//     time, and deleted only once retired for the grace period (another
//     process may still resolve it from its cache, a request may still be
//     reading it);
//   - a tree no pointer names (a crashed or uncertain publish) is deleted
//     only once its newest object is older than the grace period;
//   - versions with an open handle or a listing in progress in this process
//     are pinned and skipped;
//   - a legacy flat site keeps serving from sites/<site>/ while it has no
//     pointer; its first publish records when the pointer was created, and
//     the flat objects are deleted once the grace period after that passed.
//
// A pointer that exists but can't be parsed blocks publishing (a clear error,
// nothing changed) but not DeleteSite, which removes everything regardless.
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

// DefaultGC is how long a retired version (or an unreferenced tree) is kept
// before garbage collection may delete it. It must exceed the pointer cache
// TTL, the longest request reading a file and any pointer write still in
// flight.
const DefaultGC = 15 * time.Minute

type s3 struct {
	cli    *minio.Client
	ptrCli *minio.Client // no automatic retries: switchPointer must see every attempt
	bucket string
	prefix string
	grace  time.Duration
	now    func() time.Time

	mu   sync.Mutex
	ptrs map[string]ptrEntry
	seq  uint64         // bumped by every in-process pointer change
	pins map[string]int // pinKey → open handles and listings
}

func newS3(c S3Config) (*s3, error) {
	opts := func(retries int) *minio.Options {
		return &minio.Options{
			Creds:      credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
			Secure:     c.UseSSL,
			Region:     c.Region,
			MaxRetries: retries,
		}
	}
	cli, err := minio.New(c.Endpoint, opts(0))
	if err != nil {
		return nil, err
	}
	ptrCli, err := minio.New(c.Endpoint, opts(1))
	if err != nil {
		return nil, err
	}
	prefix := c.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &s3{
		cli: cli, ptrCli: ptrCli, bucket: c.Bucket, prefix: prefix,
		grace: DefaultGC, now: time.Now,
		ptrs: map[string]ptrEntry{}, pins: map[string]int{},
	}, nil
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
	Format  int       `json:"format"`
	Live    string    `json:"live"`
	Prev    string    `json:"prev,omitempty"`
	Retired []retired `json:"retired,omitempty"`
	// FlatRetired: when the first pointer replaced the legacy flat layout.
	FlatRetired time.Time `json:"flat_retired,omitzero"`
}

type retired struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

func (p s3Pointer) names(id string) bool { return id == p.Live || id == p.Prev }

var errBadPointer = errors.New("invalid live pointer")

// pointerTTL bounds how long a process serves a pointer read from the bucket
// (var so tests can change it).
var pointerTTL = 2 * time.Second

// maxPointerEntries: past it, expired cache entries are dropped (requests for
// arbitrary site names must not grow the cache forever). Var for tests.
var maxPointerEntries = 4096

// pointerReadHook is a test seam: called after a cached read fetched the
// pointer, before it is stored.
var pointerReadHook func(site string)

type ptrEntry struct {
	p  s3Pointer
	ok bool // a pointer exists (else: legacy flat layout or no site)
	at time.Time
}

// readPointer reads the pointer from the bucket, bypassing the cache. A
// pointer that exists but can't be parsed is errBadPointer, never "no
// pointer" (that would silently serve the legacy layout).
func (s *s3) readPointer(site string) (s3Pointer, bool, error) {
	obj, err := s.cli.GetObject(context.Background(), s.bucket, s.pointerKey(site), minio.GetObjectOptions{})
	if err != nil {
		return s3Pointer{}, false, pointerErr(err)
	}
	defer obj.Close()
	b, err := io.ReadAll(io.LimitReader(obj, 64<<10))
	if err != nil {
		return s3Pointer{}, false, pointerErr(err)
	}
	var p s3Pointer
	bad := json.Unmarshal(b, &p) != nil || p.Format != pointerFormat || checkVersion(p.Live) != nil ||
		(p.Prev != "" && checkVersion(p.Prev) != nil)
	for _, r := range p.Retired {
		bad = bad || checkVersion(r.ID) != nil
	}
	if bad {
		return s3Pointer{}, false, fmt.Errorf("storage: site %s: %w (%s): delete the site to reset it", site, errBadPointer, s.pointerKey(site))
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

// pointer is readPointer through the cache. A read stores its result only if
// no in-process change happened since it started (s.seq is global and never
// goes back, so evictions can't make a stale read look current).
func (s *s3) pointer(site string) (s3Pointer, bool, error) {
	s.mu.Lock()
	e, hit := s.ptrs[site]
	if hit && time.Since(e.at) < pointerTTL {
		s.mu.Unlock()
		return e.p, e.ok, nil
	}
	start := s.seq
	s.mu.Unlock()
	p, ok, err := s.readPointer(site)
	if pointerReadHook != nil {
		pointerReadHook(site)
	}
	if err != nil {
		return p, ok, err
	}
	s.mu.Lock()
	if s.seq == start {
		s.ptrs[site] = ptrEntry{p: p, ok: ok, at: time.Now()}
		s.trimPointers()
	} else if cur, hit := s.ptrs[site]; hit {
		p, ok = cur.p, cur.ok // newer than what this read saw
	}
	s.mu.Unlock()
	return p, ok, nil
}

// cachePointer records an in-process change; known=false just forgets the
// site (the next read fetches it).
func (s *s3) cachePointer(site string, p s3Pointer, ok, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if known {
		s.ptrs[site] = ptrEntry{p: p, ok: ok, at: time.Now()}
	} else {
		delete(s.ptrs, site)
	}
}

// trimPointers drops expired entries once the cache is large. Caller holds mu.
func (s *s3) trimPointers() {
	if len(s.ptrs) <= maxPointerEntries {
		return
	}
	for site, e := range s.ptrs {
		if time.Since(e.at) >= pointerTTL {
			delete(s.ptrs, site)
		}
	}
}

func (s *s3) writePointer(site string, p s3Pointer) error {
	b, _ := json.Marshal(p)
	_, err := s.ptrCli.PutObject(context.Background(), s.bucket, s.pointerKey(site),
		bytes.NewReader(b), int64(len(b)), minio.PutObjectOptions{ContentType: "application/json"})
	return err
}

// refused reports a definitive S3 answer that the request was not applied.
func refused(err error) bool {
	code := minio.ToErrorResponse(err).StatusCode
	return code >= 400 && code < 500
}

// pointerAttempts: tries of a pointer write before giving up.
const pointerAttempts = 3

// switchPointer writes next. A refusal of every attempt is a plain error
// (nothing changed); if any attempt may have been applied (transport error,
// 5xx) and none is confirmed, the outcome is ErrUncertain, whatever the
// pointer reads back now: that write may still land.
func (s *s3) switchPointer(site string, next s3Pointer) error {
	var err error
	maybe := false
	for i := range pointerAttempts {
		if i > 0 {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
		}
		if err = s.writePointer(site, next); err == nil {
			s.cachePointer(site, next, true, true)
			return nil
		}
		if refused(err) {
			break
		}
		maybe = true
	}
	s.cachePointer(site, s3Pointer{}, false, false)
	if got, ok, rerr := s.readPointer(site); rerr == nil && ok && got.Live == next.Live && got.Prev == next.Prev {
		s.cachePointer(site, got, true, true)
		return nil
	}
	if maybe {
		return fmt.Errorf("%w: switching the live version: %v", ErrUncertain, err)
	}
	return fmt.Errorf("storage: switching the live version: %w", err)
}

// ---- pins ----

func pinKey(site, id string) string { return site + "/" + id } // id "": flat layout

func (s *s3) pin(key string) func() {
	s.mu.Lock()
	s.pins[key]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.pins[key]--; s.pins[key] <= 0 {
				delete(s.pins, key)
			}
			s.mu.Unlock()
		})
	}
}

func (s *s3) pinned(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pins[key] > 0
}

// root resolves where the live tree's objects are (the pointer's version, or
// the legacy flat layout when there is no pointer) and pins it until unpin.
func (s *s3) root(site string) (prefix string, unpin func(), err error) {
	p, ok, err := s.pointer(site)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return s.sitePrefix(site), s.pin(pinKey(site, "")), nil
	}
	return s.versionPrefix(site, p.Live), s.pin(pinKey(site, p.Live)), nil
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
		old, had, err = s.readPointer(site) // a corrupt pointer refuses the publish
	}
	if err == nil {
		next := s.successor(old, had, id)
		if err = s.switchPointer(site, next); err == nil {
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

// successor is the pointer publishing id over old: the old live becomes prev,
// the old prev is retired now, retirements older than the grace period are
// dropped (by then the tree is collectable by age anyway: it was written
// before it was retired).
func (s *s3) successor(old s3Pointer, had bool, id string) s3Pointer {
	now := s.now()
	next := s3Pointer{Format: pointerFormat, Live: id, FlatRetired: now}
	if !had {
		return next
	}
	next.Prev, next.FlatRetired = old.Live, old.FlatRetired
	all := old.Retired
	if old.Prev != "" {
		all = append(slices.Clone(all), retired{ID: old.Prev, At: now})
	}
	for _, r := range all {
		if !next.names(r.ID) && now.Sub(r.At) < s.grace {
			next.Retired = append(next.Retired, r)
		}
	}
	return next
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

// collect deletes, best effort, the trees nobody may still read (see the
// package comment): neither live nor prev, not pinned, retired (or, if not
// recorded, last written) at least the grace period ago; and the legacy flat
// objects once the grace period after the first pointer passed.
func (s *s3) collect(ctx context.Context, site string, p s3Pointer) {
	now := s.now()
	retiredAt := map[string]time.Time{}
	for _, r := range p.Retired {
		retiredAt[r.ID] = r.At
	}
	base := s.versionsPrefix(site)
	trees := map[string][]string{}
	newest := map[string]time.Time{}
	err := s.list(ctx, base, func(obj minio.ObjectInfo) {
		id, _, _ := strings.Cut(strings.TrimPrefix(obj.Key, base), "/")
		trees[id] = append(trees[id], obj.Key)
		if obj.LastModified.After(newest[id]) {
			newest[id] = obj.LastModified
		}
	})
	for id, keys := range trees {
		if err != nil {
			break
		}
		since, ok := retiredAt[id]
		if !ok {
			since = newest[id]
		}
		if p.names(id) || now.Sub(since) < s.grace || s.pinned(pinKey(site, id)) {
			continue
		}
		err = s.remove(ctx, keys)
	}
	if err == nil && now.Sub(p.FlatRetired) >= s.grace && !s.pinned(pinKey(site, "")) {
		_, err = s.removePrefix(ctx, s.sitePrefix(site))
	}
	if err != nil {
		log.Printf("storage: site %s: collecting old versions: %v", site, err)
	}
}

func (s *s3) list(ctx context.Context, prefix string, fn func(minio.ObjectInfo)) error {
	for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		fn(obj)
	}
	return nil
}

func (s *s3) remove(ctx context.Context, keys []string) error {
	for _, k := range keys {
		if err := s.cli.RemoveObject(ctx, s.bucket, k, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// removePrefix deletes every object under prefix (listed first, so the
// listing never races its own deletions); n is how many were found.
func (s *s3) removePrefix(ctx context.Context, prefix string) (int, error) {
	var keys []string
	if err := s.list(ctx, prefix, func(obj minio.ObjectInfo) { keys = append(keys, obj.Key) }); err != nil {
		return 0, err
	}
	return len(keys), s.remove(ctx, keys)
}

// AtomicPutSite: the new tree is written apart and switched in at the end.
func (s *s3) AtomicPutSite() bool { return true }

// pinnedObject keeps its version pinned until closed.
type pinnedObject struct {
	*minio.Object
	unpin func()
}

func (o pinnedObject) Close() error {
	defer o.unpin()
	return o.Object.Close()
}

func (s *s3) OpenFile(site, p string) (io.ReadSeekCloser, FileInfo, error) {
	rel, err := CleanRel(p)
	if err != nil {
		return nil, FileInfo{}, err
	}
	root, unpin, err := s.root(site)
	if err != nil {
		return nil, FileInfo{}, err
	}
	ctx := context.Background()
	obj, err := s.cli.GetObject(ctx, s.bucket, root+rel, minio.GetObjectOptions{})
	if err != nil {
		unpin()
		return nil, FileInfo{}, s3OpenErr(err)
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		unpin()
		return nil, FileInfo{}, s3OpenErr(err)
	}
	return pinnedObject{obj, unpin}, FileInfo{Name: path.Base(rel), ModTime: st.LastModified, ETag: st.ETag}, nil
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
// every version, the indexes and the metadata. It works with a corrupt
// pointer too.
func (s *s3) DeleteSite(site string) (bool, error) {
	ctx := context.Background()
	n, err := s.removePrefix(ctx, s.sitePrefix(site))
	existed := n > 0
	if err != nil {
		return existed, err
	}
	_, had, err := s.readPointer(site)
	if errors.Is(err, errBadPointer) {
		had, err = true, nil
	}
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
	err := s.list(ctx, sitesBase, func(obj minio.ObjectInfo) {
		rest := strings.TrimPrefix(obj.Key, sitesBase)
		if i := strings.IndexByte(rest, '/'); i > 0 {
			set[rest[:i]] = true
		}
	})
	if err != nil {
		return nil, err
	}
	for _, base := range []string{s.prefix + "live/", s.prefix + "meta/"} {
		err := s.list(ctx, base, func(obj minio.ObjectInfo) {
			if n := strings.TrimSuffix(strings.TrimPrefix(obj.Key, base), ".json"); n != "" && !strings.Contains(n, "/") {
				set[n] = true
			}
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// ListFiles lists the live tree, pinned while listing.
func (s *s3) ListFiles(site string) ([]string, error) {
	base, unpin, err := s.root(site)
	if err != nil {
		return nil, err
	}
	defer unpin()
	var out []string
	err = s.list(context.Background(), base, func(obj minio.ObjectInfo) {
		if rel := strings.TrimPrefix(obj.Key, base); rel != "" && !strings.HasSuffix(rel, "/") {
			out = append(out, rel)
		}
	})
	if err != nil {
		return nil, err
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
	next := cur
	next.Live, next.Prev = cur.Prev, cur.Live
	if err := s.switchPointer(site, next); err != nil {
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
