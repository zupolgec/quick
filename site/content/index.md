---
title: quick
description: Self-hosted static hosting for teams. Publish a folder with one command and get name.yourdomain behind your company sign-in.
---

# quick

Publish a folder from your terminal. It goes live at its own subdomain, behind your company sign-in.

```bash
curl -fsSL https://quick-cli.com/install.sh | sh   # macOS and Linux
quick login --server quick.acme.com                  # your company's quick server
quick deploy launch ./dist                           # -> https://launch.quick.acme.com
```

## What you get

- **Private by default.** Every site starts behind your company sign-in: Google Workspace or any OIDC provider. Open one to the world with `quick publish`, or hand out an access code with `quick private`.
- **Agents ship too.** `quick skill` teaches Claude Code, Codex, Gemini or Cursor to deploy for you. Deploy tokens are tied to one site and can only deploy it.
- **Your box, your bucket.** Two containers and a wildcard DNS record. Files on local disk or any S3-compatible bucket. MIT licensed, no account anywhere.
- **Redeploys send the diff.** Files are split into content-defined chunks; only new chunks travel. Nine measured redeploys of a 3.1 MB build sent 457.2 KB in total, instead of 27.9 MB.
- **Static-host conventions.** `index.html`, clean URLs, `404.html`, `200.html` for single-page apps, `_redirects` with same-origin proxying, precompressed `.br`/`.gz` and Unity WebGL builds as they are, custom domains.
- **Safe by design.** One deploy at a time per site, atomic publishing, one-command rollback, locks, and a confirmation before you overwrite someone else's work.

## Docs

- [Getting started](https://quick-cli.com/docs/index.md)
- [CLI reference](https://quick-cli.com/docs/cli.md)
- [Site conventions](https://quick-cli.com/docs/sites.md)
- [Access and safety](https://quick-cli.com/docs/access.md)
- [CI and agents](https://quick-cli.com/docs/ci-and-agents.md)
- [Custom domains](https://quick-cli.com/docs/custom-domains.md)
- [Self-hosting](https://quick-cli.com/docs/self-hosting.md)

Source: https://github.com/zupolgec/quick (MIT).
