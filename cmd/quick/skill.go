package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const skillDoc = `---
name: quick
description: >-
  Publishes and manages static sites on the internal "quick" hosting with the ` + "`quick`" + ` CLI.
  Use it when the user wants to put a folder of HTML/assets online and get
  a <name>.<domain> URL, republish a site, change its visibility (public,
  access code, company SSO), lock or delete it, check its status,
  or understand why a deploy excludes certain files. Covers mirror deploys,
  .quickignore, the 404.html / 200.html / clean URL conventions, Google login and status.
---

# quick CLI

` + "`quick`" + ` is the CLI for an internal static hosting: you publish a folder of
HTML/assets and get ` + "`https://<name>.<domain>`" + `. By default a site is visible
only to accounts in the company domain (Google SSO); you can open it to the public,
protect it with a code, or lock it against overwrites.

The server configures itself: the only required input is the URL, via ` + "`--server`" + `
or the ` + "`QUICK_SERVER`" + ` variable. After the first deploy, the folder remembers site
and server in a ` + "`.quick`" + ` file, so later commands don't need
arguments.

## First use

` + "```bash" + `
export QUICK_SERVER=https://quick.example.com   # once (or use --server)
quick login                                     # opens the browser for Google login
quick deploy my-site ./build                    # -> https://my-site.quick.example.com
` + "```" + `

## Commands

| Command | What it does |
|---|---|
| ` + "`quick`" + ` | Overview (server, login, linked site) + command list |
| ` + "`quick status`" + ` | Site status: real visibility, lock, and what would be deployed |
| ` + "`quick login`" + ` | Google login (once; the token is remembered) |
| ` + "`quick deploy [<site>] [folder]`" + ` | Publish a folder (default: the current one) |
| ` + "`quick ignore [folder]`" + ` | Create an editable ` + "`.quickignore`" + ` with the defaults already inside |
| ` + "`quick publish <site>`" + ` | Open to the public (no SSO) |
| ` + "`quick unpublish <site>`" + ` | Back behind company SSO (default) |
| ` + "`quick private <site> [--code X]`" + ` | Access by code (generated if absent) |
| ` + "`quick lock <site>`" + ` / ` + "`quick unlock <site>`" + ` | Lock/unlock overwrites (owner only) |
| ` + "`quick delete <site>`" + ` | Delete the site (irreversible, with confirmation) |

` + "`<site>`" + ` is optional if the folder has a ` + "`.quick`" + ` file: in that case the name
and server come from there. Without ` + "`.quick`" + ` and without a name, the site takes the name
of the current folder.

## Deploy: it's a mirror

**The deploy replaces the entire content of the site**, it does not add: files not
present in the package are removed from the site. Consequences:

- To update a single file you still republish the whole folder.
- A deploy from an empty folder would wipe the site: the CLI **blocks** it
  (use ` + "`--force`" + ` to empty it on purpose).
- Before publishing, the CLI shows a summary (file count, size) and
  asks for confirmation. Skip the prompt with ` + "`--yes`" + `; in non-interactive contexts
  without ` + "`--yes`" + ` the deploy is refused, for safety.

Useful flags: ` + "`--dry-run`" + ` (show what would be deployed without publishing),
` + "`--yes`" + `, ` + "`--force`" + `, ` + "`--public`" + ` / ` + "`--private[=code]`" + ` (visibility right after the deploy).

## What is NOT published

Exclusions are decided in three tiers:

1. **Security (always, not overridable):** hidden files (` + "`.git`" + `, ` + "`.env`" + `, ` + "`.quick`" + `…,
   except ` + "`.well-known`" + `) and secrets (` + "`*.pem`" + `, ` + "`*.key`" + `, ` + "`id_rsa`" + `, keystores).
2. **Convenience (default, overridable):** ` + "`node_modules/`" + `, ` + "`vendor/`" + `, ` + "`*.log`" + `, temporary files.
3. **` + "`.quickignore`" + ` (if present):** becomes the source of truth for the convenience
   exclusions (gitignore syntax, with ` + "`!`" + ` to re-include). Create it with ` + "`quick ignore`" + `.

Use ` + "`quick status`" + ` or ` + "`quick deploy ... --dry-run`" + ` to see included/excluded files.

## Served site conventions

- ` + "`index.html`" + ` is a folder's index. ` + "`/about`" + ` serves ` + "`about.html`" + `;
  ` + "`about/index.html`" + ` is served at ` + "`/about/`" + `.
- HTML pages have canonical URLs: ` + "`/about.html`" + ` -> ` + "`/about`" + `,
  ` + "`/about/index.html`" + ` -> ` + "`/about`" + `, and if that is a folder,
  ` + "`/about`" + ` -> ` + "`/about/`" + `.
- ` + "`404.html`" + ` at the root or in the nearest folder: page shown (with status 404)
  for nonexistent paths.
- ` + "`200.html`" + ` at the root: app shell for SPAs; served (status 200) for
  any route that doesn't match a file. Without it, missing paths
  give a real 404 (no silent fallback to the home page).

## Notes

- New subdomains are immediate; visibility changes are instant.
- A **locked** site can be overwritten or deleted only by its owner.
`

func skillCmd(args []string) {
	fs := flag.NewFlagSet("skill", flag.ExitOnError)
	target := fs.String("target", "claude", "target agent (claude, codex, gemini, …)")
	project := fs.Bool("project", false, "write to the project's .<target>/skills/quick instead of global")
	all := fs.Bool("all", false, "publish for all known agents (claude, codex, gemini)")
	dir := fs.String("dir", "", "explicit folder (ignores --target/--project)")
	fs.Parse(args)
	if *dir == "" && fs.NArg() > 0 {
		*dir = fs.Arg(0)
	}

	var dirs []string
	switch {
	case *dir != "":
		dirs = []string{*dir}
	case *all:
		for _, t := range []string{"claude", "codex", "gemini"} {
			dirs = append(dirs, skillDir(t, *project))
		}
	default:
		dirs = []string{skillDir(*target, *project)}
	}

	for _, d := range dirs {
		dst := filepath.Join(d, "SKILL.md")
		if err := os.MkdirAll(d, 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(dst, []byte(skillDoc), 0o644); err != nil {
			fatal(err)
		}
		fmt.Printf("✓ skill published to %s\n", dst)
	}
	fmt.Println("  Open SKILL.md format: read by Claude Code, Codex, Gemini, Cursor and others.")
}

// skillDir builds the skill folder per the cross-vendor schema (Claude, Codex,
// Gemini, …): ~/.<tool>/skills/quick (global) or .<tool>/skills/quick (project).
func skillDir(tool string, project bool) string {
	if tool == "" || strings.ContainsAny(tool, "/\\.") {
		fatal(fmt.Errorf("invalid target %q", tool))
	}
	if project {
		return filepath.Join("."+tool, "skills", "quick")
	}
	home, err := os.UserHomeDir()
	fatal(err)
	return filepath.Join(home, "."+tool, "skills", "quick")
}
