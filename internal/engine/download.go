package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"s4browser/internal/fsx"
	"s4browser/internal/journal"
)

// This file is the download path of docs/engine-design.md section 8. A download
// job runs these steps: stepPrepare (HeadObject and the part plan),
// stepJournalTemp, stepSinkPrepare, stepTransfer, stepCommit, stepJournalDelete.

// downloadBuffers hands out the DownloadBufferSize slices that attempts read into.
var downloadBuffers = sync.Pool{New: func() any {
	buf := make([]byte, DownloadBufferSize)
	return &buf
}}

// DownloadStats counts the download lead gate's work since the engine started.
// Stage 0.5 used them to choose MaxLead: a long ParkedMax is a connection that
// sat idle while S3 could have closed it.
type DownloadStats struct {
	// Parks counts attempts that had to wait for the frontier.
	Parks int64
	// ParkedTotal is the summed wait time of all parks.
	ParkedTotal time.Duration
	// ParkedMax is the longest single wait.
	ParkedMax time.Duration
	// Preemptions counts parked attempts cancelled to free a slot.
	Preemptions int64
}

// StatsProvider is implemented by the engine returned from New. It is for tests,
// benchmarks and diagnostics, and is not part of the Engine contract.
type StatsProvider interface {
	DownloadStats() DownloadStats
}

type downloadStats struct {
	parks       atomic.Int64
	parkedNanos atomic.Int64
	parkedMax   atomic.Int64
	preemptions atomic.Int64
}

func (d *downloadStats) addPark(wait time.Duration) {
	d.parks.Add(1)
	d.parkedNanos.Add(int64(wait))
	for {
		old := d.parkedMax.Load()
		if int64(wait) <= old || d.parkedMax.CompareAndSwap(old, int64(wait)) {
			return
		}
	}
}

// DownloadStats implements StatsProvider.
func (s *scheduler) DownloadStats() DownloadStats {
	return DownloadStats{
		Parks:       s.dlStats.parks.Load(),
		ParkedTotal: time.Duration(s.dlStats.parkedNanos.Load()),
		ParkedMax:   time.Duration(s.dlStats.parkedMax.Load()),
		Preemptions: s.dlStats.preemptions.Load(),
	}
}

// ---- Lead gate ----

// leadGate enforces SinkWriter.MaxLead. The frontier is the offset of the
// lowest part that is not done. The scheduler advances it, and attempts that
// would write beyond frontier+lead wait for it. Writes inside the lowest part
// itself are always allowed, so a lead smaller than a part cannot stop the part
// that has to finish first.
type leadGate struct {
	lead     int64
	frontier atomic.Int64
	// limit is the highest offset a write may start at.
	limit atomic.Int64
	clock Clock
	stats *downloadStats
	// poke tells the scheduler loop that an attempt parked.
	poke func()

	mu      sync.Mutex
	changed chan struct{} // Closed and replaced by every advance.
	waiters map[*leadWaiter]struct{}
}

// leadWaiter is one parked attempt.
type leadWaiter struct {
	offset    int64
	cancel    context.CancelCauseFunc
	preempted bool
}

func newLeadGate(lead int64, clock Clock, stats *downloadStats, poke func()) *leadGate {
	return &leadGate{lead: lead, clock: clock, stats: stats, poke: poke,
		changed: make(chan struct{}), waiters: make(map[*leadWaiter]struct{})}
}

// advance moves the frontier and wakes every parked attempt. end is where the
// lowest unfinished part ends.
func (g *leadGate) advance(frontier, end int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.frontier.Store(frontier)
	g.limit.Store(max(frontier+g.lead, end-1))
	close(g.changed)
	g.changed = make(chan struct{})
}

