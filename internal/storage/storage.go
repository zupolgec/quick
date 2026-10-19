// Package storage abstracts where site files and policy metadata live: local
// filesystem (bind mount) or S3-compatible object storage. The rest of
// quick-server works only against Backend, unaware of the implementation.
package storage

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var ErrNotFound = errors.New("storage: not found")

// Anti-bomb caps applied during tar extraction: limiting the incoming gzip
// stream isn't enough, since a small archive can expand hugely (gzip bomb) or
// hold a vast number of tiny files. Exported so the delta deploy can check a
// manifest against the same caps before any upload; var (not const) so tests
// can lower them without generating enormous archives.
var (
	MaxExtractBytes int64 = 500 << 20 // total extracted (decompressed) bytes
	MaxExtractFiles       = 20000     // max number of files
)

var (
	errArchiveTooBig = fmt.Errorf("storage: archive too large once extracted (over %d MiB)", MaxExtractBytes>>20)
	errTooManyFiles  = fmt.Errorf("storage: archive exceeds the %d-file limit", MaxExtractFiles)
)

type FileInfo struct {
	Name    string
	ModTime time.Time
	ETag    string
}

type Backend interface {
	// PutSite replaces the whole site tree with the tar contents, keeping the
	// previous version for a possible Rollback.
	PutSite(site string, tr *tar.Reader) error
	// AtomicPutSite reports whether PutSite consumes the whole tar before
	// touching the live version (so the live files can be read while the tar
	// is being produced). False for backends that overwrite in place.
	AtomicPutSite() bool
	// OpenFile opens a single site file; ErrNotFound if missing or a dir.
	OpenFile(site, p string) (io.ReadSeekCloser, FileInfo, error)
	// DeleteSite removes site contents and metadata; existed=false if nothing was there.
	DeleteSite(site string) (existed bool, err error)
	SiteExists(site string) (bool, error)
	ListSites() ([]string, error)
	// ListFiles lists the live version's regular files (slash-separated paths
	// relative to the site root); empty if the site has no contents.
	ListFiles(site string) ([]string, error)
	// Rollback restores the previous version (the last deploy becomes the "next").
	// ok=false if there is no previous version to restore.
	Rollback(site string) (ok bool, err error)
	GetMeta(site string) (data []byte, ok bool, err error)
	PutMeta(site string, data []byte) error
}

type Config struct {
	Kind     string // "local" | "s3"
	SitesDir string // local
	MetaDir  string // local
	S3       S3Config
}

func New(c Config) (Backend, error) {
	switch c.Kind {
	case "", "local":
		return newLocal(c.SitesDir, c.MetaDir)
	case "s3":
		return newS3(c.S3)
	default:
		return nil, fmt.Errorf("storage: unknown kind %q (use local|s3)", c.Kind)
	}
}

// CleanRel normalizes a relative path and blocks traversal.
func CleanRel(p string) (string, error) {
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("storage: unsafe path: %q", p)
	}
	return rel, nil
}

// ---- local (filesystem) backend ----

type local struct {
	sitesDir string
	metaDir  string
}

