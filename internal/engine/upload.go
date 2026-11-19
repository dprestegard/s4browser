package engine

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"

	"s4browser/internal/journal"
)

// errBadPartResponse makes a part response without a usable ETag or CRC32 a
// retryable failure, because Complete needs both values.
var errBadPartResponse = errors.New("engine: part response has no usable ETag or CRC32")

// ---- Part attempts ----

// startPart dispatches the lowest pending part of j.
func (s *scheduler) startPart(j *job) {
	index := j.nextPart()
	setPart(&j.parts[index], PartEvDispatch)
	j.inflight++
	s.activeParts++
	s.busy++

	if j.spec.Direction == Download {
		s.startDownloadPart(j, index)
		return
	}

	span := j.partRange(index)
	ctx, cancel := context.WithCancelCause(j.ctx)
	activity := new(AttemptActivity)
	entry := s.watchdog.add(activity, cancel)
	body := &meteredBody{
		ctx:         ctx,
		section:     io.NewSectionReader(j.handle, span.Offset, span.Length),
		length:      span.Length,
		activity:    activity,
		limiter:     s.upload,
		transferred: &j.transferred,
		clock:       s.cfg.Clock,
		sendBuffer:  s.sendBuffer,
		connRate:    &s.connRate,
	}
	ref, uploadID, single := j.spec.Ref, j.uploadID, j.singlePut
	started := s.cfg.Clock.Now()

	s.spawn(func() func() {
		defer cancel(nil)
		var result PutResult
		var err error
		if single {
			result, err = s.cfg.Store.PutObject(ctx, PutObjectRequest{Ref: ref, Body: body, ContentLength: span.Length})
		} else {
			result, err = s.cfg.Store.UploadPart(ctx, UploadPartRequest{
				Ref: ref, UploadID: uploadID, PartNumber: span.Number, Body: body, ContentLength: span.Length,
			})
		}
		if local := body.localFailure(); local != nil {
			err = local
		}
		var sum uint32
		if err == nil {
			var ok bool
			if sum, ok = decodeCRC32(result.ChecksumCRC32); !ok || result.ETag == "" {
				err = errBadPartResponse
			}
		}
		class := ClassifyAttempt(ctx, err)
		stalledAwaiting := body.sent.Load() && errors.Is(context.Cause(ctx), ErrStalled)
		return func() {
			s.watchdog.remove(entry)
			s.observeUpload(span.Length, started, class == ClassNone, stalledAwaiting)
			s.onPartDone(j, index, body.retire(), result.ETag, sum, class, err)
		}
	})
}

// observeUpload maintains the per-connection upload rate estimate that sizes
// the drain allowance (meteredBody.drainAllowance).
//
// A finished attempt of at least one send buffer gives a sample: its length over
// its whole duration, which includes connecting and the server's latency, so it
// is lower than the link and the allowance it gives is generous. Samples are
// averaged. An attempt the watchdog stopped after its whole body was read gives
// an upper bound instead, and the estimate is halved below it. Repeated stalls
// therefore keep doubling the allowance until it covers the real drain time,
// while a dead connection is still detected within MaxDrainAllowance.
func (s *scheduler) observeUpload(length int64, started time.Time, ok, stalledAwaiting bool) {
	elapsed := s.cfg.Clock.Now().Sub(started).Seconds()
	if elapsed <= 0 {
		return
	}
	sample := float64(length) / elapsed
	switch {
	case ok && length >= s.sendBuffer:
		if s.connRateEst == 0 {
			s.connRateEst = sample
		} else {
			s.connRateEst = 0.75*s.connRateEst + 0.25*sample
		}
	case stalledAwaiting && length >= MeterQuantum:
		if s.connRateEst == 0 || sample < s.connRateEst {
			s.connRateEst = sample
		}
		s.connRateEst /= 2
	default:
		return
	}
	s.connRate.Store(max(int64(s.connRateEst), 1))
}

func decodeCRC32(encoded string) (uint32, bool) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != crc32.Size {
		return 0, false
	}
	return binary.BigEndian.Uint32(raw), true
}

func encodeCRC32(sum uint32) string {
	var raw [crc32.Size]byte
	binary.BigEndian.PutUint32(raw[:], sum)
	return base64.StdEncoding.EncodeToString(raw[:])
}

