// Delta deploy (client side): the folder is described as a manifest of
// content-defined chunks; only the chunks the server lacks are uploaded.
// Protocol in internal/quick.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/quick"
)

// errNoDelta: the server has no delta endpoints (older server): use the tar.
var errNoDelta = errors.New("server does not support delta deploy")

// errDeltaBusy: the server can't take a delta deploy right now (staging caps
// full): use the tar.
var errDeltaBusy = errors.New("server busy with other deploys")

// errReplan: the server refused chunks not covered by an outstanding plan.
var errReplan = errors.New("chunk upload failed (409): the server asks to plan again")

// chunkBatch is the raw chunk bytes per upload request, well under the
// server's per-request cap.
const chunkBatch = 32 << 20

// commitAttempts bounds plan/upload/commit rounds: the server answers 409
// when chunks promised by the plan vanished (a concurrent deploy).
const commitAttempts = 3

// localChunk is where a chunk's bytes are in the folder.
type localChunk struct {
	path string
	off  int64
	size int
}

type deltaStats struct {
	total int64  // folder size
	sent  int64  // raw bytes of the uploaded chunks
	lease string // the server's deploy lease, for the tar fallback
}

// lockedError is a 423 from the server: another deploy of the site is in
// progress, or this one lost its lease. Never retried.
type lockedError struct{ quick.DeployLocked }

func (e *lockedError) Error() string {
	if e.DeployLocked.Error == quick.LeaseLost {
		lost := "this deploy lost the site"
		if e.Holder != "" {
			lost += " to " + e.Holder
		}
		return lost + " (taken over with --takeover, site deleted, or idle too long): nothing was published, run it again"
	}
	who := e.Holder
	if who == "" {
		who = "someone else"
	}
	since := ""
	if t, err := time.Parse(time.RFC3339, e.Since); err == nil {
		t, now := t.Local(), time.Now()
		layout := "15:04"
		if t.YearDay() != now.YearDay() || t.Year() != now.Year() {
			layout = "Jan 2 15:04"
		}
		since = " (since " + t.Format(layout) + ")"
	}
	return who + " is publishing this site" + since + "; retry in a moment or use --takeover"
}

// lockedResponse turns a 423 into a lockedError; nil for any other status.
func lockedResponse(code int, body []byte) error {
	if code != http.StatusLocked {
		return nil
	}
	var l quick.DeployLocked
	if json.Unmarshal(body, &l) != nil || l.Error == "" {
		return fmt.Errorf("deploy refused (423): %s", strings.TrimSpace(string(body)))
	}
	return &lockedError{l}
}

// leaseRequest sets what every deploy request carries: the lease of the deploy
// it belongs to (if any) and a JSON preference for 423 answers.
func leaseRequest(req *http.Request, lease string) {
	req.Header.Set("Accept", "application/json")
	if lease != "" {
		req.Header.Set(quick.DeployLeaseHeader, lease)
	}
}

// buildManifest chunks exactly the files selected by the plan (same ignore
// rules as the tar upload).
func buildManifest(dir string, p *plan) (quick.DeployManifest, map[string]localChunk, error) {
	m := quick.DeployManifest{Files: make([]quick.ManifestFile, 0, len(p.files))}
	locs := map[string]localChunk{}
	for _, pf := range p.files {
		path := filepath.Join(dir, filepath.FromSlash(pf.rel))
		f, err := os.Open(path)
		if err != nil {
			return m, nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return m, nil, err
		}
		idx, err := chunk.Index(f)
		f.Close()
		if err != nil {
			return m, nil, err
		}
		mf := quick.ManifestFile{
			Path: pf.rel, Mode: uint32(fi.Mode().Perm()), Size: idx.Size,
			SHA256: hex.EncodeToString(idx.Hash[:]), Chunks: make([]quick.ManifestChunk, 0, len(idx.Chunks)),
		}
		var off int64
		for _, c := range idx.Chunks {
			h := hex.EncodeToString(c.Hash[:])
			mf.Chunks = append(mf.Chunks, quick.ManifestChunk{Hash: h, Size: c.Size})
			if _, ok := locs[h]; !ok {
				locs[h] = localChunk{path: path, off: off, size: c.Size}
			}
			off += int64(c.Size)
		}
		m.Files = append(m.Files, mf)
	}
	return m, locs, nil
}

