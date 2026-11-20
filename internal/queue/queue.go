// Package queue is the service between the Wails bindings and engine.Engine. It
// opens the journal, runs launch cleanup, builds the engine from the app's
// credentials, keeps the transfer settings on disk, turns engine progress into
// batched UI events and validates what the user submits.
//
// It holds no job state of its own: the engine owns every job, and the service
// only maps snapshots to JobView.
package queue

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/fsx"
	"s4browser/internal/journal"
	"s4browser/internal/s3x"
)

// Event names sent through Options.Emit.
const (
	// EventQueue carries an Event: the jobs that changed, about 5 times a second.
	EventQueue = "s4:queue"
	// EventStatus carries a Status, whenever launch cleanup makes progress.
	EventStatus = "s4:queue-status"
)

// Options configures Open. Dir, Provider and Emit are what the app supplies. The
// rest are for tests and for the dev fake S3.
type Options struct {
	// Dir holds journal.json and transfer-settings.json: the app's config folder.
	Dir string
	// Provider returns a profile's credentials, normally creds.Manager.Provider.
	// It must return one long-lived *aws.CredentialsCache per profile.
	Provider func(profile string) (aws.CredentialsProvider, error)
	// Refresh discards a profile's cached credentials and loads them again, like
	// creds.Manager.Refresh. The credential prober calls it. Nil does nothing.
	Refresh func(ctx context.Context, profile string) error
	// Subscribe registers fn to run when a profile's credentials change, like
	// creds.Manager.OnCredentialsChanged. Optional.
	Subscribe func(fn func(profile string))
	// Region returns a bucket's region when the request does not say. Optional.
	Region func(ctx context.Context, profile, bucket string) (string, error)
	// Emit delivers an event to the UI. Optional.
	Emit func(event string, payload any)
	// ClientOptions are applied to every S3 client after the defaults. The dev
	// fake S3 and the tests use them to set the endpoint.
	ClientOptions []func(*s3.Options)

	// Seams for tests. Zero values give the production behaviour.
	Source      engine.Source
	Sink        engine.Sink
	Guard       engine.WriteGuard
	WrapStore   func(engine.ObjectStore) engine.ObjectStore
	WrapCleaner func(journal.RemoteCleaner) journal.RemoteCleaner
	Retry       *engine.RetryPolicy
	Timeouts    *engine.Timeouts
	// RecoveryBaseDelay is the first wait between launch cleanup tries.
	RecoveryBaseDelay time.Duration
}

// Service is the queue. It is safe for concurrent use.
type Service struct {
	opts     Options
	eng      engine.Engine
	store    journal.Store
	recovery *journal.Recovery
	// fileStore is the journal when it is a file, for its warnings.
	fileStore *journal.FileStore
	settings  *settingsFile
	// clients returns the S3 client for a profile and region, for listing.
	clients s3x.ClientFunc
	// setMu makes SetSettings (save, then apply) one step.
	setMu sync.Mutex

	uploadsOff string // Why uploads are disabled. Empty means enabled.

	// ctx ends when Close starts, and stops launch cleanup.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	cleanup  cleanupState
	removed  map[engine.JobID]struct{} // Jobs the user cleared, so a late batch cannot bring them back.
	seq      uint64
	settWarn string
}

// cleanupState is the progress of launch cleanup.
type cleanupState struct {
	running  int
	finished bool
	aborted  int
	temps    int
	err      string
	warnings map[string][]string // by pass: "" for launch, a profile ID otherwise
}

// ErrUnavailable means transfers cannot run, with the reason in the message.
var ErrUnavailable = errors.New("transfers are not available")

