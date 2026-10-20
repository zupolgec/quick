package main

// Revocation races on custom domains of people sites: a request paused in a
// DNS lookup while the owner removes the caller.

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

// hookDNS runs hook once, on the first lookup, before answering from fakeDNS.
type hookDNS struct {
	*fakeDNS
	hook  func()
	fired atomic.Bool
}

func (h *hookDNS) fire() {
	if h.hook != nil && h.fired.CompareAndSwap(false, true) {
		h.hook()
	}
}

func (h *hookDNS) LookupCNAME(ctx context.Context, host string) (string, error) {
	h.fire()
	return h.fakeDNS.LookupCNAME(ctx, host)
}

func (h *hookDNS) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	h.fire()
	return h.fakeDNS.LookupIPAddr(ctx, host)
}

func newHookDNS(e *peopleEnv) *hookDNS {
	d := &hookDNS{fakeDNS: &fakeDNS{cname: map[string]string{}, ips: map[string][]string{
		"app.quick.test": {"203.0.113.10"}, "open.quick.test": {"203.0.113.10"},
	}}}
	e.s.resolver = d
	return d
}

// A domain move whose DNS check overlaps the caller's removal leaves the
// domain where it was.
func TestPeopleDomainMoveRevokedDuringDNS(t *testing.T) {
	e := newPeopleEnv(t)
	dns := newHookDNS(e)
	e.setPolicy(t, "app", peoplePolicy(bob))
	e.setPolicy(t, "open", policy{CreatedBy: ann, Domains: []siteDomain{{Name: "www.example.com", VerifiedAt: "2026-01-01T00:00:00Z"}}})
	e.s.domains.set("www.example.com", "open", true)
	dns.point("www.example.com", "app")
	dns.hook = func() { e.revoke(t, bob) }

	w := e.api(t, bob, http.MethodPost, "/api/site/app/domains", quick.DomainRequest{Domain: "www.example.com"})
	if w.Code != http.StatusNotFound {
		t.Errorf("move by a revoked viewer: %d %s", w.Code, w.Body)
	}
	if p := e.policyOf(t, "open"); len(p.Domains) != 1 {
		t.Errorf("the original site lost its domain: %+v", p.Domains)
	}
	if c, ok := e.s.domains.get("www.example.com"); !ok || c.site != "open" {
		t.Errorf("domain index: %+v %v", c, ok)
	}
}

// Listing domains activates pending ones (DNS lookups); a removal meanwhile
// must not let the answer show the site's newer domains.
func TestPeopleDomainListRevokedDuringActivation(t *testing.T) {
	e := newPeopleEnv(t)
	dns := newHookDNS(e)
	p := peoplePolicy(bob)
	p.Domains = []siteDomain{{Name: "pend.example.com"}}
	e.setPolicy(t, "app", p)
	e.s.domains.set("pend.example.com", "app", false)
	dns.point("pend.example.com", "app")
	dns.hook = func() {
		e.revoke(t, bob)
		cur := e.policyOf(t, "app")
		cur.Domains = append(cur.Domains, siteDomain{Name: "late.example.com"})
		e.setPolicy(t, "app", cur)
	}
	w := e.api(t, bob, http.MethodGet, "/api/site/app/domains", nil)
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "late.example.com") {
		t.Errorf("list by a revoked viewer: %d %s", w.Code, w.Body)
	}
}

// A conflict with a people site names it only to those who can open it.
func TestPeopleDomainConflictDoesNotNameHiddenSite(t *testing.T) {
	e := newPeopleEnv(t)
	newHookDNS(e)
	p := peoplePolicy(bob)
	p.Domains = []siteDomain{{Name: "taken.example.com"}}
	e.setPolicy(t, "app", p)
	e.s.domains.set("taken.example.com", "app", false)

	w := e.api(t, carl, http.MethodPost, "/api/site/open/domains", quick.DomainRequest{Domain: "taken.example.com"})
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "app") {
		t.Errorf("non-viewer conflict: %d %q", w.Code, w.Body)
	}
	w = e.api(t, bob, http.MethodPost, "/api/site/open/domains", quick.DomainRequest{Domain: "taken.example.com"})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "site app") {
		t.Errorf("viewer conflict: %d %q", w.Code, w.Body)
	}
}
