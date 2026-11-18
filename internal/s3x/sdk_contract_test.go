package s3x

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"s4browser/internal/engine"
)

// These tests talk only to a local httptest server with fake static keys.

const partSize = 3 * 1024 * 1024 // Above the SDK's 2 MiB Expect: 100-continue threshold.

type capturedRequest struct {
	header http.Header
	query  string
	body   []byte
}

type recorder struct {
	mu       sync.Mutex
	requests []capturedRequest
	respond  func(w http.ResponseWriter, r *http.Request, body []byte)
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	rec.requests = append(rec.requests, capturedRequest{r.Header.Clone(), r.URL.RawQuery, body})
	rec.mu.Unlock()
	rec.respond(w, r, body)
}

func (rec *recorder) only(t *testing.T) capturedRequest {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(rec.requests))
	}
	return rec.requests[0]
}

func newClient(server *httptest.Server, provider aws.CredentialsProvider) *s3.Client {
	return s3.New(s3.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(server.URL),
		UsePathStyle:     true,
		HTTPClient:       server.Client(),
		Credentials:      provider,
		RetryMaxAttempts: 5, // Proves SingleAttemptOptions overrides a retrying client.
	})
}

func fakeKeys() aws.CredentialsProvider {
	return credentials.NewStaticCredentialsProvider("TESTKEYID", "test-not-a-secret", "")
}

// countingReaderAt counts the bytes read from the source, like a file on disk.
type countingReaderAt struct {
	data []byte
	read atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := bytes.NewReader(c.data).ReadAt(p, off)
	c.read.Add(int64(n))
	return n, err
}

func crc32Base64(data []byte) string {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], crc32.ChecksumIEEE(data))
	return base64.StdEncoding.EncodeToString(raw[:])
}

func uploadPart(ctx context.Context, client *s3.Client, source *countingReaderAt) (*s3.UploadPartOutput, error) {
	return client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:            aws.String("bucket"),
		Key:               aws.String("key"),
		UploadId:          aws.String("upload"),
		PartNumber:        aws.Int32(1),
		Body:              io.NewSectionReader(source, 0, int64(len(source.data))),
		ContentLength:     aws.Int64(int64(len(source.data))),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	}, SingleAttemptOptions)
}

func TestSingleAttemptOptionsDisableRetries(t *testing.T) {
	rec := &recorder{respond: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `<Error><Code>SlowDown</Code><Message>slow</Message></Error>`)
	}}
	server := httptest.NewTLSServer(rec)
	defer server.Close()

	source := &countingReaderAt{data: make([]byte, 1024)}
	_, err := uploadPart(context.Background(), newClient(server, fakeKeys()), source)
	if err == nil {
		t.Fatal("expected an error")
	}
	rec.only(t)

	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "SlowDown" {
		t.Fatalf("no API error code in %v", err)
	}
	var respErr *awshttp.ResponseError
	if !errors.As(err, &respErr) || respErr.HTTPStatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("no HTTP status in %v", err)
	}
}

// TestHTTPSUploadReadsSourceOnceWithTrailingCRC32 pins the production path. The
// body is streamed once, and memory does not scale with the part size.
func TestHTTPSUploadReadsSourceOnceWithTrailingCRC32(t *testing.T) {
	data := bytes.Repeat([]byte("s4browser"), partSize/9+1)[:partSize]
	want := crc32Base64(data)

	rec := &recorder{respond: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("ETag", `"part-etag"`)
		w.Header().Set("x-amz-checksum-crc32", want)
	}}
	server := httptest.NewTLSServer(rec)
	defer server.Close()

	source := &countingReaderAt{data: data}
	out, err := uploadPart(context.Background(), newClient(server, fakeKeys()), source)
	if err != nil {
		t.Fatal(err)
	}
	if got := source.read.Load(); got != partSize {
		t.Errorf("source read %d bytes, want exactly %d", got, partSize)
	}
	if aws.ToString(out.ChecksumCRC32) != want {
		t.Errorf("response checksum %q", aws.ToString(out.ChecksumCRC32))
	}

	req := rec.only(t)
	for name, value := range map[string]string{
		"X-Amz-Content-Sha256":         "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"X-Amz-Trailer":                "x-amz-checksum-crc32",
		"Content-Encoding":             "aws-chunked",
		"X-Amz-Decoded-Content-Length": "3145728",
	} {
		if got := req.header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if !bytes.Contains(req.body, data) {
		t.Error("part data is not one contiguous aws-chunked chunk")
	}
	if !strings.Contains(string(req.body), "x-amz-checksum-crc32:"+want) {
		t.Errorf("trailer missing from body tail %q", req.body[len(req.body)-64:])
	}
}

