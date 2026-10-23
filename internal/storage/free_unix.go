//go:build unix

package storage

import "syscall"

// FreeBytes is the space available to the server on the sites' filesystem.
func (l *local) FreeBytes() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(l.sitesDir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
