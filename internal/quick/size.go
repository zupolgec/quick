package quick

import (
	"errors"
	"strconv"
	"strings"
)

// LimitRequest is the body of POST /api/site/<name>/limit (administrators
// only): SizeLimit in bytes, 0 = back to the server default.
type LimitRequest struct {
	SizeLimit int64 `json:"size_limit"`
}

// LimitResponse is the size limit of a site (GET and POST .../limit).
type LimitResponse struct {
	Site      string   `json:"site"`
	SizeLimit int64    `json:"size_limit"`       // bytes in effect
	Custom    bool     `json:"custom"`           // set by an administrator (else the default)
	Default   int64    `json:"default"`          // server default
	Max       int64    `json:"max"`              // highest limit an administrator can set
	Admin     bool     `json:"admin"`            // the caller can change it
	Admins    []string `json:"admins,omitempty"` // who can (none configured: the system administrator)
}

var sizeUnits = map[string]int64{"": 1, "b": 1, "k": 1 << 10, "kb": 1 << 10, "m": 1 << 20, "mb": 1 << 20, "g": 1 << 30, "gb": 1 << 30}

// ParseSize reads a positive size like "100MB", "1.5GB", "800M" or a plain
// number of bytes. Units are binary (1 MB = 1024 KB), as FormatSize prints.
func ParseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	num, unit := s, ""
	if i >= 0 {
		num, unit = s[:i], strings.TrimSpace(s[i:])
	}
	mult, ok := sizeUnits[unit]
	if !ok || num == "" {
		return 0, errors.New("invalid size " + strconv.Quote(s) + " (e.g. 500MB, 1.5GB)")
	}
	f, err := strconv.ParseFloat(num, 64)
	n := int64(f * float64(mult))
	if err != nil || n <= 0 || float64(n) != f*float64(mult) {
		return 0, errors.New("invalid size " + strconv.Quote(s) + " (e.g. 500MB, 1.5GB)")
	}
	return n, nil
}

// FormatSize prints a size in binary units, at most one decimal: "100 MB",
// "1.5 GB".
func FormatSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(1024), 0
	for x := n / 1024; x >= 1024 && exp < 3; x /= 1024 {
		div *= 1024
		exp++
	}
	v := strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64)
	return strings.TrimSuffix(v, ".0") + " " + string("KMGT"[exp]) + "B"
}
