package processor

import (
	"context"
	"fmt"
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

	branch, _ := r.git.CurrentBranch()
	head, err := r.git.HeadHash()
	if err != nil {
		r.log.Print("review checkpoint unreadable, starting reviews from scratch: %v", err)
		return reviewResume{}
	}
	resume, err := resolveReviewResume(cp, reviewResumeInput{
		Mode: r.cfg.Mode, Branch: branch, Reviewers: reviewerKeys(r.cfg), Head: head,
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

	fingerprint, err := r.git.DiffFingerprint()
	if err != nil {
		r.log.Print("review checkpoint save skipped: %v", err)
		return
	}
	if fingerprint != "" {
		r.log.Print("review checkpoint skipped: uncommitted changes")
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
	case cp.Version != reviewCheckpointVersion || cp.Mode != r.cfg.Mode || cp.Branch != branch:
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
			cp.Stages[index] = stage
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
