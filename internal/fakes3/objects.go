package fakes3

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Object operations used by the app's object commands: batch delete, tagging,
// bucket versioning, object headers and user metadata, and expiry of presigned
// URLs.

// storedHeaders are the standard headers S3 keeps with an object and returns on
// GET and HEAD.
var storedHeaders = []string{"Content-Type", "Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"}

// objExtra is what a client sets on an object besides its data.
type objExtra struct {
	headers map[string]string // canonical header name -> value
	meta    map[string]string // user metadata, lower-case names without x-amz-meta-
	tags    []Tag
}

// Tag is one object tag.
type Tag struct {
	Key   string
	Value string
}

func (e objExtra) clone() objExtra {
	return objExtra{headers: maps.Clone(e.headers), meta: maps.Clone(e.meta), tags: append([]Tag(nil), e.tags...)}
}

// extraFrom reads the stored headers, user metadata and x-amz-tagging from a
// PutObject or CreateMultipartUpload request.
func extraFrom(h http.Header) objExtra {
	var e objExtra
	for _, name := range storedHeaders {
		if v := h.Get(name); v != "" {
			if e.headers == nil {
				e.headers = map[string]string{}
			}
			e.headers[name] = v
		}
	}
	for name, vals := range h {
		low := strings.ToLower(name)
		if rest, ok := strings.CutPrefix(low, "x-amz-meta-"); ok && len(vals) > 0 {
			if e.meta == nil {
				e.meta = map[string]string{}
			}
			e.meta[rest] = vals[0]
		}
	}
	if v := h.Get("x-amz-tagging"); v != "" {
		if q, err := url.ParseQuery(v); err == nil {
			for k, vals := range q {
				e.tags = append(e.tags, Tag{Key: k, Value: first(vals)})
			}
			sortTags(e.tags)
		}
	}
	return e
}

func sortTags(t []Tag) {
	for i := 1; i < len(t); i++ {
		for j := i; j > 0 && t[j].Key < t[j-1].Key; j-- {
			t[j], t[j-1] = t[j-1], t[j]
		}
	}
}

func (e objExtra) writeHeaders(h http.Header) {
	for name, v := range e.headers {
		h.Set(name, v)
	}
	for name, v := range e.meta {
		h.Set("x-amz-meta-"+name, v)
	}
	if len(e.tags) > 0 {
		h.Set("x-amz-tagging-count", strconv.Itoa(len(e.tags)))
	}
}

// ---- presigned URLs ----

// checkPresigned refuses a query-signed request whose URL has expired, or that
// asks for more than S3's seven days. The signature itself is not checked.
func (s *Server) checkPresigned(q url.Values) error {
	date, expires := q.Get("X-Amz-Date"), q.Get("X-Amz-Expires")
	if date == "" || expires == "" {
		return nil
	}
	signed, err := time.Parse("20060102T150405Z", date)
	secs, err2 := strconv.Atoi(expires)
	if err != nil || err2 != nil || secs < 1 {
		return s3err(http.StatusBadRequest, "AuthorizationQueryParametersError", "X-Amz-Date or X-Amz-Expires is malformed")
	}
	if secs > 7*24*3600 {
		return s3err(http.StatusBadRequest, "AuthorizationQueryParametersError", "X-Amz-Expires must be less than a week (in seconds) that is 604800")
	}
	if s.core.clock.Now().After(signed.Add(time.Duration(secs) * time.Second)) {
		return s3err(http.StatusForbidden, "AccessDenied", "Request has expired")
	}
	return nil
}

// ---- tagging ----

type xmlTag struct {
	Key   string
	Value string
}

