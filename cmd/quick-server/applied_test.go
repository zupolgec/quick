package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

// metaFailsAfterPublish publishes fine, then can't save metadata.
type metaFailsAfterPublish struct {
	storage.Backend
	published atomic.Bool
}

func (m *metaFailsAfterPublish) PutSite(site string, tr *tar.Reader) error {
	err := m.Backend.PutSite(site, tr)
	if err == nil {
		m.published.Store(true)
	}
	return err
}

func (m *metaFailsAfterPublish) PutMeta(site string, data []byte) error {
	if m.published.Load() {
		return errors.New("disk full")
	}
	return m.Backend.PutMeta(site, data)
}

// The site is live but its metadata couldn't be saved: the answer must say
// published (with a warning), never a plain failure, or the CLI would claim
// nothing was published. Delta commit and tar upload.
func TestPublishedButMetadataNotSavedIsASuccessWithWarning(t *testing.T) {
	for _, tarUpload := range []bool{false, true} {
		s := newDeltaTestServer(t)
		s.store = &metaFailsAfterPublish{Backend: s.store}
		s.meta = newMetaStore(s.store, []byte("secret"), 0)
		c := &deltaClient{t: t, s: s, site: "demo"}
		files := map[string][]byte{"index.html": []byte("live")}
		var code int
		var body string
		if tarUpload {
			code, body = tarDeployCode(c, files)
		} else {
			m, data := manifestOf(t, files)
			_, pl := c.plan(m)
			c.upload(data, pl.Missing)
			w := c.commit(m)
			code, body = w.Code, w.Body.String()
		}
		var res quick.DeployResponse
		if code != http.StatusOK || json.Unmarshal([]byte(body), &res) != nil || res.Warning == "" {
			t.Fatalf("tar %v: %d %q, want 200 with a warning", tarUpload, code, body)
		}
		if got := string(readSite(t, s, "demo", "index.html")); got != "live" {
			t.Fatalf("tar %v: live %q", tarUpload, got)
		}
		(&deltaClient{t: t, s: s, site: "demo"}).leased() // the lease ended with the deploy
	}
}

// The creator is recorded before the first publish: if saving the metadata
// afterwards fails, the live site still has its owner (in shared/owned mode an
// ownerless site could be taken by anyone).
func TestFirstDeployRecordsTheOwnerBeforePublishing(t *testing.T) {
	for _, tarUpload := range []bool{false, true} {
		s := newDeltaTestServer(t)
		s.store = &metaFailsAfterPublish{Backend: s.store}
		s.meta = newMetaStore(s.store, []byte("secret"), 0)
		c := &deltaClient{t: t, s: s, site: "demo"}
		files := map[string][]byte{"index.html": []byte("live")}
		if tarUpload {
			tarDeployCode(c, files)
		} else {
			m, data := manifestOf(t, files)
			_, pl := c.plan(m)
			c.upload(data, pl.Missing)
			c.commit(m)
		}
		p, err := s.meta.loadFresh("demo")
		if err != nil || p.CreatedBy == "" {
			t.Fatalf("tar %v: created_by %q (%v), want the deployer", tarUpload, p.CreatedBy, err)
		}
	}
}

// putSiteFails refuses every publish.
type putSiteFails struct{ storage.Backend }

func (putSiteFails) PutSite(string, *tar.Reader) error { return errors.New("disk full") }

// A first deploy that fails leaves no trace: no metadata, no site listed. A
// failed deploy of an existing site keeps its owner.
func TestFailedFirstDeployLeavesNoMetadata(t *testing.T) {
	for _, tarUpload := range []bool{false, true} {
		s := newDeltaTestServer(t)
		s.store = putSiteFails{s.store}
		s.meta = newMetaStore(s.store, []byte("secret"), 0)
		c := &deltaClient{t: t, s: s, site: "demo"}
		files := map[string][]byte{"index.html": []byte("x")}
		var code int
		if tarUpload {
			code, _ = tarDeployCode(c, files)
		} else {
			m, data := manifestOf(t, files)
			_, pl := c.plan(m)
			c.upload(data, pl.Missing)
			code = c.commit(m).Code
		}
		if code == http.StatusOK {
			t.Fatalf("tar %v: publish failure answered 200", tarUpload)
		}
		if b, ok, _ := s.store.GetMeta("demo"); ok {
			t.Fatalf("tar %v: failed first deploy left metadata behind (code %d): %s", tarUpload, code, b)
		}
		names, _ := s.store.ListSites()
		for _, n := range names {
			if n == "demo" {
				t.Fatalf("tar %v: failed first deploy is listed", tarUpload)
			}
		}
	}
}

// Settings saved before the first deploy survive its failure, without a creator.
func TestFailedFirstDeployKeepsEarlierSettings(t *testing.T) {
	s := newDeltaTestServer(t)
	s.store = putSiteFails{s.store}
	s.meta = newMetaStore(s.store, []byte("secret"), 0)
	if err := s.meta.save("demo", policy{Access: "public"}); err != nil {
		t.Fatal(err)
	}
	c := &deltaClient{t: t, s: s, site: "demo"}
	if code, _ := tarDeployCode(c, map[string][]byte{"index.html": []byte("x")}); code == http.StatusOK {
		t.Fatal("publish failure answered 200")
	}
	p, err := s.meta.loadFresh("demo")
	if err != nil || p.Access != "public" || p.CreatedBy != "" {
		t.Fatalf("policy after failed first deploy: %+v (%v)", p, err)
	}
}
