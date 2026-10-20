// Custom domains: a site can be served on its own hostname (www.example.com
// CNAME <site>.<base>). A domain is claimed by one site and served only once
// its DNS points to that site ("active"); TLS certificates are issued on demand
// by the proxy, which asks /api/domains/check before each one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
	"golang.org/x/net/publicsuffix"
)

const maxSiteDomains = 10

type siteDomain struct {
	Name       string `json:"name"`
	AddedBy    string `json:"added_by,omitempty"`
	AddedAt    string `json:"added_at,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"` // empty = DNS not pointing here yet
}

type resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type domainClaim struct {
	site   string
	active bool
}

// domainIndex maps every claimed domain to its site, so serving doesn't scan
// metadata. Rebuilt from metadata at startup, kept in sync on every change.
type domainIndex struct {
	mu sync.RWMutex
	m  map[string]domainClaim
}

func newDomainIndex() *domainIndex { return &domainIndex{m: map[string]domainClaim{}} }

func loadDomainIndex(st storage.Backend, meta *metaStore) (*domainIndex, error) {
	idx := newDomainIndex()
	sites, err := st.ListSites()
	if err != nil {
		return nil, err
	}
	for _, site := range sites {
		p, err := meta.load(site)
		if err != nil {
			return nil, err
		}
		for _, d := range p.Domains {
			idx.set(d.Name, site, d.VerifiedAt != "")
		}
	}
	return idx, nil
}

func (x *domainIndex) get(d string) (domainClaim, bool) {
	if x == nil {
		return domainClaim{}, false
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	c, ok := x.m[d]
	return c, ok
}

func (x *domainIndex) set(d, site string, active bool) {
	x.mu.Lock()
	x.m[d] = domainClaim{site, active}
	x.mu.Unlock()
}

func (x *domainIndex) remove(d string) {
	x.mu.Lock()
	delete(x.m, d)
	x.mu.Unlock()
}

func (x *domainIndex) dropSite(site string) {
	x.mu.Lock()
	for d, c := range x.m {
		if c.site == site {
			delete(x.m, d)
		}
	}
	x.mu.Unlock()
}

// normalizeDomain lowercases and validates a hostname a user wants to attach:
// plain ASCII labels (punycode for IDNs), at least a registrable domain, never
// an IP, a public suffix or a name under the base domain.
func normalizeDomain(d, base string) (string, error) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	bad := errors.New("invalid domain name")
	if d == "" || len(d) > 253 || net.ParseIP(d) != nil {
		return "", bad
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", bad
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", bad
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", bad
			}
		}
	}
	if d == base || strings.HasSuffix(d, "."+base) {
		return "", errors.New("already served under " + base)
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(d); err != nil {
		return "", bad
	}
	return d, nil
}

func isApexDomain(d string) bool {
	e, err := publicsuffix.EffectiveTLDPlusOne(d)
	return err == nil && e == d
}

// verifyDNS reports whether d points to the site: a CNAME to <site>.<base>,
// or, for apex domains that can't CNAME, the same addresses.
func (s *server) verifyDNS(ctx context.Context, d, site string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	target := site + "." + s.baseDomain
	if s.cnameIs(ctx, d, target) {
		return true
	}
	if !isApexDomain(d) {
		return false
	}
	want, err := s.resolver.LookupIPAddr(ctx, target)
	if err != nil {
		return false
	}
	got, err := s.resolver.LookupIPAddr(ctx, d)
	if err != nil || len(got) == 0 {
		return false
	}
	for _, g := range got {
		if !slices.ContainsFunc(want, func(w net.IPAddr) bool { return w.IP.Equal(g.IP) }) {
			return false
		}
	}
	return true
}

func (s *server) cnameIs(ctx context.Context, host, target string) bool {
	c, err := s.resolver.LookupCNAME(ctx, host)
	return err == nil && strings.TrimSuffix(strings.ToLower(c), ".") == target
}

