package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/zupolgec/quick/internal/quick"
)

// limitCmd: quick limit [<site>] shows the site's size limit;
// quick limit [<site>] <size>|default changes it (administrators).
func limitCmd(args []string) {
	p1, args := firstPos(args)
	p2, args := firstPos(args)
	fs := flag.NewFlagSet("limit", flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	auth := fs.String("token", os.Getenv("QUICK_TOKEN"), "login ID token (default: saved login)")
	fs.Parse(args)
	pos := positionals([]string{p1, p2}, fs.Args())
	if len(pos) > 2 {
		usage()
	}
	name, size := limitArgs(pos)
	var req quick.LimitRequest
	if size != "" && !strings.EqualFold(size, "default") {
		n, err := quick.ParseSize(size)
		fatal(err)
		req.SizeLimit = n
	}
	name, cfg, tok := tokenConfigAndAuth(name, *server, *auth)
	endpoint := cfg.Server + "/api/site/" + url.PathEscape(name) + "/limit"

	method, action := http.MethodGet, "read size limit"
	var body []byte
	if size != "" {
		method, action = http.MethodPost, "change size limit"
		body, _ = json.Marshal(req)
	}
	r, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	fatal(err)
	r.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	var res quick.LimitResponse
	fatal(json.Unmarshal(doTokenRequest(r, action), &res))

	which := "default"
	if res.Custom {
		which = "raised by an administrator"
	}
	if size != "" {
		fmt.Printf("%s %s: size limit %s (%s)\n", check(), cBold(name), cBold(quick.FormatSize(res.SizeLimit)), which)
		return
	}
	fmt.Printf("%s: size limit %s (%s)\n", cBold(name), quick.FormatSize(res.SizeLimit), which)
	if res.Admin {
		fmt.Printf("  you can change it up to %s: quick limit %s <size>|default\n", quick.FormatSize(res.Max), name)
	} else if len(res.Admins) > 0 {
		fmt.Printf("  to raise it, ask an administrator: %s\n", strings.Join(res.Admins, ", "))
	} else {
		fmt.Println("  to raise it, ask your system administrator")
	}
}

// limitArgs splits [<site>] [<size>|default]: a single positional is the size
// when it reads as one, else the site.
func limitArgs(pos []string) (site, size string) {
	switch len(pos) {
	case 0:
		return "", ""
	case 1:
		if _, err := quick.ParseSize(pos[0]); err == nil || strings.EqualFold(pos[0], "default") {
			return "", pos[0]
		}
		return pos[0], ""
	}
	return pos[0], pos[1]
}
