package fakes3

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Bucket-level operations used to browse: ListBuckets, HeadBucket,
// GetBucketLocation, and S3's redirect for a request signed for the wrong region.

// signingRegion returns the region in a SigV4 Authorization header:
// "Credential=<key>/<date>/<region>/s3/aws4_request". Empty if absent.
func signingRegion(r *http.Request) string {
	_, rest, ok := strings.Cut(r.Header.Get("Authorization"), "Credential=")
	if !ok {
		return ""
	}
	scope, _, _ := strings.Cut(rest, ",")
	parts := strings.Split(scope, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func bucketOp(r *http.Request, q map[string][]string) Op {
	if r.Method == http.MethodHead {
		return OpHeadBucket
	}
	if _, ok := q["uploads"]; ok {
		return OpListUploads
	}
	return OpListObjects
}

// checkRegion answers a request signed for another region than the bucket's
// with 301 PermanentRedirect and the x-amz-bucket-region header, as S3 does.
func (s *Server) checkRegion(w http.ResponseWriter, r *http.Request, info reqInfo) error {
	s.core.mu.Lock()
	b := s.core.buckets[info.bucket]
	region := ""
	if b != nil {
		region = b.region
	}
	s.core.mu.Unlock()
	if b == nil {
		return nil // the operation reports NoSuchBucket
	}
	if !s.hideRegion {
		w.Header().Set("x-amz-bucket-region", region)
	}
	signed := signingRegion(r)
	if signed == "" || signed == region {
		return nil
	}
	info.op = bucketOp(r, r.URL.Query())
	s.core.record(RequestRecord{Op: info.op, Bucket: info.bucket, AccessKeyID: info.accessKey, Status: http.StatusMovedPermanently, Code: "PermanentRedirect"})
	return s3err(http.StatusMovedPermanently, "PermanentRedirect",
		"The bucket you are attempting to access must be addressed using the specified endpoint. Please send all future requests to this endpoint.")
}

func (s *Server) serveHeadBucket(ctx context.Context, w http.ResponseWriter, info reqInfo) error {
	info.op = OpHeadBucket
	info.rangeStart = -1
	err := s.core.exec(ctx, &info, nil, func(io.Reader) error {
		s.core.mu.Lock()
		defer s.core.mu.Unlock()
		_, e := s.core.bucketLocked(info.bucket)
		if e != nil {
			return e
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

type locationResult struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	XMLNS   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

// serveLocation answers GetBucketLocation from any region. us-east-1 is an
// empty constraint, as in S3.
func (s *Server) serveLocation(ctx context.Context, w http.ResponseWriter, info reqInfo) error {
	info.op = OpLocation
	info.rangeStart = -1
	var region string
	err := s.core.exec(ctx, &info, nil, func(io.Reader) error {
		s.core.mu.Lock()
		defer s.core.mu.Unlock()
		b, e := s.core.bucketLocked(info.bucket)
		if e != nil {
			return e
		}
		region = b.region
		return nil
	})
	if err != nil {
		return err
	}
	if region == "us-east-1" {
		region = ""
	}
	writeXML(w, http.StatusOK, locationResult{XMLNS: s3NS, Value: region})
	return nil
}

type xmlBucket struct {
	Name         string
	CreationDate string
	BucketRegion string `xml:",omitempty"`
}

type listBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	XMLNS   string   `xml:"xmlns,attr"`
	Owner   struct {
		ID          string
		DisplayName string
	}
	Buckets           []xmlBucket `xml:"Buckets>Bucket"`
	ContinuationToken string      `xml:",omitempty"`
	Prefix            string      `xml:",omitempty"`
}

// serveService handles requests without a bucket: only ListBuckets. Like S3,
// BucketRegion is reported only when the request has a parameter, and
// max-buckets, continuation-token, prefix and bucket-region are honoured.
func (s *Server) serveService(ctx context.Context, w http.ResponseWriter, r *http.Request, info reqInfo, q map[string][]string) error {
	if r.Method != http.MethodGet {
		return s3err(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
	}
	info.op = OpListBuckets
	info.rangeStart = -1
	maxBuckets := 10000
	if v := first(q["max-buckets"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			return s3err(http.StatusBadRequest, "InvalidArgument", "max-buckets must be an integer between 1 and 10000")
		}
		maxBuckets = n
	}
	after := ""
	if tok := first(q["continuation-token"]); tok != "" {
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			return s3err(http.StatusBadRequest, "InvalidArgument", "The continuation token provided is incorrect")
		}
		after = string(raw)
	}
	prefix, regionFilter := first(q["prefix"]), first(q["bucket-region"])
	withRegion := len(q) > 0 && !s.hideRegion
	res := listBucketsResult{XMLNS: s3NS, Prefix: prefix}
	res.Owner.ID, res.Owner.DisplayName = "fakes3owner", "fakes3"
	err := s.core.exec(ctx, &info, nil, func(io.Reader) error {
		s.core.mu.Lock()
		defer s.core.mu.Unlock()
		names := make([]string, 0, len(s.core.buckets))
		for name := range s.core.buckets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			b := s.core.buckets[name]
			if name <= after || !strings.HasPrefix(name, prefix) || (regionFilter != "" && b.region != regionFilter) {
				continue
			}
			if len(res.Buckets) == maxBuckets {
				res.ContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(res.Buckets[len(res.Buckets)-1].Name))
				break
			}
			xb := xmlBucket{Name: name, CreationDate: isoTime(b.created)}
			if withRegion {
				xb.BucketRegion = b.region
			}
			res.Buckets = append(res.Buckets, xb)
		}
		return nil
	})
	if err != nil {
		return err
	}
	writeXML(w, http.StatusOK, res)
	return nil
}
