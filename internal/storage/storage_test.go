package storage

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
)

// tarOf costruisce un tar in memoria da una mappa path->contenuto.
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

// #5: l'estrazione si ferma oltre il tetto di byte (anche con gzip piccolo: qui
// simuliamo abbassando il limite invece di generare 500 MiB).
func TestPutSiteRejectsTooManyBytes(t *testing.T) {
	defer func(v int64) { maxExtractBytes = v }(maxExtractBytes)
	maxExtractBytes = 1024

	l := newLocalT(t)
	err := l.PutSite("demo", tarOf(map[string]string{
		"big.bin": strings.Repeat("x", 4096),
	}))
	if err != errArchiveTooBig {
		t.Fatalf("err = %v, voluto errArchiveTooBig", err)
	}
}

// #5: l'estrazione si ferma oltre il tetto sul numero di file.
func TestPutSiteRejectsTooManyFiles(t *testing.T) {
	defer func(v int) { maxExtractFiles = v }(maxExtractFiles)
	maxExtractFiles = 3

	files := map[string]string{"a.txt": "1", "b.txt": "2", "c.txt": "3", "d.txt": "4"}
	l := newLocalT(t)
	if err := l.PutSite("demo", tarOf(files)); err != errTooManyFiles {
		t.Fatalf("err = %v, voluto errTooManyFiles", err)
	}
}

// Un deploy entro i limiti passa e il file è leggibile.
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
		t.Fatalf("contenuto = %q", b)
	}
}
