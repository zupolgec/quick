// Sites limited to chosen people (access "people"): only the owner (the
// site's creator) and the company accounts in its viewer list can open them,
// after SSO. Write rules are documented on canWrite.
package main

import (
	"fmt"
	"html/template"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/zupolgec/quick/internal/quick"
)

// sameUser compares two identities (emails) the one way used everywhere:
// trimmed, case-insensitive; an empty identity matches nothing.
func sameUser(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && strings.EqualFold(a, b)
}

// isOwner reports whether email is the site's creator.
func isOwner(p policy, email string) bool { return sameUser(p.CreatedBy, email) }

// actsAsOwner: the owner, or an administrator (QUICK_ADMINS), who can do on
// any site whatever its owner can.
func (s *server) actsAsOwner(p policy, email string) bool {
	return isOwner(p, email) || s.isAdmin(email)
}

// mayView is canView plus administrators, who see every site.
func (s *server) mayView(p policy, email string) bool { return canView(p, email) || s.isAdmin(email) }

// canView reports whether the signed-in email may open the site's contents.
// Only "people" sites restrict signed-in users; everything else is up to the
// usual gate (SSO, code, public).
func canView(p policy, email string) bool {
	if p.Access != quick.AccessPeople {
		return true
	}
	return isOwner(p, email) || slices.ContainsFunc(p.Viewers, func(v string) bool { return sameUser(v, email) })
}

// blindDeploy reports whether a deploy must not touch the site's live content
// on the caller's behalf: a people site and a caller who can't open it. Such a
// caller may still publish (canWrite), but the delta deploy would be an
// oracle (which chunks a plan asks for, whether a commit can reuse a live
// chunk, both reveal content), so the delta endpoints answer 404 and the CLI
// falls back to the full upload, which never reads the live files. Deploy
// tokens never count as viewers, whoever created them.
func blindDeploy(p policy, ident authIdentity) bool {
	return p.Access == quick.AccessPeople && (ident.Token || !canView(p, ident.Email))
}

// refuseBlindDelta answers 404 to a delta request from a blind deployer
// (blindDeploy), before any lease or manifest is looked at, so every such
// request gets the same answer.
func refuseBlindDelta(w http.ResponseWriter, p policy, ident authIdentity) bool {
	if !blindDeploy(p, ident) {
		return false
	}
	http.Error(w, "delta deploy not available for this site: upload the whole folder", http.StatusNotFound)
	return true
}

// siteVisibleTo answers 404 (as the dashboard does) when the site is a people
// site email can't open, so its per-site metadata stays theirs; it reports
// whether the caller may go on. Writers re-check under the site lock.
func (s *server) siteVisibleTo(w http.ResponseWriter, name, email string) bool {
	p, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return false
	}
	if !s.mayView(p, email) {
		http.Error(w, "site not found", http.StatusNotFound)
		return false
	}
	return true
}

// Kinds of policy errors the dashboard shows in its own words.
const (
	errNotOwner     = "not_owner"     // arg: the owner
	errNoOwner      = "no_owner"      // site without a recorded creator
	errBadEmail     = "bad_email"     // arg: the address
	errDomain       = "domain"        // arg: the address
	errTooMany      = "too_many"      // over maxViewers
	errOwnerAlways  = "owner_always"  // arg: the owner
	errCodeRequired = "code_required" // access=code without a code
)

// policyErr is a refused policy change: msg for the API, kind/arg for the
// dashboard (empty kind: a generic message).
type policyErr struct {
	status int
	msg    string
	kind   string
	arg    string
}

// editViewers computes the viewer list of a site becoming (or staying)
// "people" after req: lowercased, deduplicated, never including the owner.
func (s *server) editViewers(cur policy, req quick.PolicyRequest) ([]string, *policyErr) {
	var list []string
	if cur.Access == quick.AccessPeople {
		list = slices.Clone(cur.Viewers)
	}
	add := req.AddViewers
	if req.Viewers != nil {
		list = nil
		add = append(slices.Clone(*req.Viewers), add...)
	}
	for _, raw := range add {
		v, perr := s.normViewer(raw)
		if perr != nil {
			return nil, perr
		}
		if !isOwner(cur, v) && !slices.ContainsFunc(list, func(e string) bool { return sameUser(e, v) }) {
			list = append(list, v)
		}
	}
	for _, raw := range req.RemoveViewers {
		v := strings.ToLower(strings.TrimSpace(raw))
		if isOwner(cur, v) {
			return nil, &policyErr{status: http.StatusBadRequest, kind: errOwnerAlways, arg: cur.CreatedBy,
				msg: "the owner (" + cur.CreatedBy + ") can always see the site and can't be removed"}
		}
		list = slices.DeleteFunc(list, func(e string) bool { return sameUser(e, v) })
	}
	if len(list) > maxViewers {
		return nil, &policyErr{status: http.StatusBadRequest, kind: errTooMany,
			msg: fmt.Sprintf("too many people: a site can be shared with at most %d people", maxViewers)}
	}
	return list, nil
}