type xmlTagging struct {
	XMLName xml.Name `xml:"Tagging"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	TagSet  struct {
		Tags []xmlTag `xml:"Tag"`
	}
}

func (s *Server) serveTagging(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo) error {
	switch r.Method {
	case http.MethodGet:
		tags, err := s.core.getTagging(ctx, info)
		if err != nil {
			return err
		}
		res := xmlTagging{XMLNS: s3NS}
		for _, t := range tags {
			res.TagSet.Tags = append(res.TagSet.Tags, xmlTag(t))
		}
		writeXML(w, http.StatusOK, res)
		return nil
	case http.MethodPut:
		var req xmlTagging
		if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			return s3err(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
		}
		tags := make([]Tag, len(req.TagSet.Tags))
		for i, t := range req.TagSet.Tags {
			tags[i] = Tag(t)
		}
		if err := s.core.putTagging(ctx, info, tags); err != nil {
			return err
		}
		w.WriteHeader(http.StatusOK)
		return nil
	case http.MethodDelete:
		if err := s.core.putTagging(ctx, info, nil); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	return s3err(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
}

// validTags applies S3's rules for an object's tag set.
func validTags(tags []Tag) *apiError {
	if len(tags) > 10 {
		return s3err(http.StatusBadRequest, "BadRequest", "Object tags cannot be greater than 10")
	}
	seen := map[string]bool{}
	for _, t := range tags {
		if n := utf8.RuneCountInString(t.Key); n < 1 || n > 128 {
			return s3err(http.StatusBadRequest, "InvalidTag", "The TagKey you have provided is invalid")
		}
		if utf8.RuneCountInString(t.Value) > 256 {
			return s3err(http.StatusBadRequest, "InvalidTag", "The TagValue you have provided is invalid")
		}
		if strings.HasPrefix(strings.ToLower(t.Key), "aws:") {
			return s3err(http.StatusBadRequest, "InvalidTag", "Your TagKey cannot be prefixed with aws:")
		}
		if seen[t.Key] {
			return s3err(http.StatusBadRequest, "InvalidTag", "Cannot provide multiple Tags with the same key")
		}
		seen[t.Key] = true
	}
	return nil
}

func (c *core) getTagging(ctx context.Context, info reqInfo) (tags []Tag, err error) {
	info.op = OpGetTagging
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
		tags = append([]Tag(nil), o.extra.tags...)
		return nil
	})
	return tags, err
}

// putTagging replaces an object's tags; nil tags is DeleteObjectTagging.
func (c *core) putTagging(ctx context.Context, info reqInfo, tags []Tag) error {
	info.op = OpPutTagging
	if tags == nil {
		info.op = OpDeleteTagging
	}
	info.rangeStart = -1
	return c.exec(ctx, &info, nil, func(io.Reader) error {
		if e := validTags(tags); e != nil {
			return e
		}
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
		o.extra.tags = append([]Tag(nil), tags...)
		return nil
	})
}

// ---- versioning ----

type versioningResult struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	XMLNS   string   `xml:"xmlns,attr"`
	Status  string   `xml:",omitempty"`
}

func (s *Server) serveVersioning(ctx context.Context, w http.ResponseWriter, info reqInfo) error {
	info.op = OpVersioning
	info.rangeStart = -1
	var status string
	err := s.core.exec(ctx, &info, nil, func(io.Reader) error {
		s.core.mu.Lock()
		defer s.core.mu.Unlock()
		b, e := s.core.bucketLocked(info.bucket)
		if e != nil {
			return e
		}
		status = b.versioning
		return nil
	})
	if err != nil {
		return err
	}
	writeXML(w, http.StatusOK, versioningResult{XMLNS: s3NS, Status: status})
	return nil
}

// ---- batch delete ----

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Quiet   bool
	Objects []struct {
		Key       string
		VersionID string `xml:"VersionId"`
	} `xml:"Object"`
}

type xmlDeleted struct {
	Key                   string
	DeleteMarker          bool   `xml:",omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

type xmlDeleteError struct {
	Key     string
	Code    string
	Message string
}

type deleteResult struct {
	XMLName xml.Name `xml:"DeleteResult"`
	XMLNS   string   `xml:"xmlns,attr"`
	Deleted []xmlDeleted     `xml:"Deleted"`
	Errors  []xmlDeleteError `xml:"Error"`
}

func (s *Server) serveDeleteObjects(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo) error {
	var req deleteRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil || len(req.Objects) == 0 || len(req.Objects) > 1000 {
		return s3err(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
	}
	keys := make([]string, len(req.Objects))
	for i, o := range req.Objects {
		keys[i] = o.Key
	}
	res, err := s.core.deleteObjects(ctx, info, keys)
	if err != nil {
		return err
	}
	if req.Quiet {
		res.Deleted = nil
	}
	res.XMLNS = s3NS
	writeXML(w, http.StatusOK, res)
	return nil
}

// deleteObjects removes keys one by one. A key the guard refuses, or one a
// fault rule for OpDelete with that key fails, is reported as that key's error.
// In a versioned bucket every delete reports a delete marker; the fake keeps no
// older versions.
func (c *core) deleteObjects(ctx context.Context, info reqInfo, keys []string) (res deleteResult, err error) {
	info.op = OpDeleteObjects
	info.rangeStart = -1
	err = c.exec(ctx, &info, nil, func(io.Reader) error {
		c.mu.Lock()
		b, e := c.bucketLocked(info.bucket)
		c.mu.Unlock()
		if e != nil {
			return e
		}
		for _, key := range keys {
			if c.guard != nil {
				if gerr := c.guard.CheckWrite(key); gerr != nil {
					c.mu.Lock()
					c.refused++
					c.mu.Unlock()
					res.Errors = append(res.Errors, xmlDeleteError{Key: key, Code: "AccessDenied", Message: "write refused by test-prefix guard: " + gerr.Error()})
					continue
				}
			}
			if f := c.keyFault(info, key); f != nil {
				ae := f.apiErr()
				res.Errors = append(res.Errors, xmlDeleteError{Key: key, Code: ae.Code, Message: ae.Message})
				continue
			}
			c.mu.Lock()
			if _, ok := b.objects[key]; ok {
				delete(b.objects, key)
				b.dirty = true
			}
			versioned := b.versioning == "Enabled"
			marker := ""
			if versioned {
				marker = fmt.Sprintf("fake-marker-%06d", len(res.Deleted)+1)
			}
			c.mu.Unlock()
			res.Deleted = append(res.Deleted, xmlDeleted{Key: key, DeleteMarker: versioned, DeleteMarkerVersionID: marker})
		}
		return nil
	})
	return res, err
}

// keyFault lets a test fail one key of a batch delete with a FaultError rule
// for OpDelete on that key. Other fault kinds do not apply inside a batch.
func (c *core) keyFault(info reqInfo, key string) *Fault {
	ki := info
	ki.op, ki.key = OpDelete, key
	_, f := c.rules.match(&ki)
	if f == nil || f.Kind != FaultError {
		return nil
	}
	return f
}

// ---- test controls ----

// SetVersioning sets what GetBucketVersioning reports: "", "Enabled" or
// "Suspended".
func (s *Server) SetVersioning(bucket, status string) error {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	b, e := s.core.bucketLocked(bucket)
	if e != nil {
		return e
	}
	b.versioning = status
	return nil
}

// ObjectExtras is what a client set on an object besides its data.
type ObjectExtras struct {
	// Headers holds stored standard headers such as Content-Type, by canonical name.
	Headers map[string]string
	// Meta is user metadata with lower-case names, without "x-amz-meta-".
	Meta map[string]string
	Tags []Tag
}

// Extras returns an object's headers, metadata and tags.
func (s *Server) Extras(bucket, key string) (ObjectExtras, bool) {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	b := s.core.buckets[bucket]
	if b == nil || b.objects[key] == nil {
		return ObjectExtras{}, false
	}
	e := b.objects[key].extra.clone()
	return ObjectExtras{Headers: e.headers, Meta: e.meta, Tags: e.tags}, true
}

// SetExtras replaces an object's headers, metadata and tags, for seeding.
func (s *Server) SetExtras(bucket, key string, x ObjectExtras) error {
	s.core.mu.Lock()
	defer s.core.mu.Unlock()
	b, e := s.core.bucketLocked(bucket)
	if e != nil {
		return e
	}
	o := b.objects[key]
	if o == nil {
		return noSuchKey()
	}
	o.extra = objExtra{headers: maps.Clone(x.Headers), meta: maps.Clone(x.Meta), tags: append([]Tag(nil), x.Tags...)}
	return nil
}
