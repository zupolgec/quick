package chunk

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"testing"
	"testing/iotest"
)

func randBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func split(t *testing.T, data []byte) []Chunk {
	t.Helper()
	f, err := Index(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return f.Chunks
}

func hashSet(cs []Chunk) map[[32]byte]bool {
	m := map[[32]byte]bool{}
	for _, c := range cs {
		m[c.Hash] = true
	}
	return m
}

func TestDeterministicAndReassembles(t *testing.T) {
	data := randBytes(1, 3<<20)
	a := split(t, data)
	b := split(t, data)
	if len(a) != len(b) {
		t.Fatalf("chunk counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("chunk %d differs", i)
		}
	}
	// Chunk sizes add up and hashes match the data at each offset.
	off := 0
	for _, c := range a {
		if sha256.Sum256(data[off:off+c.Size]) != c.Hash {
			t.Fatalf("chunk at %d: hash mismatch", off)
		}
		off += c.Size
	}
	if off != len(data) {
		t.Fatalf("chunks cover %d bytes, want %d", off, len(data))
	}
}

// Short reads from the source must not change the boundaries.
func TestBoundariesIndependentOfReadSize(t *testing.T) {
	data := randBytes(2, 1<<20)
	a := split(t, data)
	f, err := Index(iotest.OneByteReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Chunks) != len(a) {
		t.Fatalf("chunk counts differ: %d vs %d", len(f.Chunks), len(a))
	}
	for i := range a {
		if a[i] != f.Chunks[i] {
			t.Fatalf("chunk %d differs with one-byte reads", i)
		}
	}
}

func TestBoundsAndAverage(t *testing.T) {
	data := randBytes(3, 8<<20)
	cs := split(t, data)
	for i, c := range cs {
		if c.Size > MaxSize {
			t.Fatalf("chunk %d: %d bytes > max %d", i, c.Size, MaxSize)
		}
		if i < len(cs)-1 && c.Size < MinSize {
			t.Fatalf("chunk %d: %d bytes < min %d", i, c.Size, MinSize)
		}
	}
	avg := len(data) / len(cs)
	if avg < AvgSize/2 || avg > AvgSize*2 {
		t.Fatalf("average chunk %d bytes, want about %d", avg, AvgSize)
	}
}

// Low-entropy input (no natural cut points) is cut at MaxSize.
func TestZerosCutAtMax(t *testing.T) {
	cs := split(t, make([]byte, 3*MaxSize+10))
	for i, c := range cs[:len(cs)-1] {
		if c.Size != MaxSize {
			t.Fatalf("chunk %d: %d bytes, want %d", i, c.Size, MaxSize)
		}
	}
	if last := cs[len(cs)-1].Size; last != 10 {
		t.Fatalf("last chunk %d bytes, want 10", last)
	}
}

func TestEmptyAndTiny(t *testing.T) {
	f, err := Index(bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Chunks) != 0 || f.Size != 0 || f.Hash != sha256.Sum256(nil) {
		t.Fatalf("empty file: %+v", f)
	}
	tiny := []byte("hello")
	f, err = Index(bytes.NewReader(tiny))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Chunks) != 1 || f.Chunks[0].Size != 5 || f.Hash != sha256.Sum256(tiny) || f.Size != 5 {
		t.Fatalf("tiny file: %+v", f)
	}
}

// An insertion or deletion in the middle only changes the chunks around the
// edit: everything before and after is reused.
func TestEditInMiddleReusesMostChunks(t *testing.T) {
	data := randBytes(4, 16<<20)
	orig := split(t, data)
	have := hashSet(orig)

	mid := len(data) / 2
	edits := map[string][]byte{
		"insert":  append(append(append([]byte{}, data[:mid]...), []byte("some inserted bytes")...), data[mid:]...),
		"delete":  append(append([]byte{}, data[:mid]...), data[mid+1000:]...),
		"replace": append(append(append([]byte{}, data[:mid]...), randBytes(5, 4096)...), data[mid+4096:]...),
	}
	for name, edited := range edits {
		cs := split(t, edited)
		missing := 0
		for _, c := range cs {
			if !have[c.Hash] {
				missing++
			}
		}
		if missing > 4 {
			t.Errorf("%s: %d of %d chunks new, want at most 4", name, missing, len(cs))
		}
	}
}

func TestReaderErrorPropagates(t *testing.T) {
	r := io.MultiReader(bytes.NewReader(randBytes(6, 100)), iotest.ErrReader(io.ErrUnexpectedEOF))
	if _, err := Index(r); err == nil {
		t.Fatal("expected error from reader")
	}
}
