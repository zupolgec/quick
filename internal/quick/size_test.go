package quick

import "testing"

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"100MB":   100 << 20,
		"100 mb":  100 << 20,
		"1.5GB":   3 << 29,
		"2G":      2 << 30,
		"512K":    512 << 10,
		"800M":    800 << 20,
		"1048576": 1 << 20,
	}
	for in, want := range ok {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "-1MB", "1.5", "abc", "10XB", "0MB"} {
		if got, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", in, got)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int64]string{
		100 << 20: "100 MB",
		3 << 29:   "1.5 GB",
		2 << 30:   "2 GB",
		500:       "500 B",
		1536:      "1.5 KB",
	} {
		if got := FormatSize(n); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", n, got, want)
		}
	}
}
