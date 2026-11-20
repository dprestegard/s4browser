package fakes3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"s4browser/internal/engine"
)

// Op names an S3 operation.
type Op string

// Operations the fake serves.
const (
	OpCreateUpload Op = "CreateMultipartUpload"
	OpUploadPart   Op = "UploadPart"
	OpComplete     Op = "CompleteMultipartUpload"
	OpAbort        Op = "AbortMultipartUpload"
	OpListUploads  Op = "ListMultipartUploads"
	OpListParts    Op = "ListParts"
	OpPut          Op = "PutObject"
	OpHead         Op = "HeadObject"
	OpGet          Op = "GetObject"
	OpDelete       Op = "DeleteObject"
	OpListObjects  Op = "ListObjectsV2"
	OpListBuckets  Op = "ListBuckets"
	OpHeadBucket   Op = "HeadBucket"
	OpLocation     Op = "GetBucketLocation"
	// OpDeleteObjects is the batch delete. The guard checks each key in it, and
	// a refused key is reported as that key's error.
	OpDeleteObjects Op = "DeleteObjects"
	OpGetTagging    Op = "GetObjectTagging"
	OpPutTagging    Op = "PutObjectTagging"
	OpDeleteTagging Op = "DeleteObjectTagging"
	OpVersioning    Op = "GetBucketVersioning"
)

// mutating reports whether the write guard must approve op.
func (o Op) mutating() bool {
	switch o {
	case OpCreateUpload, OpUploadPart, OpComplete, OpAbort, OpPut, OpDelete, OpPutTagging, OpDeleteTagging:
		return true
	}
	return false
}

// FaultKind selects what a Fault does.
type FaultKind uint8

// Fault kinds.
const (
	// FaultError answers with an S3 error (Status, Code, Message).
	FaultError FaultKind = iota + 1
	// FaultReset kills the connection with a TCP reset once AtByte body bytes
	// have moved. AtByte 0 resets before anything is read or sent.
	FaultReset
	// FaultStall stops moving bytes after AtByte body bytes. Delay 0 stalls
	// until the request context ends, otherwise for Delay and then continues.
	FaultStall
	// FaultSlow delays the response by Delay and throttles body bytes to BytesPerSec.
	FaultSlow
	// FaultTruncate (GET) sends only AtByte bytes of the promised range, then
	// closes the connection cleanly.
	FaultTruncate
	// FaultChangeObject gives the addressed object a new ETag before the
	// request is served, as if another writer replaced it.
	FaultChangeObject
)

// Fault is what a matching Rule does to a request.
type Fault struct {
	Kind FaultKind

	// Status, Code, Message and RetryAfter describe a FaultError.
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	// In200 sends the error inside a 200 response, as S3 does for Complete.
	In200 bool

	// AtByte is the body offset for reset, stall and truncate.
	AtByte int64
	// After performs the operation first and applies the fault instead of the
	// response, so the work is done but the client never learns of it.
	After bool
	// AfterHeaders sends GET response headers before a body-level fault.
	AfterHeaders bool
	// Delay is the stall or slow-response duration.
	Delay time.Duration
	// BytesPerSec throttles body transfer for FaultSlow.
	BytesPerSec int64
}

// AccessDenied is a 403 AccessDenied, as for an IAM policy that refuses the call.
func AccessDenied() Fault {
	return Fault{Kind: FaultError, Status: http.StatusForbidden, Code: "AccessDenied", Message: "Access Denied"}
}

// InternalError is a 500 InternalError.
func InternalError() Fault {
	return Fault{Kind: FaultError, Status: http.StatusInternalServerError, Code: "InternalError", Message: "We encountered an internal error. Please try again."}
}

// ServiceUnavailable is a 503 ServiceUnavailable.
func ServiceUnavailable() Fault {
	return Fault{Kind: FaultError, Status: http.StatusServiceUnavailable, Code: "ServiceUnavailable", Message: "Service Unavailable"}
}

// SlowDown is S3's 503 SlowDown throttle.
func SlowDown() Fault {
	return Fault{Kind: FaultError, Status: http.StatusServiceUnavailable, Code: "SlowDown", Message: "Please reduce your request rate."}
}

// ExpiredToken is the 400 ExpiredToken that expired session credentials get.
func ExpiredToken() Fault {
	return Fault{Kind: FaultError, Status: http.StatusBadRequest, Code: "ExpiredToken", Message: "The provided token has expired."}
}

