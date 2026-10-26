package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
)

// setupOAuthProxy sets up the reverse proxy to oauth2-proxy for /oauth2/*
// (sign_in, callback, etc.). It preserves the original Host and sets
// X-Forwarded-Proto=https so oauth2-proxy builds correct redirects and cookies
// (.<BASE_DOMAIN>), exactly as the Caddy block did before. oauth2-proxy's own
// HTML error pages are swapped for quick's branded one.
func (s *server) setupOAuthProxy() error {
	target, err := url.Parse(s.oauth2URL)
	if err != nil {
		return err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // oauth2-proxy sees the site's original host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		ModifyResponse: s.brandAuthError,
	}
	s.oauthProxy = rp
	return nil
}

func (s *server) handleOAuth2(w http.ResponseWriter, r *http.Request) {
	s.oauthProxy.ServeHTTP(w, r)
}

// brandAuthError replaces an oauth2-proxy HTML error page (a refused or failed
// sign-in) with quick's own, keeping the status. Everything else passes through.
func (s *server) brandAuthError(resp *http.Response) error {
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode < 400 || ct != "text/html" {
		return nil
	}
	log.Printf("oauth2-proxy %d on %s", resp.StatusCode, resp.Request.URL.Path)
	l := pickLang(resp.Request)
	t := textsFor(l)
	heading, intro := t.AuthFailedHeading, t.AuthFailedIntro
	if resp.StatusCode == http.StatusForbidden {
		heading, intro = t.AuthDeniedHeading, t.AuthDeniedAny
		if d := s.companyDomain(); d != "" {
			intro = fmt.Sprintf(t.AuthDeniedIntro, d)
		}
	}
	var buf bytes.Buffer
	if err := authErrorPage.Execute(&buf, map[string]any{
		"Lang": string(l), "T": t, "Heading": heading, "Intro": intro,
	}); err != nil {
		return err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(&buf)
	resp.ContentLength = int64(buf.Len())
	resp.Header.Set("Content-Length", strconv.Itoa(buf.Len()))
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")
	resp.Header.Del("Content-Encoding")
	resp.Header.Add("Vary", "Accept-Language")
	return nil
}

var authErrorPage = template.Must(template.New("auth-error").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.T.SSOTitle}}</title>` + brandHead + `
<style>` + brandCSS + `
body{display:grid;place-items:center;padding:1.5rem}
.card{width:min(420px,100%);background:var(--card);padding:2.5rem 2rem 2.25rem}
.card .brand{margin-bottom:2rem}
.card .logo{height:2rem}
h1{font-size:1.75rem;line-height:1.1;margin:0 0 .5rem;color:var(--ink)}
p{margin:0 0 1.75rem;color:var(--muted);font-size:1rem;overflow-wrap:anywhere}
.btn{width:100%;min-height:3rem;font-size:1rem}
</style></head><body>
<div class="card">
  ` + brandLink + `
  <h1>{{.Heading}}</h1>
  <p>{{.Intro}}</p>
  <a class="btn" href="/oauth2/sign_in">{{.T.AuthRetry}}</a>
</div></body></html>`))
