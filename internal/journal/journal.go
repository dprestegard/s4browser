// Package journal is the crash-safe record of remote and local work that must
// be cleaned up if the app dies: multipart upload IDs and download temp files.
//
// The journal never contains secrets. Entry is an allowlist, and a test fails
// if a field is added without review.
package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// On-disk names, all inside the app's config directory.
const (
	// FileName is the committed journal.
	FileName = "journal.json"
	// TempFileName receives the next snapshot before it is renamed over FileName.
	TempFileName = "journal.json.tmp"
	// LockFileName is held exclusively for the life of the process.
	LockFileName = "journal.lock"
	// CorruptPrefix starts the name a corrupt journal is moved to.
	CorruptPrefix = "journal.corrupt-"
	// FormatVersion is the current File.Version.
	FormatVersion = 1
)

// Intent matching window. See MatchesIntent.
const (
	IntentSkewBefore = 5 * time.Minute
	IntentWindow     = 1 * time.Hour
)

// Sentinel errors.
var (
	// ErrLocked means another process holds the journal.
	ErrLocked = errors.New("journal: locked by another process")
	// ErrCorrupt means the journal cannot be parsed.
	ErrCorrupt = errors.New("journal: corrupt")
	// ErrUnsupportedVersion means the journal was written by a newer build.
	ErrUnsupportedVersion = errors.New("journal: unsupported version")
	// ErrInvalidEntry means an entry violates its kind's field rules.
	ErrInvalidEntry = errors.New("journal: invalid entry")
	// ErrUploadGone is returned by RemoteCleaner when the upload no longer exists.
	ErrUploadGone = errors.New("journal: upload no longer exists")
	// ErrCredentials is returned by RemoteCleaner when cleanup needs credentials
	// that are not available yet. The entry is kept.
	ErrCredentials = errors.New("journal: credentials unavailable")
)

// Kind is what an entry tracks.
type Kind string

const (
	// KindMultipartUpload tracks a remote multipart upload.
	KindMultipartUpload Kind = "multipart-upload"
	// KindDownloadTemp tracks a local download temp file.
	KindDownloadTemp Kind = "download-temp"
)

// Phase is how far an entry's work has progressed.
type Phase string

const (
	// PhaseCreating is written before CreateMultipartUpload is sent.
	PhaseCreating Phase = "creating"
	// PhaseActive is written once the upload ID is known, or for a temp file.
	PhaseActive Phase = "active"
)

// Entry is one piece of work to clean up after a crash. Every field is
// reviewed for secrecy: Profile is a profile name, never a key.
type Entry struct {
	// ID is unique per job run: "<job id>-<run number>".
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Phase     Phase     `json:"phase"`
	CreatedAt time.Time `json:"createdAt"`

	// Multipart upload fields.
	Profile  string `json:"profile,omitempty"`
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Key      string `json:"key,omitempty"`
	UploadID string `json:"uploadId,omitempty"`

	// TempPath is the absolute path of a download temp file.
	TempPath string `json:"tempPath,omitempty"`
}

// Validate checks the field rules for the entry's kind and phase.
func (e Entry) Validate() error {
	fail := func(reason string) error {
		return fmt.Errorf("%w: %q: %s", ErrInvalidEntry, e.ID, reason)
	}
	if e.ID == "" || e.CreatedAt.IsZero() {
		return fail("ID and CreatedAt are required")
	}
	switch e.Kind {
	case KindMultipartUpload:
		if e.Region == "" || e.Bucket == "" || e.Key == "" || e.TempPath != "" {
			return fail("upload needs region, bucket and key, and no temp path")
		}
		switch e.Phase {
		case PhaseCreating:
			if e.UploadID != "" {
				return fail("creating entry cannot have an upload ID")
			}
		case PhaseActive:
			if e.UploadID == "" {
				return fail("active entry needs an upload ID")
			}
		default:
			return fail("unknown phase")
		}
	case KindDownloadTemp:
		if e.Phase != PhaseActive || e.TempPath == "" {
			return fail("temp entry must be active with a temp path")
		}
		if e.Profile != "" || e.Region != "" || e.Bucket != "" || e.Key != "" || e.UploadID != "" {
			return fail("temp entry cannot have upload fields")
		}
	default:
		return fail("unknown kind")
	}
	return nil
}

// File is the JSON document stored in FileName.
type File struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Encode returns the file content for entries, sorted by ID.
func Encode(entries []Entry) ([]byte, error) {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })
	for _, entry := range sorted {
		if err := entry.Validate(); err != nil {
			return nil, err
		}
	}
	if sorted == nil {
		sorted = []Entry{}
	}
	return json.MarshalIndent(File{Version: FormatVersion, Entries: sorted}, "", "  ")
}

// Decode parses file content. Unknown fields are rejected, so a journal holding
// anything outside the allowlist is treated as corrupt.
func Decode(data []byte) ([]Entry, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var file File
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data", ErrCorrupt)
	}
	if file.Version != FormatVersion {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedVersion, file.Version)
	}
	seen := make(map[string]struct{}, len(file.Entries))
	for _, entry := range file.Entries {
		if err := entry.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		if _, dup := seen[entry.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate entry %q", ErrCorrupt, entry.ID)
		}
		seen[entry.ID] = struct{}{}
	}
	return file.Entries, nil
}

// RemoteUpload is one in-progress upload reported by S3.
type RemoteUpload struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// MatchesIntent reports whether a listed upload may be the one started by a
// PhaseCreating entry whose upload ID was never recorded. Only an exact key
// initiated inside [CreatedAt-IntentSkewBefore, CreatedAt+IntentWindow] matches,
// so unrelated uploads are not aborted.
func MatchesIntent(e Entry, u RemoteUpload) bool {
	if e.Kind != KindMultipartUpload || e.Phase != PhaseCreating || u.Key != e.Key {
		return false
	}
	earliest := e.CreatedAt.Add(-IntentSkewBefore)
	latest := e.CreatedAt.Add(IntentWindow)
	return !u.Initiated.Before(earliest) && !u.Initiated.After(latest)
}

// Store is the durable set of entries. Implementations serialize access and may
// group concurrent calls into one snapshot write.
type Store interface {
	// Put inserts or replaces an entry. It returns after the change is durable.
	Put(ctx context.Context, entry Entry) error
	// Delete removes an entry, and is durable on return. Unknown IDs are ignored.
	Delete(ctx context.Context, id string) error
	// List returns all entries sorted by ID.
	List() []Entry
	// Close flushes and releases the process lock.
	Close() error
}

// RemoteCleaner performs the S3 side of launch cleanup.
type RemoteCleaner interface {
	// AbortUpload aborts the entry's upload, or returns ErrUploadGone or ErrCredentials.
	AbortUpload(ctx context.Context, e Entry, uploadID string) error
	// ListUploads returns in-progress uploads whose key begins with e.Key.
	ListUploads(ctx context.Context, e Entry) ([]RemoteUpload, error)
}

// Report summarizes one cleanup pass.
type Report struct {
	UploadsAborted int
	TempsRemoved   int
	// Deferred counts entries kept for a later pass.
	Deferred int
	// Warnings are shown to the user.
	Warnings []string
}

// Recoverer runs launch cleanup in the background. It is implemented in stage 2.1.
type Recoverer interface {
	Recover(ctx context.Context) (Report, error)
}
