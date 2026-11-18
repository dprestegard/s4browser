package fakes3

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"s4browser/internal/engine"
	"s4browser/internal/journal"
)

// Store returns the in-process engine.ObjectStore. It calls the same operations
// as the HTTPS endpoint, with the same rules, faults, request log and state, but
// without a network stack, so a simulated 2 TB transfer needs no sockets.
func (s *Server) Store() engine.ObjectStore { return &memStore{s.core} }

type memStore struct{ c *core }

func infoOf(ref engine.ObjectRef) reqInfo {
	return reqInfo{bucket: ref.Bucket, key: ref.Key, rangeStart: -1, accessKey: AccessKeyID}
}

// opErr converts core errors into what an engine.ObjectStore returns.
func opErr(op Op, err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.toOpError(op, "fakes3-inproc")
	}
	return err
}

func (m *memStore) CreateMultipartUpload(ctx context.Context, ref engine.ObjectRef) (string, error) {
	id, err := m.c.createUpload(ctx, createIn{info: infoOf(ref), crcRequired: true})
	return id, opErr(OpCreateUpload, err)
}

func (m *memStore) UploadPart(ctx context.Context, req engine.UploadPartRequest) (engine.PutResult, error) {
	info := infoOf(req.Ref)
	info.uploadID, info.part = req.UploadID, req.PartNumber
	out, err := m.c.uploadPart(ctx, partIn{info: info, body: req.Body, contentLength: req.ContentLength, hasChecksum: true})
	return engine.PutResult{ETag: out.etag, ChecksumCRC32: out.crc}, opErr(OpUploadPart, err)
}

func (m *memStore) CompleteMultipartUpload(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
	info := infoOf(req.Ref)
	info.uploadID = req.UploadID
	out, err := m.c.completeUpload(ctx, completeIn{info: info, parts: req.Parts, objectSize: req.ObjectSize})
	if err != nil {
		return engine.CompleteUploadResult{}, opErr(OpComplete, err)
	}
	return engine.CompleteUploadResult{ETag: out.etag, ChecksumCRC32: out.crc}, nil
}

func (m *memStore) AbortMultipartUpload(ctx context.Context, ref engine.ObjectRef, uploadID string) error {
	info := infoOf(ref)
	info.uploadID = uploadID
	err := m.c.abortUpload(ctx, info)
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == "NoSuchUpload" {
		return nil
	}
	return opErr(OpAbort, err)
}

func (m *memStore) PutObject(ctx context.Context, req engine.PutObjectRequest) (engine.PutResult, error) {
	out, err := m.c.putObject(ctx, putIn{info: infoOf(req.Ref), body: req.Body, contentLength: req.ContentLength})
	return engine.PutResult{ETag: out.etag, ChecksumCRC32: out.crc}, opErr(OpPut, err)
}

func (m *memStore) HeadObject(ctx context.Context, ref engine.ObjectRef) (engine.ObjectInfo, error) {
	meta, err := m.c.headObject(ctx, infoOf(ref))
	if err != nil {
		return engine.ObjectInfo{}, opErr(OpHead, err)
	}
	return engine.ObjectInfo{Size: meta.size, ETag: meta.etag, ChecksumCRC32: meta.crc}, nil
}

func (m *memStore) GetObjectRange(ctx context.Context, req engine.GetRangeRequest) (engine.GetRangeResult, error) {
	if req.Length <= 0 || req.Offset < 0 {
		return engine.GetRangeResult{}, &engine.OpError{Op: string(OpGet), Class: engine.ClassPermanent, Err: engine.ErrInvalidSize}
	}
	in := getIn{info: infoOf(req.Ref), rng: &byteRange{start: req.Offset, end: req.Offset + req.Length - 1}, ifMatch: req.IfMatch}
	out, err := m.c.getObject(ctx, in)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusPreconditionFailed {
			return engine.GetRangeResult{}, &engine.OpError{
				Op: string(OpGet), Class: engine.ClassPermanent, Status: ae.Status, Code: ae.Code,
				RequestID: "fakes3-inproc", Err: fmt.Errorf("%w: %w", engine.ErrObjectChanged, ae),
			}
		}
		return engine.GetRangeResult{}, opErr(OpGet, err)
	}
	return engine.GetRangeResult{Body: out.body}, nil
}