// deltaDeploy publishes the folder sending only the chunks the server lacks.
// errNoDelta if the server doesn't have the endpoints. The first plan opens
// the site's deploy lease (takeover breaks another deploy's); every later
// request, re-plans included, carries it.
// While it holds the lease it is recorded in held: given back (before
// returning) if the deploy fails or is interrupted (lease.go), kept for the
// tar fallback (busy server).
func deltaDeploy(cfg *cliConfig, name, tok, dir string, p *plan, takeover bool) (quick.DeployResponse, deltaStats, error) {
	res, st, err := deltaRounds(cfg, name, tok, dir, p, takeover)
	switch {
	case err == nil:
		held.clear() // the commit released it
	case errors.Is(err, errDeltaBusy):
		if st.lease == "" {
			held.clear() // the refused first plan opened nothing
		}
		// else the tar fallback carries it
	default:
		held.release()
	}
	return res, st, err
}

func deltaRounds(cfg *cliConfig, name, tok, dir string, p *plan, takeover bool) (quick.DeployResponse, deltaStats, error) {
	var res quick.DeployResponse
	m, locs, err := buildManifest(dir, p)
	if err != nil {
		return res, deltaStats{}, err
	}
	body, err := gzipJSON(m)
	if err != nil {
		return res, deltaStats{}, err
	}
	st := deltaStats{total: p.totalSize}
	q := "?name=" + url.QueryEscape(name)
	for attempt := 1; ; attempt++ {
		if err := intr.stopped(); err != nil {
			return res, st, err
		}
		pq := q
		ctx := intr.context()
		// Until a plan succeeds there is no lease: a retried first plan is still
		// a first plan (takeover included); later ones send the lease back.
		first := st.lease == ""
		if first {
			if takeover {
				pq += "&takeover=1"
			}
			// The server may open the lease before we learn its id: this
			// answer is still awaited for a while after an interrupt.
			ctx = intr.planContext()
		}
		code, out, err := deltaPost(ctx, cfg, tok, "/api/deploy/plan"+pq, st.lease, body, true)
		if err != nil {
			if intr.stopped() != nil {
				return res, st, errInterrupted
			}
			return res, st, err
		}
		if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) && first {
			return res, st, errNoDelta
		}
		if code == http.StatusServiceUnavailable {
			return res, st, errDeltaBusy
		}
		if err := lockedResponse(code, out); err != nil {
			return res, st, err
		}
		if code == http.StatusConflict && attempt < commitAttempts {
			continue // the site changed while planning: plan again
		}
		if code != http.StatusOK {
			return res, st, fmt.Errorf("deploy failed (%d): %s", code, strings.TrimSpace(string(out)))
		}
		var pl quick.DeployPlanResponse
		if err := json.Unmarshal(out, &pl); err != nil {
			return res, st, err
		}
		st.lease = pl.Lease
		held.set(cfg, tok, name, st.lease)
		if err := intr.stopped(); err != nil {
			return res, st, err // deltaDeploy releases it
		}
		if first {
			fmt.Printf("  sending %s of %s\n", humanSize(pl.MissingBytes), humanSize(pl.TotalBytes))
		}
		n, err := uploadChunks(cfg, tok, q, st.lease, pl.Missing, locs)
		st.sent += n
		if errors.Is(err, errReplan) && attempt < commitAttempts {
			continue // the server forgot the plan (expired, superseded): plan again
		}
		if err != nil {
			return res, st, err
		}
		if err := intr.stopped(); err != nil {
			return res, st, err
		}
		code, out, err = deltaPost(intr.context(), cfg, tok, "/api/deploy/commit"+q, st.lease, body, true)
		if err != nil {
			return res, st, err // interrupted: errPublishStarted once sent (send)
		}
		if err := lockedResponse(code, out); err != nil {
			return res, st, err
		}
		if code == http.StatusConflict && attempt < commitAttempts {
			continue // something changed on the server meanwhile: plan again
		}
		if code != http.StatusOK {
			return res, st, fmt.Errorf("deploy failed (%d): %s", code, strings.TrimSpace(string(out)))
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return res, st, err // pub stays sent: may have published
		}
		pub.set(published)
		return res, st, nil
	}
}

