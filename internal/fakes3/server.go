package fakes3

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"s4browser/internal/engine"
)

// Credentials the fake accepts. It does not verify signatures, but it records
// the access key ID of each request, and rules can reject any of them.
const (
	AccessKeyID     = "AKIAFAKES3TESTKEY00"
	SecretAccessKey = "fakes3-test-secret-not-real"
	DefaultBucket   = "test-bucket"
	Region          = "us-east-1"
)

// Options configures a Server.
type Options struct {
	// Buckets that exist. Defaults to DefaultBucket.
	Buckets []string
	// Guard, when set, makes the server refuse every mutating request whose key
	// it rejects, with 403 AccessDenied and no effect.
	Guard engine.WriteGuard
	// Clock supplies timestamps and fault delays. Defaults to real time.
	Clock engine.Clock
}

// Server is an HTTPS S3 endpoint on loopback plus an in-process ObjectStore
// that share one set of objects, uploads, rules and request log.
type Server struct {
	core     *core
	listener net.Listener
	http     *http.Server
	client   *http.Client
	certPEM  []byte
	url      string

	ctx    context.Context
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup

	reqMu sync.Mutex
	reqID int
}

type realClock struct{}

func (realClock) Now() time.Time                          { return time.Now() }
func (realClock) NewTimer(d time.Duration) engine.Timer   { return realTimer{time.NewTimer(d)} }
func (realClock) NewTicker(d time.Duration) engine.Ticker { return realTicker{time.NewTicker(d)} }

type realTimer struct{ *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.Timer.C }

type realTicker struct{ *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.Ticker.C }

// New starts a server. Call Close when done.
func New(opts Options) (*Server, error) {
	if len(opts.Buckets) == 0 {
		opts.Buckets = []string{DefaultBucket}
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	cert, certPEM, err := generateCert()
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"http/1.1"}, // S3 clients use HTTP/1.1.
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	s := &Server{
		core:     newCore(opts.Clock, opts.Guard, opts.Buckets),
		listener: listener,
		certPEM:  certPEM,
		url:      "https://" + listener.Addr().String(),
	}
	s.ctx, s.cancel = context.WithCancelCause(context.Background())
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		MaxIdleConns:          engine.MaxActivePartsLimit,
		MaxIdleConnsPerHost:   engine.MaxActivePartsLimit,
		ExpectContinueTimeout: time.Second,
		WriteBufferSize:       64 * 1024,
		ReadBufferSize:        64 * 1024,
	}
	s.client = &http.Client{Transport: transport}
	s.http = &http.Server{
		Handler:           http.HandlerFunc(s.serveHTTP),
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){}, // No HTTP/2.
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = s.http.Serve(listener)
	}()
	return s, nil
}

// Close stops the server, ends stalled requests and closes all connections.
func (s *Server) Close() {
	s.cancel(errors.New("fakes3: server closed"))
	_ = s.http.Close()
	s.client.CloseIdleConnections()
	s.wg.Wait()
}

// Endpoint is the base URL, "https://127.0.0.1:<port>". Use path-style addressing.
func (s *Server) Endpoint() string { return s.url }

// HTTPClient returns a client that trusts the server's generated certificate.
func (s *Server) HTTPClient() *http.Client { return s.client }

// CertPEM is the generated self-signed certificate, valid for 127.0.0.1.
func (s *Server) CertPEM() []byte { return s.certPEM }

func generateCert() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "s4browser fakes3 test certificate"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return cert, certPEM, nil
}

// ---- HTTP handler ----

func (s *Server) nextRequestID() string {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	s.reqID++
	return fmt.Sprintf("FAKE%012X", s.reqID)
}

