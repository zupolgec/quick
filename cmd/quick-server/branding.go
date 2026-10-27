package main

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

// Shared branding for the server-rendered pages, the same flat style as
// quick-cli.com: Hanken Grotesk (self-hosted), only the logo's colours
// (lavender, the letters' navy, violet, a dark ground in dark mode), square
// corners, no shadows or borders. The vector logo is inlined so its letters
// follow each colour scheme; PNGs stay for external links. All assets are
// embedded in the binary and served same-origin from /__quick/fonts and
// /__quick/img on every host (and /fonts, /img on the apex).

//go:embed fonts/hanken-grotesk-latin.woff2 img/logo.png img/logo-dark.png img/logo.svg img/favicon.png img/favicon.svg
var assets embed.FS

//go:embed img/logo-inline.svg
var logoInline string

func isAssetPath(p string) bool {
	return strings.HasPrefix(p, "/fonts/") || strings.HasPrefix(p, "/img/")
}

// handleAsset serves the embedded font/image at asset path p ("/img/logo.png").
// Wired in route() so asset URLs are always same-origin (no CORS, works on
// site subdomains too).
func (s *server) handleAsset(w http.ResponseWriter, r *http.Request, p string) {
	p = path.Clean(strings.TrimPrefix(p, "/"))
	if !strings.HasPrefix(p, "fonts/") && !strings.HasPrefix(p, "img/") {
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "application/octet-stream"
	switch {
	case strings.HasSuffix(p, ".woff2"):
		ct = "font/woff2"
	case strings.HasSuffix(p, ".png"):
		ct = "image/png"
	case strings.HasSuffix(p, ".svg"):
		ct = "image/svg+xml"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

// brandHead loads the favicon (the Q from the logo). Concatenated into each <head>.
const brandHead = `<link rel="icon" type="image/svg+xml" href="/__quick/img/favicon.svg"><link rel="icon" type="image/png" href="/__quick/img/favicon.png"><link rel="apple-touch-icon" href="/__quick/img/favicon.png">`

// brandLink is the clickable logo for page headers; brandWordmark the static one.
var brandLink = `<a class="brand" href="/" aria-label="Quick">` + strings.TrimSpace(logoInline) + `</a>`
var brandWordmark = `<span class="brand" role="img" aria-label="Quick">` + strings.TrimSpace(logoInline) + `</span>`

// brandCSS holds the design tokens, base typography and the shared brand/button
// primitives. Each page appends its own layout CSS after this.
const brandCSS = `@font-face{font-family:'Hanken Grotesk';font-style:normal;font-weight:400 700;font-display:swap;src:url("/__quick/fonts/hanken-grotesk-latin.woff2") format("woff2")}
:root{
  --bg:#e8e6fd;--card:#ffffff;--sunk:#f3f2fe;--ink:#1a2238;--fg:#1a2238;--muted:#5b5878;
  --brand:#4a3ddb;--brand-hover:#3a2ec4;--btn:#4a3ddb;--btn-fg:#fff;--accent:#4a3ddb;--ring:#4a3ddb;
  --err:#b42318;--ok:#4a3ddb;
  --font-head:'Hanken Grotesk',-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;
  --font-body:'Hanken Grotesk',-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;
  --font-code:ui-monospace,SFMono-Regular,Menlo,monospace}
@media (prefers-color-scheme:dark){:root{
  --bg:#0e1018;--card:#181b26;--sunk:#232737;--ink:#ffffff;--fg:#ffffff;--muted:#a3a7b7;
  --accent:#e8e6fd;--ring:#e8e6fd;--err:#ff8a7a;--ok:#e8e6fd}}
*{box-sizing:border-box}
html,body{width:100%;overflow-x:hidden}
body{margin:0;min-height:100vh;font:400 17px/1.55 var(--font-body);letter-spacing:-.005em;background:var(--bg);color:var(--fg);-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
h1,h2,h3{font-family:var(--font-head);font-weight:500;letter-spacing:-.025em}
a{color:var(--accent);text-decoration:underline;text-decoration-color:transparent;text-underline-offset:.2em;transition:text-decoration-color .18s}
a:hover{text-decoration-color:currentColor}
a:focus-visible,button:focus-visible,input:focus-visible,select:focus-visible{outline:2px solid var(--ring);outline-offset:2px}
::selection{background:var(--brand);color:#fff}
.brand{display:inline-flex;align-items:center;color:var(--ink)}
.brand:hover{text-decoration:none}
.logo{height:1.6rem;width:auto;display:block}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:.45rem;font-family:var(--font-head);font-weight:500;border:0;border-radius:0;background:var(--btn);color:var(--btn-fg);cursor:pointer;text-decoration:none;transition:background-color .18s,transform .14s}
.btn:hover{background:var(--brand-hover);text-decoration:none}
.btn:active{transform:scale(.97)}`
