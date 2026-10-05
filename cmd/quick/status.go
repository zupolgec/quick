package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	fs.Parse(args)

	sf := loadSiteFile(".")
	// same resolution as deploy: published folder is the one in .quick (or current).
	dir := "."
	if sf != nil && sf.Dir != "" {
		dir = sf.Dir
	}
	name := ""
	if sf != nil {
		name = sf.Name
	}
	if name == "" {
		abs, _ := filepath.Abs(dir)
		name = filepath.Base(abs)
	}

	linked := ""
	if sf != nil {
		linked = sf.Server
	}
	cfg, err := resolveConfig(*server, linked)
	fatal(err)

	fmt.Printf("Server:  %s\n", cfg.Server)
	tok, logged := silentToken(cfg)
	if logged {
		fmt.Println("Access:  authenticated")
	} else {
		fmt.Println("Access:  not authenticated (run `quick login`)")
	}

	fmt.Printf("Site:    %s  → https://%s.%s\n", name, name, cfg.BaseDomain)

	// real visibility from the server (needs a token; reports a missing site).
	if logged && quick.ValidName(name) {
		if pol, ok := getPolicy(cfg, name, tok); ok {
			if !pol.Exists {
				fmt.Println("Status:  not yet published")
			} else {
				fmt.Printf("Status:  %s\n", describeAccess(pol.Access))
				if pol.CreatedBy != "" {
					fmt.Printf("Created: %s%s\n", pol.CreatedBy, fmtWhen(pol.CreatedAt))
				}
				if pol.UpdatedBy != "" {
					fmt.Printf("Last:    %s%s\n", pol.UpdatedBy, fmtWhen(pol.UpdatedAt))
				}
				if pol.Locked {
					fmt.Printf("Lock:    locked by %s\n", pol.Owner)
				}
			}
		}
	}

	if pl, err := buildPlan(dir); err == nil {
		from := ""
		if dir != "." {
			from = " from ./" + filepath.ToSlash(dir)
		}
		fmt.Printf("Deploy:  %s, %s%s (excluded by: %s", plural(len(pl.files), "file", "files"), humanSize(pl.totalSize), from, pl.ignoreSource())
		if pl.excluded > 0 {
			fmt.Printf(", %d excluded", pl.excluded)
		}
		fmt.Println(")")
	}
}

func fmtWhen(ts string) string {
	if ts == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return " (" + t.Local().Format("2006-01-02 15:04") + ")"
	}
	return " (" + ts + ")"
}

func describeAccess(access string) string {
	switch access {
	case quick.AccessPublic:
		return "public (no SSO)"
	case quick.AccessCode:
		return "private, access by code"
	default:
		return "behind company SSO"
	}
}

// getPolicy reads the site's current policy (GET). ok=false if the request
// fails (e.g. expired token), in which case status omits the visibility.
func getPolicy(cfg *cliConfig, name, tok string) (quick.PolicyResponse, bool) {
	endpoint := cfg.Server + "/api/site/" + url.PathEscape(name) + "/policy"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return quick.PolicyResponse{}, false
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := httpClient.Do(req)
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
		fmt.Printf("%s already exists: leaving it as is.\n", filepath.Join(dir, quickignoreName))
		return
	}
	fmt.Printf("✓ wrote %s\n", path)
	fmt.Println("  Edit it to decide what NOT to publish. From now on it is the source of truth.")
}
