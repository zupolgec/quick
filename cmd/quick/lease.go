// Deploy lease (client side) and interrupts.
//
// While the CLI holds a site's lease it gives it back when it stops early,
// so a rerun doesn't wait for the server's idle timeout. One owner: the main
// goroutine. The first SIGINT/SIGTERM only cancels the deploy's context
// (interrupts): the request in flight is abandoned, nothing new is sent
// (every step checks it), and the deploy returns errInterrupted. On the way
// out it releases the lease (bounded, releaseTimeout) before main exits with
// 130/143. A second signal exits at once.
//
//   - The server opens the lease while answering the first plan, before the
//     CLI knows its id: that request runs on a context that outlives the
//     interrupt by planWait, so the id usually arrives and is released.
//   - A commit (or tar upload) in flight when the signal arrives may still
//     complete on the server. What the CLI says about it never depends on
//     errors or signal timing but on the run's publish outcome (pub).
//
// Best effort, remaining gaps (the lease then idles out on the server, 2
// minutes): the plan answer takes longer than planWait (the server releases
// a lease whose answer it can't deliver), the release request fails or
// times out, a second signal cuts it short, the process is killed.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// releaseTimeout bounds the release request: the user is waiting to exit.
const releaseTimeout = 2 * time.Second

// planWait is how long the first plan may still take after an interrupt.
var planWait = 2 * time.Second

var (
	// errInterrupted: the deploy stopped on a signal.
	errInterrupted = errors.New("interrupted")
	// errPublishStarted: interrupted while the commit or tar upload was in
	// flight; the server may have published.
	errPublishStarted = fmt.Errorf("%w: the publish had already started and may have completed", errInterrupted)
)

// interrupts turns the first SIGINT/SIGTERM into a cancelled context. One
// per command run: created when the command starts its interruptible part,
// stopped at its end (stop), after which signals have their default effect
// again. The command honors the recorded signal at its very end.
type interrupts struct {
	ctx, plan   context.Context
	cancel      context.CancelFunc
	cancelPlan  context.CancelFunc
	mu          sync.Mutex
	sig         os.Signal
	done        chan struct{} // closed by stop
	finished    chan struct{} // closed when the watcher returns
	stopOnce    sync.Once
	stopSignals func()
}

// intr is the running command's interrupt state (nil: none, e.g. tests).
var intr *interrupts

// signalStop is signal.Stop (test seam).
var signalStop = signal.Stop

// newInterrupts watches sigs until stop: the first signal cancels, a second
// calls exit.
func newInterrupts(sigs <-chan os.Signal, exit func(int)) *interrupts {
	in := &interrupts{done: make(chan struct{}), finished: make(chan struct{})}
	in.ctx, in.cancel = context.WithCancel(context.Background())
	in.plan, in.cancelPlan = context.WithCancel(context.Background())
	wait := planWait
	go func() {
		defer close(in.finished)
		select {
		case first := <-sigs:
			in.mu.Lock()
			in.sig = first
			in.mu.Unlock()
			in.cancel()
			time.AfterFunc(wait, in.cancelPlan)
		case <-in.done:
			return
		}
		select {
		case <-sigs:
			exit(in.code())
		case <-in.done:
		}
	}()
	return in
}

// catchInterrupts installs the signal handling for the rest of a command
// run; pair with stop.
func catchInterrupts() *interrupts {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	in := newInterrupts(sigs, os.Exit)
	in.stopSignals = func() { signalStop(sigs) }
	intr = in
	return in
}

// stop ends the handling: default signal behavior is back, the watcher is
// gone, contexts are released.
func (in *interrupts) stop() {
	in.stopOnce.Do(func() {
		if in.stopSignals != nil {
			in.stopSignals()
		}
		close(in.done)
		<-in.finished
		in.cancel()
		in.cancelPlan()
		if intr == in {
			intr = nil
		}
	})
}

// code is the exit status for the signal received (128 + signal), 0 if none.
func (in *interrupts) code() int {
	if in == nil {
		return 0
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	switch in.sig {
	case nil:
		return 0
	case syscall.SIGTERM:
		return 143
	}
	return 130
}

// context is for the deploy's requests; planContext for the first plan.
func (in *interrupts) context() context.Context {
	if in == nil {
		return context.Background()
	}
	return in.ctx
}

func (in *interrupts) planContext() context.Context {
	if in == nil {
		return context.Background()
	}
	return in.plan
}

// stopped is errInterrupted once a signal arrived: check before each step.
func (in *interrupts) stopped() error {
	if in.context().Err() != nil {
		return errInterrupted
	}
	return nil
}

// outcome is what a run did to the live site, as far as the CLI can know.
type outcome int32

const (
	notSent   outcome = iota // no publish request (commit, tar upload) written
	sent                     // written, no definitive answer: may have published
	refused                  // the server refused it (4xx): nothing applied
	published                // complete, decoded 2xx
)

// publishOutcome is the run's outcome: set by send when a publish request is
// written and when a refusal comes back, by the callers on a decoded
// success, reset only when a run starts. Every message about the result
// derives from it.
type publishOutcome struct{ v atomic.Int32 }

var pub publishOutcome

func (p *publishOutcome) set(o outcome) { p.v.Store(int32(o)) }
func (p *publishOutcome) get() outcome  { return outcome(p.v.Load()) }

// interruptedReport is what the CLI says when it stops on a signal.
func interruptedReport(o outcome) string {
	switch o {
	case sent:
		return "interrupted: the publish had already started and may have completed"
	case published:
		return "interrupted: the site was published"
	}
	return "interrupted: nothing was published, the site is free again"
}

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

// release gives the lease back to the server (best effort, bounded, on a
// context of its own) and forgets it.
func (a *activeLease) release() {
	a.mu.Lock()
	cfg, tok, site, id := a.cfg, a.tok, a.site, a.id
	a.cfg, a.tok, a.site, a.id = nil, "", "", ""
	a.mu.Unlock()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cfg.Server+"/api/deploy/lease?name="+url.QueryEscape(site), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	leaseRequest(req, id)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
