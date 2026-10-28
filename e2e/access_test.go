package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `quick access` against the real server in dev mode, where everyone is the
// same user (dev@example.com, the owner): the owner paths end to end.
// Multi-user rules are covered by the server tests.
func TestAccessChosenPeopleEndToEnd(t *testing.T) {
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

	project := filepath.Join(tmp, "project")
	os.MkdirAll(project, 0o755)
	if err := os.WriteFile(filepath.Join(project, "index.html"), []byte("<h1>secret</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	os.MkdirAll(home, 0o755)
	quick := func(wantOK bool, args ...string) string {
		t.Helper()
		cmd := exec.Command(cliBin, append(args, "--server", "http://"+addr, "--token", "dev")...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
			"QUICK_NO_SKILL_AUTOUPDATE=1", "QUICK_SERVER=", "QUICK_TOKEN=", "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantOK {
			t.Fatalf("quick %s: err %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	visit := func() int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
		req.Host = "secret.127.0.0.1"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	listed := func() bool {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/sites", nil)
		req.Host = "127.0.0.1"
		req.Header.Set("Authorization", "Bearer dev")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res struct {
			Sites []struct{ Site, Access string }
		}
		json.NewDecoder(resp.Body).Decode(&res)
		for _, s := range res.Sites {
			if s.Site == "secret" {
				return true
			}
		}
		return false
	}

	quick(true, "deploy", "secret", ".", "--yes")

	if out := quick(true, "access", "secret", "only-me"); !strings.Contains(out, "visible only to you") {
		t.Errorf("only-me:\n%s", out)
	}
	if out := quick(true, "access", "secret"); !strings.Contains(out, "only the owner") || !strings.Contains(out, "Owner:   dev@example.com") {
		t.Errorf("show only-me:\n%s", out)
	}
	if code := visit(); code != http.StatusOK {
		t.Errorf("owner visit: %d", code)
	}
	if !listed() {
		t.Error("owner doesn't see the site in /api/sites")
	}

	if out := quick(true, "access", "secret", "add", "Bob@example.com", "carl@example.com"); !strings.Contains(out, "bob@example.com, carl@example.com") {
		t.Errorf("add:\n%s", out)
	}
	if out := quick(true, "access", "secret", "remove", "carl@example.com"); !strings.Contains(out, "visible to you and: bob@example.com") {
		t.Errorf("remove:\n%s", out)
	}
	out := quick(true, "access", "secret")
	if !strings.Contains(out, "chosen people (1)") || !strings.Contains(out, "Viewers: bob@example.com") {
		t.Errorf("show people:\n%s", out)
	}
	if out := quick(false, "access", "secret", "remove", "dev@example.com"); !strings.Contains(out, "can't be removed") {
		t.Errorf("remove owner:\n%s", out)
	}
	if out := quick(false, "access", "secret", "add", "not-an-email"); !strings.Contains(out, "invalid email") {
		t.Errorf("bad email:\n%s", out)
	}

	// Old shortcuts still work and leave chosen people.
	quick(true, "unpublish", "secret")
	if out := quick(true, "access", "secret"); !strings.Contains(out, "everyone at the company") || strings.Contains(out, "bob@") {
		t.Errorf("after unpublish:\n%s", out)
	}
	if out := quick(false, "access", "secret", "code", "--code", "abc123"); !strings.Contains(out, "at least 8 characters") {
		t.Errorf("short code:\n%s", out)
	}
	out = quick(true, "access", "secret", "code", "--code", "abc12345")
	if !strings.Contains(out, "code: abc12345") || !strings.Contains(out, "link: https://secret.127.0.0.1/?quick_code=abc12345") {
		t.Errorf("code:\n%s", out)
	}
	if out = quick(true, "access", "secret"); !strings.Contains(out, "Code:    abc12345") || !strings.Contains(out, "Link:    https://secret.127.0.0.1/?quick_code=abc12345") {
		t.Errorf("show code:\n%s", out)
	}
	// The link signs in and leaves a clean address; the cookie opens the site.
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/?quick_code=abc12345", nil)
	req.Host = "secret.127.0.0.1"
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "https://secret.127.0.0.1/" || len(resp.Cookies()) == 0 {
		t.Fatalf("code link: %d %q %v", resp.StatusCode, loc, resp.Cookies())
	}
	req, _ = http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	req.Host = "secret.127.0.0.1"
	req.AddCookie(resp.Cookies()[0])
	if resp, err = http.DefaultTransport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("visit with the code cookie: %d", resp.StatusCode)
	}
	if out := quick(true, "access", "secret", "public"); !strings.Contains(out, "public") {
		t.Errorf("public:\n%s", out)
	}
	if code := visit(); code != http.StatusOK {
		t.Errorf("public visit: %d", code)
	}
}
