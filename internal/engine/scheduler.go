package engine

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// scheduler implements Engine. The loop goroutine is the single owner of all job
// and part state. Everything else talks to it through commands and results.
type scheduler struct {
	cfg      Config
	upload   *rateLimiter
	download *rateLimiter
	watchdog *watchdog

	commands     chan command
	results      chan func()
	batches      chan chan ProgressBatch
	closeRequest chan struct{}
	// poke wakes the loop when a download attempt parks, so that preemption can
	// be considered. Sends never block.
	poke      chan struct{}
	dlStats   downloadStats
	closeOnce sync.Once
	stopped   chan struct{}

	watchdogStop chan struct{}
	progressStop chan struct{}
	finalBatch   chan ProgressBatch
	// wg counts every goroutine the engine starts, except the loop itself.
	wg sync.WaitGroup

	// baseCtx ends when the engine closes. abortCtx outlives it, so that uploads
	// can still be aborted during shutdown.
	baseCtx     context.Context
	baseCancel  context.CancelCauseFunc
	abortCtx    context.Context
	abortCancel context.CancelFunc

	// Loop-owned state.
	settings Settings
	jobs     map[JobID]*job
	order    []*job
	queue    []*job
	// held keeps queued jobs, per profile, that were not admitted because that
	// profile's jobs wait for credentials. Entries may be stale, as in queue.
	held        map[string][]*job
	slotted     []*job
	roundRobin  int
	activeParts int
	// busy counts part attempts and control requests that are not cleanup.
	busy        int
	outstanding int
	open        int
	closing     bool
	abortsReady bool
	stopAborts  context.CancelFunc

	wakes      wakeHeap
	wakeSeq    uint64
	timer      Timer
	timerAt    time.Time
	timerArmed bool

	probes map[string]*probeState

	// sendBuffer is Config.SendBuffer with the default applied. connRateEst is
	// the loop's per-connection upload rate estimate in bytes per second, and
	// connRate publishes it to upload bodies. See observeUpload.
	sendBuffer  int64
	connRateEst float64
	connRate    atomic.Int64

	transitions []Transition
	resync      bool
	batchSeq    uint64
}

type command struct {
	run   func() error
	reply chan error
}

var _ Engine = (*scheduler)(nil)

// New validates cfg and starts the engine. The caller must call Close.
func New(cfg Config) (Engine, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	s := &scheduler{
		cfg:          cfg,
		upload:       newRateLimiter(cfg.Clock, cfg.Settings.UploadBytesPerSec),
		download:     newRateLimiter(cfg.Clock, cfg.Settings.DownloadBytesPerSec),
		watchdog:     newWatchdog(cfg.Timeouts),
		commands:     make(chan command),
		results:      make(chan func()),
		batches:      make(chan chan ProgressBatch),
		closeRequest: make(chan struct{}),
		poke:         make(chan struct{}, 1),
		stopped:      make(chan struct{}),
		watchdogStop: make(chan struct{}),
		progressStop: make(chan struct{}),
		finalBatch:   make(chan ProgressBatch, 1),
		settings:     cfg.Settings,
		jobs:         make(map[JobID]*job),
		held:         make(map[string][]*job),
		probes:       make(map[string]*probeState),
		timer:        cfg.Clock.NewTimer(time.Hour),
		sendBuffer:   cfg.SendBuffer,
	}
	if s.sendBuffer == 0 {
		s.sendBuffer = DefaultSendBuffer
	}
	s.timer.Stop()
	s.baseCtx, s.baseCancel = context.WithCancelCause(context.Background())
	s.abortCtx, s.abortCancel = context.WithCancel(context.Background())

	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.watchdog.run(cfg.Clock, s.watchdogStop)
	}()
	go func() {
		defer s.wg.Done()
		s.runProgress()
	}()
	go s.loop()
	return s, nil
}

