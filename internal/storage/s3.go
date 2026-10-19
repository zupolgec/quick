// S3-compatible backend (Hetzner Object Storage / MinIO / R2, ...) via minio-go.
// Object layout: <prefix>sites/<site>/<path...> for contents,
// <prefix>meta/<site>.json for policy, <prefix>index/<site>/<version> for the
// delta deploy chunk index. With this backend the container is
// stateless (no volume).
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

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
	return &s3{cli: cli, bucket: c.Bucket, prefix: prefix}, nil
}

func (s *s3) siteKey(site, rel string) string { return s.prefix + "sites/" + site + "/" + rel }
func (s *s3) sitePrefix(site string) string   { return s.prefix + "sites/" + site + "/" }
func (s *s3) metaKey(site string) string      { return s.prefix + "meta/" + site + ".json" }

// Index blobs live under their own prefix: ListSites reads only sites/ and meta/.
func (s *s3) indexPrefix(site string) string { return s.prefix + "index/" + site + "/" }

func (s *s3) PutSite(site string, tr *tar.Reader) error {
	ctx := context.Background()
	keep := map[string]bool{}
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
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel, err := CleanRel(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		// Anti-bomb caps: the declared size is authoritative (tar.Reader reads
		// exactly hdr.Size per entry, and PutObject consumes the same).
		if files++; files > MaxExtractFiles {
			return errTooManyFiles
		}
		if hdr.Size < 0 || extracted+hdr.Size > MaxExtractBytes {
			return errArchiveTooBig
		}
		extracted += hdr.Size
		key := s.siteKey(site, rel)
		ct := mime.TypeByExtension(filepath.Ext(rel))
		if ct == "" {
			ct = "application/octet-stream"
		}
		if _, err := s.cli.PutObject(ctx, s.bucket, key, tr, hdr.Size,
			minio.PutObjectOptions{ContentType: ct}); err != nil {
			return err
		}
		keep[key] = true
	}
	// Remove objects left over from the previous deploy (eventual consistency,
	// not an atomic swap: accepted tradeoff).
	for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix: s.sitePrefix(site), Recursive: true,
	}) {
		if obj.Err != nil {
			return obj.Err
		}
		if !keep[obj.Key] {
			if err := s.cli.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// AtomicPutSite: objects are overwritten while the tar is read.
func (s *s3) AtomicPutSite() bool { return false }

func (s *s3) OpenFile(site, p string) (io.ReadSeekCloser, FileInfo, error) {
	rel, err := CleanRel(p)
	if err != nil {
		return nil, FileInfo{}, err
	}
	ctx := context.Background()
	obj, err := s.cli.GetObject(ctx, s.bucket, s.siteKey(site, rel), minio.GetObjectOptions{})
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

func (s *s3) DeleteSite(site string) (bool, error) {
	ctx := context.Background()
	existed := false
	for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix: s.sitePrefix(site), Recursive: true,
	}) {
		if obj.Err != nil {
			return existed, obj.Err
		}
		if err := s.cli.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return existed, err
		}
		existed = true
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
	metaBase := s.prefix + "meta/"
	for obj := range s.cli.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: metaBase, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if n := strings.TrimSuffix(strings.TrimPrefix(obj.Key, metaBase), ".json"); n != "" {
			set[n] = true
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
	var out []string
	base := s.sitePrefix(site)
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

// Rollback isn't supported on object storage: no atomic rename nor a copy of
// the previous version (use bucket versioning instead, see README).
func (s *s3) Rollback(site string) (bool, error) {
	return false, errors.New("rollback not supported on s3 storage (use bucket versioning)")
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
