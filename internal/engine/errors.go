package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"s4browser/internal/fsx"
)

// Class is the engine's reaction to a failed operation.
type Class uint8

const (
	// ClassNone means no error.
	ClassNone Class = iota
	// ClassRetryable is retried with backoff under RetryPolicy.
	ClassRetryable
	// ClassCredential moves the job to JobCredentialWait and charges no attempt.
	ClassCredential
	// ClassPermanent fails the job.
	ClassPermanent
	// ClassCancelled is a deliberate stop. It charges no attempt and is not reported.
	ClassCancelled
)

func (c Class) String() string {
	switch c {
	case ClassNone:
		return "none"
	case ClassRetryable:
		return "retryable"
	case ClassCredential:
		return "credential"
	case ClassPermanent:
		return "permanent"
	case ClassCancelled:
		return "cancelled"
	}
	return "unknown"
}

// Sentinel errors. Match them with errors.Is.
var (
	// Cancellation causes, passed to context.CancelCauseFunc.

	// ErrStalled cancels an attempt the watchdog found idle. It is retryable.
	ErrStalled = errors.New("engine: transfer stalled")
	// ErrJobPaused cancels attempts of a pausing job.
	ErrJobPaused = errors.New("engine: job paused")
	// ErrJobCancelled cancels attempts of a cancelling or failing job.
	ErrJobCancelled = errors.New("engine: job cancelled")
	// ErrEngineClosed cancels attempts at shutdown and rejects later commands.
	ErrEngineClosed = errors.New("engine: closed")
	// ErrPartPreempted cancels a parked download attempt so that the job's
	// lowest unfinished part can take its slot. Bytes already written are kept.
	ErrPartPreempted = errors.New("engine: part preempted")

	// ErrCredentialsUnavailable marks a failed credential provider. internal/s3x
	// wraps provider errors with it, because the SDK reports them as plain errors.
	ErrCredentialsUnavailable = errors.New("engine: credentials unavailable")

	// Permanent failures.

	// ErrInvalidSize rejects a negative size.
	ErrInvalidSize = errors.New("engine: invalid size")
	// ErrObjectTooLarge rejects an object above MaxObjectSize.
	ErrObjectTooLarge = errors.New("engine: object exceeds the S3 size limit")
	// ErrObjectChanged means the remote object changed during a download.
	ErrObjectChanged = errors.New("engine: remote object changed during transfer")
	// ErrSourceChanged means the local file changed during an upload.
	ErrSourceChanged = errors.New("engine: source file changed during transfer")
	// ErrChecksumMismatch means S3's composite checksum differs from the local one.
	ErrChecksumMismatch = errors.New("engine: checksum mismatch")
	// ErrLocalIO wraps a failed read or write of local data.
	ErrLocalIO = errors.New("engine: local I/O failed")
	// ErrWriteRefused means a WriteGuard rejected a key.
	ErrWriteRefused = errors.New("engine: write refused by guard")
	// ErrJournal means a required journal write failed, so no upload was started.
	ErrJournal = errors.New("engine: journal write failed")

	// ErrDestinationExists means the download target appeared before commit.
	ErrDestinationExists = fsx.ErrDestinationExists
	// ErrInsufficientSpace means the destination volume cannot hold the object.
	ErrInsufficientSpace = fsx.ErrInsufficientSpace
	// ErrUnsafePath means a destination path failed validation.
	ErrUnsafePath = fsx.ErrUnsafePath
	// ErrDiskFull means a download write found the drive full. It is permanent,
	// but the scheduler pauses the job instead of failing it, so the temp file
	// and the parts already written are kept until the user resumes.
	ErrDiskFull = fsx.ErrDiskFull

	// Command errors, returned directly to the caller.

	// ErrIllegalState means a command is not valid in the job's current state.
	ErrIllegalState = errors.New("engine: command not valid in current job state")
	// ErrUnknownJob means no job has the given ID.
	ErrUnknownJob = errors.New("engine: unknown job")
	// ErrInvalidSettings means Settings.Validate failed.
	ErrInvalidSettings = errors.New("engine: invalid settings")
	// ErrInvalidConfig means New was given an unusable Config.
	ErrInvalidConfig = errors.New("engine: invalid config")
)

// OpError is the only error type ObjectStore implementations return for a
// completed request. It carries no credentials, signatures or presigned URLs.
type OpError struct {
	// Op is the operation name, such as "UploadPart".
	Op string
	// Class is the result of ClassifyResponse, or of Classify when no response arrived.
	Class Class
	// Throttled selects the longer backoff base.
	Throttled bool
	// Status is the HTTP status, or 0 when no response arrived.
	Status int
	// Code is the S3 error code, or "" when there is none.
	Code string
	// RequestID is S3's x-amz-request-id, kept for diagnostics.
	RequestID string
	// RetryAfter is the server's requested delay, or 0.
	RetryAfter time.Duration
	// Err is the underlying cause.
	Err error
}

