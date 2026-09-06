package processor

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/umputun/ralphex/pkg/processor/phase"
)

func (r *Runner) loadReviewResume(ctx context.Context) reviewResume {
	if r.checkpoints == nil || r.git == nil {
		return reviewResume{}
	}

	cp, found, err := r.checkpoints.Load()
	if err != nil {
		r.log.Print("review checkpoint unreadable, starting reviews from scratch: %v", err)
		return reviewResume{}
	}
	if !found {
		return reviewResume{}
	}
	if !r.reviewTreeIsClean("load") {
		return reviewResume{}
	}

	branch, err := r.git.CurrentBranch()
	if err != nil {
		r.log.Print("review checkpoint unreadable, starting reviews from scratch: resolve current branch: %v", err)
		return reviewResume{}
	}
	head, err := r.git.HeadHash()
	if err != nil {
		r.log.Print("review checkpoint unreadable, starting reviews from scratch: %v", err)
		return reviewResume{}
	}
	if cp.TaskStartedAtHead != "" && cp.TaskStartedAtHead != head {
		r.clearReviewCheckpoint("interrupted task phase committed new work")
		return reviewResume{}
	}
	resume, err := resolveReviewResume(cp, reviewResumeInput{
		Mode: r.cfg.Mode, Branch: branch, Plan: r.cfg.PlanFile, Reviewers: reviewerKeys(r.cfg), Head: head,
	}, func(revision string) (bool, error) {
		return r.git.ContainsRevisionContext(ctx, revision)
	})
	if err != nil {
		r.log.Print("review checkpoint unreadable, starting reviews from scratch: %v", err)
		return reviewResume{}
	}
	for _, note := range resume.notes {
		r.log.Print("review checkpoint: %s", note)
	}
	return resume
}

func (r *Runner) saveReviewStage(_ context.Context, stage ReviewStage) {
	if r.checkpoints == nil || r.git == nil {
		return
	}

	if !r.reviewTreeIsClean("save") {
		return
	}

	head, err := r.git.HeadHash()
	if err != nil {
		r.log.Print("review checkpoint save skipped: %v", err)
		return
	}
	branch, err := r.git.CurrentBranch()
	if err != nil {
		r.log.Print("review checkpoint save skipped: resolve current branch: %v", err)
		return
	}
	cp, found, err := r.checkpoints.Load()
	switch {
	case errors.Is(err, ErrReviewCheckpointCorrupt):
		r.log.Print("review checkpoint is corrupt; replacing it")
		cp = ReviewCheckpoint{}
	case err != nil:
		r.log.Print("review checkpoint save skipped: load existing checkpoint: %v", err)
		return
	case !found:
		cp = ReviewCheckpoint{}
	case cp.Version != reviewCheckpointVersion || cp.Mode != r.cfg.Mode || cp.Branch != branch ||
		reviewPlanKey(cp.Plan) != reviewPlanKey(r.cfg.PlanFile):
		cp = ReviewCheckpoint{}
	case !slices.Equal(cp.Reviewers, reviewerKeys(r.cfg)):
		internal := make([]ReviewStage, 0, 1)
		for _, saved := range cp.Stages {
			if saved.Stage == reviewStageInternal {
				internal = append(internal, saved)
				break
			}
		}
		cp.Stages = internal
	}

	cp.Version = reviewCheckpointVersion
	cp.Mode = r.cfg.Mode
	cp.Branch = branch
	cp.Plan = r.cfg.PlanFile
	cp.Reviewers = slices.Clone(reviewerKeys(r.cfg))
	stage.Head = head
	stage.CompletedAt = time.Now().UTC()

	cp.Stages = mergeReviewStage(cp.Stages, stage, r.cfg.Mode, cp.Reviewers)
	if err := r.checkpoints.Save(cp); err != nil {
		r.log.Print("review checkpoint save failed: %v", err)
	}
}

func mergeReviewStage(saved []ReviewStage, stage ReviewStage, mode Mode, reviewers []string) []ReviewStage {
	target, ok := reviewStagePosition(stage, mode, reviewers)
	if !ok {
		return nil
	}

	merged := make([]ReviewStage, 0, target+1)
	for position := range target {
		if position >= len(saved) || !reviewStageAtPosition(saved[position], position, mode, reviewers) {
			return merged
		}
		merged = append(merged, saved[position])
	}
	return append(merged, stage)
}

func reviewStagePosition(stage ReviewStage, mode Mode, reviewers []string) (int, bool) {
	offset := 0
	if mode != ModeCodexOnly {
		if stage.Stage == reviewStageInternal {
			return 0, true
		}
		offset = 1
	}

	switch stage.Stage {
	case reviewStageExternal:
		if stage.Index < 0 || stage.Index >= len(reviewers) || stage.Reviewer != reviewers[stage.Index] {
			return 0, false
		}
		return offset + stage.Index, true
	case reviewStagePostReview:
		return offset + len(reviewers), true
	default:
		return 0, false
	}
}

