package main

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

// fakeDeltaServer speaks the delta protocol: plan asks for every chunk it
// hasn't received; the first conflicts answers are 409.
type fakeDeltaServer struct {
	mu              sync.Mutex
	have            map[string]bool
	plans, commits  int
	commitConflicts int          // commits answered 409 (dropping received chunks, as if they vanished)
	uploadConflicts int          // chunk uploads answered 409 (plan forgotten)
	planConflicts   map[int]bool // plan numbers (1-based) answered 409 (site changed while planning)
	planLeases      []string     // lease header of each plan
	planTakeovers   []bool       // ?takeover=1 on each plan
}

func (f *fakeDeltaServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	switch r.URL.Path {
	case "/api/deploy/plan":
		f.plans++
		f.planLeases = append(f.planLeases, r.Header.Get(quick.DeployLeaseHeader))
		f.planTakeovers = append(f.planTakeovers, r.URL.Query().Get("takeover") == "1")
		var m quick.DeployManifest
		json.NewDecoder(gz).Decode(&m)
		if f.planConflicts[f.plans] {
			http.Error(w, "the site changed while planning: plan again", http.StatusConflict)
			return
		}
		res := quick.DeployPlanResponse{Missing: []string{}, Lease: "L1"}
		for _, fl := range m.Files {
			for _, c := range fl.Chunks {
				if !f.have[c.Hash] {
					res.Missing = append(res.Missing, c.Hash)
				}
			}
		}
		json.NewEncoder(w).Encode(res)
	case "/api/deploy/chunks":
		if f.uploadConflicts > 0 {
			f.uploadConflicts--
			http.Error(w, "plan again", http.StatusConflict)
			return
		}
		hdr := make([]byte, quick.ChunkRecordHeader)
		for {
			if _, err := io.ReadFull(gz, hdr); err != nil {
				break
			}
			io.CopyN(io.Discard, gz, int64(binary.BigEndian.Uint32(hdr[32:])))
			f.have[hex.EncodeToString(hdr[:32])] = true
		}
		json.NewEncoder(w).Encode(quick.ChunkUploadResponse{})
	case "/api/deploy/commit":
		f.commits++
		if f.commitConflicts > 0 {
			f.commitConflicts--
			f.have = map[string]bool{}
			http.Error(w, "plan again", http.StatusConflict)
			return
		}
		json.NewEncoder(w).Encode(quick.DeployResponse{Site: "demo", URL: "https://demo.example"})
	default:
		http.NotFound(w, r)
	}
}

