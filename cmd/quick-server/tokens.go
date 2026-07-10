package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

func (s *server) handleTokens(w http.ResponseWriter, r *http.Request, name string, rest string) {
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.handleTokenList(w, r, name)
	case rest == "" && r.Method == http.MethodPost:
		s.handleTokenCreate(w, r, name)
	case rest != "" && r.Method == http.MethodDelete:
		s.handleTokenRevoke(w, r, name, strings.Trim(rest, "/"))
	case rest != "" && r.Method == http.MethodPost && r.FormValue("_method") == "delete":
		s.handleTokenRevoke(w, r, name, strings.Trim(rest, "/"))
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleTokenList(w http.ResponseWriter, r *http.Request, name string) {
	email, err := s.authenticateUser(r)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	p, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(p, email, actToken); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	_ = json.NewEncoder(w).Encode(quick.TokensResponse{Site: name, Tokens: tokenInfos(p.Tokens)})
}

func (s *server) handleTokenCreate(w http.ResponseWriter, r *http.Request, name string) {
	email, err := s.authenticateUser(r)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	browserForm := !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
	if browserForm && !s.meta.validCSRF(email, r.FormValue("csrf")) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	p, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(p, email, actToken); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	if len(p.Tokens) >= maxSiteTokens {
		http.Error(w, fmt.Sprintf("site has the maximum of %d deploy tokens", maxSiteTokens), http.StatusBadRequest)
		return
	}

	req, err := tokenCreateRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	exp, err := tokenExpiry(req.ExpiresIn)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw := "qk_" + randomID(32)
	now := nowStamp()
	st := siteToken{
		ID:        randomID(8),
		Name:      req.Name,
		Hash:      s.meta.hashToken(name, raw),
		Scopes:    []string{quick.TokenScopeDeploy},
		CreatedBy: email,
		CreatedAt: now,
	}
	if !exp.IsZero() {
		st.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	p.Tokens = append(p.Tokens, st)
	if err := s.meta.save(name, p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if browserForm {
		s.renderDashboardSitePage(w, r, name, email, raw)
		return
	}
	_ = json.NewEncoder(w).Encode(quick.TokenCreateResponse{Site: name, Token: raw, Info: tokenInfo(st)})
}

func (s *server) handleTokenRevoke(w http.ResponseWriter, r *http.Request, name string, tokenID string) {
	email, err := s.authenticateUser(r)
	if err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	browserForm := !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
	if browserForm && !s.meta.validCSRF(email, r.FormValue("csrf")) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	unlock := s.locks.lock(name)
	defer unlock()
	p, err := s.meta.load(name)
	if err != nil {
		http.Error(w, "could not read site state: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if ok, reason := s.canWrite(p, email, actToken); !ok {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	for i, st := range p.Tokens {
		if st.ID == tokenID {
			p.Tokens = append(p.Tokens[:i], p.Tokens[i+1:]...)
			if err := s.meta.save(name, p); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if browserForm {
				http.Redirect(w, r, "/dashboard/site/"+name, http.StatusSeeOther)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"site": name, "revoked": true, "id": tokenID})
			return
		}
	}
	http.Error(w, "token not found", http.StatusNotFound)
}

func tokenCreateRequest(r *http.Request) (quick.TokenCreateRequest, error) {
	var req quick.TokenCreateRequest
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, errors.New("invalid json")
		}
	} else {
		req.Name = r.FormValue("name")
		req.ExpiresIn = r.FormValue("expires_in")
		req.Scopes = []string{quick.TokenScopeDeploy}
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = "deploy"
	}
	if len(req.Name) > 40 {
		return req, errors.New("token name must be 40 characters or less")
	}
	for _, r := range req.Name {
		if !(r == '-' || r == '_' || r == ' ' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return req, errors.New("token name can only contain letters, numbers, spaces, dot, hyphen and underscore")
		}
	}
	for _, sc := range req.Scopes {
		if sc != quick.TokenScopeDeploy {
			return req, errors.New("unsupported token scope " + sc)
		}
	}
	return req, nil
}

func tokenExpiry(v string) (time.Time, error) {
	switch strings.TrimSpace(v) {
	case "", "90d":
		return time.Now().Add(90 * 24 * time.Hour), nil
	case "30d":
		return time.Now().Add(30 * 24 * time.Hour), nil
	case "180d":
		return time.Now().Add(180 * 24 * time.Hour), nil
	case "365d":
		return time.Now().Add(365 * 24 * time.Hour), nil
	case "never":
		return time.Time{}, nil
	default:
		return time.Time{}, errors.New("expires_in must be 30d, 90d, 180d, 365d, or never")
	}
}

func tokenInfos(tokens []siteToken) []quick.TokenInfo {
	out := make([]quick.TokenInfo, 0, len(tokens))
	for _, st := range tokens {
		out = append(out, tokenInfo(st))
	}
	return out
}

func tokenInfo(st siteToken) quick.TokenInfo {
	scopes := st.Scopes
	if len(scopes) == 0 {
		scopes = []string{quick.TokenScopeDeploy}
	}
	return quick.TokenInfo{
		ID: st.ID, Name: st.Name, Scopes: scopes, CreatedBy: st.CreatedBy, CreatedAt: st.CreatedAt,
		ExpiresAt: st.ExpiresAt, LastUsedAt: st.LastUsedAt, LastUsedBy: st.LastUsedBy,
	}
}
