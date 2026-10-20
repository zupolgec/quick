package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zupolgec/quick/internal/quick"
)

// accessArgs is a parsed `quick access` command line.
type accessArgs struct {
	Name   string
	Op     string // "" = show | team | public | code | only-me | add | remove
	Emails []string
	Code   string
	Server string
	Token  string
}

// parseAccessArgs parses `quick access [<site>] [level | add|remove <email>...]`
// with flags anywhere on the line.
func parseAccessArgs(args []string) (accessArgs, error) {
	var a accessArgs
	fs := flag.NewFlagSet("access", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.Server, "server", "", "server URL (or QUICK_SERVER)")
	fs.StringVar(&a.Token, "token", "", "login ID token (default: saved login)")
	fs.StringVar(&a.Code, "code", "", "access code for `code` (if empty, generated)")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return a, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) > 0 {
		a.Name, pos = pos[0], pos[1:]
	}
	if len(pos) > 0 {
		a.Op, pos = pos[0], pos[1:]
	}
	switch a.Op {
	case "", "team", "public", "code", "only-me":
		if len(pos) > 0 {
			return a, fmt.Errorf("unexpected argument %q", pos[0])
		}
	case "add", "remove":
		for _, p := range pos {
			a.Emails = append(a.Emails, quick.SplitList(p)...)
		}
		if len(a.Emails) == 0 {
			return a, fmt.Errorf("quick access <site> %s <email>...: missing email", a.Op)
		}
	default:
		return a, fmt.Errorf("unknown access %q (use team, public, code, only-me, add or remove)", a.Op)
	}
	if a.Code != "" && a.Op != "code" {
		return a, errors.New("--code only goes with `code`")
	}
	return a, nil
}

// accessRequest is the policy change for a parsed command (not for show).
func accessRequest(a accessArgs) quick.PolicyRequest {
	var req quick.PolicyRequest
	switch a.Op {
	case "team":
		req.Access = new("sso")
	case "public":
		req.Access = new(quick.AccessPublic)
	case "code":
		req.Access, req.Code = new(quick.AccessCode), new(a.Code)
	case "only-me":
		req.Access, req.Viewers = new(quick.AccessPeople), &[]string{}
	case "add":
		req.AddViewers = a.Emails
	case "remove":
		req.RemoveViewers = a.Emails
	}
	return req
}

// accessCall reads (no Op) or changes the site's access.
func accessCall(cfg *cliConfig, a accessArgs, tok string) (quick.PolicyResponse, error) {
	if a.Op == "" {
		res, ok := getPolicy(cfg, a.Name, tok)
		if !ok {
			return res, errors.New("could not read the site's access (are you logged in? run `quick login`)")
		}
		return res, nil
	}
	return postPolicy(context.Background(), cfg, a.Name, tok, accessRequest(a))
}

// describeSiteAccess renders who can see the site, for `quick access <site>`.
func describeSiteAccess(res quick.PolicyResponse) string {
	var b strings.Builder
	level := "everyone at the company (company SSO)"
	switch res.Access {
	case quick.AccessPublic:
		level = "public (no SSO)"
	case quick.AccessCode:
		level = "anyone with the access code"
	case quick.AccessPeople:
		switch {
		case res.ViewersHidden:
			level = "chosen people (you're not on the list)"
		case len(res.Viewers) == 0:
			level = "only the owner"
		default:
			level = fmt.Sprintf("chosen people (%d) and the owner", len(res.Viewers))
		}
	}
	fmt.Fprintf(&b, "Access:  %s\n", level)
	if res.CreatedBy != "" {
		fmt.Fprintf(&b, "Owner:   %s\n", res.CreatedBy)
	}
	for i, v := range res.Viewers {
		if i == 0 {
			fmt.Fprintf(&b, "Viewers: %s\n", v)
		} else {
			fmt.Fprintf(&b, "         %s\n", v)
		}
	}
	if res.Locked {
		fmt.Fprintf(&b, "Lock:    locked by %s\n", res.Owner)
	}
	return b.String()
}

func accessCmd(args []string) {
	a, err := parseAccessArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "usage: quick access <site> [team | public | code [--code X] | only-me | add <email>... | remove <email>...]")
		os.Exit(2)
	}
	sf := loadSiteFile(".")
	if a.Name == "" && sf != nil {
		a.Name = sf.Name
	}
	if a.Name == "" {
		fatal(errors.New("missing site name (or run inside a folder with a .quick file)"))
	}
	if !quick.ValidName(a.Name) {
		fatal(fmt.Errorf("invalid site name %q", a.Name))
	}
	if a.Op != "" && !confirmSiteMismatch(sf, a.Name, "modify") {
		return
	}
	if a.Op == "code" && a.Code == "" {
		a.Code = genCode()
	}
	verb := "modify"
	if a.Op == "" {
		verb = "read"
	}
	cfg, ok := resolveSiteConfig(a.Server, sf, a.Name, verb)
	if !ok {
		return
	}
	tok := a.Token
	if tok == "" {
		tok = os.Getenv("QUICK_TOKEN")
	}
	if tok == "" {
		if tok, err = idToken(cfg); err != nil {
			fatal(err)
		}
	}
	res, err := accessCall(cfg, a, tok)
	fatal(err)
	url := "https://" + a.Name + "." + cfg.BaseDomain
	switch a.Op {
	case "":
		fmt.Printf("Site:    %s → %s\n", a.Name, url)
		fmt.Print(describeSiteAccess(res))
	case "team":
		fmt.Printf("✓ %s visible to everyone at the company → %s\n", a.Name, url)
	case "public":
		fmt.Printf("✓ %s public → %s\n", a.Name, url)
	case "code":
		fmt.Printf("✓ %s protected by code → %s\n  code: %s\n", a.Name, url, a.Code)
	default: // only-me, add, remove
		if len(res.Viewers) == 0 {
			fmt.Printf("✓ %s visible only to you → %s\n", a.Name, url)
		} else {
			fmt.Printf("✓ %s visible to you and: %s → %s\n", a.Name, strings.Join(res.Viewers, ", "), url)
		}
	}
}
