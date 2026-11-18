package fakes3

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"s4browser/internal/engine"
)

func TestPatternIsDeterministicAndOffsetAddressable(t *testing.T) {
	pt := Pattern{Seed: 42}
	whole := make([]byte, 1000)
	pt.Fill(whole, 0)
	// Any sub-range, aligned or not, equals the matching slice of the whole.
	for _, c := range []struct{ off, n int }{{0, 1}, {1, 7}, {3, 13}, {8, 8}, {7, 100}, {999, 1}, {500, 500}} {
		part := make([]byte, c.n)
		pt.Fill(part, int64(c.off))
		if !bytes.Equal(part, whole[c.off:c.off+c.n]) {
			t.Errorf("range %+v differs", c)
		}
	}
	// The documented definition: word i is SplitMix64(seed+i), little endian.
	want := SplitMix64(42 + 3)
	var got uint64
	for i := 7; i >= 0; i-- {
		got = got<<8 | uint64(whole[24+i])
	}
	if got != want {
		t.Errorf("word 3 = %x, want %x", got, want)
	}
	// Known SplitMix64 output for state 0 (the reference implementation's first value).
	if v := SplitMix64(0); v != 0xe220a8397b1dcdaf {
		t.Errorf("SplitMix64(0) = %x", v)
	}
	if bytes.Equal(whole[:64], func() []byte { b := make([]byte, 64); Pattern{Seed: 43}.Fill(b, 0); return b }()) {
		t.Error("different seeds give the same bytes")
	}
	if got, want := pt.CRC32(10, 500), crc32.ChecksumIEEE(whole[10:510]); got != want {
		t.Errorf("CRC32 %08x, want %08x", got, want)
	}
	if off, ok := pt.Equal(whole, 0, make([]byte, 64)); !ok || off != 0 {
		t.Errorf("Equal on matching data: %d %v", off, ok)
	}
	whole[777] ^= 1
	if off, ok := pt.Equal(whole, 0, make([]byte, 64)); ok || off != 777 {
		t.Errorf("Equal found the flip at %d (ok %v)", off, ok)
	}
}

func TestPatternReaderSeeksLikeASection(t *testing.T) {
	r := Pattern{Seed: 1}.NewReader(100, 50)
	a, _ := io.ReadAll(r)
	if len(a) != 50 {
		t.Fatalf("read %d", len(a))
	}
	if pos, err := r.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatal(pos, err)
	}
	b, _ := io.ReadAll(r)
	if !bytes.Equal(a, b) {
		t.Error("re-read after Seek differs")
	}
	exp := make([]byte, 50)
	Pattern{Seed: 1}.Fill(exp, 100)
	if !bytes.Equal(a, exp) {
		t.Error("reader differs from Fill")
	}
}

func TestChunkDecoderHandlesSignaturesAndTrailers(t *testing.T) {
	body := "5;chunk-signature=abc\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\nx-amz-trailer-signature:sig\r\n\r\n"
	dec := &chunkDecoder{br: bufioReader(body)}
	got, err := io.ReadAll(dec)
	if err != nil || string(got) != "hello world" {
		t.Fatalf("%q %v", got, err)
	}
	if dec.trailers["x-amz-checksum-crc32"] != "AAAAAA==" {
		t.Errorf("trailers %v", dec.trailers)
	}
	for name, bad := range map[string]string{
		"bad size":      "zz\r\nhello\r\n",
		"short":         "5\r\nhel",
		"no terminator": "5\r\nhelloXX0\r\n\r\n",
	} {
		if _, err := io.ReadAll(&chunkDecoder{br: bufioReader(bad)}); err == nil {
			t.Errorf("%s decoded without error", name)
		}
	}
}

func TestFakeClockFiresInOrder(t *testing.T) {
	c := NewFakeClock(time.Time{})
	start := c.Now()
	t1 := c.NewTimer(10 * time.Second)
	t2 := c.NewTimer(5 * time.Second)
	tick := c.NewTicker(3 * time.Second)
	if c.Pending() != 3 {
		t.Fatalf("pending %d", c.Pending())
	}
	c.Advance(4 * time.Second)
	select {
	case at := <-tick.C():
		if at.Sub(start) != 3*time.Second {
			t.Errorf("tick at %v", at.Sub(start))
		}
	default:
		t.Fatal("ticker did not fire")
	}
	select {
	case <-t2.C():
		t.Fatal("5 s timer fired at 4 s")
	default:
	}
	c.Advance(2 * time.Second)
	<-t2.C()
	if !t1.Stop() {
		t.Error("Stop on an active timer returned false")
	}
	c.Advance(time.Hour)
	select {
	case <-t1.C():
		t.Error("stopped timer fired")
	default:
	}
	if t1.Reset(time.Second) {
		t.Error("Reset of a stopped timer returned true")
	}
	c.Advance(time.Second)
	<-t1.C()
	tick.Stop()
	if c.Pending() != 0 {
		t.Errorf("pending %d", c.Pending())
	}
	if got := c.Now().Sub(start); got != time.Hour+7*time.Second {
		t.Errorf("elapsed %v", got)
	}
}

