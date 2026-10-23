package storage

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
)

// tarOf builds an in-memory tar from a path->content map.
func tarOf(files map[string]string) *tar.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	return tar.NewReader(&buf)
}

func newLocalT(t *testing.T) *local {
	t.Helper()
	l, err := newLocal(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Extraction stops past the byte cap (we lower the limit instead of generating 500 MiB).
func TestPutSiteRejectsTooManyBytes(t *testing.T) {
	defer func(v int64) { MaxExtractBytes = v }(MaxExtractBytes)
	MaxExtractBytes = 1024

	l := newLocalT(t)
	err := l.PutSite("demo", tarOf(map[string]string{
		"big.bin": strings.Repeat("x", 4096),
	}))
	if err != errArchiveTooBig {
		t.Fatalf("err = %v, want errArchiveTooBig", err)
	}
}

// Extraction stops past the file-count cap.
func TestPutSiteRejectsTooManyFiles(t *testing.T) {
	defer func(v int) { MaxExtractFiles = v }(MaxExtractFiles)
	MaxExtractFiles = 3

	files := map[string]string{"a.txt": "1", "b.txt": "2", "c.txt": "3", "d.txt": "4"}
	l := newLocalT(t)
	if err := l.PutSite("demo", tarOf(files)); err != errTooManyFiles {
		t.Fatalf("err = %v, want errTooManyFiles", err)
	}
}

func TestListFiles(t *testing.T) {
	l := newLocalT(t)
	if got, err := l.ListFiles("demo"); err != nil || len(got) != 0 {
		t.Fatalf("missing site: %v %v", got, err)
	}
	if err := l.PutSite("demo", tarOf(map[string]string{"index.html": "a", "css/x.css": "b"})); err != nil {
		t.Fatal(err)
	}
	got, err := l.ListFiles("demo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "css/x.css,index.html" {
		t.Fatalf("ListFiles = %v", got)
	}
}

func TestPutSiteWithinLimits(t *testing.T) {
	l := newLocalT(t)
	if err := l.PutSite("demo", tarOf(map[string]string{"index.html": "ciao"})); err != nil {
		t.Fatalf("PutSite: %v", err)
	}
	rc, _, err := l.OpenFile("demo", "index.html")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "ciao" {
		t.Fatalf("content = %q", b)
	}
}

func TestLocalReportsFreeSpace(t *testing.T) {
	be, err := New(Config{Kind: "local", SitesDir: t.TempDir(), MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	sr, ok := be.(SpaceReporter)
	if !ok {
		t.Skip("no free space reporting on this platform")
	}
	if n, err := sr.FreeBytes(); err != nil || n <= 0 {
		t.Fatalf("FreeBytes = %d, %v", n, err)
	}
}
