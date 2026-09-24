package processor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/processor/phase"
)

type runRecorder struct {
	runner *Runner
	mu     sync.Mutex

	saveFailureLogged bool
}

func (r *runRecorder) TaskIteration(failed bool) {
	r.update(func(record *RunRecord) {
		r.runner.currentTasks.Iterations++
		record.Tasks.Iterations++
		if failed {
			r.runner.currentTasks.FailedRetries++
			record.Tasks.FailedRetries++
		}
	})
}

func (r *runRecorder) InternalReviewDone(loopIterations int, endedBy string) {
	r.update(func(record *RunRecord) {
		record.InternalReview.FirstRan = true
		record.InternalReview.LoopIterations = loopIterations
		record.InternalReview.EndedBy = endedBy
	})
}

func (r *runRecorder) ExternalIteration(index int, key, label, reviewerOutput, evaluatorResponse string) {
	r.update(func(record *RunRecord) {
		reviewer := externalRecord(record, key, label)
		boundedReviewerOutput, reviewerTruncated := truncateForRecord(reviewerOutput)
		boundedEvaluatorResponse, evaluatorTruncated := truncateForRecord(evaluatorResponse)
		reviewer.Iterations = append(reviewer.Iterations, ExternalIterationRecord{
			Index: index, ReviewerOutput: boundedReviewerOutput, EvaluatorResponse: boundedEvaluatorResponse,
			Truncated: reviewerTruncated || evaluatorTruncated,
		})
	})
}

func (r *runRecorder) ExternalDone(done phase.ReviewerCompletion) {
	r.update(func(record *RunRecord) {
		key := done.Reviewer.Tool + ":" + done.Reviewer.ModelSpec
		reviewer := externalRecord(record, key, done.Label)
		reviewer.Duration = Duration(done.Duration)
		reviewer.EndedBy = done.EndedBy
		reviewer.HadFindings = done.HadFindings
	})
}

func (r *runRecorder) PostReviewDone(iterations int) {
	r.update(func(record *RunRecord) {
		record.PostReview.Ran = true
		record.PostReview.Iterations = iterations
	})
}

func externalRecord(record *RunRecord, key, label string) *ExternalReviewerRecord {
	for i := range record.External {
		if record.External[i].Key == key {
			if label != "" {
				record.External[i].Label = label
			}
			return &record.External[i]
		}
	}
	record.External = append(record.External, ExternalReviewerRecord{Key: key, Label: label})
	return &record.External[len(record.External)-1]
}

func (r *runRecorder) update(mutate func(*RunRecord)) {
	if r == nil || r.runner == nil {
		return
	}
	r.mu.Lock()
	mutate(&r.runner.record)
	boundExternalReviewText(r.runner.record.External)
	r.runner.snapshotRunTimings()
	record := cloneRunRecord(r.runner.record)
	r.mu.Unlock()
	r.save(record)
}

func (r *runRecorder) save(record RunRecord) {
	store := r.runner.recordStore
	if store == nil {
		return
	}
	if err := store.Save(record); err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.saveFailureLogged {
			r.runner.log.Print("run record: save failed: %v", err)
			r.saveFailureLogged = true
		}
	}
}

func cloneRunRecord(record RunRecord) RunRecord {
	cloned := record
	if record.Validation != nil {
		validation := *record.Validation
		cloned.Validation = &validation
	}
	if record.PhaseDurations != nil {
		cloned.PhaseDurations = make(map[string]Duration, len(record.PhaseDurations))
		maps.Copy(cloned.PhaseDurations, record.PhaseDurations)
	}
	cloned.External = make([]ExternalReviewerRecord, len(record.External))
	for i := range record.External {
		cloned.External[i] = record.External[i]
		cloned.External[i].Iterations = append([]ExternalIterationRecord(nil), record.External[i].Iterations...)
	}
	return cloned
}

func (r *Runner) prepareReviewResume(ctx context.Context) {
	switch r.cfg.Mode {
	case ModeReview, ModeCodexOnly:
		r.resume = r.loadReviewResume(ctx)
		r.resumeReady = true
	default:
		r.resume = reviewResume{}
		r.resumeReady = false
	}
}

func (r *Runner) startRunRecord() {
	r.invocationStarted = time.Now().UTC()
	r.currentTasks = TaskRunRecord{}
	r.priorPhaseDurations = nil
	r.priorValidation = ValidationRunRecord{}
	r.loadedRecord = false
	if r.recorder == nil {
		r.recorder = &runRecorder{runner: r}
		if r.deps != nil {
			r.deps.Recorder = r.recorder
		}
	}

	fresh := r.newRunRecord()
	if stored, found := r.loadRunRecord(); found {
		switch {
		case r.resumeReady && reviewResumeHasProgress(r.resume):
			fresh = stored
			reconcileRunRecord(&fresh, r.resume, reviewerKeys(r.cfg))
		case r.cfg.Mode == ModeFull:
			r.loadedRecord = true
			fresh = stored
		}
	}
	r.record = fresh
	boundExternalReviewText(r.record.External)
	r.priorPhaseDurations = maps.Clone(fresh.PhaseDurations)
	if fresh.Validation != nil {
		r.priorValidation = *fresh.Validation
	}
	r.fillRunRecordFields()
	r.recorder.save(cloneRunRecord(r.record))
}

