// Size limits and disk space.
//
// Every site has a size limit: the server default (QUICK_SITE_LIMIT, 100 MB)
// unless an administrator (QUICK_ADMINS) set another one for that site, up to
// QUICK_SITE_LIMIT_MAX (2 GB). The default keeps quick a place for sites, not
// a file store; the per-site raise is for the few that need it (game builds,
// video). A deploy over the limit is refused before anything is published.
//
// On a local disk a deploy is also refused when it would leave less than
// QUICK_MIN_FREE (5 GB) free: the disk is shared with whatever else runs on
// the host.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/zupolgec/quick/internal/quick"
	"github.com/zupolgec/quick/internal/storage"
)

const (
	defaultSiteLimit = 100 << 20
	defaultMaxLimit  = 2 << 30
	defaultMinFree   = 5 << 30
)

// configureLimits reads the limits from the environment. The largest limit
// also becomes the storage's extraction cap.
func (s *server) configureLimits() error {
	for _, v := range []struct {
		env string
		dst *int64
		def int64
	}{
		{"QUICK_SITE_LIMIT", &s.siteLimit, defaultSiteLimit},
		{"QUICK_SITE_LIMIT_MAX", &s.maxSiteLimit, defaultMaxLimit},
		{"QUICK_MIN_FREE", &s.minFree, defaultMinFree},
	} {
		*v.dst = v.def
		if raw := strings.TrimSpace(quick.Env(v.env, "")); raw != "" {
			n, err := quick.ParseSize(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", v.env, err)
			}
			*v.dst = n
		}
	}
	if s.siteLimit > s.maxSiteLimit {
		return fmt.Errorf("QUICK_SITE_LIMIT (%s) is above QUICK_SITE_LIMIT_MAX (%s)", quick.FormatSize(s.siteLimit), quick.FormatSize(s.maxSiteLimit))
	}
	storage.MaxExtractBytes = s.maxSiteLimit
	s.admins = map[string]bool{}
	for _, e := range quick.SplitList(quick.Env("QUICK_ADMINS", "")) {
		s.admins[strings.ToLower(e)] = true
	}
	if sr, ok := s.store.(storage.SpaceReporter); ok {
		s.freeSpace = sr.FreeBytes
	}
	return nil
}

func (s *server) isAdmin(email string) bool { return email != "" && s.admins[strings.ToLower(email)] }

// adminList is the administrators' emails, sorted.
func (s *server) adminList() []string {
	out := make([]string, 0, len(s.admins))
	for e := range s.admins {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// askAdmin names who to ask in a refusal: the administrators, or the system
// administrator when none is configured.
func (s *server) askAdmin() string {
	if a := s.adminList(); len(a) > 0 {
		return "ask an administrator (" + strings.Join(a, ", ") + ")"
	}
	return "ask your system administrator"
}

// defaultLimit and ceiling fall back to the storage cap (servers built in
// tests without configureLimits).
func (s *server) defaultLimit() int64 { return def64(s.siteLimit, storage.MaxExtractBytes) }
func (s *server) ceiling() int64      { return def64(s.maxSiteLimit, storage.MaxExtractBytes) }

func def64(v, d int64) int64 {
	if v > 0 {
		return v
	}
	return d
}

// limitOf is the size limit in effect for a site.
func (s *server) limitOf(p policy) int64 {
	if p.SizeLimit > 0 {
		return min(p.SizeLimit, s.ceiling())
	}
	return s.defaultLimit()
}

// errTooBig: a deploy over its site's limit. ask names who can raise it.
type errTooBig struct {
	size, limit int64
	ask         string
}

func (e errTooBig) Error() string {
	if e.size > 0 {
		return fmt.Sprintf("this deploy is %s, over the site's %s limit: %s to raise it",
			quick.FormatSize(e.size), quick.FormatSize(e.limit), e.ask)
	}
	return fmt.Sprintf("this deploy is over the site's %s limit: %s to raise it", quick.FormatSize(e.limit), e.ask)
}

// checkDeploySize answers 413 if size is over the site's limit, 507 if
// publishing it would leave the disk below the reserve. ok=false: answered.
func (s *server) checkDeploySize(w http.ResponseWriter, p policy, size int64) bool {
	if limit := s.limitOf(p); size > limit {
		http.Error(w, errTooBig{size, limit, s.askAdmin()}.Error(), http.StatusRequestEntityTooLarge)
		return false
	}
	return s.checkSpace(w, size)
}

// checkSpace answers 507 if need more bytes would leave less than minFree on
// the disk. Free space that can't be read doesn't block deploys.
func (s *server) checkSpace(w http.ResponseWriter, need int64) bool {
	if s.freeSpace == nil || s.minFree <= 0 {
		return true
	}
	free, err := s.freeSpace()
	if err != nil {
		log.Printf("WARNING: reading free disk space: %v", err)
		return true
	}
	if free-need < s.minFree {
		log.Printf("deploy refused: %s needed, %s free, %s reserve", quick.FormatSize(need), quick.FormatSize(free), quick.FormatSize(s.minFree))
		http.Error(w, "the server is running out of disk space and can't take this deploy: "+s.askAdmin(), http.StatusInsufficientStorage)
		return false
	}
	return true
}

// handleLimit: GET reads a site's size limit (any signed-in user), POST sets
// it (administrators). The dashboard posts a form with the SSO cookie, the
// CLI JSON with an ID token.
func (s *server) handleLimit(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	browserForm := r.Method == http.MethodPost && isFormPost(r) &&
		!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
	var email string
	var err error
	if browserForm {
		email, err = s.authenticateUser(r)
	} else {
		email, err = s.authenticate(r)
	}
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if browserForm && !s.meta.validCSRF(email, r.FormValue("csrf")) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	cur, err := s.meta.loadFresh(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodPost {
		if !s.isAdmin(email) {
			http.Error(w, "only an administrator can change a site's size limit: "+s.askAdmin(), http.StatusForbidden)
			return
		}
		var req quick.LimitRequest
		t := textsFor(pickLang(r))
		refuse := func(msg, page string) {
			if browserForm {
				s.renderSitePageWith(w, r, name, email, "", http.StatusBadRequest, "", page)
				return
			}
			http.Error(w, msg, http.StatusBadRequest)
		}
		if browserForm {
			if raw := strings.TrimSpace(r.FormValue("size_limit")); raw != "" && raw != "0" {
				if req.SizeLimit, err = quick.ParseSize(raw); err != nil {
					refuse(err.Error(), t.SizeInvalid)
					return
				}
			}
		} else if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		ceiling := quick.FormatSize(s.ceiling())
		switch {
		case req.SizeLimit < 0:
			refuse("invalid size limit", t.SizeInvalid)
			return
		case req.SizeLimit > s.ceiling():
			refuse("the largest size limit is "+ceiling, fmt.Sprintf(t.SizeTooHigh, ceiling))
			return
		}
		cur.SizeLimit = req.SizeLimit
		if req.SizeLimit == s.defaultLimit() {
			cur.SizeLimit = 0
		}
		if err := s.meta.save(name, cur); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("size limit %q set to %s by %s", name, quick.FormatSize(s.limitOf(cur)), email)
		if browserForm {
			http.Redirect(w, r, "/dashboard/site/"+name, http.StatusSeeOther)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(quick.LimitResponse{
		Site: name, SizeLimit: s.limitOf(cur), Custom: cur.SizeLimit > 0,
		Default: s.defaultLimit(), Max: s.ceiling(), Admin: s.isAdmin(email), Admins: s.adminList(),
	})
}
