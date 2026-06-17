// Package storage astrae dove vivono i file dei siti e i metadata di policy:
// su filesystem locale (bind mount) o su object storage S3-compatibile. Il
// resto di quick-server lavora solo su Backend, ignaro di quale impl c'è sotto.
package storage

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// ErrNotFound: file/oggetto inesistente (per il fallback try_files in serve).
var ErrNotFound = errors.New("storage: not found")

// Tetti anti-bomb applicati durante l'estrazione del tar: il limite sullo stream
// gzip in ingresso non basta, perché un archivio piccolo può espandersi a
// dismisura (gzip bomb) o contenere moltissimi file minuscoli.
// var (non const) per poterli abbassare nei test senza generare archivi enormi.
var (
	maxExtractBytes int64 = 500 << 20 // byte estratti totali (decompressi)
	maxExtractFiles       = 20000     // numero massimo di file
)

var (
	errArchiveTooBig = fmt.Errorf("storage: archivio troppo grande una volta estratto (oltre %d MiB)", maxExtractBytes>>20)
	errTooManyFiles  = fmt.Errorf("storage: l'archivio supera il limite di %d file", maxExtractFiles)
)

// FileInfo accompagna un file aperto. Il content-type lo determina chi serve
// (http.ServeContent) via estensione/sniff, qui basta nome+mtime+etag.
type FileInfo struct {
	Name    string
	ModTime time.Time
	ETag    string
}

// Backend è l'astrazione di storage condivisa da contenuti siti e metadata.
type Backend interface {
	// PutSite rimpiazza l'intero albero del sito col contenuto del tar,
	// conservando la versione precedente per un eventuale Rollback.
	PutSite(site string, tr *tar.Reader) error
	// OpenFile apre un singolo file del sito; ErrNotFound se non esiste/è dir.
	OpenFile(site, p string) (io.ReadSeekCloser, FileInfo, error)
	// DeleteSite rimuove contenuti e metadata del sito; existed=false se non c'era nulla.
	DeleteSite(site string) (existed bool, err error)
	// SiteExists indica se il sito ha contenuti o metadata.
	SiteExists(site string) (bool, error)
	// ListSites elenca i nomi dei siti noti (contenuti o metadata).
	ListSites() ([]string, error)
	// Rollback riporta il sito alla versione precedente (l'ultimo deploy diventa
	// la "prossima"). ok=false se non c'è una versione precedente da ripristinare.
	Rollback(site string) (ok bool, err error)
	// GetMeta restituisce il JSON di policy del sito (ok=false se assente).
	GetMeta(site string) (data []byte, ok bool, err error)
	// PutMeta salva il JSON di policy del sito.
	PutMeta(site string, data []byte) error
}

// Config seleziona e configura il backend.
type Config struct {
	Kind     string // "local" | "s3"
	SitesDir string // local
	MetaDir  string // local
	S3       S3Config
}

// New costruisce il backend secondo Config.Kind.
func New(c Config) (Backend, error) {
	switch c.Kind {
	case "", "local":
		return newLocal(c.SitesDir, c.MetaDir)
	case "s3":
		return newS3(c.S3)
	default:
		return nil, fmt.Errorf("storage: kind %q sconosciuto (usa local|s3)", c.Kind)
	}
}

// cleanRel normalizza un path relativo e blocca il traversal.
func cleanRel(p string) (string, error) {
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("storage: percorso non sicuro: %q", p)
	}
	return rel, nil
}

// ---- backend locale (filesystem) ----

type local struct {
	sitesDir string
	metaDir  string
}