func (s *scheduler) onPartDone(j *job, index int, published int64, etag string, sum uint32, class Class, err error) {
	s.partReturned(j)
	p := &j.parts[index]
	span := j.partRange(index)

	if class == ClassNone {
		setPart(p, PartEvSucceed)
		p.etag, p.crc = etag, sum
		j.partsDone++
		j.transferred.Add(span.Length - published)
		s.partSucceeded(j)
		return
	}
	j.transferred.Add(-published)
	s.partFailed(j, index, class, err)
}

// partReturned gives back the slot of a finished attempt.
func (s *scheduler) partReturned(j *job) {
	j.inflight--
	s.activeParts--
	s.busy--
}

// partSucceeded runs after a part is recorded as done.
func (s *scheduler) partSucceeded(j *job) {
	j.lastProgress = s.cfg.Clock.Now()
	j.dirty = true
	s.requestSucceeded(j)
	s.settle(j)
}

// partFailed applies the error policy to a part whose attempt did not succeed.
func (s *scheduler) partFailed(j *job, index int, class Class, err error) {
	p := &j.parts[index]
	span := j.partRange(index)
	interrupt := func() {
		setPart(p, PartEvInterrupt)
		j.returnPart(index)
	}
	switch class {
	case ClassCancelled:
		interrupt()
	case ClassCredential:
		interrupt()
		s.onCredentialError(j)
	case ClassPermanent:
		setPart(p, PartEvFail)
		s.fatal(j, err, span.Number)
	case ClassRetryable:
		if j.state != JobRunning && j.state != JobCredentialWait {
			interrupt()
			break
		}
		if p.attempts < ^uint16(0) {
			p.attempts++
		}
		j.retries++
		j.dirty = true
		now := s.cfg.Clock.Now()
		if s.cfg.Retry.Exhausted(int(p.attempts), s.cfg.Retry.MaxPartAttempts, now.Sub(j.lastProgress)) {
			setPart(p, PartEvFail)
			s.fatal(j, err, span.Number)
			break
		}
		setPart(p, PartEvRetry)
		j.backoffs++
		s.schedule(wake{when: now.Add(s.retryDelay(int(p.attempts), err)), kind: wakePart, job: j, part: int32(index), epoch: j.epoch})
	}
	s.settle(j)
}

func (s *scheduler) retryDelay(attempt int, err error) time.Duration {
	var throttled bool
	var retryAfter time.Duration
	var opErr *OpError
	if errors.As(err, &opErr) {
		throttled, retryAfter = opErr.Throttled, opErr.RetryAfter
	}
	return s.cfg.Retry.Delay(attempt, throttled, retryAfter, s.cfg.Jitter())
}

// ---- Control requests ----

// spawnControl runs one request that is not a part attempt. apply runs on the
// loop, and the job is settled afterwards. Cleanup requests do not count as busy,
// because shutdown waits for busy work before it starts aborting.
func (s *scheduler) spawnControl(j *job, cleanup bool, work func() func()) {
	j.control++
	if !cleanup {
		s.busy++
	}
	s.spawn(func() func() {
		apply := work()
		return func() {
			j.control--
			if !cleanup {
				s.busy--
			}
			apply()
			s.settle(j)
		}
	})
}

// controlFailed applies the retry policy to a failed control request.
func (s *scheduler) controlFailed(j *job, class Class, err error) {
	switch class {
	case ClassCredential:
		s.onCredentialError(j)
	case ClassPermanent:
		s.fatal(j, err, 0)
	case ClassRetryable:
		if j.state != JobRunning && j.state != JobCredentialWait {
			return
		}
		j.ctlAttempts++
		j.retries++
		j.dirty = true
		now := s.cfg.Clock.Now()
		if s.cfg.Retry.Exhausted(j.ctlAttempts, s.cfg.Retry.MaxControlAttempts, now.Sub(j.lastProgress)) {
			s.fatal(j, err, 0)
			return
		}
		j.ctlBackoff = true
		s.schedule(wake{when: now.Add(s.retryDelay(j.ctlAttempts, err)), kind: wakeControl, job: j, epoch: j.epoch})
	}
}

