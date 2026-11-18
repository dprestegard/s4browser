package s3x

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"s4browser/internal/engine"
)

// ClientFunc returns the S3 client for an object's profile and region.
type ClientFunc func(ctx context.Context, ref engine.ObjectRef) (*s3.Client, error)

// StaticClient serves every object from one client.
func StaticClient(client *s3.Client) ClientFunc {
	return func(context.Context, engine.ObjectRef) (*s3.Client, error) { return client, nil }
}

// Store implements engine.ObjectStore with the AWS SDK. Every call makes exactly
// one attempt, because the engine owns retry policy.
type Store struct {
	clients ClientFunc
}

var _ engine.ObjectStore = (*Store)(nil)

// NewStore returns a Store that obtains clients from clients.
func NewStore(clients ClientFunc) *Store { return &Store{clients: clients} }

// CreateMultipartUpload implements engine.ObjectStore.
func (s *Store) CreateMultipartUpload(ctx context.Context, ref engine.ObjectRef) (string, error) {
	const op = "CreateMultipartUpload"
	client, err := s.clients(ctx, ref)
	if err != nil {
		return "", toOpError(op, err)
	}
	out, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:            aws.String(ref.Bucket),
		Key:               aws.String(ref.Key),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
		ChecksumType:      types.ChecksumTypeComposite,
	}, SingleAttemptOptions)
	if err != nil {
		return "", toOpError(op, err)
	}
	return aws.ToString(out.UploadId), nil
}

// UploadPart implements engine.ObjectStore.
func (s *Store) UploadPart(ctx context.Context, req engine.UploadPartRequest) (engine.PutResult, error) {
	const op = "UploadPart"
	client, err := s.clients(ctx, req.Ref)
	if err != nil {
		return engine.PutResult{}, toOpError(op, err)
	}
	out, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:            aws.String(req.Ref.Bucket),
		Key:               aws.String(req.Ref.Key),
		UploadId:          aws.String(req.UploadID),
		PartNumber:        aws.Int32(req.PartNumber),
		Body:              req.Body,
		ContentLength:     aws.Int64(req.ContentLength),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	}, SingleAttemptOptions)
	if err != nil {
		return engine.PutResult{}, toOpError(op, err)
	}
	return engine.PutResult{ETag: aws.ToString(out.ETag), ChecksumCRC32: aws.ToString(out.ChecksumCRC32)}, nil
}

// CompleteMultipartUpload implements engine.ObjectStore.
func (s *Store) CompleteMultipartUpload(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
	const op = "CompleteMultipartUpload"
	client, err := s.clients(ctx, req.Ref)
	if err != nil {
		return engine.CompleteUploadResult{}, toOpError(op, err)
	}
	parts := make([]types.CompletedPart, len(req.Parts))
	for i, part := range req.Parts {
		parts[i] = types.CompletedPart{
			PartNumber:    aws.Int32(part.Number),
			ETag:          aws.String(part.ETag),
			ChecksumCRC32: aws.String(part.ChecksumCRC32),
		}
	}
	out, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(req.Ref.Bucket),
		Key:             aws.String(req.Ref.Key),
		UploadId:        aws.String(req.UploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		ChecksumType:    types.ChecksumTypeComposite,
		MpuObjectSize:   aws.Int64(req.ObjectSize),
	}, SingleAttemptOptions)
	if err != nil {
		return engine.CompleteUploadResult{}, toOpError(op, err)
	}
	return engine.CompleteUploadResult{ETag: aws.ToString(out.ETag), ChecksumCRC32: aws.ToString(out.ChecksumCRC32)}, nil
}

// AbortMultipartUpload implements engine.ObjectStore. NoSuchUpload is success.
func (s *Store) AbortMultipartUpload(ctx context.Context, ref engine.ObjectRef, uploadID string) error {
	const op = "AbortMultipartUpload"
	client, err := s.clients(ctx, ref)
	if err != nil {
		return toOpError(op, err)
	}
	_, err = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(ref.Bucket),
		Key:      aws.String(ref.Key),
		UploadId: aws.String(uploadID),
	}, SingleAttemptOptions)
	if err == nil {
		return nil
	}
	mapped := toOpError(op, err)
	var opErr *engine.OpError
	if errors.As(mapped, &opErr) && opErr.Code == "NoSuchUpload" {
		return nil
	}
	return mapped
}

