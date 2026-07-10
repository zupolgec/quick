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

func envToken() string {
	if v := os.Getenv("QUICK_API_TOKEN"); v != "" {
		return v
	}
	return os.Getenv("QUICK_TOKEN")
}

func tokenCmd(args []string) {
	if len(args) == 0 {
		tokenUsage()
	}
	switch args[0] {
	case "create":
		tokenCreateCmd(args[1:])
	case "list", "ls":
		tokenListCmd(args[1:])
	case "revoke", "rm":
		tokenRevokeCmd(args[1:])
	default:
		tokenUsage()
	}
}

func tokenUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  quick token create <site> --name github-actions [--expires 90d]
  quick token list   <site>
  quick token revoke <site> <token-id>`)
	os.Exit(2)
}

func tokenCreateCmd(args []string) {
	name, args := firstPos(args)
	fs := flag.NewFlagSet("token create", flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	auth := fs.String("token", os.Getenv("QUICK_TOKEN"), "Google ID token (default: saved login)")
	label := fs.String("name", "deploy", "token label")
	expires := fs.String("expires", "90d", "30d, 90d, 180d, 365d, or never")
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	name, cfg, tok := tokenConfigAndAuth(name, *server, *auth)
	req := quick.TokenCreateRequest{Name: *label, Scopes: []string{quick.TokenScopeDeploy}, ExpiresIn: *expires}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest(http.MethodPost, cfg.Server+"/api/site/"+url.PathEscape(name)+"/tokens", bytes.NewReader(body))
	fatal(err)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+tok)
	respBody := doTokenRequest(httpReq, "create token")
	var res quick.TokenCreateResponse
	json.Unmarshal(respBody, &res)
	fmt.Printf("%s token created for %s\n", check(), cBold(name))
	fmt.Printf("  id: %s\n", res.Info.ID)
	fmt.Printf("  token: %s\n", res.Token)
	fmt.Println("  Store it as QUICK_API_TOKEN. It will not be shown again.")
}

func tokenListCmd(args []string) {
	name, args := firstPos(args)
	fs := flag.NewFlagSet("token list", flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	auth := fs.String("token", os.Getenv("QUICK_TOKEN"), "Google ID token (default: saved login)")
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	name, cfg, tok := tokenConfigAndAuth(name, *server, *auth)
	req, err := http.NewRequest(http.MethodGet, cfg.Server+"/api/site/"+url.PathEscape(name)+"/tokens", nil)
	fatal(err)
	req.Header.Set("Authorization", "Bearer "+tok)
	respBody := doTokenRequest(req, "list tokens")
	var res quick.TokensResponse
	json.Unmarshal(respBody, &res)
	if len(res.Tokens) == 0 {
		fmt.Printf("No deploy tokens for %s\n", name)
		return
	}
	for _, t := range res.Tokens {
		exp := t.ExpiresAt
		if exp == "" {
			exp = "never"
		}
		last := t.LastUsedAt
		if last == "" {
			last = "never used"
		}
		fmt.Printf("%s  %s  scope=%s  expires=%s  last=%s\n", t.ID, t.Name, strings.Join(t.Scopes, ","), exp, last)
	}
}

func tokenRevokeCmd(args []string) {
	name, args := firstPos(args)
	tokenID, args := firstPos(args)
	fs := flag.NewFlagSet("token revoke", flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	auth := fs.String("token", os.Getenv("QUICK_TOKEN"), "Google ID token (default: saved login)")
	fs.Parse(args)
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if tokenID == "" && fs.NArg() > 1 {
		tokenID = fs.Arg(1)
	}
	if tokenID == "" {
		fatal(errors.New("missing token id"))
	}
	name, cfg, tok := tokenConfigAndAuth(name, *server, *auth)
	req, err := http.NewRequest(http.MethodDelete, cfg.Server+"/api/site/"+url.PathEscape(name)+"/tokens/"+url.PathEscape(tokenID), nil)
	fatal(err)
	req.Header.Set("Authorization", "Bearer "+tok)
	doTokenRequest(req, "revoke token")
	fmt.Printf("%s token %s revoked for %s\n", check(), tokenID, cBold(name))
}

func tokenConfigAndAuth(name, server, tok string) (string, *cliConfig, string) {
	if name == "" {
		if sf := loadSiteFile("."); sf != nil {
			name = sf.Name
			if server == "" {
				server = sf.Server
			}
		}
	}
	if name == "" {
		fatal(errors.New("missing site name (or run inside a folder with a .quick file)"))
	}
	cfg, err := resolveConfig(server)
	fatal(err)
	if tok == "" {
		if tok, err = idToken(cfg); err != nil {
			fatal(err)
		}
	}
	return name, cfg, tok
}

func firstPos(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func doTokenRequest(req *http.Request, action string) []byte {
	resp, err := httpClient.Do(req)
	fatal(err)
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "%s failed (%d): %s\n", action, resp.StatusCode, strings.TrimSpace(string(rb)))
		os.Exit(1)
	}
	return rb
}
