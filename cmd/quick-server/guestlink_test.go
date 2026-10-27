package main

// Email links for outside people: on a "people" site, addresses outside the
// company domains can be listed when the server can send email; they open the
// site with a one-time link asked for on the sign-in page.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zupolgec/quick/internal/quick"
)

const dave = "dave@client.it" // outside the company domain (example.com)

type sentMail struct{ to, subject, body, html string }

type fakeMailer struct{ sent chan sentMail }

func (f *fakeMailer) Send(m mailMessage) error {
	f.sent <- sentMail{m.To, m.Subject, m.Text, m.HTML}
	return nil
}

// next returns the next mail sent, or fails after a short wait.
func (f *fakeMailer) next(t *testing.T) sentMail {
	t.Helper()
	select {
	case m := <-f.sent:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no email sent")
		return sentMail{}
	}
}

// none checks that no mail is sent.
func (f *fakeMailer) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-f.sent:
		t.Fatalf("unexpected email to %s", m.to)
	case <-time.After(100 * time.Millisecond):
	}
}

func newGuestEnv(t *testing.T) (*peopleEnv, *fakeMailer) {
	t.Helper()
	e := newPeopleEnv(t)
	m := &fakeMailer{sent: make(chan sentMail, 10)}
	e.s.mail = m
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: []string{bob, dave}})
	return e, m
}

// post sends a form to path on host with the given cookies.
func (e *peopleEnv) post(host, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://"+host+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.s.route(w, r)
	return w
}

func (e *peopleEnv) visitWith(host, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.s.route(w, r)
	return w
}

var linkRE = regexp.MustCompile(`https://\S+/__quick/email\?\S+`)

// askLink asks a link for email on the app site and returns the one mailed.
func askLink(t *testing.T, e *peopleEnv, m *fakeMailer, email string) string {
	t.Helper()
	w := e.post("app.quick.test", "/__quick/email", url.Values{"email": {email}, "rd": {"https://app.quick.test/page"}})
	if w.Code != http.StatusOK {
		t.Fatalf("ask link: %d %s", w.Code, w.Body)
	}
	mail := m.next(t)
	if mail.to != email {
		t.Fatalf("mail to %q, want %q", mail.to, email)
	}
	link := linkRE.FindString(mail.body)
	if link == "" {
		t.Fatalf("no link in mail:\n%s", mail.body)
	}
	if !strings.Contains(mail.html, `<img src="https://quick.test/img/logo.png"`) {
		t.Fatalf("html part without the logo:\n%s", mail.html)
	}
	if !strings.Contains(mail.html, `href="`+link+`"`) {
		t.Fatalf("html part without the link:\n%s", mail.html)
	}
	return link
}

// useLink opens the link (a page with a button) and confirms it, returning the
// confirmation response.
func useLink(t *testing.T, e *peopleEnv, link string) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	page := e.visitWith(u.Host, u.RequestURI())
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `method="post"`) {
		t.Fatalf("link page: %d %s", page.Code, page.Body)
	}
	if len(page.Result().Cookies()) != 0 {
		t.Fatal("opening the link must not sign in: mail scanners open links too")
	}
	return e.post(u.Host, "/__quick/email", u.Query())
}

func guestCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "quick_guest_app" && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no guest cookie (status %d)", w.Code)
	return nil
}

func TestOutsideViewerNeedsEmail(t *testing.T) {
	e := newPeopleEnv(t)
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessPeople})
	w, _ := e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: []string{dave}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "email") {
		t.Fatalf("without email: %d %s", w.Code, w.Body)
	}
	e.s.mail = &fakeMailer{sent: make(chan sentMail, 1)}
	w, resp := e.setAccess(t, ann, "app", quick.PolicyRequest{AddViewers: []string{dave}})
	if w.Code != http.StatusOK || len(resp.Viewers) != 1 || resp.Viewers[0] != dave {
		t.Fatalf("with email: %d %s", w.Code, w.Body)
	}
}

func TestGuestOpensSiteWithEmailLink(t *testing.T) {
	e, m := newGuestEnv(t)
	page := e.visit("app.quick.test", "", "/page", nil)
	if !strings.Contains(page.Body.String(), `action="/__quick/email"`) {
		t.Fatalf("sign-in page without the email form:\n%s", page.Body)
	}
	link := askLink(t, e, m, dave)
	if !strings.HasPrefix(link, "https://app.quick.test/__quick/email?") {
		t.Fatalf("link %q", link)
	}
	w := useLink(t, e, link)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://app.quick.test/page" {
		t.Fatalf("confirm: %d %s", w.Code, w.Header().Get("Location"))
	}
	c := guestCookie(t, w)
	if time.Until(c.Expires) > 7*24*time.Hour+time.Minute {
		t.Fatalf("session longer than 7 days: %v", c.Expires)
	}
	if got := e.visitWith("app.quick.test", "/", c); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "<h1>app</h1>") {
		t.Fatalf("guest visit: %d %s", got.Code, got.Body)
	}
	// quick.js knows who they are.
	r := httptest.NewRequest(http.MethodGet, "https://app.quick.test/__quick/api/me", nil)
	r.Header.Set("X-Quick", "1")
	r.AddCookie(c)
	api := httptest.NewRecorder()
	e.s.route(api, r)
	if !strings.Contains(api.Body.String(), dave) {
		t.Fatalf("me: %d %s", api.Code, api.Body)
	}
	// the link works once.
	if again := e.post("app.quick.test", "/__quick/email", mustQuery(t, link)); again.Code == http.StatusFound {
		t.Fatal("link reused")
	}
}

