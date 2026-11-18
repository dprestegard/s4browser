package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestClassifyResponse(t *testing.T) {
	tests := []struct {
		status        int
		code          string
		want          Class
		wantThrottled bool
	}{
		{503, "SlowDown", ClassRetryable, true},
		{503, "ServiceUnavailable", ClassRetryable, false},
		{500, "InternalError", ClassRetryable, false},
		{200, "InternalError", ClassRetryable, false}, // Complete can fail inside a 200.
		{502, "", ClassRetryable, false},
		{504, "", ClassRetryable, false},
		{429, "", ClassRetryable, true},
		{408, "", ClassRetryable, false},
		{400, "RequestTimeout", ClassRetryable, false},
		{400, "IncompleteBody", ClassRetryable, false},
		{400, "BadDigest", ClassRetryable, false},
		{403, "RequestTimeTooSkewed", ClassRetryable, false},
		{409, "OperationAborted", ClassRetryable, false},
		{409, "ConditionalRequestConflict", ClassRetryable, false},
		{400, "ExpiredToken", ClassCredential, false},
		{400, "TokenRefreshRequired", ClassCredential, false},
		{400, "InvalidToken", ClassCredential, false},
		{403, "InvalidAccessKeyId", ClassCredential, false},
		{403, "SignatureDoesNotMatch", ClassCredential, false},
		{403, "InvalidSecurity", ClassCredential, false},
		{403, "AccessDenied", ClassPermanent, false},
		{404, "NoSuchKey", ClassPermanent, false},
		{404, "NoSuchBucket", ClassPermanent, false},
		{404, "NoSuchUpload", ClassPermanent, false},
		{400, "InvalidPart", ClassPermanent, false},
		{400, "InvalidPartOrder", ClassPermanent, false},
		{400, "EntityTooSmall", ClassPermanent, false},
		{400, "EntityTooLarge", ClassPermanent, false},
		{412, "PreconditionFailed", ClassPermanent, false},
		{416, "InvalidRange", ClassPermanent, false},
		{301, "PermanentRedirect", ClassPermanent, false},
		{501, "NotImplemented", ClassPermanent, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", tt.status, tt.code), func(t *testing.T) {
			got, throttled := ClassifyResponse(tt.status, tt.code)
			if got != tt.want || throttled != tt.wantThrottled {
				t.Fatalf("got %v throttled=%v, want %v throttled=%v", got, throttled, tt.want, tt.wantThrottled)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Class
	}{
		{"nil", nil, ClassNone},
		{"stall", ErrStalled, ClassRetryable},
		{"wrapped stall", fmt.Errorf("x: %w", ErrStalled), ClassRetryable},
		{"truncated body", io.ErrUnexpectedEOF, ClassRetryable},
		{"deadline", context.DeadlineExceeded, ClassRetryable},
		{"unknown transport", errors.New("connection reset by peer"), ClassRetryable},
		{"cancel", context.Canceled, ClassCancelled},
		{"pause cause", ErrJobPaused, ClassCancelled},
		{"cancel cause", ErrJobCancelled, ClassCancelled},
		{"closing cause", ErrEngineClosed, ClassCancelled},
		{"preempted cause", ErrPartPreempted, ClassCancelled},
		{"provider failure", fmt.Errorf("get identity: %w", ErrCredentialsUnavailable), ClassCredential},
		{"op error", &OpError{Class: ClassPermanent, Err: errors.New("denied")}, ClassPermanent},
		{"wrapped op error", fmt.Errorf("x: %w", &OpError{Class: ClassCredential, Err: errors.New("e")}), ClassCredential},
		{"object changed", ErrObjectChanged, ClassPermanent},
		{"source changed", ErrSourceChanged, ClassPermanent},
		{"local io", fmt.Errorf("%w: disk", ErrLocalIO), ClassPermanent},
		{"destination exists", ErrDestinationExists, ClassPermanent},
		{"no space", ErrInsufficientSpace, ClassPermanent},
		{"guard", ErrWriteRefused, ClassPermanent},
		{"journal", ErrJournal, ClassPermanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyAttemptPrefersContextCause(t *testing.T) {
	generic := errors.New("operation error S3: UploadPart: canceled")

	stalled, cancel := context.WithCancelCause(context.Background())
	cancel(ErrStalled)
	if got := ClassifyAttempt(stalled, generic); got != ClassRetryable {
		t.Errorf("stalled attempt: got %v", got)
	}

	paused, cancel := context.WithCancelCause(context.Background())
	cancel(ErrJobPaused)
	if got := ClassifyAttempt(paused, generic); got != ClassCancelled {
		t.Errorf("paused attempt: got %v", got)
	}

	if got := ClassifyAttempt(context.Background(), &OpError{Class: ClassPermanent, Err: generic}); got != ClassPermanent {
		t.Errorf("live attempt: got %v", got)
	}
	if got := ClassifyAttempt(paused, nil); got != ClassNone {
		t.Errorf("successful attempt: got %v", got)
	}
}
