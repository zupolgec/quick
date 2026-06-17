// Backend S3-compatibile (Hetzner Object Storage / MinIO / R2, …) via minio-go.
// Layout oggetti: <prefix>sites/<site>/<path...> per i contenuti,
// <prefix>meta/<site>.json per la policy. Con questo backend il container è
// stateless (nessun volume).
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"archive/tar"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config: connessione all'endpoint S3-compatibile.
type S3Config struct {
	Endpoint  string // host[:port], senza schema
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	Prefix    string // prefisso opzionale dentro al bucket (es. "quick/")
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
		rel, err := cleanRel(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		// Limiti anti-bomb: la dimensione dichiarata è autorevole (tar.Reader legge
		// esattamente hdr.Size per entry, e PutObject ne consuma altrettanti).
		if files++; files > maxExtractFiles {
			return errTooManyFiles
		}
		if hdr.Size < 0 || extracted+hdr.Size > maxExtractBytes {
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
	// Rimuove gli oggetti rimasti dal deploy precedente (consistenza eventuale,
	// non è uno swap atomico: tradeoff accettato).
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

func (s *s3) OpenFile(site, p string) (io.ReadSeekCloser, FileInfo, error) {
	rel, err := cleanRel(p)
	if err != nil {
		return nil, FileInfo{}, err
	}
	ctx := context.Background()
	obj, err := s.cli.GetObject(ctx, s.bucket, s.siteKey(site, rel), minio.GetObjectOptions{})
	if err != nil {
		return nil, FileInfo{}, ErrNotFound
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, FileInfo{}, ErrNotFound
	}
	return obj, FileInfo{Name: path.Base(rel), ModTime: st.LastModified, ETag: st.ETag}, nil
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

// Rollback non è supportato sull'object storage: niente rename atomico né copia
// della versione precedente (vedi README, va fatto via versioning del bucket).
func (s *s3) Rollback(site string) (bool, error) {
	return false, errors.New("rollback non supportato su storage s3 (usa il versioning del bucket)")
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