// opened reports whether the site's content was served (the sign-in page is a 200 too).
func opened(w *httptest.ResponseRecorder) bool {
	b := w.Body.String()
	return w.Code == http.StatusOK && (strings.Contains(b, "<h1>app</h1>") || strings.Contains(b, "<h1>open</h1>"))
}

func mustQuery(t *testing.T, link string) url.Values {
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestGuestLinkOnlyForListedOutsiders(t *testing.T) {
	e, m := newGuestEnv(t)
	for _, email := range []string{"eve@client.it", bob, ann, "not-an-email"} {
		w := e.post("app.quick.test", "/__quick/email", url.Values{"email": {email}})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "email") {
			t.Fatalf("%s: %d %s", email, w.Code, w.Body)
		}
	}
	m.none(t)
	// same answer as for a listed address: the list stays secret.
	listed := e.post("app.quick.test", "/__quick/email", url.Values{"email": {dave}})
	m.next(t)
	other := e.post("app.quick.test", "/__quick/email", url.Values{"email": {"eve@client.it"}})
	if listed.Body.String() != other.Body.String() {
		t.Fatal("the answer reveals who is on the list")
	}
}

func TestGuestFormOnlyWhereUseful(t *testing.T) {
	e, _ := newGuestEnv(t)
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: []string{bob}})
	if strings.Contains(e.visit("app.quick.test", "", "/", nil).Body.String(), "/__quick/email") {
		t.Fatal("email form on a site without outside people")
	}
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: []string{dave}})
	e.s.mail = nil
	if strings.Contains(e.visit("app.quick.test", "", "/", nil).Body.String(), "/__quick/email") {
		t.Fatal("email form without a mail server")
	}
}

func TestGuestCookieFollowsTheList(t *testing.T) {
	e, m := newGuestEnv(t)
	c := guestCookie(t, useLink(t, e, askLink(t, e, m, dave)))
	// not valid on another site.
	e.setPolicy(t, "open", policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: []string{dave}})
	other := &http.Cookie{Name: "quick_guest_open", Value: c.Value}
	if opened(e.visitWith("open.quick.test", "/", other)) {
		t.Fatal("cookie of one site opens another")
	}
	// removed from the list: out at once.
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: []string{bob}})
	if opened(e.visitWith("app.quick.test", "/", c)) {
		t.Fatal("removed guest still in")
	}
	// back on the list, but the site is now for the whole company.
	e.setPolicy(t, "app", policy{CreatedBy: ann})
	if opened(e.visitWith("app.quick.test", "/", c)) {
		t.Fatal("guest opens a company site")
	}
	// email off: guests are off too.
	e.setPolicy(t, "app", policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: []string{dave}})
	if !opened(e.visitWith("app.quick.test", "/", c)) {
		t.Fatal("guest back on the list kept out")
	}
	e.s.mail = nil
	if opened(e.visitWith("app.quick.test", "/", c)) {
		t.Fatal("guest in without a mail server")
	}
	// a forged cookie.
	e.s.mail = m
	if opened(e.visitWith("app.quick.test", "/", &http.Cookie{Name: "quick_guest_app", Value: "9999999999." + dave + ".00"})) {
		t.Fatal("forged cookie accepted")
	}
}

func TestGuestLinkExpires(t *testing.T) {
	e, m := newGuestEnv(t)
	now := time.Now()
	e.s.guests.now = func() time.Time { return now }
	link := askLink(t, e, m, dave)
	now = now.Add(guestLinkTTL + time.Second)
	if w := e.post("app.quick.test", "/__quick/email", mustQuery(t, link)); w.Code == http.StatusFound {
		t.Fatal("expired link accepted")
	}
}

func TestGuestLinkOnlyForItsSite(t *testing.T) {
	e, m := newGuestEnv(t)
	e.setPolicy(t, "open", policy{CreatedBy: ann, Access: quick.AccessPeople, Viewers: []string{dave}})
	link := askLink(t, e, m, dave)
	if w := e.post("open.quick.test", "/__quick/email", mustQuery(t, link)); w.Code == http.StatusFound {
		t.Fatal("link of one site opens another")
	}
}

func TestGuestLinkRateLimit(t *testing.T) {
	e, m := newGuestEnv(t)
	for range guestMailsPerHour {
		askLink(t, e, m, dave)
	}
	w := e.post("app.quick.test", "/__quick/email", url.Values{"email": {dave}})
	if w.Code != http.StatusOK {
		t.Fatalf("over the limit: %d", w.Code)
	}
	m.none(t)
}

func TestDashboardAddsOutsidePeople(t *testing.T) {
	e, _ := newGuestEnv(t)
	e.setPolicy(t, "app", peoplePolicy(bob))
	page, csrf := e.sitePage(t, ann, "app")
	if !strings.Contains(page, "riceve un link di accesso via email") {
		t.Error("the page doesn't say outside people get an email link")
	}
	w := e.form(ann, "/api/site/app/policy", url.Values{"csrf": {csrf}, "op": {"add"}, "email": {dave}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	page, _ = e.sitePage(t, ann, "app")
	if !strings.Contains(page, dave+`<span class="person-tag">link via email</span>`) || strings.Contains(page, bob+`<span class="person-tag">`) {
		t.Errorf("outside people not marked:\n%s", page)
	}
}
