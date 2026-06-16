package main

import (
	"net/http"
	"strings"
)

// handleSite è il catch-all: risolve il sito dall'host, applica la policy
// (pubblico / codice / SSO) e, se l'accesso è concesso, serve il file.
// È il punto d'estensione futuro: oggi l'unico esito è lo static-serve, domani
// il manifest del sito potrà dirottare certi path verso un backend.
func (s *server) handleSite(w http.ResponseWriter, r *http.Request) {
	host := fwdHost(r)
	sub := subOf(host, s.baseDomain)
	if sub == "" || s.reserved[sub] {
		http.NotFound(w, r)
		return
	}
	p := s.meta.load(sub)
	switch p.Access {
	case "public":
		s.serveSite(w, r, sub)
	case "code":
		if c, err := r.Cookie("quick_access_" + sub); err == nil && s.meta.validAccessCookie(sub, c.Value) {
			s.serveSite(w, r, sub)
			return
		}
		s.redirect(w, r, host, "/__quick/code")
	default: // SSO: delego a oauth2-proxy passandogli il cookie di sessione
		if _, ok := s.checkSSO(r); !ok {
			s.redirect(w, r, host, "/oauth2/sign_in")
			return
		}
		s.serveSite(w, r, sub)
	}
}

// serveSite emula `try_files {path} {path}/index.html /index.html` leggendo dallo
// storage e delegando a http.ServeContent (range, etag, content-type).
func (s *server) serveSite(w http.ResponseWriter, r *http.Request, sub string) {
	p := r.URL.Path
	for _, cand := range []string{p, strings.TrimRight(p, "/") + "/index.html", "/index.html"} {
		rc, fi, err := s.store.OpenFile(sub, cand)
		if err != nil {
			continue
		}
		if fi.ETag != "" {
			w.Header().Set("ETag", fi.ETag)
		}
		// Niente MIME-sniffing: un sito pubblico non deve poter far interpretare
		// un file col tipo sbagliato (vettore XSS). Il content-type resta quello
		// dedotto dall'estensione, con fallback a octet-stream (scarica, non rende).
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, fi.Name, fi.ModTime, rc)
		rc.Close()
		return
	}
	http.NotFound(w, r)
}
