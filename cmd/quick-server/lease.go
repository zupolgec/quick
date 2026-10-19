// Deploy lease: one deploy at a time per site.
//
// A delta deploy is several requests (plan, chunk uploads, commit) spread over
// minutes, and only the commit takes the site lock: without a lease two
// deploys of the same site would interleave and the last commit would win
// silently. The lease makes the second one wait its turn, with a clear answer.
//
//   - The plan acquires it and answers with its id (random); the CLI sends the
//     id back (quick.DeployLeaseHeader) on every chunk upload, the commit and
//     re-plans of the same deploy, so the 409 re-plan loop keeps it.
//   - The lease is bound to the identity that opened it (ownerOf): the id
//     alone is not enough, another identity using it gets 423 "deploy owned
//     by X"; taking over is the only way in for someone else.
//   - Every request carrying the id renews it. A request in flight (a long
//     chunk upload) keeps it alive however long it takes; it expires after
//     leaseIdle without requests. An expired lease is dead: its holder's later
//     requests are refused as lost (423) even if nobody took the site since,
//     so an interrupted deploy is never published (re-running it is cheap:
//     what it uploaded is still staged).
//   - Released when the deploy ends: successful commit, or a commit failure
//     other than 409 (which asks for a re-plan). A failed first plan releases
//     the lease it just opened; a failed re-plan or upload keeps it (the CLI's
//     tar fallback on 503 carries the id, otherwise it idles out). An
//     interrupted or failing CLI gives it back with DELETE /api/deploy/lease
//     (handleDeployLease), so a rerun doesn't wait for the idle timeout.
//   - Anything else that would publish the site meanwhile gets 423 Locked
//     naming the holder and since when: a plan without the id (even by the
//     same person from another terminal), chunks or commit with another or no
//     id, the legacy tar upload (which holds the lease itself while it
//     uploads), rollback. ?takeover=1 on a plan, tar upload or rollback breaks
//     the lease after the usual auth and ownership checks (logged); the
//     broken holder's next request gets 423 lease lost. A commit already
//     publishing when the lease is broken completes; the forcing deploy's own
//     commit queues behind it on the site lock and publishes after.
//   - Delete is allowed during a deploy and clears the lease: the holder's
//     commit is then refused and can't bring the site back.
//   - Chunks and commit require the id: no CLI without lease support speaks
//     the delta protocol (the protocol and the lease shipped together). Older
//     CLIs only use the tar upload, which works whenever no lease is live.
//
// In memory only, like the other per-site state of a single instance: a
// restart drops all leases, and deploys in progress get 423 lease lost.
//
// Lock order: s.locks (site) -> stage / domainsMu -> leaseTable.mu. The lease
// mutex is a leaf: held only inside this file's methods, which never take
// another lock, so it can be taken anywhere (the commit and tar upload check
// the lease under the site lock, the plan and uploads without it).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

// leaseIdle is how long a deploy may go without requests before it loses the
// site.
const leaseIdle = 2 * time.Minute

type deployLease struct {
	id       string
	holder   string // identity actor: email or token:<name>@<site>
	owner    string // identity key (ownerOf): only it can use the id
	started  time.Time
	last     time.Time // end of the last request
	inflight int       // requests of this deploy being served
}

// leaseTable holds the live lease of each site. Zero value ready to use.
type leaseTable struct {
	mu   sync.Mutex
	m    map[string]*deployLease
	idle time.Duration    // 0 = leaseIdle
	now  func() time.Time // nil = time.Now (test seam)
}

// Why a request was refused.
const (
	refLocked  = iota // another deploy holds the site
	refLost           // the request's own lease is gone
	refForeign        // the id is the live lease, but another identity's
)

// leaseError is a refusal: the site is someone else's (or nobody's) right now.
type leaseError struct {
	kind   int
	holder string
	since  time.Time
}

func (e *leaseError) Error() string {
	switch e.kind {
	case refLost:
		return "this deploy is no longer the active one for this site (another deploy took over, the site was deleted, or it was idle too long): nothing was published, run it again"
	case refForeign:
		return fmt.Sprintf("deploy owned by %s: only they can continue it; retry in a moment or use --takeover", e.holder)
	}
	return fmt.Sprintf("%s is publishing this site (since %s UTC); retry in a moment or use --takeover", e.holder, e.since.UTC().Format("15:04"))
}

