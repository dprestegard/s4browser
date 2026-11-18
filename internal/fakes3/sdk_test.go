package fakes3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"s4browser/internal/engine"
	"s4browser/internal/s3x"
)

const (
	mib     = 1 << 20
	testKey = "s4browser-test/run1/object.bin"
)

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

type sdkUpload struct {
	id    string
	parts []types.CompletedPart
}

// sdkMultipart uploads size bytes of Pattern{seed} through the real SDK client
// with the same options the engine uses: composite CRC32, trailing checksums.
func sdkMultipart(t *testing.T, s *Server, key string, size, partSize int64, seed uint64) (sdkUpload, *s3.CompleteMultipartUploadOutput) {
	t.Helper()
	ctx := context.Background()
	c := s.S3Client()
	src := NewSource()
	src.Set("f", SyntheticFile{Size: size, Seed: seed})
	h, _ := src.Open(ctx, "f")

	cr, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(key),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32, ChecksumType: types.ChecksumTypeComposite,
	}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	up := sdkUpload{id: aws.ToString(cr.UploadId)}
	plan, err := engine.PlanParts(size, partSize)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < plan.PartCount; i++ {
		pr := plan.Part(i)
		out, err := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(DefaultBucket), Key: aws.String(key), UploadId: cr.UploadId,
			PartNumber: aws.Int32(pr.Number), Body: io.NewSectionReader(h, pr.Offset, pr.Length),
			ContentLength: aws.Int64(pr.Length), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
		}, s3x.SingleAttemptOptions)
		if err != nil {
			t.Fatalf("part %d: %v", pr.Number, err)
		}
		up.parts = append(up.parts, types.CompletedPart{PartNumber: aws.Int32(pr.Number), ETag: out.ETag, ChecksumCRC32: out.ChecksumCRC32})
	}
	return up, completeSDK(t, c, key, up, size)
}

func completeSDK(t *testing.T, c *s3.Client, key string, up sdkUpload, size int64) *s3.CompleteMultipartUploadOutput {
	t.Helper()
	out, err := c.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(key), UploadId: aws.String(up.id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: up.parts}, MpuObjectSize: aws.Int64(size),
	}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func getRange(ctx context.Context, c *s3.Client, key string, off, n int64, ifMatch string) (*s3.GetObjectOutput, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(key),
		Range: aws.String("bytes=" + itoa(off) + "-" + itoa(off+n-1)),
	}
	if ifMatch != "" {
		in.IfMatch = aws.String(ifMatch)
	}
	return c.GetObject(ctx, in, s3x.SingleAttemptOptions)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func readAll(t *testing.T, out *s3.GetObjectOutput) []byte {
	t.Helper()
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMultipartRoundTripWithRealSDK(t *testing.T) {
	s := newTestServer(t, Options{})
	const size = 12 * mib
	up, done := sdkMultipart(t, s, testKey, size, 5*mib, 7)

	// The composite CRC32 the server returns is the engine's own formula.
	var crcs []string
	for _, p := range up.parts {
		crcs = append(crcs, aws.ToString(p.ChecksumCRC32))
	}
	want, err := engine.CompositeCRC32(crcs)
	if err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(done.ChecksumCRC32); got != want {
		t.Fatalf("composite CRC32 %q, want %q", got, want)
	}
	if done.ChecksumType != types.ChecksumTypeComposite {
		t.Errorf("checksum type %q", done.ChecksumType)
	}
	if !strings.HasSuffix(strings.Trim(aws.ToString(done.ETag), `"`), "-3") {
		t.Errorf("multipart ETag %q should end in -3", aws.ToString(done.ETag))
	}

	// The server consumed the bodies and kept only metadata.
	st, ok := s.Object(DefaultBucket, testKey)
	if !ok || st.Size != size || len(st.Parts) != 3 || !st.Multipart {
		t.Fatalf("object state %+v", st)
	}
	for i := int32(1); i <= 3; i++ {
		if n := s.Attempts(OpUploadPart, DefaultBucket, testKey, i); n != 1 {
			t.Errorf("part %d sent %d times", i, n)
		}
	}
	var body int64
	for _, r := range s.Requests() {
		if r.Op == OpUploadPart {
			body += r.BodyBytes
		}
	}
	if body != size {
		t.Errorf("server consumed %d part-body bytes, want %d (decoded aws-chunked)", body, size)
	}

	// Content check: right seed passes, wrong seed fails.
	if err := s.VerifyObject(DefaultBucket, testKey, 8, 1); err == nil {
		t.Fatal("wrong seed verified")
	}
	if err := s.VerifyObject(DefaultBucket, testKey, 7, 1); err != nil {
		t.Fatal(err)
	}

	c := s.S3Client()
	head, err := c.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(testKey), ChecksumMode: types.ChecksumModeEnabled,
	}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt64(head.ContentLength) != size || aws.ToString(head.ChecksumCRC32) != want {
		t.Errorf("head: size %d, checksum %q", aws.ToInt64(head.ContentLength), aws.ToString(head.ChecksumCRC32))
	}

	// Ranged GET across a part boundary with If-Match.
	off, n := int64(5*mib-100), int64(300)
	out, err := getRange(context.Background(), c, testKey, off, n, aws.ToString(head.ETag))
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.ContentRange) != "bytes 5242780-5243079/12582912" {
		t.Errorf("Content-Range %q", aws.ToString(out.ContentRange))
	}
	got := readAll(t, out)
	exp := make([]byte, n)
	Pattern{Seed: 7}.Fill(exp, off)
	if !bytes.Equal(got, exp) {
		t.Fatal("ranged GET bytes differ from the pattern")
	}
}