// uploadChunks sends the missing chunks in gzip batches of chunk records.
func uploadChunks(cfg *cliConfig, tok, q, lease string, missing []string, locs map[string]localChunk) (int64, error) {
	var sent int64
	var buf bytes.Buffer
	var gz *gzip.Writer
	var raw int
	flush := func() error {
		if gz == nil {
			return nil
		}
		if err := gz.Close(); err != nil {
			return err
		}
		if err := intr.stopped(); err != nil {
			return err
		}
		code, out, err := deltaPost(intr.context(), cfg, tok, "/api/deploy/chunks"+q, lease, buf.Bytes(), false)
		if err != nil {
			if intr.stopped() != nil {
				return errInterrupted
			}
			// The server reads only part of a refused upload and may close
			// the connection before we read its 423: ask with an empty batch.
			if pc, pout, perr := deltaPost(intr.context(), cfg, tok, "/api/deploy/chunks"+q, lease, emptyGzip(), false); perr == nil {
				if lerr := lockedResponse(pc, pout); lerr != nil {
					return lerr
				}
			}
			return err
		}
		if code == http.StatusConflict {
			return errReplan
		}
		if err := lockedResponse(code, out); err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("chunk upload failed (%d): %s", code, strings.TrimSpace(string(out)))
		}
		sent += int64(raw)
		buf.Reset()
		gz, raw = nil, 0
		return nil
	}
	// One file open at a time: missing chunks come in manifest order, so
	// consecutive ones mostly share a file.
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	data := make([]byte, chunk.MaxSize)
	for _, h := range missing {
		loc, ok := locs[h]
		if !ok {
			return sent, fmt.Errorf("server asked for unknown chunk %s", h)
		}
		if f == nil || f.Name() != loc.path {
			if f != nil {
				f.Close()
			}
			var err error
			if f, err = os.Open(loc.path); err != nil {
				f = nil
				return sent, err
			}
		}
		b := data[:loc.size]
		if _, err := f.ReadAt(b, loc.off); err != nil {
			return sent, err
		}
		if gz == nil {
			// Builds (Unity .data/.wasm) are often already compressed: favor speed.
			gz, _ = gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		}
		hb, _ := hex.DecodeString(h)
		gz.Write(hb)
		binary.Write(gz, binary.BigEndian, uint32(loc.size))
		gz.Write(b)
		if raw += loc.size; raw >= chunkBatch {
			if err := flush(); err != nil {
				return sent, err
			}
		}
	}
	return sent, flush()
}

// emptyGzip is a chunk batch with no records.
func emptyGzip() []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Close()
	return buf.Bytes()
}

func gzipJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(v); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// deltaPost POSTs a gzip body and returns status and response body.
func deltaPost(ctx context.Context, cfg *cliConfig, tok, path, lease string, body []byte, manifest bool) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Server+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	leaseRequest(req, lease)
	if manifest {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
	} else {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return send(req, strings.HasPrefix(path, "/api/deploy/commit"))
}

// send runs a deploy request and reads the whole answer. A request that may
// publish (commit, tar upload) interrupted after it was written has an
// uncertain outcome, errPublishStarted, unless the server's answer was a
// refusal (4xx publishes nothing, even if its body is cut short); the
// caller decodes a complete answer. Interrupted before that, or any other
// request: errInterrupted. For a publish request it also keeps the run's
// outcome (pub): sent once written, refused on a 4xx; the caller marks a
// decoded success as published.
func send(req *http.Request, publish bool) (int, []byte, error) {
	var wrote atomic.Bool
	if publish {
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			WroteHeaders: func() { wrote.Store(true); pub.set(sent) },
		}))
	}
	interrupted := func(err error) error {
		switch {
		case intr.stopped() == nil:
			return err
		case publish && wrote.Load():
			return errPublishStarted
		}
		return errInterrupted
	}
	resp, err := deployClient.Do(req)
	if err != nil {
		return 0, nil, interrupted(err)
	}
	defer resp.Body.Close()
	refusal := resp.StatusCode >= 400 && resp.StatusCode < 500
	if publish && refusal {
		pub.set(refused) // the status alone says nothing was applied
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		if refusal {
			return resp.StatusCode, out, nil
		}
		return resp.StatusCode, out, interrupted(err)
	}
	return resp.StatusCode, out, nil
}