func (r *Runner) loadRunRecord() (RunRecord, bool) {
	if r.recordStore == nil {
		return RunRecord{}, false
	}
	stored, found, err := r.recordStore.Load()
	if err != nil {
		r.log.Print("run record: load failed: %v", err)
		return RunRecord{}, false
	}
	if !found || stored.Version != runRecordVersion {
		return RunRecord{}, false
	}
	branch, err := r.currentBranch()
	if err != nil {
		r.log.Print("run record: current branch failed: %v", err)
		return RunRecord{}, false
	}
	return stored, stored.Branch == branch
}

func (r *Runner) finishRunRecord() {
	if r.recorder == nil {
		return
	}
	r.recorder.mu.Lock()
	r.snapshotRunTimings()
	r.record.FinishedAt = time.Now().UTC()
	record := cloneRunRecord(r.record)
	r.recorder.mu.Unlock()
	r.recorder.save(record)
}

func (r *Runner) resetRunRecord() {
	r.loadedRecord = false
	r.priorPhaseDurations = nil
	r.priorValidation = ValidationRunRecord{}
	if r.recordStore != nil {
		if err := r.recordStore.Remove(); err != nil {
			r.log.Print("run record: removal failed: %v", err)
		}
	}
	r.record = r.newRunRecord()
	r.record.Tasks = r.currentTasks
	r.fillRunRecordFields()
}

func (r *Runner) adoptLoadedRunRecord() {
	if !r.loadedRecord {
		return
	}
	if reviewResumeHasProgress(r.resume) {
		// Task events may already have added this invocation's timings. Trim
		// only the historical snapshot, then add current measurements once.
		r.record.PhaseDurations = maps.Clone(r.priorPhaseDurations)
		reconcileRunRecord(&r.record, r.resume, reviewerKeys(r.cfg))
		r.priorPhaseDurations = maps.Clone(r.record.PhaseDurations)
		r.snapshotRunTimings()
		r.loadedRecord = false
		r.recorder.save(cloneRunRecord(r.record))
		return
	}
	r.record = r.newRunRecord()
	r.record.Tasks = r.currentTasks
	r.loadedRecord = false
	r.priorPhaseDurations = nil
	r.priorValidation = ValidationRunRecord{}
	r.fillRunRecordFields()
	if r.recorder != nil {
		r.recorder.save(cloneRunRecord(r.record))
	}
}

// reconcileRunRecord keeps only review outcomes backed by honored checkpoints.
// A changed chain or reset HEAD can invalidate a suffix while retaining internal
// review or earlier reviewers; stale assessments must not enter the new report.
func reconcileRunRecord(record *RunRecord, resume reviewResume, keys []string) {
	var external []ExternalReviewerRecord
	for _, key := range keys[:min(resume.completedReviewers, len(keys))] {
		for _, reviewer := range record.External {
			if reviewer.Key == key {
				external = append(external, reviewer)
				break
			}
		}
	}
	if len(external) != len(record.External) {
		// These aggregate buckets cannot be split by reviewer. Omit the old
		// measurements rather than attach invalidated stages to the new chain.
		delete(record.PhaseDurations, "external review")
		delete(record.PhaseDurations, "evaluation")
	}
	record.External = external
	if !resume.skipInternal {
		record.InternalReview = InternalReviewRunRecord{}
		delete(record.PhaseDurations, "internal review")
	}
	if !resume.skipPostReview {
		if record.PostReview.Ran {
			// Post-review shares the internal-review timer bucket.
			delete(record.PhaseDurations, "internal review")
		}
		record.PostReview = PostReviewRunRecord{}
	}
	delete(record.PhaseDurations, "other") // finalize/report always run again
	record.Report = ""
}

func (r *Runner) newRunRecord() RunRecord {
	record := RunRecord{Version: runRecordVersion, StartedAt: time.Now().UTC()}
	if !r.invocationStarted.IsZero() {
		record.StartedAt = r.invocationStarted
	}
	if branch, err := r.currentBranch(); err == nil {
		record.Branch = branch
	}
	return record
}

func (r *Runner) currentBranch() (string, error) {
	if r.git == nil {
		return "", errors.New("git checker not configured")
	}
	branch, err := r.git.CurrentBranch()
	if err != nil {
		return "", fmt.Errorf("resolve current branch: %w", err)
	}
	return branch, nil
}

func (r *Runner) fillRunRecordFields() {
	r.record.Version = runRecordVersion
	r.record.Plan = r.cfg.PlanFile
	r.record.BaseRef = r.cfg.DefaultBranch
	r.record.Mode = r.cfg.Mode
	r.record.Executor = r.cfg.taskProvider()
	r.record.TaskModel = r.cfg.TaskModel
	r.record.ReviewModel = r.cfg.ReviewModel
	if r.record.ReviewModel == "" {
		r.record.ReviewModel = r.record.TaskModel
	}
	r.record.FinishedAt = time.Time{}
	if r.record.StartedAt.IsZero() {
		r.record.StartedAt = time.Now().UTC()
	}
}

func reviewResumeHasProgress(resume reviewResume) bool {
	return resume.skipInternal || resume.completedReviewers > 0 || resume.skipPostReview
}
