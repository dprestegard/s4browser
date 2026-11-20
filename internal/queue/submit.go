package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"s4browser/internal/engine"
	"s4browser/internal/fsx"
)

// maxKeyBytes is S3's key length limit.
const maxKeyBytes = 1024

// Limits on what one SubmitItem lists. A folder of a million files must not
// produce a million-entry reply: the counts are always exact, the lists are the
// first entries only.
const (
	// MaxListedRefusals is the most refusals listed in one item.
	MaxListedRefusals = 200
	// MaxListedJobs is the most job IDs listed in one folder item.
	MaxListedJobs = 1000
)

// regionTimeout bounds the region lookup that a request without a region makes.
const regionTimeout = 75 * time.Second

// Item kinds.
const (
	KindFile   = "file"   // An uploaded local file.
	KindFolder = "folder" // An uploaded local folder, expanded to one job per file.
	KindObject = "object" // A downloaded object.
	KindPrefix = "prefix" // A downloaded folder (key prefix), expanded to one job per object.
)

// Refusal codes that are not fsx path codes (those are in package fsx).
const (
	CodeRelative   = "relative"   // The local path is not absolute.
	CodeUnreadable = "unreadable" // A file or folder cannot be read.
	CodeNotFound   = "not-found"  // The local path does not exist (any more).
	CodeNotRegular = "not-regular"
	CodeKeyTooLong = "key-too-long"
	CodeDuplicate  = "duplicate" // Two dropped items would upload to the same key.
	CodeFolderKey  = "folder-key"
	CodeStopped    = "stopped" // The request ended before the item was finished.
	CodeFolderRoot = "folder-root"
	CodeListing    = "listing" // S3 could not list the prefix.
)

// UploadRequest asks for local files and folders to be uploaded to one bucket. A
// folder becomes one job per file, under "<prefix><folder name>/<relative path>".
type UploadRequest struct {
	ProfileID string `json:"profileId"`
	Bucket    string `json:"bucket"`
	// Region is the bucket's region. Empty means look it up.
	Region string `json:"region"`
	// Prefix is the destination folder in the bucket: empty for the top level, or
	// "a/b/" (a missing trailing slash is added).
	Prefix string `json:"prefix"`
	// Paths are absolute local paths of files and folders, as the file drop
	// delivers them.
	Paths []string `json:"paths"`
}

// DownloadRequest asks for objects and folders to be downloaded into one local
// folder. A key ending in "/" is a folder: every object below it is downloaded to
// "<DestDir>\<folder name>\<relative path>". Any other key is one object, which
// is saved as "<DestDir>\<last name of the key>".
type DownloadRequest struct {
	ProfileID string   `json:"profileId"`
	Bucket    string   `json:"bucket"`
	Region    string   `json:"region"`
	Keys      []string `json:"keys"`
	// DestDir is an absolute, existing folder.
	DestDir string `json:"destDir"`
}

// Refusal is one file or object that was not queued, and why.
type Refusal struct {
	// Path is the local path of an upload, or the object key of a download.
	Path string `json:"path"`
	// Code is a stable short name: an fsx code such as "reserved-name",
	// "collision", "exists", "traversal" or "too-long", or one of the Code
	// constants of this package.
	Code string `json:"code"`
	// Reason is a sentence for the user.
	Reason string `json:"reason"`
}

// SubmitItem is the outcome for one requested path or key.
//
// A file or object is either queued (JobID set, Queued 1) or refused (Error and
// Code set). A folder or prefix is expanded: Queued counts the jobs created, and
// every file or object that was not queued is in Refused with its reason. Error
// is set on a folder only when the folder as a whole could not be processed or
// the request stopped early (the jobs queued so far remain).
type SubmitItem struct {
	// Source is the path or key as requested.
	Source string `json:"source"`
	// Kind is file, folder, object or prefix.
	Kind  string `json:"kind"`
	JobID string `json:"jobId,omitempty"`
	Error string `json:"error,omitempty"`
	// Code names Error, using the same codes as Refusal.
	Code string `json:"code,omitempty"`
	// Queued is the number of jobs this item created.
	Queued int `json:"queued"`
	// JobIDs lists the jobs of a folder, at most MaxListedJobs.
	JobIDs []string `json:"jobIds,omitempty"`
	// Refused lists what was not queued, at most MaxListedRefusals. RefusedTotal
	// is the exact number.
	Refused      []Refusal `json:"refused,omitempty"`
	RefusedTotal int       `json:"refusedTotal"`
	// Examined counts the files or objects looked at, queued or not.
	Examined int `json:"examined"`
	// EmptyFolders counts local folders with nothing in them, which an upload
	// skips because S3 has no folders to create.
	EmptyFolders int `json:"emptyFolders,omitempty"`
	// Placeholders counts zero-byte "folder/" objects a download skipped.
	Placeholders int `json:"placeholders,omitempty"`
}

func (it *SubmitItem) fail(code, format string, args ...any) {
	it.Error, it.Code = fmt.Sprintf(format, args...), code
}

