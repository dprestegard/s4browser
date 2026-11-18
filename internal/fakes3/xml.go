package fakes3

import (
	"encoding/xml"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"s4browser/internal/engine"
)

const (
	xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
	s3NS      = "http://s3.amazonaws.com/doc/2006-03-01/"
)

func xmlEncoder(w io.Writer) *xml.Encoder { return xml.NewEncoder(w) }

func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string
	Key      string
	UploadID string `xml:"UploadId"`
}

func newInitiate(bucket, key, id string) initiateResult {
	return initiateResult{XMLNS: s3NS, Bucket: bucket, Key: key, UploadID: id}
}

type completeResult struct {
	XMLName       xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS         string   `xml:"xmlns,attr"`
	Location      string
	Bucket        string
	Key           string
	ETag          string
	ChecksumCRC32 string `xml:",omitempty"`
	ChecksumType  string `xml:",omitempty"`
}

type completeRequest struct {
	Parts []struct {
		PartNumber    int32
		ETag          string
		ChecksumCRC32 string
	} `xml:"Part"`
}

func decodeCompleteRequest(r io.Reader) ([]engine.CompletedPart, error) {
	var req completeRequest
	if err := xml.NewDecoder(r).Decode(&req); err != nil {
		return nil, err
	}
	parts := make([]engine.CompletedPart, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = engine.CompletedPart{Number: p.PartNumber, ETag: p.ETag, ChecksumCRC32: p.ChecksumCRC32}
	}
	return parts, nil
}

type xmlContents struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
}

type xmlPrefix struct{ Prefix string }

type listBucketResult struct {
	XMLName        xml.Name `xml:"ListBucketResult"`
	XMLNS          string   `xml:"xmlns,attr"`
	Name           string
	Prefix         string
	Delimiter      string `xml:",omitempty"`
	MaxKeys        int
	EncodingType   string `xml:",omitempty"`
	KeyCount       int
	IsTruncated    bool
	StartAfter     string `xml:",omitempty"`
	ContinueToken  string `xml:"ContinuationToken,omitempty"`
	NextToken      string `xml:"NextContinuationToken,omitempty"`
	Contents       []xmlContents
	CommonPrefixes []xmlPrefix
}

func buildListObjects(info reqInfo, in listObjectsIn, out listObjectsOut, urlEncode bool) listBucketResult {
	enc := func(s string) string { return s }
	res := listBucketResult{XMLNS: s3NS, Name: info.bucket, MaxKeys: in.maxKeys, IsTruncated: out.truncated,
		ContinueToken: in.token, NextToken: out.next}
	if res.MaxKeys <= 0 || res.MaxKeys > 1000 {
		res.MaxKeys = 1000
	}
	if urlEncode {
		res.EncodingType = "url"
		enc = func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "%2F", "/") }
	}
	res.Prefix, res.Delimiter, res.StartAfter = enc(in.prefix), enc(in.delimiter), enc(in.startAfter)
	for _, o := range out.objects {
		res.Contents = append(res.Contents, xmlContents{Key: enc(o.key), LastModified: isoTime(o.modified), ETag: o.etag, Size: o.size, StorageClass: "STANDARD"})
	}
	for _, p := range out.prefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, xmlPrefix{Prefix: enc(p)})
	}
	res.KeyCount = len(out.objects) + len(out.prefixes)
	return res
}

type xmlUpload struct {
	Key               string
	UploadID          string `xml:"UploadId"`
	StorageClass      string
	Initiated         string
	ChecksumAlgorithm string `xml:",omitempty"`
}

type listUploadsResult struct {
	XMLName            xml.Name `xml:"ListMultipartUploadsResult"`
	XMLNS              string   `xml:"xmlns,attr"`
	Bucket             string
	KeyMarker          string
	UploadIDMarker     string `xml:"UploadIdMarker"`
	NextKeyMarker      string `xml:",omitempty"`
	NextUploadIDMarker string `xml:"NextUploadIdMarker,omitempty"`
	Prefix             string
	MaxUploads         int
	IsTruncated        bool
	Uploads            []xmlUpload `xml:"Upload"`
}

func buildListUploads(info reqInfo, in listUploadsIn, out listUploadsOut) listUploadsResult {
	res := listUploadsResult{XMLNS: s3NS, Bucket: info.bucket, KeyMarker: in.keyMarker, UploadIDMarker: in.idMarker,
		Prefix: in.prefix, MaxUploads: in.maxUploads, IsTruncated: out.truncated,
		NextKeyMarker: out.nextKey, NextUploadIDMarker: out.nextUploadID}
	if res.MaxUploads <= 0 || res.MaxUploads > 1000 {
		res.MaxUploads = 1000
	}
	for _, u := range out.uploads {
		res.Uploads = append(res.Uploads, xmlUpload{Key: u.key, UploadID: u.id, StorageClass: "STANDARD", Initiated: isoTime(u.created)})
	}
	return res
}

type xmlPart struct {
	PartNumber    int32
	LastModified  string
	ETag          string
	Size          int64
	ChecksumCRC32 string `xml:",omitempty"`
}

type listPartsResult struct {
	XMLName              xml.Name `xml:"ListPartsResult"`
	XMLNS                string   `xml:"xmlns,attr"`
	Bucket               string
	Key                  string
	UploadID             string `xml:"UploadId"`
	PartNumberMarker     int32
	NextPartNumberMarker int32 `xml:",omitempty"`
	MaxParts             int
	IsTruncated          bool
	Parts                []xmlPart `xml:"Part"`
	StorageClass         string
	ChecksumAlgorithm    string `xml:",omitempty"`
	ChecksumType         string `xml:",omitempty"`
}

func buildListParts(info reqInfo, in listPartsIn, out listPartsOut) listPartsResult {
	res := listPartsResult{XMLNS: s3NS, Bucket: info.bucket, Key: info.key, UploadID: info.uploadID,
		PartNumberMarker: in.marker, NextPartNumberMarker: out.next, MaxParts: in.max, IsTruncated: out.truncated, StorageClass: "STANDARD"}
	if res.MaxParts <= 0 || res.MaxParts > 1000 {
		res.MaxParts = 1000
	}
	if out.crcAlgo {
		res.ChecksumAlgorithm, res.ChecksumType = "CRC32", "COMPOSITE"
	}
	for _, p := range out.parts {
		res.Parts = append(res.Parts, xmlPart{PartNumber: p.number, LastModified: isoTime(p.modified), ETag: p.etag, Size: p.size, ChecksumCRC32: p.crc})
	}
	return res
}

var _ = strconv.Itoa
