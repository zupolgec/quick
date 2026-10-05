package main

import "os"

var useColor = colorEnabled()

func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func cGreen(s string) string  { return paint("32", s) }
func cCyan(s string) string   { return paint("36", s) }
func cYellow(s string) string { return paint("33", s) }
func cBold(s string) string   { return paint("1", s) }

func check() string { return cGreen("✓") }
