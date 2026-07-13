package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Reverse proxy for _redirects rules with status 200 and an https target
// ("/api/* https://api.example.com/:splat 200"): lets a static frontend call a
// non-CORS API same-origin. Pure pass-through: no credential injection, the
// upstream host is fixed in the rule, never taken from the request.

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicAddr reports whether ip is a global, publicly routable address. The
// proxy only ever dials these: quick-server shares a Docker network with
// oauth2-proxy and every other service on the box, and any authenticated user
// can deploy rules, so private/link-local/metadata ranges are off limits.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap() // judge ::ffff:10.0.0.1 as 10.0.0.1
	return ip.IsValid() &&
		!ip.IsLoopback() &&
		!ip.IsPrivate() && // RFC 1918 + IPv6 ULA fc00::/7
		!ip.IsLinkLocalUnicast() && // 169.254/16 incl. cloud metadata, fe80::/10
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() &&
		!ip.IsUnspecified() &&
		!cgnat.Contains(ip)
}

// proxyDialAllow is the vetting hook; tests override it to admit loopback so
// an httptest upstream can play the external API.
var proxyDialAllow = publicAddr

// newProxyTransport returns the transport for rule proxying. DialContext
// resolves the hostname itself, vets the addresses and dials the vetted IP
// directly, so a DNS rebind between check and dial can't reach a private
// address. Only the dial is overridden: TLS still handshakes and verifies
// against the URL hostname, so the pinned IP must present a valid cert for it.
func newProxyTransport() *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		// Proxy deliberately nil: an HTTPS_PROXY env would bypass the vetting.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if proxyDialAllow(ip) {
					return d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
				}
			}
			return nil, fmt.Errorf("proxy target %q resolves to no publicly routable address", host)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, // headers only: bodies may stream forever (SSE)
		ForceAttemptHTTP2:     true,
	}
}

type proxyTargetKey struct{}

func (s *server) setupSiteProxy() {
	s.siteProxy = &httputil.ReverseProxy{
		Transport:     newProxyTransport(),
		FlushInterval: -1, // flush every write so streaming responses stream
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := pr.In.Context().Value(proxyTargetKey{}).(*url.URL)
			pr.Out.URL = target
			pr.Out.Host = "" // Host header = the target's host, not the site's
			pr.Out.Header.Set("X-Quick-Proxy", "1")
			stripQuickCookies(pr.Out)
			// No SetXForwarded: Rewrite already strips inbound X-Forwarded-*,
			// and the upstream has no business learning the visitor's IP.
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("site proxy %s %s: %v", r.Method, r.URL, err)
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		},
	}
}

// stripQuickCookies removes quick's own auth cookies (site access codes and
// oauth2-proxy sessions, including its _0/_1 shards) so they never reach an
// upstream. Everything else forwards untouched.
func stripQuickCookies(out *http.Request) {
	cookies := out.Cookies()
	out.Header.Del("Cookie")
	for _, c := range cookies {
		if strings.HasPrefix(c.Name, "quick_access_") || strings.HasPrefix(c.Name, "_oauth2_proxy") {
			continue
		}
		out.AddCookie(c)
	}
}

// serveRuleProxy proxies the request to target. Two loop guards: targets under
// the base domain are refused outright, and the X-Quick-Proxy hop header
// catches indirect loops (e.g. an external domain pointing back at quick).
func (s *server) serveRuleProxy(w http.ResponseWriter, r *http.Request, target *url.URL) {
	if r.Header.Get("X-Quick-Proxy") != "" {
		http.Error(w, "proxy loop detected", http.StatusBadGateway)
		return
	}
	host := strings.ToLower(target.Hostname())
	if base := strings.ToLower(s.baseDomain); base != "" && (host == base || strings.HasSuffix(host, "."+base)) {
		http.Error(w, "proxy target not allowed", http.StatusBadGateway)
		return
	}
	s.siteProxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), proxyTargetKey{}, target)))
}
