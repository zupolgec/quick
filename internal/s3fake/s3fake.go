// Package s3fake is an in-memory S3 HTTP server for tests: just the subset the
// storage backend uses through minio-go (path-style PUT/GET/HEAD/DELETE of
// objects, multipart uploads, paginated and URL-encoded ListObjectsV2), with
// hooks to inject failures, including transport faults. Signatures are not
// checked.
package s3fake

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	Drop                      // close the connection without answering or applying
	ApplyAndDrop              // apply, then close the connection without answering
	DeferAndDrop              // close the connection; the PUT is applied by ApplyDeferred
)

// Server is a fake S3 endpoint. Helpers take keys within Bucket.
type Server struct {
	*httptest.Server
	Bucket string

	// Hook, if set, is called for every request (method, key within the
	// bucket; "" for bucket-level requests) before it is served. Set it while
	// no request is in flight.
	Hook func(method, key string) Fault

	// MaxKeys, if > 0, caps listing pages (exercises pagination).
	MaxKeys int

	mu       sync.Mutex
	objs     map[string]object // key within the bucket
	uploads  map[string]map[int][]byte
	puts     int
	deferred []func()
}

// New starts a fake server with one bucket.
func New(bucket string) *Server {
	s := &Server{Bucket: bucket, objs: map[string]object{}, uploads: map[string]map[int][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Endpoint is host:port for minio.New.
func (s *Server) Endpoint() string { return strings.TrimPrefix(s.URL, "http://") }

// Put stores an object directly (seeding, e.g. a legacy layout).
func (s *Server) Put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store(key, data)
}

func (s *Server) store(key string, data []byte) {
	sum := md5.Sum(data)
	s.objs[key] = object{data: bytes.Clone(data), mod: time.Now().UTC(), etag: `"` + hex.EncodeToString(sum[:]) + `"`}
}

// Age moves the LastModified of every object under prefix back by d.
func (s *Server) Age(prefix string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, o := range s.objs {
		if strings.HasPrefix(k, prefix) {
			o.mod = o.mod.Add(-d)
			s.objs[k] = o
		}
	}
}

// ApplyDeferred applies the PUTs dropped with DeferAndDrop (a request the
// client gave up on, landing late).
func (s *Server) ApplyDeferred() {
	s.mu.Lock()
	fs := s.deferred
	s.deferred = nil
	for _, f := range fs {
		f()
	}
	s.mu.Unlock()
}

// Get returns an object's content.
func (s *Server) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[key]
	return bytes.Clone(o.data), ok
}

// Keys lists the bucket's keys under prefix, sorted.
func (s *Server) Keys(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
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
		out[k] = string(o.data)
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

func drop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			c.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
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
	switch fault {
	case Fail:
		io.Copy(io.Discard, r.Body)
		s3Error(w, http.StatusForbidden, "AccessDenied")
		return
	case Drop:
		io.Copy(io.Discard, r.Body)
		drop(w)
		return
	}
	q := r.URL.Query()
	var apply func() // the change a request makes, run under mu
	var answer func()
	switch {
	case key == "" && r.Method == http.MethodGet && q.Get("list-type") == "2":
		s.list(w, r)
		return
	case key == "":
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
		return
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := rand.Text()
		s.mu.Lock()
		s.uploads[id] = map[int][]byte{}
		s.mu.Unlock()
		writeXML(w, struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			Bucket   string
			Key      string
			UploadID string `xml:"UploadId"`
		}{Bucket: bucket, Key: key, UploadID: id})
		return
	case r.Method == http.MethodPut && q.Has("uploadId"):
		data, err := readBody(r)
		n, _ := strconv.Atoi(q.Get("partNumber"))
		if err != nil || n < 1 {
			s3Error(w, http.StatusBadRequest, "InvalidPart")
			return
		}
		s.mu.Lock()
		parts, ok := s.uploads[q.Get("uploadId")]
		if ok {
			parts[n] = data
		}
		s.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		sum := md5.Sum(data)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(http.StatusOK)
		return
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var req struct {
			Parts []struct {
				PartNumber int
			} `xml:"Part"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
			s3Error(w, http.StatusBadRequest, "MalformedXML")
			return
		}
		id := q.Get("uploadId")
		s.mu.Lock()
		parts, ok := s.uploads[id]
		var data []byte
		for _, p := range req.Parts {
			b, have := parts[p.PartNumber]
			ok = ok && have
			data = append(data, b...)
		}
		s.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusBadRequest, "InvalidPart")
			return
		}
		apply = func() { delete(s.uploads, id); s.store(key, data); s.puts++ }
		answer = func() {
			writeXML(w, struct {
				XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
				Bucket  string
				Key     string
				ETag    string
			}{Bucket: bucket, Key: key, ETag: s.objs[key].etag})
		}
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		s.mu.Lock()
		delete(s.uploads, q.Get("uploadId"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	case r.Method == http.MethodPut:
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			s3Error(w, http.StatusNotImplemented, "NotImplemented")
			return
		}
		data, err := readBody(r)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		apply = func() { s.store(key, data); s.puts++ }
		answer = func() {
			w.Header().Set("ETag", s.objs[key].etag)
			w.WriteHeader(http.StatusOK)
		}
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		s.mu.Lock()
		o, ok := s.objs[key]
		s.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", o.etag)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", o.mod, bytes.NewReader(o.data))
		return
	case r.Method == http.MethodDelete:
		apply = func() { delete(s.objs, key) }
		answer = func() { w.WriteHeader(http.StatusNoContent) }
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
		return
	}
	s.mu.Lock()
	switch fault {
	case DeferAndDrop:
		s.deferred = append(s.deferred, apply)
	default:
		apply()
	}
	s.mu.Unlock()
	switch fault {
	case ApplyAndFail:
		s3Error(w, http.StatusForbidden, "AccessDenied")
	case ApplyAndDrop, DeferAndDrop:
		drop(w)
	default:
		s.mu.Lock()
		answer()
		s.mu.Unlock()
	}
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, xml.Header)
	xml.NewEncoder(w).Encode(v)
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
			io.Copy(io.Discard, br) // trailers
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
	EncodingType          string        `xml:"EncodingType,omitempty"`
	IsTruncated           bool          `xml:"IsTruncated"`
	ContinuationToken     string        `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	Contents              []listContent `xml:"Contents"`
}

// list serves ListObjectsV2 (recursive only: no delimiter support), paginated
// and URL-encoding keys when asked, like S3.
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
	if s.MaxKeys > 0 && s.MaxKeys < maxKeys {
		maxKeys = s.MaxKeys
	}
	after := q.Get("start-after")
	if t := q.Get("continuation-token"); t != "" {
		after = t
	}
	enc := func(k string) string { return k }
	res := listResult{Name: s.Bucket, Prefix: prefix, MaxKeys: maxKeys, ContinuationToken: q.Get("continuation-token")}
	if q.Get("encoding-type") == "url" {
		res.EncodingType = "url"
		enc = url.QueryEscape
		res.Prefix = enc(prefix)
	}
	s.mu.Lock()
	var keys []string
	for k := range s.objs {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i == maxKeys {
			res.IsTruncated = true
			res.NextContinuationToken = keys[i-1]
			break
		}
		o := s.objs[k]
		res.Contents = append(res.Contents, listContent{Key: enc(k), LastModified: o.mod.Format(time.RFC3339Nano), ETag: o.etag, Size: int64(len(o.data)), StorageClass: "STANDARD"})
	}
	res.KeyCount = len(res.Contents)
	s.mu.Unlock()
	writeXML(w, res)
}
