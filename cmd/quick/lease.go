// Deploy lease (client side): while the CLI holds a site's lease it gives it
// back when it stops early, interrupted (SIGINT/SIGTERM) or failing, so a
// rerun doesn't wait for the server's idle timeout.
//
// The server opens the lease while answering the first plan, before the CLI
// knows its id: an interrupt in that window waits up to planWait for the
// answer, releases the id it brings and stops the deploy there.
//
// Best effort, remaining gaps (the lease then idles out on the server, 2
// minutes): the plan answer arrives later than planWait or never (the
// server releases a lease whose plan answer it can't deliver), the release
// request fails or times out (releaseTimeout), the process is killed
// (SIGKILL, crash, lost network).
package main

import (
	"errors"
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

// planWait bounds how long an interrupt waits for a plan in flight.
var planWait = 2 * time.Second

// errInterrupted: the deploy stops, the interrupt handler releases and exits.
var errInterrupted = errors.New("interrupted")

// activeLease is the lease this process holds, if any.
type activeLease struct {
	mu          sync.Mutex
	cfg         *cliConfig
	tok, site   string
	id          string
	planning    chan struct{} // open while the first plan is in flight
	interrupted bool
}

// held is the process's lease (one deploy per process).
var held activeLease

// startPlan records that the first plan is about to be sent: the server may
// open a lease before we learn its id.
func (a *activeLease) startPlan(cfg *cliConfig, tok, site string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg, a.tok, a.site, a.id = cfg, tok, site, ""
	a.planning, a.interrupted = make(chan struct{}), false
}

func (a *activeLease) donePlanning() {
	if a.planning != nil {
		close(a.planning)
		a.planning = nil
	}
}

// set records the lease id. false: the process was interrupted meanwhile,
// stop (the interrupt handler releases it).
func (a *activeLease) set(cfg *cliConfig, tok, site, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg, a.tok, a.site, a.id = cfg, tok, site, id
	a.donePlanning()
	return !a.interrupted
}

// clear forgets the lease: the server released it (deploy ended) or none was
// opened.
func (a *activeLease) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg, a.tok, a.site, a.id = nil, "", "", ""
	a.donePlanning()
}

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
	a.donePlanning()
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

// interrupt stops the deploy: waits (bounded) for a plan in flight, then
// releases whatever lease is held. Reports whether there was one.
func (a *activeLease) interrupt() bool {
	a.mu.Lock()
	a.interrupted = true
	planning := a.planning
	a.mu.Unlock()
	if planning != nil {
		select {
		case <-planning:
		case <-time.After(planWait):
		}
	}
	if !a.held() {
		return false
	}
	fmt.Fprintln(os.Stderr, "\ninterrupted: giving the site back to other deploys")
	a.release()
	return true
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
	a.interrupt()
	code := 130
	if sig == syscall.SIGTERM {
		code = 143
	}
	exit(code)
}
