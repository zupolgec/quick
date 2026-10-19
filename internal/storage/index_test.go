package storage

import (
	"bytes"
	"testing"
)

// Index blobs are kept per site and version next to the metadata; pruning
// keeps only the named versions; deleting the site removes them all, and they
// never show up as sites.
func TestLocalIndexBlobs(t *testing.T) {
	l := newLocalT(t)
	if _, ok, err := l.GetIndex("demo", "aa"); ok || err != nil {
		t.Fatalf("missing index: ok=%v err=%v", ok, err)
	}
	for _, v := range []string{"aa", "bb", "cc"} {
		if err := l.PutIndex("demo", v, []byte("idx-"+v)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.PutIndex("demo-two", "aa", []byte("other")); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := l.GetIndex("demo", "bb"); !ok || err != nil || !bytes.Equal(b, []byte("idx-bb")) {
		t.Fatalf("GetIndex: %q %v %v", b, ok, err)
	}
	if sites, _ := l.ListSites(); len(sites) != 0 {
		t.Fatalf("index blobs listed as sites: %v", sites)
	}
	if err := l.PruneIndexes("demo", "cc", "aa"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l.GetIndex("demo", "bb"); ok {
		t.Fatal("pruned index still there")
	}
	if _, ok, _ := l.GetIndex("demo", "aa"); !ok {
		t.Fatal("kept index removed")
	}
	if _, ok, _ := l.GetIndex("demo-two", "aa"); !ok {
		t.Fatal("pruning touched another site")
	}
	if err := l.PutSite("demo", tarOf(map[string]string{"index.html": "x"})); err != nil {
		t.Fatal(err)
	}
	if _, err := l.DeleteSite("demo"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"aa", "cc"} {
		if _, ok, _ := l.GetIndex("demo", v); ok {
			t.Fatalf("index %s survived DeleteSite", v)
		}
	}
	if _, ok, _ := l.GetIndex("demo-two", "aa"); !ok {
		t.Fatal("DeleteSite removed another site's index")
	}
}

// Versions end up in paths and keys: only lowercase hex is accepted.
func TestIndexVersionValidated(t *testing.T) {
	l := newLocalT(t)
	for _, v := range []string{"", "../x", "AB", "a/b", "a.b", "*"} {
		if err := l.PutIndex("demo", v, []byte("x")); err == nil {
			t.Errorf("PutIndex accepted version %q", v)
		}
		if _, _, err := l.GetIndex("demo", v); err == nil {
			t.Errorf("GetIndex accepted version %q", v)
		}
	}
}