// advance starts the job's current step.
func (s *scheduler) advance(j *job) {
	if j.control > 0 || j.ctlBackoff {
		return
	}
	for {
		switch j.step {
		case stepPrepare:
			if j.spec.Direction == Download {
				s.startPrepareDownload(j)
			} else {
				s.startPrepare(j)
			}
		case stepJournalIntent:
			s.startJournalPut(j, journal.PhaseCreating)
		case stepCreate:
			s.startCreate(j)
		case stepJournalActive:
			s.startJournalPut(j, journal.PhaseActive)
		case stepTransfer:
			j.phase = PhaseTransferring
			if j.partsDone < j.partCount() {
				return
			}
			j.phase = PhaseFinalizing
			j.step = stepVerifySource
			if j.spec.Direction == Download {
				j.step = stepCommit
			}
			j.dirty = true
			continue
		case stepVerifySource:
			s.startVerifySource(j)
		case stepComplete:
			s.startComplete(j)
		case stepHeadRecover:
			s.startHeadRecover(j)
		case stepJournalTemp:
			s.startJournalTemp(j)
		case stepSinkPrepare:
			s.startSinkPrepare(j)
		case stepCommit:
			s.startCommit(j)
		case stepJournalDelete:
			s.startJournalDelete(j)
		case stepFinish:
			if s.fire(j, JobEvFinished) {
				s.retire(j)
			}
		}
		return
	}
}

// nextStep moves to step with a fresh retry budget.
func (j *job) nextStep(step jobStep) {
	j.step = step
	j.ctlAttempts = 0
}

// ---- Prepare ----

type prepared struct {
	handle SourceHandle
	stat   SourceStat
	plan   PartPlan
	single bool
	err    error
}

func (s *scheduler) startPrepare(j *job) {
	ctx, spec, preferred := j.ctx, j.spec, s.settings.PartSize
	s.spawnControl(j, false, func() func() {
		out := s.prepare(ctx, spec, preferred)
		class := ClassifyAttempt(ctx, out.err)
		return func() {
			if out.err != nil {
				s.controlFailed(j, class, out.err)
				return
			}
			j.handle, j.stat, j.plan, j.singlePut = out.handle, out.stat, out.plan, out.single
			j.totalBytes = out.stat.Size
			j.parts = make([]partEntry, j.partCount())
			j.dirty = true
			if j.singlePut {
				j.nextStep(stepTransfer)
			} else {
				j.nextStep(stepJournalIntent)
			}
		}
	})
}

func (s *scheduler) prepare(ctx context.Context, spec JobSpec, preferred int64) prepared {
	if s.cfg.Guard != nil {
		if err := s.cfg.Guard.CheckWrite(spec.Ref.Key); err != nil {
			return prepared{err: err}
		}
	}
	handle, err := s.cfg.Source.Open(ctx, spec.LocalPath)
	if err != nil {
		return prepared{err: fmt.Errorf("%w: open source: %w", ErrLocalIO, err)}
	}
	stat, err := handle.Stat()
	if err != nil {
		_ = handle.Close()
		return prepared{err: fmt.Errorf("%w: stat source: %w", ErrLocalIO, err)}
	}
	out := prepared{handle: handle, stat: stat, single: UseSinglePut(stat.Size)}
	if !out.single {
		if out.plan, err = PlanParts(stat.Size, preferred); err != nil {
			_ = handle.Close()
			return prepared{err: err}
		}
	}
	return out
}

// ---- Journal ----

func (s *scheduler) journalEntry(j *job, phase journal.Phase) journal.Entry {
	ref := j.spec.Ref
	return journal.Entry{
		ID: j.journalID, Kind: journal.KindMultipartUpload, Phase: phase, CreatedAt: s.cfg.Clock.Now().UTC(),
		Profile: ref.Profile, Region: ref.Region, Bucket: ref.Bucket, Key: ref.Key, UploadID: j.uploadID,
	}
}

func (s *scheduler) journalContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.cfg.Timeouts.Control)
}

// startJournalPut records the intent before Create, or the upload ID after it.
// A failed write fails the job. After Create, cleanup then aborts the upload.
func (s *scheduler) startJournalPut(j *job, phase journal.Phase) {
	if j.journalID == "" || phase == journal.PhaseCreating {
		j.journalID = fmt.Sprintf("%s-%d", j.id, j.run)
	}
	entry := s.journalEntry(j, phase)
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
			if phase == journal.PhaseCreating {
				j.nextStep(stepCreate)
			} else {
				j.nextStep(stepTransfer)
			}
		}
	})
}

