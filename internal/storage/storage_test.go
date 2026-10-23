package storage

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
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

// A file identical to the live one is shared with it (hardlink), so the
// rollback copy of an unchanged file takes no space; changed files are not.
// Rolling back and forth keeps every version's content.
func TestPutSiteSharesUnchangedFiles(t *testing.T) {
	l := newLocalT(t)
	video := strings.Repeat("v", 1<<20)
	if err := l.PutSite("demo", tarOf(map[string]string{"video.mp4": video, "index.html": "one"})); err != nil {
		t.Fatal(err)
	}
	if err := l.PutSite("demo", tarOf(map[string]string{"video.mp4": video, "index.html": "two"})); err != nil {
		t.Fatal(err)
	}
	stat := func(dir, name string) os.FileInfo {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	live, prev := filepath.Join(l.sitesDir, "demo"), l.prevPath("demo")
	if !os.SameFile(stat(live, "video.mp4"), stat(prev, "video.mp4")) {
		t.Error("unchanged file not shared between versions")
	}
	if os.SameFile(stat(live, "index.html"), stat(prev, "index.html")) {
		t.Error("changed file shared between versions")
	}
	read := func(name string) string {
		rc, _, err := l.OpenFile("demo", name)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		return string(b)
	}
	if ok, err := l.Rollback("demo"); !ok || err != nil {
		t.Fatalf("rollback: %v %v", ok, err)
	}
	if read("index.html") != "one" || read("video.mp4") != video {
		t.Error("rolled back content differs")
	}
	if ok, err := l.Rollback("demo"); !ok || err != nil {
		t.Fatalf("roll forward: %v %v", ok, err)
	}
	if read("index.html") != "two" || read("video.mp4") != video {
		t.Error("rolled forward content differs")
	}
}

// A path listed twice in a deploy, first identical to the live file (so
// linked) then different, must not change the live file through the link.
func TestPutSiteDuplicatePathKeepsLiveIntact(t *testing.T) {
	l := newLocalT(t)
	if err := l.PutSite("demo", tarOf(map[string]string{"a.txt": "old"})); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, c := range []string{"old", "new"} {
		_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(c))
	}
	_ = tw.Close()
	if err := l.PutSite("demo", tar.NewReader(&buf)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(l.prevPath("demo"), "a.txt")); string(b) != "old" {
		t.Fatalf("previous version changed to %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(l.sitesDir, "demo", "a.txt")); string(b) != "new" {
		t.Fatalf("live is %q, want new", b)
	}
}
