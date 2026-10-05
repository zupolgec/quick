package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const siteFileName = ".quick"

type siteFile struct {
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	Dir    string `json:"dir,omitempty"` // published subfolder (relative); empty = "."
}

func loadSiteFile(dir string) *siteFile {
	b, err := os.ReadFile(filepath.Join(dir, siteFileName))
	if err != nil {
		return nil
	}
	var sf siteFile
	if json.Unmarshal(b, &sf) != nil || sf.Name == "" {
		return nil
	}
	return &sf
}

func saveSiteFile(dir string, sf siteFile) {
	b, _ := json.MarshalIndent(sf, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, siteFileName), append(b, '\n'), 0o644)
}

// confirmSiteMismatch asks for confirmation when the current folder is linked
// (via .quick) to a site different from the one the command will act on. verb is
// the action shown (e.g. "deploy to", "modify", "delete"). Returns false if
// the user cancels.
func confirmSiteMismatch(sf *siteFile, name, verb string) bool {
	if sf == nil || sf.Name == "" || sf.Name == name {
		return true
	}
	fmt.Fprintf(os.Stderr, "⚠️  this folder is linked to site %q (.quick), but you are about to %s %q.\n", sf.Name, verb, name)
	fmt.Fprint(os.Stderr, "Proceed anyway? [y/N]: ")
	if !yesNo(readLine()) {
		fmt.Fprintln(os.Stderr, "cancelled")
		return false
	}
	return true
}

// resolveSiteConfig resolves the server for a command on site name and asks
// for confirmation when the folder is linked (via .quick) to the same site on
// another server. Returns false if the user cancels.
func resolveSiteConfig(serverFlag string, sf *siteFile, name, verb string) (*cliConfig, bool) {
	linked := ""
	if sf != nil {
		linked = sf.Server
	}
	cfg, err := resolveConfig(serverFlag, linked)
	fatal(err)
	if linked == "" || sf.Name != name || normalizeServer(linked) == cfg.Server {
		return cfg, true
	}
	fmt.Fprintf(os.Stderr, "⚠️  this folder is linked to %q on %s (.quick), but you are about to %s it on %s.\n", name, linked, verb, cfg.Server)
	fmt.Fprint(os.Stderr, "Proceed anyway? [y/N]: ")
	if !yesNo(readLine()) {
		fmt.Fprintln(os.Stderr, "cancelled")
		return nil, false
	}
	return cfg, true
}