func (s *scheduler) startJournalDelete(j *job) {
	id := j.journalID
	s.spawnControl(j, false, func() func() {
		ctx, cancel := s.journalContext()
		defer cancel()
		err := s.cfg.Journal.Delete(ctx, id)
		return func() {
			if err != nil {
				// Next launch's cleanup finds nothing left to do and removes the entry.
				j.addWarning(journalWarning(j))
			}
			j.journaled = false
			j.nextStep(stepFinish)
		}
	})
}

// ---- Create ----

// createInDoubt reports whether a failed Create may still have created an upload.
func createInDoubt(err error) bool {
	if errors.Is(err, ErrCredentialsUnavailable) {
		return false
	}
	var opErr *OpError
	if errors.As(err, &opErr) {
		return opErr.Status == 0
	}
	return true
}

// startCreate sends CreateMultipartUpload. It runs detached from the job's
// context, so a user cancel waits for the upload ID and can abort that upload.
//
// A request that ends with no response may have created an upload whose ID is
// lost. Its intent is kept as a separate journal entry, so launch cleanup finds it.
func (s *scheduler) startCreate(j *job) {
	ref := j.spec.Ref
	ctx, cancel := context.WithTimeout(s.baseCtx, s.cfg.Timeouts.Control)
	orphan := s.journalEntry(j, journal.PhaseCreating)
	orphan.ID = fmt.Sprintf("%s-c%d", j.journalID, j.ctlAttempts+1)
	s.spawnControl(j, false, func() func() {
		defer cancel()
		uploadID, err := s.cfg.Store.CreateMultipartUpload(ctx, ref)
		class := ClassifyAttempt(ctx, err)
		var journalErr error
		if err != nil && createInDoubt(err) {
			putCtx, putCancel := s.journalContext()
			journalErr = s.cfg.Journal.Put(putCtx, orphan)
			putCancel()
		}
		return func() {
			if journalErr != nil {
				j.addWarning("An upload that may have been started could not be recorded and may remain on S3.")
			}
			if err != nil {
				s.controlFailed(j, class, err)
				return
			}
			j.uploadID = uploadID
			j.nextStep(stepJournalActive)
		}
	})
}

// ---- Finalize ----

func (s *scheduler) startVerifySource(j *job) {
	handle, want, single := j.handle, j.stat, j.singlePut
	s.spawnControl(j, false, func() func() {
		got, err := handle.Stat()
		if err != nil {
			err = fmt.Errorf("%w: stat source: %w", ErrLocalIO, err)
		} else if got.Size != want.Size || !got.ModTime.Equal(want.ModTime) {
			err = fmt.Errorf("%w: size or modification time differs", ErrSourceChanged)
		}
		return func() {
			switch {
			case err != nil:
				if single {
					j.addWarning("The object on S3 may not match the local file, because the file changed during upload.")
				}
				s.fatal(j, err, 0)
			case single:
				j.nextStep(stepFinish)
			default:
				j.nextStep(stepComplete)
			}
		}
	})
}

func (s *scheduler) startComplete(j *job) {
	parts := make([]CompletedPart, len(j.parts))
	checksums := make([]string, len(j.parts))
	for i := range j.parts {
		sum := encodeCRC32(j.parts[i].crc)
		parts[i] = CompletedPart{Number: int32(i + 1), ETag: j.parts[i].etag, ChecksumCRC32: sum}
		checksums[i] = sum
	}
	composite, err := CompositeCRC32(checksums)
	if err != nil {
		s.fatal(j, err, 0)
		s.settle(j)
		return
	}
	j.composite = composite
	j.completeTries++
	j.completeInFlight = true
	retried := j.completeTries > 1
	request := CompleteUploadRequest{Ref: j.spec.Ref, UploadID: j.uploadID, Parts: parts, ObjectSize: j.totalBytes}
	ctx, cancel := context.WithTimeout(s.baseCtx, s.cfg.Timeouts.Complete)
	s.spawnControl(j, false, func() func() {
		defer cancel()
		result, err := s.cfg.Store.CompleteMultipartUpload(ctx, request)
		class := ClassifyAttempt(ctx, err)
		return func() {
			j.completeInFlight = false
			switch {
			case err == nil:
				s.onCompleted(j, result.ChecksumCRC32)
			case retried && isNoSuchUpload(err):
				// An earlier Complete may have succeeded and lost its response.
				j.nextStep(stepHeadRecover)
			default:
				s.controlFailed(j, class, err)
			}
		}
	})
}

func isNoSuchUpload(err error) bool {
	var opErr *OpError
	return errors.As(err, &opErr) && opErr.Code == "NoSuchUpload"
}

