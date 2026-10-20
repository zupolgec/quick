// Deploy mirror selection, shared by deploy, --dry-run and status. Three
// cascading tiers decide what gets uploaded:
//
//	Tier 1  security blocklist — always, never overridable: dotfiles (except
//	        .well-known) and secrets. Even a .quickignore cannot re-include these.
//	Tier 2  convenience defaults — node_modules, vendor, logs, OS junk. Silent;
//	        overridden by publishing a .quickignore.
//	Tier 3  .quickignore — if present, replaces Tier 2 as the source of truth
//	        (gitignore syntax, with ! negation).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	gitignore "github.com/sabhiram/go-gitignore"

	"github.com/zupolgec/quick/internal/quick"
)

const quickignoreName = ".quickignore"

// extensions/names that must NEVER leave the machine, even without a leading
// dot. Matched on the basename, case-insensitive.
var tier1SecretExt = []string{
	".pem", ".key", ".p12", ".pfx", ".ppk", ".kdbx",
}
var tier1SecretNames = []string{
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
}

// convenience exclusions; also the contents `quick ignore` writes into the
// sample .quickignore.
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

type planFile struct {
	rel  string // relative path, slash-separated
	size int64
}

type plan struct {
	files          []planFile
	excluded       int
	totalSize      int64
	hasQuickignore bool
	reserved       []string // files under the reserved __quick/ folder
}

// buildPlan walks the folder and applies the three exclusion tiers.
func buildPlan(dir string) (*plan, error) {
	p := &plan{}

	// Tier 2/3: a present .quickignore replaces the built-in defaults.
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

		// Tier 1: dotfiles (except .well-known) and secrets; prune the whole
		// subtree on an excluded directory.
		if tier1Blocked(rel, fi.IsDir()) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			p.excluded++
			return nil
		}

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
			if quick.IsReservedPath(rel) {
				p.reserved = append(p.reserved, rel)
			}
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

// tier1Blocked applies the non-overridable security blocklist.
func tier1Blocked(rel string, isDir bool) bool {
	// dotfile in any path component, except .well-known.
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

// readQuickignore returns the folder's .quickignore rules; ok=false if absent
// or empty of effective rules.
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

func quickignoreTemplate() string {
	var b strings.Builder
	b.WriteString("# .quickignore — files that are NOT published with the deploy.\n")
	b.WriteString("# gitignore syntax: one rule per line, ! to re-include.\n")
	b.WriteString("# (private keys, .env and dotfiles always stay excluded, even without listing them.)\n\n")
	for _, d := range tier2Defaults {
		b.WriteString(d)
		b.WriteByte('\n')
	}
	return b.String()
}

// writeQuickignore writes the template into the folder, never overwriting an
// existing one. Returns the path written, or "" if one already existed.
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

func (p *plan) ignoreSource() string {
	if p.hasQuickignore {
		return ".quickignore"
	}
	return "default rules"
}

func printPlan(name string, p *plan) {
	fmt.Printf("Deploy of %q — preview (nothing was published):\n", name)
	fmt.Printf("  %s, %s (excluded by: %s)\n", plural(len(p.files), "file", "files"), humanSize(p.totalSize), p.ignoreSource())
	if p.excluded > 0 {
		fmt.Printf("  %s excluded\n", plural(p.excluded, "file/folder", "files/folders"))
	}
	for _, f := range p.files {
		fmt.Printf("    %s  %s\n", humanSize(f.size), f.rel)
	}
}

// confirmDeploy prints the summary and asks for confirmation. Skips the prompt
// with --yes; without an interactive terminal (scripts) it refuses unless --yes,
// so a site is never replaced by accident in automation. exists distinguishes a
// first publish from a destructive replace.
func confirmDeploy(name string, cfg *cliConfig, p *plan, exists, yes bool) bool {
	url := "https://" + name + "." + cfg.BaseDomain
	if exists {
		fmt.Printf("About to replace the entire contents of %s\n", url)
	} else {
		fmt.Printf("About to publish %s\n", url)
	}
	fmt.Printf("  %s, %s (excluded by: %s", plural(len(p.files), "file", "files"), humanSize(p.totalSize), p.ignoreSource())
	if p.excluded > 0 {
		fmt.Printf(", %d excluded", p.excluded)
	}
	fmt.Println(")")

	if yes {
		return true
	}
	if !stdinIsTTY() {
		fmt.Fprintln(os.Stderr, "refused: not interactive. Re-run with --yes to confirm.")
		return false
	}
	fmt.Print("Proceed? [y/N]: ")
	return yesNo(readLine())
}

// confirmOverwrite requires retyping the site name when the last deploy was by
// someone else, to avoid overwriting their work by mistake.
func confirmOverwrite(name, lastBy string) bool {
	fmt.Println()
	fmt.Printf("%s Site %s was last updated by %s\n",
		cYellow("!"), cBold(cYellow("'"+name+"'")), cBold(localPart(lastBy)))
	if !stdinIsTTY() {
		fmt.Fprintln(os.Stderr, "refused: not interactive. Re-run with --yes to confirm.")
		return false
	}
	fmt.Printf("%s Type %s to confirm the overwrite: ", cGreen("?"), cBold("'"+name+"'"))
	return readLine() == name
}

// plural renders a count with the right singular/plural noun (e.g. "1 file",
// "3 files").
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

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

func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
