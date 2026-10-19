package storage

import (
	"errors"
	"net/http"
	"testing"

	"github.com/minio/minio-go/v7"
)

// Only genuine absence maps to ErrNotFound: timeouts, access denied and the
// like are operational errors, never "the file isn't there".
func TestS3OpenErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		notFound bool
	}{
		{"no such key", minio.ErrorResponse{Code: "NoSuchKey", StatusCode: http.StatusNotFound}, true},
		{"bare 404", minio.ErrorResponse{StatusCode: http.StatusNotFound}, true},
		{"access denied", minio.ErrorResponse{Code: "AccessDenied", StatusCode: http.StatusForbidden}, false},
		{"server error", minio.ErrorResponse{Code: "InternalError", StatusCode: http.StatusInternalServerError}, false},
		{"network", errors.New("dial tcp: i/o timeout"), false},
	}
	for _, c := range cases {
		got := s3OpenErr(c.err)
		if errors.Is(got, ErrNotFound) != c.notFound {
			t.Errorf("%s: mapped to %v, want notFound=%v", c.name, got, c.notFound)
		}
		if !c.notFound && got != c.err {
			t.Errorf("%s: original error not propagated: %v", c.name, got)
		}
	}
}
