package fakes3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"s4browser/internal/engine"
	"s4browser/internal/s3x"
)

func sdkPart(ctx context.Context, c *s3.Client, key, uploadID string, n int32, size int64) (*s3.UploadPartOutput, error) {
	return c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(key), UploadId: aws.String(uploadID), PartNumber: aws.Int32(n),
		Body: io.NewSectionReader(zeroAt{}, 0, size), ContentLength: aws.Int64(size), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	}, s3x.SingleAttemptOptions)
}

func sdkCreate(t *testing.T, c *s3.Client, key string) string {
	t.Helper()
	cr, err := c.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(key),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32, ChecksumType: types.ChecksumTypeComposite,
	}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	return aws.ToString(cr.UploadId)
}

// TestStatusFaultsAreClassifiedByTheEngine runs each error fault through the
// real SDK and checks the engine's own classification of what the client sees.
func TestStatusFaultsAreClassifiedByTheEngine(t *testing.T) {
	cases := []struct {
		name      string
		fault     Fault
		code      string
		class     engine.Class
		throttled bool
	}{
		{"500", InternalError(), "InternalError", engine.ClassRetryable, false},
		{"503", ServiceUnavailable(), "ServiceUnavailable", engine.ClassRetryable, false},
		{"SlowDown", SlowDown(), "SlowDown", engine.ClassRetryable, true},
		{"expired token", ExpiredToken(), "ExpiredToken", engine.ClassCredential, false},
		{"invalid key", InvalidAccessKeyID(), "InvalidAccessKeyId", engine.ClassCredential, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, Options{})
			c := s.S3Client()
			id := sdkCreate(t, c, testKey)
			s.AddRule(Rule{Op: OpUploadPart, Part: 1, Attempt: 1, Fault: tc.fault})

			_, err := sdkPart(context.Background(), c, testKey, id, 1, 1000)
			if apiCode(err) != tc.code {
				t.Fatalf("first attempt: %v", err)
			}
			class, throttled := engine.ClassifyResponse(tc.fault.Status, apiCode(err))
			if class != tc.class || throttled != tc.throttled {
				t.Errorf("class %v throttled %v", class, throttled)
			}
			// The rule was for attempt 1 only, so the retry succeeds.
			if _, err := sdkPart(context.Background(), c, testKey, id, 1, 1000); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got := s.Attempts(OpUploadPart, DefaultBucket, testKey, 1); got != 2 {
				t.Errorf("attempts = %d", got)
			}
			// The refused attempt stored nothing, so the part was stored once.
			if len(s.ActiveUploads(DefaultBucket)[0].Parts) != 1 {
				t.Errorf("part not stored by the retry")
			}
		})
	}
}

