// Auto-configurazione della CLI: l'unico dato che l'utente deve fornire è l'URL
// del server (--server o QUICK_SERVER). Il resto (client OAuth, hosted domain,
// dominio dei siti) lo chiede al server via GET /api/config e lo cache in
// ~/.config/quick/config.json. Niente valori hardcoded nel binario.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wayexperience/quick/internal/quick"
)

// promptServer chiede l'URL del server quando non è dato da flag/env/cache.
func promptServer() string {
	fmt.Fprint(os.Stderr, "URL del server quick (es. https://quick.example.com): ")
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

type cliConfig struct {
	Server            string `json:"server"`
	OAuthClientID     string `json:"oauth_client_id"`
	OAuthClientSecret string `json:"oauth_client_secret,omitempty"`
	HostedDomain      string `json:"hosted_domain"`
	BaseDomain        string `json:"base_domain"`
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.Getenv("HOME")
	}
	return filepath.Join(dir, "quick", "config.json")
}

func loadConfig() *cliConfig {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return nil
	}
	var c cliConfig
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return &c
}

func saveConfig(c *cliConfig) {
	p := configPath()
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	_ = os.WriteFile(p, b, 0o600)
}

// resolveConfig determina il server (flag > env > cache) e restituisce la config,
// usando la cache se è dello stesso server, altrimenti rifetchando da /api/config.
func resolveConfig(serverFlag string) (*cliConfig, error) {
	server := serverFlag
	if server == "" {
		server = os.Getenv("QUICK_SERVER")
	}
	saved := loadConfig()
	if server == "" && saved != nil {
		server = saved.Server
	}
	if server == "" {
		server = promptServer() // chiedi una volta, poi viene ricordato
	}
	if server == "" {
		return nil, errors.New("server richiesto (--server, QUICK_SERVER, o inseriscilo al prompt)")
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	server = strings.TrimRight(server, "/")
	if saved != nil && saved.Server == server && saved.OAuthClientID != "" {
		return saved, nil
	}
	c, err := fetchConfig(server)
	if err != nil {
		return nil, err
	}
	c.Server = server
	saveConfig(c)
	return c, nil
}

func fetchConfig(server string) (*cliConfig, error) {
	cli := &http.Client{Timeout: 15 * time.Second}
	resp, err := cli.Get(server + "/api/config")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("server non raggiungibile o /api/config assente")
	}
	var r quick.ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &cliConfig{
		OAuthClientID:     r.OAuthClientID,
		OAuthClientSecret: r.OAuthClientSecret,
		HostedDomain:      r.HostedDomain,
		BaseDomain:        r.BaseDomain,
	}, nil
}
