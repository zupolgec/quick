package main

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zupolgec/quick/internal/storage"
)

// _redirects (Netlify-style) per-site rules: redirects (301/302/307/308),
// local rewrites (200 + local path, e.g. the SPA catch-all `/* /index.html 200`)
// and same-origin proxying (200 + absolute https URL). Rules apply only when no
// static file matches the request (shadowing), first match wins.

// Caps so a hostile _redirects can't blow up memory or the per-request match
// loop. Vars, not consts: tests lower them (same pattern as maxExtract* in
// internal/storage).
var (
	maxRedirectsBytes int64 = 128 << 10
	maxRedirectRules        = 1000
)

type redirectRule struct {
	from     string   // cleaned path; for wildcard rules the prefix without the trailing "/*"
	wildcard bool     // from ended in "/*"; the rest of the path is :splat
	to       string   // local path or absolute URL, may contain ":splat"
	status   int      // 301, 302, 307, 308 (redirect) or 200 (rewrite/proxy)
	proxy    bool     // status 200 with an absolute https URL target
	proxyURL *url.URL // parsed at load time so scheme/host are fixed forever
}

// parseRedirects parses a _redirects file. Invalid lines are skipped, not
// fatal: a typo must not take down the working rules around it. skipped
// reports how many lines were dropped so the caller can log once per load.
func parseRedirects(data []byte) (rules []redirectRule, skipped int) {
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if strings.HasPrefix(f, "#") {
				fields = fields[:i]
				break
			}
		}
		if len(fields) == 0 {
			continue
		}
		r, ok := parseRule(fields)
		if !ok {
			skipped++
			continue
		}
		rules = append(rules, r)
		if len(rules) >= maxRedirectRules {
			break
		}
	}
	return rules, skipped
}

func parseRule(fields []string) (redirectRule, bool) {
	if len(fields) < 2 || len(fields) > 3 {
		return redirectRule{}, false
	}
	from, to := fields[0], fields[1]

	r := redirectRule{status: http.StatusMovedPermanently}
	if len(fields) == 3 {
		st, err := strconv.Atoi(fields[2])
		if err != nil {
			return redirectRule{}, false // includes Netlify's "301!" force syntax
		}
		switch st {
		case 301, 302, 307, 308, 200:
			r.status = st
		default:
			return redirectRule{}, false
		}
	}

	// from: absolute path, optionally ending in "/*". No mid-path wildcards,
	// no :param placeholders (v1).
	if !strings.HasPrefix(from, "/") {
		return redirectRule{}, false
	}
	if strings.HasSuffix(from, "/*") {
		r.wildcard = true
		from = strings.TrimSuffix(from, "/*")
	}
	if strings.ContainsAny(from, "*:") {
		return redirectRule{}, false
	}
	if from != "" {
		from = path.Clean(from)
	}
	r.from = from

	if strings.Contains(to, ":splat") && !r.wildcard {
		return redirectRule{}, false
	}
	r.to = to
	switch {
	case strings.HasPrefix(to, "/"): // local rewrite or local redirect
		return r, true
	case r.status == http.StatusOK: // proxy: https only, host fixed in the rule
		u, err := url.Parse(to)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return redirectRule{}, false
		}
		r.proxy = true
		r.proxyURL = u
		return r, true
	default: // external redirect
		u, err := url.Parse(to)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return redirectRule{}, false
		}
		return r, true
	}
}

type ruleMatch struct {
	rule  redirectRule
	splat string
}

// matchRules returns the first rule matching path p (file order). A wildcard
// prefix matches itself, itself+"/" and anything below it, but never a sibling
// (`/api/*` matches /api and /api/x, not /api-x).
func matchRules(rules []redirectRule, p string) (ruleMatch, bool) {
	for _, r := range rules {
		if p == r.from && r.from != "" {
			return ruleMatch{rule: r}, true
		}
		if !r.wildcard {
			continue
		}
		if p == r.from+"/" {
			return ruleMatch{rule: r}, true
		}
		if rest, ok := strings.CutPrefix(p, r.from+"/"); ok {
			return ruleMatch{rule: r, splat: rest}, true
		}
	}
	return ruleMatch{}, false
}

// targetPath resolves :splat in a string target (redirect location or local
// rewrite path). Local rewrite paths are re-cleaned so a splat can't smuggle
// traversal segments (storage cleanRel blocks them too; defense in depth).
func (m ruleMatch) targetPath() string {
	t := strings.ReplaceAll(m.rule.to, ":splat", m.splat)
	if m.rule.status == http.StatusOK && !m.rule.proxy {
		t = path.Clean("/" + strings.TrimPrefix(t, "/"))
	}
	return t
}

// proxyTarget builds the upstream URL: :splat is substituted in the path only,
// never in scheme or host, so a request can't steer the proxy elsewhere. The
// inbound query is appended after any query hardcoded in the rule.
func (m ruleMatch) proxyTarget(rawQuery string) *url.URL {
	u := *m.rule.proxyURL
	u.Path = strings.ReplaceAll(u.Path, ":splat", m.splat)
	u.RawPath = ""
	u.RawQuery = joinQuery(u.RawQuery, rawQuery)
	return &u
}

// appendQuery is withRawQuery for rule targets, which may already embed a "?".
func appendQuery(loc, rawQuery string) string {
	if rawQuery == "" {
		return loc
	}
	if strings.Contains(loc, "?") {
		return loc + "&" + rawQuery
	}
	return loc + "?" + rawQuery
}

func joinQuery(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "&" + b
}

type cachedRules struct {
	rules []redirectRule
	at    time.Time
}

// rulesStore caches parsed _redirects per site (same shape as metaStore).
// Unlike metaStore it also caches the negative result: most sites have no
// _redirects and every probe is a storage round trip (an S3 GET). Storage
// backends collapse errors into ErrNotFound, so a transient error caches as
// "no rules" for one TTL — fail-open on serving, never fail-closed.
type rulesStore struct {
	be    storage.Backend
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]cachedRules
}

func newRulesStore(be storage.Backend, ttl time.Duration) *rulesStore {
	return &rulesStore{be: be, ttl: ttl, cache: map[string]cachedRules{}}
}

func (rs *rulesStore) load(site string) []redirectRule {
	rs.mu.Lock()
	if c, ok := rs.cache[site]; ok && time.Since(c.at) < rs.ttl {
		rs.mu.Unlock()
		return c.rules
	}
	rs.mu.Unlock()

	var rules []redirectRule
	if rc, _, err := rs.be.OpenFile(site, "/_redirects"); err == nil {
		data, rerr := io.ReadAll(io.LimitReader(rc, maxRedirectsBytes+1))
		rc.Close()
		if rerr == nil && int64(len(data)) <= maxRedirectsBytes {
			var skipped int
			rules, skipped = parseRedirects(data)
			if skipped > 0 {
				log.Printf("site %s: _redirects: %d invalid line(s) skipped", site, skipped)
			}
		} else {
			log.Printf("site %s: _redirects unreadable or over %d bytes, ignored", site, maxRedirectsBytes)
		}
	}
	rs.mu.Lock()
	rs.cache[site] = cachedRules{rules, time.Now()}
	rs.mu.Unlock()
	return rules
}

func (rs *rulesStore) forget(site string) {
	rs.mu.Lock()
	delete(rs.cache, site)
	rs.mu.Unlock()
}
