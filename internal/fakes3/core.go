package fakes3

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"s4browser/internal/engine"
)

// Metadata-only object model. No object data is ever stored: an uploaded part
// is recorded as its length, CRC32 and ETag, and a synthetic object's bytes are
// generated from its Pattern on demand.

type partMeta struct {
	size int64
	crc  uint32
	etag string // hex, unquoted
}

type object struct {
	size      int64
	etag      string // unquoted
	parts     []partMeta
	multipart bool
	// crc is the base64 checksum S3 reports: the composite value for multipart
	// objects, the plain CRC32 otherwise. Empty when unknown.
	crc       string
	modified  time.Time
	seed      uint64
	seedKnown bool
	changes   int
}

type upload struct {
	id          string
	key         string
	seq         int
	created     time.Time
	crcRequired bool
	parts       map[int32]*uploadedPart
}

type uploadedPart struct {
	partMeta
	modified time.Time
}

type bucket struct {
	region  string
	created time.Time
	objects map[string]*object
	uploads map[string]*upload
	sorted  []string
	dirty   bool
}

// reqInfo identifies one request for rules and the log.
type reqInfo struct {
	op         Op
	bucket     string
	key        string
	uploadID   string
	part       int32
	rangeStart int64 // -1 when the request has no range
	accessKey  string
	fault      *Fault
}

type core struct {
	clock engine.Clock
	guard engine.WriteGuard
	rules ruleSet

	mu         sync.Mutex
	buckets    map[string]*bucket
	nextUpload int
	seq        int
	log        []RequestRecord
	logCap     int
	refused    int
	counts     map[attemptKey]int
}

func newCore(clock engine.Clock, guard engine.WriteGuard, buckets []string, regions map[string]string) *core {
	c := &core{
		clock: clock, guard: guard,
		buckets: make(map[string]*bucket),
		logCap:  1 << 20,
		counts:  make(map[attemptKey]int),
	}
	for _, name := range buckets {
		c.buckets[name] = newBucket(Region, clock.Now())
	}
	for name, region := range regions {
		c.buckets[name] = newBucket(region, clock.Now())
	}
	return c
}

func newBucket(region string, created time.Time) *bucket {
	return &bucket{region: region, created: created, objects: make(map[string]*object), uploads: make(map[string]*upload)}
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 64*1024); return &b }}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (f *Fault) bodyLevel(op Op) bool {
	if f.After {
		return false
	}
	switch f.Kind {
	case FaultSlow:
		return f.BytesPerSec > 0
	case FaultTruncate:
		return op == OpGet
	case FaultReset, FaultStall:
		switch op {
		case OpGet:
			return f.AtByte > 0 || f.AfterHeaders
		case OpUploadPart, OpPut:
			return f.AtByte > 0
		}
	}
	return false
}

func (f *Fault) apiErr() *apiError {
	status := f.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &apiError{Status: status, Code: f.Code, Message: f.Message, RetryAfter: f.RetryAfter, In200: f.In200}
}

