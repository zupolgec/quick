// quick status (anche `quick` da solo): riassume server, autenticazione, sito
// della cartella corrente, visibilità sul server e cosa salirebbe col deploy.
// quick ignore: materializza un .quickignore modificabile nella cartella.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/wayexperience/quick/internal/quick"
)

func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	server := fs.String("server", "", "URL del server (o QUICK_SERVER)")
	fs.Parse(args)

	dir := "."
	sf := loadSiteFile(dir)
	name := ""
	if sf != nil {
		name = sf.Name
	}
	if name == "" {
		abs, _ := filepath.Abs(dir)
		name = filepath.Base(abs)
	}

	srv := *server
	if srv == "" && sf != nil {
		srv = sf.Server
	}
	cfg, err := resolveConfig(srv)
	fatal(err)

	fmt.Printf("Server:  %s\n", cfg.Server)
	tok, logged := silentToken(cfg)
	if logged {
		fmt.Println("Accesso: autenticato")
	} else {
		fmt.Println("Accesso: non autenticato (esegui `quick login`)")
	}

	fmt.Printf("Sito:    %s  → https://%s.%s\n", name, name, cfg.BaseDomain)

	// Visibilità reale dal server (serve un token; con sito inesistente lo dice).
	if logged && quick.ValidName(name) {
		if pol, ok := getPolicy(cfg, name, tok); ok {
			if !pol.Exists {
				fmt.Println("Stato:   non ancora pubblicato")
			} else {
				fmt.Printf("Stato:   %s\n", describeAccess(pol.Access))
				if pol.Locked {
					fmt.Printf("Lock:    bloccato da %s\n", pol.Owner)
				}
			}
		}
	}

	// Cosa salirebbe col deploy dalla cartella corrente.
	if pl, err := buildPlan(dir); err == nil {
		fmt.Printf("Deploy:  %d file, %s (esclusioni: %s", len(pl.files), humanSize(pl.totalSize), pl.ignoreSource())
		if pl.excluded > 0 {
			fmt.Printf(", %d esclusi", pl.excluded)
		}
		fmt.Println(")")
	}
}

// describeAccess traduce il valore di policy in una frase per l'utente.
func describeAccess(access string) string {
	switch access {
	case quick.AccessPublic:
		return "pubblico (niente SSO)"
	case quick.AccessCode:
		return "privato, accesso con codice"
	default:
		return "dietro SSO aziendale"
	}
}

// getPolicy legge la policy corrente del sito (GET). ok=false se la richiesta
// fallisce (es. token scaduto): in quel caso lo status omette la visibilità.
func getPolicy(cfg *cliConfig, name, tok string) (quick.PolicyResponse, bool) {
	endpoint := cfg.Server + "/api/site/" + url.PathEscape(name) + "/policy"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return quick.PolicyResponse{}, false
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return quick.PolicyResponse{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return quick.PolicyResponse{}, false
	}
	body, _ := io.ReadAll(resp.Body)
	var pol quick.PolicyResponse
	if json.Unmarshal(body, &pol) != nil {
		return quick.PolicyResponse{}, false
	}
	return pol, true
}

func ignoreCmd(args []string) {
	dir := "."
	if len(args) > 0 && args[0] != "" {
		dir = args[0]
	}
	path, err := writeQuickignore(dir)
	fatal(err)
	if path == "" {
		fmt.Printf("%s esiste già: lo lascio com'è.\n", filepath.Join(dir, quickignoreName))
		return
	}
	fmt.Printf("✓ scritto %s\n", path)
	fmt.Println("  Modificalo per decidere cosa NON pubblicare. Da ora fa fede lui.")
}
