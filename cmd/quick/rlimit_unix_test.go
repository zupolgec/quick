//go:build unix

package main

import (
	"syscall"
	"testing"
)

// withFDLimit runs fn with the soft open-file limit lowered to n, so leaking
// one descriptor per file shows up with a few thousand files.
func withFDLimit(t *testing.T, n uint64, fn func()) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Skip("getrlimit:", err)
	}
	lim := old
	lim.Cur = n
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Skip("setrlimit:", err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old)
	fn()
}