func TestRetryAfterHeaderAndRuleTimes(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	id := sdkCreate(t, c, testKey)
	rule := s.AddRule(Rule{Op: OpUploadPart, Times: 2, Fault: SlowDown().WithRetryAfter(3 * time.Second)})
	for i := 0; i < 2; i++ {
		if _, err := sdkPart(context.Background(), c, testKey, id, 1, 10); apiCode(err) != "SlowDown" {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := sdkPart(context.Background(), c, testKey, id, 1, 10); err != nil {
		t.Fatal(err)
	}
	if s.RuleFired(rule) != 2 {
		t.Errorf("fired %d", s.RuleFired(rule))
	}
	// The in-process store reports the hint to the engine.
	s.ClearRules()
	s.AddRule(Rule{Op: OpHead, Fault: SlowDown().WithRetryAfter(3 * time.Second)})
	_, err := s.Store().HeadObject(context.Background(), engine.ObjectRef{Bucket: DefaultBucket, Key: "x"})
	var oe *engine.OpError
	if !errors.As(err, &oe) || oe.RetryAfter != 3*time.Second || !oe.Throttled || oe.Class != engine.ClassRetryable {
		t.Fatalf("op error %+v", oe)
	}
}

func TestResetMidBodyThenRetry(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	id := sdkCreate(t, c, testKey)
	s.AddRule(Rule{Op: OpUploadPart, Part: 2, Attempt: 1, Fault: ResetAt(1 * mib)})

	const size = 3 * mib
	if _, err := sdkPart(context.Background(), c, testKey, id, 1, size); err != nil {
		t.Fatal(err)
	}
	_, err := sdkPart(context.Background(), c, testKey, id, 2, size)
	if err == nil || apiCode(err) != "" {
		t.Fatalf("expected a transport error, got %v", err)
	}
	if got := engine.Classify(err); got != engine.ClassRetryable {
		t.Errorf("transport failure classified as %v", got)
	}
	if _, err := sdkPart(context.Background(), c, testKey, id, 2, size); err != nil {
		t.Fatal(err)
	}
	// Part 1 completed once and was never sent again; part 2 took two attempts.
	if a, b := s.Attempts(OpUploadPart, DefaultBucket, testKey, 1), s.Attempts(OpUploadPart, DefaultBucket, testKey, 2); a != 1 || b != 2 {
		t.Fatalf("attempts: part 1 = %d, part 2 = %d", a, b)
	}
	for _, r := range s.Requests() {
		if r.Op == OpUploadPart && r.Part == 2 && r.Attempt == 1 {
			if r.Fault != FaultReset || r.BodyBytes < 1*mib || r.BodyBytes >= size {
				t.Errorf("reset attempt record %+v", r)
			}
		}
	}
}

func TestLostCompleteResponseLeavesObjectDone(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	up, _ := sdkMultipartParts(t, s, testKey, 6*mib, 5*mib)
	s.AddRule(Rule{Op: OpComplete, Attempt: 1, Fault: LostResponse()})

	_, err := c.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(testKey), UploadId: aws.String(up.id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: up.parts},
	}, s3x.SingleAttemptOptions)
	if err == nil || apiCode(err) != "" {
		t.Fatalf("expected a lost response, got %v", err)
	}
	if _, ok := s.Object(DefaultBucket, testKey); !ok {
		t.Fatal("the complete did not take effect")
	}
	// The retry finds NoSuchUpload, and HEAD with checksums confirms success.
	_, err = c.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(testKey), UploadId: aws.String(up.id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: up.parts},
	}, s3x.SingleAttemptOptions)
	if apiCode(err) != "NoSuchUpload" {
		t.Fatalf("retry: %v", err)
	}
	head, err := s.Store().HeadObject(context.Background(), engine.ObjectRef{Bucket: DefaultBucket, Key: testKey})
	if err != nil || head.ChecksumCRC32 == "" {
		t.Fatalf("head %+v %v", head, err)
	}
}

// sdkMultipartParts uploads real parts but does not complete.
func sdkMultipartParts(t *testing.T, s *Server, key string, size, partSize int64) (sdkUpload, int) {
	t.Helper()
	c := s.S3Client()
	up := sdkUpload{id: sdkCreate(t, c, key)}
	plan, _ := engine.PlanParts(size, partSize)
	for i := 0; i < plan.PartCount; i++ {
		pr := plan.Part(i)
		out, err := sdkPart(context.Background(), c, key, up.id, pr.Number, pr.Length)
		if err != nil {
			t.Fatal(err)
		}
		up.parts = append(up.parts, types.CompletedPart{PartNumber: aws.Int32(pr.Number), ETag: out.ETag, ChecksumCRC32: out.ChecksumCRC32})
	}
	return up, plan.PartCount
}