func validateConfig(cfg Config) error {
	missing := ""
	switch {
	case cfg.Store == nil:
		missing = "Store"
	case cfg.Source == nil:
		missing = "Source"
	case cfg.Sink == nil:
		missing = "Sink"
	case cfg.Journal == nil:
		missing = "Journal"
	case cfg.Prober == nil:
		missing = "Prober"
	case cfg.Clock == nil:
		missing = "Clock"
	case cfg.Jitter == nil:
		missing = "Jitter"
	}
	if missing != "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidConfig, missing)
	}
	if err := cfg.Settings.Validate(); err != nil {
		return err
	}
	if cfg.SendBuffer < 0 {
		return fmt.Errorf("%w: negative SendBuffer", ErrInvalidConfig)
	}
	t := cfg.Timeouts
	if t.Stall <= 0 || t.Response <= 0 || t.Control <= 0 || t.Complete <= 0 || t.ShutdownAbort <= 0 || t.ProbeMin <= 0 || t.ProbeMax < t.ProbeMin {
		return fmt.Errorf("%w: every timeout must be positive", ErrInvalidConfig)
	}
	r := cfg.Retry
	if r.BaseDelay <= 0 || r.ThrottleBaseDelay <= 0 || r.MaxDelay <= 0 || r.MaxPartAttempts <= 0 || r.MaxControlAttempts <= 0 || r.MaxAbortAttempts <= 0 {
		return fmt.Errorf("%w: incomplete retry policy", ErrInvalidConfig)
	}
	return nil
}

// ---- Commands ----

