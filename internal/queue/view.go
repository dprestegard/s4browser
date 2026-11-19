package queue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"s4browser/internal/engine"
)

// JobView is one job as the UI shows it. It is what Jobs returns and what Event
// carries.
type JobView struct {
	ID string `json:"id"`
	// Direction is "upload" or "download".
	Direction string `json:"direction"`
	Profile   string `json:"profile"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	// LocalPath is the source file of an upload or the destination of a download.
	LocalPath string `json:"localPath"`
	// Name is the file name to show.
	Name string `json:"name"`
	// State is one of queued, running, pausing, paused, credential-wait,
	// cancelling, failing, cancelled, failed and completed.
	State string `json:"state"`
	// Phase is preparing, transferring or finalizing. It matters while running.
	Phase string `json:"phase"`
	// TotalBytes is 0 until the job has been prepared.
	TotalBytes int64 `json:"totalBytes"`
	// DoneBytes can fall when a failed attempt's bytes are taken back.
	DoneBytes   int64 `json:"doneBytes"`
	BytesPerSec int64 `json:"bytesPerSec"`
	// EtaSeconds is the time left at the current rate, or -1 when unknown.
	EtaSeconds int64 `json:"etaSeconds"`
	PartsDone  int   `json:"partsDone"`
	PartsTotal int   `json:"partsTotal"`
	// Retries counts retryable failures in this run.
	Retries int `json:"retries"`
	// Reason says why a job is waiting, or why it failed. It is empty while a job
	// runs or completes.
	Reason string `json:"reason,omitempty"`
	// ErrorCode is the S3 code of a failure, such as AccessDenied, when there is one.
	ErrorCode string `json:"errorCode,omitempty"`
	// Detail is the engine's full error text, for a details view or a bug report.
	// It holds no credentials.
	Detail string `json:"detail,omitempty"`
	// Warning is a non-fatal problem, such as an abort left for the next launch.
	Warning string `json:"warning,omitempty"`
	// Finished is true in a terminal state.
	Finished bool `json:"finished"`
	// The Can fields say which commands the job accepts now.
	CanPause  bool `json:"canPause"`
	CanResume bool `json:"canResume"`
	CanCancel bool `json:"canCancel"`
	CanRetry  bool `json:"canRetry"`
	CanRemove bool `json:"canRemove"`
}

var phaseNames = map[engine.JobPhase]string{
	engine.PhasePreparing:    "preparing",
	engine.PhaseTransferring: "transferring",
	engine.PhaseFinalizing:   "finalizing",
}

func directionName(d engine.Direction) string {
	if d == engine.Download {
		return "download"
	}
	return "upload"
}

// view maps an engine snapshot to a JobView.
func view(j engine.JobSnapshot) JobView {
	v := JobView{
		ID: string(j.ID), Direction: directionName(j.Spec.Direction), Profile: j.Spec.Ref.Profile,
		Bucket: j.Spec.Ref.Bucket, Key: j.Spec.Ref.Key, LocalPath: j.Spec.LocalPath, Name: filepath.Base(j.Spec.LocalPath),
		State: j.State.String(), Phase: phaseNames[j.Phase], TotalBytes: j.TotalBytes, DoneBytes: j.DoneBytes,
		BytesPerSec: j.BytesPerSec, EtaSeconds: -1, PartsDone: j.PartsDone, PartsTotal: j.PartsTotal,
		Retries: j.Retries, Warning: j.Warning, Finished: j.State.IsTerminal(),
	}
	if j.State == engine.JobRunning && j.BytesPerSec > 0 && j.TotalBytes > j.DoneBytes {
		v.EtaSeconds = (j.TotalBytes - j.DoneBytes + j.BytesPerSec - 1) / j.BytesPerSec
	}
	finalizing := j.Phase == engine.PhaseFinalizing
	switch j.State {
	case engine.JobQueued:
		v.Reason = "Waiting for a free transfer slot."
		v.CanPause, v.CanCancel = true, true
	case engine.JobRunning:
		v.CanPause, v.CanCancel = !finalizing, true
	case engine.JobPausing:
		v.Reason = "Pausing."
		v.CanCancel = true
	case engine.JobPaused:
		v.Reason = "Paused."
		v.CanResume, v.CanCancel = true, true
	case engine.JobCredentialWait:
		v.Reason = "Waiting for credentials for " + j.Spec.Ref.Profile + ". Sign in again and load the credentials again, and the transfer continues from where it stopped."
		v.CanPause, v.CanCancel = true, true
	case engine.JobCancelling:
		v.Reason = "Cancelling."
	case engine.JobFailing:
		v.Reason = "Stopping after an error."
	case engine.JobCancelled:
		v.Reason = "Cancelled."
		v.CanRemove = true
	case engine.JobFailed:
		v.Reason = "Failed."
		v.CanRetry, v.CanRemove = true, true
	case engine.JobCompleted:
		v.CanRemove = true
	}
	if j.Err != nil {
		v.ErrorCode, v.Detail = j.Err.Code, j.Err.Message
		v.Reason = failureReason(j.Err)
		if j.Err.PartNumber > 0 {
			v.Reason = fmt.Sprintf("%s (part %d)", v.Reason, j.Err.PartNumber)
		}
	}
	return v
}

// Jobs returns every job in submission order.
func (s *Service) Jobs(ctx context.Context) ([]JobView, error) {
	snaps, err := s.eng.Snapshot(ctx)
	if err != nil {
		return nil, describe(err)
	}
	out := make([]JobView, 0, len(snaps))
	for _, j := range snaps {
		out = append(out, view(j))
	}
	return out, nil
}

// Event is the batched progress event, "s4:queue". The engine produces at most
// one batch per 200 ms, so the UI receives about 5 per second however many parts
// are in flight. Merge Jobs into the list by ID. When Resync is true the engine
// dropped state changes, and the UI must call Jobs again.
type Event struct {
	// Seq increases by one with every event.
	Seq uint64 `json:"seq"`
	// At is the time of the batch in Unix milliseconds.
	At   int64     `json:"at"`
	Jobs []JobView `json:"jobs"`
	// Removed lists jobs the user cleared, to drop from the list.
	Removed []string `json:"removed,omitempty"`
	Resync  bool     `json:"resync,omitempty"`
}

// observer adapts the engine's progress batches to events.
type observer struct{ s *Service }

func (o observer) OnProgress(b engine.ProgressBatch) {
	s := o.s
	jobs := make([]JobView, 0, len(b.Jobs))
	s.mu.Lock()
	for _, j := range b.Jobs {
		if _, gone := s.removed[j.ID]; !gone {
			jobs = append(jobs, view(j))
		}
	}
	if len(jobs) == 0 && !b.Resync {
		s.mu.Unlock()
		return
	}
	s.seq++
	ev := Event{Seq: s.seq, At: b.At.UnixMilli(), Jobs: jobs, Resync: b.Resync}
	// The event is sent under the lock, so events reach the UI in sequence order.
	s.emit(EventQueue, ev)
	s.mu.Unlock()
}

// emitRemoved tells the UI that jobs were cleared.
func (s *Service) emitRemoved(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	s.seq++
	s.emit(EventQueue, Event{Seq: s.seq, At: time.Now().UnixMilli(), Jobs: []JobView{}, Removed: ids})
	s.mu.Unlock()
}

// describe turns an engine error into a message for the user.
func describe(err error) error {
	switch {
	case errors.Is(err, engine.ErrIllegalState):
		return errors.New("that is not possible for a transfer in its current state")
	case errors.Is(err, engine.ErrUnknownJob):
		return errors.New("that transfer no longer exists")
	case errors.Is(err, engine.ErrEngineClosed):
		return errors.New("s4browser is closing, so transfers are stopped")
	case errors.Is(err, engine.ErrInvalidSettings):
		return fmt.Errorf("invalid transfer settings: %w", err)
	}
	return err
}

// failureReason is a short sentence for a failed job. The engine's own text,
// which names the operation, the status and the request, stays in JobView.Detail.
func failureReason(e *engine.JobError) string {
	switch e.Code {
	case "NotFound", "NoSuchKey":
		return "The object was not found in the bucket."
	case "NoSuchBucket":
		return "The bucket does not exist."
	case "AccessDenied", "Forbidden":
		return "Access was denied. The profile may not be allowed to do this."
	}
	msg := e.Message
	if i := strings.Index(msg, ": operation error"); i > 0 {
		msg = msg[:i]
	}
	if e.Code != "" && !strings.Contains(msg, e.Code) {
		msg += " (" + e.Code + ")"
	}
	if msg == "" {
		msg = "The transfer failed."
	}
	return msg
}