func (s *scheduler) onCompleted(j *job, checksum string) {
	j.uploadDone = true
	j.keepJournal = false
	if checksum != j.composite {
		j.addWarning("The object exists on S3, but its checksum does not match the local file.")
		s.fatal(j, fmt.Errorf("%w: S3 reported %q, expected %q", ErrChecksumMismatch, checksum, j.composite), 0)
		return
	}
	j.nextStep(stepJournalDelete)
}

// startHeadRecover checks whether a Complete whose response was lost succeeded.
func (s *scheduler) startHeadRecover(j *job) {
	ref := j.spec.Ref
	ctx, cancel := context.WithTimeout(s.baseCtx, s.cfg.Timeouts.Control)
	s.spawnControl(j, false, func() func() {
		defer cancel()
		info, err := s.cfg.Store.HeadObject(ctx, ref)
		class := ClassifyAttempt(ctx, err)
		return func() {
			if err != nil {
				s.controlFailed(j, class, err)
				return
			}
			s.onCompleted(j, info.ChecksumCRC32)
		}
	})
}

// ---- Cleanup ----

// cleanup drains a cancelling or failing job, aborts its upload, removes its
// journal entry and ends it.
func (s *scheduler) cleanup(j *job) {
	if !j.drained() || j.ctlBackoff {
		return
	}
	if s.closing && s.busy > 0 {
		return // Shutdown step 3: all attempts exit before any abort starts.
	}
	for {
		switch j.clean {
		case cleanAbort:
			if j.spec.Direction == Download {
				if j.sink == nil {
					j.clean = cleanJournal
					continue
				}
				s.startSinkAbort(j)
				return
			}
			if j.uploadID == "" || j.uploadDone {
				j.clean = cleanJournal
				continue
			}
			if j.keepJournal {
				j.addWarning("The upload's outcome is unknown, so it will be resolved at next launch.")
				j.clean = cleanFinish
				continue
			}
			s.startAbort(j)
			return
		case cleanJournal:
			if !j.journaled || j.keepJournal {
				j.clean = cleanFinish
				continue
			}
			s.startCleanupDelete(j)
			return
		case cleanFinish:
			if s.fire(j, JobEvCleanupDone) {
				s.retire(j)
			}
			return
		}
	}
}

func (s *scheduler) startAbort(j *job) {
	j.abortTries++
	ref, uploadID, tries := j.spec.Ref, j.uploadID, j.abortTries
	ctx, cancel := context.WithTimeout(s.abortCtx, s.cfg.Timeouts.Control)
	s.spawnControl(j, true, func() func() {
		defer cancel()
		err := s.cfg.Store.AbortMultipartUpload(ctx, ref, uploadID)
		class := ClassifyAttempt(ctx, err)
		return func() {
			switch {
			case err == nil:
				j.clean = cleanJournal
			case class == ClassRetryable && !s.closing && tries < s.cfg.Retry.MaxAbortAttempts:
				j.ctlBackoff = true
				s.schedule(wake{when: s.cfg.Clock.Now().Add(s.retryDelay(tries, err)), kind: wakeControl, job: j, epoch: j.epoch})
			default:
				j.keepJournal = true
				j.clean = cleanFinish
				j.addWarning("The multipart upload could not be aborted now and will be aborted at next launch.")
			}
		}
	})
}

func (s *scheduler) startCleanupDelete(j *job) {
	id := j.journalID
	s.spawnControl(j, true, func() func() {
		ctx, cancel := s.journalContext()
		defer cancel()
		err := s.cfg.Journal.Delete(ctx, id)
		return func() {
			if err != nil {
				j.addWarning(journalWarning(j))
			}
			j.journaled = false
			j.clean = cleanFinish
		}
	})
}

func journalWarning(j *job) string {
	if j.spec.Direction == Download {
		return "The transfer journal could not be updated, so this download's temporary file will be checked again at next launch."
	}
	return "The transfer journal could not be updated, so this upload will be checked again at next launch."
}

// retire releases everything a job held once it reaches a terminal state.
func (s *scheduler) retire(j *job) {
	if j.handle != nil {
		_ = j.handle.Close()
		j.handle = nil
	}
	if j.cancel != nil {
		j.cancel(ErrJobCancelled)
	}
	j.parts, j.returned, j.partial, j.gate = nil, nil, nil, nil
	s.releaseSlot(j)
	s.open--
}
