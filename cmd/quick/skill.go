package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const skillAutoupdateEnv = "QUICK_NO_SKILL_AUTOUPDATE"

const skillDoc = `---
name: quick
description: >-
  Publishes and manages static sites on the internal "quick" hosting with the ` + "`quick`" + ` CLI.
  Use it when the user wants to put a folder of HTML/assets online and get
  a <name>.<domain> URL, republish a site, change its visibility (public,
  access code, company SSO), lock or delete it, check its status,
  create deploy tokens for CI/agent environments, attach a custom domain, or understand why a deploy
  excludes certain files. Covers mirror deploys, .quickignore, the 404.html /
  200.html / clean URL / _redirects conventions (redirects, SPA rewrites and
  same-origin API proxying), login, multiple servers, deploy tokens and status.
---

# quick CLI

` + "`quick`" + ` is the CLI for an internal static hosting: you publish a folder of
HTML/assets and get ` + "`https://<name>.<domain>`" + `. By default a site is visible
only to accounts in the company domain (company SSO); you can open it to the public,
protect it with a code, or lock it against overwrites.

The server configures itself: the only required input is the URL, via ` + "`--server`" + `
or the ` + "`QUICK_SERVER`" + ` variable. After the first deploy, the folder remembers site
and server in a ` + "`.quick`" + ` file, so later commands don't need
arguments.

## First use

` + "```bash" + `
export QUICK_SERVER=https://quick.example.com   # once (or use --server)
quick login                                     # opens the browser to log in
quick deploy my-site ./build                    # -> https://my-site.quick.example.com
` + "```" + `

## Commands

| Command | What it does |
|---|---|
| ` + "`quick`" + ` | Overview (server, login, linked site) + command list |
| ` + "`quick status`" + ` | Site status: real visibility, lock, and what would be deployed |
| ` + "`quick login`" + ` | Log in (once per server; the login is remembered and that server becomes the default) |
| ` + "`quick servers [use <server>]`" + ` | Known servers with login status; ` + "`use`" + ` changes the default |
| ` + "`quick deploy [<site>] [folder]`" + ` | Publish a folder (default: the current one) |
| ` + "`quick ignore [folder]`" + ` | Create an editable ` + "`.quickignore`" + ` with the defaults already inside |
| ` + "`quick publish <site>`" + ` | Open to the public (no SSO) |
| ` + "`quick unpublish <site>`" + ` | Back behind company SSO (default) |
| ` + "`quick private <site> [--code X]`" + ` | Access by code (generated if absent) |
| ` + "`quick lock <site>`" + ` / ` + "`quick unlock <site>`" + ` | Lock/unlock overwrites (owner only) |
| ` + "`quick token create <site> --name github-actions [--expires 90d]`" + ` | Create a site-scoped deploy token |
| ` + "`quick token list <site>`" + ` | List deploy tokens for a site |
| ` + "`quick token revoke <site> <token-id>`" + ` | Revoke a deploy token |
| ` + "`quick domains <site>`" + ` | List custom domains and the DNS records still missing |
| ` + "`quick domains add <site> <domain>`" + ` | Attach a custom domain (e.g. www.example.com) |
| ` + "`quick domains rm <site> <domain>`" + ` | Detach a custom domain |
| ` + "`quick rollback <site>`" + ` | Restore the previous version (with confirmation; ` + "`--yes`" + ` skips it; ` + "`--force`" + ` even during someone's deploy) |
| ` + "`quick delete <site>`" + ` | Delete the site (irreversible, with confirmation) |
| ` + "`quick upgrade [--check]`" + ` | Update the CLI to the latest release (` + "`--check`" + ` only checks) |

` + "`<site>`" + ` is optional if the folder has a ` + "`.quick`" + ` file: in that case the name
and server come from there. Without ` + "`.quick`" + ` and without a name, the site takes the name
of the current folder.

## Multiple servers

The CLI can work with several quick servers (e.g. one per company). Each server
keeps its own login. The server is chosen in this order: ` + "`--server`" + `, then
` + "`QUICK_SERVER`" + `, then the folder's ` + "`.quick`" + `, then the default server.
` + "`quick login --server X`" + ` makes X the default; ` + "`quick servers`" + ` shows which one it is.
If the folder is linked to the same site on another server, the CLI asks before acting.

A server you have already used can be named by a word of its address:
` + "`--server acme`" + ` means ` + "`quick.acme.com`" + `, ` + "`quick servers use acme`" + ` makes it the default. A new server needs its full address the first time. The first deploy
from a folder writes the server into ` + "`.quick`" + `, so later commands there need no flag:

` + "```bash" + `
quick deploy my-site --server acme    # once; then just: quick deploy
` + "```" + `

## Deploy: it's a mirror

**The deploy replaces the entire content of the site**, it does not add: files not
present in the package are removed from the site. Consequences:

- To update a single file you still republish the whole folder, but the upload
  is incremental: only the parts of files the server doesn't already have travel
  (content-defined chunks, ~64 KiB). Republishing a big build where part of a
  file changed, or with renamed files, sends little; the CLI reports it as
  "uploaded X of Y". Against an older or busy server it uploads the whole folder.
  Builds compressed as a whole (e.g. Unity Gzip/Brotli compression) change
  everywhere on each build, so they get little benefit.
- A deploy from an empty folder would wipe the site: the CLI **blocks** it
  (use ` + "`--force`" + ` to empty it on purpose).
- Before publishing, the CLI shows a summary (file count, size) and
  asks for confirmation. Skip the prompt with ` + "`--yes`" + `; in non-interactive contexts
  without ` + "`--yes`" + ` the deploy is refused, for safety.
- One deploy at a time per site. While someone is publishing it, another deploy
  of the same site (yours from another terminal too) or a rollback is refused
  with ` + "`<who> is publishing this site (since 14:02); retry in a moment or use --force`" + `.
  The site frees up as soon as that deploy ends, or after 2 minutes without
  activity if it was interrupted (an interrupted deploy never publishes
  anything). Wait and retry; use ` + "`--force`" + ` (on ` + "`deploy`" + ` or ` + "`rollback`" + `) only when
  you know the other deploy is stale or must be overridden, never by reflex:
  the other deploy then stops with ` + "`this deploy lost the site ... nothing was published, run it again`" + `.
  Deleting the site always works and ends a deploy in progress.

Useful flags: ` + "`--dry-run`" + ` (show what would be deployed without publishing),
` + "`--yes`" + `, ` + "`--force`" + ` (empty the site on purpose, or take over from a deploy in
progress), ` + "`--public`" + ` / ` + "`--private[=code]`" + ` (visibility right after the deploy).

## CI and web agents

For non-interactive environments such as GitHub Actions, Claude Code web or
other hosted agents, create a site-scoped deploy token:

` + "```bash" + `
quick token create my-site --name github-actions --expires 90d
# Save the printed value as a secret named QUICK_API_TOKEN.
QUICK_API_TOKEN=qk_... quick deploy my-site ./build --yes
` + "```" + `

Deploy tokens are intentionally narrow: one token is valid for one site and only
for deploys. It cannot delete, rollback, change visibility/lock, or create other
tokens. The cleartext token is shown once; later use ` + "`quick token list`" + ` to see token
IDs and ` + "`quick token revoke`" + ` to remove them. Expiry choices are ` + "`30d`" + `, ` + "`90d`" + `,
` + "`180d`" + `, ` + "`365d`" + `, or ` + "`never`" + `.

## Custom domains

` + "`quick domains add my-site www.example.com`" + ` attaches a domain and prints the DNS
record to create at the domain's DNS provider (any provider; DNS only, no CDN
proxy): ` + "`www.example.com CNAME my-site.<domain>`" + `. An apex like ` + "`example.com`" + `
can't CNAME: use ALIAS/CNAME flattening or the same A record.

The domain stays "waiting for DNS" until the record resolves, then becomes
active on its own (` + "`quick domains my-site`" + ` shows the state); the HTTPS certificate
is issued on the first visit. A domain belongs to one site; it moves to another
site only when its DNS points there. Public and access-code sites are served on
the custom domain; company-SSO sites redirect visitors to their regular
` + "`<site>.<domain>`" + ` address, since sign-in doesn't carry across domains.

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
- ` + "`_redirects`" + ` at the root (Netlify-style): per-path rules, applied only
  when no file matches (files always win) and before ` + "`200.html`" + `:

  ` + "```" + `
  /old    /new                            301   # redirect (302/307/308 too; 301 if omitted)
  /*      /index.html                     200   # local rewrite: SPA catch-all
  /api/*  https://api.example.com/:splat  200   # proxy: call a non-CORS API same-origin
  ` + "```" + `

  Only a trailing ` + "`/*`" + ` wildcard, referenced as ` + "`:splat`" + `. Proxying is a
  pass-through to public https hosts only (no private IPs, no quick subdomains,
  no injected credentials) and follows the site's visibility. The
  ` + "`_redirects`" + ` file itself is never served.
- Precompressed assets work as-is: ` + "`x.js.br`" + `, ` + "`x.wasm.gz`" + `, ` + "`x.data.br`" + `,
  ` + "`x.json.gz`" + ` are served with the right type and Content-Encoding. Unity WebGL
  builds deploy unchanged with any compression setting (Brotli, Gzip, or
  decompression fallback ` + "`.unityweb`" + `); no server config needed.
- Every site can import the browser library ` + "`/__quick/quick.js`" + ` (ESM, types at
  ` + "`/__quick/quick.d.ts`" + `). ` + "`await quick.id.me()`" + ` returns the signed-in user
  (` + "`{email}`" + `) on SSO sites, ` + "`null`" + ` on public or access-code sites. The
  ` + "`/__quick/`" + ` path prefix is reserved for quick: do not put site files there.

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

func refreshInstalledSkillsSilent() {
	if os.Getenv(skillAutoupdateEnv) != "" {
		return
	}
	for _, path := range installedSkillCandidates() {
		b, err := os.ReadFile(path)
		if err != nil || string(b) == skillDoc || !looksLikeQuickSkill(string(b)) {
			continue
		}
		_ = os.WriteFile(path, []byte(skillDoc), 0o644)
	}
}

func installedSkillCandidates() []string {
	var out []string
	for _, t := range []string{"claude", "codex", "gemini"} {
		out = append(out, filepath.Join("."+t, "skills", "quick", "SKILL.md"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, t := range []string{"claude", "codex", "gemini"} {
			out = append(out, filepath.Join(home, "."+t, "skills", "quick", "SKILL.md"))
		}
	}
	return out
}

func looksLikeQuickSkill(s string) bool {
	return strings.Contains(s, "name: quick") && strings.Contains(s, "# quick CLI")
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