// exec runs one operation under the guard, the rule engine and the request log.
// fn reads the request body from the reader it is given, which already carries
// any body-level fault.
func (c *core) exec(ctx context.Context, info *reqInfo, body io.Reader, fn func(body io.Reader) error) (err error) {
	rec := RequestRecord{Op: info.op, Bucket: info.bucket, Key: info.key, UploadID: info.uploadID, Part: info.part, AccessKeyID: info.accessKey}
	counter := &countReader{r: body}
	defer func() {
		rec.BodyBytes = counter.n
		var ae *apiError
		switch {
		case err == nil:
			rec.Status = http.StatusOK
		case errors.As(err, &ae):
			rec.Status, rec.Code = ae.Status, ae.Code
		}
		c.record(rec)
	}()

	if info.op.mutating() && c.guard != nil {
		if gerr := c.guard.CheckWrite(info.key); gerr != nil {
			rec.Refused = true
			c.mu.Lock()
			c.refused++
			c.mu.Unlock()
			return s3err(http.StatusForbidden, "AccessDenied", "write refused by test-prefix guard: "+gerr.Error())
		}
	}

	var f *Fault
	rec.Attempt, f = c.rules.match(info)
	info.fault = f
	if body == nil {
		counter = &countReader{r: strings.NewReader("")}
	}
	var reader io.Reader = counter
	if f != nil {
		rec.Fault = f.Kind
		bodyLevel := f.bodyLevel(info.op)
		switch f.Kind {
		case FaultChangeObject:
			c.changeETag(info.bucket, info.key)
		case FaultError:
			if !f.After {
				return f.apiErr()
			}
		case FaultReset:
			if !f.After && !bodyLevel {
				return errReset
			}
		case FaultStall:
			if !f.After && !bodyLevel {
				if serr := sleep(ctx, c.clock, f.Delay); serr != nil {
					return serr
				}
			}
		case FaultSlow:
			if f.Delay > 0 {
				if serr := sleep(ctx, c.clock, f.Delay); serr != nil {
					return serr
				}
			}
		}
		if bodyLevel && info.op != OpGet {
			reader = &faultReader{ctx: ctx, clock: c.clock, r: counter, f: *f}
		}
	}

	if err = fn(reader); err != nil || f == nil || !f.After {
		return err
	}
	switch f.Kind {
	case FaultError:
		return f.apiErr()
	case FaultReset:
		return errReset
	case FaultStall:
		return sleep(ctx, c.clock, f.Delay)
	}
	return nil
}

func (c *core) record(rec RequestRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	rec.Seq = c.seq
	c.counts[attemptKey{rec.Op, rec.Bucket, rec.Key, rec.Part}]++
	if len(c.log) < c.logCap {
		c.log = append(c.log, rec)
	}
}

func (c *core) changeETag(bucketName, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.buckets[bucketName]; b != nil {
		if o := b.objects[key]; o != nil {
			o.changes++
			sum := md5.Sum(fmt.Appendf(nil, "%s|changed|%d", o.etag, o.changes))
			o.etag = hex.EncodeToString(sum[:])
			if o.multipart {
				o.etag += fmt.Sprintf("-%d", len(o.parts))
			}
		}
	}
}

// bucketLocked returns the bucket or a NoSuchBucket error.
func (c *core) bucketLocked(name string) (*bucket, *apiError) {
	if b := c.buckets[name]; b != nil {
		return b, nil
	}
	return nil, s3err(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
}

func crcBase64(sum uint32) string {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], sum)
	return base64.StdEncoding.EncodeToString(raw[:])
}

// partETag stands in for S3's MD5 ETag. It is a 32-hex-digit hash of the part's
// CRC32 and length, because hashing 2 TB with MD5 would make simulation slow.
func partETag(crc uint32, size int64) string {
	var raw [12]byte
	binary.BigEndian.PutUint32(raw[:4], crc)
	binary.BigEndian.PutUint64(raw[4:], uint64(size))
	sum := md5.Sum(raw[:])
	return hex.EncodeToString(sum[:])
}

// multipartETag follows S3's rule: MD5 of the concatenated binary part ETags,
// then "-<part count>".
func multipartETag(parts []partMeta) string {
	h := md5.New()
	for _, p := range parts {
		raw, _ := hex.DecodeString(p.etag)
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil)) + fmt.Sprintf("-%d", len(parts))
}

func unquote(etag string) string { return strings.Trim(etag, `"`) }

func quote(etag string) string { return `"` + etag + `"` }

// ---- multipart upload ----

func validKey(key string) *apiError {
	if key == "" {
		return s3err(http.StatusBadRequest, "InvalidArgument", "Object key must not be empty")
	}
	if len(key) > 1024 {
		return s3err(http.StatusBadRequest, "KeyTooLongError", "Your key is too long")
	}
	return nil
}

