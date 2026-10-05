package main

import (
	"bytes"
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

func rollbackCmd(args []string) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	token := fs.String("token", os.Getenv("QUICK_TOKEN"), "login ID token (default: saved login)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
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
	if !confirmSiteMismatch(sf, name, "restore") {
		return
	}
	if !*yes {
		fmt.Fprintf(os.Stderr, "  Roll %s back to the previous version? [y/N]: ", name)
		if !yesNo(readLine()) {
			fmt.Fprintln(os.Stderr, "cancelled")
			return
		}
	}

	cfg, ok := resolveSiteConfig(*server, sf, name, "restore")
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

	endpoint := cfg.Server + "/api/site/" + url.PathEscape(name) + "/rollback"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(nil))
	fatal(err)
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := httpClient.Do(req)
	fatal(err)
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "rollback failed (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(rb)))
		os.Exit(1)
	}
	var res quick.RollbackResponse
	json.Unmarshal(rb, &res)
	fmt.Printf("%s %s restored to the previous version → %s\n", check(), cBold(name), cCyan(res.URL))
}
