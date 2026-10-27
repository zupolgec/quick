package e2e

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A site deployed with the CLI gets its _headers on its responses, plus the
// noindex every quick address carries; _headers itself is never served.
func TestHeadersEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	tmp := t.TempDir()
	serverBin := build(t, tmp, "github.com/zupolgec/quick/cmd/quick-server")
	cliBin := build(t, tmp, "github.com/zupolgec/quick/cmd/quick")

	addr := freePort(t)
	srv := exec.Command(serverBin)
	srv.Env = append(os.Environ(),
		"QUICK_DEV_NOAUTH=1",
		"QUICK_BASE_DOMAIN=127.0.0.1",
		"QUICK_ADDR="+addr,
		"QUICK_SITES_DIR="+filepath.Join(tmp, "sites"),
		"QUICK_META_DIR="+filepath.Join(tmp, "meta"),
		"QUICK_CHUNK_DIR="+filepath.Join(tmp, "chunks"),
	)
	var srvLog bytes.Buffer
	srv.Stdout, srv.Stderr = &srvLog, &srvLog
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Process.Kill()
		srv.Wait()
		if t.Failed() {
			t.Logf("server log:\n%s", srvLog.String())
		}
	})
	for i := 0; ; i++ {
		resp, err := http.Get("http://" + addr + "/api/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if i > 100 {
			t.Fatalf("server not up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	site := filepath.Join(tmp, "site")
	os.MkdirAll(filepath.Join(site, "assets"), 0o755)
	files := map[string]string{
		"index.html":   "<h1>draft</h1>",
		"assets/a.svg": "<svg/>",
		"_headers":     "/*\n  X-Frame-Options: DENY\n  Set-Cookie: nope=1\n/assets/*\n  Cache-Control: public, max-age=600\n",
		"_redirects":   "/old / 301\n",
		"robots.txt":   "User-agent: *\nAllow: /\n",
	}
	for p, c := range files {
		if err := os.WriteFile(filepath.Join(site, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(tmp, "home")
	os.MkdirAll(home, 0o755)
	cmd := exec.Command(cliBin, "deploy", "draft", site, "--public", "--yes", "--server", "http://"+addr, "--token", "dev")
	cmd.Dir = tmp
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"QUICK_NO_SKILL_AUTOUPDATE=1", "QUICK_SERVER=", "QUICK_TOKEN=", "NO_COLOR=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("quick deploy: %v\n%s", err, out)
	}

	visit := func(p string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+p, nil)
		req.Host = "draft.127.0.0.1"
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}

	r := visit("/")
	if r.StatusCode != http.StatusOK || r.Header.Get("X-Frame-Options") != "DENY" || r.Header.Get("Set-Cookie") != "" {
		t.Errorf("/: %d %v", r.StatusCode, r.Header)
	}
	r = visit("/assets/a.svg")
	if r.Header.Get("Cache-Control") != "public, max-age=600" || r.Header.Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Errorf("/assets/a.svg: %v", r.Header)
	}
	if !slices.Contains(r.Header.Values("X-Content-Type-Options"), "nosniff") {
		t.Errorf("/assets/a.svg lost nosniff: %v", r.Header)
	}
	if r = visit("/robots.txt"); r.StatusCode != http.StatusOK || r.Header.Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Errorf("/robots.txt: %d %v", r.StatusCode, r.Header)
	}
	if r = visit("/old"); r.StatusCode != http.StatusMovedPermanently || r.Header.Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Errorf("/old: %d %v", r.StatusCode, r.Header)
	}
	if r = visit("/_headers"); r.StatusCode != http.StatusNotFound {
		t.Errorf("/_headers: %d, want 404", r.StatusCode)
	}
}