func TestSyntheticSourceAndHandles(t *testing.T) {
	src := NewSource()
	ctx := context.Background()
	if _, err := src.Open(ctx, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open missing: %v", err)
	}
	src.Set("f", SyntheticFile{Size: 100, Seed: 3})
	h, err := src.Open(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 30)
	if n, err := h.ReadAt(buf, 90); n != 10 || err != io.EOF {
		t.Fatalf("tail read: %d %v", n, err)
	}
	if n, err := h.ReadAt(buf, 100); n != 0 || err != io.EOF {
		t.Fatalf("read at EOF: %d %v", n, err)
	}
	st, _ := h.Stat()
	if st.Size != 100 {
		t.Fatal(st)
	}
	// An edit during upload is visible through the open handle's Stat.
	src.Set("f", SyntheticFile{Size: 100, Seed: 3, ModTime: st.ModTime.Add(time.Second)})
	if st2, _ := h.Stat(); st2.ModTime.Equal(st.ModTime) {
		t.Error("Stat did not see the change")
	}
	boom := errors.New("disk failure")
	src.Set("bad", SyntheticFile{Size: 1000, Seed: 1, FailAt: 500, FailErr: boom})
	bh, _ := src.Open(ctx, "bad")
	if n, err := bh.ReadAt(make([]byte, 100), 450); n != 50 || !errors.Is(err, boom) {
		t.Fatalf("read across the fault: %d %v", n, err)
	}
	if _, err := bh.ReadAt(make([]byte, 10), 600); !errors.Is(err, boom) {
		t.Fatalf("read past the fault: %v", err)
	}
	if err := bh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSyntheticSinkVerifiesAndTracksCoverage(t *testing.T) {
	ctx := context.Background()
	sink := NewSink(func(string) uint64 { return 11 })
	spec := engine.SinkSpec{JobID: "job", DestPath: "/dest/file", Size: 100}
	w, err := sink.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	chunk := func(off, n int64) []byte { b := make([]byte, n); Pattern{Seed: 11}.Fill(b, off); return b }
	// Out-of-order writes with an overlap, which a mid-part retry could cause.
	for _, c := range [][2]int64{{50, 30}, {0, 20}, {75, 25}, {20, 30}} {
		if _, err := w.WriteAt(chunk(c[0], c[1]), c[0]); err != nil {
			t.Fatal(err)
		}
	}
	total, distinct := sink.BytesWritten("/dest/file")
	if total != 105 || distinct != 100 {
		t.Fatalf("total %d distinct %d", total, distinct)
	}
	if _, err := w.WriteAt([]byte("wrong"), 0); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("wrong bytes accepted: %v", err)
	}
	if _, err := w.WriteAt(chunk(90, 20), 90); err == nil {
		t.Fatal("write past the end accepted")
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !sink.Committed("/dest/file") {
		t.Fatal("not committed")
	}
	if _, err := sink.Prepare(ctx, spec); !errors.Is(err, engine.ErrDestinationExists) {
		t.Fatalf("collision: %v", err)
	}

	// Commit with a gap fails, Abort is recorded.
	spec2 := engine.SinkSpec{JobID: "j2", DestPath: "/dest/gap", Size: 100}
	w2, _ := sink.Prepare(ctx, spec2)
	w2.WriteAt(chunk(0, 40), 0)
	if err := w2.Commit(ctx); err == nil {
		t.Fatal("commit with a gap succeeded")
	}
	if err := w2.Abort(); err != nil || !sink.Aborted("/dest/gap") {
		t.Fatal("abort not recorded")
	}
	sink.FreeSpace = 10
	if _, err := sink.Prepare(ctx, engine.SinkSpec{DestPath: "/dest/big", Size: 11}); !errors.Is(err, engine.ErrInsufficientSpace) {
		t.Fatalf("space: %v", err)
	}
	sink.MarkExisting("/dest/there")
	if _, err := sink.Prepare(ctx, engine.SinkSpec{DestPath: "/dest/there", Size: 1}); !errors.Is(err, engine.ErrDestinationExists) {
		t.Fatalf("existing: %v", err)
	}
	// A zero-byte object commits with no writes.
	w0, err := sink.Prepare(ctx, engine.SinkSpec{DestPath: "/dest/empty", Size: 0})
	if err != nil || w0.Commit(ctx) != nil {
		t.Fatalf("empty: %v", err)
	}
	if w.MaxLead() != 0 {
		t.Error("MaxLead")
	}
}

func bufioReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }
