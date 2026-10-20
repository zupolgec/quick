package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/storage"
)

func putSite(t *testing.T, st storage.Backend, site string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	if err := st.PutSite(site, tar.NewReader(&buf)); err != nil {
		t.Fatal(err)
	}
}

func newTestServer(t *testing.T) *server {
	t.Helper()
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: t.TempDir(), MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{store: st, rules: newRulesStore(st, 5*time.Second)}
	s.setupSiteProxy()
	return s
}

func get(t *testing.T, s *server, sub, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.serveSite(w, httptest.NewRequest(http.MethodGet, path, nil), sub)
	return w
}

func TestServeNotFoundDoesNotFallBackToIndex(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"index.html":       "<h1>home</h1>",
		"about/index.html": "<h1>about</h1>",
	})

	if w := get(t, s, "demo", "/"); w.Code != http.StatusOK {
		t.Errorf("/: code %d, want 200", w.Code)
	}
	if w := get(t, s, "demo", "/index.html"); w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "/" {
		t.Errorf("/index.html: code %d location %q, want 307 /", w.Code, w.Header().Get("Location"))
	}
	if w := get(t, s, "demo", "/about/"); w.Code != http.StatusOK || w.Body.String() != "<h1>about</h1>" {
		t.Errorf("/about/: code %d body %q", w.Code, w.Body.String())
	}
	// Missing asset must 404, NOT fall back to the home page with 200.
	w := get(t, s, "demo", "/missing.css")
	if w.Code != http.StatusNotFound {
		t.Errorf("/missing.css: code %d, want 404", w.Code)
	}
	if w.Body.String() == "<h1>home</h1>" {
		t.Errorf("/missing.css returned the home page instead of a 404")
	}
}

// After a rollback the restored files are older than the version the browser
// cached, so a plain If-Modified-Since revalidation would 304 and keep showing
// the rolled-back-from version. The identity ETag must make the conditional
// request return the restored content instead.
func TestServeConditionalCacheReflectsRollback(t *testing.T) {
	s := newTestServer(t)
	// Different lengths so the identity ETag changes regardless of mtime resolution.
	putSite(t, s.store, "demo", map[string]string{"index.html": "VERSION ONE"})
	putSite(t, s.store, "demo", map[string]string{"index.html": "VERSION TWO is longer"})

	// Capture the v2 validators, as a browser would cache them.
	w2 := get(t, s, "demo", "/")
	etag2, lastMod2 := w2.Header().Get("ETag"), w2.Header().Get("Last-Modified")
	if etag2 == "" {
		t.Fatal("missing ETag: conditional caching would fall back to the date alone")
	}

	if ok, err := s.store.Rollback("demo"); err != nil || !ok {
		t.Fatalf("rollback: ok=%v err=%v", ok, err)
	}

	// A browser that cached v2 revalidates with v2's validators. After the
	// rollback it must get 200 + the restored v1, not a 304.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("If-None-Match", etag2)
	r.Header.Set("If-Modified-Since", lastMod2)
	s.serveSite(w, r, "demo")
	if w.Code != http.StatusOK {
		t.Fatalf("after rollback: code %d, want 200 (304 = browser shows the wrong version)", w.Code)
	}
	if w.Body.String() != "VERSION ONE" {
		t.Errorf("body %q, want the restored v1", w.Body.String())
	}

	// Caching still works for unchanged content: the current ETag yields 304.
	cur := get(t, s, "demo", "/").Header().Get("ETag")
	w4 := httptest.NewRecorder()
	r4 := httptest.NewRequest(http.MethodGet, "/", nil)
	r4.Header.Set("If-None-Match", cur)
	s.serveSite(w4, r4, "demo")
	if w4.Code != http.StatusNotModified {
		t.Errorf("unchanged content: code %d, want 304", w4.Code)
	}
}

func TestServeCleanURLs(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"about.html":      "<h1>flat</h1>",
		"blog/index.html": "<h1>folder</h1>",
	})

	if w := get(t, s, "demo", "/about"); w.Code != http.StatusOK || w.Body.String() != "<h1>flat</h1>" {
		t.Errorf("/about: code %d body %q", w.Code, w.Body.String())
	}
	if w := get(t, s, "demo", "/blog"); w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "/blog/" {
		t.Errorf("/blog: code %d location %q, want 307 /blog/", w.Code, w.Header().Get("Location"))
	}
	if w := get(t, s, "demo", "/blog/"); w.Code != http.StatusOK || w.Body.String() != "<h1>folder</h1>" {
		t.Errorf("/blog/: code %d body %q", w.Code, w.Body.String())
	}
}

