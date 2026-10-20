package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zupolgec/quick/internal/quick"
)

func TestParseAccessArgs(t *testing.T) {
	cases := []struct {
		args []string
		want accessArgs
	}{
		{nil, accessArgs{}},
		{[]string{"foo"}, accessArgs{Name: "foo"}},
		{[]string{"foo", "team"}, accessArgs{Name: "foo", Op: "team"}},
		{[]string{"foo", "public", "--server", "acme"}, accessArgs{Name: "foo", Op: "public", Server: "acme"}},
		{[]string{"foo", "code"}, accessArgs{Name: "foo", Op: "code"}},
		{[]string{"foo", "code", "--code", "s3cret"}, accessArgs{Name: "foo", Op: "code", Code: "s3cret"}},
		{[]string{"--server", "acme", "foo", "only-me"}, accessArgs{Name: "foo", Op: "only-me", Server: "acme"}},
		{[]string{"foo", "add", "a@x.it", "--server", "acme", "b@x.it"}, accessArgs{Name: "foo", Op: "add", Emails: []string{"a@x.it", "b@x.it"}, Server: "acme"}},
		{[]string{"foo", "add", "a@x.it,b@x.it"}, accessArgs{Name: "foo", Op: "add", Emails: []string{"a@x.it", "b@x.it"}}},
		{[]string{"foo", "remove", "a@x.it"}, accessArgs{Name: "foo", Op: "remove", Emails: []string{"a@x.it"}}},
	}
	for _, c := range cases {
		got, err := parseAccessArgs(c.args)
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %+v, want %+v", c.args, got, c.want)
		}
	}
	for _, bad := range [][]string{
		{"foo", "everyone"},       // unknown level
		{"foo", "add"},            // no emails
		{"foo", "remove"},         // no emails
		{"foo", "team", "a@x.it"}, // stray argument
		{"foo", "public", "--code", "x"},
		{"foo", "--nope"},
	} {
		if _, err := parseAccessArgs(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestAccessRequest(t *testing.T) {
	cases := map[string]struct {
		a    accessArgs
		want string
	}{
		"team":    {accessArgs{Op: "team"}, `{"access":"sso"}`},
		"public":  {accessArgs{Op: "public"}, `{"access":"public"}`},
		"code":    {accessArgs{Op: "code", Code: "abc"}, `{"access":"code","code":"abc"}`},
		"only-me": {accessArgs{Op: "only-me"}, `{"access":"people","viewers":[]}`},
		"add":     {accessArgs{Op: "add", Emails: []string{"a@x.it", "b@x.it"}}, `{"add_viewers":["a@x.it","b@x.it"]}`},
		"remove":  {accessArgs{Op: "remove", Emails: []string{"a@x.it"}}, `{"remove_viewers":["a@x.it"]}`},
	}
	for name, c := range cases {
		b, _ := json.Marshal(accessRequest(c.a))
		if string(b) != c.want {
			t.Errorf("%s: %s, want %s", name, b, c.want)
		}
	}
}

// The command talks to the policy endpoint: GET to show, POST to change, and
// the server's refusal reaches the user as is.
func TestAccessCallsPolicyEndpoint(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	status, reply := http.StatusOK, `{"site":"foo","access":"people","created_by":"ann@x.it","viewers":["bob@x.it"],"exists":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotAuth, gotBody = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	cfg := &cliConfig{Server: srv.URL, BaseDomain: "quick.test"}

	res, err := accessCall(cfg, accessArgs{Name: "foo"}, "tok")
	if err != nil || gotMethod != http.MethodGet || gotPath != "/api/site/foo/policy" || gotAuth != "Bearer tok" {
		t.Fatalf("show: err %v, %s %s auth %q", err, gotMethod, gotPath, gotAuth)
	}
	if res.Access != quick.AccessPeople || !reflect.DeepEqual(res.Viewers, []string{"bob@x.it"}) {
		t.Errorf("show: res %+v", res)
	}

	if _, err := accessCall(cfg, accessArgs{Name: "foo", Op: "add", Emails: []string{"carl@x.it"}}, "tok"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/site/foo/policy" || gotBody != `{"add_viewers":["carl@x.it"]}` {
		t.Errorf("add: %s %s body %s", gotMethod, gotPath, gotBody)
	}

	status, reply = http.StatusForbidden, "only the owner (ann@x.it) can change who can see this site\n"
	_, err = accessCall(cfg, accessArgs{Name: "foo", Op: "team"}, "tok")
	if err == nil || !strings.Contains(err.Error(), "only the owner (ann@x.it) can change who can see this site") {
		t.Errorf("refusal: %v", err)
	}
}

func TestDescribeSiteAccess(t *testing.T) {
	cases := []struct {
		res  quick.PolicyResponse
		want []string
		not  []string
	}{
		{quick.PolicyResponse{Access: "sso", CreatedBy: "ann@x.it"}, []string{"everyone at the company", "Owner:   ann@x.it"}, nil},
		{quick.PolicyResponse{Access: "public"}, []string{"public"}, nil},
		{quick.PolicyResponse{Access: "code"}, []string{"access code"}, nil},
		{quick.PolicyResponse{Access: "people", CreatedBy: "ann@x.it"}, []string{"only the owner"}, []string{"Viewers"}},
		{quick.PolicyResponse{Access: "people", CreatedBy: "ann@x.it", Viewers: []string{"bob@x.it", "carl@x.it"}}, []string{"chosen people (2)", "bob@x.it", "carl@x.it"}, nil},
		{quick.PolicyResponse{Access: "people", CreatedBy: "ann@x.it", ViewersHidden: true}, []string{"chosen people", "you're not on the list"}, []string{"only the owner"}},
	}
	for _, c := range cases {
		got := describeSiteAccess(c.res)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%+v: missing %q in\n%s", c.res, w, got)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%+v: unexpected %q in\n%s", c.res, n, got)
			}
		}
	}
}

// In a folder linked with .quick the site is optional: a first word that is an
// access level or add/remove is the operation, not a site name. A site that is
// literally named like a level can still be targeted explicitly.
func TestParseAccessArgsSiteOptional(t *testing.T) {
	cases := []struct {
		args     []string
		name, op string
		emails   int
	}{
		{[]string{"only-me"}, "", "only-me", 0},
		{[]string{"team"}, "", "team", 0},
		{[]string{"code", "--code", "x1"}, "", "code", 0},
		{[]string{"add", "a@example.com", "b@example.com"}, "", "add", 2},
		{[]string{"remove", "a@example.com"}, "", "remove", 1},
		{[]string{"team", "team"}, "team", "team", 0},              // site named "team"
		{[]string{"add", "add", "a@example.com"}, "add", "add", 1}, // site named "add"
		{[]string{"mysite", "only-me"}, "mysite", "only-me", 0},
		{[]string{"mysite"}, "mysite", "", 0},
		{nil, "", "", 0},
	}
	for _, c := range cases {
		a, err := parseAccessArgs(c.args)
		if err != nil || a.Name != c.name || a.Op != c.op || len(a.Emails) != c.emails {
			t.Errorf("%v: got name=%q op=%q emails=%v err=%v", c.args, a.Name, a.Op, a.Emails, err)
		}
	}
}