func TestErrorInsideA200ForComplete(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	up, _ := sdkMultipartParts(t, s, testKey, 6*mib, 5*mib)
	s.AddRule(Rule{Op: OpComplete, Attempt: 1, Fault: InternalError().ErrorIn200()})
	in := &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(testKey), UploadId: aws.String(up.id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: up.parts},
	}
	if _, err := c.CompleteMultipartUpload(context.Background(), in, s3x.SingleAttemptOptions); apiCode(err) != "InternalError" {
		t.Fatalf("first: %v", err)
	}
	if _, ok := s.Object(DefaultBucket, testKey); ok {
		t.Fatal("a failed complete created the object")
	}
	if _, err := c.CompleteMultipartUpload(context.Background(), in, s3x.SingleAttemptOptions); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestStallWithNoBytesSentEndsWithTheClientContext(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	if err := s.PutSynthetic(DefaultBucket, "obj", 1, 1000); err != nil {
		t.Fatal(err)
	}
	s.AddRule(Rule{Op: OpGet, Attempt: 1, Fault: StallAt(0, 0)})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := getRange(ctx, c, "obj", 0, 10, "")
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("stalled GET returned %v after %v", err, time.Since(start))
	}
	if got := engine.Classify(err); got != engine.ClassRetryable {
		t.Errorf("deadline classified as %v", got)
	}
	// Attempt 2 is not stalled.
	out, err := getRange(context.Background(), c, "obj", 0, 10, "")
	if err != nil || len(readAll(t, out)) != 10 {
		t.Fatalf("second attempt: %v", err)
	}
}

func TestStallMidUploadBody(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	id := sdkCreate(t, c, testKey)
	s.AddRule(Rule{Op: OpUploadPart, Attempt: 1, Fault: StallAt(1*mib, 0)})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := sdkPart(ctx, c, testKey, id, 1, 3*mib); err == nil {
		t.Fatal("stalled upload succeeded")
	}
	s.Close() // ends the stalled handler so that it is logged
	var rec RequestRecord
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && rec.Seq == 0; time.Sleep(10 * time.Millisecond) {
		for _, r := range s.Requests() {
			if r.Op == OpUploadPart {
				rec = r
			}
		}
	}
	if rec.Fault != FaultStall || rec.BodyBytes < mib || rec.BodyBytes >= 3*mib {
		t.Fatalf("stalled request record %+v", rec)
	}
}

func TestSlowResponseUsesTheClock(t *testing.T) {
	clock := NewFakeClock(time.Time{})
	s := newTestServer(t, Options{Clock: clock})
	c := s.S3Client()
	if err := s.PutSynthetic(DefaultBucket, "obj", 1, 100); err != nil {
		t.Fatal(err)
	}
	s.AddRule(Rule{Op: OpHead, Fault: SlowResponse(40 * time.Second)})

	done := make(chan error, 1)
	go func() {
		_, err := c.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("obj")}, s3x.SingleAttemptOptions)
		done <- err
	}()
	if !clock.WaitPending(1, 5*time.Second) {
		t.Fatal("server never started waiting")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before the clock moved: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	clock.Advance(40 * time.Second)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSlowBodyThrottlesDownload(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	if err := s.PutSynthetic(DefaultBucket, "obj", 1, 1*mib); err != nil {
		t.Fatal(err)
	}
	s.AddRule(Rule{Op: OpGet, Fault: SlowBody(2 * mib)}) // 1 MiB takes about 500 ms
	start := time.Now()
	out, err := getRange(context.Background(), c, "obj", 0, 1*mib, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(readAll(t, out)); n != mib {
		t.Fatalf("read %d bytes", n)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("download took %v, throttle not applied", elapsed)
	}
}

func TestTruncatedBodyAndReset(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault Fault
	}{{"truncate", TruncateAt(100 * 1024)}, {"reset", Fault{Kind: FaultReset, AtByte: 100 * 1024}}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, Options{})
			c := s.S3Client()
			if err := s.PutSynthetic(DefaultBucket, "obj", 1, 1*mib); err != nil {
				t.Fatal(err)
			}
			s.AddRule(Rule{Op: OpGet, Attempt: 1, Fault: tc.fault})
			out, err := getRange(context.Background(), c, "obj", 0, 1*mib, "")
			if err != nil {
				t.Fatal(err) // Headers are sent before the fault.
			}
			data, err := io.ReadAll(out.Body)
			out.Body.Close()
			if err == nil || len(data) >= mib {
				t.Fatalf("read %d bytes, err %v", len(data), err)
			}
			exp := make([]byte, len(data))
			Pattern{Seed: 1}.Fill(exp, 0)
			if !bytes.Equal(data, exp) {
				t.Error("bytes before the fault are wrong")
			}
			if got := engine.Classify(err); got != engine.ClassRetryable {
				t.Errorf("short body classified as %v (%v)", got, err)
			}
			// The retry of the remaining range, as the engine does, succeeds.
			rest, err := getRange(context.Background(), c, "obj", int64(len(data)), mib-int64(len(data)), "")
			if err != nil || len(readAll(t, rest)) != mib-len(data) {
				t.Fatalf("remaining range: %v", err)
			}
		})
	}
}

