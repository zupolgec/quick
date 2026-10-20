package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

// fakeDNS answers CNAME and A lookups from maps; anything else is NXDOMAIN.
type fakeDNS struct {
	cname map[string]string
	ips   map[string][]string
}

func (f *fakeDNS) LookupCNAME(_ context.Context, host string) (string, error) {
	if c, ok := f.cname[host]; ok {
		return c + ".", nil
	}
	if _, ok := f.ips[host]; ok {
		return host + ".", nil
	}
	return "", errors.New("no such host")
}

func (f *fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if c, ok := f.cname[host]; ok {
		host = c
	}
	var out []net.IPAddr
	for _, s := range f.ips[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	if len(out) == 0 {
		return nil, errors.New("no such host")
	}
	return out, nil
}

func (f *fakeDNS) point(d, site string) { f.cname[d] = site + ".quick.test" }

func newDomainTestServer(t *testing.T) (*server, *fakeDNS) {
	t.Helper()
	s := newAPITestServer(t, true)
	dns := &fakeDNS{cname: map[string]string{}, ips: map[string][]string{
		"app.quick.test":   {"203.0.113.10"},
		"other.quick.test": {"203.0.113.10"},
	}}
	s.resolver = dns
	s.domains = newDomainIndex()
	s.apexMux = s.buildApexMux()
	putSite(t, s.store, "other", map[string]string{"index.html": "<h1>other</h1>"})
	return s, dns
}

func apexCall(s *server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://quick.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.route(w, r)
	return w
}

func hostGet(s *server, host, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	w := httptest.NewRecorder()
	s.route(w, r)
	return w
}

func addDomain(t *testing.T, s *server, site, domain string) (int, quick.DomainsResponse) {
	t.Helper()
	w := apexCall(s, http.MethodPost, "/api/site/"+site+"/domains", `{"domain":"`+domain+`"}`)
	var res quick.DomainsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, res
}

func TestNormalizeDomain(t *testing.T) {
	ok := map[string]string{
		"WWW.Example.com.": "www.example.com",
		" example.co.uk ":  "example.co.uk",
	}
	for in, want := range ok {
		if got, err := normalizeDomain(in, "quick.test"); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "localhost", "co.uk", "1.2.3.4", "foo.quick.test", "quick.test", "a_b.example.com", "-a.example.com", "exa mple.com", "*.example.com"} {
		if got, err := normalizeDomain(bad, "quick.test"); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func TestCustomDomainServesPublicSite(t *testing.T) {
	s, dns := newDomainTestServer(t)
	if err := s.meta.save("app", policy{Access: "public"}); err != nil {
		t.Fatal(err)
	}
	dns.point("www.example.com", "app")

	code, res := addDomain(t, s, "app", "www.example.com")
	if code != http.StatusOK || len(res.Domains) != 1 || !res.Domains[0].Active {
		t.Fatalf("add: code %d res %+v", code, res)
	}
	recs := res.Domains[0].Records
	if len(recs) != 1 || recs[0].Type != "CNAME" || recs[0].Name != "www.example.com" || recs[0].Value != "app.quick.test" {
		t.Errorf("records %+v", recs)
	}
	if w := hostGet(s, "www.example.com", "/"); w.Code != http.StatusOK || w.Body.String() != "<h1>app</h1>" {
		t.Errorf("serve: code %d body %q", w.Code, w.Body)
	}
	if w := apexCall(s, http.MethodGet, "/api/domains/check?domain=www.example.com", ""); w.Code != http.StatusOK {
		t.Errorf("check: code %d", w.Code)
	}
	// The TLS ask comes from the proxy on the container's internal host name.
	r := httptest.NewRequest(http.MethodGet, "http://quick-server-1:8080/api/domains/check?domain=www.example.com", nil)
	w := httptest.NewRecorder()
	s.route(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("check via internal host: code %d", w.Code)
	}
	if w := apexCall(s, http.MethodGet, "/api/domains/check?domain=unknown.example.com", ""); w.Code != http.StatusNotFound {
		t.Errorf("check unknown: code %d", w.Code)
	}
}

// The SSO cookie doesn't cross domains: SSO sites send visitors to their
// regular address.
func TestCustomDomainSSOSiteRedirects(t *testing.T) {
	s, dns := newDomainTestServer(t)
	dns.point("www.example.com", "app")
	addDomain(t, s, "app", "www.example.com")
	w := hostGet(s, "www.example.com", "/page?x=1")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://app.quick.test/page?x=1" {
		t.Errorf("code %d location %q", w.Code, w.Header().Get("Location"))
	}
}

func TestCustomDomainPendingUntilDNSPoints(t *testing.T) {
	s, dns := newDomainTestServer(t)
	s.meta.save("app", policy{Access: "public"})
	code, res := addDomain(t, s, "app", "www.example.com")
	if code != http.StatusOK || len(res.Domains) != 1 || res.Domains[0].Active {
		t.Fatalf("add: code %d res %+v", code, res)
	}
	if w := hostGet(s, "www.example.com", "/"); w.Code != http.StatusNotFound {
		t.Errorf("pending domain served: code %d", w.Code)
	}
	if w := apexCall(s, http.MethodGet, "/api/domains/check?domain=www.example.com", ""); w.Code != http.StatusNotFound {
		t.Errorf("check pending: code %d", w.Code)
	}
	dns.point("www.example.com", "app")
	if w := apexCall(s, http.MethodGet, "/api/domains/check?domain=www.example.com", ""); w.Code != http.StatusOK {
		t.Errorf("check after DNS: code %d", w.Code)
	}
	if w := hostGet(s, "www.example.com", "/"); w.Code != http.StatusOK {
		t.Errorf("serve after DNS: code %d", w.Code)
	}
	p, _ := s.meta.load("app")
	if len(p.Domains) != 1 || p.Domains[0].VerifiedAt == "" {
		t.Errorf("activation not saved: %+v", p.Domains)
	}
}

// Apex domains can't CNAME: matching A records are accepted for them only.
func TestCustomDomainApexByARecord(t *testing.T) {
	s, dns := newDomainTestServer(t)
	dns.ips["example.com"] = []string{"203.0.113.10"}
	dns.ips["shop.example.com"] = []string{"203.0.113.10"}
	if _, res := addDomain(t, s, "app", "example.com"); len(res.Domains) != 1 || !res.Domains[0].Active || res.Domains[0].Records[0].Type != "A" {
		t.Errorf("apex: %+v", res)
	}
	if _, res := addDomain(t, s, "app", "shop.example.com"); len(res.Domains) != 2 || res.Domains[1].Active {
		t.Errorf("subdomain by A record must stay pending: %+v", res)
	}
}

func TestCustomDomainBelongsToOneSite(t *testing.T) {
	s, dns := newDomainTestServer(t)
	dns.point("www.example.com", "app")
	addDomain(t, s, "app", "www.example.com")
	if code, _ := addDomain(t, s, "other", "www.example.com"); code != http.StatusConflict {
		t.Errorf("second site: code %d, want 409", code)
	}
	// DNS is the source of truth: once it points to the other site, it can take it.
	dns.point("www.example.com", "other")
	if code, res := addDomain(t, s, "other", "www.example.com"); code != http.StatusOK || !res.Domains[0].Active {
		t.Errorf("takeover: code %d res %+v", code, res)
	}
	if p, _ := s.meta.load("app"); len(p.Domains) != 0 {
		t.Errorf("old site still lists the domain: %+v", p.Domains)
	}
}

func TestCustomDomainRemoveAndSiteDelete(t *testing.T) {
	s, dns := newDomainTestServer(t)
	s.meta.save("app", policy{Access: "public"})
	dns.point("www.example.com", "app")
	dns.point("blog.example.com", "app")
	addDomain(t, s, "app", "www.example.com")
	addDomain(t, s, "app", "blog.example.com")

	if w := apexCall(s, http.MethodDelete, "/api/site/app/domains/www.example.com", ""); w.Code != http.StatusOK {
		t.Fatalf("remove: code %d %s", w.Code, w.Body)
	}
	if w := hostGet(s, "www.example.com", "/"); w.Code != http.StatusNotFound {
		t.Errorf("removed domain still served: %d", w.Code)
	}
	if w := apexCall(s, http.MethodGet, "/api/site/app/domains", ""); !strings.Contains(w.Body.String(), "blog.example.com") || strings.Contains(w.Body.String(), "www.example.com") {
		t.Errorf("list: %s", w.Body)
	}
	if w := apexCall(s, http.MethodDelete, "/api/site/app", ""); w.Code != http.StatusOK {
		t.Fatalf("delete site: %d", w.Code)
	}
	if w := apexCall(s, http.MethodGet, "/api/domains/check?domain=blog.example.com", ""); w.Code != http.StatusNotFound {
		t.Errorf("deleted site's domain still valid: %d", w.Code)
	}
}

func TestDomainIndexLoadsFromMetadata(t *testing.T) {
	s, _ := newDomainTestServer(t)
	s.meta.save("app", policy{Domains: []siteDomain{{Name: "www.example.com", VerifiedAt: nowStamp()}, {Name: "new.example.com"}}})
	idx, err := loadDomainIndex(s.store, s.meta)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := idx.get("www.example.com"); !ok || c.site != "app" || !c.active {
		t.Errorf("www: %+v %v", c, ok)
	}
	if c, ok := idx.get("new.example.com"); !ok || c.active {
		t.Errorf("new: %+v %v", c, ok)
	}
}

func plainHTTPGet(s *server, host, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	s.route(w, r)
	return w
}

// On plain HTTP the proxy hands quick every host no other service claims (it
// replaces the proxy's catch-all 503): a registered domain goes to HTTPS, any
// other host gets 503 like before.
func TestCustomDomainPlainHTTP(t *testing.T) {
	s, dns := newDomainTestServer(t)
	s.meta.save("app", policy{Access: "public"})
	dns.point("www.example.com", "app")
	addDomain(t, s, "app", "www.example.com")

	if w := plainHTTPGet(s, "www.example.com", "/a?b=1"); w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "https://www.example.com/a?b=1" {
		t.Errorf("registered: %d %q", w.Code, w.Header().Get("Location"))
	}
	for _, host := range []string{"unknown.example.org", "203.0.113.10"} {
		if w := plainHTTPGet(s, host, "/"); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s over http: %d, want 503", host, w.Code)
		}
	}
	// A pending domain isn't served yet: 503 too.
	addDomain(t, s, "app", "pending.example.com")
	if w := plainHTTPGet(s, "pending.example.com", "/"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("pending over http: %d, want 503", w.Code)
	}
}
