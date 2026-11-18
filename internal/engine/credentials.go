package engine

import (
	"context"
	"time"
)

// probeState tracks credential recovery for one profile. At most one probe runs
// at a time.
type probeState struct {
	profile   string
	interval  time.Duration
	scheduled bool
	running   bool
	epoch     uint32
}

func (s *scheduler) probeFor(profile string) *probeState {
	p := s.probes[profile]
	if p == nil {
		p = &probeState{profile: profile}
		s.probes[profile] = p
	}
	return p
}

// onCredentialError moves every running job on the profile to credential-wait.
// Nothing is aborted, and attempts already in flight keep going.
func (s *scheduler) onCredentialError(j *job) {
	profile := j.spec.Ref.Profile
	for _, other := range s.slotted {
		if other.spec.Ref.Profile == profile {
			s.fire(other, JobEvCredentialError)
		}
	}
	p := s.probeFor(profile)
	if !p.scheduled && !p.running {
		s.scheduleProbe(p)
	}
}

// scheduleProbe plans the next probe. The interval doubles every time and resets
// only when a request succeeds, so a probe that passes while requests still fail
// does not cause a tight loop.
func (s *scheduler) scheduleProbe(p *probeState) {
	if p.interval == 0 {
		p.interval = s.cfg.Timeouts.ProbeMin
	}
	p.epoch++
	p.scheduled = true
	s.schedule(wake{when: s.cfg.Clock.Now().Add(p.interval), kind: wakeProbe, probe: p, epoch: p.epoch})
	p.interval = min(p.interval*2, s.cfg.Timeouts.ProbeMax)
}

func (s *scheduler) runProbe(p *probeState) {
	if p.running || s.closing {
		return
	}
	p.running = true
	p.scheduled = false
	ctx, cancel := context.WithTimeout(s.baseCtx, s.cfg.Timeouts.Control)
	s.spawn(func() func() {
		defer cancel()
		err := s.cfg.Prober.Probe(ctx, p.profile)
		return func() {
			p.running = false
			s.onProbeDone(p, err)
		}
	})
}

func (s *scheduler) onProbeDone(p *probeState, err error) {
	if s.closing || !s.anyWaiting(p.profile) {
		return
	}
	if err != nil {
		s.scheduleProbe(p)
		return
	}
	now := s.cfg.Clock.Now()
	for _, j := range s.slotted {
		if j.spec.Ref.Profile == p.profile && s.fire(j, JobEvCredentialsRestored) {
			j.lastProgress = now
			s.settle(j)
		}
	}
}

func (s *scheduler) anyWaiting(profile string) bool {
	for _, j := range s.slotted {
		if j.spec.Ref.Profile == profile && j.state == JobCredentialWait {
			return true
		}
	}
	return false
}

// requestSucceeded ends the probe back-off for a profile.
func (s *scheduler) requestSucceeded(j *job) {
	if p := s.probes[j.spec.Ref.Profile]; p != nil {
		p.interval = 0
	}
}
