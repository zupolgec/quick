// Staging area of the delta deploy: chunks uploaded (or, on backends that
// overwrite in place, copied from the live version) waiting for a commit, one
// file per chunk under <dir>/<site>/<sha256>.
//
// Accounting lives in memory, rebuilt from the directory at startup, and every
// writer goes through put under one mutex, so the caps hold under concurrency
// and no request has to list the directory:
//
//   - uploads are accepted only for chunks an outstanding plan of that site
//     asked for (same hash and size); plans expire after planTTL (an upload
//     session), staged chunks after stageTTL;
//   - each chunk is charged its size rounded up to stageBlock (disk blocks,
//     inode): many tiny chunks cost what they really cost;
//   - per site: at most maxSiteBytes charged and maxSiteChunks chunks
//     (planned + staged). A plan that doesn't fit evicts the oldest requests
//     of that site, only as many as needed; one that can't fit at all is
//     refused without touching existing plans;
//   - across all sites: maxTotalBytes staged, maxTotalWanted outstanding
//     requests and maxSites sites with outstanding requests, checked before a
//     plan is retained (refused with 503, no state change; expired requests
//     are reclaimed first). Empty site states are dropped.
//
// Commit copies of live chunks (non-atomic backends only) skip the plan and the
// per-site caps (they belong to a commit already authorized and bounded by the
// manifest caps) but count toward maxTotalBytes, so the disk stays bounded.
//
// A commit pins its site (pins are tracked apart from site states): the
// sweeper leaves pinned sites alone, so verified chunks can't vanish before
// they're published.
package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const stageBlock = 4 << 10 // charge granularity per staged chunk

// planTTL: how long a plan's chunk requests wait for their upload.
const planTTL = time.Hour

var (
	errNotPlanned = errors.New("chunk not requested by an outstanding plan: plan again")
	errStageFull  = errors.New("too much pending upload data")
	errStageBusy  = errors.New("too many pending deploys on the server, retry later")
)

type stagedChunk struct {
	charge int64
	at     time.Time // last staged or confirmed by a plan
}

type siteStage struct {
	staged map[[32]byte]stagedChunk
	wanted map[[32]byte]wantedChunk
	bytes  int64 // charged bytes of staged
}

type wantedChunk struct {
	size int
	exp  time.Time
	seq  uint64 // plan order, for oldest-first eviction
}

type chunkStage struct {
	dir           string
	maxSiteBytes  int64
	maxSiteChunks int
	maxTotalBytes int64
	// maxTotalWanted: outstanding chunk requests across all sites.
	maxTotalWanted int
	// maxSites: sites with outstanding requests (pending plans).
	maxSites int

	mu          sync.Mutex
	sites       map[string]*siteStage
	pins        map[string]int // commits in progress, per site
	total       int64          // charged bytes staged, all sites
	wantedTotal int            // outstanding requests, all sites
	seq         uint64
}

func charge(size int) int64 { return (int64(size) + stageBlock - 1) / stageBlock * stageBlock }

// newChunkStage opens the staging dir and rebuilds the accounting from it.
// Per-site caps fit the largest legal deploy (storage caps); the aggregate cap
// is a few of those.
//
// Largest legal manifest: every chunk but a file's last is at least MinSize,
// so a file of n bytes has at most n/MinSize+1 chunks and a manifest at most
// maxBytes/MinSize + maxFiles. Each chunk is charged under its size +
// stageBlock, so maxBytes + maxChunks*stageBlock covers any legal manifest.
func newChunkStage(dir string) *chunkStage {
	maxChunks := int(storage.MaxExtractBytes/chunk.MinSize) + storage.MaxExtractFiles
	c := &chunkStage{
		dir:           dir,
		maxSiteBytes:  storage.MaxExtractBytes + int64(maxChunks)*stageBlock,
		maxSiteChunks: maxChunks,
		maxTotalBytes: 4 * storage.MaxExtractBytes,
		// A few maximal plans at once; ~100 bytes per request in memory.
		maxTotalWanted: 4 * maxChunks,
		maxSites:       1000,
		sites:          map[string]*siteStage{},
		pins:           map[string]int{},
	}
	sites, _ := os.ReadDir(dir)
	for _, sd := range sites {
		if !sd.IsDir() || !quick.ValidName(sd.Name()) {
			continue
		}
		ents, _ := os.ReadDir(filepath.Join(dir, sd.Name()))
		for _, e := range ents {
			p := filepath.Join(dir, sd.Name(), e.Name())
			h, herr := parseHash(e.Name())
			fi, err := e.Info()
			if herr != nil || err != nil || !fi.Mode().IsRegular() {
				os.Remove(p) // upload leftovers
				continue
			}
			ss := c.site(sd.Name())
			ch := charge(int(fi.Size()))
			ss.staged[h] = stagedChunk{charge: ch, at: fi.ModTime()}
			ss.bytes += ch
			c.total += ch
		}
	}
	return c
}