// GuardStore wraps store so that every write is checked by guard first. A
// refused call returns an error wrapping engine.ErrWriteRefused without
// reaching store.
func GuardStore(store engine.ObjectStore, guard engine.WriteGuard) engine.ObjectStore {
	return guardedStore{store, guard}
}

type guardedStore struct {
	engine.ObjectStore
	guard engine.WriteGuard
}

func refused(op string, err error) error {
	return &engine.OpError{Op: op, Class: engine.ClassPermanent, Err: err}
}

func (g guardedStore) CreateMultipartUpload(ctx context.Context, ref engine.ObjectRef) (string, error) {
	if err := g.guard.CheckWrite(ref.Key); err != nil {
		return "", refused(string(OpCreateUpload), err)
	}
	return g.ObjectStore.CreateMultipartUpload(ctx, ref)
}

func (g guardedStore) UploadPart(ctx context.Context, req engine.UploadPartRequest) (engine.PutResult, error) {
	if err := g.guard.CheckWrite(req.Ref.Key); err != nil {
		return engine.PutResult{}, refused(string(OpUploadPart), err)
	}
	return g.ObjectStore.UploadPart(ctx, req)
}

func (g guardedStore) CompleteMultipartUpload(ctx context.Context, req engine.CompleteUploadRequest) (engine.CompleteUploadResult, error) {
	if err := g.guard.CheckWrite(req.Ref.Key); err != nil {
		return engine.CompleteUploadResult{}, refused(string(OpComplete), err)
	}
	return g.ObjectStore.CompleteMultipartUpload(ctx, req)
}

func (g guardedStore) AbortMultipartUpload(ctx context.Context, ref engine.ObjectRef, uploadID string) error {
	if err := g.guard.CheckWrite(ref.Key); err != nil {
		return refused(string(OpAbort), err)
	}
	return g.ObjectStore.AbortMultipartUpload(ctx, ref, uploadID)
}

func (g guardedStore) PutObject(ctx context.Context, req engine.PutObjectRequest) (engine.PutResult, error) {
	if err := g.guard.CheckWrite(req.Ref.Key); err != nil {
		return engine.PutResult{}, refused(string(OpPut), err)
	}
	return g.ObjectStore.PutObject(ctx, req)
}

// Cleaner returns a journal.RemoteCleaner backed by the fake, for launch-cleanup
// tests. Wrap it with GuardCleaner in test tooling.
func (s *Server) Cleaner() journal.RemoteCleaner { return cleaner{s.core} }

type cleaner struct{ c *core }

func (cl cleaner) AbortUpload(ctx context.Context, e journal.Entry, uploadID string) error {
	info := reqInfo{bucket: e.Bucket, key: e.Key, uploadID: uploadID, rangeStart: -1, accessKey: AccessKeyID}
	err := cl.c.abortUpload(ctx, info)
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.Code == "NoSuchUpload":
			return journal.ErrUploadGone
		case ae.Code == "ExpiredToken" || ae.Code == "InvalidAccessKeyId":
			return fmt.Errorf("%w: %w", journal.ErrCredentials, ae)
		}
	}
	return err
}

func (cl cleaner) ListUploads(ctx context.Context, e journal.Entry) ([]journal.RemoteUpload, error) {
	out, err := cl.c.listUploads(ctx, listUploadsIn{info: reqInfo{bucket: e.Bucket, rangeStart: -1, accessKey: AccessKeyID}, prefix: e.Key})
	if err != nil {
		return nil, err
	}
	res := make([]journal.RemoteUpload, len(out.uploads))
	for i, u := range out.uploads {
		res[i] = journal.RemoteUpload{Key: u.key, UploadID: u.id, Initiated: u.created}
	}
	return res, nil
}

// GuardCleaner wraps a cleaner so AbortUpload is checked by guard first.
func GuardCleaner(inner journal.RemoteCleaner, guard engine.WriteGuard) journal.RemoteCleaner {
	return guardedCleaner{inner, guard}
}

type guardedCleaner struct {
	journal.RemoteCleaner
	guard engine.WriteGuard
}

func (g guardedCleaner) AbortUpload(ctx context.Context, e journal.Entry, uploadID string) error {
	if err := g.guard.CheckWrite(e.Key); err != nil {
		return err
	}
	return g.RemoteCleaner.AbortUpload(ctx, e, uploadID)
}
