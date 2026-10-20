package main

import "testing"

// With one positional, a command taking "[<site>] <arg>" must read it as the
// argument and leave the site to the folder's .quick file.
func TestSiteAndArg(t *testing.T) {
	cases := []struct {
		in        []string
		site, arg string
	}{
		{nil, "", ""},
		{[]string{"www.example.com"}, "", "www.example.com"},
		{[]string{"launch", "www.example.com"}, "launch", "www.example.com"},
	}
	for _, c := range cases {
		site, arg := siteAndArg(c.in)
		if site != c.site || arg != c.arg {
			t.Errorf("siteAndArg(%q) = %q, %q; want %q, %q", c.in, site, arg, c.site, c.arg)
		}
	}
}
