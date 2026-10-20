---
title: CI and agents
description: Deploy tokens for GitHub Actions and hosted agents, the quick agent skill, and the Markdown versions of these docs.
---

# CI and agents

## Deploy tokens

CI runners and hosted agents (GitHub Actions, Claude Code on the web, other sandboxes) can't open a browser to sign in. Give them a deploy token instead:

```bash
quick token create launch --name github-actions --expires 90d
# the value is printed once: save it as a secret named QUICK_API_TOKEN

QUICK_API_TOKEN=qk_... quick deploy launch ./dist --yes
```

A token is deliberately narrow:

- It is valid for **one site**, and only for **deploys**. It cannot delete, roll back, change visibility or the lock, or create other tokens.
- Expiry is `30d`, `90d` (default), `180d`, `365d` or `never`.
- Up to 20 tokens per site, names up to 40 characters.
- The server stores only a keyed hash; the value is shown once.

`quick token list launch` shows token IDs; `quick token revoke launch <id>` removes one. The dashboard on your server (open the site, then Deploy tokens) does the same in the browser. In `shared` and `owned` modes, managing tokens follows the same rules as changing the site's visibility.

## GitHub Actions

```yaml
name: deploy
on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: npm ci && npm run build
      - run: curl -fsSL https://quick-cli.com/install.sh | sh
      - run: ~/.local/bin/quick deploy launch ./dist --server quick.acme.com --yes
        env:
          QUICK_API_TOKEN: ${{ secrets.QUICK_API_TOKEN }}
```

## The agent skill

```bash
quick skill                            # ~/.claude/skills/quick/SKILL.md
quick skill --all                      # Claude Code, Codex and Gemini
quick skill --target codex --project   # .codex/skills/quick/ in this repository
```

`quick skill` installs a `SKILL.md` that teaches a coding agent how to use the CLI: deploys as mirrors, confirmation and `--yes`, visibility, tokens, `_redirects`, and when not to use `--takeover`. `SKILL.md` is an open format read by Claude Code, Codex, Gemini, Cursor and others.

When you upgrade the CLI, skills you already installed for Claude, Codex or Gemini are refreshed on the next command. New ones are never created without `quick skill`. Set `QUICK_NO_SKILL_AUTOUPDATE=1` to turn the refresh off.

The current skill is published at [quick-cli.com/skill.md](/skill.md).

## These docs, for agents

- [/llms.txt](/llms.txt) lists every page with a one-line summary.
- [/llms-full.txt](/llms-full.txt) holds all of them in one file.
- Every page has a Markdown twin: add `.md` to its address (`/docs/cli.md`), or ask for it with `Accept: text/markdown`.

```bash
curl -H "Accept: text/markdown" https://quick-cli.com/docs/sites
```