func TestObjectChangesBetweenRangedGets(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()
	if err := s.PutSynthetic(DefaultBucket, "obj", 1, 1000); err != nil {
		t.Fatal(err)
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("obj")}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	etag := aws.ToString(head.ETag)
	s.AddRule(Rule{Op: OpGet, Attempt: 2, Fault: ChangeObject()})

	if _, err := getRange(ctx, c, "obj", 0, 100, etag); err != nil {
		t.Fatal(err)
	}
	_, err = getRange(ctx, c, "obj", 100, 100, etag)
	if apiCode(err) != "PreconditionFailed" {
		t.Fatalf("second ranged GET: %v", err)
	}
	// Through the engine's store the 412 is ErrObjectChanged, a permanent error.
	_, err = s.Store().GetObjectRange(ctx, engine.GetRangeRequest{Ref: engine.ObjectRef{Bucket: DefaultBucket, Key: "obj"}, Offset: 0, Length: 10, IfMatch: etag, Size: 1000})
	if !errors.Is(err, engine.ErrObjectChanged) || engine.Classify(err) != engine.ClassPermanent {
		t.Fatalf("store: %v", err)
	}
	// Without If-Match the new ETag is visible.
	out, err := getRange(ctx, c, "obj", 0, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.ETag) == etag {
		t.Error("ETag did not change")
	}
	out.Body.Close()
}

func TestCredentialFaultsAcrossAllOperations(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	if err := s.PutSynthetic(DefaultBucket, "obj", 1, 100); err != nil {
		t.Fatal(err)
	}
	id := s.AddRule(Rule{Fault: ExpiredToken()})
	ctx := context.Background()
	if _, err := getRange(ctx, c, "obj", 0, 10, ""); apiCode(err) != "ExpiredToken" {
		t.Fatalf("get: %v", err)
	}
	// A HEAD error has no body, exactly as on S3, so the SDK can report only the
	// status as a generic "BadRequest" and the credential code is lost.
	_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("obj")}, s3x.SingleAttemptOptions)
	var re *awshttp.ResponseError
	if !errors.As(err, &re) || re.HTTPStatusCode() != 400 || apiCode(err) != "BadRequest" {
		t.Fatalf("head: %v", err)
	}
	if _, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(DefaultBucket)}, s3x.SingleAttemptOptions); apiCode(err) != "ExpiredToken" {
		t.Fatalf("list: %v", err)
	}
	// Credentials "renewed": the rule is removed and requests pass.
	s.RemoveRule(id)
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("obj")}, s3x.SingleAttemptOptions); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Requests() {
		if r.AccessKeyID != AccessKeyID {
			t.Fatalf("access key %q not recorded", r.AccessKeyID)
		}
	}
}

