package engine

// JobState is the lifecycle state of a transfer job. The scheduler goroutine is
// the only writer.
type JobState uint8

const (
	// JobQueued waits for an active-file slot.
	JobQueued JobState = iota
	// JobRunning holds a slot and may dispatch work.
	JobRunning
	// JobPausing has cancelled its in-flight attempts and waits for them to exit.
	JobPausing
	// JobPaused holds no slot and no in-flight work. Completed parts are kept.
	JobPaused
	// JobCredentialWait holds its slot, dispatches nothing and awaits a
	// successful credential probe. Completed parts are kept. Its slot does not
	// count against MaxActiveFiles, so other profiles' jobs still start.
	JobCredentialWait
	// JobCancelling drains attempts, then aborts the upload or removes the temp file.
	JobCancelling
	// JobFailing is JobCancelling caused by a fatal error.
	JobFailing
	// JobCancelled is terminal.
	JobCancelled
	// JobFailed is terminal unless the user retries, which starts from nothing.
	JobFailed
	// JobCompleted is terminal.
	JobCompleted
)

var jobStateNames = [...]string{
	JobQueued:         "queued",
	JobRunning:        "running",
	JobPausing:        "pausing",
	JobPaused:         "paused",
	JobCredentialWait: "credential-wait",
	JobCancelling:     "cancelling",
	JobFailing:        "failing",
	JobCancelled:      "cancelled",
	JobFailed:         "failed",
	JobCompleted:      "completed",
}

func (s JobState) String() string {
	if int(s) < len(jobStateNames) {
		return jobStateNames[s]
	}
	return "unknown"
}

// IsTerminal reports whether the job has released all resources.
func (s JobState) IsTerminal() bool {
	return s == JobCancelled || s == JobFailed || s == JobCompleted
}

// JobEvent is an input to the job state machine.
type JobEvent uint8

const (
	// JobEvAdmit: the scheduler granted an active-file slot.
	JobEvAdmit JobEvent = iota
	// JobEvPause: the user asked to pause, or a download found its drive full.
	JobEvPause
	// JobEvResume: the user asked to resume a paused job.
	JobEvResume
	// JobEvCancel: the user asked to cancel, or the engine is closing.
	JobEvCancel
	// JobEvRetry: the user asked to run a failed job again.
	JobEvRetry
	// JobEvDrained: the last in-flight attempt of a pausing job has exited.
	JobEvDrained
	// JobEvCredentialError: a request of this job or of another job on the same
	// profile returned ClassCredential.
	JobEvCredentialError
	// JobEvCredentialsRestored: a credential probe for the job's profile passed.
	JobEvCredentialsRestored
	// JobEvFatal: a ClassPermanent error, or an exhausted retry budget.
	JobEvFatal
	// JobEvCleanupDone: attempts have drained and remote and local cleanup finished.
	JobEvCleanupDone
	// JobEvFinished: the object is complete and durable at its destination.
	JobEvFinished
)

type jobEdge struct {
	from JobState
	ev   JobEvent
}

var jobTransitions = map[jobEdge]JobState{
	{JobQueued, JobEvAdmit}:  JobRunning,
	{JobQueued, JobEvPause}:  JobPaused,
	{JobQueued, JobEvCancel}: JobCancelling,

	{JobRunning, JobEvPause}:           JobPausing,
	{JobRunning, JobEvCancel}:          JobCancelling,
	{JobRunning, JobEvCredentialError}: JobCredentialWait,
	{JobRunning, JobEvFatal}:           JobFailing,
	{JobRunning, JobEvFinished}:        JobCompleted,

	{JobPausing, JobEvDrained}: JobPaused,
	{JobPausing, JobEvCancel}:  JobCancelling,
	{JobPausing, JobEvFatal}:   JobFailing,

	{JobPaused, JobEvResume}: JobQueued,
	{JobPaused, JobEvCancel}: JobCancelling,

	{JobCredentialWait, JobEvCredentialsRestored}: JobRunning,
	{JobCredentialWait, JobEvPause}:               JobPausing,
	{JobCredentialWait, JobEvCancel}:              JobCancelling,
	{JobCredentialWait, JobEvFatal}:               JobFailing,
	// A CompleteMultipartUpload already in flight may succeed during the wait.
	{JobCredentialWait, JobEvFinished}: JobCompleted,

	{JobCancelling, JobEvCleanupDone}: JobCancelled,
	{JobFailing, JobEvCleanupDone}:    JobFailed,

	{JobFailed, JobEvRetry}: JobQueued,
}

