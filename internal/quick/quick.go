// Package quick is the contract shared by the CLI (cmd/quick) and the server
// (cmd/quick-server): name validation, access modes, API DTOs and a few
// helpers. Whatever the two sides must agree on lives here so it can't diverge.
package quick

import (
	"os"
	"regexp"
	"strings"
)

// NameRe validates a site name (= subdomain): lowercase, digits, hyphen.
var NameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func ValidName(name string) bool { return NameRe.MatchString(name) }

// Site access modes. Empty string = SSO (company default).
const (
	AccessSSO    = ""
	AccessPublic = "public"
	AccessCode   = "code"
)

// PolicyRequest is the body of PATCH/POST /api/site/<name>/policy.
type PolicyRequest struct {
	Access *string `json:"access,omitempty"` // "sso" | "public" | "code"
	Code   *string `json:"code,omitempty"`   // required for access=code
	Locked *bool   `json:"locked,omitempty"`
}

// PolicyResponse is the policy endpoints' response (POST mutates, GET reads).
type PolicyResponse struct {
	Site      string `json:"site"`
	Access    string `json:"access"`
	Locked    bool   `json:"locked"`
	Owner     string `json:"owner"`
	Exists    bool   `json:"exists"` // site has contents or metadata
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at,omitempty"` // RFC3339
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"` // RFC3339
}

// SiteInfo describes a site in the /api/sites list (and the dashboard).
type SiteInfo struct {
	Site      string `json:"site"`
	URL       string `json:"url"`
	Access    string `json:"access"`
	Locked    bool   `json:"locked"`
	Owner     string `json:"owner,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type SitesResponse struct {
	Sites []SiteInfo `json:"sites"`
}

type RollbackResponse struct {
	Site       string `json:"site"`
	RolledBack bool   `json:"rolled_back"`
	URL        string `json:"url,omitempty"`
}

type DeleteResponse struct {
	Site    string `json:"site"`
	Deleted bool   `json:"deleted"`
}

type DeployResponse struct {
	Site string `json:"site"`
	URL  string `json:"url"`
	By   string `json:"by"`
}

const (
	TokenScopeDeploy = "deploy"
)

type TokenCreateRequest struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes,omitempty"`
	ExpiresIn string   `json:"expires_in,omitempty"` // "30d", "90d", "180d", "365d", or "never"
}

type TokenInfo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	CreatedBy  string   `json:"created_by,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	LastUsedBy string   `json:"last_used_by,omitempty"`
}

type TokenCreateResponse struct {
	Site  string    `json:"site"`
	Token string    `json:"token"`
	Info  TokenInfo `json:"info"`
}

type TokensResponse struct {
	Site   string      `json:"site"`
	Tokens []TokenInfo `json:"tokens"`
}

// ConfigResponse is the public /api/config response: everything the CLI needs
// to self-configure without hardcoded values.
type ConfigResponse struct {
	OAuthClientID string `json:"oauth_client_id"`
	// Set only when the server reuses a "Web"-type OAuth client for the CLI
	// (which needs the secret in the token exchange). For a "Desktop" client it
	// stays empty and the CLI uses PKCE only.
	OAuthClientSecret string `json:"oauth_client_secret,omitempty"`
	HostedDomain      string `json:"hosted_domain"`
	BaseDomain        string `json:"base_domain"`
	Version           string `json:"version,omitempty"`
}

// Env returns env var k, or def if empty/absent.
func Env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// SplitList splits a comma-separated list, ignoring spaces and empties.
func SplitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
