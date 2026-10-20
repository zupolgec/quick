// Command quick is the CLI for quick: self-hostable static hosting with SSO.
// The server is given via --server or QUICK_SERVER; everything else
// auto-configures from GET <server>/api/config.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
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
	default:
		refreshInstalledSkillsSilent()
		runCmd(os.Args[1], os.Args[2:])
	}
}

func runCmd(cmd string, args []string) {
	switch cmd {
	case "status":
		statusCmd(args)
	case "ignore":
		ignoreCmd(args)
	case "skill":
		skillCmd(args)
	case "token", "tokens":
		tokenCmd(args)
	case "domains", "domain":
		domainsCmd(args)
	case "login":
		fs := flag.NewFlagSet("login", flag.ExitOnError)
		server := fs.String("server", "", "server URL (or QUICK_SERVER)")
		fs.Parse(args)
		cfg, err := resolveConfig(*server, "")
		fatal(err)
		if _, err := login(cfg); err != nil {
			fatal(err)
		}
		saveServerConfig(cfg, true) // the server you log in to becomes the default
		fmt.Println("✓ logged in to " + cfg.Server)
	case "servers", "server":
		serversCmd(args)
	case "deploy":
		deploy(args)
	case "rollback":
		rollbackCmd(args)
	case "upgrade", "self-update":
		upgradeCmd(args)
	case "delete", "rm":
		deleteCmd(args)
	case "publish", "unpublish", "private", "lock", "unlock":
		policyCmd(cmd, args)
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
	fmt.Fprintln(w, `usage (server via --server or QUICK_SERVER; a known server also by short name, e.g. --server acme):
  quick                             # overview + this help
  quick status                      # status: server, site, visibility, deploy
  quick login                       # log in (once per server; it becomes the default)
  quick servers [use <server>]      # known servers, login status; change the default
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
  quick domains [add|rm] <site> [domain]  # custom domains (www.example.com)
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
		if haveLogin(cfg.Server) {
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
	token := fs.String("token", envToken(), "login ID token or Quick deploy token")
	public := fs.Bool("public", false, "make the site public (no SSO)")
	private := fs.String("private", "", "make the site private with this code (--private= empty = generated)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	dryRun := fs.Bool("dry-run", false, "show what would be published without doing it")
	force := fs.Bool("force", false, "proceed even if there are no files to publish")
	takeover := fs.Bool("takeover", false, "publish even if someone is publishing this site right now (their deploy stops)")
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
	if len(pl.reserved) > 0 {
		fatal(errors.New(quick.ReservedPathError(pl.reserved[0])))
	}

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

	cfg, ok := resolveSiteConfig(*server, sf, *name, "deploy to")
	if !ok {
		return
	}

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

	// Delta deploy when the server supports it (only changed chunks travel);
	// otherwise, or if the server turns out not to have it, the whole tar.
	relDir := filepath.ToSlash(filepath.Clean(dir))
	if relDir == "." {
		relDir = ""
	}
	opts := deployOpts{
		takeover: *takeover, public: *public, privateSet: privateSet, private: *private,
		save: func() { saveSiteFile(".", siteFile{Name: *name, Server: cfg.Server, Dir: relDir}) },
	}
	in := catchInterrupts() // from here on Ctrl-C stops the deploy and gives the site back (lease.go)
	code := runPublish(in, cfg, *name, tok, dir, pl, opts, os.Stdout, os.Stderr)
	in.stop()
	if code != 0 {
		os.Exit(code)
	}
}

type deployOpts struct {
	takeover           bool
	public, privateSet bool
	private            string
	save               func() // record the folder's site (.quick)
}

// runPublish publishes the folder and applies the visibility asked for,
// reporting to out/errOut, and returns the exit status: 0, 1 on failure, or
// 130/143 when a signal arrived (recorded by in, honored here whenever it
// came). Any lease is released before it returns.
func runPublish(in *interrupts, cfg *cliConfig, name, tok, dir string, pl *plan, o deployOpts, out, errOut io.Writer) int {
	res, st, delta, err := publishFolder(cfg, name, tok, dir, pl, o.takeover)
	if err != nil {
		// What to say comes from the run's outcome, not from the error or
		// when a signal came: once a publish request was sent without a
		// definitive answer, the site may have been published.
		outcome := pub.get()
		if code := in.code(); code != 0 {
			fmt.Fprintln(errOut, "\n"+interruptedReport(outcome))
			return code
		}
		fmt.Fprintln(errOut, "error:", err)
		if outcome == sent {
			fmt.Fprintln(errOut, "the deploy may have been published; check the site")
		}
		return 1
	}
	if delta {
		fmt.Fprintf(out, "%s uploaded %s of %s\n", check(), humanSize(st.sent), humanSize(st.total))
	}
	fmt.Fprintf(out, "%s %s published → %s\n", check(), cBold(name), cCyan(res.URL))
	if res.Warning != "" {
		fmt.Fprintln(errOut, "warning:", res.Warning)
	}
	o.save()

	// optional visibility applied right after the deploy.
	var req *quick.PolicyRequest
	var done string
	switch {
	case o.public:
		req, done = &quick.PolicyRequest{Access: new(quick.AccessPublic)}, "  → public (no SSO)"
	case o.privateSet:
		code := o.private
		if code == "" {
			code = genCode()
		}
		req, done = &quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code}, "  → private, code: "+code
	}
	if req != nil {
		if _, err := postPolicy(in.context(), cfg, name, tok, *req); err != nil {
			if code := in.code(); code != 0 {
				fmt.Fprintln(errOut, "\npublished; visibility change interrupted")
				return code
			}
			fmt.Fprintln(errOut, "error:", err)
			return 1
		}
		fmt.Fprintln(out, done)
	}
	return in.code() // a signal after everything still counts
}

// publishFolder publishes with the delta deploy when the server supports it
// (only changed chunks travel); otherwise, or if the server turns out not to
// have it or is busy, with the whole tar. delta tells which one published.
func publishFolder(cfg *cliConfig, name, tok, dir string, pl *plan, takeover bool) (res quick.DeployResponse, st deltaStats, delta bool, err error) {
	pub.set(notSent) // a new run
	if cfg.DeltaDeploy {
		res, st, err = deltaDeploy(cfg, name, tok, dir, pl, takeover)
		switch {
		case errors.Is(err, errNoDelta):
		case errors.Is(err, errDeltaBusy):
			fmt.Println("  server busy, uploading the whole folder")
		default:
			return res, st, true, err
		}
	}
	// The lease of a delta attempt that fell back (busy server) goes along.
	if err = intr.stopped(); err == nil {
		res, err = tarDeploy(cfg, name, tok, dir, pl, takeover && st.lease == "", st.lease)
	}
	if err != nil {
		held.release() // the upload may have failed before the server got it
	} else {
		held.clear() // the server ended the lease with the tar upload
	}
	return res, st, false, err
}

// tarDeploy uploads the whole folder as one tar.gz (servers without delta
// deploy, or busy). takeover breaks another deploy's lease; lease continues a
// delta attempt's.
func tarDeploy(cfg *cliConfig, name, tok, dir string, pl *plan, takeover bool, lease string) (quick.DeployResponse, error) {
	var res quick.DeployResponse
	payload, err := tarGzFromPlan(dir, pl) // stops on an interrupt
	if err == nil {
		err = intr.stopped() // nothing sent yet: nothing published
	}
	if err != nil {
		return res, err
	}
	u := cfg.Server + "/api/deploy?name=" + url.QueryEscape(name)
	if takeover {
		u += "&takeover=1"
	}
	req, err := http.NewRequestWithContext(intr.context(), http.MethodPost, u, payload)
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+tok)
	leaseRequest(req, lease)
	code, respBody, err := send(req, true) // interrupted: errPublishStarted once sent
	if err != nil {
		return res, err
	}
	if err := lockedResponse(code, respBody); err != nil {
		return res, err
	}
	if code != http.StatusOK {
		return res, fmt.Errorf("deploy failed (%d): %s", code, strings.TrimSpace(string(respBody)))
	}
	if err := json.Unmarshal(respBody, &res); err != nil {
		return res, err // pub stays sent: may have published
	}
	pub.set(published)
	return res, nil
}

func tarGz(dir string) (*bytes.Buffer, error) {
	p, err := buildPlan(dir)
	if err != nil {
		return nil, err
	}
	return tarGzFromPlan(dir, p)
}

// openForTar opens a file to pack (test seam: slow files).
var openForTar = func(path string) (io.ReadCloser, error) { return os.Open(path) }

// stoppable stops reading (errInterrupted) once the deploy is interrupted:
// packaging a big folder doesn't go on after Ctrl-C.
type stoppable struct{ r io.Reader }

func (s stoppable) Read(p []byte) (int, error) {
	if err := intr.stopped(); err != nil {
		return 0, err
	}
	return s.r.Read(p)
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
		f, err := openForTar(path)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, stoppable{f}); err != nil {
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
