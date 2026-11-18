package fakes3

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"s4browser/internal/engine"
	"s4browser/internal/journal"
	"s4browser/internal/s3x"
)

func newGuard(t *testing.T) *engine.TestPrefixGuard {
	t.Helper()
	g, err := engine.NewTestPrefixGuard("run1")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var forbiddenKeys = []string{
	"outside.bin",
	"s4browser-test/other-run/x",
	"s4browser-test/run1",
	"s4browser-test/run1/",
	"s4browser-test/run1/../escape",
	"s4browser-test/run1//double",
	"s4browser-test/run1\\back",
}

// TestServerGuardRefusesWritesWithNoEffect drives the HTTPS endpoint with the
// real SDK. Every refused write must leave no object, upload or part behind.
func TestServerGuardRefusesWritesWithNoEffect(t *testing.T) {
	s := newTestServer(t, Options{Guard: newGuard(t)})
	c := s.S3Client()
	ctx := context.Background()

	for _, key := range forbiddenKeys {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String(key), Body: bytes.NewReader([]byte("data"))}, s3x.SingleAttemptOptions)
		if apiCode(err) != "AccessDenied" {
			t.Errorf("put %q: %v", key, err)
		}
		_, err = c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(DefaultBucket), Key: aws.String(key)}, s3x.SingleAttemptOptions)
		if apiCode(err) != "AccessDenied" {
			t.Errorf("create %q: %v", key, err)
		}
		_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String(key)}, s3x.SingleAttemptOptions)
		if apiCode(err) != "AccessDenied" {
			t.Errorf("delete %q: %v", key, err)
		}
	}
	if n := s.ObjectCount(DefaultBucket); n != 0 {
		t.Errorf("%d objects exist after refused puts", n)
	}
	if n := len(s.ActiveUploads(DefaultBucket)); n != 0 {
		t.Errorf("%d uploads exist after refused creates", n)
	}
	if got, want := s.RefusedWrites(), 3*len(forbiddenKeys); got != want {
		t.Errorf("refused %d writes, want %d", got, want)
	}
	for _, r := range s.Requests() {
		if !r.Refused || r.Status != 403 {
			t.Errorf("record %+v", r)
		}
	}
	// A refused request never reaches the rule engine, so it has no attempt.
	if n := s.Attempts(OpPut, DefaultBucket, "outside.bin", 0); n != 0 {
		t.Errorf("refused put counted as %d attempts", n)
	}

	// Existing data outside the prefix survives a refused delete and abort.
	if err := s.PutSynthetic(DefaultBucket, "precious", 1, 10); err != nil {
		t.Fatal(err)
	}
	_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("precious")}, s3x.SingleAttemptOptions)
	if apiCode(err) != "AccessDenied" {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := s.Object(DefaultBucket, "precious"); !ok {
		t.Fatal("refused delete removed the object")
	}

	// Inside the prefix everything works, including a full multipart round trip.
	key := "s4browser-test/run1/ok/object.bin"
	_, done := sdkMultipart(t, s, key, 6*mib, 5*mib, 4)
	if aws.ToString(done.ChecksumCRC32) == "" {
		t.Error("no checksum")
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("s4browser-test/run1/small"), Body: bytes.NewReader([]byte("x"))}, s3x.SingleAttemptOptions); err != nil {
		t.Fatal(err)
	}
}

func TestServerGuardRefusesPartAndCompleteForForeignUploads(t *testing.T) {
	// An upload started before the guard existed, on a key outside the prefix,
	// cannot be extended, completed or aborted through the guarded server.
	s := newTestServer(t, Options{})
	c := s.S3Client()
	id := sdkCreate(t, c, "foreign/key")

	s.core.guard = newGuard(t) // no request is in flight, so this is safe
	gc := c
	if _, err := sdkPart(context.Background(), gc, "foreign/key", id, 1, 10); apiCode(err) != "AccessDenied" {
		t.Fatalf("part: %v", err)
	}
	_, err := gc.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{Bucket: aws.String(DefaultBucket), Key: aws.String("foreign/key"), UploadId: aws.String(id)}, s3x.SingleAttemptOptions)
	if apiCode(err) != "AccessDenied" {
		t.Fatalf("abort: %v", err)
	}
	_, err = gc.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String("foreign/key"), UploadId: aws.String(id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: aws.String(`"x"`)}}},
	}, s3x.SingleAttemptOptions)
	if apiCode(err) != "AccessDenied" {
		t.Fatalf("complete: %v", err)
	}
	if ups := s.ActiveUploads(DefaultBucket); len(ups) != 1 || len(ups[0].Parts) != 0 {
		t.Fatalf("uploads after refused calls: %+v", ups)
	}
}

