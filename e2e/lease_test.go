package e2e

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer collects a process's output while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// slowProxy forwards TCP to target, client to server at most rate bytes/s per
// connection: long enough uploads for a second deploy to start meanwhile.
func slowProxy(t *testing.T, target string, rate int) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer s.Close()
				go func() { io.Copy(c, s); c.Close() }()
				buf := make([]byte, rate/20)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, err := s.Write(buf[:n]); err != nil {
							return
						}
						time.Sleep(time.Duration(n) * time.Second / time.Duration(rate))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return l.Addr().String()
}

// Two people deploying the same site at once: the second is refused with who
// is publishing, the first publishes; with --takeover the second takes over and
// the first stops without publishing.
func TestConcurrentDeploysOfOneSite(t *testing.T) {
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
	srvLog := &syncBuffer{}
	srv.Stdout, srv.Stderr = srvLog, srvLog
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
	server := "http://" + slowProxy(t, addr, 4<<20) // a 12 MiB upload takes ~3s

	home := filepath.Join(tmp, "home")
	os.MkdirAll(home, 0o755)
	// Two people's checkouts of the same site.
	mario, luigi := filepath.Join(tmp, "mario"), filepath.Join(tmp, "luigi")
	os.MkdirAll(mario, 0o755)
	os.MkdirAll(luigi, 0o755)
	if err := os.WriteFile(filepath.Join(mario, "big.bin"), randData(10, 12<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(luigi, "index.html"), []byte("luigi"), 0o644); err != nil {
		t.Fatal(err)
	}
	quick := func(dir string, extra ...string) *exec.Cmd {
		args := append([]string{"deploy", "game", ".", "--server", server, "--token", "dev", "--yes"}, extra...)
		cmd := exec.Command(cliBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
			"QUICK_NO_SKILL_AUTOUPDATE=1", "QUICK_SERVER=", "QUICK_TOKEN=", "NO_COLOR=1")
		return cmd
	}
	// startSlow starts mario's big deploy and returns once the server gave
	// him the site.
	startSlow := func() (*exec.Cmd, *syncBuffer) {
		t.Helper()
		before := strings.Count(srvLog.String(), `deploy "game": started by`)
		cmd := quick(mario)
		out := &syncBuffer{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; strings.Count(srvLog.String(), `deploy "game": started by`) == before; i++ {
			if i > 200 {
				t.Fatalf("mario's deploy never started:\n%s", out.String())
			}
			time.Sleep(25 * time.Millisecond)
		}
		return cmd, out
	}
	served := func() string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
		req.Host = "game.127.0.0.1"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// 1. Luigi deploys while Mario's upload is running: refused, clearly.
	a, aOut := startSlow()
	out, err := quick(luigi).CombinedOutput()
	if err == nil {
		t.Fatalf("second deploy succeeded during the first:\n%s", out)
	}
	if !strings.Contains(string(out), "dev@example.com is publishing this site (since ") || !strings.Contains(string(out), "use --takeover") {
		t.Fatalf("second deploy message:\n%s", out)
	}
	// --force (empty folders) never takes over someone's deploy.
	if out, err := quick(luigi, "--force").CombinedOutput(); err == nil || !strings.Contains(string(out), "is publishing this site") {
		t.Fatalf("--force during the first deploy: %v\n%s", err, out)
	}
	if err := a.Wait(); err != nil {
		t.Fatalf("first deploy: %v\n%s", err, aOut.String())
	}
	if !strings.Contains(aOut.String(), "published") {
		t.Fatalf("first deploy output:\n%s", aOut.String())
	}

	// 2. Mario is done: Luigi goes through right away.
	if out, err := quick(luigi).CombinedOutput(); err != nil {
		t.Fatalf("deploy after the first finished: %v\n%s", err, out)
	}
	if got := served(); got != "luigi" {
		t.Fatalf("served %q, want luigi", got)
	}

	// 3. Luigi forces during Mario's next upload: Luigi publishes, Mario's
	// deploy stops and publishes nothing.
	a, aOut = startSlow()
	if out, err := quick(luigi, "--takeover").CombinedOutput(); err != nil {
		t.Fatalf("forced deploy: %v\n%s", err, out)
	}
	if err := a.Wait(); err == nil {
		t.Fatalf("broken deploy succeeded:\n%s", aOut.String())
	}
	if !strings.Contains(aOut.String(), "this deploy lost the site") || !strings.Contains(aOut.String(), "nothing was published") {
		t.Fatalf("broken deploy message:\n%s", aOut.String())
	}
	if got := served(); got != "luigi" {
		t.Fatalf("served %q after the broken deploy, want luigi", got)
	}
	if !strings.Contains(srvLog.String(), "broke the deploy of dev@example.com") {
		t.Fatal("forced takeover not logged")
	}

	// 4. Ctrl-C gives the site back: the rerun goes through at once, without
	// --takeover and without waiting for the idle timeout. During the upload
	// (the CLI printed "sending", it has the lease id) and during the plan
	// (the server opened the lease, its answer may still be on the way).
	for _, during := range []string{"upload", "plan"} {
		released := strings.Count(srvLog.String(), `deploy "game": released by its holder`)
		a, aOut = startSlow()
		for i := 0; during == "upload" && !strings.Contains(aOut.String(), "sending "); i++ {
			if i > 400 {
				t.Fatalf("no upload started:\n%s", aOut.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := a.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		err = a.Wait()
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
			t.Fatalf("interrupted during the %s: %v\n%s", during, err, aOut.String())
		}
		if strings.Count(srvLog.String(), `deploy "game": released by its holder`) != released+1 {
			t.Fatalf("interrupted during the %s: no release:\n%s", during, aOut.String())
		}
		if out, err := quick(luigi).CombinedOutput(); err != nil {
			t.Fatalf("deploy right after one interrupted during the %s: %v\n%s", during, err, out)
		}
		if got := served(); got != "luigi" {
			t.Fatalf("served %q after the interrupted deploy, want luigi", got)
		}
	}
}
