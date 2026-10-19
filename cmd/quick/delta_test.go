package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The manifest covers exactly the files of the tar path (same ignore rules),
// and every chunk location points at the right bytes.
func TestBuildManifestUsesPlanSelection(t *testing.T) {
	dir := t.TempDir()
	write := func(p string, b []byte) {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i * 7 % 251)
	}
	write("index.html", []byte("hi"))
	write("Build/game.data", big)
	write("empty.txt", nil)
	write(".env", []byte("SECRET=1"))
	write("node_modules/x.js", []byte("x"))

	p, err := buildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, locs, err := buildManifest(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range m.Files {
		got[f.Path] = true
		content, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != f.SHA256 || f.Size != int64(len(content)) {
			t.Errorf("%s: wrong size or hash", f.Path)
		}
		for _, c := range f.Chunks {
			loc := locs[c.Hash]
			b, _ := os.ReadFile(loc.path)
			if sum := sha256.Sum256(b[loc.off : loc.off+int64(loc.size)]); hex.EncodeToString(sum[:]) != c.Hash {
				t.Errorf("%s: chunk location points at the wrong bytes", f.Path)
			}
		}
	}
	if len(got) != 3 || !got["index.html"] || !got["Build/game.data"] || !got["empty.txt"] {
		t.Fatalf("manifest files = %v", got)
	}
}

// Against a server without the delta endpoints (404 on plan) the CLI must
// fall back to the tar upload.
func TestDeltaDeployFallsBackOnOldServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("hi"), 0o644)
	p, _ := buildPlan(dir)
	_, _, err := deltaDeploy(&cliConfig{Server: srv.URL}, "demo", "tok", dir, p)
	if !errors.Is(err, errNoDelta) {
		t.Fatalf("err = %v, want errNoDelta", err)
	}
}