type createIn struct {
	info        reqInfo
	crcRequired bool
}

func (c *core) createUpload(ctx context.Context, in createIn) (id string, err error) {
	in.info.op = OpCreateUpload
	err = c.exec(ctx, &in.info, nil, func(io.Reader) error {
		if e := validKey(in.info.key); e != nil {
			return e
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		b, e := c.bucketLocked(in.info.bucket)
		if e != nil {
			return e
		}
		c.nextUpload++
		id = fmt.Sprintf("fake-upload-%06d", c.nextUpload)
		b.uploads[id] = &upload{
			id: id, key: in.info.key, seq: c.nextUpload, created: c.clock.Now(),
			crcRequired: in.crcRequired, parts: make(map[int32]*uploadedPart),
		}
		return nil
	})
	return id, err
}

type partIn struct {
	info          reqInfo
	body          io.Reader
	contentLength int64 // -1 when unknown
	// declared returns the client's CRC32 after the body has been read, from a
	// header or a trailer. "" means none was sent.
	declared func() string
	// hasChecksum says whether the client announced a CRC32 before the body.
	hasChecksum bool
}

type partOut struct {
	etag string // quoted
	crc  string
}

func (c *core) uploadPart(ctx context.Context, in partIn) (out partOut, err error) {
	in.info.op = OpUploadPart
	in.info.rangeStart = -1
	err = c.exec(ctx, &in.info, in.body, func(body io.Reader) error {
		if in.info.part < 1 || in.info.part > engine.MaxPartCount {
			return s3err(http.StatusBadRequest, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive")
		}
		if in.contentLength > engine.MaxPartSize {
			return s3err(http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size")
		}
		c.mu.Lock()
		b, e := c.bucketLocked(in.info.bucket)
		var up *upload
		if e == nil {
			if up = b.uploads[in.info.uploadID]; up == nil || up.key != in.info.key {
				e = s3err(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.")
			}
		}
		required := up != nil && up.crcRequired
		c.mu.Unlock()
		if e != nil {
			return e
		}
		if required && !in.hasChecksum {
			return s3err(http.StatusBadRequest, "InvalidRequest", "Checksum Type mismatch occurred, expected checksum Type: crc32, actual checksum Type: null")
		}
		n, sum, rerr := consume(body, in.contentLength)
		if rerr != nil {
			return rerr
		}
		if n > engine.MaxPartSize {
			return s3err(http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size")
		}
		got := crcBase64(sum)
		if in.declared != nil {
			if want := in.declared(); want != "" && want != got {
				return s3err(http.StatusBadRequest, "BadDigest", "The CRC32 you specified did not match the calculated checksum.")
			}
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		// The upload may have been aborted or completed while the body streamed.
		if up = b.uploads[in.info.uploadID]; up == nil {
			return s3err(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.")
		}
		meta := partMeta{size: n, crc: sum, etag: partETag(sum, n)}
		up.parts[in.info.part] = &uploadedPart{partMeta: meta, modified: c.clock.Now()}
		out = partOut{etag: quote(meta.etag), crc: got}
		return nil
	})
	return out, err
}

// consume reads body to its end, or to length bytes when length >= 0, and
// returns the byte count and CRC32. It never buffers more than one chunk.
func consume(body io.Reader, length int64) (int64, uint32, error) {
	if length >= 0 {
		// One byte more than declared, so an overrun is seen and a chunked
		// body is still read to its end, where its trailer arrives.
		body = io.LimitReader(body, length+1)
	}
	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	buf := *bufp
	var n int64
	var sum uint32
	for {
		m, err := body.Read(buf)
		if m > 0 {
			sum = crc32.Update(sum, crc32.IEEETable, buf[:m])
			n += int64(m)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, sum, err
		}
	}
	if length >= 0 && n > length {
		return n, sum, s3err(http.StatusBadRequest, "InvalidRequest", "The request body is longer than the declared length.")
	}
	if length >= 0 && n != length {
		return n, sum, s3err(http.StatusBadRequest, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.")
	}
	return n, sum, nil
}

type completeIn struct {
	info       reqInfo
	parts      []engine.CompletedPart
	objectSize int64 // -1 when not sent
}

type completeOut struct {
	etag string // quoted
	crc  string
}

func (c *core) completeUpload(ctx context.Context, in completeIn) (out completeOut, err error) {
	in.info.op = OpComplete
	in.info.rangeStart = -1
	err = c.exec(ctx, &in.info, nil, func(io.Reader) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, e := c.bucketLocked(in.info.bucket)
		if e != nil {
			return e
		}
		up := b.uploads[in.info.uploadID]
		if up == nil || up.key != in.info.key {
			return s3err(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.")
		}
		if len(in.parts) == 0 {
			return s3err(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
		}
		if len(in.parts) > engine.MaxPartCount {
			return s3err(http.StatusBadRequest, "InvalidArgument", "Too many parts in the complete request")
		}
		metas := make([]partMeta, len(in.parts))
		crcs := make([]string, len(in.parts))
		var total int64
		for i, p := range in.parts {
			if i > 0 && p.Number <= in.parts[i-1].Number {
				return s3err(http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order. The parts list must be specified in order by part number.")
			}
			have := up.parts[p.Number]
			if have == nil || unquote(p.ETag) != have.etag {
				return s3err(http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found.  The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.")
			}
			if up.crcRequired {
				if p.ChecksumCRC32 == "" {
					return s3err(http.StatusBadRequest, "InvalidRequest", fmt.Sprintf("The upload was created using a crc32 checksum. The complete request must include the checksum for each part. It was missing for part %d in the request.", p.Number))
				}
				if p.ChecksumCRC32 != crcBase64(have.crc) {
					return s3err(http.StatusBadRequest, "InvalidPart", fmt.Sprintf("One or more of the specified parts could not be found. The checksum for part %d did not match.", p.Number))
				}
			}
			metas[i] = have.partMeta
			crcs[i] = crcBase64(have.crc)
			total += have.size
		}
		for i, m := range metas[:len(metas)-1] {
			if m.size < engine.MinPartSize {
				return s3err(http.StatusBadRequest, "EntityTooSmall", fmt.Sprintf("Your proposed upload is smaller than the minimum allowed size. Part %d is %d bytes.", in.parts[i].Number, m.size))
			}
		}
		if total > engine.MaxObjectSize {
			return s3err(http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size")
		}
		if in.objectSize >= 0 && in.objectSize != total {
			return s3err(http.StatusBadRequest, "InvalidRequest", fmt.Sprintf("The provided 'x-amz-mp-object-size' header value %d does not match what was computed: %d", in.objectSize, total))
		}
		composite, cerr := engine.CompositeCRC32(crcs)
		if cerr != nil {
			return s3err(http.StatusInternalServerError, "InternalError", cerr.Error())
		}
		obj := &object{size: total, etag: multipartETag(metas), parts: metas, multipart: true, modified: c.clock.Now()}
		if up.crcRequired {
			obj.crc = composite
		}
		b.putLocked(in.info.key, obj)
		delete(b.uploads, up.id)
		out = completeOut{etag: quote(obj.etag), crc: obj.crc}
		return nil
	})
	return out, err
}

func (c *core) abortUpload(ctx context.Context, info reqInfo) error {
	info.op = OpAbort
	info.rangeStart = -1
	return c.exec(ctx, &info, nil, func(io.Reader) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, e := c.bucketLocked(info.bucket)
		if e != nil {
			return e
		}
		up := b.uploads[info.uploadID]
		if up == nil || up.key != info.key {
			return s3err(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.")
		}
		delete(b.uploads, up.id)
		return nil
	})
}

func (b *bucket) putLocked(key string, o *object) {
	if _, ok := b.objects[key]; !ok {
		b.dirty = true
	}
	b.objects[key] = o
}

// ---- single object ----

type putIn struct {
	info          reqInfo
	body          io.Reader
	contentLength int64
	declared      func() string
}

func (c *core) putObject(ctx context.Context, in putIn) (out partOut, err error) {
	in.info.op = OpPut
	in.info.rangeStart = -1
	err = c.exec(ctx, &in.info, in.body, func(body io.Reader) error {
		if e := validKey(in.info.key); e != nil {
			return e
		}
		if in.contentLength > engine.MaxSinglePutSize {
			return s3err(http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size")
		}
		c.mu.Lock()
		b, e := c.bucketLocked(in.info.bucket)
		c.mu.Unlock()
		if e != nil {
			return e
		}
		n, sum, rerr := consume(body, in.contentLength)
		if rerr != nil {
			return rerr
		}
		if n > engine.MaxSinglePutSize {
			return s3err(http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size")
		}
		got := crcBase64(sum)
		if in.declared != nil {
			if want := in.declared(); want != "" && want != got {
				return s3err(http.StatusBadRequest, "BadDigest", "The CRC32 you specified did not match the calculated checksum.")
			}
		}
		meta := partMeta{size: n, crc: sum, etag: partETag(sum, n)}
		c.mu.Lock()
		defer c.mu.Unlock()
		b.putLocked(in.info.key, &object{size: n, etag: meta.etag, parts: []partMeta{meta}, crc: got, modified: c.clock.Now()})
		out = partOut{etag: quote(meta.etag), crc: got}
		return nil
	})
	return out, err
}

// objMeta is a copy of an object's visible metadata.
type objMeta struct {
	size      int64
	etag      string // quoted
	crc       string
	multipart bool
	modified  time.Time
	seed      uint64
	seedKnown bool
}

func metaOf(o *object) objMeta {
	return objMeta{size: o.size, etag: quote(o.etag), crc: o.crc, multipart: o.multipart, modified: o.modified, seed: o.seed, seedKnown: o.seedKnown}
}

func noSuchKey() *apiError {
	return s3err(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
}

func (c *core) headObject(ctx context.Context, info reqInfo) (m objMeta, err error) {
	info.op = OpHead
	info.rangeStart = -1
	err = c.exec(ctx, &info, nil, func(io.Reader) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, e := c.bucketLocked(info.bucket)
		if e != nil {
			return e
		}
		o := b.objects[info.key]
		if o == nil {
			return noSuchKey()
		}
		m = metaOf(o)
		return nil
	})
	return m, err
}

func (c *core) deleteObject(ctx context.Context, info reqInfo) error {
	info.op = OpDelete
	info.rangeStart = -1
	return c.exec(ctx, &info, nil, func(io.Reader) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, e := c.bucketLocked(info.bucket)
		if e != nil {
			return e
		}
		if _, ok := b.objects[info.key]; ok {
			delete(b.objects, info.key)
			b.dirty = true
		}
		return nil
	})
}

// byteRange is a requested range. Exactly one form is set.
type byteRange struct {
	start, end int64 // inclusive end; -1 means "to the end"
	suffix     int64 // last n bytes, when start < 0
}

type getIn struct {
	info    reqInfo
	rng     *byteRange
	ifMatch string
	ifNone  string
}

type getOut struct {
	meta       objMeta
	start, end int64 // inclusive; end < start for an empty body
	partial    bool
	body       io.ReadCloser
	fault      *Fault
}

func (c *core) getObject(ctx context.Context, in getIn) (out getOut, err error) {
	in.info.op = OpGet
	in.info.rangeStart = -1
	if in.rng != nil && in.rng.start >= 0 {
		in.info.rangeStart = in.rng.start
	}
	err = c.exec(ctx, &in.info, nil, func(io.Reader) error {
		c.mu.Lock()
		b, e := c.bucketLocked(in.info.bucket)
		var o *object
		if e == nil {
			if o = b.objects[in.info.key]; o == nil {
				e = noSuchKey()
			}
		}
		var m objMeta
		if e == nil {
			m = metaOf(o)
		}
		c.mu.Unlock()
		if e != nil {
			return e
		}
		if in.ifMatch != "" && !etagMatches(in.ifMatch, m.etag) {
			return s3err(http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold")
		}
		if in.ifNone != "" && etagMatches(in.ifNone, m.etag) {
			return &apiError{Status: http.StatusNotModified}
		}
		if !m.seedKnown {
			return s3err(http.StatusInternalServerError, "InternalError", "fakes3: object content is not stored; create it with PutSynthetic or confirm an upload with VerifyObject")
		}
		start, end := int64(0), m.size-1
		partial := false
		if r := in.rng; r != nil {
			partial = true
			switch {
			case r.start >= 0:
				start = r.start
				if r.end >= 0 {
					end = min(r.end, m.size-1)
				}
			default:
				start = max(0, m.size-r.suffix)
			}
			if start >= m.size {
				return s3err(http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
			}
		}
		var body io.ReadCloser = io.NopCloser(Pattern{Seed: m.seed}.NewReader(start, end-start+1))
		if f := in.info.fault; f != nil && f.bodyLevel(OpGet) {
			body = readCloser{&faultReader{ctx: ctx, clock: c.clock, r: body, f: *f}, body}
		}
		out = getOut{meta: m, start: start, end: end, partial: partial, body: body, fault: in.info.fault}
		return nil
	})
	return out, err
}

type readCloser struct {
	io.Reader
	io.Closer
}

func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || unquote(strings.TrimPrefix(candidate, "W/")) == unquote(etag) {
			return true
		}
	}
	return false
}

// ---- listings ----

type objSummary struct {
	key      string
	size     int64
	etag     string
	modified time.Time
}

type listObjectsIn struct {
	info       reqInfo
	prefix     string
	delimiter  string
	token      string
	startAfter string
	maxKeys    int
}

type listObjectsOut struct {
	objects   []objSummary
	prefixes  []string
	truncated bool
	next      string
}

func (b *bucket) sortedKeys() []string {
	if b.dirty || len(b.sorted) != len(b.objects) {
		b.sorted = b.sorted[:0]
		for k := range b.objects {
			b.sorted = append(b.sorted, k)
		}
		sort.Strings(b.sorted)
		b.dirty = false
	}
	return b.sorted
}

// prefixEnd returns the smallest string greater than every string with prefix p.
func prefixEnd(p string) string {
	raw := []byte(p)
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] < 0xff {
			raw[i]++
			return string(raw[:i+1])
		}
	}
	return "\xff\xff\xff\xff"
}

func (c *core) listObjects(ctx context.Context, in listObjectsIn) (out listObjectsOut, err error) {
	in.info.op = OpListObjects
	in.info.rangeStart = -1
	err = c.exec(ctx, &in.info, nil, func(io.Reader) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, e := c.bucketLocked(in.info.bucket)
		if e != nil {
			return e
		}
		maxKeys := in.maxKeys
		if maxKeys <= 0 || maxKeys > 1000 {
			maxKeys = 1000
		}
		keys := b.sortedKeys()
		idx := sort.SearchStrings(keys, in.prefix)
		after, inclusive := in.startAfter, false
		if in.token != "" {
			raw, derr := base64.RawURLEncoding.DecodeString(in.token)
			if derr != nil || len(raw) == 0 {
				return s3err(http.StatusBadRequest, "InvalidArgument", "The continuation token provided is incorrect")
			}
			after, inclusive = string(raw[1:]), raw[0] == 'g'
		}
		if after != "" {
			var j int
			if inclusive {
				j = sort.SearchStrings(keys, after)
			} else {
				j = sort.Search(len(keys), func(k int) bool { return keys[k] > after })
			}
			idx = max(idx, j)
		}
		count := 0
		for idx < len(keys) && strings.HasPrefix(keys[idx], in.prefix) {
			key := keys[idx]
			if count == maxKeys {
				out.truncated = true
				break
			}
			if in.delimiter != "" {
				rest := key[len(in.prefix):]
				if j := strings.Index(rest, in.delimiter); j >= 0 {
					cp := in.prefix + rest[:j+len(in.delimiter)]
					out.prefixes = append(out.prefixes, cp)
					count++
					out.next = "g" + prefixEnd(cp)
					idx = max(idx+1, sort.SearchStrings(keys, prefixEnd(cp)))
					continue
				}
			}
			o := b.objects[key]
			out.objects = append(out.objects, objSummary{key: key, size: o.size, etag: quote(o.etag), modified: o.modified})
			count++
			out.next = "k" + key
			idx++
		}
		if out.truncated {
			out.next = base64.RawURLEncoding.EncodeToString([]byte(out.next))
		} else {
			out.next = ""
		}
		return nil
	})
	return out, err
}

type uploadSummary struct {
	key, id string
	created time.Time
}

type listUploadsIn struct {
	info       reqInfo
	prefix     string
	keyMarker  string
	idMarker   string
	maxUploads int
}

type listUploadsOut struct {
	uploads               []uploadSummary
	truncated             bool
	nextKey, nextUploadID string
}

func (c *core) listUploads(ctx context.Context, in listUploadsIn) (out listUploadsOut, err error) {
	in.info.op = OpListUploads
	in.info.rangeStart = -1
	err = c.exec(ctx, &in.info, nil, func(io.Reader) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, e := c.bucketLocked(in.info.bucket)
		if e != nil {
			return e
		}
		var all []*upload
		for _, u := range b.uploads {
			if !strings.HasPrefix(u.key, in.prefix) {
				continue
			}
			if in.keyMarker != "" && (u.key < in.keyMarker || (u.key == in.keyMarker && (in.idMarker == "" || u.id <= in.idMarker))) {
				continue
			}
			all = append(all, u)
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].key != all[j].key {
				return all[i].key < all[j].key
			}
			return all[i].id < all[j].id
		})
		limit := in.maxUploads
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
		if len(all) > limit {
			out.truncated = true
			all = all[:limit]
			out.nextKey, out.nextUploadID = all[limit-1].key, all[limit-1].id
		}
		for _, u := range all {
			out.uploads = append(out.uploads, uploadSummary{key: u.key, id: u.id, created: u.created})
		}
		return nil
	})
	return out, err
}

type listPartsIn struct {
	info   reqInfo
	marker int32
	max    int
}

type partSummary struct {
	number   int32
	etag     string
	size     int64
	crc      string
	modified time.Time
}

type listPartsOut struct {
	parts     []partSummary
	truncated bool
	next      int32
	crcAlgo   bool
}

func (c *core) listParts(ctx context.Context, in listPartsIn) (out listPartsOut, err error) {
	in.info.op = OpListParts
	in.info.rangeStart = -1
	err = c.exec(ctx, &in.info, nil, func(io.Reader) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		b, e := c.bucketLocked(in.info.bucket)
		if e != nil {
			return e
		}
		up := b.uploads[in.info.uploadID]
		if up == nil || up.key != in.info.key {
			return s3err(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.")
		}
		var numbers []int32
		for n := range up.parts {
			if n > in.marker {
				numbers = append(numbers, n)
			}
		}
		sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
		limit := in.max
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
		if len(numbers) > limit {
			out.truncated = true
			numbers = numbers[:limit]
			out.next = numbers[limit-1]
		}
		out.crcAlgo = up.crcRequired
		for _, n := range numbers {
			p := up.parts[n]
			out.parts = append(out.parts, partSummary{number: n, etag: quote(p.etag), size: p.size, crc: crcBase64(p.crc), modified: p.modified})
		}
		return nil
	})
	return out, err
}
