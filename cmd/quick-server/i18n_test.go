package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/storage"
)

func TestPickLang(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		accept string
		want   lang
	}{
		{"default english", "/", "", langEN},
		{"italian browser", "/", "it-IT,it;q=0.9,en;q=0.8", langIT},
		{"english browser", "/", "en-US,en;q=0.9", langEN},
		{"q-order picks italian", "/", "en;q=0.3, it;q=0.9", langIT},
		{"unsupported falls back", "/", "fr-FR,fr;q=0.9", langEN},
		{"query overrides header", "/?lang=it", "en-US,en;q=0.9", langIT},
		{"query english over italian header", "/?lang=en", "it-IT,it", langEN},
		{"invalid query ignored", "/?lang=fr", "it-IT", langIT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, c.url, nil)
			if c.accept != "" {
				r.Header.Set("Accept-Language", c.accept)
			}
			if got := pickLang(r); got != c.want {
				t.Errorf("pickLang = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSSOPageLocalized(t *testing.T) {
	s := &server{baseDomain: "example.test"}

	en := httptest.NewRecorder()
	s.renderSSOPage(en, httptest.NewRequest(http.MethodGet, "/", nil), "foo.example.test")
	if b := en.Body.String(); !strings.Contains(b, `<html lang="en"`) || !strings.Contains(b, "Sign-in required") {
		t.Errorf("english SSO page not localized:\n%s", b)
	}
	if v := en.Header().Get("Vary"); !strings.Contains(v, "Accept-Language") {
		t.Errorf("missing Vary: Accept-Language, got %q", v)
	}

	it := httptest.NewRecorder()
	ri := httptest.NewRequest(http.MethodGet, "/", nil)
	ri.Header.Set("Accept-Language", "it-IT,it;q=0.9")
	s.renderSSOPage(it, ri, "foo.example.test")
	if b := it.Body.String(); !strings.Contains(b, `<html lang="it"`) || !strings.Contains(b, "Accesso richiesto") {
		t.Errorf("italian SSO page not localized:\n%s", b)
	}
}

func TestLandingLocalized(t *testing.T) {
	s := &server{baseDomain: "quick.example.test"}

	en := httptest.NewRecorder()
	s.handleApexRoot(en, httptest.NewRequest(http.MethodGet, "/", nil))
	b := en.Body.String()
	if !strings.Contains(b, `<html lang="en"`) || !strings.Contains(b, "Get started") {
		t.Errorf("english landing not localized:\n%s", b)
	}
	if !strings.Contains(b, "curl -fsSL https://quick.example.test/install.sh | sh") {
		t.Errorf("landing missing the install one-liner:\n%s", b)
	}
	if !strings.Contains(b, `href="/dashboard"`) {
		t.Errorf("landing missing the dashboard link:\n%s", b)
	}
	if !strings.Contains(b, `class="brand"`) || !strings.Contains(b, `/img/logo.png`) {
		t.Errorf("landing missing the brand logo:\n%s", b)
	}
	if !strings.Contains(b, "Publish a folder, get a URL.") {
		t.Errorf("landing missing the headline:\n%s", b)
	}

	it := httptest.NewRecorder()
	ri := httptest.NewRequest(http.MethodGet, "/?lang=it", nil)
	s.handleApexRoot(it, ri)
	if b := it.Body.String(); !strings.Contains(b, `<html lang="it"`) || !strings.Contains(b, "Come iniziare") {
		t.Errorf("italian landing not localized:\n%s", b)
	}

	// "/" is the landing; anything else under the apex root is 404.
	nf := httptest.NewRecorder()
	s.handleApexRoot(nf, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if nf.Code != http.StatusNotFound {
		t.Errorf("/nope: code %d, want 404", nf.Code)
	}
}

func TestCodeFormLocalized(t *testing.T) {
	en := httptest.NewRecorder()
	renderCodeForm(en, langEN, "foo.example.test", "https://foo.example.test/", true)
	if b := en.Body.String(); !strings.Contains(b, `<html lang="en"`) || !strings.Contains(b, "Wrong code, try again.") {
		t.Errorf("english code form not localized:\n%s", b)
	}

	it := httptest.NewRecorder()
	renderCodeForm(it, langIT, "foo.example.test", "https://foo.example.test/", true)
	if b := it.Body.String(); !strings.Contains(b, `<html lang="it"`) || !strings.Contains(b, "Codice errato, riprova.") {
		t.Errorf("italian code form not localized:\n%s", b)
	}
}

func TestDashboardPagesLocalized(t *testing.T) {
	st, err := storage.New(storage.Config{Kind: "local", SitesDir: t.TempDir(), MetaDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{store: st, baseDomain: "example.test", meta: newMetaStore(st, []byte("secret"), 0)}
	putSite(t, st, "demo", map[string]string{"index.html": "x"})
	if err := s.meta.save("demo", policy{CreatedBy: "a@example.test", Tokens: []siteToken{{ID: "t1", Name: "ci", Scopes: []string{"deploy"}}}}); err != nil {
		t.Fatal(err)
	}
	itReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Language", "it-IT")
		return r
	}

	dash := httptest.NewRecorder()
	s.renderDashboard(dash, langIT, "a@example.test")
	site := httptest.NewRecorder()
	s.renderDashboardSitePage(site, itReq(), "demo", "a@example.test", "qk_secret")
	for name, body := range map[string]string{"dashboard": dash.Body.String(), "site page": site.Body.String()} {
		for _, en := range []string{">Manage<", "Back to dashboard", "Deploy tokens", "Create token", "Revoke", "never used", "It will not be shown again", ">Name<", ">Expires<", "90 days"} {
			if strings.Contains(body, en) {
				t.Errorf("%s in italian still contains %q", name, en)
			}
		}
	}
	if !strings.Contains(dash.Body.String(), ">Gestisci<") {
		t.Error("dashboard: missing italian manage label")
	}
}
