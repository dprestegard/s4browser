package journal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"s4browser/internal/fsx"
)

// Launch cleanup defaults. See RecoveryOptions.
const (
	DefaultRecoveryParallel  = 4
	DefaultRecoveryAttempts  = 5
	DefaultRecoveryBaseDelay = 500 * time.Millisecond
	recoveryMaxDelay         = 8 * time.Second
)

// RecoveryOptions configures a Recovery.
type RecoveryOptions struct {
	// Store is the journal opened at launch. Required.
	Store Store
	// Cleaner performs the S3 side. Each call makes one attempt. Required.
	Cleaner RemoteCleaner
	// Parallel bounds concurrent entries. Default DefaultRecoveryParallel.
	Parallel int
	// Attempts bounds tries of one remote request that fails with an error other
	// than ErrUploadGone or ErrCredentials. Default DefaultRecoveryAttempts.
	Attempts int
	// BaseDelay is the first wait between tries. It doubles, up to 8 s.
	// Default DefaultRecoveryBaseDelay.
	BaseDelay time.Duration
	// RemoveFile deletes a temp file. Default os.Remove. Tests replace it.
	RemoveFile func(path string) error
}

// Recovery is the production Recoverer. It owns the entries that were in the
// journal when it was created, the launch set, and never touches an entry
// written later, because those belong to jobs of this session.
//
// The launch set is what the store loaded when it was opened, if the store can
// say (FileStore can), and otherwise what it holds when NewRecovery runs. Recover
// may run in the background while new jobs start.
//
// Intent matching is guarded against this session's uploads. A creating entry
// can only name its upload by key and time, so a new job uploading the same key
// within the hour would match too. Before aborting a match, Recovery lists the
// uploads and only then reads the journal. If any entry outside the launch set
// names the same bucket and key, the launch entry is deferred and nothing is
// aborted. The order is what makes this safe: a job makes its intent durable
// before it sends Create, so any upload of this session that appears in the
// listing already has its entry by the time the journal is read.
type Recovery struct {
	opts   RecoveryOptions
	launch map[string]struct{}

	// run serializes passes.
	run sync.Mutex
}

var _ Recoverer = (*Recovery)(nil)

// NewRecovery snapshots the launch set from o.Store.
func NewRecovery(o RecoveryOptions) (*Recovery, error) {
	if o.Store == nil || o.Cleaner == nil {
		return nil, errors.New("journal: recovery needs a store and a cleaner")
	}
	if o.Parallel <= 0 {
		o.Parallel = DefaultRecoveryParallel
	}
	if o.Attempts <= 0 {
		o.Attempts = DefaultRecoveryAttempts
	}
	if o.BaseDelay <= 0 {
		o.BaseDelay = DefaultRecoveryBaseDelay
	}
	if o.RemoveFile == nil {
		o.RemoveFile = os.Remove
	}
	r := &Recovery{opts: o, launch: make(map[string]struct{})}
	launch := o.Store.List()
	if loaded, ok := o.Store.(interface{ Loaded() []Entry }); ok {
		launch = loaded.Loaded()
	}
	for _, entry := range launch {
		r.launch[entry.ID] = struct{}{}
	}
	return r, nil
}

// Recover implements Recoverer. It processes every launch entry still in the
// journal. Entries it cannot finish stay and are counted as deferred. Call it
// again to retry them. It returns ctx's error if ctx ends first.
func (r *Recovery) Recover(ctx context.Context) (Report, error) {
	return r.pass(ctx, func(Entry) bool { return true })
}

// RecoverProfile retries the launch entries of one profile. Call it when that
// profile's credentials change, alongside engine.NotifyCredentialsChanged.
func (r *Recovery) RecoverProfile(ctx context.Context, profile string) (Report, error) {
	return r.pass(ctx, func(e Entry) bool { return e.Kind == KindMultipartUpload && e.Profile == profile })
}

// Pending returns the launch entries still in the journal.
func (r *Recovery) Pending() []Entry {
	return slices.DeleteFunc(r.opts.Store.List(), func(e Entry) bool { return !r.owns(e) })
}

func (r *Recovery) owns(e Entry) bool {
	_, ok := r.launch[e.ID]
	return ok
}

// outcome is the result of one entry.
type outcome struct {
	aborted  int
	removed  int
	deferred bool
	warning  string
}

func (r *Recovery) pass(ctx context.Context, want func(Entry) bool) (Report, error) {
	r.run.Lock()
	defer r.run.Unlock()

	var todo []Entry
	for _, e := range r.Pending() {
		if want(e) {
			todo = append(todo, e)
		}
	}
	results := make([]outcome, len(todo))
	slots := make(chan struct{}, r.opts.Parallel)
	var wg sync.WaitGroup
	for i, e := range todo {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			results[i] = outcome{deferred: true}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			results[i] = r.recoverEntry(ctx, e)
		}()
	}
	wg.Wait()

	var report Report
	for i, o := range results {
		if o.deferred && o.warning != "" {
			if report.Deferrals == nil {
				report.Deferrals = map[string]string{}
			}
			report.Deferrals[todo[i].ID] = o.warning
		}
		report.UploadsAborted += o.aborted
		report.TempsRemoved += o.removed
		if o.deferred {
			report.Deferred++
		}
		if o.warning != "" {
			report.Warnings = append(report.Warnings, o.warning)
		}
	}
	if ctx.Err() != nil {
		return report, context.Cause(ctx)
	}
	return report, nil
}