func accessKeyOf(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	_, rest, ok := strings.Cut(auth, "Credential=")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	stop := context.AfterFunc(s.ctx, func() { cancel(context.Cause(s.ctx)) })
	defer stop()

	reqID := s.nextRequestID()
	h := w.Header()
	h.Set("x-amz-request-id", reqID)
	h.Set("x-amz-id-2", "fakes3/"+reqID)
	h.Set("Server", "AmazonS3")

	path := strings.TrimPrefix(r.URL.Path, "/")
	bucketName, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()
	_, hasUploads := q["uploads"]
	uploadID := q.Get("uploadId")
	info := reqInfo{bucket: bucketName, key: key, uploadID: uploadID, rangeStart: -1, accessKey: accessKeyOf(r)}
	if pn := q.Get("partNumber"); pn != "" {
		n, err := strconv.ParseInt(pn, 10, 32)
		if err != nil {
			s.writeError(w, r, s3err(http.StatusBadRequest, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive"))
			return
		}
		info.part = int32(n)
	}

	var err error
	switch {
	case key == "":
		err = s.serveBucket(ctx, w, r, info, q, hasUploads)
	case r.Method == http.MethodPut && uploadID != "" && q.Get("partNumber") != "":
		err = s.serveUploadPart(ctx, w, r, info)
	case r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		err = s3err(http.StatusNotImplemented, "NotImplemented", "fakes3 does not implement CopyObject")
	case r.Method == http.MethodPut:
		err = s.servePut(ctx, w, r, info)
	case r.Method == http.MethodPost && hasUploads:
		err = s.serveCreate(ctx, w, r, info)
	case r.Method == http.MethodPost && uploadID != "":
		err = s.serveComplete(ctx, w, r, info)
	case r.Method == http.MethodDelete && uploadID != "":
		err = s.core.abortUpload(ctx, info)
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodDelete:
		err = s.core.deleteObject(ctx, info)
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodGet && uploadID != "":
		err = s.serveListParts(ctx, w, q, info)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		err = s.serveGet(ctx, w, r, info)
	default:
		err = s3err(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
	}
	s.finish(w, r, err)
}

// finish turns an error from the core into the wire behaviour.
func (s *Server) finish(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		s.writeError(w, r, ae)
	case errors.Is(err, errReset):
		s.reset(w)
	case errors.Is(err, errTruncate):
		s.closeCleanly(w)
	case errors.Is(err, context.Canceled), s.ctx.Err() != nil:
		s.reset(w)
	default:
		// A body read failed: the client went away or sent malformed framing.
		s.reset(w)
	}
}

func (s *Server) reset(w http.ResponseWriter) {
	conn := hijack(w)
	if conn == nil {
		panic(http.ErrAbortHandler)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0) // Close sends RST instead of FIN.
	}
	_ = conn.Close()
}

func (s *Server) closeCleanly(w http.ResponseWriter) {
	if conn := hijack(w); conn != nil {
		_ = conn.Close()
	}
}

// hijack takes over the connection and returns the raw TCP connection beneath TLS.
func hijack(w http.ResponseWriter) net.Conn {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return nil
	}
	if tc, ok := conn.(*tls.Conn); ok {
		return tc.NetConn()
	}
	return conn
}

type xmlError struct {
	XMLName   struct{} `xml:"Error"`
	Code      string
	Message   string
	Resource  string `xml:",omitempty"`
	RequestID string `xml:"RequestId"`
	HostID    string `xml:"HostId"`
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, e *apiError) {
	h := w.Header()
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(e.RetryAfter.Round(time.Second)/time.Second)))
	}
	if r.Method == http.MethodHead || e.Status == http.StatusNotModified {
		w.WriteHeader(e.Status)
		return
	}
	body := xmlError{Code: e.Code, Message: e.Message, Resource: r.URL.Path, RequestID: h.Get("x-amz-request-id"), HostID: h.Get("x-amz-id-2")}
	status := e.Status
	if e.In200 {
		status = http.StatusOK
	}
	writeXML(w, status, body)
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xmlHeader)
	enc := xmlEncoder(w)
	_ = enc.Encode(v)
}

// ---- upload bodies ----

// openBody returns the decoded request body, its decoded length (-1 if
// unknown), a function giving the client's declared CRC32 once the body has
// been read, and whether a CRC32 was announced.
func openBody(r *http.Request) (body io.Reader, length int64, declared func() string, announced bool) {
	trailerNames := strings.ToLower(r.Header.Get("x-amz-trailer"))
	announced = r.Header.Get("x-amz-checksum-crc32") != "" ||
		strings.Contains(trailerNames, "x-amz-checksum-crc32") ||
		strings.EqualFold(r.Header.Get("x-amz-sdk-checksum-algorithm"), "CRC32")

	chunked := strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "aws-chunked") ||
		strings.HasPrefix(r.Header.Get("x-amz-content-sha256"), "STREAMING-")
	if !chunked {
		return r.Body, r.ContentLength, func() string { return r.Header.Get("x-amz-checksum-crc32") }, announced
	}
	length = -1
	if v := r.Header.Get("x-amz-decoded-content-length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			length = n
		}
	}
	dec := &chunkDecoder{br: bufio.NewReaderSize(r.Body, 64*1024)}
	return dec, length, func() string {
		if v := dec.trailers["x-amz-checksum-crc32"]; v != "" {
			return v
		}
		return r.Header.Get("x-amz-checksum-crc32")
	}, announced
}

