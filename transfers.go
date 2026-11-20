package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"s4browser/internal/queue"
)

// transferTimeout bounds a submit call. Expanding a folder of many files, or
// listing a large prefix, can take minutes: files and objects already queued
// when it ends stay queued and the result says where it stopped. The region
// lookup has its own, shorter limit inside the queue.
const transferTimeout = 30 * time.Minute

// queueCloseTimeout is the time shutdown allows for stopping transfers. Engine
// shutdown spends up to 10 s aborting uploads.
const queueCloseTimeout = 30 * time.Second

// openQueue starts the transfer queue: it opens the journal in the app's config
// folder, runs launch cleanup in the background and connects credential changes
// to the engine. See queue.Open.
func (a *App) openQueue() (*queue.Service, error) {
	if a.profiles == nil {
		return nil, errors.New("the profile store is not available")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	o := a.queueOptions(filepath.Join(dir, "s4browser"))
	if err := devQueueOptions(&o); err != nil {
		return nil, err
	}
	return queue.Open(o)
}

// queueOptions wires the queue to the app: credentials from the profile
// manager, credential changes to the engine and to launch cleanup, region
// lookups to browsing and events to the window. Tests call it with a temporary
// folder and replace Emit.
func (a *App) queueOptions(dir string) queue.Options {
	o := queue.Options{
		Dir:      dir,
		Provider: a.profiles.Provider,
		Refresh: func(ctx context.Context, profile string) error {
			_, err := a.profiles.Refresh(ctx, profile)
			return err
		},
		Subscribe: a.profiles.OnCredentialsChanged,
		Emit:      func(event string, payload any) { runtime.EventsEmit(a.ctx, event, payload) },
	}
	if a.browse != nil {
		o.Region = a.browse.Region
	}
	return o
}

// closeQueue stops transfers and closes the journal at shutdown.
func (a *App) closeQueue() {
	if a.queue == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), queueCloseTimeout)
	defer cancel()
	_ = a.queue.Close(ctx)
}

func (a *App) transfers() (*queue.Service, error) {
	if a.queue == nil {
		return nil, errors.New("transfers are not available: " + a.queueErr)
	}
	return a.queue, nil
}

// bindingContext is the context for a binding call that needs one.
func (a *App) bindingContext() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// TransferStatus reports whether transfers can run, whether uploads are turned
// off, and the warnings from the journal and launch cleanup. Cleanup runs in the
// background: "s4:queue-status" events carry the same value whenever it changes.
func (a *App) TransferStatus() queue.Status {
	q, err := a.transfers()
	if err != nil {
		return queue.Status{Warnings: []string{}, Error: a.queueErr}
	}
	return q.Status()
}

// QueueJobs lists every job in submission order. Call it when the dock opens and
// after an "s4:queue" event with resync set. Later changes arrive as events.
func (a *App) QueueJobs() ([]queue.JobView, error) {
	q, err := a.transfers()
	if err != nil {
		return nil, err
	}
	return q.Jobs(a.bindingContext())
}

// QueueUploads queues one upload per file path. Items the queue cannot accept
// are reported one by one in the result. The call fails as a whole if uploads
// are turned off or the target is unusable.
func (a *App) QueueUploads(req queue.UploadRequest) (queue.SubmitResult, error) {
	q, err := a.transfers()
	if err != nil {
		return queue.SubmitResult{}, err
	}
	req.ProfileID = devProfile(req.ProfileID)
	ctx, cancel := context.WithTimeout(a.bindingContext(), transferTimeout)
	defer cancel()
	return q.SubmitUploads(ctx, req)
}

// QueueDownloads queues one download per object key into a local folder.
func (a *App) QueueDownloads(req queue.DownloadRequest) (queue.SubmitResult, error) {
	q, err := a.transfers()
	if err != nil {
		return queue.SubmitResult{}, err
	}
	req.ProfileID = devProfile(req.ProfileID)
	ctx, cancel := context.WithTimeout(a.bindingContext(), transferTimeout)
	defer cancel()
	return q.SubmitDownloads(ctx, req)
}

// jobCommand runs a command on one job.
func (a *App) jobCommand(run func(*queue.Service, context.Context, string) error, id string) error {
	q, err := a.transfers()
	if err != nil {
		return err
	}
	return run(q, a.bindingContext(), id)
}

// PauseJob pauses a job. Finished parts are kept.
func (a *App) PauseJob(id string) error { return a.jobCommand((*queue.Service).Pause, id) }

// ResumeJob queues a paused job again.
func (a *App) ResumeJob(id string) error { return a.jobCommand((*queue.Service).Resume, id) }

// CancelJob cancels a job and aborts its multipart upload.
func (a *App) CancelJob(id string) error { return a.jobCommand((*queue.Service).Cancel, id) }

// RetryJob starts a failed job again from the beginning.
func (a *App) RetryJob(id string) error { return a.jobCommand((*queue.Service).Retry, id) }

// RemoveJob forgets one finished job.
func (a *App) RemoveJob(id string) error { return a.jobCommand((*queue.Service).Remove, id) }

// allCommand runs a command on every job it applies to and returns how many changed.
func (a *App) allCommand(run func(*queue.Service, context.Context) (int, error)) (int, error) {
	q, err := a.transfers()
	if err != nil {
		return 0, err
	}
	return run(q, a.bindingContext())
}

// PauseAll pauses every job that can be paused and returns how many.
func (a *App) PauseAll() (int, error) { return a.allCommand((*queue.Service).PauseAll) }

// ResumeAll resumes every paused job and returns how many.
func (a *App) ResumeAll() (int, error) { return a.allCommand((*queue.Service).ResumeAll) }

// CancelAll cancels every unfinished job and returns how many.
func (a *App) CancelAll() (int, error) { return a.allCommand((*queue.Service).CancelAll) }

// ClearFinished forgets completed and cancelled jobs, and failed ones when
// includeFailed is set, and returns how many it removed.
func (a *App) ClearFinished(includeFailed bool) (int, error) {
	return a.allCommand(func(q *queue.Service, ctx context.Context) (int, error) {
		return q.ClearFinished(ctx, includeFailed)
	})
}

// TransferSettings returns the transfer settings with their defaults and limits.
func (a *App) TransferSettings() (queue.SettingsState, error) {
	q, err := a.transfers()
	if err != nil {
		return queue.SettingsState{}, err
	}
	return q.Settings(), nil
}

// SetTransferSettings validates, saves and applies new settings while transfers
// run, and returns the settings now in force.
func (a *App) SetTransferSettings(s queue.TransferSettings) (queue.SettingsState, error) {
	q, err := a.transfers()
	if err != nil {
		return queue.SettingsState{}, err
	}
	return q.SetSettings(a.bindingContext(), s)
}