func newLocal(sitesDir, metaDir string) (*local, error) {
	for _, d := range []string{sitesDir, metaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &local{sitesDir: sitesDir, metaDir: metaDir}, nil
}

func (l *local) PutSite(site string, tr *tar.Reader) error {
	tmp := l.uniqueTmp(site, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var extracted int64
	var files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel, err := cleanRel(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		dst := filepath.Join(tmp, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if files++; files > maxExtractFiles {
				return errTooManyFiles
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.Create(dst)
			if err != nil {
				return err
			}
			// Copia limitata al budget residuo (+1 per rilevare lo sforamento):
			// una dimensione dichiarata falsa nell'header non può riempire il disco.
			n, err := io.Copy(f, io.LimitReader(tr, maxExtractBytes-extracted+1))
			f.Close()
			if err != nil {
				return err
			}
			if extracted += n; extracted > maxExtractBytes {
				return errArchiveTooBig
			}
		}
	}

	final := filepath.Join(l.sitesDir, site)
	prev := l.prevPath(site)
	if _, err := os.Stat(final); err == nil {
		// La versione attuale diventa la "precedente" (rollback a un livello):
		// la penultima viene scartata.
		os.RemoveAll(prev)
		if err := os.Rename(final, prev); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		// ripristina la versione precedente se l'avevamo spostata
		if _, e := os.Stat(prev); e == nil {
			os.Rename(prev, final)
		}
		return err
	}
	return nil
}

func (l *local) prevPath(site string) string { return filepath.Join(l.sitesDir, "."+site+".prev") }

// tmpSeq rende unici i path temporanei: due operazioni concorrenti sullo stesso
// sito (oltre al lock per-sito a monte) non condividono mai la stessa dir di lavoro.
var tmpSeq atomic.Uint64

func (l *local) uniqueTmp(site, kind string) string {
	return filepath.Join(l.sitesDir, fmt.Sprintf(".%s.%s.%d.%d", site, kind, os.Getpid(), tmpSeq.Add(1)))
}

func (l *local) ListSites() ([]string, error) {
	set := map[string]bool{}
	if ents, err := os.ReadDir(l.sitesDir); err == nil {
		for _, e := range ents {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				set[e.Name()] = true
			}
		}
	}
	if ents, err := os.ReadDir(l.metaDir); err == nil {
		for _, e := range ents {
			if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".json") {
				set[strings.TrimSuffix(n, ".json")] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func (l *local) Rollback(site string) (bool, error) {
	final := filepath.Join(l.sitesDir, site)
	prev := l.prevPath(site)
	if _, err := os.Stat(prev); err != nil {
		return false, nil // niente versione precedente
	}
	swap := l.uniqueTmp(site, "swap")
	if _, err := os.Stat(final); err == nil {
		if err := os.Rename(final, swap); err != nil {
			return false, err
		}
	}
	if err := os.Rename(prev, final); err != nil {
		os.Rename(swap, final) // ripristina
		return false, err
	}
	// L'ex-versione attuale diventa la nuova "precedente": un secondo rollback la rifà.
	if _, err := os.Stat(swap); err == nil {
		os.Rename(swap, prev)
	}
	return true, nil
}

func (l *local) OpenFile(site, p string) (io.ReadSeekCloser, FileInfo, error) {
	rel, err := cleanRel(p)
	if err != nil {
		return nil, FileInfo{}, err
	}
	full := filepath.Join(l.sitesDir, site, rel)
	f, err := os.Open(full)
	if err != nil {
		return nil, FileInfo{}, ErrNotFound
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		f.Close()
		return nil, FileInfo{}, ErrNotFound
	}
	return f, FileInfo{Name: filepath.Base(full), ModTime: st.ModTime()}, nil
}

func (l *local) DeleteSite(site string) (bool, error) {
	existed, err := l.SiteExists(site)
	if err != nil {
		return false, err
	}
	if err := os.RemoveAll(filepath.Join(l.sitesDir, site)); err != nil {
		return existed, err
	}
	os.RemoveAll(l.prevPath(site))
	if err := os.Remove(l.metaPath(site)); err != nil && !os.IsNotExist(err) {
		return existed, err
	}
	return existed, nil
}

func (l *local) SiteExists(site string) (bool, error) {
	if fi, err := os.Stat(filepath.Join(l.sitesDir, site)); err == nil {
		return fi.IsDir(), nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	_, ok, err := l.GetMeta(site)
	return ok, err
}

func (l *local) metaPath(site string) string { return filepath.Join(l.metaDir, site+".json") }

func (l *local) GetMeta(site string) ([]byte, bool, error) {
	b, err := os.ReadFile(l.metaPath(site))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (l *local) PutMeta(site string, data []byte) error {
	tmp := l.metaPath(site) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.metaPath(site))
}
