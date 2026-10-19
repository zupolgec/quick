// Delta deploy (client side): the folder is described as a manifest of
// content-defined chunks; only the chunks the server lacks are uploaded.
// Protocol in internal/quick.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zupolgec/quick/internal/chunk"
	"github.com/zupolgec/quick/internal/quick"
)

// errNoDelta: the server has no delta endpoints (older server): use the tar.
var errNoDelta = errors.New("server does not support delta deploy")

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
	total int64 // folder size
	sent  int64 // raw bytes of the uploaded chunks
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
// errNoDelta if the server doesn't have the endpoints.
func deltaDeploy(cfg *cliConfig, name, tok, dir string, p *plan) (quick.DeployResponse, deltaStats, error) {
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
		code, out, err := deltaPost(cfg, tok, "/api/deploy/plan"+q, body, true)
		if err != nil {
			return res, st, err
		}
		if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) && attempt == 1 {
			return res, st, errNoDelta
		}
		if code != http.StatusOK {
			return res, st, fmt.Errorf("deploy failed (%d): %s", code, strings.TrimSpace(string(out)))
		}
		var pl quick.DeployPlanResponse
		if err := json.Unmarshal(out, &pl); err != nil {
			return res, st, err
		}
		n, err := uploadChunks(cfg, tok, q, pl.Missing, locs)
		st.sent += n
		if err != nil {
			return res, st, err
		}
		code, out, err = deltaPost(cfg, tok, "/api/deploy/commit"+q, body, true)
		if err != nil {
			return res, st, err
		}
		if code == http.StatusConflict && attempt < commitAttempts {
			continue // something changed on the server meanwhile: plan again
		}
		if code != http.StatusOK {
			return res, st, fmt.Errorf("deploy failed (%d): %s", code, strings.TrimSpace(string(out)))
		}
		return res, st, json.Unmarshal(out, &res)
	}
}

// uploadChunks sends the missing chunks in gzip batches of chunk records.
func uploadChunks(cfg *cliConfig, tok, q string, missing []string, locs map[string]localChunk) (int64, error) {
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
		code, out, err := deltaPost(cfg, tok, "/api/deploy/chunks"+q, buf.Bytes(), false)
		if err != nil {
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
	files := map[string]*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	data := make([]byte, chunk.MaxSize)
	for _, h := range missing {
		loc, ok := locs[h]
		if !ok {
			return sent, fmt.Errorf("server asked for unknown chunk %s", h)
		}
		f := files[loc.path]
		if f == nil {
			var err error
			if f, err = os.Open(loc.path); err != nil {
				return sent, err
			}
			files[loc.path] = f
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
func deltaPost(cfg *cliConfig, tok, path string, body []byte, manifest bool) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, cfg.Server+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if manifest {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
	} else {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := deployClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}
