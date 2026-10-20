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

// Site access modes. Empty string = SSO (company default). People: only the
// site's owner and the accounts in its viewer list (signed in with SSO).
const (
	AccessSSO    = ""
	AccessPublic = "public"
	AccessCode   = "code"
	AccessPeople = "people"
)

// PolicyRequest is the body of PATCH/POST /api/site/<name>/policy.
//
// Viewer changes are for "people" sites: Viewers replaces the list (empty =
// only the owner), AddViewers/RemoveViewers edit it; any of them on a site
// that isn't "people" yet switches it. Only the site's owner can.
type PolicyRequest struct {
	Access        *string   `json:"access,omitempty"` // "sso" | "public" | "code" | "people"
	Code          *string   `json:"code,omitempty"`   // required for access=code
	Locked        *bool     `json:"locked,omitempty"`
	Viewers       *[]string `json:"viewers,omitempty"`
	AddViewers    []string  `json:"add_viewers,omitempty"`
	RemoveViewers []string  `json:"remove_viewers,omitempty"`
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
	// Viewers of a "people" site (owner excluded), only for those who can
	// open it.
	Viewers []string `json:"viewers,omitempty"`
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
	// Warning: published, but something after it failed (site state not
	// saved). Still a success: the site is live.
	Warning string `json:"warning,omitempty"`
}

const (
	TokenScopeDeploy = "deploy"
)

// Delta deploy (chunk-level transfer). The CLI describes the folder as a
// manifest of files split into content-defined chunks (internal/chunk):
//
//	POST /api/deploy/plan?name=    manifest -> DeployPlanResponse (chunks the server lacks)
//	POST /api/deploy/chunks?name=  gzip stream of chunk records (only the missing ones)
//	POST /api/deploy/commit?name=  manifest -> DeployResponse; 409 = re-plan and retry
//
// All three belong to one deploy lease (DeployLeaseHeader, below).
//
// Manifest bodies are JSON, optionally gzip (Content-Encoding: gzip). A chunk
// record is the raw 32-byte SHA-256, the length as big-endian uint32, then the
// bytes. Hashes in JSON are lowercase hex.
type DeployManifest struct {
	Files []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path   string          `json:"path"`
	Mode   uint32          `json:"mode,omitempty"` // permission bits; 0 = 0644
	Size   int64           `json:"size"`
	SHA256 string          `json:"sha256"` // whole file
	Chunks []ManifestChunk `json:"chunks"` // in order; empty for an empty file
}

type ManifestChunk struct {
	Hash string `json:"h"`
	Size int    `json:"n"`
}

type DeployPlanResponse struct {
	Missing      []string `json:"missing"`         // chunk hashes to upload
	TotalBytes   int64    `json:"total_bytes"`     // size of the whole folder
	MissingBytes int64    `json:"missing_bytes"`   // size of the missing chunks
	Lease        string   `json:"lease,omitempty"` // send back in DeployLeaseHeader
}

// One deploy at a time per site: the plan hands out a lease id that the CLI
// sends back in DeployLeaseHeader on its chunks, commit and re-plans. Any
// other deploy of the site meanwhile (plan, tar upload, rollback) gets 423
// Locked with a DeployLocked body; ?takeover=1 on a plan, tar upload or rollback
// breaks the lease. Idle leases expire on their own.
const DeployLeaseHeader = "X-Quick-Deploy"

// DeployLocked.Error values.
const (
	LeaseLocked = "locked"     // another deploy of the site is in progress
	LeaseLost   = "lease_lost" // this deploy's lease was broken or expired
)

// DeployLocked is the 423 response body: JSON when the request accepts
// application/json, otherwise just Message as text (older CLIs print it).
type DeployLocked struct {
	Error   string `json:"error"`
	Holder  string `json:"holder,omitempty"` // who holds the site's lease now
	Since   string `json:"since,omitempty"`  // RFC3339, when their deploy started
	Message string `json:"message"`
}

type ChunkUploadResponse struct {
	Stored int   `json:"stored"`
	Bytes  int64 `json:"bytes"`
}

// ChunkRecordHeader is the size of a chunk record header (hash + length).
const ChunkRecordHeader = 32 + 4

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
	// OIDC issuer of the identity provider; empty means Google (servers
	// predating generic OIDC never send it).
	OIDCIssuer string `json:"oidc_issuer,omitempty"`
	// The server accepts delta deploys (/api/deploy/plan|chunks|commit);
	// older servers never send it and get the full tar upload.
	DeltaDeploy bool `json:"delta_deploy,omitempty"`
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

// DomainRequest attaches a custom domain to a site.
type DomainRequest struct {
	Domain string `json:"domain"`
}

// DNSRecord is a record the user must create at their DNS provider.
type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// DomainInfo is a custom domain of a site. Active means its DNS points to the
// site and it is being served.
type DomainInfo struct {
	Domain  string      `json:"domain"`
	Active  bool        `json:"active"`
	Records []DNSRecord `json:"records"`
}

type DomainsResponse struct {
	Site    string       `json:"site"`
	Domains []DomainInfo `json:"domains"`
}

// ReservedDir is the path prefix quick serves on every site (code page,
// quick.js, client API): a site can't publish files under it.
const ReservedDir = "__quick"

// IsReservedPath reports whether a site-relative path (slash-separated, no
// leading slash) falls under ReservedDir.
func IsReservedPath(rel string) bool {
	return rel == ReservedDir || strings.HasPrefix(rel, ReservedDir+"/")
}

// ReservedPathError is the message for a deploy carrying reserved files.
func ReservedPathError(rel string) string {
	return "the " + ReservedDir + "/ folder is reserved by quick: rename or remove it (found " + rel + ")"
}
