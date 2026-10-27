---
title: Self-hosting
description: Run the quick server: two containers, a wildcard DNS record, an identity provider, local disk or S3.
---

# Self-hosting

A quick server is two containers behind your reverse proxy:

- **quick-server** (`ghcr.io/zupolgec/quick-server`, built for amd64 and arm64 on every release) serves the sites, the API, the dashboard and the install scripts.
- **oauth2-proxy** handles the company sign-in for browsers.

The repository's [`docker-compose.yaml`](https://github.com/zupolgec/quick/blob/main/docker-compose.yaml) runs both. It is written for Coolify with caddy-docker-proxy labels, and reads as a reference for any other setup.

## How requests flow

```
browser ─https─> your proxy (TLS for the domain and *.domain) ─> quick-server:8080
  quick.acme.com          API, dashboard, /oauth2/* (to oauth2-proxy), /install.sh
  <site>.quick.acme.com   the site's access check, then its files

CLI ─ sign-in with PKCE on a loopback port ─> ID token ─> quick.acme.com/api/*
```

The base domain is the control plane; every subdomain is a site. The proxy only forwards to quick-server, and quick-server decides everything by host name.

## DNS and TLS

Point both the base domain and its wildcard at the server:

```
quick.acme.com     A  203.0.113.10
*.quick.acme.com   A  203.0.113.10
```

The proxy needs a wildcard certificate for `*.quick.acme.com`, which means a DNS-01 challenge with your DNS provider. With that in place new sites need no DNS or certificate work.

## Identity provider

**Google Workspace** (the default) needs two OAuth clients:

- a **Web application** client for oauth2-proxy, with redirect URI `https://quick.acme.com/oauth2/callback`;
- a client for the CLI with redirect URI `http://127.0.0.1:8765/callback`. Either a **Desktop app** client (set only its ID, the CLI uses PKCE without a secret) or the same Web client again (set ID and secret: the secret is handed to the CLI by the server, which is acceptable for a PKCE loopback client).

Only accounts of the Workspace domains in `QUICK_ALLOWED_DOMAINS` get in: a personal Google account that uses a company address does not. Each sign-in asks which account to use.

**Any OIDC provider** (Zitadel, Keycloak, Authentik and others): set `QUICK_OIDC_ISSUER=https://idp.acme.com`, a Web client for oauth2-proxy, and a native PKCE client for the CLI with the same loopback redirect. Tokens are verified locally against the issuer; the domain checked is the part of the email after `@`, and only if the email is verified.

## Configuration

| Variable | Meaning |
|---|---|
| `QUICK_BASE_DOMAIN` | The base domain, e.g. `quick.acme.com` |
| `QUICK_ALLOWED_DOMAINS` | Email domains allowed in: `acme.com`, `acme.com,acme.io`, or `*` for any account |
| `QUICK_CLI_CLIENT_ID`, `QUICK_CLI_CLIENT_SECRET` | The CLI's OAuth client (secret only when reusing a Web client) |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | oauth2-proxy's Web client (also for OIDC providers) |
| `COOKIE_SECRET` | oauth2-proxy cookie secret: `openssl rand -base64 32` |
| `QUICK_META_SECRET` | Signs access codes and site cookies: `openssl rand -base64 48` |
| `QUICK_OIDC_ISSUER` | The issuer URL, for providers other than Google |
| `QUICK_OWNERSHIP` | `free` (default), `shared` or `owned`; see [Access and safety](/docs/access) |
| `QUICK_ADMINS` | Administrator emails, comma-separated: they can raise a single site's size limit |
| `QUICK_SMTP_URL`, `QUICK_MAIL_FROM` | Email sending, e.g. `smtp://user:pass@host:587` (or `smtps://` on 465) and `quick <quick@example.com>`. Lets chosen-people sites include people outside the company, who sign in with an email link |
| `QUICK_SITE_LIMIT` | Size limit of every site, per version (default `100MB`) |
| `QUICK_SITE_LIMIT_MAX` | Highest limit an administrator can give a site (default `2GB`) |
| `QUICK_MIN_FREE` | On local disk, refuse deploys that would leave less free space than this (default `5GB`) |
| `QUICK_STORAGE` | `local` (default) or `s3` |
| `QUICK_S3_ENDPOINT`, `_REGION`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY`, `_PREFIX`, `_USE_SSL` | S3-compatible storage |
| `QUICK_HOST_DIR` | Host folder for the data, to run several instances on one host |
| `QUICK_CHUNK_DIR` | Where uploaded chunks wait to be published (default: the system temp folder; cleared on publish or after 24 hours) |

The server fails closed: outside local development it refuses to start without `QUICK_META_SECRET`, `QUICK_ALLOWED_DOMAINS` and the CLI client ID. To admit any account you write `*`, explicitly.

## Storage

**Local disk** keeps site files and settings in two mounted folders. Files identical in the live and the previous version are stored once (hardlinks), so the rollback copy costs only what changed. Next to the settings the server keeps a map of the chunks of the live and the previous version, so incremental deploys never re-read the site; if the map is missing it is rebuilt from the files.

**S3-compatible storage** (Hetzner, MinIO, R2, AWS and others) makes the container stateless. Each deploy writes to `versions/<site>/<id>/` and then switches one pointer, `live/<site>.json`, which names the live and the previous version; rollback swaps them. Old versions are removed by later deploys after a 15-minute grace period, so requests in flight and other instances can finish reading them. Other instances see a new pointer within a few seconds.

The bucket needs no versioning and no conditional writes. A lifecycle rule that removes incomplete multipart uploads is a good idea. One known limit: without conditional writes, a pointer write delayed by the network could land after a later deploy and bring the site back to that version. It is very unlikely; deploying again fixes it.

## Custom domains

Custom domains need on-demand TLS on the proxy and a plain HTTP catch-all, on exactly one quick instance per proxy. In the reference compose they are switched on with `QUICK_CD_PREFIX=caddy`: the labels are named `${QUICK_CD_PREFIX:-quick_off}_1` and `_2`, so without the variable the proxy ignores them. The proxy must ask that instance before issuing a certificate (`on_demand_tls.ask`) and must have no catch-all of its own: quick answers 503 over HTTP to unknown domains.

## Upgrading

Release tags build the CLI binaries and the server image. Pull `ghcr.io/zupolgec/quick-server:latest` (or a version tag) and recreate the container. Users update the CLI with `quick upgrade`.

## Local development

```bash
QUICK_DEV_NOAUTH=1 QUICK_BASE_DOMAIN=quick.localhost \
  QUICK_SITES_DIR=./sites QUICK_META_DIR=./meta QUICK_META_SECRET=dev \
  go run ./cmd/quick-server
```

`QUICK_DEV_NOAUTH=1` skips sign-in checks and is for local development only.