// PutObject implements engine.ObjectStore.
func (s *Store) PutObject(ctx context.Context, req engine.PutObjectRequest) (engine.PutResult, error) {
	const op = "PutObject"
	client, err := s.clients(ctx, req.Ref)
	if err != nil {
		return engine.PutResult{}, toOpError(op, err)
	}
	out, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            aws.String(req.Ref.Bucket),
		Key:               aws.String(req.Ref.Key),
		Body:              req.Body,
		ContentLength:     aws.Int64(req.ContentLength),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	}, SingleAttemptOptions)
	if err != nil {
		return engine.PutResult{}, toOpError(op, err)
	}
	return engine.PutResult{ETag: aws.ToString(out.ETag), ChecksumCRC32: aws.ToString(out.ChecksumCRC32)}, nil
}

// HeadObject implements engine.ObjectStore. It always asks for the checksum,
// which lost-Complete recovery compares with the local composite value.
//
// A HEAD error has no body, so a 400 or 403 cannot say why it failed. See
// diagnoseHead for how the real cause is found.
func (s *Store) HeadObject(ctx context.Context, ref engine.ObjectRef) (engine.ObjectInfo, error) {
	const op = "HeadObject"
	client, err := s.clients(ctx, ref)
	if err != nil {
		return engine.ObjectInfo{}, toOpError(op, err)
	}
	out, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:       aws.String(ref.Bucket),
		Key:          aws.String(ref.Key),
		ChecksumMode: types.ChecksumModeEnabled,
	}, SingleAttemptOptions)
	if err == nil {
		return engine.ObjectInfo{
			Size:          aws.ToInt64(out.ContentLength),
			ETag:          aws.ToString(out.ETag),
			ChecksumCRC32: aws.ToString(out.ChecksumCRC32),
		}, nil
	}
	mapped := toOpError(op, err)
	var opErr *engine.OpError
	if errors.As(mapped, &opErr) && engine.IsCodelessAuthFailure(opErr.Status, opErr.Code) {
		return engine.ObjectInfo{}, diagnoseHead(ctx, client, ref, opErr)
	}
	return engine.ObjectInfo{}, mapped
}

// diagnoseHead finds out why a HEAD failed with a bare 400 or 403. It repeats the
// request as a one-byte GET, which needs the same permission and whose error
// response does carry an S3 code:
//
//   - A real code decides: ExpiredToken waits for credentials and AccessDenied fails.
//   - Success, or 416 on an empty object, shows access works. The HEAD is retried.
//   - No answer or no code leaves the cause unknown. That is retryable, so the
//     retry budget ends it and a real permission failure can never retry forever.
func diagnoseHead(ctx context.Context, client *s3.Client, ref engine.ObjectRef, head *engine.OpError) error {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(ref.Bucket),
		Key:    aws.String(ref.Key),
		Range:  aws.String("bytes=0-0"),
	}, SingleAttemptOptions)
	if err == nil {
		_ = out.Body.Close()
		return ambiguousHead(head, nil)
	}
	mapped := toOpError("HeadObject", err)
	var diag *engine.OpError
	if !errors.As(mapped, &diag) {
		return mapped // A credential provider failure.
	}
	switch {
	case diag.Status == 0:
		if diag.Class == engine.ClassCancelled {
			return diag
		}
		return ambiguousHead(head, err)
	case diag.Status == http.StatusRequestedRangeNotSatisfiable:
		return ambiguousHead(head, nil)
	case engine.IsCodelessAuthFailure(diag.Status, diag.Code):
		return ambiguousHead(head, err)
	}
	return diag
}

func ambiguousHead(head *engine.OpError, cause error) error {
	if cause == nil {
		cause = head.Err
	}
	return &engine.OpError{
		Op: "HeadObject", Class: engine.ClassRetryable, Status: head.Status,
		Code: engine.CodeAmbiguousAuth, RequestID: head.RequestID, Err: cause,
	}
}