func TestServeHTMLCanonicalRedirects(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"file.html":         "flat",
		"folder/index.html": "folder",
	})

	tests := []struct {
		path     string
		code     int
		location string
		body     string
	}{
		{"/file", http.StatusOK, "", "flat"},
		{"/file.html", http.StatusTemporaryRedirect, "/file", ""},
		{"/file/", http.StatusTemporaryRedirect, "/file", ""},
		{"/file/index", http.StatusTemporaryRedirect, "/file", ""},
		{"/file/index.html", http.StatusTemporaryRedirect, "/file", ""},
		{"/folder", http.StatusTemporaryRedirect, "/folder/", ""},
		{"/folder.html", http.StatusTemporaryRedirect, "/folder", ""},
		{"/folder/", http.StatusOK, "", "folder"},
		{"/folder/index", http.StatusTemporaryRedirect, "/folder", ""},
		{"/folder/index.html", http.StatusTemporaryRedirect, "/folder", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := get(t, s, "demo", tt.path)
			if w.Code != tt.code {
				t.Fatalf("code %d, want %d", w.Code, tt.code)
			}
			if loc := w.Header().Get("Location"); loc != tt.location {
				t.Fatalf("location %q, want %q", loc, tt.location)
			}
			if tt.body != "" && w.Body.String() != tt.body {
				t.Fatalf("body %q, want %q", w.Body.String(), tt.body)
			}
		})
	}
}

func TestServeHTMLCanonicalRedirectPreservesQuery(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{"folder/index.html": "folder"})

	w := get(t, s, "demo", "/folder?utm=1")
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("code %d, want 307", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/folder/?utm=1" {
		t.Fatalf("location %q, want /folder/?utm=1", loc)
	}
}

func TestServeSPAFallback(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"index.html": "home",
		"200.html":   "<div id=app></div>",
	})

	w := get(t, s, "demo", "/dashboard/settings")
	if w.Code != http.StatusOK || w.Body.String() != "<div id=app></div>" {
		t.Errorf("SPA route: code %d body %q, want 200 + app shell", w.Code, w.Body.String())
	}
}

func TestServeNotFoundUsesCustom404(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"index.html": "home",
		"404.html":   "<h1>persa</h1>",
	})

	w := get(t, s, "demo", "/nope")
	if w.Code != http.StatusNotFound {
		t.Errorf("code %d, want 404", w.Code)
	}
	if w.Body.String() != "<h1>persa</h1>" {
		t.Errorf("body %q, want the 404.html", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type %q", ct)
	}
}

func TestServeNotFoundUsesNearestCustom404(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{
		"404.html":      "root 404",
		"docs/404.html": "docs 404",
	})

	w := get(t, s, "demo", "/docs/missing/page")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code %d, want 404", w.Code)
	}
	if w.Body.String() != "docs 404" {
		t.Fatalf("body %q, want nearest docs 404", w.Body.String())
	}

	w = get(t, s, "demo", "/elsewhere/missing")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code %d, want 404", w.Code)
	}
	if w.Body.String() != "root 404" {
		t.Fatalf("body %q, want root 404", w.Body.String())
	}
}