func TestUploadedContentMustMatchBeforeItCanBeRead(t *testing.T) {
	s := newTestServer(t, Options{})
	sdkMultipart(t, s, testKey, 6*mib, 5*mib, 1)
	_, err := getRange(context.Background(), s.S3Client(), testKey, 0, 10, "")
	if apiCode(err) != "InternalError" {
		t.Fatalf("unverified upload served data: %v", err)
	}
}

func TestAbortRemovesUpload(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()
	cr, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(DefaultBucket), Key: aws.String(testKey)}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.ActiveUploads(DefaultBucket)); n != 1 {
		t.Fatalf("%d uploads", n)
	}
	abort := func() error {
		_, err := c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(DefaultBucket), Key: aws.String(testKey), UploadId: cr.UploadId}, s3x.SingleAttemptOptions)
		return err
	}
	if err := abort(); err != nil {
		t.Fatal(err)
	}
	if n := len(s.ActiveUploads(DefaultBucket)); n != 0 {
		t.Fatalf("%d uploads after abort", n)
	}
	if code := apiCode(abort()); code != "NoSuchUpload" {
		t.Fatalf("second abort: %q", code)
	}
	// The engine contract treats NoSuchUpload on abort as success.
	ref := engine.ObjectRef{Bucket: DefaultBucket, Key: testKey}
	if err := s.Store().AbortMultipartUpload(ctx, ref, aws.ToString(cr.UploadId)); err != nil {
		t.Fatalf("store abort of a missing upload: %v", err)
	}
	if _, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String(testKey), UploadId: cr.UploadId, PartNumber: aws.Int32(1),
		Body: bytes.NewReader([]byte("x")), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	}, s3x.SingleAttemptOptions); apiCode(err) != "NoSuchUpload" {
		t.Fatalf("part into aborted upload: %v", err)
	}
}