// chunkDecoder decodes the aws-chunked content encoding: hex-size lines with an
// optional ";chunk-signature=..." extension, then a zero chunk and trailers.
type chunkDecoder struct {
	br        *bufio.Reader
	remaining int64
	finished  bool
	trailers  map[string]string
}

func badChunk(format string, args ...any) error {
	return s3err(http.StatusBadRequest, "InvalidRequest", "malformed aws-chunked body: "+fmt.Sprintf(format, args...))
}

func (d *chunkDecoder) line() (string, error) {
	line, err := d.br.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (d *chunkDecoder) Read(p []byte) (int, error) {
	if d.finished {
		return 0, io.EOF
	}
	for d.remaining == 0 {
		line, err := d.line()
		if err != nil {
			return 0, err
		}
		sizeText, _, _ := strings.Cut(line, ";")
		size, perr := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if perr != nil || size < 0 {
			return 0, badChunk("bad chunk size %q", line)
		}
		if size == 0 {
			d.trailers = make(map[string]string)
			for {
				tl, err := d.line()
				if err != nil {
					return 0, err
				}
				if tl == "" {
					break
				}
				name, value, ok := strings.Cut(tl, ":")
				if !ok {
					return 0, badChunk("bad trailer %q", tl)
				}
				d.trailers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
			}
			d.finished = true
			return 0, io.EOF
		}
		d.remaining = size
	}
	n, err := d.br.Read(p[:min(int64(len(p)), d.remaining)])
	d.remaining -= int64(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	if d.remaining == 0 && err == nil {
		if crlf, cerr := d.line(); cerr != nil || crlf != "" {
			if cerr == nil {
				cerr = badChunk("missing chunk terminator")
			}
			return n, cerr
		}
	}
	return n, err
}

func (s *Server) serveUploadPart(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo) error {
	body, length, declared, announced := openBody(r)
	out, err := s.core.uploadPart(ctx, partIn{info: info, body: body, contentLength: length, declared: declared, hasChecksum: announced})
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("ETag", out.etag)
	h.Set("x-amz-checksum-crc32", out.crc)
	h.Set("x-amz-checksum-type", "FULL_OBJECT")
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) servePut(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo) error {
	body, length, declared, _ := openBody(r)
	out, err := s.core.putObject(ctx, putIn{info: info, body: body, contentLength: length, declared: declared})
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("ETag", out.etag)
	h.Set("x-amz-checksum-crc32", out.crc)
	h.Set("x-amz-checksum-type", "FULL_OBJECT")
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) serveCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo) error {
	algo := strings.ToUpper(r.Header.Get("x-amz-checksum-algorithm"))
	if algo != "" && algo != "CRC32" {
		return s3err(http.StatusBadRequest, "InvalidRequest", "fakes3 supports only the CRC32 checksum algorithm")
	}
	if t := strings.ToUpper(r.Header.Get("x-amz-checksum-type")); t != "" && t != "COMPOSITE" {
		return s3err(http.StatusBadRequest, "InvalidRequest", "fakes3 supports only the COMPOSITE checksum type for multipart uploads")
	}
	id, err := s.core.createUpload(ctx, createIn{info: info, crcRequired: algo == "CRC32"})
	if err != nil {
		return err
	}
	if algo == "CRC32" {
		w.Header().Set("x-amz-checksum-algorithm", "CRC32")
		w.Header().Set("x-amz-checksum-type", "COMPOSITE")
	}
	writeXML(w, http.StatusOK, newInitiate(info.bucket, info.key, id))
	return nil
}

func (s *Server) serveComplete(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo) error {
	req, err := decodeCompleteRequest(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return s3err(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
	}
	size := int64(-1)
	if v := r.Header.Get("x-amz-mp-object-size"); v != "" {
		if size, err = strconv.ParseInt(v, 10, 64); err != nil {
			return s3err(http.StatusBadRequest, "InvalidArgument", "x-amz-mp-object-size must be an integer")
		}
	}
	out, err := s.core.completeUpload(ctx, completeIn{info: info, parts: req, objectSize: size})
	if err != nil {
		return err
	}
	res := completeResult{XMLNS: s3NS, Location: fmt.Sprintf("%s/%s/%s", s.url, info.bucket, info.key), Bucket: info.bucket, Key: info.key, ETag: out.etag, ChecksumCRC32: out.crc}
	if out.crc != "" {
		res.ChecksumType = "COMPOSITE"
	}
	writeXML(w, http.StatusOK, res)
	return nil
}

func (s *Server) serveListParts(ctx context.Context, w http.ResponseWriter, q map[string][]string, info reqInfo) error {
	in := listPartsIn{info: info, marker: int32(atoi(first(q["part-number-marker"]))), max: atoi(first(q["max-parts"]))}
	out, err := s.core.listParts(ctx, in)
	if err != nil {
		return err
	}
	writeXML(w, http.StatusOK, buildListParts(info, in, out))
	return nil
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (s *Server) serveBucket(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo, q map[string][]string, hasUploads bool) error {
	switch {
	case r.Method == http.MethodHead:
		s.core.mu.Lock()
		_, e := s.core.bucketLocked(info.bucket)
		s.core.mu.Unlock()
		if e != nil {
			return e
		}
		w.Header().Set("x-amz-bucket-region", Region)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && hasUploads:
		in := listUploadsIn{info: info, prefix: first(q["prefix"]), keyMarker: first(q["key-marker"]), idMarker: first(q["upload-id-marker"]), maxUploads: atoi(first(q["max-uploads"]))}
		out, err := s.core.listUploads(ctx, in)
		if err != nil {
			return err
		}
		writeXML(w, http.StatusOK, buildListUploads(info, in, out))
	case r.Method == http.MethodGet:
		in := listObjectsIn{
			info: info, prefix: first(q["prefix"]), delimiter: first(q["delimiter"]),
			token: first(q["continuation-token"]), startAfter: first(q["start-after"]), maxKeys: atoi(first(q["max-keys"])),
		}
		out, err := s.core.listObjects(ctx, in)
		if err != nil {
			return err
		}
		writeXML(w, http.StatusOK, buildListObjects(info, in, out, first(q["encoding-type"]) == "url"))
	default:
		return s3err(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
	}
	return nil
}

// parseRange parses a single-range Range header. ok is false for headers S3
// ignores, which are served as a full-object response.
func parseRange(header string) (rng *byteRange, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return nil, false
	}
	from, to, found := strings.Cut(spec, "-")
	if !found {
		return nil, false
	}
	switch {
	case from == "":
		n, err := strconv.ParseInt(to, 10, 64)
		if err != nil || n <= 0 {
			return nil, false
		}
		return &byteRange{start: -1, suffix: n}, true
	default:
		start, err := strconv.ParseInt(from, 10, 64)
		if err != nil || start < 0 {
			return nil, false
		}
		end := int64(-1)
		if to != "" {
			if end, err = strconv.ParseInt(to, 10, 64); err != nil || end < start {
				return nil, false
			}
		}
		return &byteRange{start: start, end: end}, true
	}
}

func (s *Server) serveGet(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo) error {
	h := w.Header()
	if r.Method == http.MethodHead {
		m, err := s.core.headObject(ctx, info)
		if err != nil {
			return err
		}
		setObjectHeaders(h, m, r.Header.Get("x-amz-checksum-mode") == "ENABLED")
		h.Set("Content-Length", strconv.FormatInt(m.size, 10))
		w.WriteHeader(http.StatusOK)
		return nil
	}
	in := getIn{info: info, ifMatch: r.Header.Get("If-Match"), ifNone: r.Header.Get("If-None-Match")}
	in.rng, _ = parseRange(r.Header.Get("Range"))
	out, err := s.core.getObject(ctx, in)
	if err != nil {
		return err
	}
	defer out.body.Close()
	setObjectHeaders(h, out.meta, !out.partial && r.Header.Get("x-amz-checksum-mode") == "ENABLED")
	length := out.end - out.start + 1
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	status := http.StatusOK
	if out.partial {
		status = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", out.start, out.end, out.meta.size))
	}
	w.WriteHeader(status)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	// Copy through a Write-only wrapper so the copy cannot bypass faultReader.
	_, err = io.CopyBuffer(writerOnly{w}, readerOnly{out.body}, *bufp)
	if err != nil {
		return err
	}
	return nil
}

type writerOnly struct{ io.Writer }
type readerOnly struct{ io.Reader }

func setObjectHeaders(h http.Header, m objMeta, withChecksum bool) {
	h.Set("ETag", m.etag)
	h.Set("Last-Modified", m.modified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", "binary/octet-stream")
	if withChecksum && m.crc != "" {
		h.Set("x-amz-checksum-crc32", m.crc)
		if m.multipart {
			h.Set("x-amz-checksum-type", "COMPOSITE")
		} else {
			h.Set("x-amz-checksum-type", "FULL_OBJECT")
		}
	}
}

var _ = runtime.NumCPU
