package main

import "testing"

func TestSafeRedirectRefusesOtherHosts(t *testing.T) {
	for _, rd := range []string{`/\evil.example`, `/\/evil.example`, `//evil.example`, `https://evil.example/`} {
		if safeRedirect(rd, "demo.quick.example") {
			t.Errorf("safeRedirect(%q) = true, want false", rd)
		}
	}
	for _, rd := range []string{"/", "/a/b?c=1", "https://demo.quick.example/x"} {
		if !safeRedirect(rd, "demo.quick.example") {
			t.Errorf("safeRedirect(%q) = false, want true", rd)
		}
	}
}