// Open starts the queue. It opens the journal, creates launch cleanup before the
// engine, starts the engine and runs cleanup in the background.
//
//   - A journal held by another process returns an error that says so: a second
//     copy of the app must not touch the first one's uploads.
//   - A journal written by a newer build disables uploads and leaves the file
//     alone. Downloads still work, with an in-memory journal.
func Open(o Options) (*Service, error) {
	if o.Dir == "" || o.Provider == nil {
		return nil, errors.New("queue: Dir and Provider are required")
	}
	settings, settingsWarn := openSettings(o.Dir)
	s := &Service{opts: o, settings: settings, settWarn: settingsWarn, removed: map[engine.JobID]struct{}{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())

	file, err := journal.OpenFile(o.Dir)
	switch {
	case err == nil:
		s.store, s.fileStore = file, file
	case errors.Is(err, journal.ErrUnsupportedVersion):
		s.store = newMemoryJournal()
		s.uploadsOff = "The transfer journal was written by a newer version of s4browser, so uploads are turned off to protect it. Downloads still work. Update s4browser, or move journal.json out of the app folder."
	case errors.Is(err, journal.ErrLocked):
		return nil, fmt.Errorf("%w: another copy of s4browser is already running and owns the transfer journal. Close it and start s4browser again", ErrUnavailable)
	default:
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	clients := s3x.NewClients(o.Provider, o.ClientOptions...)
	s.clients = clients
	if s.fileStore != nil {
		var cleaner journal.RemoteCleaner = s3x.NewCleaner(clients)
		if o.WrapCleaner != nil {
			cleaner = o.WrapCleaner(cleaner)
		}
		// Launch cleanup owns the entries found now, so it is created before the
		// engine writes any.
		s.recovery, err = journal.NewRecovery(journal.RecoveryOptions{Store: s.store, Cleaner: cleaner, BaseDelay: o.RecoveryBaseDelay})
		if err != nil {
			_ = s.store.Close()
			return nil, err
		}
	}

	var objects engine.ObjectStore = s3x.NewStore(clients)
	if o.WrapStore != nil {
		objects = o.WrapStore(objects)
	}
	refresh := o.Refresh
	if refresh == nil {
		refresh = func(context.Context, string) error { return nil }
	}
	var source engine.Source = engine.FileSource{}
	if o.Source != nil {
		source = o.Source
	}
	var sink engine.Sink = engine.NewFileSink(fsx.DefaultStrategy())
	if o.Sink != nil {
		sink = o.Sink
	}
	cfg := engine.Config{
		Store: objects, Source: source, Sink: sink, Journal: s.store,
		Prober: s3x.NewProber(refresh, clients), Clock: engine.RealClock{}, Observer: observer{s},
		Guard: o.Guard, Jitter: rand.Float64, Settings: settings.current().engine(),
		Retry: engine.DefaultRetryPolicy(), Timeouts: engine.DefaultTimeouts(),
	}
	if o.Retry != nil {
		cfg.Retry = *o.Retry
	}
	if o.Timeouts != nil {
		cfg.Timeouts = *o.Timeouts
	}
	if s.eng, err = engine.New(cfg); err != nil {
		_ = s.store.Close()
		return nil, err
	}

	if o.Subscribe != nil {
		o.Subscribe(s.CredentialsChanged)
	}
	if s.recovery != nil {
		s.startCleanup("", func(ctx context.Context) (journal.Report, error) { return s.recovery.Recover(ctx) })
	}
	return s, nil
}

// startCleanup runs one cleanup pass in the background. scope is "" for the
// launch pass and a profile ID for a pass after that profile's credentials changed.
func (s *Service) startCleanup(scope string, pass func(context.Context) (journal.Report, error)) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.cleanup.running++
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		report, err := pass(s.ctx)
		s.mu.Lock()
		s.cleanup.running--
		s.cleanup.aborted += report.UploadsAborted
		s.cleanup.temps += report.TempsRemoved
		if s.cleanup.warnings == nil {
			s.cleanup.warnings = map[string][]string{}
		}
		if scope == "" {
			clear(s.cleanup.warnings)
			s.cleanup.finished = true
		}
		s.cleanup.warnings[scope] = report.Warnings
		s.cleanup.err = ""
		if err != nil && s.ctx.Err() == nil {
			s.cleanup.err = err.Error()
		}
		closed := s.closed
		s.mu.Unlock()
		if !closed {
			s.emit(EventStatus, s.Status())
		}
	}()
}

// CredentialsChanged tells the engine to probe the profile now and retries the
// profile's launch cleanup that was waiting for credentials. Connect it to
// creds.Manager.OnCredentialsChanged. It does not wait for S3.
func (s *Service) CredentialsChanged(profile string) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	s.eng.NotifyCredentialsChanged(profile)
	if s.recovery != nil {
		s.startCleanup(profile, func(ctx context.Context) (journal.Report, error) {
			return s.recovery.RecoverProfile(ctx, profile)
		})
	}
}

// Status is the state of the queue as a whole.
type Status struct {
	// Available is false when transfers could not start. Error then says why, for
	// example that another copy of the app holds the journal. The app fills this
	// in when it has no queue at all.
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	// UploadsDisabled is true when uploads are refused, with the reason.
	UploadsDisabled       bool   `json:"uploadsDisabled"`
	UploadsDisabledReason string `json:"uploadsDisabledReason,omitempty"`
	// Warnings are shown to the user: a damaged journal or settings file, and
	// launch cleanup problems.
	Warnings []string      `json:"warnings"`
	Cleanup  CleanupStatus `json:"cleanup"`
}

// CleanupStatus reports launch cleanup of the previous session's leftovers.
type CleanupStatus struct {
	// Running is true while a pass is in progress.
	Running bool `json:"running"`
	// Finished is true once the launch pass has ended.
	Finished       bool `json:"finished"`
	UploadsAborted int  `json:"uploadsAborted"`
	TempsRemoved   int  `json:"tempsRemoved"`
	// Pending counts leftovers still in the journal, which are retried when the
	// profile's credentials change and at the next launch.
	Pending int    `json:"pending"`
	Error   string `json:"error,omitempty"`
}

// Status returns the queue status.
func (s *Service) Status() Status {
	st := Status{Available: true, UploadsDisabled: s.uploadsOff != "", UploadsDisabledReason: s.uploadsOff, Warnings: []string{}}
	if s.settWarn != "" {
		st.Warnings = append(st.Warnings, s.settWarn)
	}
	if s.fileStore != nil {
		st.Warnings = append(st.Warnings, s.fileStore.Warnings()...)
	}
	s.mu.Lock()
	scopes := make([]string, 0, len(s.cleanup.warnings))
	for scope := range s.cleanup.warnings {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	for _, scope := range scopes {
		for _, w := range s.cleanup.warnings[scope] {
			if !slices.Contains(st.Warnings, w) {
				st.Warnings = append(st.Warnings, w)
			}
		}
	}
	st.Cleanup = CleanupStatus{
		Running: s.cleanup.running > 0, Finished: s.cleanup.finished,
		UploadsAborted: s.cleanup.aborted, TempsRemoved: s.cleanup.temps, Error: s.cleanup.err,
	}
	s.mu.Unlock()
	if s.recovery != nil {
		st.Cleanup.Pending = len(s.recovery.Pending())
	}
	return st
}

func (s *Service) emit(event string, payload any) {
	if s.opts.Emit != nil {
		s.opts.Emit(event, payload)
	}
}

// Close cancels launch cleanup, stops the engine (which cancels running jobs and
// aborts their uploads, as engine.Close describes) and then closes the journal.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	err := s.eng.Close(ctx)
	if cerr := s.store.Close(); err == nil {
		err = cerr
	}
	return err
}
