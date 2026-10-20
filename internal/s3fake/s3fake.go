// Package s3fake is an in-memory S3 HTTP server for tests: just the subset the
// storage backend uses through minio-go (path-style PUT/GET/HEAD/DELETE of
// objects, ListObjectsV2), with hooks to inject failures. Signatures are not
// checked.
package s3fake

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type object struct {
	data []byte
	mod  time.Time
	etag string
}

// Fault is what Hook decides for a request.
type Fault int

const (
	Pass         Fault = iota // serve normally
	Fail                      // answer 403 without applying
	ApplyAndFail              // apply (PUT/DELETE), then answer 403
)

// Server is a fake S3 endpoint. Keys are "<bucket>/<key>" internally; the
// helpers take the key within Bucket.
type Server struct {
	*httptest.Server
	Bucket string

	// Hook, if set, is called for every request (method, key within the
	// bucket; "" for listings) before it is served.
	Hook func(method, key string) Fault

	mu   sync.Mutex
	objs map[string]object
	puts int
}

// New starts a fake server with one bucket.
func New(bucket string) *Server {
	s := &Server{Bucket: bucket, objs: map[string]object{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Endpoint is host:port for minio.New.
func (s *Server) Endpoint() string { return strings.TrimPrefix(s.URL, "http://") }

// Put stores an object directly (seeding, e.g. a legacy layout).
func (s *Server) Put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(s.Bucket+"/"+key, data)
}

func (s *Server) store(full string, data []byte) {
	sum := md5.Sum(data)
	s.objs[full] = object{data: bytes.Clone(data), mod: time.Now().UTC(), etag: `"` + hex.EncodeToString(sum[:]) + `"`}
}

// Get returns an object's content.
func (s *Server) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[s.Bucket+"/"+key]
	return bytes.Clone(o.data), ok
}

// Keys lists the bucket's keys under prefix, sorted.
func (s *Server) Keys(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objs {
		if rest, ok := strings.CutPrefix(k, s.Bucket+"/"); ok && strings.HasPrefix(rest, prefix) {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}

// Snapshot copies every object of the bucket (key → content).
func (s *Server) Snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, o := range s.objs {
		if rest, ok := strings.CutPrefix(k, s.Bucket+"/"); ok {
			out[rest] = string(o.data)
		}
	}
	return out
}

// Puts counts the object PUT requests served so far.
func (s *Server) Puts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != s.Bucket {
		s3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	fault := Pass
	if s.Hook != nil {
		fault = s.Hook(r.Method, key)
	}
	if fault == Fail {
		io.Copy(io.Discard, r.Body)
		s3Error(w, http.StatusForbidden, "AccessDenied")
		return
	}
	full := bucket + "/" + key
	switch {
	case key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		s.list(w, r)
	case key == "":
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
	case r.Method == http.MethodPut:
		if r.Header.Get("X-Amz-Copy-Source") != "" || r.URL.Query().Has("uploadId") {
			s3Error(w, http.StatusNotImplemented, "NotImplemented")
			return
		}
		data, err := readBody(r)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		s.mu.Lock()
		s.store(full, data)
		s.puts++
		etag := s.objs[full].etag
		s.mu.Unlock()
		if fault == ApplyAndFail {
			s3Error(w, http.StatusForbidden, "AccessDenied")
			return
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		s.mu.Lock()
		o, ok := s.objs[full]
		s.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", o.etag)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", o.mod, bytes.NewReader(o.data))
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.objs, full)
		s.mu.Unlock()
		if fault == ApplyAndFail {
			s3Error(w, http.StatusForbidden, "AccessDenied")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// readBody reads a PUT payload, decoding minio's aws-chunked streaming
// signature framing (plain HTTP) when present.
func readBody(r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sz, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(sz, 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, n); err != nil {
			return nil, err
		}
		if _, err := br.Discard(2); err != nil { // CRLF
			return nil, err
		}
	}
}

type listContent struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type listResult struct {
	XMLName               xml.Name      `xml:"ListBucketResult"`
	Name                  string        `xml:"Name"`
	Prefix                string        `xml:"Prefix"`
	KeyCount              int           `xml:"KeyCount"`
	MaxKeys               int           `xml:"MaxKeys"`
	IsTruncated           bool          `xml:"IsTruncated"`
	ContinuationToken     string        `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	Contents              []listContent `xml:"Contents"`
}

// list serves ListObjectsV2 (recursive only: no delimiter support).
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("delimiter") != "" {
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
		return
	}
	prefix := q.Get("prefix")
	maxKeys := 1000
	if v, err := strconv.Atoi(q.Get("max-keys")); err == nil && v > 0 && v < maxKeys {
		maxKeys = v
	}
	after := q.Get("start-after")
	if t := q.Get("continuation-token"); t != "" {
		after = t
	}
	s.mu.Lock()
	var keys []string
	for k := range s.objs {
		if rest, ok := strings.CutPrefix(k, s.Bucket+"/"); ok && strings.HasPrefix(rest, prefix) && rest > after {
			keys = append(keys, rest)
		}
	}
	sort.Strings(keys)
	res := listResult{Name: s.Bucket, Prefix: prefix, MaxKeys: maxKeys, ContinuationToken: q.Get("continuation-token")}
	for i, k := range keys {
		if i == maxKeys {
			res.IsTruncated = true
			res.NextContinuationToken = keys[i-1]
			break
		}
		o := s.objs[s.Bucket+"/"+k]
		res.Contents = append(res.Contents, listContent{Key: k, LastModified: o.mod.Format(time.RFC3339Nano), ETag: o.etag, Size: int64(len(o.data)), StorageClass: "STANDARD"})
	}
	res.KeyCount = len(res.Contents)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, xml.Header)
	xml.NewEncoder(w).Encode(res)
}
