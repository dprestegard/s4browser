package engine

// rateSmoothing is the weight of the newest sample in BytesPerSec.
const rateSmoothing = 0.3

// runProgress delivers one batch per ProgressInterval. It runs apart from the
// loop, so a slow Observer delays later batches but never the data path.
func (s *scheduler) runProgress() {
	ticker := s.cfg.Clock.NewTicker(ProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			reply := make(chan ProgressBatch, 1)
			select {
			case s.batches <- reply:
				s.deliver(<-reply)
			case <-s.progressStop:
				s.deliver(<-s.finalBatch)
				return
			}
		case <-s.progressStop:
			s.deliver(<-s.finalBatch)
			return
		}
	}
}

func (s *scheduler) deliver(batch ProgressBatch) {
	if s.cfg.Observer != nil && (len(batch.Jobs) > 0 || len(batch.Transitions) > 0 || batch.Resync) {
		s.cfg.Observer.OnProgress(batch)
	}
}

// buildBatch collects what changed since the previous batch.
func (s *scheduler) buildBatch() ProgressBatch {
	now := s.cfg.Clock.Now()
	var jobs []JobSnapshot
	for _, j := range s.order {
		current := j.transferred.Load()
		previousRate := int64(j.rate)
		if j.holdsSlot {
			if elapsed := now.Sub(j.rateAt).Seconds(); !j.rateAt.IsZero() && elapsed > 0 {
				sample := max(float64(current-j.rateBytes)/elapsed, 0)
				j.rate = rateSmoothing*sample + (1-rateSmoothing)*j.rate
			}
			j.rateAt, j.rateBytes = now, current
		} else {
			j.rate, j.rateAt = 0, now
		}
		if j.dirty || current != j.reportedBytes || int64(j.rate) != previousRate {
			jobs = append(jobs, j.snapshot())
			j.dirty = false
			j.reportedBytes = current
		}
	}
	batch := ProgressBatch{At: now, Jobs: jobs, Transitions: s.transitions, Resync: s.resync}
	s.transitions, s.resync = nil, false
	if len(batch.Jobs) > 0 || len(batch.Transitions) > 0 || batch.Resync {
		s.batchSeq++
		batch.Seq = s.batchSeq
	}
	return batch
}