// TestGuardDecoratorStopsWritesBeforeTheStore proves the engine-side decorator:
// a refused call returns ErrWriteRefused and the wrapped store sees nothing.
func TestGuardDecoratorStopsWritesBeforeTheStore(t *testing.T) {
	s := newTestServer(t, Options{})
	guarded := GuardStore(s.Store(), newGuard(t))
	ctx := context.Background()
	body := func() *bytes.Reader { return bytes.NewReader([]byte("x")) }

	for _, key := range forbiddenKeys {
		ref := engine.ObjectRef{Bucket: DefaultBucket, Key: key}
		calls := map[string]error{}
		_, calls["create"] = guarded.CreateMultipartUpload(ctx, ref)
		_, calls["part"] = guarded.UploadPart(ctx, engine.UploadPartRequest{Ref: ref, UploadID: "u", PartNumber: 1, Body: body(), ContentLength: 1})
		_, calls["complete"] = guarded.CompleteMultipartUpload(ctx, engine.CompleteUploadRequest{Ref: ref, UploadID: "u", Parts: []engine.CompletedPart{{Number: 1}}})
		calls["abort"] = guarded.AbortMultipartUpload(ctx, ref, "u")
		_, calls["put"] = guarded.PutObject(ctx, engine.PutObjectRequest{Ref: ref, Body: body(), ContentLength: 1})
		for name, err := range calls {
			if !errors.Is(err, engine.ErrWriteRefused) {
				t.Errorf("%s %q: %v", name, key, err)
			} else if engine.Classify(err) != engine.ClassPermanent {
				t.Errorf("%s %q classified %v", name, key, engine.Classify(err))
			}
		}
	}
	if n := len(s.Requests()); n != 0 {
		t.Fatalf("the store saw %d requests", n)
	}
	if s.ObjectCount(DefaultBucket) != 0 || len(s.ActiveUploads(DefaultBucket)) != 0 {
		t.Fatal("a refused write changed state")
	}

	// Allowed keys pass straight through, and reads are never guarded.
	ref := engine.ObjectRef{Bucket: DefaultBucket, Key: "s4browser-test/run1/a"}
	if _, err := guarded.PutObject(ctx, engine.PutObjectRequest{Ref: ref, Body: body(), ContentLength: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.HeadObject(ctx, engine.ObjectRef{Bucket: DefaultBucket, Key: "anything/else"}); err == nil {
		t.Fatal("head of a missing key succeeded")
	} else if errors.Is(err, engine.ErrWriteRefused) {
		t.Fatal("a read was refused")
	}
}

func TestGuardCleanerRefusesAbortOutsidePrefix(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	id := sdkCreate(t, c, "foreign/key")
	cleaner := GuardCleaner(s.Cleaner(), newGuard(t))
	entry := journal.Entry{ID: "j-1", Kind: journal.KindMultipartUpload, Phase: journal.PhaseActive,
		CreatedAt: time.Now(), Region: Region, Bucket: DefaultBucket, Key: "foreign/key", UploadID: id}
	err := cleaner.AbortUpload(context.Background(), entry, id)
	if !errors.Is(err, engine.ErrWriteRefused) {
		t.Fatalf("abort: %v", err)
	}
	if len(s.ActiveUploads(DefaultBucket)) != 1 {
		t.Fatal("the foreign upload was aborted")
	}
	// The unguarded cleaner aborts it, then reports ErrUploadGone.
	if err := s.Cleaner().AbortUpload(context.Background(), entry, id); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleaner().AbortUpload(context.Background(), entry, id); !errors.Is(err, journal.ErrUploadGone) {
		t.Fatalf("second abort: %v", err)
	}
	ups, err := s.Cleaner().ListUploads(context.Background(), entry)
	if err != nil || len(ups) != 0 {
		t.Fatalf("list: %v %v", ups, err)
	}
}
