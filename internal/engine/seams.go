package engine

import (
	"context"
	"io"
	"time"
)

// ObjectRef names one object. Profile selects credentials and Region selects
// the endpoint, so ObjectStore implementations can serve several buckets.
type ObjectRef struct {
	Profile string
	Region  string
	Bucket  string
	Key     string
}

// CompletedPart is one finished part of a multipart upload.
type CompletedPart struct {
	Number int32
	ETag   string
	// ChecksumCRC32 is the base64 value S3 returned for this part.
	ChecksumCRC32 string
}

// UploadPartRequest uploads one part. Body is a fresh reader for each attempt
// and is positioned at its start. Implementations send ContentLength bytes,
// request a CRC32 trailing checksum and do not retry.
type UploadPartRequest struct {
	Ref           ObjectRef
	UploadID      string
	PartNumber    int32
	Body          io.ReadSeeker
	ContentLength int64
}

// PutObjectRequest uploads a whole object with the same rules as UploadPartRequest.
type PutObjectRequest struct {
	Ref           ObjectRef
	Body          io.ReadSeeker
	ContentLength int64
}

// PutResult is the outcome of UploadPart or PutObject.
type PutResult struct {
	ETag          string
	ChecksumCRC32 string
}

// CompleteUploadRequest finishes a multipart upload. Parts are sorted by number.
// ObjectSize is sent as x-amz-mp-object-size so S3 validates the total.
type CompleteUploadRequest struct {
	Ref        ObjectRef
	UploadID   string
	Parts      []CompletedPart
	ObjectSize int64
}

// CompleteUploadResult is S3's answer to CompleteMultipartUpload.
type CompleteUploadResult struct {
	ETag string
	// ChecksumCRC32 is the composite value in "<base64>-<parts>" form.
	ChecksumCRC32 string
}

// ObjectInfo is the subset of HeadObject the engine needs.
type ObjectInfo struct {
	Size          int64
	ETag          string
	ChecksumCRC32 string
}

// GetRangeRequest reads [Offset, Offset+Length) from one exact object version.
// IfMatch is always sent, and a 412 response returns ErrObjectChanged.
type GetRangeRequest struct {
	Ref     ObjectRef
	Offset  int64
	Length  int64
	IfMatch string
}

// GetRangeResult is an open ranged response. The caller closes Body. The
// implementation has verified status 206, the Content-Range start and end, the
// total size and the ETag, and returns ErrObjectChanged if total or ETag differ.
type GetRangeResult struct {
	Body io.ReadCloser
}

// ObjectStore is the engine's view of S3, implemented by internal/s3x and by
// internal/fakes3. Every method makes exactly one attempt and must honour ctx.
// Failures are returned as *OpError, except credential provider failures, which
// wrap ErrCredentialsUnavailable.
type ObjectStore interface {
	// CreateMultipartUpload starts an upload with CRC32 COMPOSITE checksums and
	// returns its upload ID.
	CreateMultipartUpload(ctx context.Context, ref ObjectRef) (uploadID string, err error)
	UploadPart(ctx context.Context, req UploadPartRequest) (PutResult, error)
	CompleteMultipartUpload(ctx context.Context, req CompleteUploadRequest) (CompleteUploadResult, error)
	// AbortMultipartUpload treats NoSuchUpload as success.
	AbortMultipartUpload(ctx context.Context, ref ObjectRef, uploadID string) error
	PutObject(ctx context.Context, req PutObjectRequest) (PutResult, error)
	HeadObject(ctx context.Context, ref ObjectRef) (ObjectInfo, error)
	GetObjectRange(ctx context.Context, req GetRangeRequest) (GetRangeResult, error)
}

// CredentialProber checks whether a profile's credentials work again.
type CredentialProber interface {
	// Probe discards cached credentials and fetches new ones. nil means retry.
	Probe(ctx context.Context, profile string) error
}

// SourceStat identifies a source file's content for change detection.
type SourceStat struct {
	Size    int64
	ModTime time.Time
}

// SourceHandle is an open upload source. ReadAt is safe for concurrent use.
type SourceHandle interface {
	io.ReaderAt
	Stat() (SourceStat, error)
	Close() error
}

// Source opens upload data. Production reads files, and tests synthesize bytes
// from the offset so that a simulated 2 TB run touches no disk.
type Source interface {
	Open(ctx context.Context, path string) (SourceHandle, error)
}

// SinkSpec describes a download destination.
type SinkSpec struct {
	JobID JobID
	// DestPath is the absolute, already validated final path.
	DestPath string
	Size     int64
}

// SinkWriter receives one download. WriteAt is safe for concurrent use on
// non-overlapping ranges. Exactly one of Commit or Abort ends its life.
type SinkWriter interface {
	io.WriterAt
	// MaxLead is the furthest a write may start past the lowest unfinished
	// offset. 0 means unlimited.
	MaxLead() int64
	// Commit flushes, closes and moves the data to DestPath without replacing
	// an existing file. It returns ErrDestinationExists on collision.
	Commit(ctx context.Context) error
	// Abort closes and removes all temporary data.
	Abort() error
}

// Sink creates download destinations. Prepare checks free space and collisions,
// returning ErrInsufficientSpace or ErrDestinationExists.
type Sink interface {
	Prepare(ctx context.Context, spec SinkSpec) (SinkWriter, error)
}

// Timer is the subset of time.Timer the engine uses.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Ticker is the subset of time.Ticker the engine uses.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock supplies all engine time, so tests can run hours of backoff instantly.
// Engine code must not call time.Now, time.Sleep, time.After or time.NewTimer.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

// BandwidthLimiter throttles one transfer direction across all jobs.
type BandwidthLimiter interface {
	// WaitN blocks until n bytes may pass or ctx ends. n may exceed the burst.
	WaitN(ctx context.Context, n int) error
	// SetRate applies a new limit immediately. 0 means unlimited.
	SetRate(bytesPerSec int64)
}
