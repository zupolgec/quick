// quick è la CLI di way-quick (hosting statico interno, generico).
//
//	quick login                                   # login Google (una volta)
//	quick deploy [cartella] --name <sito>         # pubblica una cartella
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
		usage()
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		printVersion()
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
  quick version
  quick login
  quick deploy [cartella] --name <sito>
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

	cfg, err := resolveConfig(*server)
	fatal(err)

	tok := *token
	if tok == "" {
		if tok, err = idToken(cfg); err != nil {
			fatal(err)
		}
	}

	payload, err := tarGz(dir)
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

// tarGz crea un tar.gz in memoria con i contenuti della cartella (path relativi).
func tarGz(dir string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
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