func (it *SubmitItem) refuse(path, code, reason string) {
	it.RefusedTotal++
	if len(it.Refused) < MaxListedRefusals {
		it.Refused = append(it.Refused, Refusal{Path: path, Code: code, Reason: reason})
	}
}

func (it *SubmitItem) queued(id engine.JobID) {
	it.Queued++
	if it.Kind == KindFolder || it.Kind == KindPrefix {
		if len(it.JobIDs) < MaxListedJobs {
			it.JobIDs = append(it.JobIDs, string(id))
		}
	} else {
		it.JobID = string(id)
	}
}

// SubmitResult has one item per requested path or key, in request order. One
// refused item does not stop the others.
type SubmitResult struct {
	Items []SubmitItem `json:"items"`
}

// Queued counts the jobs created, folders included.
func (r SubmitResult) Queued() int {
	n := 0
	for _, it := range r.Items {
		n += it.Queued
	}
	return n
}

// Refused counts the items and files that were refused: a refused path or key
// counts once, a folder counts each file it refused.
func (r SubmitResult) Refused() int {
	n := 0
	for _, it := range r.Items {
		n += it.RefusedTotal
		if it.Error != "" {
			n++
		}
	}
	return n
}

// SubmitUploads queues one upload per file, expanding folders.
func (s *Service) SubmitUploads(ctx context.Context, req UploadRequest) (SubmitResult, error) {
	if s.uploadsOff != "" {
		return SubmitResult{}, fmt.Errorf("uploads are turned off: %s", s.uploadsOff)
	}
	region, err := s.target(ctx, req.ProfileID, req.Bucket, req.Region)
	if err != nil {
		return SubmitResult{}, err
	}
	prefix := req.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	ref := engine.ObjectRef{Profile: req.ProfileID, Region: region, Bucket: req.Bucket}
	submit := func(local, key string) (engine.JobID, error) {
		r := ref
		r.Key = key
		return s.eng.Submit(ctx, engine.JobSpec{Direction: engine.Upload, Ref: r, LocalPath: local})
	}
	res := SubmitResult{Items: make([]SubmitItem, 0, len(req.Paths))}
	taken := map[string]struct{}{} // Top-level keys used, so two dropped items cannot share one.
	for _, p := range req.Paths {
		item := SubmitItem{Source: p}
		err := uploadPath(ctx, &item, prefix, p, taken, osLister{}, submit)
		res.Items = append(res.Items, item)
		if errors.Is(err, engine.ErrEngineClosed) {
			return res, describe(err)
		}
	}
	return res, nil
}

// uploadPath handles one dropped path. Only an engine-closed error is returned:
// everything else is recorded in item.
func uploadPath(ctx context.Context, item *SubmitItem, prefix, local string, taken map[string]struct{}, fsys dirLister, submit func(local, key string) (engine.JobID, error)) error {
	if !filepath.IsAbs(local) {
		item.fail(CodeRelative, "the path is not absolute")
		return nil
	}
	local = filepath.Clean(local)
	info, err := os.Stat(local)
	switch {
	case errors.Is(err, os.ErrNotExist):
		item.fail(CodeNotFound, "cannot read it: it no longer exists")
		return nil
	case err != nil:
		item.fail(CodeUnreadable, "cannot read it: %v", unwrapPath(err))
		return nil
	case info.IsDir():
		item.Kind = KindFolder
		return uploadFolder(ctx, item, prefix, local, taken, fsys, submit)
	case !info.Mode().IsRegular():
		item.Kind = KindFile
		item.fail(CodeNotRegular, "only regular files and folders can be uploaded")
		return nil
	}
	item.Kind = KindFile
	name := filepath.Base(local)
	if !utf8.ValidString(name) {
		item.fail(fsx.CodeInvalidText, "the file name is not valid text")
		return nil
	}
	key := prefix + name
	if len(key) > maxKeyBytes {
		item.fail(CodeKeyTooLong, "the object name would be longer than S3's limit of %d bytes", maxKeyBytes)
		return nil
	}
	if _, dup := taken["f:"+key]; dup {
		item.fail(CodeDuplicate, "another dropped item is also uploaded as %q", key)
		return nil
	}
	taken["f:"+key] = struct{}{}
	id, err := submit(local, key)
	if err != nil {
		if errors.Is(err, engine.ErrEngineClosed) {
			return err
		}
		item.fail(CodeStopped, "%v", err)
		return nil
	}
	item.Examined = 1
	item.queued(id)
	return nil
}