// do runs fn on the loop goroutine and returns its result.
func (s *scheduler) do(ctx context.Context, fn func() error) error {
	cmd := command{run: fn, reply: make(chan error, 1)}
	select {
	case s.commands <- cmd:
	case <-s.stopped:
		return ErrEngineClosed
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	select {
	case err := <-cmd.reply:
		return err
	case <-s.stopped:
		select {
		case err := <-cmd.reply:
			return err
		default:
			return ErrEngineClosed
		}
	}
}

// Submit implements Engine.
func (s *scheduler) Submit(ctx context.Context, spec JobSpec) (JobID, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := JobID(hex.EncodeToString(raw[:]))
	err := s.do(ctx, func() error {
		j := &job{id: id, spec: spec, state: JobQueued, run: 1, dirty: true}
		s.jobs[id] = j
		s.order = append(s.order, j)
		s.open++
		s.queue = append(s.queue, j)
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *scheduler) withJob(ctx context.Context, id JobID, fn func(*job) error) error {
	return s.do(ctx, func() error {
		j := s.jobs[id]
		if j == nil {
			return fmt.Errorf("%w: %s", ErrUnknownJob, id)
		}
		return fn(j)
	})
}

// Pause implements Engine.
func (s *scheduler) Pause(ctx context.Context, id JobID) error {
	return s.withJob(ctx, id, func(j *job) error {
		if j.phase == PhaseFinalizing && (j.state == JobRunning || j.state == JobCredentialWait) {
			return ErrIllegalState
		}
		if !s.fire(j, JobEvPause) {
			return ErrIllegalState
		}
		if j.state == JobPausing {
			j.cancel(ErrJobPaused)
			s.interruptBackoffs(j)
			s.settle(j)
		}
		return nil
	})
}

// Resume implements Engine.
func (s *scheduler) Resume(ctx context.Context, id JobID) error {
	return s.withJob(ctx, id, func(j *job) error {
		if !s.fire(j, JobEvResume) {
			return ErrIllegalState
		}
		j.notice = ""
		s.queue = append(s.queue, j)
		return nil
	})
}

// Cancel implements Engine.
func (s *scheduler) Cancel(ctx context.Context, id JobID) error {
	return s.withJob(ctx, id, func(j *job) error {
		return s.cancelJob(j, ErrJobCancelled)
	})
}

// cancelJob applies Cancel. During finalizing a user cancel waits only while a
// request is in flight, because the outcome of Complete or of the commit must be
// known first, and once the object or file is in place, when the job finishes
// as completed. Between requests, such as during a retry backoff, it applies at
// once. advance applies a waiting cancel before it starts the next finalizing
// request. Engine close is never deferred.
func (s *scheduler) cancelJob(j *job, cause error) error {
	if cause != ErrEngineClosed && j.phase == PhaseFinalizing &&
		(j.state == JobRunning || j.state == JobCredentialWait) &&
		(j.control > 0 || j.step >= stepJournalDelete) {
		j.cancelRequested = true
		return nil
	}
	return s.applyCancel(j, cause)
}

// applyCancel moves a job to cancelling without any deferral.
func (s *scheduler) applyCancel(j *job, cause error) error {
	if !s.fire(j, JobEvCancel) {
		return ErrIllegalState
	}
	j.cancelRequested = false
	if j.cancel != nil {
		j.cancel(cause)
	}
	s.interruptBackoffs(j)
	s.settle(j)
	return nil
}

// Retry implements Engine.
func (s *scheduler) Retry(ctx context.Context, id JobID) error {
	return s.withJob(ctx, id, func(j *job) error {
		if !s.fire(j, JobEvRetry) {
			return ErrIllegalState
		}
		j.resetRun()
		j.run++
		s.open++
		s.queue = append(s.queue, j)
		return nil
	})
}

// Remove implements Engine.
func (s *scheduler) Remove(ctx context.Context, id JobID) error {
	return s.withJob(ctx, id, func(j *job) error {
		if !j.state.IsTerminal() {
			return ErrIllegalState
		}
		delete(s.jobs, id)
		s.order = slices.DeleteFunc(s.order, func(other *job) bool { return other == j })
		return nil
	})
}

// UpdateSettings implements Engine.
func (s *scheduler) UpdateSettings(ctx context.Context, settings Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	return s.do(ctx, func() error {
		if settings.UploadBytesPerSec != s.settings.UploadBytesPerSec {
			s.upload.SetRate(settings.UploadBytesPerSec)
		}
		if settings.DownloadBytesPerSec != s.settings.DownloadBytesPerSec {
			s.download.SetRate(settings.DownloadBytesPerSec)
		}
		s.settings = settings
		return nil
	})
}

// NotifyCredentialsChanged implements Engine.
func (s *scheduler) NotifyCredentialsChanged(profile string) {
	_ = s.do(context.Background(), func() error {
		if p := s.probes[profile]; p != nil && s.anyWaiting(profile) {
			s.runProbe(p)
		}
		return nil
	})
}

// Snapshot implements Engine.
func (s *scheduler) Snapshot(ctx context.Context) ([]JobSnapshot, error) {
	var out []JobSnapshot
	err := s.do(ctx, func() error {
		out = make([]JobSnapshot, len(s.order))
		for i, j := range s.order {
			out[i] = j.snapshot()
		}
		return nil
	})
	return out, err
}

// Close implements Engine.
func (s *scheduler) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.closeRequest) })
	select {
	case <-s.stopped:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// ---- Loop ----

func (s *scheduler) loop() {
	closeRequest := s.closeRequest
	for {
		s.pump()
		if s.closing && s.open == 0 && s.outstanding == 0 {
			break
		}
		timerC := s.armTimer()
		select {
		case cmd := <-s.commands:
			if s.closing {
				cmd.reply <- ErrEngineClosed
			} else {
				cmd.reply <- cmd.run()
			}
		case apply := <-s.results:
			s.outstanding--
			apply()
		case reply := <-s.batches:
			reply <- s.buildBatch()
		case <-timerC:
			s.timerArmed = false
			s.fireWakes()
		case <-s.poke:
		case <-closeRequest:
			closeRequest = nil
			s.beginClose()
		}
	}
	s.finishClose()
}

// pump starts all work the current limits allow. It runs after every event, so a
// raised limit takes effect at once and a lowered one at the next dispatch.
func (s *scheduler) pump() {
	if s.closing {
		s.pumpShutdown()
		return
	}
	s.releaseHeld()
	for s.slots() < s.settings.MaxActiveFiles && len(s.queue) > 0 {
		j := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		if j.state != JobQueued {
			continue
		}
		// A job of a profile whose credentials are being waited for would only
		// join the wait, so it is set aside, in order, until the wait ends.
		if profile := j.spec.Ref.Profile; s.anyWaiting(profile) {
			s.held[profile] = append(s.held[profile], j)
			continue
		}
		s.admit(j)
	}
	if len(s.queue) == 0 {
		s.queue = nil
	}
	s.dispatchParts()
	s.preemptStarved()
}

// slots counts the slots that limit admission. A job waiting for credentials
// keeps its slot, so it resumes at once when they return, but does not count:
// one profile's expired session must not stop other profiles' jobs. When the
// wait ends, the running jobs can exceed MaxActiveFiles until enough of them
// finish. Parts stay within MaxActiveParts throughout.
func (s *scheduler) slots() int {
	n := 0
	for _, j := range s.slotted {
		if j.state != JobCredentialWait {
			n++
		}
	}
	return n
}

// releaseHeld returns the jobs set aside for a profile to the front of the
// queue, in their order, once no job of that profile waits for credentials.
func (s *scheduler) releaseHeld() {
	for profile, jobs := range s.held {
		if s.anyWaiting(profile) {
			continue
		}
		delete(s.held, profile)
		s.queue = append(jobs, s.queue...)
	}
}

func (s *scheduler) admit(j *job) {
	s.fire(j, JobEvAdmit)
	j.holdsSlot = true
	s.slotted = append(s.slotted, j)
	j.ctx, j.cancel = context.WithCancelCause(s.baseCtx)
	j.lastProgress = s.cfg.Clock.Now()
	s.settle(j)
}

func (s *scheduler) releaseSlot(j *job) {
	if !j.holdsSlot {
		return
	}
	j.holdsSlot = false
	s.slotted = slices.DeleteFunc(s.slotted, func(other *job) bool { return other == j })
}

// dispatchParts assigns parts round-robin across running jobs while both the
// per-file and global limits hold.
func (s *scheduler) dispatchParts() {
	for s.activeParts < s.settings.MaxActiveParts {
		count := len(s.slotted)
		started := false
		for i := 0; i < count && s.activeParts < s.settings.MaxActiveParts; i++ {
			j := s.slotted[(s.roundRobin+i)%count]
			if j.state == JobRunning && j.step == stepTransfer &&
				j.inflight < s.settings.MaxPartsPerFile && j.dispatchable() {
				s.startPart(j)
				started = true
			}
		}
		if !started {
			return
		}
		s.roundRobin++
	}
}

// spawn runs work on a new goroutine. work's return value is applied on the loop.
// The loop keeps receiving until outstanding is zero, so the send cannot block forever.
func (s *scheduler) spawn(work func() func()) {
	s.outstanding++
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.results <- work()
	}()
}

