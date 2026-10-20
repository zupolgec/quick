package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestTarGzExcludesDotfiles(t *testing.T) {
	dir := t.TempDir()
	write := func(p, c string) {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "hi")
	write("sub/index.html", "x")
	write(".DS_Store", "junk")
	write(".env", "SECRET=1")
	write(".git/config", "g")
	write(".quick", `{"name":"n"}`)
	write(".well-known/acme.txt", "a")
	// Tier 1: secrets without a leading dot.
	write("id_rsa", "PRIVATE")
	write("certs/server.key", "KEY")
	write("app.pem", "CERT")
	write("node_modules/dep/index.js", "m")
	write("build.log", "l")

	buf, err := tarGz(dir)
	if err != nil {
		t.Fatal(err)
	}
	gz, _ := gzip.NewReader(buf)
	tr := tar.NewReader(gz)
	got := map[string]bool{}
	for {
		h, e := tr.Next()
		if e != nil {
			break
		}
		got[h.Name] = true
	}

	for _, w := range []string{"index.html", "sub/index.html", ".well-known/acme.txt"} {
		if !got[w] {
			t.Errorf("missing %q in the tarball", w)
		}
	}
	bad := []string{
		".DS_Store", ".env", ".git/config", ".git", ".quick",
		"id_rsa", "certs/server.key", "app.pem",
		"node_modules/dep/index.js", "build.log",
	}
	for _, b := range bad {
		if got[b] {
			t.Errorf("%q should NOT be in the tarball", b)
		}
	}
}

// A published .quickignore becomes the Tier 2 source of truth: it can re-include
// a default (node_modules) and exclude others; Tier 1 still applies.
func TestQuickignoreOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	write := func(p, c string) {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "hi")
	write("node_modules/keep.js", "k") // no longer excluded: defaults don't apply
	write("draft.html", "d")           // excluded by .quickignore
	write(".env", "SECRET=1")          // Tier 1: always excluded, even here
	write(".quickignore", "draft.html\n")

	p, err := buildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !p.hasQuickignore {
		t.Fatal("expected hasQuickignore=true")
	}
	got := map[string]bool{}
	for _, f := range p.files {
		got[f.rel] = true
	}
	for _, w := range []string{"index.html", "node_modules/keep.js"} {
		if !got[w] {
			t.Errorf("missing %q in the plan", w)
		}
	}
	for _, b := range []string{"draft.html", ".env"} {
		if got[b] {
			t.Errorf("%q should NOT be in the plan", b)
		}
	}
}

// A __quick/ folder in the site is reported by the plan, so the deploy can
// stop before uploading (the server would refuse it anyway).
func TestPlanFlagsReservedQuickFolder(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"index.html", "__quick/quick.js", "docs/__quick/ok.txt"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pl, err := buildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.reserved) != 1 || pl.reserved[0] != "__quick/quick.js" {
		t.Fatalf("reserved %v, want only __quick/quick.js", pl.reserved)
	}
}