func TestPutHeadDeleteAndListing(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()
	data := bytes.Repeat([]byte("abc"), 1000)
	put, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String("dir/a b.txt"), Body: bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(put.ChecksumCRC32) == "" || aws.ToString(put.ETag) == "" {
		t.Fatalf("put result %+v", put)
	}
	for _, k := range []string{"dir/b", "dir/sub/c", "dir/sub/d", "top"} {
		if err := s.PutSynthetic(DefaultBucket, k, 1, 10); err != nil {
			t.Fatal(err)
		}
	}
	list := func(in s3.ListObjectsV2Input) *s3.ListObjectsV2Output {
		in.Bucket = aws.String(DefaultBucket)
		out, err := c.ListObjectsV2(ctx, &in, s3x.SingleAttemptOptions)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := list(s3.ListObjectsV2Input{Prefix: aws.String("dir/"), Delimiter: aws.String("/")})
	if len(out.Contents) != 2 || aws.ToString(out.Contents[0].Key) != "dir/a b.txt" || len(out.CommonPrefixes) != 1 || aws.ToString(out.CommonPrefixes[0].Prefix) != "dir/sub/" {
		t.Fatalf("delimited listing: %+v", out)
	}
	// Paging through every key, two at a time, returns each exactly once.
	var all []string
	token := (*string)(nil)
	for {
		page := list(s3.ListObjectsV2Input{MaxKeys: aws.Int32(2), ContinuationToken: token})
		for _, o := range page.Contents {
			all = append(all, aws.ToString(o.Key))
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		token = page.NextContinuationToken
	}
	if strings.Join(all, ",") != "dir/a b.txt,dir/b,dir/sub/c,dir/sub/d,top" {
		t.Fatalf("paged keys %v", all)
	}

	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("top")}, s3x.SingleAttemptOptions); err != nil {
		t.Fatal(err)
	}
	_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("top")}, s3x.SingleAttemptOptions)
	var nf *types.NotFound
	if !errors.As(err, &nf) {
		t.Fatalf("head of a deleted key: %v", err)
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("nope"), Key: aws.String("x")}, s3x.SingleAttemptOptions)
	if apiCode(err) != "NoSuchBucket" {
		t.Fatalf("missing bucket: %v", err)
	}
}

func TestListMultipartUploadsAndParts(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()
	up, _ := sdkMultipart(t, s, "a/done", 6*mib, 5*mib, 3)
	_ = up
	var ids []string
	for _, k := range []string{"a/one", "a/two", "b/other"} {
		cr, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(DefaultBucket), Key: aws.String(k), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32}, s3x.SingleAttemptOptions)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, aws.ToString(cr.UploadId))
	}
	if _, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(DefaultBucket), Key: aws.String("a/one"), UploadId: aws.String(ids[0]), PartNumber: aws.Int32(2),
		Body: bytes.NewReader([]byte("hello")), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	}, s3x.SingleAttemptOptions); err != nil {
		t.Fatal(err)
	}
	lu, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(DefaultBucket), Prefix: aws.String("a/")}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(lu.Uploads) != 2 || aws.ToString(lu.Uploads[0].Key) != "a/one" || aws.ToString(lu.Uploads[1].Key) != "a/two" {
		t.Fatalf("uploads %+v", lu.Uploads)
	}
	if lu.Uploads[0].Initiated == nil || lu.Uploads[0].Initiated.IsZero() {
		t.Error("no Initiated time")
	}
	lp, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(DefaultBucket), Key: aws.String("a/one"), UploadId: aws.String(ids[0])}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(lp.Parts) != 1 || aws.ToInt32(lp.Parts[0].PartNumber) != 2 || aws.ToInt64(lp.Parts[0].Size) != 5 {
		t.Fatalf("parts %+v", lp.Parts)
	}
}