// GetObjectRange implements engine.ObjectStore. It checks the status, the ETag and
// the whole Content-Range, including the total against req.Size. A different ETag
// or total means the object changed, which is permanent.
func (s *Store) GetObjectRange(ctx context.Context, req engine.GetRangeRequest) (engine.GetRangeResult, error) {
	const op = "GetObject"
	client, err := s.clients(ctx, req.Ref)
	if err != nil {
		return engine.GetRangeResult{}, toOpError(op, err)
	}
	if req.Offset < 0 || req.Length <= 0 || req.Size <= 0 || req.Offset+req.Length > req.Size {
		return engine.GetRangeResult{}, &engine.OpError{Op: op, Class: engine.ClassPermanent, Err: engine.ErrInvalidSize}
	}
	last := req.Offset + req.Length - 1
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:  aws.String(req.Ref.Bucket),
		Key:     aws.String(req.Ref.Key),
		Range:   aws.String(fmt.Sprintf("bytes=%d-%d", req.Offset, last)),
		IfMatch: aws.String(req.IfMatch),
	}, SingleAttemptOptions)
	if err != nil {
		mapped := toOpError(op, err)
		var opErr *engine.OpError
		if errors.As(mapped, &opErr) && opErr.Status == http.StatusPreconditionFailed {
			opErr.Class = engine.ClassPermanent
			opErr.Err = fmt.Errorf("%w: %w", engine.ErrObjectChanged, opErr.Err)
		}
		return engine.GetRangeResult{}, mapped
	}
	if etag := aws.ToString(out.ETag); etag != req.IfMatch {
		_ = out.Body.Close()
		return engine.GetRangeResult{}, &engine.OpError{Op: op, Class: engine.ClassPermanent,
			Err: fmt.Errorf("%w: ETag %s, expected %s", engine.ErrObjectChanged, etag, req.IfMatch)}
	}
	if err := checkContentRange(aws.ToString(out.ContentRange), req.Offset, last, req.Size); err != nil {
		_ = out.Body.Close()
		class := engine.ClassRetryable
		if errors.Is(err, engine.ErrObjectChanged) {
			class = engine.ClassPermanent
		}
		return engine.GetRangeResult{}, &engine.OpError{Op: op, Class: class, Err: err}
	}
	return engine.GetRangeResult{Body: out.Body}, nil
}

// checkContentRange verifies "bytes <first>-<last>/<total>". A malformed header or
// a different range is a broken response and is retried. A different total means
// the object was replaced.
func checkContentRange(header string, first, last, size int64) error {
	want := fmt.Sprintf("bytes %d-%d/", first, last)
	if !strings.HasPrefix(header, want) {
		return fmt.Errorf("s3x: Content-Range %q does not match the requested range %d-%d: %w", header, first, last, io.ErrUnexpectedEOF)
	}
	total, err := strconv.ParseInt(strings.TrimPrefix(header, want), 10, 64)
	if err != nil {
		return fmt.Errorf("s3x: Content-Range %q has no usable total: %w", header, io.ErrUnexpectedEOF)
	}
	if total != size {
		return fmt.Errorf("%w: Content-Range total %d, expected %d", engine.ErrObjectChanged, total, size)
	}
	return nil
}

// toOpError converts an SDK failure into the engine's error type.
func toOpError(op string, err error) error {
	if errors.Is(err, engine.ErrCredentialsUnavailable) {
		return err
	}
	out := &engine.OpError{Op: op, Err: err}

	var responseErr *awshttp.ResponseError
	if errors.As(err, &responseErr) {
		out.Status = responseErr.HTTPStatusCode()
		out.RequestID = responseErr.ServiceRequestID()
		if response := responseErr.Response; response != nil && response.Response != nil {
			out.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"))
		}
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		out.Code = apiErr.ErrorCode()
	}

	// A success status with no error code means the connection failed while the
	// response was being read. No complete answer arrived, so the outcome is unknown.
	if out.Code == "" && out.Status >= 200 && out.Status < 300 {
		out.Status = 0
	}

	switch {
	case out.Status != 0 || out.Code != "":
		out.Class, out.Throttled = engine.ClassifyResponse(out.Status, out.Code)
	case neverSent(err):
		out.Class = engine.ClassPermanent
	default:
		out.Class = engine.Classify(err)
	}
	return out
}

// neverSent reports a request rejected locally because its input is invalid.
func neverSent(err error) bool {
	var invalid smithy.InvalidParamsError
	var serialization *smithy.SerializationError
	return errors.As(err, &invalid) || errors.As(err, &serialization)
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
