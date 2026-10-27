---
title: Site conventions
description: How quick serves your files, what never gets uploaded, _redirects, _headers, search engines, single-page apps, precompressed builds and quick.js.
---

# Site conventions

quick follows the conventions most static hosts use. There is no configuration file: everything is a file in your folder.

## URLs

- `index.html` is the index of its folder.
- `/about` serves `about.html`; `about/index.html` is served at `/about/`.
- HTML pages have one canonical address: `/about.html` and `/about/index.html` redirect to `/about`, and `/about` redirects to `/about/` when it is a folder.

## Not found and single-page apps

- `404.html` is shown, with status 404, for paths that don't exist. It can sit at the root or in any folder: the one nearest to the missing path wins.
- `200.html` at the root is the app shell of a single-page app: it is served, with status 200, for every route that is not a file.
- Without `200.html`, a missing path is a real 404. There is no silent fallback to the home page.

## `_redirects`

A `_redirects` file at the root of the site defines redirects, rewrites and proxies, one rule per line, in the Netlify style:

```
/old      /new                            301   # redirect (302, 307, 308 too; 301 if omitted)
/*        /index.html                     200   # rewrite: catch-all for a single-page app
/api/*    https://api.example.com/:splat  200   # proxy: call an API from the same origin
```

- Rules apply only when no file matches the request (files always win), and before `200.html`.
- The first matching rule wins. Invalid lines are ignored.
- The only wildcard is a trailing `/*`, available as `:splat` in the destination. The query string is kept.
- A proxy passes requests through to a public `https` host: never to private or internal addresses, never to other quick sites, and no credentials are added.
- Proxied paths follow the site's visibility: a site behind sign-in has its API behind sign-in too.
- The `_redirects` file itself is never served. Changes take effect within a few seconds.

## `_headers`

A `_headers` file at the root of the site sets response headers per path, in the Netlify style: a path, then the headers indented under it.

```
/*
  X-Frame-Options: DENY
/assets/*
  Cache-Control: public, max-age=31536000, immutable
  Access-Control-Allow-Origin: *
```

- Paths are the ones in the address bar: `/about`, not `/about.html`. The only wildcard is a trailing `/*`.
- Every matching rule applies. A header set by several rules gets its values joined with `, `.
- Headers apply to the site's files, `200.html` and `404.html` included. Redirects and proxied responses don't get them.
- Headers quick manages itself can't be set, and their line is ignored: cookies (`Set-Cookie`), sign-in (`WWW-Authenticate`, `Authorization`, `Proxy-*`), how a file is sent (`Content-Type`, `Content-Encoding`, `Content-Length`, `Content-Range`, `Transfer-Encoding`, `ETag`, `Last-Modified`, `Accept-Ranges`, `Location`), security (`X-Content-Type-Options`, `Strict-Transport-Security`, `Access-Control-Allow-Credentials`, `Clear-Site-Data`), `Alt-Svc`, `X-Quick-*` and `X-Forwarded-*`.
- The `_headers` file itself is never served. Changes take effect within a few seconds.

## Search engines

Sites on a quick address are drafts and internal work, so they stay out of search results: every response on `<site>.<your-domain>` carries `X-Robots-Tag: noindex, nofollow`, pages as well as images, fonts and scripts.

- It can't be turned off. An `X-Robots-Tag` in `_headers` adds directives on top of it.
- A site that should be found goes on a [custom domain](/docs/custom-domains), where quick adds nothing.
- Don't block crawlers with `Disallow: /` in `robots.txt`: a crawler that can't fetch a page never sees its noindex and may still list the bare address.

## What is never uploaded

Exclusions are decided in three tiers:

1. **Security, always.** Hidden files (`.git`, `.env`, `.quick` and so on, except `.well-known`) and secrets (`*.pem`, `*.key`, `id_rsa`, keystores). This tier cannot be overridden.
2. **Convenience, by default.** `node_modules/`, `vendor/`, `*.log` and temporary files.
3. **`.quickignore`, if present.** It replaces tier 2. Gitignore syntax, with `!` to bring a file back. `quick ignore` writes one with the defaults already in it.

Run `quick status` or `quick deploy --dry-run` to see what is in and what is out. A folder named `__quick/` at the root is refused: that path belongs to quick.

## Precompressed files and Unity WebGL

`x.js.br`, `x.wasm.gz`, `x.data.br`, `x.json.gz` (Brotli `.br` or gzip `.gz`) are served with the type of the inner file and the right `Content-Encoding`, so the browser decompresses them itself. A Unity WebGL build deploys as it is, with any compression setting: Brotli, Gzip or "decompression fallback" `.unityweb`: when that file is compressed, quick serves it with the right `Content-Encoding` too, which starts faster than Unity's own loader. Other `.gz` files, such as archives, stay normal downloads.

Builds compressed as a whole change everywhere on every build, so they gain little from incremental deploys. Publish them uncompressed if upload time matters more than file size.

## Size limit

A site can take up to 100 MB per version, unless the server sets another default. Bigger sites, such as game builds with video, need an administrator to raise that site's limit (`quick limit <site> 1.5GB`, or the site's page in the dashboard). `quick limit <site>` shows the current one and who the administrators are. A deploy over the limit is refused before anything changes on the site, and the message says who to ask.

## Incremental deploys

The CLI splits files into content-defined chunks of about 64 KiB and uploads only the chunks the server doesn't already have in the published version. Changing part of a large file sends a few chunks; a renamed file sends nothing. The CLI reports what it really sent, for example `uploaded 9.2 KB of 3.1 MB`. Against an older server, or a busy one, it uploads the whole folder.

## `quick.js`

Every site can import a small library from `/__quick/quick.js` (an ES module; types at `/__quick/quick.d.ts`).

```html
<script type="module">
  import quick from "/__quick/quick.js";
  const user = await quick.id.me(); // { email } on sign-in sites, null on public or code sites
</script>
```
