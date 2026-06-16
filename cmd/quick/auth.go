// Login Google per la CLI: flusso OAuth "loopback" con PKCE (apre il browser,
// cattura il code su 127.0.0.1, lo scambia per un ID token). Nessun
// client_secret embeddato: il client_id arriva dalla config del server e si usa
// PKCE (richiede un client OAuth di tipo "Desktop app"). ID token e refresh
// token sono salvati in ~/.config/quick/token.json.
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
	"strings"
	"time"
)

const (
	redirectURI   = "http://127.0.0.1:8765/callback"
	authEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint = "https://oauth2.googleapis.com/token"
)

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

func loadToken() (*tokenSet, error) {
	b, err := os.ReadFile(tokenPath())
	if err != nil {
		return nil, err
	}
	var t tokenSet
	return &t, json.Unmarshal(b, &t)
}

func saveToken(t *tokenSet) error {
	p := tokenPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(t, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

// idToken restituisce un ID token valido: cache, poi refresh, poi login interattivo.
func idToken(cfg *cliConfig) (string, error) {
	t, err := loadToken()
	if err == nil && t.IDToken != "" && time.Now().Before(t.Expiry.Add(-time.Minute)) {
		return t.IDToken, nil
	}
	if err == nil && t.RefreshToken != "" {
		v := url.Values{
			"client_id":     {cfg.OAuthClientID},
			"refresh_token": {t.RefreshToken},
			"grant_type":    {"refresh_token"},
		}
		withSecret(v, cfg)
		if nt, rerr := tokenRequest(v); rerr == nil {
			if nt.RefreshToken == "" {
				nt.RefreshToken = t.RefreshToken
			}
			saveToken(nt)
			return nt.IDToken, nil
		}
	}
	fmt.Fprintln(os.Stderr, "Non sei autenticato: eseguo il login.")
	nt, err := login(cfg)
	if err != nil {
		return "", err
	}
	return nt.IDToken, nil
}

// login esegue il flusso interattivo PKCE e salva il token.
func login(cfg *cliConfig) (*tokenSet, error) {
	state := randState()
	verifier, challenge := pkcePair()
	codeCh, errCh := make(chan string, 1), make(chan error, 1)

	ln, err := net.Listen("tcp", "127.0.0.1:8765")
	if err != nil {
		return nil, fmt.Errorf("porta 8765 occupata (%w)", err)
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
		fmt.Fprintln(w, "Login completato. Torna al terminale.")
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
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	// `hd` restringe il selettore Google a un dominio: ha senso solo con un
	// singolo dominio (non con "*" o una lista).
	if hd := cfg.HostedDomain; hd != "" && hd != "*" && !strings.Contains(hd, ",") {
		q.Set("hd", hd)
	}
	authURL := authEndpoint + "?" + q.Encode()

	fmt.Println("Apro il browser per il login Google…")
	fmt.Println("Se non si apre, apri tu:\n  " + authURL)
	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return nil, err
	case <-time.After(3 * time.Minute):
		return nil, errors.New("timeout login")
	}

	v := url.Values{
		"client_id":     {cfg.OAuthClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}
	withSecret(v, cfg)
	t, err := tokenRequest(v)
	if err != nil {
		return nil, err
	}
	return t, saveToken(t)
}

// withSecret aggiunge il client_secret allo scambio token solo se il server ne
// ha fornito uno (client OAuth di tipo Web riusato per la CLI).
func withSecret(v url.Values, cfg *cliConfig) {
	if cfg.OAuthClientSecret != "" {
		v.Set("client_secret", cfg.OAuthClientSecret)
	}
}

func tokenRequest(v url.Values) (*tokenSet, error) {
	resp, err := http.PostForm(tokenEndpoint, v)
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
		return nil, errors.New("nessun id_token nella risposta")
	}
	return &tokenSet{
		IDToken:      r.IDToken,
		RefreshToken: r.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
	}, nil
}

// pkcePair genera (code_verifier, code_challenge S256).
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