// InvalidAccessKeyID is the 403 InvalidAccessKeyId for an unknown key.
func InvalidAccessKeyID() Fault {
	return Fault{Kind: FaultError, Status: http.StatusForbidden, Code: "InvalidAccessKeyId", Message: "The AWS Access Key Id you provided does not exist in our records."}
}

// ResetAt resets the connection after n body bytes.
func ResetAt(n int64) Fault { return Fault{Kind: FaultReset, AtByte: n} }

// LostResponse performs the operation, then resets the connection instead of
// answering. A retry of Complete then finds NoSuchUpload.
func LostResponse() Fault { return Fault{Kind: FaultReset, After: true} }

// StallAt stops after n body bytes for d, or until the request ends when d is 0.
func StallAt(n int64, d time.Duration) Fault { return Fault{Kind: FaultStall, AtByte: n, Delay: d} }

// SlowResponse delays the response by d.
func SlowResponse(d time.Duration) Fault { return Fault{Kind: FaultSlow, Delay: d} }

// SlowBody throttles body transfer to bytesPerSec.
func SlowBody(bytesPerSec int64) Fault { return Fault{Kind: FaultSlow, BytesPerSec: bytesPerSec} }

// TruncateAt makes a GET send only n bytes of its range.
func TruncateAt(n int64) Fault { return Fault{Kind: FaultTruncate, AtByte: n} }

// ChangeObject replaces the object's ETag before the request is served.
func ChangeObject() Fault { return Fault{Kind: FaultChangeObject} }

// ErrorIn200 delivers an error fault inside a 200 response.
func (f Fault) ErrorIn200() Fault { f.In200 = true; return f }

// WithRetryAfter adds a Retry-After hint to an error fault.
func (f Fault) WithRetryAfter(d time.Duration) Fault { f.RetryAfter = d; return f }

// Afterwards makes the fault apply after the operation has been performed.
func (f Fault) Afterwards() Fault { f.After = true; return f }

// Rule applies Fault to the requests it matches. Zero fields match anything.
//
// Attempt numbers count requests with the same operation, bucket, key and part
// number, starting at 1, so a rule is deterministic however requests for
// different keys or parts interleave.
type Rule struct {
	Op        Op
	Bucket    string
	Key       string
	KeyPrefix string
	// Part is the part number, or 0 for any.
	Part int32
	// Attempt matches exactly the nth attempt. FromAttempt matches the nth and
	// every later one. Both zero matches all attempts.
	Attempt     int
	FromAttempt int
	// RangeStart matches a ranged GET that starts at this offset.
	RangeStart *int64
	// AccessKey matches requests signed with this access key ID, so a test can
	// expire one set of credentials while another keeps working.
	AccessKey string
	// Times limits how often the rule fires. 0 means no limit.
	Times int
	Fault Fault
}

// RuleID identifies an installed rule.
type RuleID int

type installedRule struct {
	id    RuleID
	rule  Rule
	fired int
}

func (r *installedRule) matches(info *reqInfo, attempt int) bool {
	rule := &r.rule
	switch {
	case rule.Times > 0 && r.fired >= rule.Times:
		return false
	case rule.Op != "" && rule.Op != info.op:
		return false
	case rule.Bucket != "" && rule.Bucket != info.bucket:
		return false
	case rule.Key != "" && rule.Key != info.key:
		return false
	case rule.KeyPrefix != "" && !strings.HasPrefix(info.key, rule.KeyPrefix):
		return false
	case rule.Part != 0 && rule.Part != info.part:
		return false
	case rule.Attempt != 0 && rule.Attempt != attempt:
		return false
	case rule.FromAttempt != 0 && attempt < rule.FromAttempt:
		return false
	case rule.RangeStart != nil && (info.rangeStart < 0 || *rule.RangeStart != info.rangeStart):
		return false
	case rule.AccessKey != "" && rule.AccessKey != info.accessKey:
		return false
	}
	return true
}