func reviewStageAtPosition(stage ReviewStage, position int, mode Mode, reviewers []string) bool {
	offset := 0
	if mode != ModeCodexOnly {
		if position == 0 {
			return stage.Stage == reviewStageInternal
		}
		offset = 1
	}

	index := position - offset
	return index >= 0 && index < len(reviewers) && stage.Stage == reviewStageExternal &&
		stage.Index == index && stage.Reviewer == reviewers[index]
}

func (r *Runner) reviewTreeIsClean(action string) bool {
	dirty, err := r.git.IsDirtyAll()
	if err != nil {
		if action == "load" {
			r.log.Print("review checkpoint unreadable, starting reviews from scratch: %v", err)
		} else {
			r.log.Print("review checkpoint save skipped: %v", err)
		}
		return false
	}
	if dirty {
		if action == "load" {
			r.log.Print("review checkpoint: uncommitted changes; starting reviews from scratch")
		} else {
			r.log.Print("review checkpoint skipped: uncommitted changes")
		}
		return false
	}
	return true
}

func reviewPlanKey(plan string) string {
	if plan == "" {
		return ""
	}
	return filepath.Clean(plan)
}

func (r *Runner) reviewResumeAfterTask(ctx context.Context, before string, beforeErr error, alreadyInvalidated bool) reviewResume {
	if r.invalidateReviewAfterTask(before, beforeErr, alreadyInvalidated) {
		return reviewResume{}
	}
	return r.loadReviewResume(ctx)
}

func (r *Runner) invalidateReviewAfterTask(before string, beforeErr error, alreadyInvalidated bool) bool {
	if r.git == nil {
		return alreadyInvalidated
	}
	if beforeErr != nil {
		r.clearReviewCheckpoint("task phase HEAD could not be verified")
		return true
	}
	after, err := r.git.HeadHash()
	if err != nil {
		r.clearReviewCheckpoint("task phase HEAD could not be verified")
		return true
	}
	if after != before {
		r.clearReviewCheckpoint("task phase committed new work")
		return true
	}
	if alreadyInvalidated {
		return true
	}
	return !r.clearReviewTaskMarker(before)
}

// markReviewTaskStarted records the pre-task HEAD before task execution begins. If a previous
// process died during the task phase, a changed HEAD proves that its task committed new work and
// makes every saved review stage stale. The marker deliberately preserves the checkpoint's mode
// and stages so tasks-only runs can guard a full-mode checkpoint stored at the same path.
func (r *Runner) markReviewTaskStarted(head string, headErr error) bool {
	if r.checkpoints == nil || r.git == nil {
		return false
	}
	if headErr != nil {
		r.clearReviewCheckpoint("task phase HEAD could not be verified")
		return true
	}

	cp, found, err := r.checkpoints.Load()
	if err != nil {
		r.clearReviewCheckpoint("task phase checkpoint could not be read")
		return true
	}
	if !found {
		return false
	}
	if cp.TaskStartedAtHead != "" && cp.TaskStartedAtHead != head {
		r.clearReviewCheckpoint("interrupted task phase committed new work")
		return true
	}

	cp.TaskStartedAtHead = head
	if err := r.checkpoints.Save(cp); err != nil {
		r.log.Print("review checkpoint task marker save failed: %v", err)
		r.clearReviewCheckpoint("task phase could not be guarded")
		return true
	}
	return false
}

// clearReviewTaskMarker completes the durable task guard after a task invocation that did not
// change HEAD. A failure invalidates resume for this run rather than risking stale review stages.
func (r *Runner) clearReviewTaskMarker(startingHead string) bool {
	if r.checkpoints == nil {
		return true
	}
	cp, found, err := r.checkpoints.Load()
	if err != nil {
		r.clearReviewCheckpoint("task phase checkpoint could not be read")
		return false
	}
	if !found || cp.TaskStartedAtHead == "" {
		return true
	}
	if cp.TaskStartedAtHead != startingHead {
		r.clearReviewCheckpoint("task phase guard did not match starting HEAD")
		return false
	}
	cp.TaskStartedAtHead = ""
	if err := r.checkpoints.Save(cp); err != nil {
		r.log.Print("review checkpoint task marker clear failed: %v", err)
		r.clearReviewCheckpoint("task phase guard could not be cleared")
		return false
	}
	return true
}

func (r *Runner) onReviewerDone(ctx context.Context, done phase.ReviewerCompletion) error {
	if r.checkpoints == nil {
		return nil
	}
	keys := reviewerKeys(r.cfg)
	if done.Index < 0 || done.Index >= len(keys) {
		r.log.Print("review checkpoint save skipped: reviewer index %d is outside configured chain", done.Index)
		return nil
	}
	r.saveReviewStage(ctx, ReviewStage{
		Stage: reviewStageExternal, Index: done.Index, Reviewer: keys[done.Index],
		HadFindings: done.HadFindings, EndedBy: done.EndedBy,
	})
	return nil
}

func (r *Runner) clearReviewCheckpoint(reason string) {
	if r.checkpoints == nil {
		return
	}
	if err := r.checkpoints.Remove(); err != nil {
		r.log.Print("review checkpoint removal failed: %v", err)
		return
	}
	if reason != "" {
		r.log.Print("review checkpoint: cleared: %s", reason)
	}
}