func (s *server) domainRecords(d, site string) []quick.DNSRecord {
	target := site + "." + s.baseDomain
	first := quick.DNSRecord{Type: "CNAME", Name: d, Value: target}
	if isApexDomain(d) {
		// A CNAME isn't allowed on an apex: ALIAS/flattening, or the same address.
		first = quick.DNSRecord{Type: "A", Name: d, Value: "same address as " + target + " (or ALIAS/CNAME flattening)"}
	}
	return []quick.DNSRecord{first}
}

func (s *server) domainInfos(site string, ds []siteDomain) []quick.DomainInfo {
	out := []quick.DomainInfo{}
	for _, d := range ds {
		out = append(out, quick.DomainInfo{Domain: d.Name, Active: d.VerifiedAt != "", Records: s.domainRecords(d.Name, site)})
	}
	return out
}

// customDomainSite resolves a request on a custom domain to its site. It
// responds itself (and returns "") when the host is unknown or when the site
// is SSO-protected: the SSO session lives on the base domain, so visitors go
// to the site's regular address.
func (s *server) customDomainSite(w http.ResponseWriter, r *http.Request, host string) string {
	c, ok := s.domains.get(host)
	// Plain HTTP: the proxy's catch-all hands quick every host no other
	// service claims, so this replaces the proxy's own "503 for anything
	// else". Registered domains move to HTTPS (where their certificate lives).
	if r.Header.Get("X-Forwarded-Proto") == "http" {
		if ok && c.active {
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return ""
		}
		http.Error(w, "no site here", http.StatusServiceUnavailable)
		return ""
	}
	if !ok || !c.active {
		renderNotFound(w, r, host, false)
		return ""
	}
	p, err := s.meta.load(c.site)
	if err != nil {
		http.Error(w, "site temporarily unavailable", http.StatusServiceUnavailable)
		return ""
	}
	if siteIsSSO(p) {
		http.Redirect(w, r, "https://"+c.site+"."+s.baseDomain+r.URL.RequestURI(), http.StatusFound)
		return ""
	}
	return c.site
}

