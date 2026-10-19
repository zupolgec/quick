// Deploy lease (client side): while the CLI holds a site's lease it gives it
// back when it stops early, interrupted (SIGINT/SIGTERM) or failing, so a
// rerun doesn't wait for the server's idle timeout. Best effort: if the
// release doesn't get through, the lease idles out on the server.
package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// releaseTimeout bounds the release request: the user is waiting to exit.
const releaseTimeout = 2 * time.Second

// activeLease is the lease this process holds, if any.
type activeLease struct {
	mu        sync.Mutex
	cfg       *cliConfig
	tok, site string
	id        string
}

// held is the process's lease (one deploy per process).
var held activeLease

func (a *activeLease) set(cfg *cliConfig, tok, site, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg, a.tok, a.site, a.id = cfg, tok, site, id
}

// clear forgets the lease: the server released it (deploy ended).
func (a *activeLease) clear() { a.set(nil, "", "", "") }

func (a *activeLease) held() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id != ""
}

// release gives the lease back to the server (best effort) and forgets it.
func (a *activeLease) release() {
	a.mu.Lock()
	cfg, tok, site, id := a.cfg, a.tok, a.site, a.id
	a.cfg, a.tok, a.site, a.id = nil, "", "", ""
	a.mu.Unlock()
	if id == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, cfg.Server+"/api/deploy/lease?name="+url.QueryEscape(site), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	leaseRequest(req, id)
	resp, err := (&http.Client{Timeout: releaseTimeout}).Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// catchInterrupts makes SIGINT/SIGTERM release the held lease before exiting.
func catchInterrupts() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go watchInterrupts(sigs, &held, os.Exit)
}

// watchInterrupts waits for a signal, releases a's lease and exits with the
// shell's interrupted status (128 + signal).
func watchInterrupts(sigs <-chan os.Signal, a *activeLease, exit func(int)) {
	sig := <-sigs
	if a.held() {
		fmt.Fprintln(os.Stderr, "\ninterrupted: giving the site back to other deploys")
		a.release()
	}
	code := 130
	if sig == syscall.SIGTERM {
		code = 143
	}
	exit(code)
}