func newLocal(sitesDir, metaDir string) (*local, error) {
	for _, d := range []string{sitesDir, metaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &local{sitesDir: sitesDir, metaDir: metaDir}, nil
}

func (l *local) PutSite(site string, tr *tar.Reader) error {
	tmp := l.uniqueTmp(site, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var extracted int64
	var files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel, err := CleanRel(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		dst := filepath.Join(tmp, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if files++; files > MaxExtractFiles {
				return errTooManyFiles
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.Create(dst)
			if err != nil {
				return err
			}
			// Copy capped to the remaining budget (+1 to detect overflow): a
			// lying size in the header can't fill the disk.
			n, err := io.Copy(f, io.LimitReader(tr, MaxExtractBytes-extracted+1))
			f.Close()
			if err != nil {
				return err
			}
			if extracted += n; extracted > MaxExtractBytes {
				return errArchiveTooBig
			}
		}
	}

	final := filepath.Join(l.sitesDir, site)
	prev := l.prevPath(site)
	if _, err := os.Stat(final); err == nil {
		// Current version becomes the "previous" one (one-level rollback): the
		// older one is discarded.
		os.RemoveAll(prev)
		if err := os.Rename(final, prev); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		// restore the previous version if we had moved it
		if _, e := os.Stat(prev); e == nil {
			os.Rename(prev, final)
		}
		return err
	}
	return nil
}

// AtomicPutSite: extraction goes to a temp dir, swapped in only at the end.
func (l *local) AtomicPutSite() bool { return true }

func (l *local) prevPath(site string) string { return filepath.Join(l.sitesDir, "."+site+".prev") }

// tmpSeq makes temp paths unique: two concurrent operations on the same site
// (beyond the per-site lock upstream) never share the same work dir.
var tmpSeq atomic.Uint64

func (l *local) uniqueTmp(site, kind string) string {
	return filepath.Join(l.sitesDir, fmt.Sprintf(".%s.%s.%d.%d", site, kind, os.Getpid(), tmpSeq.Add(1)))
}

func (l *local) ListSites() ([]string, error) {
	set := map[string]bool{}
	if ents, err := os.ReadDir(l.sitesDir); err == nil {
		for _, e := range ents {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				set[e.Name()] = true
			}
		}
	}
	if ents, err := os.ReadDir(l.metaDir); err == nil {
		for _, e := range ents {
			if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".json") {
				set[strings.TrimSuffix(n, ".json")] = true
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

func (l *local) ListFiles(site string) ([]string, error) {
	root := filepath.Join(l.sitesDir, site)
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p == root && os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out, err
}

func (l *local) Rollback(site string) (bool, error) {
	final := filepath.Join(l.sitesDir, site)
	prev := l.prevPath(site)
	if _, err := os.Stat(prev); err != nil {
		return false, nil // no previous version
	}
	swap := l.uniqueTmp(site, "swap")
	if _, err := os.Stat(final); err == nil {
		if err := os.Rename(final, swap); err != nil {
			return false, err
		}
	}
	if err := os.Rename(prev, final); err != nil {
		os.Rename(swap, final) // restore
		return false, err
	}
	// The former current version becomes the new "previous": a second rollback redoes it.
	if _, err := os.Stat(swap); err == nil {
		os.Rename(swap, prev)
	}
	return true, nil
}

func (l *local) OpenFile(site, p string) (io.ReadSeekCloser, FileInfo, error) {
	rel, err := CleanRel(p)
	if err != nil {
		return nil, FileInfo{}, err
	}
	full := filepath.Join(l.sitesDir, site, rel)
	f, err := os.Open(full)
	if err != nil {
		// Missing (or a path through a file): not found. Anything else (e.g.
		// out of descriptors) is a real error, not to be mistaken for absence.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, FileInfo{}, ErrNotFound
		}
		return nil, FileInfo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, FileInfo{}, err // opened but unreadable: a real error
	}
	if st.IsDir() {
		f.Close()
		return nil, FileInfo{}, ErrNotFound
	}
	// Identity ETag (size + mtime). Without it the browser's conditional cache
	// uses only If-Modified-Since, and after a Rollback (which restores files
	// with an mtime older than the cached version) it would return 304 and show
	// the wrong version. The ETag compares by content, not by date.
	etag := fmt.Sprintf(`"%x-%x"`, st.Size(), st.ModTime().UnixNano())
	return f, FileInfo{Name: filepath.Base(full), ModTime: st.ModTime(), ETag: etag}, nil
}

func (l *local) DeleteSite(site string) (bool, error) {
	existed, err := l.SiteExists(site)
	if err != nil {
		return false, err
	}
	if err := os.RemoveAll(filepath.Join(l.sitesDir, site)); err != nil {
		return existed, err
	}
	os.RemoveAll(l.prevPath(site))
	if err := os.Remove(l.metaPath(site)); err != nil && !os.IsNotExist(err) {
		return existed, err
	}
	return existed, nil
}

func (l *local) SiteExists(site string) (bool, error) {
	if fi, err := os.Stat(filepath.Join(l.sitesDir, site)); err == nil {
		return fi.IsDir(), nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	_, ok, err := l.GetMeta(site)
	return ok, err
}

func (l *local) metaPath(site string) string { return filepath.Join(l.metaDir, site+".json") }

func (l *local) GetMeta(site string) ([]byte, bool, error) {
	b, err := os.ReadFile(l.metaPath(site))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (l *local) PutMeta(site string, data []byte) error {
	tmp := l.metaPath(site) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.metaPath(site))
}
