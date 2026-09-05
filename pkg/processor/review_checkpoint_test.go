package processor

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/ralphex/pkg/config"
)

func TestResolveReviewResume(t *testing.T) {
	const (
		oldHead = "1111111old"
		newHead = "2222222new"
	)
	reviewers := []string{"claude:opus:high", "codex:gpt-5.6:xhigh"}
	fullCheckpoint := ReviewCheckpoint{
		Version:   reviewCheckpointVersion,
		Mode:      ModeFull,
		Branch:    "feature",
		Reviewers: reviewers,
		Stages: []ReviewStage{
			{Stage: reviewStageInternal, Head: oldHead},
			{Stage: reviewStageExternal, Reviewer: reviewers[0], Index: 0, Head: oldHead, HadFindings: true},
			{Stage: reviewStageExternal, Reviewer: reviewers[1], Index: 1, Head: oldHead},
			{Stage: reviewStagePostReview, Head: oldHead},
		},
	}
	current := reviewResumeInput{Mode: ModeFull, Branch: "feature", Reviewers: reviewers, Head: oldHead}

	tests := []struct {
		name         string
		checkpoint   ReviewCheckpoint
		current      reviewResumeInput
		contains     func(string) (bool, error)
		want         reviewResume
		wantErr      string
		wantCalls    []string
		noteContains []string
	}{
		{
			name:      "empty checkpoint",
			current:   current,
			contains:  func(string) (bool, error) { return true, nil },
			want:      reviewResume{},
			wantCalls: nil,
		},
		{
			name:       "exact head through every stage",
			checkpoint: fullCheckpoint,
			current:    current,
			contains:   func(string) (bool, error) { return true, nil },
			want: reviewResume{
				skipInternal: true, completedReviewers: 2, hadFindings: true, skipPostReview: true,
			},
			wantCalls: []string{oldHead, oldHead, oldHead, oldHead},
		},
		{
			name:       "ancestor head adds notes",
			checkpoint: fullCheckpoint,
			current: reviewResumeInput{
				Mode: ModeFull, Branch: "feature", Reviewers: reviewers, Head: newHead,
			},
			contains: func(string) (bool, error) { return true, nil },
			want: reviewResume{
				skipInternal: true, completedReviewers: 2, hadFindings: true, skipPostReview: true,
			},
			wantCalls:    []string{oldHead, oldHead, oldHead, oldHead},
			noteContains: []string{"1111111", "2222222", reviewStageInternal},
		},
		{
			name:       "non ancestor stops at stage",
			checkpoint: fullCheckpoint,
			current:    current,
			contains:   func(string) (bool, error) { return false, nil },
			want:       reviewResume{},
			wantCalls:  []string{oldHead},
		},
		{
			name: "non ancestor after completed internal keeps earlier stage",
			checkpoint: func() ReviewCheckpoint {
				cp := fullCheckpoint
				cp.Stages = append([]ReviewStage(nil), fullCheckpoint.Stages...)
				cp.Stages[1].Head = "bad"
				return cp
			}(),
			current: current,
			contains: func(head string) (bool, error) {
				return head != "bad", nil
			},
			want:      reviewResume{skipInternal: true},
			wantCalls: []string{oldHead, "bad"},
		},
		{
			name:       "reviewer chain mismatch keeps internal only",
			checkpoint: fullCheckpoint,
			current: reviewResumeInput{
				Mode: ModeFull, Branch: "feature", Reviewers: []string{"custom:"}, Head: oldHead,
			},
			contains:     func(string) (bool, error) { return true, nil },
			want:         reviewResume{skipInternal: true},
			wantCalls:    []string{oldHead},
			noteContains: []string{"reviewer chain changed"},
		},
		{
			name:       "mode mismatch",
			checkpoint: fullCheckpoint,
			current:    reviewResumeInput{Mode: ModeReview, Branch: "feature", Reviewers: reviewers, Head: oldHead},
			contains:   func(string) (bool, error) { return true, nil },
			want:       reviewResume{}, wantCalls: nil, noteContains: []string{"does not match"},
		},
		{
			name:       "branch mismatch",
			checkpoint: fullCheckpoint,
			current:    reviewResumeInput{Mode: ModeFull, Branch: "other", Reviewers: reviewers, Head: oldHead},
			contains:   func(string) (bool, error) { return true, nil },
			want:       reviewResume{}, wantCalls: nil, noteContains: []string{"does not match"},
		},
		{
			name:       "version mismatch",
			checkpoint: func() ReviewCheckpoint { cp := fullCheckpoint; cp.Version++; return cp }(),
			current:    current,
			contains:   func(string) (bool, error) { return true, nil },
			want:       reviewResume{}, wantCalls: nil, noteContains: []string{"does not match"},
		},
		{
			name: "non contiguous external indexes",
			checkpoint: ReviewCheckpoint{
				Version: reviewCheckpointVersion, Mode: ModeFull, Branch: "feature", Reviewers: reviewers,
				Stages: []ReviewStage{
					{Stage: reviewStageInternal, Head: oldHead},
					{Stage: reviewStageExternal, Reviewer: reviewers[1], Index: 1, Head: oldHead},
				},
			},
			current:  current,
			contains: func(string) (bool, error) { return true, nil },
			want:     reviewResume{skipInternal: true}, wantCalls: []string{oldHead},
		},
		{
			name: "post review without full chain",
			checkpoint: ReviewCheckpoint{
				Version: reviewCheckpointVersion, Mode: ModeFull, Branch: "feature", Reviewers: reviewers,
				Stages: []ReviewStage{
					{Stage: reviewStageInternal, Head: oldHead},
					{Stage: reviewStageExternal, Reviewer: reviewers[0], Index: 0, Head: oldHead},
					{Stage: reviewStagePostReview, Head: oldHead},
				},
			},
			current:   current,
			contains:  func(string) (bool, error) { return true, nil },
			want:      reviewResume{skipInternal: true, completedReviewers: 1},
			wantCalls: []string{oldHead, oldHead},
		},
		{
			name:       "contains error",
			checkpoint: fullCheckpoint,
			current:    current,
			contains:   func(string) (bool, error) { return false, errors.New("git failure") },
			want:       reviewResume{},
			wantErr:    "git failure",
			wantCalls:  []string{oldHead},
		},
		{
			name: "codex only starts with external review",
			checkpoint: ReviewCheckpoint{
				Version: reviewCheckpointVersion, Mode: ModeCodexOnly, Branch: "feature", Reviewers: reviewers,
				Stages: fullCheckpoint.Stages[1:],
			},
			current:   reviewResumeInput{Mode: ModeCodexOnly, Branch: "feature", Reviewers: reviewers, Head: oldHead},
			contains:  func(string) (bool, error) { return true, nil },
			want:      reviewResume{completedReviewers: 2, hadFindings: true, skipPostReview: true},
			wantCalls: []string{oldHead, oldHead, oldHead},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			got, err := resolveReviewResume(tc.checkpoint, tc.current, func(head string) (bool, error) {
				calls = append(calls, head)
				return tc.contains(head)
			})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want.skipInternal, got.skipInternal)
			assert.Equal(t, tc.want.completedReviewers, got.completedReviewers)
			assert.Equal(t, tc.want.hadFindings, got.hadFindings)
			assert.Equal(t, tc.want.skipPostReview, got.skipPostReview)
			assert.Equal(t, tc.wantCalls, calls)
			for _, fragment := range tc.noteContains {
				assert.Condition(t, func() bool {
					for _, note := range got.notes {
						if strings.Contains(note, fragment) {
							return true
						}
					}
					return false
				}, "expected a note containing %q in %v", fragment, got.notes)
			}
		})
	}
}

func TestReviewerKeys(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "chain",
			cfg: Config{ExternalReviewers: []config.ReviewerSpec{
				{Provider: config.ExternalReviewToolClaude, ModelSpec: "opus:high"},
				{Provider: config.ExternalReviewToolCustom},
			}},
			want: []string{"claude:opus:high", "custom:"},
		},
		{
			name: "legacy triple",
			cfg: Config{
				ExternalReviewTool: config.ExternalReviewToolCodex, ExternalReviewModel: "gpt-5.6", ExternalReviewEffort: "xhigh",
			},
			want: []string{"codex:gpt-5.6:xhigh"},
		},
		{
			name: "none",
			cfg:  Config{ExternalReviewTool: config.ExternalReviewToolNone, ExternalReviewModel: "ignored", ExternalReviewEffort: "high"},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reviewerKeys(tc.cfg))
		})
	}
}