// fire applies a job event through the state table and records the transition.
func (s *scheduler) fire(j *job, ev JobEvent) bool {
	next, ok := NextJobState(j.state, ev)
	if !ok {
		return false
	}
	s.recordTransition(Transition{ID: j.id, From: j.state, To: next, At: s.cfg.Clock.Now()})
	j.state = next
	j.dirty = true
	return true
}

func (s *scheduler) recordTransition(t Transition) {
	if len(s.transitions) == MaxBufferedTransitions {
		copy(s.transitions, s.transitions[1:])
		s.transitions = s.transitions[:MaxBufferedTransitions-1]
		s.resync = true
	}
	s.transitions = append(s.transitions, t)
}

// setPart applies a part event through the state table. An illegal edge is a
// scheduler bug, so it stops the process rather than corrupting a transfer.
func setPart(p *partEntry, ev PartEvent) {
	next, ok := NextPartState(p.state, ev)
	if !ok {
		panic(fmt.Sprintf("engine: illegal part transition from %s on event %d", p.state, ev))
	}
	p.state = next
}

// settle moves a job forward after anything happened to it.
func (s *scheduler) settle(j *job) {
	switch j.state {
	case JobRunning:
		s.advance(j)
	case JobCredentialWait:
		if j.cancelRequested && j.control == 0 {
			_ = s.applyCancel(j, ErrJobCancelled)
			return
		}
		// A Complete that succeeded during the wait still finishes the job.
		if j.step >= stepJournalDelete {
			s.advance(j)
		}
	case JobPausing:
		if j.drained() && s.fire(j, JobEvDrained) {
			s.releaseSlot(j)
		}
	case JobCancelling, JobFailing:
		s.cleanup(j)
	}
}

