package engine

import "testing"

const (
	jobStateCount  = int(JobCompleted) + 1
	jobEventCount  = int(JobEvFinished) + 1
	partStateCount = int(PartFailed) + 1
	partEventCount = int(PartEvReset) + 1
)

// TestJobTransitionsExhaustive fixes the whole table, so an edge cannot be added
// or removed without also updating docs/engine-design.md and this list.
func TestJobTransitionsExhaustive(t *testing.T) {
	want := map[jobEdge]JobState{
		{JobQueued, JobEvAdmit}:                       JobRunning,
		{JobQueued, JobEvPause}:                       JobPaused,
		{JobQueued, JobEvCancel}:                      JobCancelling,
		{JobRunning, JobEvPause}:                      JobPausing,
		{JobRunning, JobEvCancel}:                     JobCancelling,
		{JobRunning, JobEvCredentialError}:            JobCredentialWait,
		{JobRunning, JobEvFatal}:                      JobFailing,
		{JobRunning, JobEvFinished}:                   JobCompleted,
		{JobPausing, JobEvDrained}:                    JobPaused,
		{JobPausing, JobEvCancel}:                     JobCancelling,
		{JobPausing, JobEvFatal}:                      JobFailing,
		{JobPaused, JobEvResume}:                      JobQueued,
		{JobPaused, JobEvCancel}:                      JobCancelling,
		{JobCredentialWait, JobEvCredentialsRestored}: JobRunning,
		{JobCredentialWait, JobEvPause}:               JobPausing,
		{JobCredentialWait, JobEvCancel}:              JobCancelling,
		{JobCredentialWait, JobEvFatal}:               JobFailing,
		{JobCredentialWait, JobEvFinished}:            JobCompleted,
		{JobCancelling, JobEvCleanupDone}:             JobCancelled,
		{JobFailing, JobEvCleanupDone}:                JobFailed,
		{JobFailed, JobEvRetry}:                       JobQueued,
	}
	for s := range jobStateCount {
		for ev := range jobEventCount {
			edge := jobEdge{JobState(s), JobEvent(ev)}
			got, ok := NextJobState(edge.from, edge.ev)
			wantState, wantOK := want[edge]
			if ok != wantOK || got != wantState {
				t.Errorf("%v + event %d: got %v/%v, want %v/%v", edge.from, ev, got, ok, wantState, wantOK)
			}
		}
	}
}

func TestEveryNonTerminalJobStateCanReachTerminal(t *testing.T) {
	for s := range jobStateCount {
		state := JobState(s)
		if !reachesTerminal(state, map[JobState]bool{}) {
			t.Errorf("%v cannot reach a terminal state", state)
		}
	}
}

func reachesTerminal(s JobState, seen map[JobState]bool) bool {
	if s.IsTerminal() {
		return true
	}
	seen[s] = true
	for ev := range jobEventCount {
		if next, ok := NextJobState(s, JobEvent(ev)); ok && !seen[next] && reachesTerminal(next, seen) {
			return true
		}
	}
	return false
}

func TestOnlyFailedLeavesTerminal(t *testing.T) {
	for _, s := range []JobState{JobCancelled, JobCompleted} {
		for ev := range jobEventCount {
			if next, ok := NextJobState(s, JobEvent(ev)); ok {
				t.Errorf("%v + event %d moved to %v", s, ev, next)
			}
		}
	}
}

func TestPartTransitionsExhaustive(t *testing.T) {
	want := map[partEdge]PartState{
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
	for s := range partStateCount {
		for ev := range partEventCount {
			edge := partEdge{PartState(s), PartEvent(ev)}
			got, ok := NextPartState(edge.from, edge.ev)
			wantState, wantOK := want[edge]
			if ok != wantOK || got != wantState {
				t.Errorf("%v + event %d: got %v/%v, want %v/%v", edge.from, ev, got, ok, wantState, wantOK)
			}
		}
	}
}

func TestStateNames(t *testing.T) {
	for s := range jobStateCount {
		if JobState(s).String() == "unknown" || JobState(s).String() == "" {
			t.Errorf("job state %d has no name", s)
		}
	}
	for s := range partStateCount {
		if PartState(s).String() == "unknown" || PartState(s).String() == "" {
			t.Errorf("part state %d has no name", s)
		}
	}
}
