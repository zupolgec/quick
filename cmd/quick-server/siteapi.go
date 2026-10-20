// Client API: same-origin endpoints under /__quick/api/ on every site, plus the
// browser library at /__quick/quick.js. Calls go through the same access gate
// as the site's files, so an SSO site's API is SSO-protected too.
package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed client/quick.js
var clientJS string

//go:embed client/quick.d.ts
var clientDTS string

func (s *server) handleClientLib(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/__quick/quick.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(clientJS))
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(clientDTS))
	}
}

// sameOriginCall rejects calls that don't come from the site's own pages.
// Every site shares the SSO cookie (same site, so SameSite=lax doesn't help):
// the custom header forces a CORS preflight that is never granted, and
// Sec-Fetch-Site, when the browser sends it, must say same-origin.
func sameOriginCall(r *http.Request) bool {
	if r.Header.Get("X-Quick") != "1" {
		return false
	}
	sfs := r.Header.Get("Sec-Fetch-Site")
	return sfs == "" || sfs == "same-origin"
}

func (s *server) handleClientAPI(w http.ResponseWriter, r *http.Request, sub string) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOriginCall(r) {
		apiError(w, http.StatusForbidden, "cross-origin call refused")
		return
	}
	p, email, signedIn, ok, err := s.siteAccess(r, sub)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "site temporarily unavailable")
		return
	}
	if !ok && signedIn {
		apiError(w, http.StatusForbidden, "this site is shared only with some people")
		return
	}
	if !ok {
		apiError(w, http.StatusUnauthorized, "sign in to use this site")
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/__quick/api/") {
	case "me":
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		// Only SSO sites know who the visitor is: public and code sites stay
		// anonymous even for a signed-in employee.
		var user any
		if siteIsSSO(p) && email != "" {
			user = map[string]string{"email": email}
		}
		apiJSON(w, http.StatusOK, map[string]any{"user": user})
	default:
		apiError(w, http.StatusNotFound, "unknown endpoint")
	}
}

func siteIsSSO(p policy) bool { return p.Access != "public" && p.Access != "code" }

func apiJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	apiJSON(w, status, map[string]string{"error": msg})
}