// site returns the site's state, creating it. Caller holds mu.
func (c *chunkStage) site(name string) *siteStage {
	ss := c.sites[name]
	if ss == nil {
		ss = &siteStage{staged: map[[32]byte]stagedChunk{}, wanted: map[[32]byte]wantedChunk{}}
		c.sites[name] = ss
	}
	return ss
}

// dropIfIdle forgets an empty, unpinned site. Caller holds mu.
func (c *chunkStage) dropIfIdle(name string) {
	if ss := c.sites[name]; ss != nil && c.pins[name] == 0 && len(ss.staged) == 0 && len(ss.wanted) == 0 {
		delete(c.sites, name)
		os.Remove(c.siteDir(name)) // only if empty
	}
}

func (c *chunkStage) siteDir(site string) string { return filepath.Join(c.dir, site) }
func (c *chunkStage) path(site string, h [32]byte) string {
	return filepath.Join(c.dir, site, hex.EncodeToString(h[:]))
}

// present reports whether the chunk is staged, refreshing its age (a plan
// counting on it keeps it alive).
func (c *chunkStage) present(site string, h [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ss := c.sites[site]
	if ss == nil {
		return false
	}
	sc, ok := ss.staged[h]
	if ok {
		sc.at = time.Now()
		ss.staged[h] = sc
	}
	return ok
}

// pruneWanted drops expired requests. Caller holds mu.
func (c *chunkStage) pruneWanted(ss *siteStage, now time.Time) {
	for h, w := range ss.wanted {
		if now.After(w.exp) {
			delete(ss.wanted, h)
			c.wantedTotal--
		}
	}
}

// reclaim drops expired requests everywhere and the states left idle. Caller
// holds mu.
func (c *chunkStage) reclaim(now time.Time) {
	for name, ss := range c.sites {
		c.pruneWanted(ss, now)
		c.dropIfIdle(name)
	}
}

// planSites counts sites with outstanding requests (what maxSites bounds).
// Caller holds mu.
func (c *chunkStage) planSites() int {
	n := 0
	for _, ss := range c.sites {
		if len(ss.wanted) > 0 {
			n++
		}
	}
	return n
}

// want records the chunks a plan asked for and returns those actually
// admitted: under the lock, chunks staged meanwhile (a concurrent upload) are
// no longer missing. Admission is decided first, on the union with the site's
// outstanding requests, and a refused plan changes nothing (except reclaiming
// expired requests). A site without outstanding requests needs one of the
// maxSites slots; commits in progress (pins) don't hold or grant one. If the
// site is over its chunk cap, only its oldest other requests are evicted, as
// many as needed (their clients get 409 on upload and plan again).
func (c *chunkStage) want(site string, missing map[[32]byte]int) (map[[32]byte]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	admitted := make(map[[32]byte]int, len(missing))
	ss := c.sites[site]
	for h, size := range missing {
		if ss != nil {
			if _, ok := ss.staged[h]; ok {
				continue
			}
		}
		admitted[h] = size
	}
	if len(admitted) == 0 {
		return admitted, nil
	}
	if ss != nil {
		c.pruneWanted(ss, now)
	}
	if ss == nil || len(ss.wanted) == 0 {
		if c.planSites() >= c.maxSites {
			c.reclaim(now)
			if c.planSites() >= c.maxSites {
				return nil, errStageBusy
			}
		}
	}
	ss = c.sites[site] // reclaim may have dropped an idle state
	staged, existing, fresh := 0, 0, len(admitted)
	if ss != nil {
		staged, existing = len(ss.staged), len(ss.wanted)
		for h := range admitted {
			if _, ok := ss.wanted[h]; ok {
				fresh--
			}
		}
	}
	if staged+len(admitted) > c.maxSiteChunks {
		return nil, fmt.Errorf("%w for this site (over %d chunks)", errStageFull, c.maxSiteChunks)
	}
	// Evictable = existing requests not in this plan, which always suffice
	// given the check above.
	evict := max(0, staged+existing+fresh-c.maxSiteChunks)
	if c.wantedTotal-evict+fresh > c.maxTotalWanted {
		c.reclaim(now) // expired plans elsewhere free room
		if c.wantedTotal-evict+fresh > c.maxTotalWanted {
			return nil, errStageBusy
		}
	}
	missing = admitted
	ss = c.site(site)
	if evict > 0 {
		var old []wantedEntry
		for h, w := range ss.wanted {
			if _, keep := missing[h]; !keep {
				old = append(old, wantedEntry{h, w.seq})
			}
		}
		slices.SortFunc(old, func(a, b wantedEntry) int { return cmp.Compare(a.seq, b.seq) })
		for _, e := range old[:evict] {
			delete(ss.wanted, e.h)
		}
		c.wantedTotal -= evict
	}
	c.seq++
	for h, size := range missing {
		if _, ok := ss.wanted[h]; !ok {
			c.wantedTotal++
		}
		ss.wanted[h] = wantedChunk{size: size, exp: now.Add(planTTL), seq: c.seq}
	}
	return admitted, nil
}

type wantedEntry struct {
	h   [32]byte
	seq uint64
}

// put stages a hash-verified chunk. Uploads (commitCopy=false) must match an
// outstanding plan and fit the per-site caps; every write fits maxTotalBytes.
// The file is written under mu: chunks are at most 256 KiB, and it keeps
// accounting and disk in lockstep. Already staged: no-op.
func (c *chunkStage) put(site string, h [32]byte, data []byte, commitCopy bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ss := c.sites[site]
	if ss == nil {
		if !commitCopy {
			return errNotPlanned // no state created for unplanned uploads
		}
		ss = c.site(site)
	}
	defer c.dropIfIdle(site)
	if _, ok := ss.staged[h]; ok {
		if _, stale := ss.wanted[h]; stale {
			delete(ss.wanted, h)
			c.wantedTotal--
		}
		return nil
	}
	ch := charge(len(data))
	if !commitCopy {
		w, ok := ss.wanted[h]
		if !ok || w.size != len(data) || time.Now().After(w.exp) {
			return errNotPlanned
		}
		if ss.bytes+ch > c.maxSiteBytes || len(ss.staged)+1 > c.maxSiteChunks {
			return fmt.Errorf("%w for this site (over %d MiB or %d chunks)", errStageFull, c.maxSiteBytes>>20, c.maxSiteChunks)
		}
	}
	if c.total+ch > c.maxTotalBytes {
		return fmt.Errorf("%w on the server, retry later", errStageFull)
	}
	if err := writeAtomic(c.siteDir(site), c.path(site, h), data); err != nil {
		return err
	}
	if _, ok := ss.wanted[h]; ok {
		delete(ss.wanted, h)
		c.wantedTotal--
	}
	ss.staged[h] = stagedChunk{charge: ch, at: time.Now()}
	ss.bytes += ch
	c.total += ch
	return nil
}

func writeAtomic(dir, dst string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".up-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), dst)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// read returns a staged chunk, verified; a corrupted one is dropped and
// reported as missing.
func (c *chunkStage) read(site string, h [32]byte) ([]byte, error) {
	b, err := os.ReadFile(c.path(site, h))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errChunkMissing
	}
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(b) != h {
		c.release(site, map[[32]byte]bool{h: true})
		return nil, errChunkMissing
	}
	return b, nil
}

// used is the bytes charged to site.
func (c *chunkStage) used(site string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ss := c.sites[site]; ss != nil {
		return ss.bytes
	}
	return 0
}

// release deletes staged chunks and gives their capacity back.
func (c *chunkStage) release(site string, hashes map[[32]byte]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ss := c.sites[site]
	if ss == nil {
		return
	}
	for h := range hashes {
		if sc, ok := ss.staged[h]; ok {
			os.Remove(c.path(site, h))
			delete(ss.staged, h)
			ss.bytes -= sc.charge
			c.total -= sc.charge
		}
	}
	c.dropIfIdle(site)
}

// removeSite drops everything staged or planned for a deleted site.
func (c *chunkStage) removeSite(site string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ss := c.sites[site]; ss != nil {
		c.total -= ss.bytes
		c.wantedTotal -= len(ss.wanted)
		ss.staged, ss.wanted, ss.bytes = map[[32]byte]stagedChunk{}, map[[32]byte]wantedChunk{}, 0
	}
	os.RemoveAll(c.siteDir(site))
	c.dropIfIdle(site)
}

// pin keeps the sweeper away from site until the returned func is called.
// Pins are kept apart from site states: a commit in progress is not a pending
// plan and gives no admission to one.
func (c *chunkStage) pin(site string) func() {
	c.mu.Lock()
	c.pins[site]++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		if c.pins[site]--; c.pins[site] <= 0 {
			delete(c.pins, site)
		}
		c.dropIfIdle(site)
		c.mu.Unlock()
	}
}

// sweep drops staged chunks and plans older than maxAge on unpinned sites.
func (c *chunkStage) sweep(maxAge time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-maxAge)
	for name, ss := range c.sites {
		if c.pins[name] > 0 {
			continue
		}
		for h, sc := range ss.staged {
			if !sc.at.After(cutoff) {
				os.Remove(c.path(name, h))
				delete(ss.staged, h)
				ss.bytes -= sc.charge
				c.total -= sc.charge
			}
		}
		c.pruneWanted(ss, now)
		c.dropIfIdle(name)
	}
}

func (c *chunkStage) sweepLoop() {
	for {
		c.sweep(stageTTL)
		time.Sleep(10 * time.Minute)
	}
}
