package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPublicAddr(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"100.64.0.1", false},      // CGNAT
		{"100.127.255.255", false},
		{"224.0.0.1", false},
		{"0.0.0.0", false},
		{"::1", false},
		{"fc00::1", false},
		{"fd12::1", false},
		{"fe80::1", false},
		{"::", false},
		{"::ffff:10.0.0.1", false}, // mapped IPv4 must be unwrapped
		{"1.1.1.1", true},
		{"8.8.8.8", true},
		{"100.63.255.255", true}, // just below CGNAT
		{"100.128.0.0", true},    // just above CGNAT
		{"2606:4700::1111", true},
	}
	for _, tt := range tests {
		if got := publicAddr(netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("publicAddr(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

// allowLoopback lets the SSRF dialer reach the httptest upstream, restoring
// the real vetting afterwards (same spirit as tests lowering maxExtract*).
func allowLoopback(t *testing.T) {
	t.Helper()
	old := proxyDialAllow
	proxyDialAllow = func(ip netip.Addr) bool { return ip.IsLoopback() || old(ip) }
	t.Cleanup(func() { proxyDialAllow = old })
}

// trustCert makes the proxy transport accept the httptest server's cert.
func trustCert(t *testing.T, s *server, ts *httptest.Server) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	s.siteProxy.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
}

func TestProxyEndToEnd(t *testing.T) {
	allowLoopback(t)

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend/redirect":
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		case "/backend/boom":
			http.Error(w, "kaboom", http.StatusInternalServerError)
		default:
			body, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "method=%s path=%s query=%s host=%s hop=%s cookie=%q body=%s",
				r.Method, r.URL.Path, r.URL.RawQuery, r.Host, r.Header.Get("X-Quick-Proxy"), r.Header.Get("Cookie"), body)
		}
	}))
	t.Cleanup(upstream.Close)

	s := newTestServer(t)
	trustCert(t, s, upstream)
	putSite(t, s.store, "demo", map[string]string{
		"_redirects": "/api/* " + upstream.URL + "/backend/:splat 200",
	})

	t.Run("get with query, hop header, cookie strip", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/users/7?page=2", nil)
		r.AddCookie(&http.Cookie{Name: "quick_access_demo", Value: "secret"})
		r.AddCookie(&http.Cookie{Name: "_oauth2_proxy", Value: "session"})
		r.AddCookie(&http.Cookie{Name: "_oauth2_proxy_0", Value: "shard"})
		r.AddCookie(&http.Cookie{Name: "other", Value: "1"})
		w := httptest.NewRecorder()
		s.serveSite(w, r, "demo")

		if w.Code != http.StatusOK {
			t.Fatalf("code %d body %q", w.Code, w.Body.String())
		}
		body := w.Body.String()
		wantHost := strings.TrimPrefix(upstream.URL, "https://")
		for _, want := range []string{
			"method=GET", "path=/backend/users/7", "query=page=2",
			"host=" + wantHost, "hop=1", `cookie="other=1"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("upstream saw %q, missing %q", body, want)
			}
		}
	})

	t.Run("post body forwarded", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/things", strings.NewReader(`{"a":1}`))
		w := httptest.NewRecorder()
		s.serveSite(w, r, "demo")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `body={"a":1}`) {
			t.Errorf("code %d body %q", w.Code, w.Body.String())
		}
	})

	t.Run("upstream redirect relayed, not followed", func(t *testing.T) {
		w := get(t, s, "demo", "/api/redirect")
		if w.Code != http.StatusFound || w.Header().Get("Location") != "https://elsewhere.example/" {
			t.Errorf("code %d location %q", w.Code, w.Header().Get("Location"))
		}
	})

	t.Run("upstream error relayed", func(t *testing.T) {
		w := get(t, s, "demo", "/api/boom")
		if w.Code != http.StatusInternalServerError {
			t.Errorf("code %d, want 500", w.Code)
		}
	})

	t.Run("inbound hop header refused", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/users/7", nil)
		r.Header.Set("X-Quick-Proxy", "1")
		w := httptest.NewRecorder()
		s.serveSite(w, r, "demo")
		if w.Code != http.StatusBadGateway {
			t.Errorf("code %d, want 502 loop guard", w.Code)
		}
	})
}

func TestProxyRefusesBaseDomain(t *testing.T) {
	s := newTestServer(t)
	s.baseDomain = "quick.example"
	for _, target := range []string{"https://quick.example/x", "https://other.quick.example/x", "https://OTHER.QUICK.example/x"} {
		putSite(t, s.store, "demo", map[string]string{"_redirects": "/api/* " + target + " 200"})
		s.rules.forget("demo")
		if w := get(t, s, "demo", "/api/hit"); w.Code != http.StatusBadGateway {
			t.Errorf("target %s: code %d, want 502", target, w.Code)
		}
	}
	// A cousin domain is not refused by this guard (it proceeds to dial).
	putSite(t, s.store, "demo", map[string]string{"_redirects": "/api/* https://notquick.example/x 200"})
	s.rules.forget("demo")
	if w := get(t, s, "demo", "/api/hit"); w.Body.String() == "proxy target not allowed\n" {
		t.Error("cousin domain wrongly refused by base-domain guard")
	}
}

func TestProxyBlocksPrivateUpstream(t *testing.T) {
	// No allowLoopback here: the real vetting must refuse the local upstream.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("SSRF guard let a loopback dial through")
	}))
	t.Cleanup(upstream.Close)

	s := newTestServer(t)
	trustCert(t, s, upstream)
	putSite(t, s.store, "demo", map[string]string{"_redirects": "/api/* " + upstream.URL + "/:splat 200"})

	w := get(t, s, "demo", "/api/x")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "upstream unreachable") {
		t.Errorf("code %d body %q, want 502 upstream unreachable", w.Code, w.Body.String())
	}
}

func TestProxyDialFailure(t *testing.T) {
	allowLoopback(t)
	s := newTestServer(t)
	putSite(t, s.store, "demo", map[string]string{"_redirects": "/api/* https://127.0.0.1:1/:splat 200"})

	w := get(t, s, "demo", "/api/x")
	if w.Code != http.StatusBadGateway {
		t.Errorf("code %d, want 502", w.Code)
	}
}
