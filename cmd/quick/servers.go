package main

import (
	"fmt"
	"slices"
)

// serversCmd lists the known servers (default marked, login status) or, with
// `use <server>`, changes the default one.
func serversCmd(args []string) {
	if len(args) > 0 {
		if args[0] != "use" || len(args) != 2 {
			fatal(fmt.Errorf("usage: quick servers [use <server>]"))
		}
		cfg, err := resolveConfig(args[1], "")
		fatal(err)
		saveServerConfig(cfg, true)
		fmt.Printf("%s default server: %s\n", check(), cfg.Server)
		return
	}
	f := loadConfigFile()
	if len(f.Servers) == 0 {
		fmt.Println("No server yet: run `quick login --server <url>`.")
		return
	}
	names := make([]string, 0, len(f.Servers))
	for s := range f.Servers {
		names = append(names, s)
	}
	slices.Sort(names)
	for _, s := range names {
		mark, auth := "  ", "not authenticated"
		if s == f.Default {
			mark = "* "
		}
		if haveLogin(s) {
			auth = "authenticated"
		}
		fmt.Printf("%s%s — %s\n", mark, s, auth)
	}
}
