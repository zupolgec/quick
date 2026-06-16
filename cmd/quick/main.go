// quick è la CLI di way-quick (hosting statico interno, generico).
//
//	quick                                         # stato: server, sito, visibilità, deploy
//	quick login                                   # login Google (una volta)
//	quick deploy [cartella] --name <sito>         # pubblica una cartella (mirror)
//	quick ignore [cartella]                       # crea un .quickignore modificabile
//	quick publish|unpublish|private|lock|unlock <sito>
//
// Il server si indica con --server o QUICK_SERVER; il resto si auto-configura
// da GET <server>/api/config.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/wayexperience/quick/internal/quick"
)

// version è sovrascrivibile a build time con -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		statusCmd(nil) // `quick` da solo: mostra lo stato invece di un errore
		return
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		printVersion()
	case "status":
		statusCmd(os.Args[2:])
	case "ignore":
		ignoreCmd(os.Args[2:])
	case "login":
		fs := flag.NewFlagSet("login", flag.ExitOnError)
		server := fs.String("server", "", "URL del server (o QUICK_SERVER)")
		fs.Parse(os.Args[2:])
		cfg, err := resolveConfig(*server)
		fatal(err)
		if _, err := login(cfg); err != nil {
			fatal(err)
		}
		fmt.Println("✓ login eseguito")
	case "deploy":
		deploy(os.Args[2:])
	case "delete", "rm":
		deleteCmd(os.Args[2:])
	case "publish", "unpublish", "private", "lock", "unlock":
		policyCmd(os.Args[1], os.Args[2:])
	default:
		usage()
	}
}

// printVersion stampa la versione + il commit git (embeddato da `go build`/`go install`).
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

func usage() {
	fmt.Fprintln(os.Stderr, `uso (server via --server o QUICK_SERVER):
  quick                             # stato: server, sito, visibilità, deploy
  quick version
  quick login
  quick deploy [cartella] --name <sito> [--dry-run] [--yes]
  quick ignore  [cartella]          # crea un .quickignore modificabile
  quick delete    <sito>            # elimina il sito (irreversibile)
  quick publish   <sito>            # apri al pubblico (niente SSO)
  quick unpublish <sito>            # torna dietro SSO aziendale
  quick private   <sito> [--code X] # accesso con codice (generato se assente)
  quick lock      <sito>            # solo tu puoi sovrascriverlo
  quick unlock    <sito>`)
	os.Exit(2)
}

func deploy(args []string) {
	dir := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	name := fs.String("name", "", "nome del sito (sottodominio); default: nome cartella")
	server := fs.String("server", "", "URL del server (o QUICK_SERVER)")
	token := fs.String("token", os.Getenv("QUICK_TOKEN"), "ID token Google (default: login salvato)")
	public := fs.Bool("public", false, "rendi il sito pubblico (niente SSO)")
	private := fs.String("private", "", "rendi il sito privato con questo codice (--private= vuoto = generato)")
	yes := fs.Bool("yes", false, "non chiedere conferma prima di pubblicare")
	dryRun := fs.Bool("dry-run", false, "mostra cosa verrebbe pubblicato senza farlo")
	force := fs.Bool("force", false, "procedi anche se non c'è nessun file da pubblicare")
	fs.Parse(args)

	privateSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "private" {
			privateSet = true
		}
	})
	if *public && privateSet {
		fatal(fmt.Errorf("--public e --private sono mutuamente esclusivi"))
	}

	sf := loadSiteFile(dir)
	if *name == "" && sf != nil {
		*name = sf.Name
	}
	if *name == "" {
		abs, _ := filepath.Abs(dir)
		*name = filepath.Base(abs)
	}
	if !quick.ValidName(*name) {
		fatal(fmt.Errorf("nome sito %q non valido (usa a-z, 0-9, trattino)", *name))
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fatal(fmt.Errorf("%q non è una cartella", dir))
	}

	// Calcola il piano (cosa sale, cosa è escluso): condiviso con --dry-run.
	pl, err := buildPlan(dir)
	fatal(err)

	if *dryRun {
		printPlan(*name, pl)
		return
	}

	// Guardia "deploy vuoto": senza file il mirror azzererebbe il sito.
	if len(pl.files) == 0 && !*force {
		fatal(fmt.Errorf("nessun file da pubblicare in %q (esclusi %d). Usa --force per svuotare comunque il sito", dir, pl.excluded))
	}

	// Se la cartella è già collegata a un altro sito (.quick), avvisa prima di
	// fare deploy altrove: facile da innescare con --name sbagliato.
	if !confirmSiteMismatch(sf, *name, "fare deploy su") {
		return
	}

	srv := *server
	if srv == "" && sf != nil {
		srv = sf.Server
	}
	cfg, err := resolveConfig(srv)
	fatal(err)

	// Riepilogo + conferma: sostituire l'intero sito è un'operazione distruttiva.
	if !confirmDeploy(*name, cfg, pl, *yes) {
		fmt.Fprintln(os.Stderr, "annullato")
		return
	}

	tok := *token
	if tok == "" {
		if tok, err = idToken(cfg); err != nil {
			fatal(err)
		}
	}

	payload, err := tarGzFromPlan(dir, pl)
	fatal(err)

	endpoint := cfg.Server + "/api/deploy?name=" + url.QueryEscape(*name)
	req, err := http.NewRequest(http.MethodPost, endpoint, payload)
	fatal(err)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	fatal(err)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "deploy fallito (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(respBody)))
		os.Exit(1)
	}

	var res quick.DeployResponse
	json.Unmarshal(respBody, &res)
	fmt.Printf("✓ %s pubblicato → %s\n", *name, res.URL)
	saveSiteFile(dir, siteFile{Name: *name, Server: cfg.Server})

	// Visibilità opzionale applicata subito dopo il deploy.
	switch {
	case *public:
		callPolicy(cfg, *name, tok, quick.PolicyRequest{Access: new(quick.AccessPublic)})
		fmt.Println("  → pubblico (niente SSO)")
	case privateSet:
		code := *private
		if code == "" {
			code = genCode()
		}
		callPolicy(cfg, *name, tok, quick.PolicyRequest{Access: new(quick.AccessCode), Code: &code})
		fmt.Printf("  → privato, codice: %s\n", code)
	}
}

// tarGz calcola il piano della cartella e ne crea il tar.gz in memoria.
func tarGz(dir string) (*bytes.Buffer, error) {
	p, err := buildPlan(dir)
	if err != nil {
		return nil, err
	}
	return tarGzFromPlan(dir, p)
}

// tarGzFromPlan impacchetta i soli file del piano (già filtrati dai tre tier).
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

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "errore:", err)
		os.Exit(1)
	}
}