// Unity WebGL builds ship precompressed files (Build/x.wasm.br, x.js.gz…): the
// browser must get the inner type plus Content-Encoding, otherwise the loader
// fails ("Content-Encoding header missing") or the wasm can't stream-compile.
func TestServePrecompressedUnityBuild(t *testing.T) {
	s := newTestServer(t)
	putSite(t, s.store, "game", map[string]string{
		"Build/game.wasm.br":         "BRWASM",
		"Build/game.framework.js.gz": "GZJS",
		"Build/game.data.br":         "BRDATA",
		"Build/game.symbols.json.gz": "GZSYM",
		"Build/game.loader.js":       "loader",
		"Build/game.wasm.unityweb":   "UWEB",
		"downloads/archive.tar.gz":   "TARGZ",
		"Build/game.data.gz":         "GZDATA",
	})
	cases := []struct {
		path, ct, enc string
	}{
		{"/Build/game.wasm.br", "application/wasm", "br"},
		{"/Build/game.framework.js.gz", "text/javascript; charset=utf-8", "gzip"},
		{"/Build/game.data.br", "application/octet-stream", "br"},
		{"/Build/game.data.gz", "application/octet-stream", "gzip"},
		{"/Build/game.symbols.json.gz", "application/json", "gzip"},
		{"/Build/game.loader.js", "text/javascript; charset=utf-8", ""},
		// Decompression fallback: the loader inflates it, no encoding header.
		{"/Build/game.wasm.unityweb", "application/octet-stream", ""},
		// A generic archive stays a download, never transparently decoded.
		{"/downloads/archive.tar.gz", "application/gzip", ""},
	}
	for _, c := range cases {
		w := get(t, s, "game", c.path)
		if w.Code != http.StatusOK {
			t.Errorf("%s: code %d", c.path, w.Code)
			continue
		}
		if got := w.Header().Get("Content-Type"); got != c.ct {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.ct)
		}
		if got := w.Header().Get("Content-Encoding"); got != c.enc {
			t.Errorf("%s: Content-Encoding %q, want %q", c.path, got, c.enc)
		}
		if c.enc != "" {
			// The loader's progress bar reads Content-Length.
			if got := w.Header().Get("Content-Length"); got != strconv.Itoa(w.Body.Len()) {
				t.Errorf("%s: Content-Length %q, body %d bytes", c.path, got, w.Body.Len())
			}
			if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("%s: Vary %q", c.path, got)
			}
		}
	}

	// Range on a precompressed file works on the stored bytes.
	r := httptest.NewRequest(http.MethodGet, "/Build/game.wasm.br", nil)
	r.Header.Set("Range", "bytes=0-1")
	w := httptest.NewRecorder()
	s.serveSite(w, r, "game")
	if w.Code != http.StatusPartialContent || w.Body.String() != "BR" || w.Header().Get("Content-Length") != "2" {
		t.Errorf("range: code %d body %q len %q", w.Code, w.Body.String(), w.Header().Get("Content-Length"))
	}
}

// Unity "decompression fallback" builds name their compressed files .unityweb:
// served with the matching Content-Encoding, the browser inflates them itself
// and the loader skips its slow JavaScript decompression (Unity suggests it in
// the console). Uncompressed .unityweb stays as-is.
func TestServeUnitywebWithContentEncoding(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("wasm bytes"))
	zw.Close()
	// Unity's brotli .unityweb carries this marker in a metadata block near the start.
	br := "\x6b\x00UnityWeb Compressed Content (brotli)\x00rest-of-stream"

	s := newTestServer(t)
	putSite(t, s.store, "game", map[string]string{
		"Build/a.wasm.unityweb":         gz.String(),
		"Build/a.data.unityweb":         br,
		"Build/a.framework.js.unityweb": gz.String(),
		"Build/plain.data.unityweb":     "not compressed",
	})
	cases := []struct{ path, ct, enc string }{
		{"/Build/a.wasm.unityweb", "application/wasm", "gzip"},
		{"/Build/a.data.unityweb", "application/octet-stream", "br"},
		{"/Build/a.framework.js.unityweb", "text/javascript; charset=utf-8", "gzip"},
		{"/Build/plain.data.unityweb", "application/octet-stream", ""},
	}
	for _, c := range cases {
		w := get(t, s, "game", c.path)
		if w.Code != http.StatusOK {
			t.Errorf("%s: code %d", c.path, w.Code)
			continue
		}
		if got := w.Header().Get("Content-Encoding"); got != c.enc {
			t.Errorf("%s: Content-Encoding %q, want %q", c.path, got, c.enc)
		}
		if got := w.Header().Get("Content-Type"); got != c.ct {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.ct)
		}
		if c.enc != "" && w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
			t.Errorf("%s: Content-Length %q, body %d", c.path, w.Header().Get("Content-Length"), w.Body.Len())
		}
	}
	// The whole stored file is sent: sniffing must not eat the first bytes.
	if w := get(t, s, "game", "/Build/a.wasm.unityweb"); w.Body.String() != gz.String() {
		t.Errorf("body altered after sniffing")
	}
}
