package main

import (
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/storage"
)

// failMetaOnce fails the next PutMeta of one site: before writing, or after
// the write landed (a lost S3 response).
type failMetaOnce struct {
	storage.Backend
	mu      sync.Mutex
	site    string
	applied bool // the write lands, then the error
}

func (f *failMetaOnce) PutMeta(site string, data []byte) error {
	f.mu.Lock()
	fail := f.site != "" && f.site == site
	if fail {
		f.site = ""
	}
	applied := f.applied
	f.mu.Unlock()
	if !fail {
		return f.Backend.PutMeta(site, data)
	}
	if applied {
		if err := f.Backend.PutMeta(site, data); err != nil {
			return err
		}
	}
	return errors.New("connection reset")
}

// A domain move whose source update fails ends with exactly one site durably
// claiming the domain, and the index pointing to it.
func TestDomainMoveSourceWriteFails(t *testing.T) {
	for _, applied := range []bool{false, true} {
		s, dns := newDomainTestServer(t)
		fm := &failMetaOnce{Backend: s.store}
		s.store = fm
		s.meta = newMetaStore(fm, []byte("secret"), time.Minute)
		dns.point("www.example.com", "other")
		if code, _ := addDomain(t, s, "other", "www.example.com"); code != http.StatusOK {
			t.Fatalf("claim on other: %d", code)
		}
		dns.point("www.example.com", "app")
		fm.mu.Lock()
		fm.site, fm.applied = "other", applied
		fm.mu.Unlock()
		code, _ := addDomain(t, s, "app", "www.example.com")

		// What's durable, as after a restart.
		claims := func(site string) bool {
			p, err := newMetaStore(fm.Backend, []byte("secret"), 0).loadFresh(site)
			if err != nil {
				t.Fatal(err)
			}
			return slices.ContainsFunc(p.Domains, func(d siteDomain) bool { return d.Name == "www.example.com" })
		}
		want := "other"
		if applied {
			want = "app"
		}
		inApp, inOther := claims("app"), claims("other")
		if inApp == inOther || (want == "app") != inApp {
			t.Errorf("applied=%v: durable claims app=%v other=%v (code %d), want only %s", applied, inApp, inOther, code, want)
		}
		if c, ok := s.domains.get("www.example.com"); !ok || c.site != want {
			t.Errorf("applied=%v: index %+v %v, want %s", applied, c, ok, want)
		}
		if applied && code != http.StatusOK {
			t.Errorf("applied=%v: the move happened but the answer is %d", applied, code)
		}
		if !applied && code == http.StatusOK {
			t.Errorf("applied=%v: the move failed but the answer is 200", applied)
		}
		// The index rebuilt from metadata agrees.
		idx, err := loadDomainIndex(fm.Backend, newMetaStore(fm.Backend, []byte("secret"), 0))
		if err != nil {
			t.Fatal(err)
		}
		if c, ok := idx.get("www.example.com"); !ok || c.site != want {
			t.Errorf("applied=%v: after restart the domain is on %+v %v, want %s", applied, c, ok, want)
		}
	}
}
