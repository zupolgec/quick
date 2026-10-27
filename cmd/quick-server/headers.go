package main

import (
	"net/http"
	"path"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// _headers (Netlify-style) per-site response headers: a path line (exact, or
// ending in "/*") followed by indented "Name: value" lines. Every matching rule
// applies, in file order; a header set by several rules is joined with ", ".
// Headers apply to responses served from the site's files (files, rewrites,
// 200.html, 404.html), never to redirects or proxied responses.

// forbiddenHeaders are owned by quick: content negotiation, caching
// validators, auth/SSO, cookies, transport security. A site can't set them.
var forbiddenHeaders = map[string]bool{
	"Accept-Ranges":                    true,
	"Access-Control-Allow-Credentials": true, // would let another origin read a protected site with the visitor's session
	"Alt-Svc":                          true,
	"Authorization":                    true,
	"Clear-Site-Data":                  true,
	"Connection":                       true,
	"Content-Encoding":                 true,
	"Content-Length":                   true,
	"Content-Range":                    true,
	"Content-Type":                     true,
	"Date":                             true,
	"Etag":                             true,
	"Keep-Alive":                       true,
	"Last-Modified":                    true,
	"Location":                         true,
	"Server":                           true,
	"Set-Cookie":                       true,
	"Strict-Transport-Security":        true,
	"Te":                               true,
	"Trailer":                          true,
	"Transfer-Encoding":                true,
	"Upgrade":                          true,
	"Www-Authenticate":                 true,
	"X-Content-Type-Options":           true,
}

// forbiddenHeaderPrefixes cover quick's own headers, proxy plumbing and
// proxy auth (Proxy-Authenticate, Proxy-Authorization, ...).
var forbiddenHeaderPrefixes = []string{"X-Quick-", "X-Forwarded-", "Proxy-"}

func headerAllowed(name string) bool {
	if forbiddenHeaders[name] {
		return false
	}
	for _, p := range forbiddenHeaderPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

type headerField struct {
	name, value string
}

type headerRule struct {
	from     string // cleaned path; for wildcard rules the prefix without the trailing "/*"
	wildcard bool
	headers  []headerField
}

// parseHeaders parses a _headers file. Like parseRedirects, invalid lines are
// skipped and counted, never fatal; a bad path line also drops the header
// lines under it. Only full-line comments: values may contain "#".
func parseHeaders(data []byte) (rules []headerRule, skipped int) {
	var cur *headerRule // nil before the first path or after an invalid one
	flush := func() {
		if cur != nil && len(cur.headers) > 0 && len(rules) < maxRedirectRules {
			rules = append(rules, *cur)
		}
		cur = nil
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "/") {
			flush()
			if r, ok := parseHeaderPath(line); ok {
				cur = &r
			} else {
				skipped++
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || cur == nil || value == "" || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			skipped++
			continue
		}
		name = http.CanonicalHeaderKey(name)
		if !headerAllowed(name) {
			skipped++
			continue
		}
		cur.headers = append(cur.headers, headerField{name, value})
	}
	flush()
	return rules, skipped
}

// parseHeaderPath accepts the same paths as a _redirects source: absolute,
// optionally ending in "/*", nothing else.
func parseHeaderPath(p string) (headerRule, bool) {
	var r headerRule
	if strings.HasSuffix(p, "/*") {
		r.wildcard = true
		p = strings.TrimSuffix(p, "/*")
	}
	if strings.ContainsAny(p, "*: \t") {
		return headerRule{}, false
	}
	if p != "" {
		p = path.Clean(p)
	}
	r.from = p
	return r, true
}

func (r headerRule) matches(p string) bool {
	if p == r.from && r.from != "" {
		return true
	}
	return r.wildcard && (p == r.from+"/" || strings.HasPrefix(p, r.from+"/"))
}

// applyHeaderRules adds the site's _headers for request path p. Add, not Set:
// quick's defaults (X-Robots-Tag on quick addresses) stay, the site's
// directives come on top.
func applyHeaderRules(h http.Header, rules []headerRule, p string) {
	var order []string
	vals := map[string][]string{}
	for _, r := range rules {
		if !r.matches(p) {
			continue
		}
		for _, f := range r.headers {
			if _, ok := vals[f.name]; !ok {
				order = append(order, f.name)
			}
			vals[f.name] = append(vals[f.name], f.value)
		}
	}
	for _, name := range order {
		h.Add(name, strings.Join(vals[name], ", "))
	}
}