// normViewer validates a viewer address: a bare, well-formed email in one of
// the company's allowed domains (any domain with QUICK_ALLOWED_DOMAINS="*").
func (s *server) normViewer(raw string) (string, *policyErr) {
	v := strings.ToLower(strings.TrimSpace(raw))
	a, err := mail.ParseAddress(v)
	local, domain, _ := strings.Cut(v, "@")
	if err != nil || a.Name != "" || a.Address != v || local == "" || domain == "" {
		return "", &policyErr{status: http.StatusBadRequest, kind: errBadEmail, arg: raw,
			msg: "invalid email address " + strconv.Quote(raw)}
	}
	if !s.domainAllowed(domain) {
		return "", &policyErr{status: http.StatusBadRequest, kind: errDomain, arg: v,
			msg: v + " is not a company account (allowed: " + strings.TrimSpace(s.domain) + ")"}
	}
	return v, nil
}

func isFormPost(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/x-www-form-urlencoded") || strings.HasPrefix(ct, "multipart/form-data")
}

// policyFormRequest turns the dashboard's access forms into a policy request:
//
//	op=access access=team|only-me|people|public|code [code=...]
//	op=add    email=a@x, b@x
//	op=remove email=a@x
func policyFormRequest(r *http.Request, cur policy) (quick.PolicyRequest, *policyErr) {
	var req quick.PolicyRequest
	switch r.FormValue("op") {
	case "access":
		switch a := r.FormValue("access"); a {
		case "team":
			req.Access = new("sso")
		case "only-me":
			req.Access, req.Viewers = new(quick.AccessPeople), &[]string{}
		case quick.AccessPeople, quick.AccessPublic:
			req.Access = new(a)
		case quick.AccessCode:
			code := strings.TrimSpace(r.FormValue("code"))
			if code == "" && cur.Access == quick.AccessCode {
				return req, nil // keep the current code
			}
			req.Access, req.Code = new(quick.AccessCode), &code
		default:
			return req, &policyErr{status: http.StatusBadRequest, msg: "invalid access"}
		}
	case "add":
		req.AddViewers = strings.FieldsFunc(r.FormValue("email"), func(c rune) bool {
			return c == ',' || c == ';' || c == ' ' || c == '\n' || c == '\r' || c == '\t'
		})
		if len(req.AddViewers) == 0 {
			return req, &policyErr{status: http.StatusBadRequest, kind: errBadEmail, msg: "missing email"}
		}
	case "remove":
		req.RemoveViewers = []string{r.FormValue("email")}
	default:
		return req, &policyErr{status: http.StatusBadRequest, msg: "invalid form"}
	}
	return req, nil
}

// policyErrText is the dashboard's wording for a refused change.
func policyErrText(e *policyErr, t uiText) string {
	switch e.kind {
	case errNotOwner:
		return t.AccessErrNotOwnerA + " (" + e.arg + ") " + t.AccessErrNotOwnerB
	case errNoOwner:
		return t.AccessErrNoOwner
	case errBadEmail:
		if e.arg == "" {
			return t.AccessErrMissingEmail
		}
		return e.arg + ": " + t.AccessErrBadEmail
	case errDomain:
		return e.arg + ": " + t.AccessErrDomain
	case errTooMany:
		return t.AccessErrTooMany
	case errOwnerAlways:
		return t.AccessErrOwnerAlways
	case errCodeRequired:
		return t.AccessErrCode
	default:
		return t.AccessErrGeneric
	}
}

// renderRestricted is the page a signed-in visitor gets on a people site
// they're not on: who owns it, who they're signed in as, and a way to switch
// account. It never shows the viewer list.
func (s *server) renderRestricted(w http.ResponseWriter, r *http.Request, host string, p policy, email string) {
	l := pickLang(r)
	siteURL := "https://" + host + r.URL.RequestURI()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Accept-Language")
	w.WriteHeader(http.StatusForbidden)
	_ = restrictedPage.Execute(w, map[string]any{
		"Host": host, "Owner": p.CreatedBy, "Email": email, "Lang": string(l), "T": textsFor(l),
		"Switch": "https://" + s.baseDomain + "/oauth2/sign_out?rd=" + url.QueryEscape(siteURL),
	})
}

var restrictedPage = template.Must(template.New("restricted").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.RestrictedTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
body{display:grid;place-items:center;padding:1.5rem}
.card{width:min(380px,100%);background:var(--card);border:1px solid var(--border);border-radius:18px;padding:2.25rem 1.75rem;text-align:center;box-shadow:0 1px 2px rgba(13,24,50,.05),0 18px 48px rgba(13,24,50,.10)}
.card .brand{font-size:1.5rem;margin-bottom:.9rem}
h1{font-size:1.15rem;font-weight:700;margin:0 0 .35rem;color:var(--ink)}
p{margin:0 0 1rem;color:var(--muted);font-size:.9rem;overflow-wrap:anywhere}
p b{color:var(--ink);font-weight:600}
.btn{width:100%;padding:.8rem;font-size:.95rem;margin-top:.6rem}
</style></head><body>
<div class="card">
  ` + brandWordmark + `
  <h1>{{.T.RestrictedHeading}}</h1>
  <p>{{.T.RestrictedBy}} <b>{{.Owner}}</b> {{.T.RestrictedCanOpen}} <b>{{.Host}}</b>.</p>
  {{if .Email}}<p>{{.T.RestrictedSignedIn}} <b>{{.Email}}</b>. {{.T.RestrictedAsk}}</p>{{end}}
  <a class="btn" href="{{.Switch}}">{{.T.RestrictedSwitch}}</a>
</div></body></html>`))
