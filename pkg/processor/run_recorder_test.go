package processor

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/processor/phase"
)

type runRecordMemoryStore struct {
	mu        sync.Mutex
	record    RunRecord
	found     bool
	loadErr   error
	saveErr   error
	removeErr error
	saves     []RunRecord
	removes   int
}

func (s *runRecordMemoryStore) Load() (RunRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRunRecord(s.record), s.found, s.loadErr
}

func (s *runRecordMemoryStore) Save(record RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, cloneRunRecord(record))
	if s.saveErr == nil {
		s.record, s.found = cloneRunRecord(record), true
	}
	return s.saveErr
}

func (s *runRecordMemoryStore) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removes++
	if s.removeErr == nil {
		s.record, s.found = RunRecord{}, false
	}
	return s.removeErr
}

func TestRunRecorderSavesAfterEveryEvent(t *testing.T) {
	store := &runRecordMemoryStore{}
	runner := &Runner{log: newMockLogger(), recordStore: store, record: RunRecord{Version: runRecordVersion}}
	recorder := &runRecorder{runner: runner}
	runner.recorder = recorder

	recorder.TaskIteration(false)
	recorder.TaskIteration(true)
	recorder.InternalReviewDone(2, "review_done")
	recorder.ExternalIteration(1, "codex:gpt:high", "codex", "review", "evaluation")
	recorder.ExternalDone(phase.ReviewerCompletion{
		Reviewer: phase.ExternalReviewer{Tool: "codex", ModelSpec: "gpt:high"},
		Label:    "codex", HadFindings: true, EndedBy: "done",
	})
	recorder.PostReviewDone(3)

	require.Len(t, store.saves, 6)
	last := store.saves[len(store.saves)-1]
	assert.Equal(t, TaskRunRecord{Iterations: 2, FailedRetries: 1}, last.Tasks)
	assert.Equal(t, InternalReviewRunRecord{FirstRan: true, LoopIterations: 2, EndedBy: "review_done"}, last.InternalReview)
	require.Len(t, last.External, 1)
	assert.Equal(t, "review", last.External[0].Iterations[0].ReviewerOutput)
	assert.True(t, last.External[0].HadFindings)
	assert.Equal(t, PostReviewRunRecord{Ran: true, Iterations: 3}, last.PostReview)
}

func TestRunRecorderLogsSaveFailureOnce(t *testing.T) {
	log := newMockLogger()
	store := &runRecordMemoryStore{saveErr: errors.New("disk full")}
	runner := &Runner{log: log, recordStore: store}
	recorder := &runRecorder{runner: runner}

	recorder.TaskIteration(false)
	recorder.TaskIteration(true)
	recorder.PostReviewDone(1)

	assert.Len(t, store.saves, 3, "later events must continue attempting persistence")
	count := 0
	for _, call := range log.PrintCalls() {
		if call.Format == "run record: save failed: %v" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestRunnerRunRecordStartupBranchAndCheckpointRules(t *testing.T) {
	tests := []struct {
		name            string
		recordBranch    string
		checkpointFound bool
		wantTasks       int
	}{
		{name: "matching branch and honored checkpoint keeps record", recordBranch: "feature", checkpointFound: true, wantTasks: 7},
		{name: "matching branch without honored checkpoint starts fresh", recordBranch: "feature", wantTasks: 0},
		{name: "mismatched branch starts fresh", recordBranch: "other", checkpointFound: true, wantTasks: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Mode: ModeReview, PlanFile: "docs/plans/work.md", DefaultBranch: "main",
				TaskModel: "task:high", ReviewModel: "review:xhigh",
				AppConfig: &config.Config{Executor: config.ExecutorCodex},
			}
			checkpoint := &checkpointMemoryStore{found: tc.checkpointFound, cp: ReviewCheckpoint{
				Version: reviewCheckpointVersion, Mode: ModeReview, Branch: "feature", Plan: cfg.PlanFile,
				Stages: []ReviewStage{{Stage: reviewStageInternal, Head: "head"}},
			}}
			git := &checkpointGit{head: "head", branch: "feature", contains: true}
			runner, _, external, _ := newCheckpointRunner(cfg, checkpoint, git)
			external.enabled = false
			store := &runRecordMemoryStore{found: true, record: RunRecord{
				Version: runRecordVersion, Branch: tc.recordBranch, Tasks: TaskRunRecord{Iterations: 7},
			}}
			runner.SetRunRecordStore(store)

			require.NoError(t, runner.Run(t.Context()))
			assert.Equal(t, tc.wantTasks, store.record.Tasks.Iterations)
			assert.Equal(t, "feature", store.record.Branch)
			assert.Equal(t, cfg.PlanFile, store.record.Plan)
			assert.Equal(t, "main", store.record.BaseRef)
			assert.Equal(t, ModeReview, store.record.Mode)
			assert.Equal(t, "codex", store.record.Executor)
			assert.Equal(t, "task:high", store.record.TaskModel)
			assert.Equal(t, "review:xhigh", store.record.ReviewModel)
			assert.False(t, store.record.StartedAt.IsZero())
			assert.False(t, store.record.FinishedAt.IsZero())
		})
	}
}

func TestClearReviewCheckpointResetsAndRemovesRunRecord(t *testing.T) {
	store := &runRecordMemoryStore{found: true, record: RunRecord{
		Version: runRecordVersion, Branch: "feature", Tasks: TaskRunRecord{Iterations: 9},
	}}
	runner := &Runner{
		cfg: Config{Mode: ModeFull, PlanFile: "plan.md"}, log: newMockLogger(),
		git: &checkpointGit{branch: "feature"}, recordStore: store, record: store.record,
	}

	runner.clearReviewCheckpoint("task phase committed new work")

	assert.Equal(t, 1, store.removes)
	assert.False(t, store.found)
	assert.Zero(t, runner.record.Tasks.Iterations)
	assert.Equal(t, "feature", runner.record.Branch)
}
