package engine

import (
	"container/heap"
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// jobStep is the next piece of work in an upload's path, in design section 7
// order. The scheduler re-enters the current step after a pause or credential
// wait, so every step can be started again.
type jobStep uint8

const (
	stepPrepare jobStep = iota
	stepJournalIntent
	stepCreate
	stepJournalActive
	stepTransfer
	stepVerifySource
	stepComplete
	stepHeadRecover
	stepJournalDelete
	stepFinish
)

// cleanStep is the next piece of work of a cancelling or failing job.
type cleanStep uint8

const (
	cleanAbort cleanStep = iota
	cleanJournal
	cleanFinish
)

// partEntry is one row of the part table. MemPerPartEntry budgets its size,
// including the ETag string's allocation.
type partEntry struct {
	etag     string
	crc      uint32
	attempts uint16
	state    PartState
}

// job is one transfer. Except for transferred, only the scheduler goroutine
// touches it.
type job struct {
	id    JobID
	spec  JobSpec
	state JobState
	phase JobPhase
	step  jobStep
	run   int

	// ctx governs the current admission. It is replaced each time the job is admitted.
	ctx    context.Context
	cancel context.CancelCauseFunc

	holdsSlot bool

	// Source and plan.
	handle     SourceHandle
	stat       SourceStat
	plan       PartPlan
	singlePut  bool
	totalBytes int64

	// Part table. parts is allocated once the size is known and released at a
	// terminal state. cursor is the lowest part never dispatched, and returned
	// holds lower parts that came back to pending.
	parts     []partEntry
	cursor    int
	returned  partHeap
	partsDone int
	inflight  int
	backoffs  int

	// epoch invalidates scheduled wakes when backoffs are interrupted.
	epoch uint32

	// Control request state. control counts requests in flight.
	control     int
	ctlAttempts int
	ctlBackoff  bool

	// Upload state.
	uploadID         string
	journalID        string
	journaled        bool
	uploadDone       bool
	completeTries    int
	completeInFlight bool
	composite        string

	// Cleanup state.
	clean           cleanStep
	abortTries      int
	keepJournal     bool
	cancelRequested bool

	// lastProgress is when the job last finished a part or entered JobRunning.
	lastProgress time.Time

	// Reporting.
	transferred   atomic.Int64
	retries       int
	jobErr        *JobError
	warning       string
	dirty         bool
	reportedBytes int64
	rateBytes     int64
	rateAt        time.Time
	rate          float64
}

// partCount returns how many parts the job sends. A single PUT is one part, even
// for a zero-byte file.
func (j *job) partCount() int {
	if j.singlePut {
		return 1
	}
	return j.plan.PartCount
}

// partRange returns the byte range of the part at zero-based index i.
func (j *job) partRange(i int) PartRange {
	if j.singlePut {
		return PartRange{Number: 1, Length: j.totalBytes}
	}
	return j.plan.Part(i)
}

// hasPendingPart reports whether a part can be dispatched now.
func (j *job) hasPendingPart() bool {
	return len(j.returned) > 0 || j.cursor < len(j.parts)
}

// nextPart removes and returns the lowest-numbered pending part.
func (j *job) nextPart() int {
	if len(j.returned) > 0 {
		return int(heap.Pop(&j.returned).(int32))
	}
	i := j.cursor
	j.cursor++
	return i
}

// returnPart makes a part dispatchable again.
func (j *job) returnPart(i int) {
	heap.Push(&j.returned, int32(i))
}

// drained reports whether nothing of this job is running.
func (j *job) drained() bool {
	return j.inflight == 0 && j.control == 0
}

// resetRun prepares a failed job to start from nothing.
func (j *job) resetRun() {
	j.phase = PhasePreparing
	j.step = stepPrepare
	j.handle = nil
	j.plan = PartPlan{}
	j.singlePut = false
	j.parts = nil
	j.cursor = 0
	j.returned = nil
	j.partsDone = 0
	j.backoffs = 0
	j.ctlAttempts = 0
	j.ctlBackoff = false
	j.uploadID = ""
	j.journaled = false
	j.uploadDone = false
	j.completeTries = 0
	j.completeInFlight = false
	j.composite = ""
	j.clean = cleanAbort
	j.abortTries = 0
	j.keepJournal = false
	j.cancelRequested = false
	j.transferred.Store(0)
	j.reportedBytes = 0
	j.rateBytes = 0
	j.rate = 0
	j.retries = 0
	j.jobErr = nil
	j.warning = ""
	j.epoch++
}

func (j *job) addWarning(text string) {
	if j.warning == "" {
		j.warning = text
	} else {
		j.warning += " " + text
	}
	j.dirty = true
}

func (j *job) snapshot() JobSnapshot {
	snapshot := JobSnapshot{
		ID:          j.id,
		Spec:        j.spec,
		State:       j.state,
		Phase:       j.phase,
		TotalBytes:  j.totalBytes,
		DoneBytes:   j.transferred.Load(),
		BytesPerSec: int64(j.rate),
		PartsDone:   j.partsDone,
		PartsTotal:  j.partCount(),
		Retries:     j.retries,
		Warning:     j.warning,
	}
	if j.jobErr != nil {
		copied := *j.jobErr
		snapshot.Err = &copied
	}
	return snapshot
}

// newJobError builds the user-facing error. OpError text holds no credentials.
func newJobError(err error, part int32) *JobError {
	out := &JobError{Class: Classify(err), Message: err.Error(), PartNumber: part}
	var opErr *OpError
	if errors.As(err, &opErr) {
		out.Code = opErr.Code
	}
	return out
}

// partHeap is a min-heap of zero-based part indexes.
type partHeap []int32

func (h partHeap) Len() int           { return len(h) }
func (h partHeap) Less(a, b int) bool { return h[a] < h[b] }
func (h partHeap) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *partHeap) Push(x any)        { *h = append(*h, x.(int32)) }
func (h *partHeap) Pop() any {
	old := *h
	last := len(old) - 1
	x := old[last]
	*h = old[:last]
	return x
}

// wakeKind says what a scheduled wake does.
type wakeKind uint8

const (
	wakePart wakeKind = iota
	wakeControl
	wakeProbe
)

// wake is one entry of the scheduler's single timer heap. No goroutine sleeps for
// a retry.
type wake struct {
	when  time.Time
	seq   uint64
	kind  wakeKind
	job   *job
	part  int32
	epoch uint32
	probe *probeState
}

type wakeHeap []wake

func (h wakeHeap) Len() int { return len(h) }
func (h wakeHeap) Less(a, b int) bool {
	if h[a].when.Equal(h[b].when) {
		return h[a].seq < h[b].seq
	}
	return h[a].when.Before(h[b].when)
}
func (h wakeHeap) Swap(a, b int) { h[a], h[b] = h[b], h[a] }
func (h *wakeHeap) Push(x any)   { *h = append(*h, x.(wake)) }
func (h *wakeHeap) Pop() any {
	old := *h
	last := len(old) - 1
	x := old[last]
	old[last] = wake{}
	*h = old[:last]
	return x
}
