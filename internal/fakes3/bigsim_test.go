//go:build !race

// Memory tests are excluded under the race detector, which multiplies heap use
// (docs/engine-design.md section 4).

package fakes3

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"s4browser/internal/engine"
	"s4browser/internal/s3x"
)

func heapObjects() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// TestSimulated2TiBUploadUsesBoundedMemory uploads a 2 TiB synthetic object
// through the in-process store with 32 concurrent parts, then reads it back over
// HTTPS with the real SDK. Neither data nor part bodies are ever held: the store
// keeps 9,987 (length, CRC32, ETag) records, so heap use is independent of the
// object size.
func TestSimulated2TiBUploadUsesBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 2 TiB through the CRC loop; takes tens of seconds")
	}
	const (
		size     = int64(2) << 40
		seed     = uint64(0x2754)
		workers  = 32
		maxBytes = 48 << 20 // allowed heap growth; the design bound is about 67 MiB
	)
	s := newTestServer(t, Options{})
	store := s.Store()
	ctx := context.Background()
	ref := engine.ObjectRef{Region: Region, Bucket: DefaultBucket, Key: "s4browser-test/run1/two-tib.bin"}
	src := NewSource()
	src.Set("big", SyntheticFile{Size: size, Seed: seed})
	h, err := src.Open(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.PlanParts(size, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %d parts of %d MiB", plan.PartCount, plan.PartSize/mib)

	runtime.GC()
	baseline := heapObjects()
	var peak atomic.Uint64
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			if v := heapObjects(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()

	id, err := store.CreateMultipartUpload(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	completed := make([]engine.CompletedPart, plan.PartCount)
	jobs := make(chan int)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				pr := plan.Part(i)
				res, err := store.UploadPart(ctx, engine.UploadPartRequest{
					Ref: ref, UploadID: id, PartNumber: pr.Number,
					Body: io.NewSectionReader(h, pr.Offset, pr.Length), ContentLength: pr.Length,
				})
				if err != nil {
					errs <- err
					return
				}
				completed[i] = engine.CompletedPart{Number: pr.Number, ETag: res.ETag, ChecksumCRC32: res.ChecksumCRC32}
			}
		}()
	}
feed:
	for i := 0; i < plan.PartCount; i++ {
		select {
		case jobs <- i:
		case err := <-errs:
			t.Error(err)
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}

	done, err := store.CompleteMultipartUpload(ctx, engine.CompleteUploadRequest{Ref: ref, UploadID: id, Parts: completed, ObjectSize: size})
	if err != nil {
		t.Fatal(err)
	}
	crcs := make([]string, len(completed))
	for i, p := range completed {
		crcs[i] = p.ChecksumCRC32
	}
	if want, _ := engine.CompositeCRC32(crcs); done.ChecksumCRC32 != want {
		t.Fatalf("composite %q, want %q", done.ChecksumCRC32, want)
	}
	if !strings.HasSuffix(done.ChecksumCRC32, "-9987") {
		t.Errorf("composite checksum %q should end in the part count", done.ChecksumCRC32)
	}
	for i := 1; i <= plan.PartCount; i++ {
		if n := s.Attempts(OpUploadPart, DefaultBucket, ref.Key, int32(i)); n != 1 {
			t.Fatalf("part %d was sent %d times", i, n)
		}
	}
	// Check the uploaded content against the synthetic source: every 64th part
	// in full, plus the last part. (Every part's CRC was already checked by the
	// server against the bytes it received.)
	if err := s.VerifyObject(DefaultBucket, ref.Key, seed, 64); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyObject(DefaultBucket, ref.Key, seed+1, 1000); err == nil {
		t.Fatal("a wrong seed verified")
	}

	// Read the far end of the object back over HTTPS with the real SDK.
	c := s.S3Client()
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String(ref.Key), ChecksumMode: types.ChecksumModeEnabled}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt64(head.ContentLength) != size || aws.ToString(head.ChecksumCRC32) != done.ChecksumCRC32 {
		t.Fatalf("head: %d bytes, checksum %q", aws.ToInt64(head.ContentLength), aws.ToString(head.ChecksumCRC32))
	}
	for _, off := range []int64{0, plan.PartSize - 8, size - 3*mib, size - 1} {
		n := min(3*mib, size-off)
		out, err := getRange(ctx, c, ref.Key, off, n, aws.ToString(head.ETag))
		if err != nil {
			t.Fatal(err)
		}
		got := readAll(t, out)
		exp := make([]byte, n)
		Pattern{Seed: seed}.Fill(exp, off)
		if !bytes.Equal(got, exp) {
			t.Fatalf("ranged GET at %d differs from the pattern", off)
		}
	}

	close(stop)
	sampler.Wait()
	growth := int64(peak.Load()) - int64(baseline)
	t.Logf("heap growth: %.1f MiB for a %d TiB object, %d requests", float64(growth)/mib, size>>40, len(s.Requests()))
	if growth > maxBytes {
		t.Fatalf("heap grew by %d MiB, limit %d MiB", growth/mib, int64(maxBytes)/mib)
	}
}

// TestSyntheticTerabyteObjectsListAndReadOverHTTPS shows that object size costs
// nothing: a 40 TiB synthetic object is listed, headed and range-read by the SDK.
func TestSyntheticTerabyteObjectsListAndReadOverHTTPS(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()
	const size = int64(40) << 40
	if err := s.PutSynthetic(DefaultBucket, "huge", 77, size); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	before := heapObjects()
	out, err := getRange(ctx, c, "huge", size-1024, 1024, "")
	if err != nil {
		t.Fatal(err)
	}
	got := readAll(t, out)
	exp := make([]byte, 1024)
	Pattern{Seed: 77}.Fill(exp, size-1024)
	if !bytes.Equal(got, exp) || !strings.HasPrefix(aws.ToString(out.ContentRange), "bytes 4398046") {
		t.Fatalf("content range %q", aws.ToString(out.ContentRange))
	}
	list, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(DefaultBucket)}, s3x.SingleAttemptOptions)
	if err != nil || aws.ToInt64(list.Contents[0].Size) != size {
		t.Fatalf("list: %v", err)
	}
	runtime.GC()
	if after := heapObjects(); int64(after)-int64(before) > 8<<20 {
		t.Errorf("heap grew by %d bytes", int64(after)-int64(before))
	}
}
