// Command quick is the CLI for quick: self-hostable static hosting with SSO.
// The server is given via --server or QUICK_SERVER; everything else
// auto-configures from GET <server>/api/config.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

// httpClient: control calls to the server, with a total timeout so the CLI
// never hangs if the server doesn't respond.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// deployClient: tar upload. No total timeout (a large tar on a slow link takes
// time); only the blocking points are bounded — connect, TLS, response headers —
// without capping the transfer.
var deployClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}

func main() {
	if len(os.Args) < 2 {
		overview() // bare `quick`: overview + help, never an error
		return
	}
	switch os.Args[1] {
	case "help", "--help", "-h":
		printUsage(os.Stdout) // explicit help: stdout, exit 0
	case "version", "--version", "-v":
		printVersion()
	case "status":
		statusCmd(os.Args[2:])
	case "ignore":
		ignoreCmd(os.Args[2:])
	case "skill":
		skillCmd(os.Args[2:])
	case "token", "tokens":
		tokenCmd(os.Args[2:])
	case "login":
		fs := flag.NewFlagSet("login", flag.ExitOnError)
		server := fs.String("server", "", "server URL (or QUICK_SERVER)")
		fs.Parse(os.Args[2:])
		cfg, err := resolveConfig(*server)
		fatal(err)
		if _, err := login(cfg); err != nil {
			fatal(err)
		}
		fmt.Println("✓ logged in")
	case "deploy":
		deploy(os.Args[2:])
	case "rollback":
		rollbackCmd(os.Args[2:])
	case "upgrade", "self-update":
		upgradeCmd(os.Args[2:])
	case "delete", "rm":
		deleteCmd(os.Args[2:])
	case "publish", "unpublish", "private", "lock", "unlock":
		policyCmd(os.Args[1], os.Args[2:])
	default:
		usage()
	}
}

func printVersion() {
	rev := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				rev = s.Value
			}
		}
		if version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" {
		fmt.Printf("quick %s (%s)\n", version, rev)
	} else {
		fmt.Printf("quick %s\n", version)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `usage (server via --server or QUICK_SERVER):
  quick                             # overview + this help
  quick status                      # status: server, site, visibility, deploy
  quick login                       # Google login (once)
  quick deploy [<site>] [folder]    # publish a folder (default: current)
  quick rollback  <site>            # restore the previous version
  quick ignore  [folder]            # create an editable .quickignore
  quick skill   [--target codex|gemini|…] [--project] [--all]  # publish the Agent Skill (SKILL.md)
  quick delete    <site>            # delete the site (irreversible)
  quick publish   <site>            # open to the public (no SSO)
  quick unpublish <site>            # back behind company SSO
  quick private   <site> [--code X] # access by code (generated if absent)
  quick lock      <site>            # only you can overwrite it
  quick unlock    <site>
  quick token create|list|revoke <site>
  quick upgrade   [--check]          # update the CLI to the latest version
  quick version`)
}

// usage prints usage to stderr and exits with an error (unknown command).
func usage() {
	printUsage(os.Stderr)
	os.Exit(2)
}

// overview is what bare `quick` shows: a context line (no network, no prompt)
// then the command list. Full state lives in `quick status`.
func overview() {
	if cfg := loadConfig(); cfg != nil && cfg.Server != "" {
		auth := "not authenticated (run `quick login`)"
		if haveLogin() {
			auth = "authenticated"
		}
		fmt.Printf("Server: %s — %s\n", cfg.Server, auth)
		if sf := loadSiteFile("."); sf != nil {
			fmt.Printf("Folder linked to site: %s\n", sf.Name)
		}
		fmt.Println()
	}
	printUsage(os.Stdout)
}

func deploy(args []string) {
	// positionals [<site>] [folder]; stop at the first flag.
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") && len(pos) < 2 {
		pos = append(pos, args[0])
		args = args[1:]
	}

	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	server := fs.String("server", "", "server URL (or QUICK_SERVER)")
	token := fs.String("token", envToken(), "Google ID token or Quick deploy token")
	public := fs.Bool("public", false, "make the site public (no SSO)")
	private := fs.String("private", "", "make the site private with this code (--private= empty = generated)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	dryRun := fs.Bool("dry-run", false, "show what would be published without doing it")
	force := fs.Bool("force", false, "proceed even if there are no files to publish")
	fs.Parse(args)
	// flag.Parse stops at the first positional; recover those placed after the
	// flags, else `quick deploy --server X site ./build` would silently drop them.
	pos = append(pos, fs.Args()...)

	posSite, posDir := "", ""
	if len(pos) >= 1 {
		posSite = pos[0]
	}
	if len(pos) >= 2 {
		posDir = pos[1]
	}

	privateSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "private" {
			privateSet = true
		}
	})
	if *public && privateSet {
		fatal(fmt.Errorf("--public and --private are mutually exclusive"))
	}

	// .quick lives in the current folder, not in the deployed one: the project
	// root is stable, ./build is ephemeral.
	sf := loadSiteFile(".")

	// folder to publish: positional > dir remembered in .quick > current.
	dir := "."
	switch {
	case posDir != "":
		dir = posDir
	case sf != nil && sf.Dir != "":
		dir = sf.Dir
	}

	// site name: positional > .quick > folder name.
	siteName := posSite
	if siteName == "" && sf != nil {
		siteName = sf.Name
	}
	if siteName == "" {
		abs, _ := filepath.Abs(dir)
		siteName = filepath.Base(abs)
	}
	name := &siteName
	if !quick.ValidName(*name) {
		// likely argument-order mistake: folder passed as the first argument.
		if posSite != "" && posDir == "" && looksLikePath(posSite) {
			fatal(fmt.Errorf("%q looks like a folder: the syntax is `quick deploy <site> [folder]` (site first)", posSite))
		}
		fatal(fmt.Errorf("invalid site name %q (use a-z, 0-9, hyphen)", *name))
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fatal(fmt.Errorf("%q is not a folder", dir))
	}

	pl, err := buildPlan(dir)
	fatal(err)

	if *dryRun {
		printPlan(*name, pl)
		return
	}

	// empty-deploy guard: with no files the mirror would wipe the site.
	if len(pl.files) == 0 && !*force {
		fatal(fmt.Errorf("no files to publish in %q (%d excluded). Use --force to empty the site anyway", dir, pl.excluded))
	}

	if !confirmSiteMismatch(sf, *name, "deploy to") {
		return
	}

	srv := *server
	if srv == "" && sf != nil {
		srv = sf.Server
	}
	cfg, err := resolveConfig(srv)
	fatal(err)

	// authenticate now: the identity is needed for the "last deploy" confirmation.
	tok := *token
	if tok == "" {
		if tok, err = idToken(cfg); err != nil {
			fatal(err)
		}
	}
	me := emailFromToken(tok)
	if me != "" {
		fmt.Printf("%s Authenticated as %s\n", check(), cCyan(localPart(me)))
	}

	// Existence is known from the policy: it tells a first publish apart from a
	// replace, and drives the reinforced "last deploy wasn't yours" confirmation
	// (Shopify style: retype the name before overwriting someone else's work).
	exists := false
	if !*yes {
		if pol, ok := getPolicy(cfg, *name, tok); ok {
			exists = pol.Exists
			if pol.Exists && pol.UpdatedBy != "" && pol.UpdatedBy != me {
				if !confirmOverwrite(*name, pol.UpdatedBy) {
					fmt.Fprintln(os.Stderr, "cancelled")
					return
				}
			}
		}
	}

	// Summary + confirmation: replacing an existing site is destructive.
	if !confirmDeploy(*name, cfg, pl, exists, *yes) {
		fmt.Fprintln(os.Stderr, "cancelled")
		return
	}

	payload, err := tarGzFromPlan(dir, pl)
	fatal(err)

	endpoint := cfg.Server + "/api/deploy?name=" + url.QueryEscape(*name)
	req, err := http.NewRequest(http.MethodPost, endpoint, payload)
	fatal(err)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := deployClient.Do(req)
	fatal(err)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "deploy failed (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(respBody)))
		os.Exit(1)
	}

	var res quick.DeployResponse
	json.Unmarshal(respBody, &res)
	fmt.Printf("%s %s published → %s\n", check(), cBold(*name), cCyan(res.URL))
	relDir := filepath.ToSlash(filepath.Clean(dir))
	if relDir == "." {
		relDir = ""
	}
	saveSiteFile(".", siteFile{Name: *name, Server: cfg.Server, Dir: relDir})

	// optional visibility applied right after the deploy.
	switch {
	case *public:
		callPolicy(cfg, *name, tok, quick.PolicyRequest{Access: new(quick.AccessPublic)})
		fmt.Println("  → public (no SSO)")
	case privateSet:
		code := *private
		if code == "" {
			code = genCode()
		}
		callPolicy(cfg, *name, tok, quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code})
		fmt.Printf("  → private, code: %s\n", code)
	}
}

func tarGz(dir string) (*bytes.Buffer, error) {
	p, err := buildPlan(dir)
	if err != nil {
		return nil, err
	}
	return tarGzFromPlan(dir, p)
}

func tarGzFromPlan(dir string, p *plan) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, pf := range p.files {
		path := filepath.Join(dir, filepath.FromSlash(pf.rel))
		fi, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return nil, err
		}
		hdr.Name = pf.rel
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// looksLikePath reports whether the argument looks like a folder rather than a
// site name, for a clearer error when the order is swapped.
func looksLikePath(s string) bool {
	if strings.ContainsAny(s, "/\\.") {
		return true
	}
	fi, err := os.Stat(s)
	return err == nil && fi.IsDir()
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
