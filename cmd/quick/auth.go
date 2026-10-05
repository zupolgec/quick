package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	redirectURI         = "http://127.0.0.1:8765/callback"
	googleAuthEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenEndpoint = "https://oauth2.googleapis.com/token"
)

// endpoints returns the identity provider's authorization and token
// endpoints: Google unless the server declared an OIDC issuer.
func endpoints(cfg *cliConfig) (auth, token string) {
	if cfg.OIDCIssuer == "" {
		return googleAuthEndpoint, googleTokenEndpoint
	}
	return cfg.AuthEndpoint, cfg.TokenEndpoint
}

type tokenSet struct {
	IDToken      string    `json:"id_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

func tokenPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.Getenv("HOME")
	}
	return filepath.Join(dir, "quick", "token.json")
}

// tokenFile holds one login per server (normalized URL). The embedded
// tokenSet mirrors the default server's login: the format of older versions.
type tokenFile struct {
	tokenSet
	Servers map[string]*tokenSet `json:"servers,omitempty"`
}

// loadTokens reads the logins, migrating a single-server file to the server
// it belonged to (the default one).
func loadTokens() *tokenFile {
	f := &tokenFile{}
	b, err := os.ReadFile(tokenPath())
	if err != nil || json.Unmarshal(b, f) != nil {
		f = &tokenFile{}
	}
	if f.Servers == nil {
		f.Servers = map[string]*tokenSet{}
		if f.IDToken != "" || f.RefreshToken != "" {
			if def := loadConfigFile().Default; def != "" {
				legacy := f.tokenSet
				f.Servers[def] = &legacy
			}
		}
	}
	return f
}

func loadToken(server string) (*tokenSet, error) {
	if t := loadTokens().Servers[server]; t != nil {
		return t, nil
	}
	return nil, os.ErrNotExist
}

func saveToken(server string, t *tokenSet) error {
	f := loadTokens()
	f.Servers[server] = t
	f.tokenSet = tokenSet{}
	if d := f.Servers[loadConfigFile().Default]; d != nil {
		f.tokenSet = *d
	}
	p := tokenPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

// idToken returns a valid ID token: cache, then refresh, then interactive login.
func idToken(cfg *cliConfig) (string, error) {
	if tok, ok := silentToken(cfg); ok {
		return tok, nil
	}
	fmt.Fprintln(os.Stderr, "Not authenticated: logging in.")
	nt, err := login(cfg)
	if err != nil {
		return "", err
	}
	return nt.IDToken, nil
}

// haveLogin reports whether a saved login exists for server, without network
// or checking expiry; for the quick overview.
func haveLogin(server string) bool {
	t, err := loadToken(server)
	return err == nil && (t.IDToken != "" || t.RefreshToken != "")
}

// silentToken returns an ID token without interaction (cache, then refresh).
// ok=false when an interactive login would be needed. A login issued for a
// different OAuth client is never used (nor sent to this server).
func silentToken(cfg *cliConfig) (string, bool) {
	t, err := loadToken(cfg.Server)
	if err != nil || (t.IDToken != "" && !audienceHas(t.IDToken, cfg.OAuthClientID)) {
		return "", false
	}
	if t.IDToken != "" && time.Now().Before(t.Expiry.Add(-time.Minute)) {
		return t.IDToken, true
	}
	if t.RefreshToken != "" {
		v := url.Values{
			"client_id":     {cfg.OAuthClientID},
			"refresh_token": {t.RefreshToken},
			"grant_type":    {"refresh_token"},
		}
		withSecret(v, cfg)
		if nt, rerr := tokenRequest(cfg, v); rerr == nil {
			if nt.RefreshToken == "" {
				nt.RefreshToken = t.RefreshToken
			}
			saveToken(cfg.Server, nt)
			return nt.IDToken, true
		}
	}
	return "", false
}

func login(cfg *cliConfig) (*tokenSet, error) {
	state := randState()
	verifier, challenge := pkcePair()
	codeCh, errCh := make(chan string, 1), make(chan error, 1)

	ln, err := net.Listen("tcp", "127.0.0.1:8765")
	if err != nil {
		return nil, fmt.Errorf("port 8765 already in use (%w)", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("state mismatch")
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			errCh <- errors.New(e)
			return
		}
		fmt.Fprintln(w, "Login complete. Return to the terminal.")
		codeCh <- q.Get("code")
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	q := url.Values{
		"client_id":             {cfg.OAuthClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	if cfg.OIDCIssuer == "" {
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
		// `hd` restricts the Google account picker to one domain; only meaningful
		// for a single domain (not "*" or a list).
		if hd := cfg.HostedDomain; hd != "" && hd != "*" && !strings.Contains(hd, ",") {
			q.Set("hd", hd)
		}
	} else {
		q.Set("scope", "openid email profile offline_access")
	}
	authEndpoint, _ := endpoints(cfg)
	authURL := authEndpoint + "?" + q.Encode()

	fmt.Println("Opening the browser for login…")
	fmt.Println("If it doesn't open, open it yourself:\n  " + authURL)
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return nil, err
	case <-time.After(3 * time.Minute):
		return nil, errors.New("login timed out")
	}

	v := url.Values{
		"client_id":     {cfg.OAuthClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}
	withSecret(v, cfg)
	t, err := tokenRequest(cfg, v)
	if err != nil {
		return nil, err
	}
	return t, saveToken(cfg.Server, t)
}

// withSecret adds client_secret to the token exchange only if the server
// provided one (a Web-type OAuth client reused for the CLI).
func withSecret(v url.Values, cfg *cliConfig) {
	if cfg.OAuthClientSecret != "" {
		v.Set("client_secret", cfg.OAuthClientSecret)
	}
}

func tokenRequest(cfg *cliConfig, v url.Values) (*tokenSet, error) {
	_, tokenEndpoint := endpoints(cfg)
	resp, err := httpClient.PostForm(tokenEndpoint, v)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Error != "" {
		return nil, fmt.Errorf("%s: %s", r.Error, r.ErrorDesc)
	}
	if r.IDToken == "" {
		return nil, errors.New("no id_token in the response")
	}
	return &tokenSet{
		IDToken:      r.IDToken,
		RefreshToken: r.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
	}, nil
}

func pkcePair() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

func openBrowser(u string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", u).Start()
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		exec.Command("xdg-open", u).Start()
	}
}

func randState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// emailFromToken reads the "email" claim from an ID token (JWT) payload without
// verifying the signature: only used to display who you are locally.
func emailFromToken(idtok string) string {
	parts := strings.Split(idtok, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Email string `json:"email"`
	}
	json.Unmarshal(b, &c)
	return c.Email
}

// audienceHas reports whether the ID token (unverified, local check) was
// issued for clientID; "aud" may be a string or a list.
func audienceHas(idtok, clientID string) bool {
	parts := strings.Split(idtok, ".")
	if len(parts) != 3 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var c struct {
		Aud json.RawMessage `json:"aud"`
	}
	json.Unmarshal(b, &c)
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return one == clientID
	}
	var many []string
	json.Unmarshal(c.Aud, &many)
	return slices.Contains(many, clientID)
}

func localPart(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}
