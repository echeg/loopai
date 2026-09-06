package processor

import (
	"context"
	"fmt"
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

	branch, _ := r.git.CurrentBranch()
	head, err := r.git.HeadHash()
	if err != nil {
		r.log.Print("review checkpoint unreadable, starting reviews from scratch: %v", err)
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
	branch, _ := r.git.CurrentBranch()
	cp, found, err := r.checkpoints.Load()
	switch {
	case err != nil || !found:
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

	replaced := false
	for index := range cp.Stages {
		if sameReviewStageKey(cp.Stages[index], stage) {
			cp.Stages = append(cp.Stages[:index], stage)
			replaced = true
			break
		}
	}
	if !replaced {
		cp.Stages = append(cp.Stages, stage)
	}
	if err := r.checkpoints.Save(cp); err != nil {
		r.log.Print("review checkpoint save failed: %v", err)
	}
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

func (r *Runner) reviewResumeAfterTask(ctx context.Context, before string, beforeErr error) reviewResume {
	if r.invalidateReviewAfterTask(before, beforeErr) {
		return reviewResume{}
	}
	return r.loadReviewResume(ctx)
}

func (r *Runner) invalidateReviewAfterTask(before string, beforeErr error) bool {
	if r.git == nil {
		return false
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
	return false
}

func sameReviewStageKey(left, right ReviewStage) bool {
	if left.Stage != right.Stage {
		return false
	}
	if left.Stage != reviewStageExternal {
		return true
	}
	return left.Index == right.Index && left.Reviewer == right.Reviewer
}

func (r *Runner) onReviewerDone(ctx context.Context, done phase.ReviewerCompletion) error {
	if r.checkpoints == nil {
		return nil
	}
	keys := reviewerKeys(r.cfg)
	if done.Index < 0 || done.Index >= len(keys) {
		return fmt.Errorf("reviewer index %d is outside configured chain", done.Index)
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
