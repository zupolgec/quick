# quick

**English** · [Italiano](README.it.md)

Website and docs: [quick-cli.com](https://quick-cli.com) · for agents: [llms.txt](https://quick-cli.com/llms.txt)

Internal static hosting in the style of [Shopify's Quick](https://shopify.engineering/quick):
publish a folder of HTML and assets and get `<name>.<your-domain>`. By default a site
is visible only to accounts in the company domain (Google SSO or an OIDC provider),
but you can open it to the public or protect it with a code, and lock it against
overwrites.

Everything is configured through environment variables: no domain or credential
is hardcoded. Storage can be local disk or S3-compatible object storage.

## Install the CLI

Prebuilt binary, no Go required (downloaded from the latest release):

```bash
# macOS / Linux
curl -fsSL https://quick-cli.com/install.sh | sh
# Windows (PowerShell)
irm https://quick-cli.com/install.ps1 | iex
```

Every quick server serves the same scripts on its own domain too
(`https://<your-domain>/install.sh`).

Or, with Go:

```bash
go install github.com/zupolgec/quick/cmd/quick@latest   # ends up in $(go env GOBIN)
```

To update: `quick upgrade` (downloads the latest release and replaces the binary;
`quick upgrade --check` only checks). If you installed with Go, run
`go install …@latest` again.

## Publish a site

```bash
export QUICK_SERVER=https://quick.example.com   # once (or use --server)
quick login                                     # sign in through the browser
quick deploy foo ./mysite                       # -> https://foo.quick.example.com
```

Several servers from the same machine (one per company, say): each keeps its own login.
The server is chosen in this order: `--server`, `QUICK_SERVER`, the folder's `.quick`,
the default server. `quick login --server X` makes X the default, `quick servers`
lists the known servers, `quick servers use X` changes the default. A server you have
already used can be named by a word of its address (`--server 16bit` for
`quick.16bit.cloud`). The first deploy from a folder saves the server in `.quick`:
from then on, in that folder, you don't need to give it.

The syntax is `quick deploy <site> [folder]` (folder optional, default the current
one). Without `<site>` it uses the folder's `.quick` or, failing that, the name of
the folder itself.

A deploy is a **mirror**: it uploads the folder and replaces the whole site (removed
files disappear). Before publishing the CLI shows a summary and **asks for
confirmation** (`--yes` to skip it, `--dry-run` to see what would be uploaded
without publishing); a deploy with no files is **blocked** (`--force` to empty the
site on purpose). New subdomains are instant (the wildcard already covers the certificate).

The upload is **incremental**: the CLI splits files into chunks (by content, about
64 KiB each) and sends only those the server doesn't already have in the published
version. Republishing a build where part of a large file changed (for example
Unity's `.data`) transfers a few chunks; a renamed file isn't uploaded again at all.
The CLI reports how much it really sent ("uploaded 1.2 MB of 44 MB"). With a server
that doesn't support it, or that is momentarily busy, it uploads the whole folder as
before. Builds compressed as a whole (for example Unity with Gzip or Brotli
compression) change everywhere on every build: to benefit from incremental deploys,
publish them uncompressed.

**What isn't published**, in three tiers: (1) security, always: hidden files
(`.git`, `.env`, `.quick`… except `.well-known`) and secrets (`*.pem`, `*.key`,
`id_rsa`, keystores); (2) convenience, default and overridable: `node_modules/`,
`vendor/`, `*.log`, temporary files; (3) `.quickignore`, if present, is the source of
truth for the convenience exclusions (gitignore syntax, with `!`). Create it with
`quick ignore`.

**Served-site conventions**: `index.html` is a folder's index; `/about` serves
`about.html`, while `about/index.html` is served at `/about/`. HTML pages have
canonical URLs: `/about.html` → `/about`, `/about/index.html` → `/about` and, if it is
a folder, `/about` → `/about/`. `404.html` can sit at the root or in the folder
nearest to the missing path (status 404); `200.html` at the root is the SPA app shell
(served with 200 for routes that aren't files). Without `200.html`, a missing path
gives a real 404.

**Precompressed files and Unity WebGL**: `x.js.br`, `x.wasm.gz`, `x.data.br`,
`x.json.gz` (Brotli `.br` or gzip `.gz`) are served with the type of the inner file
and `Content-Encoding`, so the browser decompresses them itself. A Unity WebGL build
is published as it is, with any compression (Brotli, Gzip or "decompression
fallback" `.unityweb`: if the file is compressed, quick serves it with the right
`Content-Encoding` and the browser decompresses it itself, faster than Unity's
loader). Other `.gz` files (archives) stay normal downloads.

**`quick.js` library**: every site can import `/__quick/quick.js` (ESM, types at
`/__quick/quick.d.ts`). `await quick.id.me()` returns the signed-in user (`{email}`)
on SSO sites, `null` on public or code-protected ones.

```html
<script type="module">
  import quick from "/__quick/quick.js";
  const user = await quick.id.me();
</script>
```

The `/__quick/` path is reserved for quick: a deploy containing a `__quick/` folder
at the root of the site is refused.

**`_redirects`** (Netlify style): a `_redirects` file at the root of the site defines
redirects, rewrites and proxies per path. Rules apply only when no file matches the
request (files always win) and before `200.html`; the first matching rule wins. One
line per rule:

```
/old       /new                            301   # redirect (also 302/307/308; 301 if omitted)
/*         /index.html                     200   # local rewrite: SPA catch-all
/api/*     https://api.example.com/:splat  200   # proxy: the API answers same-origin (no CORS)
```

The only wildcard is a trailing `/*`, referenced as `:splat` in the destination; the
query string is preserved; invalid lines are ignored. The proxy is a pass-through to
public `https` hosts (never private or internal-network IPs, never quick subdomains;
no injected keys, for now). Proxied paths follow the site's visibility (SSO, code,
public), so a protected site has its APIs protected too. The `_redirects` file is
never served; changes propagate within a few seconds.

**`_headers`** (Netlify style): a `_headers` file at the root of the site sets
response headers per path. A path line, then the headers indented under it:

```
/*
  X-Frame-Options: DENY
/assets/*
  Cache-Control: public, max-age=31536000, immutable
  Access-Control-Allow-Origin: *
```

Paths are the ones in the address bar (`/about`, not `/about.html`); the only
wildcard is a trailing `/*`. Every matching rule applies, and a header set by
several rules gets its values joined with `, `. Headers apply to the site's files,
`200.html` and `404.html` included, not to redirects or proxied responses. Headers
quick manages itself can't be set (the line is ignored): `Set-Cookie`,
`WWW-Authenticate`, `Authorization`, `Proxy-*`, `Content-Type`, `Content-Encoding`,
`Content-Length`, `Content-Range`, `Transfer-Encoding`, `ETag`, `Last-Modified`,
`Accept-Ranges`, `Location`, `X-Content-Type-Options`, `Strict-Transport-Security`,
`Alt-Svc`, `Clear-Site-Data`, `Access-Control-Allow-Credentials`, `X-Quick-*`,
`X-Forwarded-*` and hop-by-hop headers. The `_headers` file is never served; changes
propagate within a few seconds.

**Search engines**: every response on a `<site>.<domain>` address carries
`X-Robots-Tag: noindex, nofollow`, pages as well as images, fonts and scripts, so
drafts stay out of search results. It can't be turned off: an `X-Robots-Tag` in
`_headers` adds directives on top of it. A site that must be found goes on a custom
domain (`quick domains add`), where quick adds nothing. Don't block crawlers with
`Disallow: /` in `robots.txt`: a crawler that can't fetch a page never sees its
noindex and may still list the bare address.

`quick status` shows server, login, site visibility and what the deploy would upload.
`quick skill` publishes an Agent Skill (`SKILL.md`) that teaches an agent how to use
the CLI. `SKILL.md` is an open cross-vendor format (Claude Code, Codex, Gemini,
Cursor…): default `~/.claude/skills/quick/`, or `--target codex|gemini|…`, `--project`
(the repository folder), `--all` (every known agent). When the CLI is updated, quick
`SKILL.md` files already installed for Claude/Codex/Gemini are refreshed silently on
the first operational command; no new skills are created without `quick skill`.

## Deploys from CI and web agents

For environments without a browser or loopback login (GitHub Actions, Claude Code on
the web, CI runners) use a site-scoped **deploy token**:

```bash
quick token create foo --name github-actions --expires 90d
# save the value, printed only once, as QUICK_API_TOKEN
QUICK_API_TOKEN=qk_... quick deploy foo ./dist --yes
```

The dashboard (`/dashboard` → site → Deploy tokens) offers the same flow in the
browser: create, list and revoke tokens. Each token is valid for **one site only** and
for the `deploy` scope only: it can't delete, roll back, change visibility or lock, or
create other tokens. Tokens are stored only as an HMAC in the site's metadata and the
cleartext value is shown once.

Intentional limits: 90 days by default, allowed expiries `30d`, `90d`, `180d`,
`365d` or `never`, at most 20 tokens per site, names up to 40 characters. In `free`
mode whoever can modify the site can also manage its tokens; in `shared` and `owned`
the same restrictions as policy changes apply.

The first deploy writes a **`.quick`** file in the **current folder** (site name,
server and the published sub-folder), so from there you can repeat without
arguments: `quick deploy`, `quick publish`, and so on. Example: `quick deploy foo ./build`
writes `.quick` with `dir: build`, so a bare `quick deploy` from the same folder
republishes `./build` to `foo`, even if `build` is regenerated every time.
(`.quick` isn't uploaded.) If you try to deploy to a different site from the linked
one, the CLI points it out and asks for confirmation.

The server can be given as a bare domain (`quick.example.com`) or a full URL; the CLI
adds `https://`, also tries `deploy.<domain>`, and remembers whichever answers.

## Custom domains

```bash
quick domains add foo www.example.com   # prints the DNS records to create
quick domains foo                       # status: active or waiting for DNS
quick domains rm foo www.example.com
```

At the domain's DNS provider, whichever it is (DNS only, no CDN proxy):
`www.example.com CNAME foo.<domain>`. A bare domain like `example.com` can't have a
CNAME: use ALIAS/CNAME flattening, or the same A record, plus a TXT record
`_quick.example.com` with the site name (`foo`), which tells quick which site the
domain is for.

The domain stays "waiting" until the record resolves, then activates on its own;
the certificate is issued on the first visit. A domain belongs to one site and moves
to another only when its DNS points there (for a bare domain: when the TXT names it). Public and code-protected sites are served
on the custom domain; company-SSO ones redirect to `<site>.<domain>`, because the
sign-in doesn't carry across domains. The domain's DNS is checked through public
resolvers (1.1.1.1, 8.8.8.8), so the domain activates as soon as the record exists.

On the proxy side you need on-demand TLS and an "any domain" HTTP block. They are
switched on with the variable `QUICK_CD_PREFIX=caddy` on a single instance per
proxy: the compose labels are named `${QUICK_CD_PREFIX:-quick_off}_1`/`_2`, so
without the variable they become `quick_off_*` and the proxy ignores them; with
`caddy` they become `caddy_1`/`caddy_2` and the proxy reads them. The proxy must have
`on_demand_tls.ask` pointing at that instance and no catch-all of its own (quick
answers 503 over HTTP to unknown domains). Procedure in `docs/custom-domains.md`.

## Visibility and lock

```bash
quick access    foo                      # who can see foo
quick access    foo only-me              # only you (the owner)
quick access    foo add giulia@acme.com  # you plus chosen company accounts
quick access    foo remove giulia@acme.com
quick access    foo team                 # back to everyone at the company (default)
quick publish   foo            # open to anyone, no SSO
quick unpublish foo            # back behind the company SSO (default)
quick private   foo            # access by code (generated and printed)
quick private   foo --code abc # access by a code you choose
quick lock      foo            # from now on only you can overwrite foo
quick unlock    foo
```

Changing visibility is **instant** (just a metadata file). `quick-server` makes the
access decision: public → always served; code → code entry page, then a signed cookie
valid for 7 days; SSO → checks the Google session through oauth2-proxy. **Chosen people** (`only-me`, `add`, `remove`):
SSO plus a list; only the owner (the creator) and the listed accounts get in, others
see a "restricted" page naming the owner (never the list). Only the owner changes the
list or the visibility of such a site, or deletes it, in every ownership mode; deploys
follow the usual rules, and whoever can't open the site (and any deploy token) uploads
the whole folder instead of a delta. In a folder linked with `.quick` the site name is
optional (`quick access only-me`). When the server can send email (`QUICK_SMTP_URL`),
the list can also hold **people outside the company**: on the sign-in page they enter
their address and get a link that works once, for 15 minutes, and keeps them signed in
on that browser for 7 days. Anyone may ask; only listed addresses get an email, and the
answer is the same either way. Removing someone locks them out at once. Without email
the list takes company accounts only. The **lock**
records you (from your Google identity) as owner: others can no longer overwrite it
or change its policy until you `unlock`.

**Administrators** (`QUICK_ADMINS`) see, open and manage every site, chosen-people and
"only me" ones included, and can do on any site whatever its owner can (visibility,
lock, delete, tokens, domains, size limit). The dashboard names them on each site's page.

On deploy the CLI shows **who you are** (`Authenticated as …`) and, if the last
publish of that site was by someone else, asks you to **retype the site name** before
overwriting their work.

**One deploy at a time per site.** While someone publishes a site, another deploy of
the same site (yours too, from another terminal) and rollback are refused with a
message saying who is publishing and since when, for example
`mario@… is publishing this site (since 14:02); retry in a moment or use --takeover`.
The site frees up as soon as that deploy ends. If you interrupt it (Ctrl-C) or it
fails, the CLI frees the site immediately; if the CLI dies abruptly or loses the
network, the site frees itself after two minutes without activity. A deploy
interrupted before publishing publishes nothing; if publishing had already started,
it may still complete (the CLI warns that the deploy may have been published: check
the site before running it again).
`--takeover` (on `deploy` and `rollback`) takes the place of the deploy in progress:
whoever was running it stops without publishing (unless they were already publishing:
then it finishes, and your deploy publishes right after) and the CLI tells them.
Deleting the site is always possible and ends the deploy in progress; if a publish has
already started, the deletion waits for it to finish. Older CLIs respect the turn too:
their upload is refused with the same message.

## Undoing a deploy

```bash
quick rollback foo   # restores the previous version (a second rollback redoes it)
```

Every deploy keeps the previous version: `rollback` swaps them. This works on local
storage and on object storage alike. On local storage, files that didn't change
between the two versions are stored once (hardlinks), so a large unchanged video
doesn't take twice the space.

Publishing is atomic on both: the new version is written aside and becomes visible
all at once only at the end, so visitors see the old version or the new one, never a
mix. A deploy that fails leaves the site as it was.

## Who can modify a site

Who owns a site is tracked at the first deploy (creator) and on every update. The rule
for who can overwrite/delete/change visibility is set on the server with
`QUICK_OWNERSHIP`:

- `free` (default): anyone in the company can do anything.
- `shared`: anyone publishes content, but only the creator deletes or changes
  visibility.
- `owned`: only the creator can act on the site.

On top of that, the creator can `quick lock` their site to reserve changes to
themselves in any mode (`quick unlock` to reopen it).

## Size limits

Every site can take up to **100 MB** per version, so quick stays a place for sites
and not a file store. An administrator (`QUICK_ADMINS`) can raise the limit of a
single site, up to `QUICK_SITE_LIMIT_MAX` (2 GB by default), from the site's page in
the dashboard or from the CLI:

```bash
quick limit foo          # the site's limit, and who can change it
quick limit foo 1.5GB    # administrators only; `default` goes back to 100 MB
```

A deploy over the limit is refused before anything is published, with a message
naming the administrators to ask (or the system administrator, if none is set). On local storage a deploy is also refused when it
would leave less than `QUICK_MIN_FREE` (5 GB by default) free on the disk.

## Deleting a site

```bash
quick delete foo     # removes content and metadata (irreversible)
```

Deletion asks for confirmation; if the site is public or code-protected you must
retype its name.

## Roadmap (static-host conventions, next steps)

"By file, zero config" conventions not implemented yet, in order of usefulness:

- **`Cache-Control` defaults**: HTML `no-cache`, assets with a long `max-age` (best
  with hashed asset names).
- **Compression (gzip + brotli)**: negotiated through `Accept-Encoding`, with `Vary:
  Accept-Encoding` and a distinct ETag per encoding (a cache must not serve the
  brotli body to a client asking for identity). Two complementary levels:
  - _On the fly_: the server compresses compressible responses (html, css, js, json,
    svg, wasm…) above a threshold (~1 KB). It covers every site without the user
    preparing anything; it is the level that gives the real benefit, since nobody
    uploads `.br`/`.gz` by hand. It costs CPU per request and needs the brotli
    dependency (`github.com/andybalholm/brotli`); compressed responses give up
    `http.ServeContent` (no Range on an encoded body, like CDNs).
  - _Precompressed sidecars_: serve `file.css.br`/`.gz` if present and accepted. It
    makes sense **only** if they are produced at deploy time (`PutSite`/CLI) with
    brotli q=11 (better than on the fly and paid only once) so the server knows which
    files have a sidecar and doesn't waste existence probes (relevant on S3, where
    each candidate is a round trip). It becomes the fast path, with on the fly as
    fallback. Probing `.br`/`.gz` blindly on every asset: to be avoided.

Platform and agent integration:

- **`/mcp` endpoint**: expose quick as an MCP server (deploy, status, publish… as tools
  any agent can call, not only as documentation). There are official MCP SDKs in Go,
  so it is the natural route for cross-agent *actions*.

## Architecture

The **apex** (`<BASE_DOMAIN>`) is the control plane: API, authentication, a home page
with instructions and the sites dashboard (both behind SSO). Every **subdomain** is
just a site.

```
browser ──https──> coolify-proxy (caddy-docker-proxy, apex + wildcard TLS via DNS-01)
                     │  labels on quick-server:  <BASE_DOMAIN>, *.<BASE_DOMAIN> -> quick-server:8080
                     ▼
                 quick-server (routes by HOST):
                   <BASE_DOMAIN> (apex):  /api/health|config|deploy|sites|site/<n>/{policy,rollback}
                                          /api/deploy/{plan,chunks,commit} (incremental deploy)
                                          /oauth2/* -> oauth2-proxy (Google SSO, sign_in + callback)
                                          /install.sh, /install.ps1 (public, for the install command)
                                          /, /dashboard -> home and dashboard (signed in) | sign-in page (guest)
                   <sub>.<BASE_DOMAIN>:   per-site policy (public/code/sso) + serves from Storage
                                          /__quick/code -> code page; SSO -> sign-in page
                 Storage: local (bind mount) | S3-compatible (stateless)

CLI quick ── Google PKCE login (loopback) ──> ID token ──> POST <apex>/api/deploy | /api/site/.../…
```

The proxy only does `reverse_proxy` to quick-server: no `file_server` and no files in
`/dynamic`. Routing lives in the container **labels** (graceful reload, no proxy
restart). The Quick-style "backend" model lives under `/__quick/` on every site
(same-origin, behind the same access check as the files): the
`/__quick/quick.js` library (+ types at `/__quick/quick.d.ts`) and the
`/__quick/api/*` API. Today there is `quick.id.me()`; database, email and AI are on
the roadmap (local `docs/`).

## Components

| Path | What |
|---|---|
| `cmd/quick/` | CLI: `login` (PKCE), `deploy`, `publish`/`private`/`lock`; configures itself from `/api/config` |
| `cmd/quick-server/` | Single front: serves sites, policy/gate, deploy, `/oauth2/*`, `/api/config` |
| `internal/quick/` | Shared CLI↔server contract (DTOs, name validation, access modes) |
| `internal/storage/` | Storage backends: `local` (FS) and `s3` (minio-go) |
| `internal/chunk/` | File chunking for incremental deploys (FastCDC) |
| `site/` | [quick-cli.com](https://quick-cli.com): homepage, docs, llms.txt (Cloudflare Worker) |
| `docker-compose.yaml` | Stack for Coolify (Caddy labels + env) |
| oauth2-proxy (env `OAUTH2_PROXY_*`) | Google or OIDC SSO (`QUICK_OIDC_ISSUER`) |

## Configuration (env)

See `.env.example`. In short: `QUICK_BASE_DOMAIN`, `QUICK_ALLOWED_DOMAINS` (one, a list
`a,b`, or `*`), `GOOGLE_CLIENT_ID/SECRET` (OAuth **Web** client for oauth2-proxy),
`COOKIE_SECRET`, `QUICK_META_SECRET`, `QUICK_OWNERSHIP`=`free|shared|owned`,
`QUICK_ADMINS` (administrator emails), `QUICK_SMTP_URL` + `QUICK_MAIL_FROM` (email
sending, for people outside the company on chosen-people sites), `QUICK_SITE_LIMIT` (default site limit, 100MB),
`QUICK_SITE_LIMIT_MAX` (highest limit an administrator can set, 2GB), `QUICK_MIN_FREE`
(free disk space a deploy must leave, 5GB; local storage only),
`QUICK_STORAGE`=`local|s3` (+ `QUICK_S3_*`), `QUICK_HOST_DIR` (host folder for the
data, for several instances on the same host), `QUICK_CHUNK_DIR` (incremental-deploy
chunks waiting to be published; default the system temp folder, removed on publish or
after 24 hours). Next to the metadata the server keeps the chunk map of the published
version and of the previous one (`<site>.<version>.idx` in the metadata folder,
`index/<site>/` on S3), so incremental deploys don't re-read the site; if it is
missing or invalid it is rebuilt from the published files.

On S3 every deploy writes the files to `versions/<site>/<id>/` and then updates, in one
go, the pointer `live/<site>.json`, which names the published version and the
previous one (rollback swaps them). Older versions are removed by later deploys, but
only after a 15-minute grace period from when they left the pointer, so requests still
in flight and other instances can finish reading them; the same applies to the
leftovers of an interrupted deploy. The server caches the pointer for a few seconds:
with several instances on the same bucket the others see the change within that time.
Sites published before this layout (files directly in `sites/<site>/`) keep working
and move to the new layout on their first deploy; the old files are removed by a later
deploy, once the grace period has passed. If the pointer write has an uncertain
outcome (dropped connection) the deploy answers with a server error and the CLI
reports that the outcome is unknown.
Known limit: without conditional writes on the bucket, a pointer write left pending by
a network problem can reach S3 late, even after a later successful deploy, and bring
the site back to that deploy's version. If it arrived after the grace period, the
pointer would name a version already removed (the site would answer 404 until the
next deploy). It is very unlikely; if it happens, just deploy again. An unreadable
pointer blocks the site's deploys with a clear error; deleting the site cleans it up
anyway. The bucket needs nothing special (no versioning, no conditional writes); a
lifecycle rule that deletes incomplete multipart uploads is a good idea.
With an OIDC IdP (e.g. Zitadel): `QUICK_OIDC_ISSUER`, and in
`GOOGLE_CLIENT_ID/SECRET` the IdP's Web client for oauth2-proxy; for the CLI a native
client with PKCE and redirect `http://127.0.0.1:8765/callback`.

Fail-closed: in production `QUICK_META_SECRET`, `QUICK_ALLOWED_DOMAINS` and
`QUICK_OAUTH_CLIENT_ID` are required. If one is missing the server doesn't start (no
insecure default, no accounts admitted by surprise). To admit any Google account use an
explicit `*`.
`QUICK_OIDC_ISSUER` (optional): issuer of a generic OIDC IdP (e.g. `https://idp.16bit.it`).
Empty = Google, as before; when set, tokens are verified locally against that issuer
and the domain checked is the part of the email after the `@` (only if verified). Only
in local development does `QUICK_DEV_NOAUTH=1` skip these checks.

CLI OAuth client (`QUICK_CLI_CLIENT_ID` / `QUICK_CLI_CLIENT_SECRET`): two ways
- **Desktop app** → set only the ID; the CLI uses PKCE without a secret.
- **reusing a Web client** (even the same one as oauth2-proxy) → set ID + secret; the
  secret is served to the CLI through `/api/config` (acceptable for the PKCE-bound
  loopback client). It is the way to reuse an existing Web client without hardcoding
  anything.

## Deploying on Coolify (4.1.x)

1. Create an application from the git repo with the **Docker Compose** build pack and
   turn on **Raw Docker Compose**: Coolify uses `docker-compose.yaml` as it is (image
   `ghcr.io/zupolgec/quick-server:latest`, published on every release). Leave the
   domain field empty: routing lives in the compose labels.
2. Set the env (see above). Coolify generates `SERVICE_BASE64_64_QUICKMETA`. With
   several instances on the same host give each one a different `QUICK_HOST_DIR`.
3. DNS: `<BASE_DOMAIN>` and `*.<BASE_DOMAIN>` to the server. `CF_API_TOKEN` must be in
   the proxy's env (the `caddy.tls.dns` label uses it for the wildcard certificate).
4. Deploy. Updates: a new release tag, then redeploy the app.

The `caddy` label covers apex and wildcard in the same block: the apex serves API,
dashboard and login, the subdomains the sites. The redirect of the OAuth/OIDC client
used by oauth2-proxy is `https://<BASE_DOMAIN>/oauth2/callback`.

## Local development

```bash
QUICK_DEV_NOAUTH=1 QUICK_BASE_DOMAIN=quick.localhost \
  QUICK_SITES_DIR=./sites QUICK_META_DIR=./meta QUICK_META_SECRET=dev \
  go run ./cmd/quick-server
```
`QUICK_DEV_NOAUTH=1` skips token verification (local only). For S3 storage, test with
MinIO in Docker (see `QUICK_S3_*`).

The website lives in `site/`: `npm install`, then `npm run dev` (local) or
`npm run deploy` (Cloudflare). Pages are built from `site/content/`; `skill.md` comes
from the CLI itself and the install scripts from `cmd/quick-server/install/`.

## License

MIT — see [LICENSE](LICENSE).