// RequestRecord is the log entry of one request.
type RequestRecord struct {
	Seq         int
	Op          Op
	Bucket      string
	Key         string
	UploadID    string
	Part        int32
	Attempt     int
	AccessKeyID string
	// Status is the HTTP status the operation produced, or 0 when the
	// connection was cut.
	Status int
	Code   string
	// BodyBytes is how many request body bytes the server consumed.
	BodyBytes int64
	// Fault is the kind of fault that fired, or 0.
	Fault FaultKind
	// Refused is set when the write guard rejected the request.
	Refused bool
}

// Errors the fault layer produces below the HTTP layer.
var (
	errReset    = fmt.Errorf("fakes3: connection reset by peer: %w", syscall.ECONNRESET)
	errTruncate = io.ErrUnexpectedEOF
)

// apiError is an S3 error response.
type apiError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	In200      bool
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func s3err(status int, code, message string) *apiError {
	return &apiError{Status: status, Code: code, Message: message}
}

// toOpError converts an API error to the engine's error type.
func (e *apiError) toOpError(op Op, requestID string) *engine.OpError {
	class, throttled := engine.ClassifyResponse(e.Status, e.Code)
	return &engine.OpError{
		Op: string(op), Class: class, Throttled: throttled, Status: e.Status,
		Code: e.Code, RequestID: requestID, RetryAfter: e.RetryAfter, Err: e,
	}
}

// faultReader applies a body-level fault while data moves through it.
type faultReader struct {
	ctx   context.Context
	clock engine.Clock
	r     io.Reader
	f     Fault
	n     int64
	done  bool // the AtByte action has fired
}

func (fr *faultReader) Read(p []byte) (int, error) {
	f := &fr.f
	switch f.Kind {
	case FaultReset, FaultStall, FaultTruncate:
		if !fr.done && fr.n >= f.AtByte {
			fr.done = true
			switch f.Kind {
			case FaultReset:
				return 0, errReset
			case FaultTruncate:
				return 0, errTruncate
			default:
				if err := sleep(fr.ctx, fr.clock, f.Delay); err != nil {
					return 0, err
				}
			}
		}
		if !fr.done {
			p = p[:min(int64(len(p)), f.AtByte-fr.n)]
		}
	case FaultSlow:
		if f.BytesPerSec > 0 {
			p = p[:min(len(p), 32*1024)]
		}
	}
	n, err := fr.r.Read(p)
	fr.n += int64(n)
	if f.Kind == FaultSlow && f.BytesPerSec > 0 && n > 0 {
		pause := time.Duration(int64(n) * int64(time.Second) / f.BytesPerSec)
		if serr := sleep(fr.ctx, fr.clock, pause); serr != nil && err == nil {
			err = serr
		}
	}
	return n, err
}

// sleep waits for d on clock. d <= 0 waits until ctx ends.
func sleep(ctx context.Context, clock engine.Clock, d time.Duration) error {
	if d <= 0 {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	timer := clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C():
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// ruleSet holds installed rules and attempt counters.
type ruleSet struct {
	mu       sync.Mutex
	next     RuleID
	rules    []*installedRule
	attempts map[attemptKey]int
}

type attemptKey struct {
	op     Op
	bucket string
	key    string
	part   int32
}

func (rs *ruleSet) add(r Rule) RuleID {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.next++
	rs.rules = append(rs.rules, &installedRule{id: rs.next, rule: r})
	return rs.next
}

func (rs *ruleSet) remove(id RuleID) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for i, r := range rs.rules {
		if r.id == id {
			rs.rules = append(rs.rules[:i], rs.rules[i+1:]...)
			return
		}
	}
}

func (rs *ruleSet) clear() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.rules = nil
}

func (rs *ruleSet) fired(id RuleID) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, r := range rs.rules {
		if r.id == id {
			return r.fired
		}
	}
	return 0
}

// match counts the attempt and returns the first rule that applies.
func (rs *ruleSet) match(info *reqInfo) (attempt int, f *Fault) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.attempts == nil {
		rs.attempts = make(map[attemptKey]int)
	}
	k := attemptKey{info.op, info.bucket, info.key, info.part}
	rs.attempts[k]++
	attempt = rs.attempts[k]
	for _, r := range rs.rules {
		if r.matches(info, attempt) {
			r.fired++
			fault := r.rule.Fault
			return attempt, &fault
		}
	}
	return attempt, nil
}

func (rs *ruleSet) attemptsOf(op Op, bucket, key string, part int32) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.attempts[attemptKey{op, bucket, key, part}]
}