func (e *OpError) Error() string {
	return fmt.Sprintf("%s: %s (status %d, code %q, request %q): %v",
		e.Op, e.Class, e.Status, e.Code, e.RequestID, e.Err)
}

// Unwrap returns the underlying cause.
func (e *OpError) Unwrap() error { return e.Err }

var credentialCodes = codeSet(
	"ExpiredToken", "ExpiredTokenException", "TokenRefreshRequired", "InvalidToken",
	"InvalidAccessKeyId", "InvalidSecurity", "SignatureDoesNotMatch", "RequestExpired",
	"AuthFailure", "UnrecognizedClientException", "InvalidClientTokenId",
)

var throttleCodes = codeSet(
	"SlowDown", "Throttling", "ThrottlingException", "ThrottledException",
	"RequestThrottled", "RequestThrottledException", "TooManyRequestsException",
	"RequestLimitExceeded", "BandwidthLimitExceeded", "LimitExceededException",
	"KMS.ThrottlingException",
)

// retryableCodes are transient even though most arrive with a 4xx status.
var retryableCodes = codeSet(
	"RequestTimeout", "RequestTimeoutException", "RequestTimeTooSkewed",
	"IncompleteBody", "BadDigest", "XAmzContentChecksumMismatch",
	"XAmzContentSHA256Mismatch", "OperationAborted", "ConditionalRequestConflict",
	"InternalError", "ServiceUnavailable", "Busy",
)

func codeSet(codes ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(codes))
	for _, code := range codes {
		set[code] = struct{}{}
	}
	return set
}

// ClassifyResponse maps an S3 response to a Class. The error code decides when
// it is known, then the HTTP status. throttled selects the longer backoff base.
func ClassifyResponse(status int, code string) (class Class, throttled bool) {
	if _, ok := credentialCodes[code]; ok {
		return ClassCredential, false
	}
	if _, ok := throttleCodes[code]; ok {
		return ClassRetryable, true
	}
	if _, ok := retryableCodes[code]; ok {
		return ClassRetryable, false
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ClassRetryable, true
	case status == http.StatusRequestTimeout:
		return ClassRetryable, false
	case status == http.StatusNotImplemented, status == http.StatusHTTPVersionNotSupported:
		return ClassPermanent, false
	case status >= 500:
		return ClassRetryable, false
	}
	return ClassPermanent, false
}

// CodeAmbiguousAuth marks a HEAD authorization failure whose cause could not be
// determined. It is retryable, so the retry budget bounds it.
const CodeAmbiguousAuth = "AmbiguousHeadAuth"

// IsCodelessAuthFailure reports whether a response is a 400, 401 or 403 that
// names no real S3 error code. HEAD responses have no body, so the SDK can only
// report the status text. Such a response may mean expired credentials or a real
// permission failure, and ClassifyResponse must not decide between them.
//
// An ObjectStore that sees one must find the real code before it returns, as
// internal/s3x does with a one-byte ranged GET. See docs/engine-design.md section 6.
func IsCodelessAuthFailure(status int, code string) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
	default:
		return false
	}
	switch code {
	case "", "BadRequest", "Unauthorized", "Forbidden", "UnknownError":
		return true
	}
	return false
}

// Classify returns the Class of any error seen by the engine. Attempts must use
// ClassifyAttempt, which also considers why the attempt's context ended.
//
// A failure with no S3 response, such as a connection reset, DNS failure, TLS
// error, timeout or truncated body, is retryable, so unknown errors default to
// ClassRetryable. Adapters must wrap local request-building failures in an
// OpError with ClassPermanent.
func Classify(err error) Class {
	if err == nil {
		return ClassNone
	}
	var opErr *OpError
	switch {
	case errors.Is(err, ErrStalled):
		return ClassRetryable
	case errors.Is(err, ErrJobPaused), errors.Is(err, ErrJobCancelled),
		errors.Is(err, ErrEngineClosed), errors.Is(err, ErrPartPreempted),
		errors.Is(err, context.Canceled):
		return ClassCancelled
	case errors.Is(err, ErrCredentialsUnavailable):
		return ClassCredential
	case errors.As(err, &opErr):
		return opErr.Class
	case isPermanentSentinel(err):
		return ClassPermanent
	}
	return ClassRetryable
}

// ClassifyAttempt classifies an attempt's result. A cancelled attempt context
// outranks err, because the SDK reports any cancellation as a generic failure.
func ClassifyAttempt(ctx context.Context, err error) Class {
	if err == nil {
		return ClassNone
	}
	if cause := context.Cause(ctx); cause != nil {
		return Classify(cause)
	}
	return Classify(err)
}

func isPermanentSentinel(err error) bool {
	for _, target := range []error{
		ErrInvalidSize, ErrObjectTooLarge, ErrObjectChanged, ErrSourceChanged,
		ErrChecksumMismatch, ErrLocalIO, ErrWriteRefused, ErrJournal,
		ErrDestinationExists, ErrInsufficientSpace, ErrUnsafePath, ErrDiskFull,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
