// Package e2e drives the real quick-server and quick binaries: build both,
// run the server in dev mode (QUICK_DEV_NOAUTH=1) on temp dirs, deploy a
// folder with the CLI through a byte-counting TCP proxy.
package e2e

import (
	"bytes"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func build(t *testing.T, dir, pkg string) string {
	t.Helper()
	out := filepath.Join(dir, filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// countingProxy forwards TCP to target and counts the bytes sent upstream
// (client to server).
type countingProxy struct {
	l    net.Listener
	up   atomic.Int64
	wg   sync.WaitGroup
	addr string
}

func newCountingProxy(t *testing.T, target string) *countingProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &countingProxy{l: l, addr: l.Addr().String()}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer c.Close()
				s, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer s.Close()
				go func() { io.Copy(c, s); c.Close() }()
				// Count as bytes flow: keep-alive connections are still open
				// when the test reads the counter.
				io.Copy(countingWriter{s, &p.up}, c)
			}()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return p
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n.Add(int64(n))
	return n, err
}

func randData(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed+1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestDeltaDeployEndToEnd(t *testing.T) {
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
	proxy := newCountingProxy(t, addr)

	// A Unity-like build: a big .data, a .wasm, an index.
	project := filepath.Join(tmp, "project")
	site := filepath.Join(project, "build")
	os.MkdirAll(filepath.Join(site, "Build"), 0o755)
	data := randData(1, 24<<20)
	wasm := randData(2, 8<<20)
	index := []byte("<!doctype html><title>game</title>")
	write := func(p string, b []byte) {
		if err := os.WriteFile(filepath.Join(site, p), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Build/game.data", data)
	write("Build/game.wasm", wasm)
	write("index.html", index)
	total := int64(len(data) + len(wasm) + len(index))

	home := filepath.Join(tmp, "home")
	os.MkdirAll(home, 0o755)
	deploy := func(extra ...string) string {
		t.Helper()
		args := append([]string{"deploy", "game", "build", "--server", "http://" + proxy.addr, "--token", "dev", "--yes"}, extra...)
		cmd := exec.Command(cliBin, args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
			"QUICK_NO_SKILL_AUTOUPDATE=1", "QUICK_SERVER=", "QUICK_TOKEN=", "NO_COLOR=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("quick deploy: %v\n%s", err, out)
		}
		return string(out)
	}

	// First deploy: everything travels.
	proxy.up.Store(0)
	out1 := deploy("--public")
	sent1 := proxy.up.Load()
	t.Logf("first deploy: %d bytes upstream for %d bytes of files\n%s", sent1, total, out1)
	if sent1 < total {
		t.Fatalf("first deploy sent %d bytes, less than the %d bytes of files", sent1, total)
	}
	if !strings.Contains(out1, "uploaded") {
		t.Fatalf("first deploy did not use the delta path:\n%s", out1)
	}

	// Second deploy after changing 64 KiB in the middle of game.data and
	// inserting a few bytes in game.wasm.
	copy(data[len(data)/2:], randData(3, 64<<10))
	wasm = append(append(append([]byte{}, wasm[:len(wasm)/3]...), []byte("patched")...), wasm[len(wasm)/3:]...)
	write("Build/game.data", data)
	write("Build/game.wasm", wasm)

	proxy.up.Store(0)
	out2 := deploy()
	sent2 := proxy.up.Load()
	t.Logf("second deploy: %d bytes upstream\n%s", sent2, out2)
	if sent2 > 2<<20 {
		t.Fatalf("second deploy sent %d bytes, want well under 2 MiB", sent2)
	}

	// The served content matches the folder.
	get := func(p string) []byte {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+p, nil)
		req.Host = "game.127.0.0.1"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", p, resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	if !bytes.Equal(get("/Build/game.data"), data) {
		t.Fatal("served game.data differs from the folder")
	}
	if !bytes.Equal(get("/Build/game.wasm"), wasm) {
		t.Fatal("served game.wasm differs from the folder")
	}
	if !bytes.Equal(get("/"), index) {
		t.Fatal("served index differs from the folder")
	}

	// Rollback restores the first version.
	cmd := exec.Command(cliBin, "rollback", "game", "--server", "http://"+proxy.addr, "--token", "dev", "--yes")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "HOME="+home, "QUICK_NO_SKILL_AUTOUPDATE=1", "QUICK_SERVER=", "NO_COLOR=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("quick rollback: %v\n%s", err, b)
	}
	if bytes.Equal(get("/Build/game.data"), data) {
		t.Fatal("rollback did not restore the previous version")
	}
}
