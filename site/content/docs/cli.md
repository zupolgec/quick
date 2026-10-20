---
title: CLI reference
description: Every quick command, how the server is chosen, and the deploy flags.
---

# CLI reference

`<site>` is optional inside a folder with a `.quick` file, which the first deploy writes: the name and the server come from there, so `quick publish`, `quick rollback` or `quick domains add www.example.com` act on that site. Outside such a folder, `quick deploy` uses the folder's name and the other commands ask for the site.

| Command | What it does |
|---|---|
| `quick` | Overview: server, login, linked site, and the command list |
| `quick status` | Real visibility and lock of the site, and what a deploy would upload |
| `quick login` | Sign in to a server; it becomes the default |
| `quick servers` | Known servers and their login state |
| `quick servers use <server>` | Change the default server |
| `quick deploy [<site>] [folder]` | Publish a folder (default: the current one) |
| `quick ignore [folder]` | Write an editable `.quickignore` with the defaults inside |
| `quick publish [<site>]` | Open the site to anyone, no sign-in |
| `quick unpublish [<site>]` | Put it back behind the company sign-in (the default) |
| `quick private [<site>] [--code X]` | Require an access code (generated if you don't give one) |
| `quick lock [<site>]` / `quick unlock [<site>]` | Only you can overwrite it / anyone can again |
| `quick token create [<site>] --name <n> [--expires 90d]` | Create a deploy token for one site |
| `quick token list [<site>]` | List its deploy tokens |
| `quick token revoke [<site>] <token-id>` | Revoke one |
| `quick domains [<site>]` | Custom domains and the DNS records still missing |
| `quick domains add [<site>] <domain>` | Attach a custom domain |
| `quick domains rm [<site>] <domain>` | Detach it |
| `quick rollback [<site>]` | Swap back to the previous version (run it again to undo) |
| `quick delete [<site>]` | Delete the site and its settings, for good |
| `quick skill` | Install the agent skill (see [CI and agents](/docs/ci-and-agents)) |
| `quick upgrade [--check]` | Update the CLI to the latest release |

## Deploy flags

| Flag | Effect |
|---|---|
| `--yes` | Skip the confirmation. Without it, a non-interactive deploy is refused. |
| `--dry-run` | Show what would be uploaded and stop. |
| `--force` | Allow publishing an empty folder, which empties the site. |
| `--takeover` | Take over from a deploy in progress (also on `rollback`). |
| `--public` / `--private[=code]` | Set the visibility right after the deploy. |
| `--server <server>` | Use this server for this command. |

Before publishing, the CLI prints who you are, where the site goes and how much it holds, then asks. If the last deploy of that site was by someone else, it asks you to type the site name before overwriting their work.

## Choosing the server

You can use several quick servers from one machine, one per company for example. Each keeps its own login. The server is picked in this order:

1. `--server`
2. the `QUICK_SERVER` environment variable
3. the folder's `.quick` file
4. the default server (the last one you logged in to, or the one set with `quick servers use`)

A server you have used before can be named by a word of its address: `--server acme` means `quick.acme.com`. A new server needs its full address the first time. The CLI also tries `deploy.<domain>` and remembers whichever answers.

If the folder is linked to a site on another server, or to a different site, the CLI says so and asks before going on.

## Files the CLI writes

- `.quick`, in the folder you deploy from: site name, server and published sub-folder. It is never uploaded.
- Known servers and logins, in your user config folder: `~/Library/Application Support/quick/` on macOS, `~/.config/quick/` on Linux, `%AppData%\quick\` on Windows.
