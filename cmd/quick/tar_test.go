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
	// Tier 1: segreti senza punto iniziale.
	write("id_rsa", "PRIVATE")
	write("certs/server.key", "KEY")
	write("app.pem", "CERT")
	// Tier 2: comodità.
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
			t.Errorf("manca %q nel tarball", w)
		}
	}
	bad := []string{
		".DS_Store", ".env", ".git/config", ".git", ".quick",
		"id_rsa", "certs/server.key", "app.pem",
		"node_modules/dep/index.js", "build.log",
	}
	for _, b := range bad {
		if got[b] {
			t.Errorf("%q NON dovrebbe essere nel tarball", b)
		}
	}
}

// Un .quickignore pubblicato diventa la fonte di verità del Tier 2: può
// riammettere un default (node_modules) ed escludere altro; il Tier 1 resta.
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
	write("node_modules/keep.js", "k") // non più escluso: i default non valgono
	write("draft.html", "d")           // escluso dal .quickignore
	write(".env", "SECRET=1")          // Tier 1: sempre escluso, anche così
	write(".quickignore", "draft.html\n")

	p, err := buildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !p.hasQuickignore {
		t.Fatal("atteso hasQuickignore=true")
	}
	got := map[string]bool{}
	for _, f := range p.files {
		got[f.rel] = true
	}
	for _, w := range []string{"index.html", "node_modules/keep.js"} {
		if !got[w] {
			t.Errorf("manca %q nel piano", w)
		}
	}
	for _, b := range []string{"draft.html", ".env"} {
		if got[b] {
			t.Errorf("%q NON dovrebbe essere nel piano", b)
		}
	}
}
