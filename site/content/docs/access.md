---
title: Access and safety
description: Who can open a site, who can change it, one deploy at a time, rollback and deletion.
---

# Access and safety

## Who can open a site

Every site has one of these visibilities. Changing it is instant.

```bash
quick access launch                         # who can see it now
quick access launch team                    # everyone at the company (the default)
quick access launch only-me                 # only you
quick access launch add giulia@acme.com     # you and the people you choose
quick access launch remove giulia@acme.com
quick access launch code [--code abc12345]  # access code, generated if you don't give one
quick access launch public                  # anyone, no sign-in
```

Inside a folder linked to the site (it has a `.quick` file) you can drop the name: `quick access only-me`, `quick access add giulia@acme.com`.

- **Everyone at the company** checks the visitor's session with your identity provider (Google Workspace or any OIDC provider). Only accounts in the allowed email domains get in.
- **Chosen people** works like company sign-in, but only the owner (whoever created the site) and the accounts on the list get in. Anyone else at the company sees a page saying the site is restricted and who owns it, never who else is on the list. "Only me" is the same with an empty list. The list takes company accounts, and people outside the company too when the server can send email (see below).
- **People outside the company** on a chosen-people list can't use company sign-in. On the sign-in page they enter their email and get a link: it works once, for 15 minutes, and keeps them signed in on that browser for 7 days. Only listed addresses get the email, and the page answers the same way for anyone, so it doesn't reveal who is on the list. Take someone off the list and they're out at once. This needs the server to send email (`QUICK_SMTP_URL`); without it, `quick access add` refuses addresses outside the company.
- **Access code** shows a page asking for the code. Once entered, it is remembered for 7 days on that browser. Codes have at least 8 characters. When you set the code, quick also prints a link that carries it (`https://launch.quick.acme.com/?quick_code=...`): whoever opens it gets straight in, without typing anything, and the code leaves the address bar. Whoever can change the site sees the current code and link again with `quick access launch` and on the site's page in the dashboard. Changing the code signs out everyone who entered the old one. After 10 wrong codes in 15 minutes, a visitor has to wait before trying again.
- **Public** serves the site to everyone.

Administrators (set on the server with `QUICK_ADMINS`) can see, open and manage every site, chosen-people and "only me" ones included, and can do on any site whatever its owner can: change who sees it, lock or unlock it, delete it, manage its tokens and domains, raise its size limit. The site's page in the dashboard names them.

`quick publish`, `quick unpublish` and `quick private` still work as shortcuts for `public`, `team` and `code`. The same choices are on the site's page in the dashboard.

## Who can change a site

quick records who created each site and who published it last. The server decides what that means with `QUICK_OWNERSHIP`:

| Mode | Overwrite | Delete, change visibility |
|---|---|---|
| `free` (default) | anyone in the company | anyone in the company |
| `shared` | anyone in the company | the creator only |
| `owned` | the creator only | the creator only |

In any mode, the owner can lock a site:

```bash
quick lock launch     # from now on only you can overwrite or change it
quick unlock launch
```

A site limited to chosen people is stricter in every mode: only its owner changes who can see it or deletes it. Deploys follow the rules above, so on an unlocked site a colleague may publish to it without being able to open it; lock it if that matters. Such a colleague, and any deploy token, always uploads the whole folder instead of only the changes, and the site's tokens and custom domains stay hidden from people who can't open it.

When the last deploy of a site was by someone else, the CLI asks you to type the site name before replacing their work.

## One deploy at a time

While someone publishes a site, another deploy of the same site (yours from another terminal too) and any rollback are refused with a message saying who and since when:

```
mario@… is publishing this site (since 14:02); retry in a moment or use --takeover
```

- The site is free again as soon as that deploy ends.
- A deploy you interrupt with Ctrl-C, or that fails, frees it immediately. One whose CLI died or lost the network frees it after two minutes without activity.
- A deploy interrupted before publishing publishes nothing. If publishing had already started it may still complete; the CLI says so, so check the site before running it again.
- `--takeover` stops the other deploy without publishing (unless it was already publishing: then it finishes and yours publishes right after). Use it when you know the other deploy is stale, not by reflex.
- Deleting a site always works and ends a deploy in progress.

## Rollback

```bash
quick rollback launch
```

Every deploy keeps the previous version. `rollback` swaps the two, so a second rollback restores what you just undid.

Publishing is atomic, on local disk and on S3 alike: the new version is written aside and becomes visible all at once. Visitors see the old version or the new one, never a mix. A deploy that fails leaves the site as it was.

## Delete

```bash
quick delete launch
```

This removes the files and the settings of the site and cannot be undone. The CLI asks for confirmation; for a public or code-protected site you type its name.
