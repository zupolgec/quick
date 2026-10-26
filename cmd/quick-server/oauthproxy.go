package main

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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
	msg, reqID := proxyErrorInfo(resp.Body)
	log.Printf("oauth2-proxy %d on %s: %s (request %s)", resp.StatusCode, resp.Request.URL.Path, msg, reqID)
	l := pickLang(resp.Request)
	t := textsFor(l)
	heading, intro := t.AuthFailedHeading, t.AuthFailedIntro
	switch {
	case strings.Contains(msg, "CSRF"):
		heading, intro = t.AuthExpiredHeading, t.AuthExpiredIntro
	case resp.StatusCode == http.StatusForbidden && msg == proxyDefaultMessages[http.StatusForbidden]:
		heading, intro = t.AuthDeniedHeading, t.AuthDeniedAny
		if d := s.companyDomain(); d != "" {
			intro = fmt.Sprintf(t.AuthDeniedIntro, d)
		}
	}
	if msg == proxyDefaultMessages[resp.StatusCode] {
		msg = "" // says nothing the page doesn't
	}
	var buf bytes.Buffer
	if err := authErrorPage.Execute(&buf, map[string]any{
		"Lang": string(l), "T": t, "Heading": heading, "Intro": intro,
		"Message": msg, "RequestID": reqID,
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

// proxyDefaultMessages are oauth2-proxy's generic texts per status, shown when
// it has nothing more specific to say.
var proxyDefaultMessages = map[int]string{
	http.StatusInternalServerError: "Oops! Something went wrong. For more information contact your server administrator.",
	http.StatusNotFound:            "We could not find the resource you were looking for.",
	http.StatusForbidden:           "You do not have permission to access this resource.",
	http.StatusUnauthorized:        "You need to be logged in to access this resource.",
}

var proxyContentRe = regexp.MustCompile(`(?s)<div class="content">\s*(.*?)\s*</div>`)

// proxyErrorInfo reads the "More Info" of an oauth2-proxy error page: its
// message and the request id that finds the failure in its logs.
func proxyErrorInfo(body io.Reader) (msg, reqID string) {
	b, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	for _, m := range proxyContentRe.FindAllSubmatch(b, -1) {
		text := html.UnescapeString(string(m[1]))
		if id, ok := strings.CutPrefix(text, "Request ID: "); ok {
			reqID = id
		} else if msg == "" {
			msg = text
		}
	}
	return msg, reqID
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
details{margin-top:1.5rem;color:var(--muted);font-size:.9rem}
summary{cursor:pointer;width:fit-content}
details p{margin:.6rem 0 0;font-size:.9rem}
details code{display:block;margin-top:.4rem;padding:.5rem .7rem;background:var(--sunk);color:var(--ink);font-family:var(--font-code);font-size:.85rem;overflow-wrap:anywhere}
</style></head><body>
<div class="card">
  ` + brandLink + `
  <h1>{{.Heading}}</h1>
  <p>{{.Intro}}</p>
  <a class="btn" href="/oauth2/sign_in">{{.T.AuthRetry}}</a>
  {{if or .Message .RequestID}}<details>
    <summary>{{.T.AuthDetails}}</summary>
    <p>{{.T.AuthDetailsHint}}</p>
    {{with .Message}}<code>{{.}}</code>{{end}}
    {{with .RequestID}}<code>{{$.T.AuthRequestID}}: {{.}}</code>{{end}}
  </details>{{end}}
</div></body></html>`))
