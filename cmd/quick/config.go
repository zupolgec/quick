package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

func promptServer() string {
	fmt.Fprint(os.Stderr, "quick server URL (e.g. https://quick.example.com): ")
	return readLine()
}

func readLine() string {
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

// yesNo recognizes an affirmative answer (Italian or English).
func yesNo(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "s", "si", "sì", "y", "yes":
		return true
	}
	return false
}

type cliConfig struct {
	Server            string `json:"server"`
	OAuthClientID     string `json:"oauth_client_id"`
	OAuthClientSecret string `json:"oauth_client_secret,omitempty"`
	HostedDomain      string `json:"hosted_domain"`
	BaseDomain        string `json:"base_domain"`
	// OIDC issuer and its endpoints; all empty means Google.
	OIDCIssuer    string `json:"oidc_issuer,omitempty"`
	AuthEndpoint  string `json:"auth_endpoint,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
}

// configFile holds every known server, keyed by normalized URL. The embedded
// cliConfig mirrors the default server at the top level: it is the format of
// older versions, which keep working on the default server.
type configFile struct {
	cliConfig
	Default string                `json:"default,omitempty"`
	Servers map[string]*cliConfig `json:"servers,omitempty"`
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.Getenv("HOME")
	}
	return filepath.Join(dir, "quick", "config.json")
}

// loadConfigFile reads the config, migrating the single-server format.
func loadConfigFile() *configFile {
	f := &configFile{Servers: map[string]*cliConfig{}}
	b, err := os.ReadFile(configPath())
	if err != nil || json.Unmarshal(b, f) != nil {
		return &configFile{Servers: map[string]*cliConfig{}}
	}
	if f.Servers == nil {
		f.Servers = map[string]*cliConfig{}
	}
	if len(f.Servers) == 0 && f.Server != "" {
		legacy := f.cliConfig
		legacy.Server = normalizeServer(legacy.Server)
		f.Servers[legacy.Server] = &legacy
		f.Default = legacy.Server
	}
	return f
}

func (f *configFile) save() {
	if d := f.Servers[f.Default]; d != nil {
		f.cliConfig = *d
	}
	p := configPath()
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	_ = os.WriteFile(p, b, 0o600)
}

// loadConfig returns the default server's config (nil if none).
func loadConfig() *cliConfig {
	f := loadConfigFile()
	return f.Servers[f.Default]
}

// saveServerConfig remembers a server; it becomes the default if asked or if
// there is no default yet.
func saveServerConfig(c *cliConfig, makeDefault bool) {
	f := loadConfigFile()
	f.Servers[c.Server] = c
	if makeDefault || f.Servers[f.Default] == nil {
		f.Default = c.Server
	}
	f.save()
}

// resolveConfig picks the server (flag > env > .quick > default > prompt) and
// returns its config, from the cache or fetched from /api/config.
func resolveConfig(serverFlag, siteServer string) (*cliConfig, error) {
	server := serverFlag
	if server == "" {
		server = os.Getenv("QUICK_SERVER")
	}
	if server == "" {
		server = siteServer
	}
	f := loadConfigFile()
	if server == "" {
		if d := f.Servers[f.Default]; d != nil && d.OAuthClientID != "" {
			return d, nil // no explicit server: use the default one
		}
		server = promptServer()
	}
	if server == "" {
		return nil, errors.New("server required (--server, QUICK_SERVER, or enter it at the prompt)")
	}

	server = normalizeServer(server)
	if c := f.Servers[server]; c != nil && c.OAuthClientID != "" {
		return c, nil
	}
	c, err := fetchConfig(server)
	if err != nil {
		return nil, fmt.Errorf("server unreachable (%s): %w", server, err)
	}
	c.Server = server
	saveServerConfig(c, false)
	return c, nil
}

// normalizeServer turns the server input (bare domain or URL, any case,
// trailing slashes) into the canonical URL used as key. API and auth all live
// on the apex, so there is no deploy.<domain> fallback.
func normalizeServer(input string) string {
	input = strings.TrimRight(strings.TrimSpace(input), "/")
	if !strings.Contains(input, "://") {
		input = "https://" + input
	}
	if u, err := url.Parse(input); err == nil && u.Host != "" {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.Path = strings.TrimRight(u.Path, "/")
		return u.String()
	}
	return input
}

func fetchConfig(server string) (*cliConfig, error) {
	cli := &http.Client{Timeout: 15 * time.Second}
	resp, err := cli.Get(server + "/api/config")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("server unreachable or /api/config missing")
	}
	var r quick.ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	c := &cliConfig{
		OAuthClientID:     r.OAuthClientID,
		OAuthClientSecret: r.OAuthClientSecret,
		HostedDomain:      r.HostedDomain,
		BaseDomain:        r.BaseDomain,
		OIDCIssuer:        r.OIDCIssuer,
	}
	if c.OIDCIssuer != "" {
		if c.AuthEndpoint, c.TokenEndpoint, err = discover(cli, c.OIDCIssuer); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// discover reads the authorization and token endpoints from the issuer's
// OpenID configuration.
func discover(cli *http.Client, issuer string) (auth, token string, err error) {
	resp, err := cli.Get(strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var d struct {
		Auth  string `json:"authorization_endpoint"`
		Token string `json:"token_endpoint"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&d) != nil || d.Auth == "" || d.Token == "" {
		return "", "", fmt.Errorf("identity provider %s unreachable or misconfigured", issuer)
	}
	return d.Auth, d.Token, nil
}