// NextJobState returns the state reached by applying ev to s. ok is false when
// the transition is illegal. Engine commands turn that into ErrIllegalState.
// Asynchronous events (drained, credential error, fatal) are ignored instead,
// because they can legitimately race with a command that already moved the job.
func NextJobState(s JobState, ev JobEvent) (next JobState, ok bool) {
	next, ok = jobTransitions[jobEdge{s, ev}]
	return next, ok
}

// JobPhase is the stage of work inside JobRunning and JobCredentialWait.
type JobPhase uint8

const (
	// PhasePreparing opens the source or sink and creates the upload or heads the object.
	PhasePreparing JobPhase = iota
	// PhaseTransferring moves parts.
	PhaseTransferring
	// PhaseFinalizing completes the upload or commits the temp file. Pause
	// returns ErrIllegalState here, and Cancel is deferred until the outcome is known.
	PhaseFinalizing
)

// PartState is the lifecycle state of one part.
type PartState uint8

const (
	// PartPending is eligible for dispatch.
	PartPending PartState = iota
	// PartInFlight has exactly one attempt running.
	PartInFlight
	// PartBackoff waits for its retry time. It holds no concurrency slot.
	PartBackoff
	// PartDone is finished and is never sent again in this job run.
	PartDone
	// PartFailed exhausted its budget or hit a permanent error.
	PartFailed
)

var partStateNames = [...]string{
	PartPending:  "pending",
	PartInFlight: "in-flight",
	PartBackoff:  "backoff",
	PartDone:     "done",
	PartFailed:   "failed",
}

func (s PartState) String() string {
	if int(s) < len(partStateNames) {
		return partStateNames[s]
	}
	return "unknown"
}

// PartEvent is an input to the part state machine.
type PartEvent uint8

const (
	// PartEvDispatch: the scheduler started an attempt.
	PartEvDispatch PartEvent = iota
	// PartEvSucceed: the attempt finished and its result was recorded.
	PartEvSucceed
	// PartEvRetry: a ClassRetryable failure with budget left. Charges one attempt.
	PartEvRetry
	// PartEvBackoffElapsed: the retry timer fired.
	PartEvBackoffElapsed
	// PartEvInterrupt: the attempt ended through pause, cancel, preemption or a
	// credential error, or a backoff was dropped. Charges no attempt.
	PartEvInterrupt
	// PartEvFail: a ClassPermanent failure, or a retryable one with no budget left.
	PartEvFail
	// PartEvReset: the user retried a failed job, whose remote state is gone.
	PartEvReset
)

type partEdge struct {
	from PartState
	ev   PartEvent
}

var partTransitions = map[partEdge]PartState{
	{PartPending, PartEvDispatch}:       PartInFlight,
	{PartInFlight, PartEvSucceed}:       PartDone,
	{PartInFlight, PartEvRetry}:         PartBackoff,
	{PartInFlight, PartEvInterrupt}:     PartPending,
	{PartInFlight, PartEvFail}:          PartFailed,
	{PartBackoff, PartEvBackoffElapsed}: PartPending,
	{PartBackoff, PartEvInterrupt}:      PartPending,
	{PartPending, PartEvReset}:          PartPending,
	{PartDone, PartEvReset}:             PartPending,
	{PartFailed, PartEvReset}:           PartPending,
}

// NextPartState returns the state reached by applying ev to s. ok is false when
// the transition is illegal, which always indicates a scheduler bug.
func NextPartState(s PartState, ev PartEvent) (next PartState, ok bool) {
	next, ok = partTransitions[partEdge{s, ev}]
	return next, ok
}
