package main

import (
	"bytes"
	"compress/gzip"
	"testing"
)

// Finding 4: decoding requires the whole blob to be one clean JSON document
// in an intact gzip stream, within the decompressed cap.
func TestDecodeIndexStrict(t *testing.T) {
	good, err := encodeIndex("ab", []indexFile{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeIndex(good, "ab"); err != nil {
		t.Fatalf("good index rejected: %v", err)
	}
	trailing := gzipOf([]byte(`{"format":1,"version":"ab","files":[]} {"x":1}`))
	if _, err := decodeIndex(trailing, "ab"); err == nil {
		t.Error("trailing JSON accepted")
	}
	garbage := gzipOf([]byte(`{"format":1,"version":"ab","files":[]}garbage`))
	if _, err := decodeIndex(garbage, "ab"); err == nil {
		t.Error("trailing garbage accepted")
	}
	bad := append([]byte{}, good...)
	bad[len(bad)-8] ^= 0xff // CRC-32 in the gzip trailer
	if _, err := decodeIndex(bad, "ab"); err == nil {
		t.Error("bad gzip checksum accepted")
	}
	defer func(v int64) { maxIndexSize = v }(maxIndexSize)
	maxIndexSize = 32
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(`{"format":1,"version":"ab","files":[]}` + string(bytes.Repeat([]byte(" "), 100))))
	gz.Close()
	if _, err := decodeIndex(buf.Bytes(), "ab"); err == nil {
		t.Error("index over the decompressed cap accepted")
	}
}
