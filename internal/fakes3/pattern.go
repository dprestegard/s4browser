// Package fakes3 is an in-process S3 test server. Object data is never stored:
// bytes come from a deterministic generator, and uploaded bodies are consumed,
// checksummed and recorded as metadata only. See docs/engine-design.md section 12.
package fakes3

import (
	"encoding/binary"
	"hash/crc32"
	"io"
)

// Pattern is the synthetic byte source. The 8-byte little-endian word at byte
// offset 8*i is SplitMix64(Seed + i), so any byte is computable from its offset.
type Pattern struct {
	Seed uint64
}

const splitMixGamma = 0x9E3779B97F4A7C15

// SplitMix64 is the SplitMix64 output function for state x: it adds the golden
// gamma, then applies the standard finalizer.
func SplitMix64(x uint64) uint64 {
	z := x + splitMixGamma
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Fill writes the pattern bytes for [off, off+len(p)) into p.
func (pt Pattern) Fill(p []byte, off int64) {
	if len(p) == 0 {
		return
	}
	i := uint64(off) >> 3
	skip := int(off & 7)
	if skip != 0 {
		var word [8]byte
		binary.LittleEndian.PutUint64(word[:], SplitMix64(pt.Seed+i))
		n := copy(p, word[skip:])
		p = p[n:]
		i++
	}
	for len(p) >= 8 {
		binary.LittleEndian.PutUint64(p, SplitMix64(pt.Seed+i))
		p = p[8:]
		i++
	}
	if len(p) > 0 {
		var word [8]byte
		binary.LittleEndian.PutUint64(word[:], SplitMix64(pt.Seed+i))
		copy(p, word[:])
	}
}

// Equal reports whether p holds the pattern bytes at off, and returns the
// absolute offset of the first difference otherwise. buf is scratch space.
func (pt Pattern) Equal(p []byte, off int64, scratch []byte) (firstDiff int64, ok bool) {
	for len(p) > 0 {
		n := min(len(p), len(scratch))
		pt.Fill(scratch[:n], off)
		for i := 0; i < n; i++ {
			if p[i] != scratch[i] {
				return off + int64(i), false
			}
		}
		p, off = p[n:], off+int64(n)
	}
	return 0, true
}

// CRC32 returns the CRC32 (IEEE) of the pattern bytes [off, off+length).
func (pt Pattern) CRC32(off, length int64) uint32 {
	buf := make([]byte, 256*1024)
	var sum uint32
	for length > 0 {
		n := int(min(length, int64(len(buf))))
		pt.Fill(buf[:n], off)
		sum = crc32.Update(sum, crc32.IEEETable, buf[:n])
		off += int64(n)
		length -= int64(n)
	}
	return sum
}

// NewReader returns a reader over [off, off+length) of the pattern.
func (pt Pattern) NewReader(off, length int64) *PatternReader {
	return &PatternReader{pt: pt, start: off, off: off, end: off + length}
}

// PatternReader is an io.ReadSeeker over a pattern range. It allocates nothing.
type PatternReader struct {
	pt         Pattern
	start, off int64
	end        int64
}

// Read implements io.Reader.
func (r *PatternReader) Read(p []byte) (int, error) {
	if r.off >= r.end {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.end-r.off))
	r.pt.Fill(p[:n], r.off)
	r.off += int64(n)
	return n, nil
}

// Seek implements io.Seeker relative to the start of the range.
func (r *PatternReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = r.start + offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.end + offset
	default:
		return 0, io.ErrUnexpectedEOF
	}
	if abs < r.start {
		return 0, io.ErrUnexpectedEOF
	}
	r.off = abs
	return abs - r.start, nil
}