func TestRulesMatchByKeyPrefixPartAndRange(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()
	if err := s.PutSynthetic(DefaultBucket, "a/obj", 1, 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSynthetic(DefaultBucket, "b/obj", 1, 1000); err != nil {
		t.Fatal(err)
	}
	start := int64(500)
	s.AddRule(Rule{Op: OpGet, KeyPrefix: "a/", RangeStart: &start, Fault: InternalError()})
	if _, err := getRange(ctx, c, "a/obj", 0, 10, ""); err != nil {
		t.Fatalf("range 0: %v", err)
	}
	if _, err := getRange(ctx, c, "b/obj", 500, 10, ""); err != nil {
		t.Fatalf("other prefix: %v", err)
	}
	if _, err := getRange(ctx, c, "a/obj", 500, 10, ""); apiCode(err) != "InternalError" {
		t.Fatalf("matching request: %v", err)
	}
}

func TestInProcessStoreRoundTripAndFaults(t *testing.T) {
	s := newTestServer(t, Options{})
	store := s.Store()
	ctx := context.Background()
	ref := engine.ObjectRef{Region: Region, Bucket: DefaultBucket, Key: testKey}
	src := NewSource()
	const size = 11 * mib
	src.Set("f", SyntheticFile{Size: size, Seed: 9})
	h, _ := src.Open(ctx, "f")

	id, err := store.CreateMultipartUpload(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := engine.PlanParts(size, 5*mib)
	var parts []engine.CompletedPart
	var crcs []string
	s.AddRule(Rule{Op: OpUploadPart, Part: 2, Attempt: 1, Fault: ServiceUnavailable()})
	for i := 0; i < plan.PartCount; i++ {
		pr := plan.Part(i)
		var res engine.PutResult
		for attempt := 0; attempt < 2; attempt++ {
			res, err = store.UploadPart(ctx, engine.UploadPartRequest{
				Ref: ref, UploadID: id, PartNumber: pr.Number,
				Body: io.NewSectionReader(h, pr.Offset, pr.Length), ContentLength: pr.Length,
			})
			if err == nil {
				break
			}
			if engine.Classify(err) != engine.ClassRetryable {
				t.Fatalf("part %d: %v", pr.Number, err)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, engine.CompletedPart{Number: pr.Number, ETag: res.ETag, ChecksumCRC32: res.ChecksumCRC32})
		crcs = append(crcs, res.ChecksumCRC32)
	}
	done, err := store.CompleteMultipartUpload(ctx, engine.CompleteUploadRequest{Ref: ref, UploadID: id, Parts: parts, ObjectSize: size})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := engine.CompositeCRC32(crcs)
	if done.ChecksumCRC32 != want {
		t.Fatalf("composite %q, want %q", done.ChecksumCRC32, want)
	}
	if err := s.VerifyObject(DefaultBucket, testKey, 9, 1); err != nil {
		t.Fatal(err)
	}
	head, err := store.HeadObject(ctx, ref)
	if err != nil || head.Size != size || head.ChecksumCRC32 != want {
		t.Fatalf("head %+v %v", head, err)
	}
	res, err := store.GetObjectRange(ctx, engine.GetRangeRequest{Ref: ref, Offset: 5*mib - 4, Length: 8, IfMatch: head.ETag, Size: head.Size})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	exp := make([]byte, 8)
	Pattern{Seed: 9}.Fill(exp, 5*mib-4)
	if !bytes.Equal(got, exp) {
		t.Error("ranged bytes differ")
	}
	if n := s.Attempts(OpUploadPart, DefaultBucket, testKey, 1); n != 1 {
		t.Errorf("part 1 sent %d times", n)
	}
	if n := s.Attempts(OpUploadPart, DefaultBucket, testKey, 2); n != 2 {
		t.Errorf("part 2 sent %d times", n)
	}

	// A reset in the in-process path looks like a network error, not an API error.
	s.AddRule(Rule{Op: OpPut, Attempt: 1, Fault: ResetAt(0)})
	_, err = store.PutObject(ctx, engine.PutObjectRequest{Ref: engine.ObjectRef{Bucket: DefaultBucket, Key: "k"}, Body: bytes.NewReader([]byte("x")), ContentLength: 1})
	if err == nil || engine.Classify(err) != engine.ClassRetryable {
		t.Fatalf("reset: %v", err)
	}
	// An expired token is a credential error.
	s.AddRule(Rule{Op: OpHead, Fault: ExpiredToken()})
	_, err = store.HeadObject(ctx, ref)
	if engine.Classify(err) != engine.ClassCredential {
		t.Fatalf("expired: %v", err)
	}
}

func TestInProcessStallEndsWithContext(t *testing.T) {
	s := newTestServer(t, Options{})
	s.AddRule(Rule{Op: OpHead, Fault: StallAt(0, 0)})
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel(engine.ErrStalled) }()
	_, err := s.Store().HeadObject(ctx, engine.ObjectRef{Bucket: DefaultBucket, Key: "x"})
	if !errors.Is(err, engine.ErrStalled) {
		t.Fatalf("got %v", err)
	}
}
