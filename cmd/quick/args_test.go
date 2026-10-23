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

// quick limit [<site>] [<size>|default]: a single positional is the size when
// it reads as one, else the site.
func TestLimitArgs(t *testing.T) {
	cases := []struct {
		in         []string
		site, size string
	}{
		{nil, "", ""},
		{[]string{"launch"}, "launch", ""},
		{[]string{"1.5GB"}, "", "1.5GB"},
		{[]string{"default"}, "", "default"},
		{[]string{"launch", "800MB"}, "launch", "800MB"},
	}
	for _, c := range cases {
		site, size := limitArgs(c.in)
		if site != c.site || size != c.size {
			t.Errorf("limitArgs(%q) = %q, %q; want %q, %q", c.in, site, size, c.site, c.size)
		}
	}
}
