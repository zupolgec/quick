// Cosa entra nel mirror del deploy. Il piano (plan) è calcolato qui e condiviso
// da deploy, --dry-run e status: un unico punto che decide quali file salgono.
//
// Tre livelli, in cascata:
//
//	Tier 1  blocklist di sicurezza  — sempre, non scavalcabile: dotfile (tranne
//	        .well-known) e materiale segreto (chiavi private, keystore). Anche
//	        un .quickignore non può riammettere questi file.
//	Tier 2  default di comodità      — node_modules, vendor, log, junk dell'OS.
//	        Applicati in silenzio; scavalcabili pubblicando un .quickignore.
//	Tier 3  .quickignore             — se presente, sostituisce il Tier 2 come
//	        fonte di verità (sintassi gitignore, con negazione !). `quick ignore`
//	        lo materializza con i default già dentro, pronti da modificare.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	gitignore "github.com/sabhiram/go-gitignore"
)

const quickignoreName = ".quickignore"

// tier1Secrets: estensioni/nomi che non devono MAI lasciare la macchina, anche
// senza punto iniziale. Confrontati sul solo basename, case-insensitive.
var tier1SecretExt = []string{
	".pem", ".key", ".p12", ".pfx", ".ppk", ".kdbx",
}
var tier1SecretNames = []string{
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
}

// tier2Defaults: esclusioni di comodità (sintassi gitignore). Sono anche il
// contenuto che `quick ignore` scrive nel .quickignore di esempio.
var tier2Defaults = []string{
	"node_modules/",
	"vendor/",
	"bower_components/",
	"*.log",
	"*.tmp",
	"*.swp",
	"Thumbs.db",
	"desktop.ini",
}

// planFile è un file che entra nel deploy, con la sua dimensione.
type planFile struct {
	rel  string // path relativo, separatori slash
	size int64
}

// plan è il risultato del calcolo: cosa sale, quanto pesa, quanti esclusi e se
// le regole vengono da un .quickignore pubblicato.
type plan struct {
	files          []planFile
	excluded       int
	totalSize      int64
	hasQuickignore bool
}

// buildPlan cammina la cartella e applica i tre livelli di esclusione.
func buildPlan(dir string) (*plan, error) {
	p := &plan{}

	// Tier 2/3: un .quickignore presente sostituisce i default integrati.
	var soft *gitignore.GitIgnore
	if lines, ok := readQuickignore(dir); ok {
		p.hasQuickignore = true
		soft = gitignore.CompileIgnoreLines(lines...)
	} else {
		soft = gitignore.CompileIgnoreLines(tier2Defaults...)
	}

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
		rel = filepath.ToSlash(rel)

		// Tier 1: dotfile (tranne .well-known) e segreti. Su una cartella
		// esclusa potiamo l'intero sottoalbero.
		if tier1Blocked(rel, fi.IsDir()) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			p.excluded++
			return nil
		}

		// Tier 2/3.
		match := rel
		if fi.IsDir() {
			match += "/"
		}
		if soft.MatchesPath(match) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			p.excluded++
			return nil
		}

		if fi.Mode().IsRegular() {
			p.files = append(p.files, planFile{rel: rel, size: fi.Size()})
			p.totalSize += fi.Size()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// tier1Blocked applica la blocklist di sicurezza, non scavalcabile.
func tier1Blocked(rel string, isDir bool) bool {
	// dotfile in qualunque componente del path, tranne .well-known.
	for part := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(part, ".") && part != ".well-known" {
			return true
		}
	}
	if isDir {
		return false
	}
	base := strings.ToLower(filepath.Base(rel))
	if slices.Contains(tier1SecretNames, base) {
		return true
	}
	for _, ext := range tier1SecretExt {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

// readQuickignore legge le righe del .quickignore della cartella (ok=false se
// assente o vuoto di regole effettive).
func readQuickignore(dir string) (lines []string, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, quickignoreName))
	if err != nil {
		return nil, false
	}
	for ln := range strings.SplitSeq(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if t != "" && !strings.HasPrefix(t, "#") {
			lines = append(lines, t)
		}
	}
	return lines, len(lines) > 0
}

// quickignoreTemplate è il file scritto da `quick ignore`: i default del Tier 2
// resi visibili e modificabili. Il Tier 1 (segreti) resta sempre attivo a parte.
func quickignoreTemplate() string {
	var b strings.Builder
	b.WriteString("# .quickignore — file che NON vengono pubblicati col deploy.\n")
	b.WriteString("# Sintassi gitignore: una regola per riga, ! per riammettere.\n")
	b.WriteString("# (chiavi private, .env e i dotfile restano sempre esclusi, anche senza elencarli.)\n\n")
	for _, d := range tier2Defaults {
		b.WriteString(d)
		b.WriteByte('\n')
	}
	return b.String()
}

// writeQuickignore materializza il template nella cartella (non sovrascrive uno
// esistente). Restituisce il path scritto, o "" se c'era già.
func writeQuickignore(dir string) (string, error) {
	dst := filepath.Join(dir, quickignoreName)
	if _, err := os.Stat(dst); err == nil {
		return "", nil
	}
	if err := os.WriteFile(dst, []byte(quickignoreTemplate()), 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// ignoreSource descrive a parole da dove vengono le regole di esclusione.
func (p *plan) ignoreSource() string {
	if p.hasQuickignore {
		return ".quickignore"
	}
	return "regole predefinite"
}

// printPlan elenca cosa verrebbe pubblicato (usato da --dry-run).
func printPlan(name string, p *plan) {
	fmt.Printf("Deploy di %q — anteprima (niente è stato pubblicato):\n", name)
	fmt.Printf("  %d file, %s (esclusioni: %s)\n", len(p.files), humanSize(p.totalSize), p.ignoreSource())
	if p.excluded > 0 {
		fmt.Printf("  %d file/cartelle esclusi\n", p.excluded)
	}
	for _, f := range p.files {
		fmt.Printf("    %s  %s\n", humanSize(f.size), f.rel)
	}
}

// confirmDeploy stampa il riepilogo e chiede conferma. Salta la domanda con
// --yes; senza un terminale interattivo (script) rifiuta a meno di --yes, per
// non sostituire un sito per sbaglio in automatico.
func confirmDeploy(name string, cfg *cliConfig, p *plan, yes bool) bool {
	url := "https://" + name + "." + cfg.BaseDomain
	fmt.Printf("Sto per sostituire l'intero contenuto di %s\n", url)
	fmt.Printf("  %d file, %s (esclusioni: %s", len(p.files), humanSize(p.totalSize), p.ignoreSource())
	if p.excluded > 0 {
		fmt.Printf(", %d esclusi", p.excluded)
	}
	fmt.Println(")")

	if yes {
		return true
	}
	if !stdinIsTTY() {
		fmt.Fprintln(os.Stderr, "rifiutato: non interattivo. Riesegui con --yes per confermare.")
		return false
	}
	fmt.Print("Procedo? [s/N]: ")
	return yesNo(readLine())
}

// humanSize formatta una dimensione in byte in modo leggibile.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// stdinIsTTY indica se stdin è un terminale interattivo.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