// ownerOf is the identity a lease is bound to: the actor, and for a deploy
// token its id too (a revoked and recreated token with the same name is
// another identity).
func ownerOf(ident authIdentity) string { return actorOf(ident) + "\x00" + ident.TokenID }

func (t *leaseTable) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// current returns site's live lease, forgetting a dead one. Caller holds mu.
func (t *leaseTable) current(site string, now time.Time) *deployLease {
	l := t.m[site]
	if l == nil {
		return nil
	}
	idle := t.idle
	if idle == 0 {
		idle = leaseIdle
	}
	if l.inflight > 0 || now.Sub(l.last) < idle {
		return l
	}
	delete(t.m, site)
	return nil
}

func (t *leaseTable) refusal(cur *deployLease, kind int) *leaseError {
	e := &leaseError{kind: kind}
	if cur != nil {
		e.holder, e.since = cur.holder, cur.started
	}
	return e
}

// begin starts a request that may open a deploy (plan, tar upload): with the
// site's current id (and the identity that opened it) it continues that
// deploy; without an id it opens a new one if the site is free; takeover
// replaces whatever lease is there (returned as broken when it was live).
// Pair with end or release.
func (t *leaseTable) begin(site, id, owner, holder string, takeover bool) (l, broken *deployLease, err *leaseError) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	cur := t.current(site, now)
	switch {
	case cur != nil && id != "" && cur.id == id && cur.owner == owner:
	case takeover:
		broken = cur
		cur = nil
	case cur != nil && id != "" && cur.id == id:
		return nil, nil, t.refusal(cur, refForeign)
	case id != "":
		return nil, nil, t.refusal(cur, refLost)
	case cur != nil:
		return nil, nil, t.refusal(cur, refLocked)
	}
	if cur == nil {
		if t.m == nil {
			t.m = map[string]*deployLease{}
		}
		cur = &deployLease{id: randomID(18), holder: holder, owner: owner, started: now}
		t.m[site] = cur
	}
	cur.inflight++
	cur.last = now
	return cur, broken, nil
}

// use checks that id is site's live lease and owner's. Caller holds mu.
func (t *leaseTable) use(site, id, owner string, now time.Time) (*deployLease, *leaseError) {
	cur := t.current(site, now)
	switch {
	case cur != nil && id != "" && cur.id == id && cur.owner != owner:
		return nil, t.refusal(cur, refForeign)
	case cur != nil && id == "":
		return nil, t.refusal(cur, refLocked)
	case cur == nil || cur.id != id:
		return nil, t.refusal(cur, refLost)
	}
	return cur, nil
}

// join starts a request of a deploy in progress (chunks, commit): id must be
// the site's live lease, opened by owner. Pair with end or release.
func (t *leaseTable) join(site, id, owner string) (*deployLease, *leaseError) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	cur, err := t.use(site, id, owner, now)
	if err != nil {
		return nil, err
	}
	cur.inflight++
	cur.last = now
	return cur, nil
}

// end finishes a request; the deploy goes on (idle timer restarts).
func (t *leaseTable) end(l *deployLease) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l.inflight--
	l.last = t.clock()
}

// release finishes a request and the deploy: the site is free (unless the
// lease was broken meanwhile and belongs to someone else now).
func (t *leaseTable) release(site string, l *deployLease) {
	t.mu.Lock()
	defer t.mu.Unlock()
	l.inflight--
	l.last = t.clock()
	if t.m[site] == l {
		delete(t.m, site)
	}
}

// drop ends the deploy holding id at its holder's request (interrupted CLI).
// No lease, or a dead one: nothing to do. Another live lease, or this one
// used by another identity: refused.
func (t *leaseTable) drop(site, id, owner string) (bool, *leaseError) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	if t.current(site, now) == nil {
		return false, nil
	}
	if _, err := t.use(site, id, owner, now); err != nil {
		return false, err
	}
	delete(t.m, site)
	return true, nil
}

// busy reports the site's live lease (to refuse a rollback), nil if free.
func (t *leaseTable) busy(site string) *leaseError {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur := t.current(site, t.clock()); cur != nil {
		return t.refusal(cur, refLocked)
	}
	return nil
}