func uploadFolder(ctx context.Context, item *SubmitItem, prefix, local string, taken map[string]struct{}, fsys dirLister, submit func(local, key string) (engine.JobID, error)) error {
	if filepath.Dir(local) == local {
		item.fail(CodeFolderRoot, "a drive or network share has no folder name to upload under. Choose a folder inside it")
		return nil
	}
	name := filepath.Base(local)
	if !utf8.ValidString(name) {
		item.fail(fsx.CodeInvalidText, "the folder name is not valid text")
		return nil
	}
	base := prefix + name + "/"
	if len(base) >= maxKeyBytes {
		item.fail(CodeKeyTooLong, "the object names would be longer than S3's limit of %d bytes", maxKeyBytes)
		return nil
	}
	if _, dup := taken["d:"+base]; dup {
		item.fail(CodeDuplicate, "another dropped folder is also uploaded as %q", base)
		return nil
	}
	taken["d:"+base] = struct{}{}
	// A dropped link to a folder is followed once: the user chose it. Links inside
	// the tree are not.
	root := local
	if info, err := os.Lstat(local); err == nil && info.Mode()&os.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(local)
		if err != nil {
			item.fail(CodeUnreadable, "cannot read the folder: %v", unwrapPath(err))
			return nil
		}
		root = real
	}
	w := &walker{ctx: ctx, item: item, fsys: fsys, root: root, base: base, submit: submit}
	err := w.walk("")
	switch {
	case errors.Is(err, engine.ErrEngineClosed):
		return err
	case err != nil && item.Error == "":
		item.fail(CodeStopped, "stopped after %d files: %v", item.Examined, err)
	}
	return nil
}

// SubmitDownloads queues one download per object, expanding folders.
func (s *Service) SubmitDownloads(ctx context.Context, req DownloadRequest) (SubmitResult, error) {
	region, err := s.target(ctx, req.ProfileID, req.Bucket, req.Region)
	if err != nil {
		return SubmitResult{}, err
	}
	placer, err := fsx.NewPlacer(req.DestDir)
	if err != nil {
		if fsx.CodeOf(err) != "" {
			return SubmitResult{}, errors.New("the destination folder is not an absolute path")
		}
		return SubmitResult{}, err
	}
	ref := engine.ObjectRef{Profile: req.ProfileID, Region: region, Bucket: req.Bucket}
	submit := func(key, dest string) (engine.JobID, error) {
		r := ref
		r.Key = key
		return s.eng.Submit(ctx, engine.JobSpec{Direction: engine.Download, Ref: r, LocalPath: dest})
	}
	list := func(ctx context.Context, prefix string, fn func(key string, size int64) error) error {
		return s.listObjects(ctx, ref, prefix, fn)
	}
	res := SubmitResult{Items: make([]SubmitItem, 0, len(req.Keys))}
	for _, key := range req.Keys {
		item := SubmitItem{Source: key}
		err := downloadKey(ctx, &item, placer, key, list, submit)
		res.Items = append(res.Items, item)
		if errors.Is(err, engine.ErrEngineClosed) {
			return res, describe(err)
		}
	}
	return res, nil
}

// downloadKey handles one requested key. Only an engine-closed error is
// returned: everything else is recorded in item.
func downloadKey(ctx context.Context, item *SubmitItem, placer *fsx.Placer, key string, list objectLister, submit func(key, dest string) (engine.JobID, error)) error {
	if strings.HasSuffix(key, "/") {
		item.Kind = KindPrefix
		parts, err := fsx.SplitKey(strings.TrimSuffix(key, "/"))
		if err == nil {
			err = checkNames(parts)
		}
		if err != nil {
			item.fail(fsx.CodeOf(err), "%v", err)
			return nil
		}
		err = expandPrefix(ctx, item, placer, key, parts[len(parts)-1], list, submit)
		switch {
		case errors.Is(err, engine.ErrEngineClosed):
			return err
		case err != nil && item.Error == "":
			item.fail(listingCode(ctx, err), "stopped after %d objects: %v", item.Examined, err)
		}
		return nil
	}

	item.Kind = KindObject
	parts, err := fsx.SplitKey(key)
	if err == nil {
		err = checkNames(parts)
	}
	var dest string
	if err == nil {
		dest, err = placer.Place(parts[len(parts)-1:])
	}
	if err != nil {
		item.fail(fsx.CodeOf(err), "%v", err)
		return nil
	}
	id, err := submit(key, dest)
	if err != nil {
		if errors.Is(err, engine.ErrEngineClosed) {
			return err
		}
		item.fail(CodeStopped, "%v", err)
		return nil
	}
	item.Examined = 1
	item.queued(id)
	return nil
}

// checkNames applies the Windows name rules to every name of a key, so a key
// whose parent names are unsafe is refused even when only its last name is used.
func checkNames(parts []string) error {
	for _, p := range parts {
		if err := fsx.CheckName(p); err != nil {
			return err
		}
	}
	return nil
}

func listingCode(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return CodeStopped
	}
	return CodeListing
}

// target checks the profile and bucket and finds the bucket's region.
func (s *Service) target(ctx context.Context, profile, bucket, region string) (string, error) {
	if profile == "" {
		return "", errors.New("no profile is selected")
	}
	if bucket == "" {
		return "", errors.New("no bucket is selected")
	}
	if region != "" {
		return region, nil
	}
	if s.opts.Region == nil {
		return "", errors.New("the bucket's region is not known")
	}
	ctx, cancel := context.WithTimeout(ctx, regionTimeout)
	defer cancel()
	r, err := s.opts.Region(ctx, profile, bucket)
	if err != nil {
		return "", fmt.Errorf("cannot find the region of bucket %s: %w", bucket, err)
	}
	return r, nil
}

func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
