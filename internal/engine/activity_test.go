package engine

import "testing"

func TestAttemptActivity(t *testing.T) {
	var a AttemptActivity
	start, phase, parked := a.Observe()
	if phase != AttemptAwaitResponse || parked {
		t.Fatalf("zero value: phase %v parked %v", phase, parked)
	}

	a.Touch()
	a.SetPhase(AttemptSend)
	ticks, phase, _ := a.Observe()
	if ticks != start+2 || phase != AttemptSend {
		t.Fatalf("got ticks %d phase %v", ticks, phase)
	}

	a.Park()
	a.Park()
	a.Unpark()
	if _, _, parked := a.Observe(); !parked {
		t.Fatal("nested park released too early")
	}
	a.Unpark()
	if _, _, parked := a.Observe(); parked {
		t.Fatal("still parked")
	}
}

func BenchmarkAttemptActivityTouch(b *testing.B) {
	var a AttemptActivity
	for b.Loop() {
		a.Touch()
	}
}