// TestPlainHTTPUploadReadsSourceTwice pins why the fake S3 must serve TLS: over
// plain HTTP the SDK pre-reads the body to hash it, then rewinds and sends it.
func TestPlainHTTPUploadReadsSourceTwice(t *testing.T) {
	rec := &recorder{respond: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("ETag", `"part-etag"`)
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	source := &countingReaderAt{data: make([]byte, partSize)}
	if _, err := uploadPart(context.Background(), newClient(server, fakeKeys()), source); err != nil {
		t.Fatal(err)
	}
	if got := source.read.Load(); got != 2*partSize {
		t.Errorf("source read %d bytes, want %d", got, 2*partSize)
	}
	req := rec.only(t)
	if req.header.Get("X-Amz-Trailer") != "" || req.header.Get("X-Amz-Checksum-Crc32") == "" {
		t.Errorf("expected a header checksum, got %v", req.header)
	}
}

func TestRangedGetHeaders(t *testing.T) {
	rec := &recorder{respond: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Range", "bytes 10-13/100")
		w.Header().Set("ETag", `"object-etag"`)
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "data")
	}}
	server := httptest.NewTLSServer(rec)
	defer server.Close()

	out, err := newClient(server, fakeKeys()).GetObject(context.Background(), &s3.GetObjectInput{
		Bucket:  aws.String("bucket"),
		Key:     aws.String("key"),
		Range:   aws.String("bytes=10-13"),
		IfMatch: aws.String(`"object-etag"`),
	}, SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if body, _ := io.ReadAll(out.Body); string(body) != "data" {
		t.Fatalf("body %q", body)
	}
	if aws.ToString(out.ContentRange) != "bytes 10-13/100" {
		t.Errorf("Content-Range %q", aws.ToString(out.ContentRange))
	}

	req := rec.only(t)
	if got := req.header.Get("If-Match"); got != `"object-etag"` {
		t.Errorf("If-Match = %q", got)
	}
	if got := req.header.Get("Range"); got != "bytes=10-13" {
		t.Errorf("Range = %q", got)
	}
	if got := req.header.Get("X-Amz-Checksum-Mode"); got != "" {
		t.Errorf("X-Amz-Checksum-Mode = %q, want unset", got)
	}
}

func TestPreconditionFailedIsVisible(t *testing.T) {
	rec := &recorder{respond: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusPreconditionFailed)
		io.WriteString(w, `<Error><Code>PreconditionFailed</Code><Message>no</Message></Error>`)
	}}
	server := httptest.NewTLSServer(rec)
	defer server.Close()

	_, err := newClient(server, fakeKeys()).GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("bucket"), Key: aws.String("key"),
		Range: aws.String("bytes=0-1"), IfMatch: aws.String(`"old"`),
	}, SingleAttemptOptions)

	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "PreconditionFailed" {
		t.Fatalf("got %v", err)
	}
	rec.only(t)
}

// headerCapture records outgoing headers, because the Go server hides Expect.
type headerCapture struct {
	next   http.RoundTripper
	expect atomic.Value
}

func (c *headerCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.expect.Store(r.Header.Get("Expect"))
	return c.next.RoundTrip(r)
}

func TestLargeBodiesSendExpectContinue(t *testing.T) {
	rec := &recorder{respond: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("ETag", `"part-etag"`)
	}}
	server := httptest.NewTLSServer(rec)
	defer server.Close()

	capture := &headerCapture{next: server.Client().Transport}
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(server.URL),
		UsePathStyle: true,
		HTTPClient:   &http.Client{Transport: capture},
		Credentials:  fakeKeys(),
	})
	source := &countingReaderAt{data: make([]byte, partSize)}
	if _, err := uploadPart(context.Background(), client, source); err != nil {
		t.Fatal(err)
	}
	if got := capture.expect.Load(); got != "100-continue" {
		t.Fatalf("Expect = %q", got)
	}
}

func TestCompleteErrorInside200IsAnAPIError(t *testing.T) {
	rec := &recorder{respond: func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>InternalError</Code><Message>retry</Message></Error>`)
	}}
	server := httptest.NewTLSServer(rec)
	defer server.Close()

	_, err := newClient(server, fakeKeys()).CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String("bucket"),
		Key:      aws.String("key"),
		UploadId: aws.String("upload"),
	}, SingleAttemptOptions)

	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InternalError" {
		t.Fatalf("got %v", err)
	}
	rec.only(t)
	if class, _ := engine.ClassifyResponse(http.StatusOK, apiErr.ErrorCode()); class != engine.ClassRetryable {
		t.Fatalf("classified as %v", class)
	}
}

type failingProvider struct{ err error }

func (p failingProvider) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{}, p.err
}

func TestProviderFailureSurvivesSDKWrapping(t *testing.T) {
	rec := &recorder{respond: func(http.ResponseWriter, *http.Request, []byte) {}}
	server := httptest.NewTLSServer(rec)
	defer server.Close()

	cause := errors.New("helper exited with status 1")
	provider := aws.NewCredentialsCache(MarkCredentialErrors(failingProvider{cause}))
	source := &countingReaderAt{data: make([]byte, 1024)}

	_, err := uploadPart(context.Background(), newClient(server, provider), source)
	if !errors.Is(err, engine.ErrCredentialsUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("got %v", err)
	}
	if got := engine.Classify(err); got != engine.ClassCredential {
		t.Fatalf("classified as %v", got)
	}
	if len(rec.requests) != 0 {
		t.Fatalf("server saw %d requests", len(rec.requests))
	}
}

func TestProviderCancellationIsNotACredentialFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := MarkCredentialErrors(failingProvider{context.Canceled}).Retrieve(ctx)
	if errors.Is(err, engine.ErrCredentialsUnavailable) {
		t.Fatalf("cancellation marked as credential failure: %v", err)
	}
}