// fatal moves a job to failing. A user's deferred cancel wins over the failure.
func (s *scheduler) fatal(j *job, err error, part int32) {
	if j.cancelRequested {
		_ = s.applyCancel(j, ErrJobCancelled)
		return
	}
	if !s.fire(j, JobEvFatal) {
		return
	}
	j.jobErr = newJobError(err, part)
	j.cancel(ErrJobCancelled)
	s.interruptBackoffs(j)
}

// interruptBackoffs returns every part in backoff to pending and drops scheduled wakes.
func (s *scheduler) interruptBackoffs(j *job) {
	j.epoch++
	j.ctlBackoff = false
	if j.backoffs == 0 {
		return
	}
	j.backoffs = 0
	for i := range j.parts {
		if p := &j.parts[i]; p.state == PartBackoff {
			setPart(p, PartEvInterrupt)
			j.returnPart(i)
		}
	}
}

// ---- Timer ----

func (s *scheduler) schedule(w wake) {
	s.wakeSeq++
	w.seq = s.wakeSeq
	heap.Push(&s.wakes, w)
}

func (s *scheduler) armTimer() <-chan time.Time {
	if len(s.wakes) == 0 {
		if s.timerArmed {
			s.timer.Stop()
			s.timerArmed = false
		}
		return nil
	}
	when := s.wakes[0].when
	if !s.timerArmed || !when.Equal(s.timerAt) {
		s.timer.Reset(max(when.Sub(s.cfg.Clock.Now()), 0))
		s.timerAt = when
		s.timerArmed = true
	}
	return s.timer.C()
}

func (s *scheduler) fireWakes() {
	now := s.cfg.Clock.Now()
	for len(s.wakes) > 0 && !s.wakes[0].when.After(now) {
		w := heap.Pop(&s.wakes).(wake)
		switch w.kind {
		case wakePart:
			if j := w.job; w.epoch == j.epoch {
				setPart(&j.parts[w.part], PartEvBackoffElapsed)
				j.backoffs--
				j.returnPart(int(w.part))
			}
		case wakeControl:
			if j := w.job; w.epoch == j.epoch && j.ctlBackoff {
				j.ctlBackoff = false
				s.settle(j)
			}
		case wakeProbe:
			if p := w.probe; p.scheduled && w.epoch == p.epoch {
				s.runProbe(p)
			}
		}
	}
}

// ---- Shutdown ----

// beginClose performs steps 1 and 2 of the shutdown order.
func (s *scheduler) beginClose() {
	s.closing = true
	for _, j := range s.order {
		if j.state.IsTerminal() {
			continue
		}
		if j.completeInFlight {
			// The outcome will not be known, so next launch resolves the entry.
			j.keepJournal = true
		}
		_ = s.cancelJob(j, ErrEngineClosed)
		// A cleanup retry waiting on the clock must not hold up shutdown.
		s.interruptBackoffs(j)
	}
	s.baseCancel(ErrEngineClosed)
}

// pumpShutdown performs steps 3 to 5. Aborts wait until every attempt has exited.
func (s *scheduler) pumpShutdown() {
	if s.busy > 0 {
		return
	}
	if !s.abortsReady {
		s.abortsReady = true
		deadline, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.ShutdownAbort)
		s.stopAborts = cancel
		context.AfterFunc(deadline, s.abortCancel)
	}
	for _, j := range s.order {
		if !j.state.IsTerminal() {
			s.settle(j)
		}
	}
}

// finishClose performs step 6 and releases everything the loop owns.
func (s *scheduler) finishClose() {
	if s.stopAborts != nil {
		s.stopAborts()
	}
	s.abortCancel()
	s.timer.Stop()
	close(s.watchdogStop)
	s.finalBatch <- s.buildBatch()
	close(s.progressStop)
	s.wg.Wait()
	close(s.stopped)
}