func deltaFixture(t *testing.T, files int) (string, *plan) {
	t.Helper()
	dir := t.TempDir()
	for i := range files {
		p := filepath.Join(dir, fmt.Sprintf("f%04d.txt", i))
		if err := os.WriteFile(p, []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := buildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func runFake(t *testing.T, f *fakeDeltaServer, dir string, p *plan) (quick.DeployResponse, deltaStats, error) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	return deltaDeploy(&cliConfig{Server: srv.URL}, "demo", "tok", dir, p, false)
}

// A 409 on commit (chunks vanished on the server) means plan again: the CLI
// re-plans, re-uploads what is missing and commits.
func TestDeltaDeployRetriesAfterCommitConflict(t *testing.T) {
	dir, p := deltaFixture(t, 3)
	f := &fakeDeltaServer{have: map[string]bool{}, commitConflicts: 1}
	res, st, err := runFake(t, f, dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if f.plans != 2 || f.commits != 2 || res.URL == "" {
		t.Fatalf("plans %d commits %d res %+v", f.plans, f.commits, res)
	}
	if st.sent != 2*p.totalSize {
		t.Fatalf("sent %d, want everything twice (%d)", st.sent, 2*p.totalSize)
	}
}

// A 409 on chunk upload (the server forgot the plan) also means plan again.
func TestDeltaDeployRetriesAfterUploadConflict(t *testing.T) {
	dir, p := deltaFixture(t, 3)
	f := &fakeDeltaServer{have: map[string]bool{}, uploadConflicts: 1}
	if _, _, err := runFake(t, f, dir, p); err != nil {
		t.Fatal(err)
	}
	if f.plans != 2 || f.commits != 1 {
		t.Fatalf("plans %d commits %d, want 2 and 1", f.plans, f.commits)
	}
}

// A 409 on plan (the site changed while planning) means plan again, within
// the same bounded attempts: a first plan opened nothing and simply retries;
// a re-plan sends the deploy's lease back, never a takeover.
func TestDeltaDeployRetriesAfterPlanConflict(t *testing.T) {
	dir, p := deltaFixture(t, 3)
	for _, c := range []struct {
		name       string
		f          *fakeDeltaServer
		wantLeases []string
	}{
		{"first plan", &fakeDeltaServer{have: map[string]bool{}, planConflicts: map[int]bool{1: true}}, []string{"", ""}},
		{"re-plan", &fakeDeltaServer{have: map[string]bool{}, commitConflicts: 1, planConflicts: map[int]bool{2: true}}, []string{"", "L1", "L1"}},
	} {
		srv := httptest.NewServer(c.f)
		res, _, err := deltaDeploy(&cliConfig{Server: srv.URL}, "demo", "tok", dir, p, true)
		srv.Close()
		if err != nil || res.URL == "" {
			t.Fatalf("%s: err %v res %+v", c.name, err, res)
		}
		if !slices.Equal(c.f.planLeases, c.wantLeases) {
			t.Errorf("%s: plan leases %q, want %q", c.name, c.f.planLeases, c.wantLeases)
		}
		for i, tk := range c.f.planTakeovers {
			if tk != (i == 0 || c.wantLeases[i] == "") {
				t.Errorf("%s: plan %d takeover=%v", c.name, i+1, tk)
			}
		}
	}
	// Still bounded.
	f := &fakeDeltaServer{have: map[string]bool{}, planConflicts: map[int]bool{1: true, 2: true, 3: true, 4: true}}
	if _, _, err := runFake(t, f, dir, p); err == nil || !strings.Contains(err.Error(), "409") || f.plans != commitAttempts {
		t.Fatalf("endless plan conflicts: err %v after %d plans", err, f.plans)
	}
}

// The retry is bounded: a server that keeps answering 409 ends in an error.
func TestDeltaDeployGivesUpAfterRepeatedConflicts(t *testing.T) {
	dir, p := deltaFixture(t, 1)
	f := &fakeDeltaServer{have: map[string]bool{}, commitConflicts: 100}
	if _, _, err := runFake(t, f, dir, p); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("err = %v, want a 409 failure", err)
	}
	if f.commits != commitAttempts {
		t.Fatalf("commits %d, want %d", f.commits, commitAttempts)
	}
}

// A busy server (503 on plan: staging caps full) is not an error: the CLI
// falls back to the whole-folder upload. 413 stays an error (too big).
func TestDeltaDeployPlanStatusHandling(t *testing.T) {
	dir, p := deltaFixture(t, 1)
	for code, wantFallback := range map[int]bool{http.StatusServiceUnavailable: true, http.StatusRequestEntityTooLarge: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", code)
		}))
		_, _, err := deltaDeploy(&cliConfig{Server: srv.URL}, "demo", "tok", dir, p, false)
		srv.Close()
		if errors.Is(err, errDeltaBusy) != wantFallback || err == nil {
			t.Errorf("plan %d: err = %v, fallback want %v", code, err, wantFallback)
		}
		if !wantFallback && !strings.Contains(err.Error(), "413") {
			t.Errorf("plan 413: unclear error %v", err)
		}
	}
}

// Uploading chunks of thousands of files must not keep one descriptor per
// file open.
func TestDeltaUploadManyFilesWithLowFDLimit(t *testing.T) {
	dir, p := deltaFixture(t, 2500)
	f := &fakeDeltaServer{have: map[string]bool{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	withFDLimit(t, 512, func() {
		if _, _, err := deltaDeploy(&cliConfig{Server: srv.URL}, "demo", "tok", dir, p, false); err != nil {
			t.Fatal(err)
		}
	})
	if len(f.have) != 2500 {
		t.Fatalf("server got %d chunks, want 2500", len(f.have))
	}
}
