package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// /__quick/ belongs to quick (code page, quick.js, API): a deploy carrying
// files there is refused, whatever client sends it, and nothing is published.
func TestDeployRefusesReservedQuickFolder(t *testing.T) {
	for _, tarUpload := range []bool{false, true} {
		s := newDeltaTestServer(t)
		c := &deltaClient{t: t, s: s, site: "demo"}
		files := map[string][]byte{"index.html": []byte("x"), "__quick/quick.js": []byte("evil")}
		var code int
		var body string
		if tarUpload {
			code, body = tarDeployCode(c, files)
		} else {
			m, _ := manifestOf(t, files)
			b, _ := json.Marshal(m)
			w := c.do(http.MethodPost, "/api/deploy/plan", b, nil)
			code, body = w.Code, w.Body.String()
		}
		if code != http.StatusBadRequest || !strings.Contains(body, "__quick/") {
			t.Fatalf("tar %v: %d %q, want 400 naming __quick/", tarUpload, code, body)
		}
		if exists, _ := s.store.SiteExists("demo"); exists {
			t.Fatalf("tar %v: refused deploy published something", tarUpload)
		}
	}
}
