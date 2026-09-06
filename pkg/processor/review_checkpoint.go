package processor

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/umputun/ralphex/pkg/config"
)

const reviewCheckpointVersion = 1

// ErrReviewCheckpointCorrupt identifies checkpoint data that cannot be decoded and may be replaced.
var ErrReviewCheckpointCorrupt = errors.New("review checkpoint is corrupt")

const (
	reviewStageInternal   = "internal_review"
	reviewStageExternal   = "external_review"
	reviewStagePostReview = "post_review"
)

// ReviewCheckpoint is the durable record of completed review stages.
type ReviewCheckpoint struct {
	Version           int           `json:"version"`
	Mode              Mode          `json:"mode"`
	Branch            string        `json:"branch"`
	Plan              string        `json:"plan"`
	Reviewers         []string      `json:"reviewers"`
	Stages            []ReviewStage `json:"stages"`
	TaskStartedAtHead string        `json:"task_started_at_head,omitempty"`
}

// ReviewStage describes one successfully completed review stage.
type ReviewStage struct {
	Stage       string    `json:"stage"`
	Reviewer    string    `json:"reviewer,omitempty"`
	Index       int       `json:"index,omitempty"`
	Head        string    `json:"head"`
	HadFindings bool      `json:"had_findings,omitempty"`
	EndedBy     string    `json:"ended_by,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}

//go:generate moq -out review_checkpoint_store_mock_test.go -pkg processor_test -skip-ensure -fmt goimports . ReviewCheckpointStore

// ReviewCheckpointStore persists review progress outside an ephemeral worktree.
type ReviewCheckpointStore interface {
	Load() (ReviewCheckpoint, bool, error)
	Save(ReviewCheckpoint) error
	Remove() error
}

func reviewerKeys(cfg Config) []string {
	if len(cfg.ExternalReviewers) > 0 {
		keys := make([]string, 0, len(cfg.ExternalReviewers))
		for _, reviewer := range cfg.ExternalReviewers {
			keys = append(keys, reviewer.Provider+":"+reviewer.ModelSpec)
		}
		return keys
	}
	if cfg.ExternalReviewTool == "" || cfg.ExternalReviewTool == config.ExternalReviewToolNone {
		return nil
	}
	return []string{cfg.ExternalReviewTool + ":" + joinModelEffort(cfg.ExternalReviewModel, cfg.ExternalReviewEffort)}
}

type reviewResume struct {
	skipInternal       bool
	completedReviewers int
	hadFindings        bool
	skipPostReview     bool
	notes              []string
}

type reviewResumeInput struct {
	Mode      Mode
	Branch    string
	Plan      string
	Reviewers []string
	Head      string
}

func resolveReviewResume(
	cp ReviewCheckpoint,
	current reviewResumeInput,
	contains func(head string) (bool, error),
) (reviewResume, error) {
	var resume reviewResume
	if cp.Version == 0 && cp.Mode == "" && cp.Branch == "" && len(cp.Stages) == 0 {
		return resume, nil
	}
	if cp.Version != reviewCheckpointVersion || cp.Mode != current.Mode || cp.Branch != current.Branch ||
		reviewPlanKey(cp.Plan) != reviewPlanKey(current.Plan) {
		resume.notes = append(resume.notes, "checkpoint does not match the current version, mode, branch, or plan; starting reviews from scratch")
		return resume, nil
	}

	reviewersMatch := slices.Equal(cp.Reviewers, current.Reviewers)
	if !reviewersMatch {
		resume.notes = append(resume.notes, "external reviewer chain changed; only internal review can be resumed")
	}

	resolver := checkpointResolver{checkpoint: cp, current: current, contains: contains, resume: resume}
	if current.Mode != ModeCodexOnly {
		ok, err := resolver.resolveInternal()
		if err != nil || !ok {
			return resolver.resume, err
		}
	}

	if !reviewersMatch {
		return resolver.resume, nil
	}
	return resolver.resolveExternalAndPost()
}

type checkpointResolver struct {
	checkpoint ReviewCheckpoint
	current    reviewResumeInput
	contains   func(string) (bool, error)
	resume     reviewResume
	position   int
}

func (r *checkpointResolver) resolveInternal() (bool, error) {
	if r.position >= len(r.checkpoint.Stages) || r.checkpoint.Stages[r.position].Stage != reviewStageInternal {
		return false, nil
	}
	ok, err := r.honor(r.checkpoint.Stages[r.position])
	if err != nil || !ok {
		return ok, err
	}
	r.resume.skipInternal = true
	r.position++
	return true, nil
}

func (r *checkpointResolver) resolveExternalAndPost() (reviewResume, error) {
	for index, reviewer := range r.current.Reviewers {
		if r.position >= len(r.checkpoint.Stages) {
			return r.resume, nil
		}
		stage := r.checkpoint.Stages[r.position]
		if stage.Stage != reviewStageExternal || stage.Index != index || stage.Reviewer != reviewer {
			return r.resume, nil
		}
		ok, err := r.honor(stage)
		if err != nil || !ok {
			return r.resume, err
		}
		r.resume.completedReviewers++
		r.resume.hadFindings = r.resume.hadFindings || stage.HadFindings
		r.position++
	}

	if r.position >= len(r.checkpoint.Stages) || r.checkpoint.Stages[r.position].Stage != reviewStagePostReview {
		return r.resume, nil
	}
	ok, err := r.honor(r.checkpoint.Stages[r.position])
	if err != nil || !ok {
		return r.resume, err
	}
	r.resume.skipPostReview = true
	return r.resume, nil
}

func (r *checkpointResolver) honor(stage ReviewStage) (bool, error) {
	contained, err := r.contains(stage.Head)
	if err != nil {
		return false, fmt.Errorf("check checkpoint stage %s at %s: %w", stage.Stage, stage.Head, err)
	}
	if !contained {
		return false, nil
	}
	if stage.Head != r.current.Head {
		r.resume.notes = append(r.resume.notes, fmt.Sprintf(
			"branch moved from %s to %s since %s; resuming anyway",
			shortRevision(stage.Head), shortRevision(r.current.Head), stage.Stage,
		))
	}
	return true, nil
}

func shortRevision(revision string) string {
	if len(revision) <= 7 {
		return revision
	}
	return revision[:7]
}
