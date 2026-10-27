---
title: Getting started
description: Install the quick CLI, sign in to your server and publish your first site.
---

# Getting started

quick is static hosting for a team. You publish a folder from your terminal and it goes live at `https://<name>.<your-domain>`. By default only people who can sign in with your company account can open it.

You need a quick server. If your company already runs one, ask for its address (for example `quick.acme.com`). If not, see [Self-hosting](/docs/self-hosting).

## Install the CLI

```bash
# macOS and Linux
curl -fsSL https://quick-cli.com/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://quick-cli.com/install.ps1 | iex
```

The script downloads the prebuilt binary from the latest [GitHub release](https://github.com/zupolgec/quick/releases) into `~/.local/bin` (`%LOCALAPPDATA%\quick` on Windows). On macOS and Linux, set `QUICK_INSTALL_DIR` to put it somewhere else. Every quick server also serves the same scripts at `https://<server>/install.sh`.

With Go installed you can build it instead:

```bash
go install github.com/zupolgec/quick/cmd/quick@latest
```

Update later with `quick upgrade` (`quick upgrade --check` only checks).

## Sign in

```bash
quick login --server quick.acme.com
```

Your browser opens your company sign-in page. When you are back, the CLI remembers the login and makes this server the default. You can give the server as a bare domain or a full URL.

## Publish a folder

```bash
quick deploy launch ./dist
```

```
✓ Authenticated as giulia
About to publish https://launch.quick.acme.com
  3 files, 879.0 KB (excluded by: default rules)
Proceed? [y/N]: y
  sending 879.0 KB of 879.0 KB
✓ uploaded 879.0 KB of 879.0 KB
✓ launch published → https://launch.quick.acme.com
```

That is the whole loop. A few things worth knowing:

- **A deploy is a mirror.** The folder replaces the whole site: files you removed disappear from it.
- **The folder remembers.** The first deploy writes a `.quick` file with the site name, the server and the folder. From then on `quick deploy` with no arguments republishes it.
- **Redeploys are incremental.** Only the parts of files the server does not have yet are uploaded.
- **New sites are instant.** There is nothing to configure per site.

## Next

- [CLI reference](/docs/cli): every command and flag.
- [Site conventions](/docs/sites): `404.html`, SPAs, `_redirects`, `_headers`, search engines, what is never uploaded.
- [Access and safety](/docs/access): public sites, access codes, locks, rollback.
- [CI and agents](/docs/ci-and-agents): deploy tokens, GitHub Actions, the agent skill.
