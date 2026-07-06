package main

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
)

// MIME types Go doesn't know by default: with nosniff on, a wrong type means
// "downloaded" instead of served correctly.
func init() {
	mime.AddExtensionType(".wasm", "application/wasm")
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// handleSite is the catch-all: resolves the site from the host, applies policy
// (public / code / SSO) and, if access is granted, serves the file.
func (s *server) handleSite(w http.ResponseWriter, r *http.Request) {
	host := fwdHost(r)
	sub := subOf(host, s.baseDomain)
	if sub == "" {
		http.NotFound(w, r)
		return
	}
	p, err := s.meta.load(sub)
	if err != nil {
		http.Error(w, "site temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	switch p.Access {
	case "public":
		s.serveSite(w, r, sub)
	case "code":
		if c, err := r.Cookie("quick_access_" + sub); err == nil && s.meta.validAccessCookie(sub, c.Value) {
			s.serveSite(w, r, sub)
			return
		}
		s.redirect(w, r, host, "/__quick/code")
	default: // SSO: if not authenticated, show the sign-in page (no bare redirect)
		if _, ok := s.checkSSO(r); !ok {
			s.renderSSOPage(w, r, host)
			return
		}
		s.serveSite(w, r, sub)
	}
}

// serveSite resolves a path to a file with static-host conventions:
//
//	/path       -> exact file
//	/about      -> /about.html        (clean URLs, no extension)
//	/about/     -> /about/index.html  (directory index)
//
// HTML pages use Cloudflare-like auto trailing slash canonical URLs: flat HTML
// files are served without a slash, directory indexes with a slash. A path that
// matches nothing does NOT fall back to the home page: if 200.html exists it acts
// as an SPA app shell (200), otherwise it's a 404 (with the nearest 404.html if
// present).
func (s *server) serveSite(w http.ResponseWriter, r *http.Request, sub string) {
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	if target, ok := s.htmlRedirectTarget(sub, p); ok {
		http.Redirect(w, r, withRawQuery(target, r.URL.RawQuery), http.StatusTemporaryRedirect)
		return
	}
	if cand, ok := s.htmlAssetCandidate(sub, p); ok {
		s.serveFile(w, r, sub, cand, http.StatusOK)
		return
	}
	// SPA opt-in: 200.html acts as the app shell for non-file routes.
	if s.serveFile(w, r, sub, "/200.html", http.StatusOK) {
		return
	}
	if !s.serveNearest404(w, r, sub, p) {
		w.WriteHeader(http.StatusNotFound)
	}
}

func withRawQuery(p, rawQuery string) string {
	if rawQuery == "" {
		return p
	}
	return p + "?" + rawQuery
}

func (s *server) htmlRedirectTarget(sub, p string) (string, bool) {
	if strings.HasSuffix(p, "/index.html") {
		return s.indexRedirectTarget(sub, strings.TrimSuffix(p, "/index.html"))
	}
	if strings.HasSuffix(p, "/index") {
		return s.indexRedirectTarget(sub, strings.TrimSuffix(p, "/index"))
	}
	if strings.HasSuffix(p, ".html") {
		target := strings.TrimSuffix(p, ".html")
		if target == "" {
			target = "/"
		}
		return target, s.htmlCanonicalReachable(sub, target)
	}
	if strings.HasSuffix(p, "/") {
		if s.siteFileExists(sub, indexPath(p)) {
			return "", false
		}
		target := strings.TrimRight(p, "/")
		if target == "" {
			return "", false
		}
		return target, s.siteFileExists(sub, target+".html")
	}
	if path.Ext(p) == "" && !s.siteFileExists(sub, p) && !s.siteFileExists(sub, p+".html") && s.siteFileExists(sub, indexPath(p)) {
		return p + "/", true
	}
	return "", false
}

func (s *server) indexRedirectTarget(sub, target string) (string, bool) {
	if target == "" {
		target = "/"
	}
	return target, s.htmlCanonicalReachable(sub, target)
}

func (s *server) htmlCanonicalReachable(sub, p string) bool {
	if s.siteFileExists(sub, p) {
		return true
	}
	if path.Ext(p) != "" {
		return false
	}
	return s.siteFileExists(sub, p+".html") || s.siteFileExists(sub, indexPath(p))
}

func (s *server) htmlAssetCandidate(sub, p string) (string, bool) {
	if s.siteFileExists(sub, p) {
		return p, true
	}
	if path.Ext(p) == "" && !strings.HasSuffix(p, "/") && s.siteFileExists(sub, p+".html") {
		return p + ".html", true
	}
	if s.siteFileExists(sub, indexPath(p)) {
		return indexPath(p), true
	}
	return "", false
}

func indexPath(p string) string {
	if p == "/" {
		return "/index.html"
	}
	return strings.TrimRight(p, "/") + "/index.html"
}

func (s *server) siteFileExists(sub, cand string) bool {
	rc, _, err := s.store.OpenFile(sub, cand)
	if err != nil {
		return false
	}
	rc.Close()
	return true
}

func (s *server) serveNearest404(w http.ResponseWriter, r *http.Request, sub, p string) bool {
	for _, cand := range nearest404Candidates(p) {
		if s.serveFile(w, r, sub, cand, http.StatusNotFound) {
			return true
		}
	}
	return false
}

func nearest404Candidates(p string) []string {
	start := p
	if strings.HasSuffix(start, "/") {
		start = strings.TrimRight(start, "/")
	}
	dir := path.Dir(start)
	if strings.HasSuffix(p, "/") && start != "" {
		dir = start
	}
	if dir == "." {
		dir = "/"
	}

	var cands []string
	for {
		if dir == "/" || dir == "" {
			cands = append(cands, "/404.html")
			break
		}
		cands = append(cands, dir+"/404.html")
		dir = path.Dir(dir)
	}
	return cands
}

// serveFile serves cand if it exists and returns true. With status 200 it uses
// http.ServeContent (range, etag, content-type). With an error status it writes
// that status with the content-type from the extension (ServeContent would force 200).
func (s *server) serveFile(w http.ResponseWriter, r *http.Request, sub, cand string, status int) bool {
	rc, fi, err := s.store.OpenFile(sub, cand)
	if err != nil {
		return false
	}
	defer rc.Close()
	// No MIME sniffing: a public site must not be able to get a file interpreted
	// as the wrong type (XSS vector). The octet-stream fallback downloads, not renders.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if status == http.StatusOK {
		if fi.ETag != "" {
			w.Header().Set("ETag", fi.ETag)
		}
		http.ServeContent(w, r, fi.Name, fi.ModTime, rc)
		return true
	}
	if ct := mime.TypeByExtension(path.Ext(cand)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(status)
	io.Copy(w, rc)
	return true
}