// wait blocks until a write starting at offset is inside the lead, or ctx ends.
// Inside the lead it costs one atomic load. While it waits, the attempt is parked
// so that the watchdog ignores it.
func (g *leadGate) wait(ctx context.Context, offset int64, activity *AttemptActivity, waiter *leadWaiter) error {
	if g.lead <= 0 || offset <= g.limit.Load() {
		return nil
	}
	activity.Park()
	defer activity.Unpark()
	started := g.clock.Now()
	g.mu.Lock()
	waiter.offset = offset
	g.waiters[waiter] = struct{}{}
	g.mu.Unlock()
	g.poke()
	defer func() {
		g.mu.Lock()
		delete(g.waiters, waiter)
		g.mu.Unlock()
		g.stats.addPark(g.clock.Now().Sub(started))
	}()
	for {
		g.mu.Lock()
		if offset <= g.limit.Load() {
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// wasPreempted reports whether preempt cancelled this attempt. It is the only
// reliable sign, because the attempt's context may already have ended for another
// reason when preempt chose it.
func (g *leadGate) wasPreempted(w *leadWaiter) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return w.preempted
}

// preempt cancels the parked attempt with the highest offset that has not been
// preempted yet. It reports whether one was cancelled. Its written bytes are kept.
func (g *leadGate) preempt() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	var pick *leadWaiter
	for w := range g.waiters {
		if !w.preempted && (pick == nil || w.offset > pick.offset) {
			pick = w
		}
	}
	if pick == nil {
		return false
	}
	pick.preempted = true
	pick.cancel(ErrPartPreempted)
	return true
}

// advanceLowest moves lowest past finished parts and publishes the new frontier.
func (j *job) advanceLowest() {
	moved := false
	for j.lowest < len(j.parts) && j.parts[j.lowest].state == PartDone {
		j.lowest++
		moved = true
	}
	if moved {
		j.publishFrontier()
	}
}

// publishFrontier tells the gate where the lowest unfinished part is.
func (j *job) publishFrontier() {
	if j.gate == nil {
		return
	}
	frontier, end := j.totalBytes, j.totalBytes
	if j.lowest < j.partCount() {
		span := j.partRange(j.lowest)
		frontier, end = span.Offset, span.Offset+span.Length
	}
	j.gate.advance(frontier, end)
}

// preemptStarved breaks the deadlock the lead gate could cause. If a job's lowest
// unfinished part is waiting for a slot, and parked attempts of the same job hold
// the slots, one of them gives its slot up. At most one preemption per job is
// outstanding, so a burst of events cannot cancel more attempts than needed.
func (s *scheduler) preemptStarved() {
	for _, j := range s.slotted {
		if j.gate == nil || j.gate.lead <= 0 || j.state != JobRunning || j.step != stepTransfer || j.preempting > 0 {
			continue
		}
		if j.lowest >= len(j.parts) || j.parts[j.lowest].state != PartPending {
			continue
		}
		if j.inflight < s.settings.MaxPartsPerFile && s.activeParts < s.settings.MaxActiveParts {
			continue
		}
		if j.gate.preempt() {
			j.preempting++
			s.dlStats.preemptions.Add(1)
		}
	}
}

// ---- Prepare ----

type downloadPlan struct {
	info ObjectInfo
	plan PartPlan
	err  error
}

// startPrepareDownload heads the object and plans its parts.
func (s *scheduler) startPrepareDownload(j *job) {
	ref, preferred := j.spec.Ref, s.settings.PartSize
	ctx, cancel := context.WithTimeout(j.ctx, s.cfg.Timeouts.Control)
	s.spawnControl(j, false, func() func() {
		defer cancel()
		out := downloadPlan{}
		out.info, out.err = s.cfg.Store.HeadObject(ctx, ref)
		if out.err == nil {
			switch {
			case out.info.Size < 0:
				out.err = fmt.Errorf("%w: object size %d", ErrInvalidSize, out.info.Size)
			case out.info.ETag == "":
				out.err = fmt.Errorf("%w: S3 returned no ETag, so a consistent download cannot be guaranteed", ErrObjectChanged)
			default:
				out.plan, out.err = PlanParts(out.info.Size, preferred)
			}
		}
		class := ClassifyAttempt(ctx, out.err)
		return func() {
			if out.err != nil {
				s.controlFailed(j, class, out.err)
				return
			}
			j.etag, j.plan, j.totalBytes = out.info.ETag, out.plan, out.info.Size
			j.parts = make([]partEntry, out.plan.PartCount)
			j.dirty = true
			if j.totalBytes >= JournalTempMinSize {
				j.nextStep(stepJournalTemp)
			} else {
				j.nextStep(stepSinkPrepare)
			}
		}
	})
}

func (s *scheduler) tempEntry(j *job) journal.Entry {
	return journal.Entry{
		ID: j.journalID, Kind: journal.KindDownloadTemp, Phase: journal.PhaseActive,
		CreatedAt: s.cfg.Clock.Now().UTC(), TempPath: fsx.TempPath(j.spec.LocalPath, string(j.id)),
	}
}

// startJournalTemp records the temp file before it exists, so that a crash after
// creating it can be cleaned up. A failed write fails the job.
func (s *scheduler) startJournalTemp(j *job) {
	j.journalID = fmt.Sprintf("%s-%d", j.id, j.run)
	entry := s.tempEntry(j)
	s.spawnControl(j, false, func() func() {
		ctx, cancel := s.journalContext()
		defer cancel()
		err := s.cfg.Journal.Put(ctx, entry)
		return func() {
			if err != nil {
				s.fatal(j, fmt.Errorf("%w: %w", ErrJournal, err), 0)
				return
			}
			j.journaled = true
			j.nextStep(stepSinkPrepare)
		}
	})
}

// startSinkPrepare checks free space and collisions and creates the temp file.
func (s *scheduler) startSinkPrepare(j *job) {
	ctx := j.ctx
	spec := SinkSpec{JobID: j.id, DestPath: j.spec.LocalPath, Size: j.totalBytes}
	s.spawnControl(j, false, func() func() {
		writer, err := s.cfg.Sink.Prepare(ctx, spec)
		if err != nil {
			err = localError("prepare download", err)
		}
		class := ClassifyAttempt(ctx, err)
		return func() {
			if writer != nil {
				// Kept even if the job is being cancelled, so that cleanup aborts it.
				j.sink = writer
				j.gate = newLeadGate(writer.MaxLead(), s.cfg.Clock, &s.dlStats, s.nudge)
				j.publishFrontier()
			}
			if err != nil {
				s.controlFailed(j, class, err)
				return
			}
			j.dirty = true
			j.nextStep(stepTransfer)
		}
	})
}

// nudge wakes the loop. It never blocks.
func (s *scheduler) nudge() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// localError marks err as a local I/O failure, which is permanent. Errors that
// are cancellations, or already local, keep their class.
func localError(what string, err error) error {
	if errors.Is(err, ErrLocalIO) || Classify(err) == ClassCancelled {
		return err
	}
	return fmt.Errorf("%w: %s: %w", ErrLocalIO, what, err)
}

// ---- Part attempts ----

// startDownloadPart dispatches an attempt for part index. It resumes after the
// bytes an earlier attempt already wrote, so finished bytes are never fetched again.
func (s *scheduler) startDownloadPart(j *job, index int) {
	span := j.partRange(index)
	done := j.partial[int32(index)]
	ctx, cancel := context.WithCancelCause(j.ctx)
	activity := new(AttemptActivity)
	entry := s.watchdog.add(activity, cancel)
	attempt := &downloadAttempt{
		ctx: ctx, activity: activity, waiter: leadWaiter{cancel: cancel},
		limiter: s.download, transferred: &j.transferred, gate: j.gate, sink: j.sink,
	}
	request := GetRangeRequest{
		Ref: j.spec.Ref, Offset: span.Offset + done, Length: span.Length - done,
		IfMatch: j.etag, Size: j.totalBytes,
	}

	s.spawn(func() func() {
		defer cancel(nil)
		written, err := attempt.run(s.cfg.Store, request)
		class := ClassifyAttempt(ctx, err)
		preempted := attempt.gate.wasPreempted(&attempt.waiter)
		return func() {
			s.watchdog.remove(entry)
			s.onDownloadPartDone(j, index, written, preempted, class, err)
		}
	})
}

func (s *scheduler) onDownloadPartDone(j *job, index int, written int64, preempted bool, class Class, err error) {
	if preempted && j.preempting > 0 {
		j.preempting--
	}
	s.partReturned(j)
	p := &j.parts[index]
	span := j.partRange(index)
	total := j.partial[int32(index)] + written

	if class == ClassNone && total != span.Length {
		err = fmt.Errorf("%w: part %d wrote %d of %d bytes", ErrLocalIO, span.Number, total, span.Length)
		class = ClassPermanent
	}
	if class == ClassNone {
		delete(j.partial, int32(index))
		setPart(p, PartEvSucceed)
		j.partsDone++
		j.advanceLowest()
		s.partSucceeded(j)
		return
	}
	if written > 0 {
		if j.partial == nil {
			j.partial = make(map[int32]int64)
		}
		j.partial[int32(index)] = total
		// The next attempt resumes after these bytes, so an attempt that wrote
		// some is progress: it starts the part's budget and the outage clock
		// afresh. Otherwise a link that drops every few hundred MiB would fail a
		// large part while bytes were flowing the whole time.
		if class == ClassRetryable {
			p.attempts = 0
			j.lastProgress = s.cfg.Clock.Now()
		}
	}
	s.partFailed(j, index, class, err)
}

// downloadAttempt is one ranged GET and the writes it feeds.
type downloadAttempt struct {
	ctx      context.Context
	waiter   leadWaiter
	activity *AttemptActivity
	limiter  *rateLimiter
	gate     *leadGate
	sink     SinkWriter
	// transferred is the job's progress counter.
	transferred *atomic.Int64
}

// run fetches request and writes it at its offset. It returns the bytes it wrote,
// which stay in the file whatever the outcome.
func (d *downloadAttempt) run(store ObjectStore, request GetRangeRequest) (written int64, err error) {
	result, err := store.GetObjectRange(d.ctx, request)
	if err != nil {
		return 0, err
	}
	defer result.Body.Close()
	d.activity.SetPhase(AttemptReceive)

	buffer := downloadBuffers.Get().(*[]byte)
	defer downloadBuffers.Put(buffer)
	var pending int64
	// The remaining bytes are published on every path, so progress never lags the file.
	defer func() { d.transferred.Add(pending) }()

	offset, remaining := request.Offset, request.Length
	for remaining > 0 {
		if err = d.gate.wait(d.ctx, offset, d.activity, &d.waiter); err != nil {
			return written, err
		}
		want := int(min(int64(len(*buffer)), remaining))
		filled, readErr := d.fill(result.Body, (*buffer)[:want])
		if filled > 0 {
			n, writeErr := d.write((*buffer)[:filled], offset)
			offset += int64(n)
			remaining -= int64(n)
			written += int64(n)
			pending += int64(n)
			if writeErr != nil {
				return written, fmt.Errorf("%w: write at offset %d: %w", ErrLocalIO, offset, writeErr)
			}
		}
		if readErr != nil {
			return written, readErr
		}
		if pending >= MeterQuantum {
			n := pending
			pending = 0
			d.transferred.Add(n)
			if err = d.throttle(n); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// fill reads until p is full. A body that ends early is io.ErrUnexpectedEOF.
func (d *downloadAttempt) fill(body io.Reader, p []byte) (int, error) {
	filled := 0
	for filled < len(p) {
		n, err := body.Read(p[filled:])
		filled += n
		if n > 0 {
			d.activity.Touch()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if filled == len(p) {
					return filled, nil
				}
				return filled, io.ErrUnexpectedEOF
			}
			return filled, err
		}
	}
	return filled, nil
}

// write stores p at offset. The attempt is parked meanwhile: a slow disk is not a
// stalled connection, so the watchdog must not cancel it.
func (d *downloadAttempt) write(p []byte, offset int64) (int, error) {
	d.activity.Park()
	defer d.activity.Unpark()
	return d.sink.WriteAt(p, offset)
}

// throttle charges n bytes to the bandwidth limiter. It also notices a cancelled
// attempt, because some stores only stop when a Read fails.
func (d *downloadAttempt) throttle(n int64) error {
	if d.ctx.Err() != nil {
		return context.Cause(d.ctx)
	}
	if !d.limiter.limited.Load() {
		return nil
	}
	d.activity.Park()
	defer d.activity.Unpark()
	return d.limiter.WaitN(d.ctx, int(n))
}

// ---- Commit ----

// startCommit syncs the temp file and renames it into place. It runs under the
// engine's context, not the job's: a user cancel during finalizing is deferred
// until the outcome is known, as for CompleteMultipartUpload.
func (s *scheduler) startCommit(j *job) {
	if got := j.transferred.Load(); got != j.totalBytes {
		s.fatal(j, fmt.Errorf("%w: %d of %d bytes were written", ErrLocalIO, got, j.totalBytes), 0)
		s.settle(j)
		return
	}
	sink, ctx := j.sink, s.baseCtx
	s.spawnControl(j, false, func() func() {
		err := sink.Commit(ctx)
		if err != nil {
			err = localError("commit download", err)
		}
		class := ClassifyAttempt(ctx, err)
		return func() {
			switch {
			case err == nil:
				j.sink = nil // Committed, so nothing is left to abort.
				if j.journaled {
					j.nextStep(stepJournalDelete)
				} else {
					j.nextStep(stepFinish)
				}
			case class != ClassCancelled:
				s.fatal(j, err, 0)
			}
		}
	})
}

// ---- Cleanup ----

// startSinkAbort removes the temp file of a cancelling or failing download. A
// failed abort is retried, and then left to the journal for the next launch.
func (s *scheduler) startSinkAbort(j *job) {
	j.abortTries++
	sink, tries := j.sink, j.abortTries
	s.spawnControl(j, true, func() func() {
		err := sink.Abort()
		class := Classify(err)
		return func() {
			switch {
			case err == nil:
				j.sink = nil
				j.clean = cleanJournal
				j.abortTries = 0
			case class == ClassRetryable && !s.closing && tries < s.cfg.Retry.MaxAbortAttempts:
				j.ctlBackoff = true
				s.schedule(wake{when: s.cfg.Clock.Now().Add(s.retryDelay(tries, err)), kind: wakeControl, job: j, epoch: j.epoch})
			default:
				j.sink = nil
				j.keepJournal = j.journaled
				j.clean = cleanFinish
				j.addWarning("The temporary download file could not be removed now and will be removed at next launch.")
			}
		}
	})
}