func (r *Recovery) recoverEntry(ctx context.Context, e Entry) outcome {
	var o outcome
	switch {
	case e.Kind == KindDownloadTemp:
		o = r.removeTemp(e)
	case e.Phase == PhaseActive:
		o = r.abortActive(ctx, e)
	default:
		o = r.abortIntent(ctx, e)
	}
	if o.deferred {
		return o
	}
	if err := r.opts.Store.Delete(ctx, e.ID); err != nil {
		o.deferred = true
		o.warning = joinWarning(o.warning, "The transfer journal could not be updated, so cleanup will be checked again at next launch.")
	}
	return o
}

// removeTemp deletes a download temp file. A path that does not have the temp
// name form is never deleted: its entry is dropped with a warning.
func (r *Recovery) removeTemp(e Entry) outcome {
	if !fsx.IsTempPath(e.TempPath) {
		return outcome{warning: fmt.Sprintf("The journal named %s as a download temp file, but it is not one, so it was left alone.", e.TempPath)}
	}
	err := r.opts.RemoveFile(e.TempPath)
	switch {
	case err == nil:
		return outcome{removed: 1}
	case errors.Is(err, fs.ErrNotExist):
		return outcome{}
	default:
		return outcome{deferred: true, warning: fmt.Sprintf("The unfinished download %s could not be removed now and will be removed at next launch.", e.TempPath)}
	}
}

// abortActive aborts an upload whose ID was recorded.
func (r *Recovery) abortActive(ctx context.Context, e Entry) outcome {
	err := r.retry(ctx, func() error { return r.opts.Cleaner.AbortUpload(ctx, e, e.UploadID) })
	switch {
	case err == nil:
		return outcome{aborted: 1}
	case errors.Is(err, ErrUploadGone):
		return outcome{}
	default:
		return r.remoteDeferred(ctx, e, err)
	}
}

// abortIntent finds and aborts the uploads a creating entry may have started.
func (r *Recovery) abortIntent(ctx context.Context, e Entry) outcome {
	var uploads []RemoteUpload
	err := r.retry(ctx, func() error {
		var listErr error
		uploads, listErr = r.opts.Cleaner.ListUploads(ctx, e)
		return listErr
	})
	if err != nil {
		return r.remoteDeferred(ctx, e, err)
	}

	// Read the journal only after listing. See the type comment.
	known := make(map[string]struct{})
	for _, other := range r.opts.Store.List() {
		if other.UploadID != "" {
			known[other.UploadID] = struct{}{}
		}
		if !r.owns(other) && other.Kind == KindMultipartUpload && other.Bucket == e.Bucket && other.Key == e.Key {
			return outcome{deferred: true, warning: fmt.Sprintf(
				"An earlier upload to %s may have been left on S3. It will be cleaned up later, because an upload to the same key is in progress now.", location(e))}
		}
	}

	var o outcome
	for _, u := range uploads {
		if _, live := known[u.UploadID]; live || !MatchesIntent(e, u) {
			continue
		}
		err := r.retry(ctx, func() error { return r.opts.Cleaner.AbortUpload(ctx, e, u.UploadID) })
		switch {
		case err == nil:
			o.aborted++
		case errors.Is(err, ErrUploadGone):
		default:
			deferred := r.remoteDeferred(ctx, e, err)
			deferred.aborted = o.aborted
			return deferred
		}
	}
	return o
}

func (r *Recovery) remoteDeferred(ctx context.Context, e Entry, err error) outcome {
	switch {
	case ctx.Err() != nil:
		return outcome{deferred: true}
	case errors.Is(err, ErrCredentials):
		return outcome{deferred: true, warning: fmt.Sprintf(
			"An unfinished upload to %s will be cleaned up when the credentials of profile %s work again.", location(e), e.Profile)}
	default:
		return outcome{deferred: true, warning: fmt.Sprintf(
			"An unfinished upload to %s could not be cleaned up now and will be tried again at next launch: %v", location(e), err)}
	}
}

// retry runs call until it succeeds, reports a final answer (ErrUploadGone or
// ErrCredentials), the attempts run out or ctx ends.
func (r *Recovery) retry(ctx context.Context, call func() error) error {
	delay := r.opts.BaseDelay
	for attempt := 1; ; attempt++ {
		err := call()
		if err == nil || errors.Is(err, ErrUploadGone) || errors.Is(err, ErrCredentials) || attempt >= r.opts.Attempts {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return err
		}
		delay = min(delay*2, recoveryMaxDelay)
	}
}

func location(e Entry) string { return "s3://" + e.Bucket + "/" + e.Key }

func joinWarning(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}
