package storage

import "testing"

// The instance settings read back, start empty, and never show up as a site.
func TestSettingsStayOutOfTheSiteList(t *testing.T) {
	s3b, _ := newFakeS3(t)
	for name, b := range map[string]Backend{"local": newLocalT(t), "s3": s3b} {
		if _, ok, err := b.GetSettings(); ok || err != nil {
			t.Fatalf("%s: empty settings: ok %v err %v", name, ok, err)
		}
		if err := b.PutSettings([]byte(`{"lock_new_sites":true}`)); err != nil {
			t.Fatal(err)
		}
		if err := b.PutMeta("demo", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		got, ok, err := b.GetSettings()
		if !ok || err != nil || string(got) != `{"lock_new_sites":true}` {
			t.Errorf("%s: read back %q %v %v", name, got, ok, err)
		}
		if sites, _ := b.ListSites(); len(sites) != 1 || sites[0] != "demo" {
			t.Errorf("%s: ListSites %v, want only demo", name, sites)
		}
	}
}
