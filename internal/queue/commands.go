package queue

import (
	"context"
	"errors"

	"s4browser/internal/engine"
)

// command runs one engine command on one job.
func (s *Service) command(ctx context.Context, id string, apply func(context.Context, engine.JobID) error) error {
	return describe(apply(ctx, engine.JobID(id)))
}

// Pause pauses one job. Finished parts are kept.
func (s *Service) Pause(ctx context.Context, id string) error {
	return s.command(ctx, id, s.eng.Pause)
}

// Resume queues a paused job again. It continues at its first unfinished part.
func (s *Service) Resume(ctx context.Context, id string) error {
	return s.command(ctx, id, s.eng.Resume)
}

// Cancel cancels one job. An upload's multipart upload is aborted.
func (s *Service) Cancel(ctx context.Context, id string) error {
	return s.command(ctx, id, s.eng.Cancel)
}

// Retry starts a failed job again from the beginning.
func (s *Service) Retry(ctx context.Context, id string) error {
	return s.command(ctx, id, s.eng.Retry)
}

// Remove forgets one finished job.
func (s *Service) Remove(ctx context.Context, id string) error {
	s.hide(engine.JobID(id))
	if err := s.command(ctx, id, s.eng.Remove); err != nil {
		s.unhide(engine.JobID(id))
		return err
	}
	s.emitRemoved([]string{id})
	return nil
}

// each applies cmd to every job that accepts it, in submission order, and
// returns how many it changed. A job that moved on in the meantime is skipped.
func (s *Service) each(ctx context.Context, accepts func(JobView) bool, cmd func(context.Context, engine.JobID) error) (int, error) {
	snaps, err := s.eng.Snapshot(ctx)
	if err != nil {
		return 0, describe(err)
	}
	n := 0
	for _, j := range snaps {
		if !accepts(view(j)) {
			continue
		}
		switch err := cmd(ctx, j.ID); {
		case err == nil:
			n++
		case errors.Is(err, engine.ErrIllegalState), errors.Is(err, engine.ErrUnknownJob):
		default:
			return n, describe(err)
		}
	}
	return n, nil
}

// PauseAll pauses every job that can be paused and returns how many.
func (s *Service) PauseAll(ctx context.Context) (int, error) {
	return s.each(ctx, func(v JobView) bool { return v.CanPause }, s.eng.Pause)
}

// ResumeAll resumes every paused job and returns how many.
func (s *Service) ResumeAll(ctx context.Context) (int, error) {
	return s.each(ctx, func(v JobView) bool { return v.CanResume }, s.eng.Resume)
}

// CancelAll cancels every job that is not finished and returns how many.
func (s *Service) CancelAll(ctx context.Context) (int, error) {
	return s.each(ctx, func(v JobView) bool { return v.CanCancel }, s.eng.Cancel)
}

// ClearFinished forgets completed and cancelled jobs, and failed ones too when
// includeFailed is set, and returns how many it removed. Failed jobs stay by
// default because the user may want to retry them.
func (s *Service) ClearFinished(ctx context.Context, includeFailed bool) (int, error) {
	snaps, err := s.eng.Snapshot(ctx)
	if err != nil {
		return 0, describe(err)
	}
	var cleared []string
	for _, j := range snaps {
		if !j.State.IsTerminal() || (j.State == engine.JobFailed && !includeFailed) {
			continue
		}
		s.hide(j.ID)
		switch err := s.eng.Remove(ctx, j.ID); {
		case err == nil:
			cleared = append(cleared, string(j.ID))
		case errors.Is(err, engine.ErrIllegalState), errors.Is(err, engine.ErrUnknownJob):
			s.unhide(j.ID)
		default:
			s.unhide(j.ID)
			s.emitRemoved(cleared)
			return len(cleared), describe(err)
		}
	}
	s.emitRemoved(cleared)
	return len(cleared), nil
}

// hide stops events from carrying a job that is being removed. It is set before
// the removal, so a batch built earlier cannot bring the job back.
func (s *Service) hide(id engine.JobID) {
	s.mu.Lock()
	s.removed[id] = struct{}{}
	s.mu.Unlock()
}

func (s *Service) unhide(id engine.JobID) {
	s.mu.Lock()
	delete(s.removed, id)
	s.mu.Unlock()
}
