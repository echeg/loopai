package processor_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/processor/mocks"
	"github.com/umputun/ralphex/pkg/status"
)

func TestRunnerReviewCheckpoints_WithGeneratedMocks(t *testing.T) {
	appCfg, err := config.Load(t.TempDir())
	require.NoError(t, err)

	var mu sync.Mutex
	var saved []processor.ReviewCheckpoint
	store := &mocks.ReviewCheckpointStoreMock{
		LoadFunc: func() (processor.ReviewCheckpoint, bool, error) {
			return processor.ReviewCheckpoint{}, false, nil
		},
		SaveFunc: func(cp processor.ReviewCheckpoint) error {
			mu.Lock()
			defer mu.Unlock()
			saved = append(saved, cp)
			return nil
		},
		RemoveFunc: func() error { return nil },
	}
	git := &mocks.GitCheckerMock{
		HeadHashFunc:                func() (string, error) { return "head", nil },
		DiffFingerprintFunc:         func() (string, error) { return "", nil },
		ContainsRevisionContextFunc: func(context.Context, string) (bool, error) { return true, nil },
		CurrentBranchFunc:           func() (string, error) { return "feature", nil },
	}
	review := &mocks.ExecutorMock{RunFunc: func(context.Context, string) executor.Result {
		return executor.Result{Output: "done", Signal: status.ExternalReviewDone}
	}}
	external := &mocks.ExecutorMock{RunFunc: func(context.Context, string) executor.Result {
		return executor.Result{}
	}}
	log := &mocks.LoggerMock{
		PrintFunc: func(string, ...any) {}, PrintRawFunc: func(string, ...any) {},
		PrintSectionFunc: func(status.Section) {}, PrintAlignedFunc: func(string) {},
		LogQuestionFunc: func(string, []string) {}, LogAnswerFunc: func(string) {},
		LogDraftReviewFunc: func(string, string) {}, PathFunc: func() string { return "progress.txt" },
	}
	cfg := processor.Config{
		Mode: processor.ModeCodexOnly, MaxIterations: 2, AppConfig: appCfg,
		ExternalReviewers: []config.ReviewerSpec{{Provider: config.ExternalReviewToolCodex, ModelSpec: "gpt:high"}},
	}
	r := processor.NewWithExecutors(cfg, log, processor.Executors{
		Task:      review,
		Externals: []processor.ExternalReviewer{{Tool: config.ExternalReviewToolCodex, Exec: external}},
	}, &status.PhaseHolder{})
	r.SetGitChecker(git)
	r.SetReviewCheckpoints(store)

	require.NoError(t, r.Run(t.Context()))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, saved, 1)
	require.Len(t, saved[0].Stages, 1)
	assert.Equal(t, "external_review", saved[0].Stages[0].Stage)
	assert.Equal(t, "codex:gpt:high", saved[0].Stages[0].Reviewer)
	assert.Len(t, store.RemoveCalls(), 1)
}
