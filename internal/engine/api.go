package engine

import (
	"context"
	"time"

	"s4browser/internal/journal"
)

// JobID identifies a job. It is 16 lowercase hex characters.
type JobID string

// Direction says which way a job moves data.
type Direction uint8

const (
	// Upload copies a local file to S3.
	Upload Direction = iota
	// Download copies an S3 object to a local file.
	Download
)

// JobSpec describes one file transfer.
type JobSpec struct {
	Direction Direction
	Ref       ObjectRef
	// LocalPath is the upload source, or the validated download destination.
	LocalPath string
}

// JobError is the user-facing failure of a job. It holds no secrets.
type JobError struct {
	Class   Class
	Code    string
	Message string
	// PartNumber is the failing part, or 0 for a job-level failure.
	PartNumber int32
}

// JobSnapshot is a consistent copy of one job's status.
type JobSnapshot struct {
	ID         JobID
	Spec       JobSpec
	State      JobState
	Phase      JobPhase
	TotalBytes int64
	// DoneBytes counts bytes in finished parts plus in-flight progress. It can
	// fall when a failed attempt's bytes are rolled back.
	DoneBytes   int64
	BytesPerSec int64
	PartsDone   int
	PartsTotal  int
	// Retries counts retryable failures in this run.
	Retries int
	Err     *JobError
	// Warning reports a non-fatal problem, such as an abort deferred to next launch.
	Warning string
}

// Transition records one job state change.
type Transition struct {
	ID   JobID
	From JobState
	To   JobState
	At   time.Time
}

// ProgressBatch is delivered once per ProgressInterval when anything changed.
type ProgressBatch struct {
	// Seq increases by one with every batch.
	Seq uint64
	At  time.Time
	// Jobs holds the jobs that changed since the previous batch.
	Jobs []JobSnapshot
	// Transitions holds state changes in order.
	Transitions []Transition
	// Resync means transitions were dropped, so the receiver must call Snapshot.
	Resync bool
}

// Observer receives progress on the engine's progress goroutine. A slow
// Observer delays later batches but never slows the data path.
type Observer interface {
	OnProgress(batch ProgressBatch)
}

// Config supplies the engine's dependencies. Stage 0.4 adds
// func New(cfg Config) (Engine, error), which rejects nil required fields.
type Config struct {
	Store    ObjectStore      // Required.
	Source   Source           // Required.
	Sink     Sink             // Required.
	Journal  journal.Store    // Required.
	Prober   CredentialProber // Required.
	Clock    Clock            // Required.
	Observer Observer         // Optional.
	// Guard is checked before every remote write. Nil allows all keys, and test
	// tooling must always set it.
	Guard WriteGuard
	// Jitter returns values in [0,1). It is called only on the scheduler goroutine.
	Jitter   func() float64 // Required.
	Settings Settings
	Retry    RetryPolicy
	Timeouts Timeouts
	// SendBuffer is how many bytes the transport can still hold once an upload
	// body has been read: the socket send buffer. The watchdog allows time for
	// it to drain before Timeouts.Response starts. 0 means DefaultSendBuffer.
	SendBuffer int64
}

// Engine is the transfer engine. All methods are safe for concurrent use.
// Commands are applied in order by the scheduler goroutine and return once
// applied. After Close they return ErrEngineClosed.
type Engine interface {
	// Submit queues a job and returns its ID.
	Submit(ctx context.Context, spec JobSpec) (JobID, error)
	// Pause, Resume, Cancel and Retry apply the matching JobEvent and return
	// ErrIllegalState when the state machine rejects it.
	Pause(ctx context.Context, id JobID) error
	Resume(ctx context.Context, id JobID) error
	Cancel(ctx context.Context, id JobID) error
	Retry(ctx context.Context, id JobID) error
	// Remove forgets a job in a terminal state.
	Remove(ctx context.Context, id JobID) error
	// UpdateSettings applies new limits. Concurrency limits apply at the next
	// dispatch and never interrupt running attempts. Bandwidth limits apply
	// immediately. PartSize applies to jobs not yet planned.
	UpdateSettings(ctx context.Context, s Settings) error
	// NotifyCredentialsChanged runs a probe for profile now.
	NotifyCredentialsChanged(profile string)
	// Snapshot returns the status of all jobs in submission order.
	Snapshot(ctx context.Context) ([]JobSnapshot, error)
	// Close stops the engine in the order given in docs/engine-design.md. It
	// returns when all engine goroutines have exited, or with ctx's error.
	Close(ctx context.Context) error
}