func TestCompleteEnforcesS3Rules(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()

	newUpload := func(key string, sizes ...int) (sdkUpload, []int) {
		cr, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(DefaultBucket), Key: aws.String(key),
			ChecksumAlgorithm: types.ChecksumAlgorithmCrc32, ChecksumType: types.ChecksumTypeComposite,
		}, s3x.SingleAttemptOptions)
		if err != nil {
			t.Fatal(err)
		}
		up := sdkUpload{id: aws.ToString(cr.UploadId)}
		for i, n := range sizes {
			out, err := c.UploadPart(ctx, &s3.UploadPartInput{
				Bucket: aws.String(DefaultBucket), Key: aws.String(key), UploadId: cr.UploadId, PartNumber: aws.Int32(int32(i + 1)),
				Body: io.NewSectionReader(zeroAt{}, 0, int64(n)), ContentLength: aws.Int64(int64(n)), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
			}, s3x.SingleAttemptOptions)
			if err != nil {
				t.Fatal(err)
			}
			up.parts = append(up.parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag, ChecksumCRC32: out.ChecksumCRC32})
		}
		return up, sizes
	}
	complete := func(key string, up sdkUpload, parts []types.CompletedPart, objSize int64) error {
		in := &s3.CompleteMultipartUploadInput{
			Bucket: aws.String(DefaultBucket), Key: aws.String(key), UploadId: aws.String(up.id),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		}
		if objSize >= 0 {
			in.MpuObjectSize = aws.Int64(objSize)
		}
		_, err := c.CompleteMultipartUpload(ctx, in, s3x.SingleAttemptOptions)
		return err
	}

	t.Run("a small part before the last", func(t *testing.T) {
		up, _ := newUpload("k/small", 1*mib, 1*mib)
		if code := apiCode(complete("k/small", up, up.parts, -1)); code != "EntityTooSmall" {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("a small last part is fine", func(t *testing.T) {
		up, _ := newUpload("k/lastsmall", 5*mib, 1)
		if err := complete("k/lastsmall", up, up.parts, 5*mib+1); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("parts out of order", func(t *testing.T) {
		up, _ := newUpload("k/order", 5*mib, 5*mib)
		if code := apiCode(complete("k/order", up, []types.CompletedPart{up.parts[1], up.parts[0]}, -1)); code != "InvalidPartOrder" {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("wrong ETag", func(t *testing.T) {
		up, _ := newUpload("k/etag", 5*mib, 1)
		bad := append([]types.CompletedPart(nil), up.parts...)
		bad[0].ETag = aws.String(`"00000000000000000000000000000000"`)
		if code := apiCode(complete("k/etag", up, bad, -1)); code != "InvalidPart" {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("missing part", func(t *testing.T) {
		up, _ := newUpload("k/missing", 5*mib, 1)
		bad := append([]types.CompletedPart(nil), up.parts...)
		bad[1].PartNumber = aws.Int32(9)
		if code := apiCode(complete("k/missing", up, bad, -1)); code != "InvalidPart" {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("object size mismatch", func(t *testing.T) {
		up, _ := newUpload("k/size", 5*mib, 1)
		if code := apiCode(complete("k/size", up, up.parts, 123)); code != "InvalidRequest" {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("missing part checksum", func(t *testing.T) {
		up, _ := newUpload("k/nocrc", 5*mib, 1)
		bad := append([]types.CompletedPart(nil), up.parts...)
		bad[0].ChecksumCRC32 = nil
		if code := apiCode(complete("k/nocrc", up, bad, -1)); code != "InvalidRequest" {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("no parts", func(t *testing.T) {
		up, _ := newUpload("k/none")
		if code := apiCode(complete("k/none", up, nil, -1)); code != "MalformedXML" {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("part number out of range", func(t *testing.T) {
		up, _ := newUpload("k/range", 1)
		for _, n := range []int32{0, 10001} {
			_, err := c.UploadPart(ctx, &s3.UploadPartInput{
				Bucket: aws.String(DefaultBucket), Key: aws.String("k/range"), UploadId: aws.String(up.id), PartNumber: aws.Int32(n),
				Body: bytes.NewReader([]byte("x")), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
			}, s3x.SingleAttemptOptions)
			if apiCode(err) != "InvalidArgument" {
				t.Fatalf("part %d: %v", n, err)
			}
		}
	})
	t.Run("a second complete finds NoSuchUpload", func(t *testing.T) {
		up, _ := newUpload("k/twice", 1)
		if err := complete("k/twice", up, up.parts, 1); err != nil {
			t.Fatal(err)
		}
		if code := apiCode(complete("k/twice", up, up.parts, 1)); code != "NoSuchUpload" {
			t.Fatalf("code %q", code)
		}
	})
}

// zeroAt is an io.ReaderAt of zero bytes, for cheap fixed-size parts.
type zeroAt struct{}

func (zeroAt) ReadAt(p []byte, _ int64) (int, error) { clear(p); return len(p), nil }

func TestServerRejectsBadChecksumsAndFraming(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()
	cr, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(DefaultBucket), Key: aws.String(testKey), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, target string, header map[string]string, body string) (*http.Response, string) {
		req, _ := http.NewRequestWithContext(ctx, method, s.Endpoint()+target, strings.NewReader(body))
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := s.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	target := "/" + DefaultBucket + "/" + testKey + "?partNumber=1&uploadId=" + aws.ToString(cr.UploadId)

	resp, body := do(http.MethodPut, target, map[string]string{"x-amz-checksum-crc32": "AAAAAA=="}, "hello")
	if resp.StatusCode != 400 || !strings.Contains(body, "<Code>BadDigest</Code>") {
		t.Fatalf("header mismatch: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "<RequestId>") || resp.Header.Get("x-amz-request-id") == "" || resp.Header.Get("Content-Type") != "application/xml" {
		t.Errorf("error envelope incomplete: %v %s", resp.Header, body)
	}

	// A hand-built aws-chunked body whose trailer is wrong.
	chunked := "5\r\nhello\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	resp, body = do(http.MethodPut, target, map[string]string{
		"Content-Encoding": "aws-chunked", "x-amz-content-sha256": "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"x-amz-trailer": "x-amz-checksum-crc32", "x-amz-decoded-content-length": "5",
	}, chunked)
	if resp.StatusCode != 400 || !strings.Contains(body, "BadDigest") {
		t.Fatalf("trailer mismatch: %d %s", resp.StatusCode, body)
	}
	// Decoded length that disagrees with the header.
	resp, body = do(http.MethodPut, target, map[string]string{
		"Content-Encoding": "aws-chunked", "x-amz-content-sha256": "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"x-amz-trailer": "x-amz-checksum-crc32", "x-amz-decoded-content-length": "9",
	}, chunked)
	if resp.StatusCode != 400 || !strings.Contains(body, "IncompleteBody") {
		t.Fatalf("length mismatch: %d %s", resp.StatusCode, body)
	}
	// No checksum on an upload that was created with CRC32.
	resp, body = do(http.MethodPut, target, nil, "hello")
	if resp.StatusCode != 400 || !strings.Contains(body, "InvalidRequest") {
		t.Fatalf("missing checksum: %d %s", resp.StatusCode, body)
	}
	if n := len(s.ActiveUploads(DefaultBucket)[0].Parts); n != 0 {
		t.Fatalf("rejected parts were recorded: %d", n)
	}
}

func TestRangedGetSemantics(t *testing.T) {
	s := newTestServer(t, Options{})
	c := s.S3Client()
	ctx := context.Background()
	if err := s.PutSynthetic(DefaultBucket, "obj", 5, 1000); err != nil {
		t.Fatal(err)
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("obj")}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	etag := aws.ToString(head.ETag)

	_, err = getRange(ctx, c, "obj", 0, 10, `"deadbeef"`)
	if apiCode(err) != "PreconditionFailed" {
		t.Fatalf("stale If-Match: %v", err)
	}
	_, err = getRange(ctx, c, "obj", 1000, 10, etag)
	if apiCode(err) != "InvalidRange" {
		t.Fatalf("range past the end: %v", err)
	}
	out, err := getRange(ctx, c, "obj", 990, 100, etag) // clipped to the object
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.ContentRange) != "bytes 990-999/1000" || len(readAll(t, out)) != 10 {
		t.Errorf("clipped range: %q", aws.ToString(out.ContentRange))
	}
	suffix, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("obj"), Range: aws.String("bytes=-5")}, s3x.SingleAttemptOptions)
	if err != nil {
		t.Fatal(err)
	}
	exp := make([]byte, 5)
	Pattern{Seed: 5}.Fill(exp, 995)
	if !bytes.Equal(readAll(t, suffix), exp) {
		t.Error("suffix range bytes differ")
	}
	whole, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("obj")}, s3x.SingleAttemptOptions)
	if err != nil || aws.ToInt64(whole.ContentLength) != 1000 || len(readAll(t, whole)) != 1000 {
		t.Fatalf("whole object: %v", err)
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(DefaultBucket), Key: aws.String("nope")}, s3x.SingleAttemptOptions)
	var nsk *types.NoSuchKey
	if !errors.As(err, &nsk) {
		t.Fatalf("missing key: %v", err)
	}
}

func TestTLSIsRequired(t *testing.T) {
	s := newTestServer(t, Options{})
	plain := strings.Replace(s.Endpoint(), "https://", "http://", 1)
	resp, err := http.Get(plain + "/" + DefaultBucket + "/x")
	if err == nil {
		// Go's server answers a plain request on a TLS port with a 400 hint.
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("plain HTTP got %d", resp.StatusCode)
		}
	}
	if !strings.HasPrefix(s.Endpoint(), "https://127.0.0.1:") {
		t.Fatalf("endpoint %q", s.Endpoint())
	}
	// A client that does not trust the generated certificate must fail.
	_, err = http.Get(s.Endpoint() + "/" + DefaultBucket + "/x")
	if err == nil {
		t.Fatal("default client trusted the test certificate")
	}
}
