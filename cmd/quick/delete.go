package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/zupolgec/quick/internal/quick"
)

func deleteCmd(args []string) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	token := fs.String("token", os.Getenv("QUICK_TOKEN"), "login ID token (default: saved login)")
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0) // positional placed after the flags
	}

	sf := loadSiteFile(".")
	if name == "" && sf != nil {
		name = sf.Name
	}
	if name == "" {
		fatal(errors.New("missing site name (or run inside a folder with a .quick file)"))
	}
	if !confirmSiteMismatch(sf, name, "delete") {
		return
	}

	cfg, ok := resolveSiteConfig(*server, sf, name, "delete")
	if !ok {
		return
	}

	var err error
	tok := *token
	if tok == "" {
		if tok, err = idToken(cfg); err != nil {
			fatal(err)
		}
	}

	// current state drives the confirmation level (exists, locked, access)
	pol := fetchPolicy(cfg, name, tok)
	if !pol.Exists {
		fatal(fmt.Errorf("site %q not found", name))
	}

	siteURL := "https://" + name + "." + cfg.BaseDomain
	fmt.Fprintf(os.Stderr, "\n  ⚠️  you are about to permanently DELETE the site\n      %s  (%s)\n", name, siteURL)
	if pol.Locked {
		fmt.Fprintf(os.Stderr, "      locked by %s\n", pol.Owner)
	}
	fmt.Fprintf(os.Stderr, "      the files are removed and the operation is not reversible.\n\n")

	if pol.Access == quick.AccessPublic || pol.Access == quick.AccessCode {
		label := "public"
		if pol.Access == quick.AccessCode {
			label = "protected by code"
		}
		fmt.Fprintf(os.Stderr, "  The site is %s. To confirm, retype its name (%s): ", label, name)
		if readLine() != name {
			fatal(errors.New("name does not match: deletion cancelled"))
		}
	} else {
		fmt.Fprint(os.Stderr, "  Confirm deletion? [y/N]: ")
		if !yesNo(readLine()) {
			fmt.Fprintln(os.Stderr, "cancelled")
			return
		}
	}

	callDelete(cfg, name, tok)
	fmt.Printf("✓ %s deleted\n", name)
}

func fetchPolicy(cfg *cliConfig, name, tok string) quick.PolicyResponse {
	endpoint := cfg.Server + "/api/site/" + url.PathEscape(name) + "/policy"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	fatal(err)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := httpClient.Do(req)
	fatal(err)
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "cannot read status (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(rb)))
		os.Exit(1)
	}
	var p quick.PolicyResponse
	json.Unmarshal(rb, &p)
	return p
}

func callDelete(cfg *cliConfig, name, tok string) {
	endpoint := cfg.Server + "/api/site/" + url.PathEscape(name)
	req, err := http.NewRequest(http.MethodDelete, endpoint, nil)
	fatal(err)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := httpClient.Do(req)
	fatal(err)
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "deletion failed (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(rb)))
		os.Exit(1)
	}
}
