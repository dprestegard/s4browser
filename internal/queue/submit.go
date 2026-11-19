package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"s4browser/internal/engine"
)

// maxKeyBytes is S3's key length limit.
const maxKeyBytes = 1024

// UploadRequest asks for local files to be uploaded to one bucket. Stage 2.3
// adds folders: a folder path is expanded into one job per file, and the
// folder's name becomes a prefix. Until then a folder is refused item by item.
type UploadRequest struct {
	ProfileID string `json:"profileId"`
	Bucket    string `json:"bucket"`
	// Region is the bucket's region. Empty means look it up.
	Region string `json:"region"`
	// Prefix is the destination folder in the bucket: empty for the top level, or
	// "a/b/" (a missing trailing slash is added).
	Prefix string `json:"prefix"`
	// Paths are absolute local paths, as the file drop delivers them.
	Paths []string `json:"paths"`
}

// DownloadRequest asks for objects to be downloaded into one local folder. A key
// ending in "/" is a folder, which stage 2.3 will expand.
type DownloadRequest struct {
	ProfileID string   `json:"profileId"`
	Bucket    string   `json:"bucket"`
	Region    string   `json:"region"`
	Keys      []string `json:"keys"`
	// DestDir is an absolute, existing folder.
	DestDir string `json:"destDir"`
}

// SubmitItem is the outcome for one requested path or key. Exactly one of JobID
// and Error is set.
type SubmitItem struct {
	// Source is the path or key as requested.
	Source string `json:"source"`
	JobID  string `json:"jobId,omitempty"`
	Error  string `json:"error,omitempty"`
}

// SubmitResult has one item per requested path or key, in request order. One
// refused item does not stop the others.
type SubmitResult struct {
	Items []SubmitItem `json:"items"`
}

// Queued counts the items that became jobs.
func (r SubmitResult) Queued() int {
	n := 0
	for _, it := range r.Items {
		if it.JobID != "" {
			n++
		}
	}
	return n
}

// SubmitUploads queues one upload per file.
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
	res := SubmitResult{Items: make([]SubmitItem, 0, len(req.Paths))}
	for _, p := range req.Paths {
		item := SubmitItem{Source: p}
		id, err := s.submitUpload(ctx, engine.ObjectRef{Profile: req.ProfileID, Region: region, Bucket: req.Bucket}, prefix, p)
		if err != nil {
			if errors.Is(err, engine.ErrEngineClosed) {
				return res, describe(err)
			}
			item.Error = err.Error()
		}
		item.JobID = string(id)
		res.Items = append(res.Items, item)
	}
	return res, nil
}

func (s *Service) submitUpload(ctx context.Context, ref engine.ObjectRef, prefix, local string) (engine.JobID, error) {
	if !filepath.IsAbs(local) {
		return "", errors.New("the path is not absolute")
	}
	info, err := os.Stat(local)
	switch {
	case err != nil:
		return "", fmt.Errorf("cannot read the file: %w", unwrapPath(err))
	case info.IsDir():
		return "", errors.New("folders cannot be uploaded yet")
	case !info.Mode().IsRegular():
		return "", errors.New("only regular files can be uploaded")
	}
	name := filepath.Base(local)
	if !utf8.ValidString(name) {
		return "", errors.New("the file name is not valid text")
	}
	ref.Key = prefix + name
	if len(ref.Key) > maxKeyBytes {
		return "", fmt.Errorf("the object name would be longer than S3's limit of %d bytes", maxKeyBytes)
	}
	id, err := s.eng.Submit(ctx, engine.JobSpec{Direction: engine.Upload, Ref: ref, LocalPath: local})
	return id, err
}

// SubmitDownloads queues one download per object.
func (s *Service) SubmitDownloads(ctx context.Context, req DownloadRequest) (SubmitResult, error) {
	region, err := s.target(ctx, req.ProfileID, req.Bucket, req.Region)
	if err != nil {
		return SubmitResult{}, err
	}
	if !filepath.IsAbs(req.DestDir) {
		return SubmitResult{}, errors.New("the destination folder is not an absolute path")
	}
	if info, err := os.Stat(req.DestDir); err != nil || !info.IsDir() {
		return SubmitResult{}, fmt.Errorf("the destination %q is not a folder that exists", req.DestDir)
	}
	res := SubmitResult{Items: make([]SubmitItem, 0, len(req.Keys))}
	for _, key := range req.Keys {
		item := SubmitItem{Source: key}
		id, err := s.submitDownload(ctx, engine.ObjectRef{Profile: req.ProfileID, Region: region, Bucket: req.Bucket, Key: key}, req.DestDir)
		if err != nil {
			if errors.Is(err, engine.ErrEngineClosed) {
				return res, describe(err)
			}
			item.Error = err.Error()
		}
		item.JobID = string(id)
		res.Items = append(res.Items, item)
	}
	return res, nil
}

func (s *Service) submitDownload(ctx context.Context, ref engine.ObjectRef, destDir string) (engine.JobID, error) {
	if strings.HasSuffix(ref.Key, "/") {
		return "", errors.New("folders cannot be downloaded yet")
	}
	name, err := localName(ref.Key)
	if err != nil {
		return "", err
	}
	return s.eng.Submit(ctx, engine.JobSpec{Direction: engine.Download, Ref: ref, LocalPath: filepath.Join(destDir, name)})
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

// reservedNames are Windows device names, which cannot be file names.
var reservedNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

// localName returns the file name a key downloads to, or an error. It refuses a
// name instead of changing it. This is the minimum that keeps a single download
// inside the chosen folder; stage 2.3 owns the full Windows rules and the
// handling of whole folders.
func localName(key string) (string, error) {
	name := path.Base(key)
	switch {
	case key == "" || name == "." || name == "..":
		return "", fmt.Errorf("the object name %q cannot be a file name", key)
	case strings.ContainsAny(name, `<>:"\|?*`) || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", fmt.Errorf("the object name %q has characters Windows does not allow in a file name", name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return "", fmt.Errorf("the object name %q ends with a dot or a space, which Windows does not allow", name)
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if reservedNames[stem] || (len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9') {
		return "", fmt.Errorf("the object name %q is a reserved Windows device name", name)
	}
	if len(name) > 255 {
		return "", fmt.Errorf("the object name %q is too long for a file name", name)
	}
	return name, nil
}
