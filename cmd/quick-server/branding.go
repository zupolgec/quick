package main

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

// Shared branding for the server-rendered pages: the Quick design language
// (self-hosted Manrope display + system body, Quick Violet → Electric Violet).
// The logo and favicon are raster assets (PNG) for now; all assets are embedded
// in the binary and served same-origin from /fonts and /img — no external deps.

//go:embed fonts/manrope-latin.woff2 img/logo.png img/logo-dark.png img/favicon.png
var assets embed.FS

// handleAsset serves the embedded fonts/images. Wired in route() for every host
// so asset URLs are always same-origin (no CORS, works on site subdomains too).
func (s *server) handleAsset(w http.ResponseWriter, r *http.Request) {
	p := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
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
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

// brandHead loads the favicon (the Q from the logo). Concatenated into each <head>.
const brandHead = `<link rel="icon" type="image/png" href="/img/favicon.png"><link rel="apple-touch-icon" href="/img/favicon.png">`

// logoImg is the "Quick" wordmark, swapping a light/dark variant by color scheme.
const logoImg = `<picture><source srcset="/img/logo-dark.png" media="(prefers-color-scheme:dark)"><img src="/img/logo.png" alt="Quick" class="logo" width="422" height="120"></picture>`

// brandLink is the clickable logo for page headers; brandWordmark the static one.
const brandLink = `<a class="brand" href="/">` + logoImg + `</a>`
const brandWordmark = `<span class="brand">` + logoImg + `</span>`

// brandCSS holds the design tokens, base typography and the shared brand/button
// primitives. Each page appends its own layout CSS after this. Manrope (display)
// is self-hosted; body text uses the native system stack.
const brandCSS = `@font-face{font-family:'Manrope';font-style:normal;font-weight:200 800;font-display:swap;src:url("/fonts/manrope-latin.woff2") format("woff2")}
:root{
  --bg:#f5f6fb;--card:#fff;--ink:#0d1832;--fg:#0d1832;--muted:#6b7280;--border:#e6e8f0;
  --brand:#4e36f5;--brand-2:#6b57ff;--ring:#4e36f5;
  --btn:linear-gradient(135deg,#4e36f5,#6b57ff);--btn-fg:#fff;--accent:#4e36f5;
  --err:#dc2626;--err-bg:#fef2f2;--ok:#16a34a;
  --font-head:'Manrope',-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;
  --font-body:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,system-ui,sans-serif}
@media (prefers-color-scheme:dark){:root{
  --bg:#080b16;--card:#121829;--ink:#eef0f6;--fg:#eef0f6;--muted:#98a2b8;--border:#222a3d;
  --brand:#6b57ff;--brand-2:#8b7bff;--ring:#8b7bff;--accent:#a89bff;
  --err:#f87171;--err-bg:#2a1416;--ok:#34d399}}
*{box-sizing:border-box}
html,body{width:100%;overflow-x:hidden}
body{margin:0;min-height:100vh;font:400 16px/1.6 var(--font-body);background:var(--bg);color:var(--fg);-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
h1,h2,h3{font-family:var(--font-head)}
a{color:var(--accent);text-decoration:none}a:hover{text-decoration:underline}
a:focus-visible,button:focus-visible,input:focus-visible{outline:none;box-shadow:0 0 0 3px color-mix(in srgb,var(--brand) 35%,transparent)}
.brand{display:inline-flex;align-items:center}
.brand:hover{text-decoration:none}
.logo{height:1.5rem;width:auto;display:block}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:.45rem;font-family:var(--font-head);font-weight:700;border:0;border-radius:12px;background:var(--btn);color:var(--btn-fg);cursor:pointer;transition:filter .15s}
.btn:hover{filter:brightness(1.07);text-decoration:none}`