// clear drops the site's lease (delete, forced rollback) and returns it if it
// was live.
func (t *leaseTable) clear(site string) *deployLease {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur := t.current(site, t.clock())
	delete(t.m, site)
	return cur
}

// leaseRefused writes a 423 for err: JSON for clients that accept it (the
// CLI), the message as text otherwise (older CLIs print the body).
func leaseRefused(w http.ResponseWriter, r *http.Request, err *leaseError) {
	if !strings.Contains(r.Header.Get("Accept"), "application/json") {
		http.Error(w, err.Error(), http.StatusLocked)
		return
	}
	res := quick.DeployLocked{Error: quick.LeaseLocked, Holder: err.holder, Message: err.Error()}
	if err.kind == refLost {
		res.Error = quick.LeaseLost
	}
	if !err.since.IsZero() {
		res.Since = err.since.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusLocked)
	_ = json.NewEncoder(w).Encode(res)
}

func takeoverRequested(r *http.Request) bool { return r.URL.Query().Get("takeover") == "1" }

// refuseDeploy answers a deploy request refused by the lease. The body is
// read first (up to limit, discarded): a client still sending a large upload
// when the server answers and closes would see a connection reset instead
// of the 423 (older CLIs and proxies don't wait for 100-continue). unlock,
// if set, drops the site lock before that read (callers hold it through
// sync.OnceFunc, so their deferred unlock stays safe).
func refuseDeploy(w http.ResponseWriter, r *http.Request, err *leaseError, limit int64, unlock func()) {
	if unlock != nil {
		unlock()
	}
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, limit))
	leaseRefused(w, r, err)
}

// beginLease is begin for a handler: the request's id and takeover flag, the
// refusal written (see refuseDeploy), a broken lease logged. ok=false:
// response written.
func (s *server) beginLease(w http.ResponseWriter, r *http.Request, site string, ident authIdentity, limit int64, unlock func()) (*deployLease, bool) {
	id := r.Header.Get(quick.DeployLeaseHeader)
	l, broken, err := s.leases.begin(site, id, ownerOf(ident), actorOf(ident), takeoverRequested(r))
	if err != nil {
		refuseDeploy(w, r, err, limit, unlock)
		return nil, false
	}
	if broken != nil {
		logBroken(site, broken, l.holder)
	}
	if l.id != id {
		log.Printf("deploy %q: started by %s", site, l.holder)
	}
	return l, true
}

// joinLease is join for a handler, refusals as in beginLease.
func (s *server) joinLease(w http.ResponseWriter, r *http.Request, site string, ident authIdentity, limit int64, unlock func()) (*deployLease, bool) {
	l, err := s.leases.join(site, r.Header.Get(quick.DeployLeaseHeader), ownerOf(ident))
	if err != nil {
		refuseDeploy(w, r, err, limit, unlock)
		return nil, false
	}
	return l, true
}

// handleDeployLease: DELETE /api/deploy/lease?name= with the lease id gives
// the site back (an interrupted or failed CLI), so a rerun doesn't wait for
// the idle timeout. Usual deploy auth; idempotent (204 also when the lease is
// already gone); 423 if another deploy holds the site. Never publishes: a
// commit already running completes, later requests of that deploy get 423.
func (s *server) handleDeployLease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	name, ok := deployName(w, r)
	if !ok {
		return
	}
	id := r.Header.Get(quick.DeployLeaseHeader)
	if id == "" {
		http.Error(w, "missing "+quick.DeployLeaseHeader, http.StatusBadRequest)
		return
	}
	_, ident, ok := s.authorizeDeploy(w, r, name, false)
	if !ok {
		return
	}
	dropped, err := s.leases.drop(name, id, ownerOf(ident))
	if err != nil {
		leaseRefused(w, r, err)
		return
	}
	if dropped {
		log.Printf("deploy %q: released by its holder", name)
	}
	w.WriteHeader(http.StatusNoContent)
}

func logBroken(site string, l *deployLease, by string) {
	log.Printf("deploy %q: %s broke the deploy of %s (started %s)", site, by, l.holder, l.started.UTC().Format(time.RFC3339))
}

func actorOf(ident authIdentity) string {
	if ident.Actor != "" {
		return ident.Actor
	}
	return ident.Email
}
