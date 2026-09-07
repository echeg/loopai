package processor

import (
	"maps"
	"time"
)

// SetRunTimingsSource supplies cumulative timing measurements for the current invocation.
// The callback returns phase durations, validation duration, and validation command count.
func (r *Runner) SetRunTimingsSource(source func() (map[string]time.Duration, time.Duration, int)) {
	r.timingsSource = source
}

func (r *Runner) reportRunRecord() RunRecord {
	if r.recorder != nil {
		r.recorder.mu.Lock()
		defer r.recorder.mu.Unlock()
	}
	r.snapshotRunTimings()
	// Report metadata measures completion through finalize; finishRunRecord later
	// includes the report session itself in the durable record's final timestamp.
	r.record.FinishedAt = time.Now().UTC()
	return cloneRunRecord(r.record)
}

// snapshotRunTimings replaces the current invocation's measurements while adding
// the honored checkpoint's historical measurements exactly once. Callers hold
// the recorder lock when recording events or finishing the run.
func (r *Runner) snapshotRunTimings() {
	if r.timingsSource == nil {
		return
	}
	phases, validationDuration, validationRuns := r.timingsSource()
	r.record.PhaseDurations = make(map[string]Duration, len(phases)+len(r.priorPhaseDurations))
	maps.Copy(r.record.PhaseDurations, r.priorPhaseDurations)
	for name, duration := range phases {
		r.record.PhaseDurations[name] += Duration(duration)
	}
	r.record.Validation = &ValidationRunRecord{
		Duration: r.priorValidation.Duration + Duration(validationDuration),
		Runs:     r.priorValidation.Runs + validationRuns,
	}
}