// handleDomainCheck is the proxy's on-demand TLS gate: 200 only for a domain
// whose DNS points to the site that claimed it. A pending domain is verified
// (and activated) here, so the first visit after the DNS change just works.
func (s *server) handleDomainCheck(w http.ResponseWriter, r *http.Request) {
	d, err := normalizeDomain(r.URL.Query().Get("domain"), s.baseDomain)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c, ok := s.domains.get(d)
	if ok && !c.active {
		c.active = s.activateDomain(r.Context(), c.site, d)
	}
	if !ok || !c.active {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// activateDomain re-checks a pending domain and saves it as active if its DNS
// now points to the site.
func (s *server) activateDomain(ctx context.Context, site, d string) bool {
	if !s.verifyDNS(ctx, d, site) {
		return false
	}
	s.domainsMu.Lock()
	defer s.domainsMu.Unlock()
	unlock := s.locks.lock(site)
	defer unlock()
	p, err := s.meta.loadFresh(site)
	if err != nil {
		return false
	}
	i := slices.IndexFunc(p.Domains, func(x siteDomain) bool { return x.Name == d })
	if i < 0 {
		return false // removed meanwhile
	}
	if p.Domains[i].VerifiedAt == "" {
		p.Domains[i].VerifiedAt = nowStamp()
		if err := s.meta.save(site, p); err != nil {
			log.Printf("domain %s: saving activation failed: %v", d, err)
			return false
		}
		log.Printf("domain %s active for %q", d, site)
	}
	s.domains.set(d, site, true)
	return true
}

func (s *server) handleDomains(w http.ResponseWriter, r *http.Request, name, rest string) {
	email, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.listDomains(w, r, name)
	case rest == "" && r.Method == http.MethodPost:
		var req quick.DomainRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		s.addDomain(w, r, name, req.Domain, email)
	case rest != "" && r.Method == http.MethodDelete:
		s.removeDomain(w, name, rest, email)
	default:
		http.NotFound(w, r)
	}
}

// listDomains also activates pending domains whose DNS is now in place, so
// `quick domains` reflects reality right after the DNS change.
func (s *server) listDomains(w http.ResponseWriter, r *http.Request, name string) {
	p, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	changed := false
	for _, d := range p.Domains {
		if d.VerifiedAt == "" && s.activateDomain(r.Context(), name, d.Name) {
			changed = true
		}
	}
	if changed {
		if p, err = s.meta.load(name); err != nil {
			http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(quick.DomainsResponse{Site: name, Domains: s.domainInfos(name, p.Domains)})
}

func (s *server) addDomain(w http.ResponseWriter, r *http.Request, name, raw, email string) {
	d, err := normalizeDomain(raw, s.baseDomain)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if exists, err := s.store.SiteExists(name); err != nil || !exists {
		http.Error(w, "site not found", http.StatusNotFound)
		return
	}
	pointsHere := s.verifyDNS(r.Context(), d, name)

	s.domainsMu.Lock()
	defer s.domainsMu.Unlock()
	if c, ok := s.domains.get(d); ok && c.site != name {
		// DNS decides: a domain that now points here can move from another site.
		if !pointsHere {
			http.Error(w, "domain already used by site "+c.site, http.StatusConflict)
			return
		}
		if err := s.dropDomainFromSite(c.site, d); err != nil {
			http.Error(w, "could not update site "+c.site+": "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		log.Printf("domain %s moved from %q to %q by %s", d, c.site, name, email)
	}

	unlock := s.locks.lock(name)
	defer unlock()
	cur, err := s.meta.loadFresh(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(cur, email, actPolicy); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	i := slices.IndexFunc(cur.Domains, func(x siteDomain) bool { return x.Name == d })
	if i < 0 {
		if len(cur.Domains) >= maxSiteDomains {
			http.Error(w, "too many domains for this site", http.StatusBadRequest)
			return
		}
		cur.Domains = append(cur.Domains, siteDomain{Name: d, AddedBy: email, AddedAt: nowStamp()})
		i = len(cur.Domains) - 1
	}
	if pointsHere && cur.Domains[i].VerifiedAt == "" {
		cur.Domains[i].VerifiedAt = nowStamp()
	}
	if err := s.meta.save(name, cur); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.domains.set(d, name, cur.Domains[i].VerifiedAt != "")
	log.Printf("domain %s added to %q by %s (active=%v)", d, name, email, pointsHere)
	_ = json.NewEncoder(w).Encode(quick.DomainsResponse{Site: name, Domains: s.domainInfos(name, cur.Domains)})
}

// dropDomainFromSite removes d from site's metadata. Caller holds domainsMu.
func (s *server) dropDomainFromSite(site, d string) error {
	unlock := s.locks.lock(site)
	defer unlock()
	p, err := s.meta.loadFresh(site)
	if err != nil {
		return err
	}
	p.Domains = slices.DeleteFunc(p.Domains, func(x siteDomain) bool { return x.Name == d })
	if err := s.meta.save(site, p); err != nil {
		return err
	}
	s.domains.remove(d)
	return nil
}

func (s *server) removeDomain(w http.ResponseWriter, name, raw, email string) {
	d := strings.TrimSuffix(strings.ToLower(strings.Trim(raw, "/ ")), ".")
	s.domainsMu.Lock()
	defer s.domainsMu.Unlock()
	unlock := s.locks.lock(name)
	defer unlock()
	cur, err := s.meta.loadFresh(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(cur, email, actPolicy); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	n := len(cur.Domains)
	cur.Domains = slices.DeleteFunc(cur.Domains, func(x siteDomain) bool { return x.Name == d })
	if len(cur.Domains) == n {
		http.Error(w, "domain not attached to this site", http.StatusNotFound)
		return
	}
	if err := s.meta.save(name, cur); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.domains.remove(d)
	log.Printf("domain %s removed from %q by %s", d, name, email)
	_ = json.NewEncoder(w).Encode(quick.DomainsResponse{Site: name, Domains: s.domainInfos(name, cur.Domains)})
}
