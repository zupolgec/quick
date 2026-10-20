package main

import (
	"bytes"
	"context"
	"crypto/rand"
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

func policyCmd(action string, args []string) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet(action, flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	token := fs.String("token", os.Getenv("QUICK_TOKEN"), "login ID token (default: saved login)")
	var code string
	if action == "private" {
		fs.StringVar(&code, "code", "", "access code (if empty, generated)")
	}
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
	if !confirmSiteMismatch(sf, name, "modify") {
		return
	}

	payload := quick.PolicyRequest{}
	switch action {
	case "publish":
		payload.Access = new(quick.AccessPublic)
	case "unpublish":
		payload.Access = new("sso")
	case "private":
		if code == "" {
			code = genCode()
		}
		payload.Access, payload.Code = new(quick.AccessCode), new(code)
	case "lock":
		payload.Locked = new(true)
	case "unlock":
		payload.Locked = new(false)
	}

	cfg, ok := resolveSiteConfig(*server, sf, name, "modify")
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

	res := callPolicy(cfg, name, tok, payload)
	url := "https://" + res.Site + "." + cfg.BaseDomain
	switch action {
	case "private":
		fmt.Printf("✓ %s protected by code → %s\n  code: %s\n", name, url, code)
	case "publish":
		fmt.Printf("✓ %s public → %s\n", name, url)
	case "unpublish":
		fmt.Printf("✓ %s back behind SSO → %s\n", name, url)
	case "lock":
		fmt.Printf("✓ %s locked (only %s can overwrite it)\n", name, res.Owner)
	case "unlock":
		fmt.Printf("✓ %s unlocked\n", name)
	}
}

func callPolicy(cfg *cliConfig, name, tok string, payload quick.PolicyRequest) quick.PolicyResponse {
	res, err := postPolicy(context.Background(), cfg, name, tok, payload)
	fatal(err)
	return res
}

// postPolicy changes the site's policy; ctx cancels it (interrupts).
func postPolicy(ctx context.Context, cfg *cliConfig, name, tok string, payload quick.PolicyRequest) (quick.PolicyResponse, error) {
	var res quick.PolicyResponse
	body, _ := json.Marshal(payload)
	endpoint := cfg.Server + "/api/site/" + url.PathEscape(name) + "/policy"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := httpClient.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	json.Unmarshal(rb, &res)
	return res, nil
}

// genCode creates a short, readable access code (no ambiguous characters).
func genCode() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
