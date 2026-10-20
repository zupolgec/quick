package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/zupolgec/quick/internal/quick"
)

func domainsCmd(args []string) {
	sub := "list"
	if len(args) > 0 {
		switch args[0] {
		case "add", "list", "ls", "remove", "rm":
			sub, args = args[0], args[1:]
		}
	}
	p1, args := firstPos(args)
	p2, args := firstPos(args)
	fs := flag.NewFlagSet("domains "+sub, flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	auth := fs.String("token", os.Getenv("QUICK_TOKEN"), "login ID token (default: saved login)")
	fs.Parse(args)
	pos := positionals([]string{p1, p2}, fs.Args())
	var name, domain string
	if sub == "list" || sub == "ls" {
		if len(pos) > 1 {
			domainsUsage()
		}
		if len(pos) == 1 {
			name = pos[0]
		}
	} else {
		name, domain = siteAndArg(pos)
	}
	if sub != "list" && sub != "ls" && domain == "" {
		fatal(errors.New("missing domain"))
	}
	name, cfg, tok := tokenConfigAndAuth(name, *server, *auth)
	base := cfg.Server + "/api/site/" + url.PathEscape(name) + "/domains"

	var req *http.Request
	var err error
	switch sub {
	case "add":
		body, _ := json.Marshal(quick.DomainRequest{Domain: domain})
		req, err = http.NewRequest(http.MethodPost, base, bytes.NewReader(body))
		fatal(err)
		req.Header.Set("Content-Type", "application/json")
	case "remove", "rm":
		req, err = http.NewRequest(http.MethodDelete, base+"/"+url.PathEscape(domain), nil)
		fatal(err)
	default:
		req, err = http.NewRequest(http.MethodGet, base, nil)
		fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	var res quick.DomainsResponse
	json.Unmarshal(doTokenRequest(req, sub+" domain"), &res)

	switch sub {
	case "add":
		fmt.Printf("%s %s attached to %s\n", check(), cBold(domain), cBold(name))
	case "remove", "rm":
		fmt.Printf("%s %s removed from %s\n", check(), cBold(domain), cBold(name))
		return
	}
	printDomains(name, res.Domains)
}

func printDomains(site string, ds []quick.DomainInfo) {
	if len(ds) == 0 {
		fmt.Printf("No custom domains for %s\n", site)
		return
	}
	pending := false
	for _, d := range ds {
		if d.Active {
			fmt.Printf("  %s  %s\n", cCyan("https://"+d.Domain), "active")
			continue
		}
		pending = true
		fmt.Printf("  %s  waiting for DNS. Create this record (DNS only, no proxy):\n", cBold(d.Domain))
		for _, r := range d.Records {
			fmt.Printf("    %-5s %s → %s\n", r.Type, r.Name, r.Value)
		}
	}
	if pending {
		fmt.Println("  Then run `quick domains " + site + "` or just open the domain: it activates once DNS points here.")
	}
	fmt.Println("  Sites with company sign-in send custom-domain visitors to their regular address.")
}

func domainsUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  quick domains        <site>
  quick domains add    <site> <domain>
  quick domains rm     <site> <domain>`)
	os.Exit(2)
}
