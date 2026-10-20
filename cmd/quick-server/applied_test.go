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
