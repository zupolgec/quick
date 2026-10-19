// Package chunk splits files into content-defined chunks (FastCDC, gear
// rolling hash with normalized chunking) for the delta deploy. CLI and server
// must cut identically, so everything here is part of the wire contract:
// changing the gear table, the masks or the size bounds doesn't break
// correctness (chunks are addressed by SHA-256) but makes every chunk "new"
// once, until both sides agree again.
//
// Content-defined boundaries depend only on the bytes around them, so an
// insertion or deletion in the middle of a file shifts at most a couple of
// chunks; everything before and after keeps the same hashes.
package chunk

import (
	"crypto/sha256"
	"errors"
	"io"
)

const (
	MinSize = 16 << 10  // no cut before this many bytes (except the file's last chunk)
	AvgSize = 64 << 10  // target average
	MaxSize = 256 << 10 // forced cut
)

// Normalized chunking: before AvgSize a stricter mask (2 more bits) makes cuts
// rarer, after it a looser one (2 fewer bits) makes them likelier, pulling the
// distribution toward AvgSize. Masks use the high bits: with the gear update
// (fp<<1 + g) the high bits depend on the last 64 bytes, the low ones only on
// the last few.
const (
	maskS = uint64(1<<18-1) << (64 - 18)
	maskL = uint64(1<<14-1) << (64 - 14)
)

// gear is a fixed pseudo-random table (splitmix64, fixed seed): deterministic
// across builds and platforms.
var gear = func() (t [256]uint64) {
	x := uint64(0x5155494b43444331) // "QUICKCDC1"-ish seed; never change
	for i := range t {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		t[i] = z ^ (z >> 31)
	}
	return
}()

// cut returns the length of the next chunk at the start of data. data holds at
// least MaxSize bytes unless it is the tail of the stream.
func cut(data []byte) int {
	n := len(data)
	if n <= MinSize {
		return n
	}
	if n > MaxSize {
		n = MaxSize
	}
	normal := min(AvgSize, n)
	var fp uint64
	i := MinSize
	for ; i < normal; i++ {
		fp = fp<<1 + gear[data[i]]
		if fp&maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = fp<<1 + gear[data[i]]
		if fp&maskL == 0 {
			return i + 1
		}
	}
	return n
}

// Split reads r to the end and calls fn with each chunk in order. The slice
// is only valid during the call.
func Split(r io.Reader, fn func([]byte) error) error {
	buf := make([]byte, 2*MaxSize)
	start, end := 0, 0
	eof := false
	for {
		if !eof && end-start < MaxSize {
			copy(buf, buf[start:end])
			end -= start
			start = 0
			for end < len(buf) {
				n, err := r.Read(buf[end:])
				end += n
				if errors.Is(err, io.EOF) {
					eof = true
					break
				}
				if err != nil {
					return err
				}
			}
		}
		if start == end {
			return nil
		}
		n := cut(buf[start:end])
		if err := fn(buf[start : start+n]); err != nil {
			return err
		}
		start += n
	}
}

// Chunk is one piece of a file: SHA-256 of its bytes and length.
type Chunk struct {
	Hash [32]byte
	Size int
}

// File is the chunk index of one file plus its whole-content SHA-256.
type File struct {
	Size   int64
	Hash   [32]byte
	Chunks []Chunk
}

// Index chunks r and hashes each chunk and the whole content.
func Index(r io.Reader) (File, error) {
	var f File
	whole := sha256.New()
	err := Split(r, func(b []byte) error {
		whole.Write(b)
		f.Chunks = append(f.Chunks, Chunk{Hash: sha256.Sum256(b), Size: len(b)})
		f.Size += int64(len(b))
		return nil
	})
	if err != nil {
		return File{}, err
	}
	copy(f.Hash[:], whole.Sum(nil))
	return f, nil
}
