package processor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

func TestRunRecorderBoundsAggregateReviewText(t *testing.T) {
	store := &runRecordMemoryStore{}
	runner := &Runner{log: newMockLogger(), recordStore: store}
	recorder := &runRecorder{runner: runner}
	output := strings.Repeat("界", runRecordTextCap)
	for _, key := range []string{"one", "two", "three"} {
		for i := 1; i <= 10; i++ {
			recorder.ExternalIteration(i, key, key, output, output)
		}
	}
	require.Len(t, store.saves, 30)
	for _, saved := range store.saves {
		total := 0
		for _, reviewer := range saved.External {
			for _, iteration := range reviewer.Iterations {
				total += len(iteration.ReviewerOutput) + len(iteration.EvaluatorResponse)
				assert.True(t, utf8.ValidString(iteration.ReviewerOutput))
				assert.True(t, utf8.ValidString(iteration.EvaluatorResponse))
				assert.True(t, iteration.Truncated)
			}
		}
		assert.LessOrEqual(t, total, runRecordExternalTextCap)
	}
	for _, reviewer := range store.record.External {
		require.Len(t, reviewer.Iterations, 10)
		assert.Equal(t, 10, reviewer.Iterations[9].Index)
		assert.NotEmpty(t, reviewer.Iterations[9].ReviewerOutput)
		assert.NotEmpty(t, reviewer.Iterations[9].EvaluatorResponse)
	}
}

func TestRunnerBoundsLoadedReviewTextBeforeSaving(t *testing.T) {
	output := strings.Repeat("x", runRecordTextCap)
	stored := RunRecord{Version: runRecordVersion, Branch: "feature", External: []ExternalReviewerRecord{{Key: "legacy"}}}
	for i := 1; i <= 30; i++ {
		stored.External[0].Iterations = append(stored.External[0].Iterations, ExternalIterationRecord{
			Index: i, ReviewerOutput: output, EvaluatorResponse: output,
		})
	}
	store := &runRecordMemoryStore{found: true, record: stored}
	runner := &Runner{
		cfg: Config{Mode: ModeFull}, log: newMockLogger(), recordStore: store,
		git: &checkpointGit{branch: "feature"},
	}
	runner.startRunRecord()
	require.Len(t, store.saves, 1)
	require.Len(t, store.record.External, 1)
	require.Len(t, store.record.External[0].Iterations, 30)
	total := 0
	for _, iteration := range store.record.External[0].Iterations {
		total += len(iteration.ReviewerOutput) + len(iteration.EvaluatorResponse)
		assert.True(t, iteration.Truncated)
	}
	assert.LessOrEqual(t, total, runRecordExternalTextCap)
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
				AppConfig: &config.Config{TaskProvider: config.ExecutorCodex, ReviewProvider: config.ExecutorCodex},
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

func TestFullRunReportPreservesCurrentTasksAfterNewCommits(t *testing.T) {
	for _, stored := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh run", true: "stale prior run"}[stored], func(t *testing.T) {
			planFile := filepath.Join(t.TempDir(), "plan.md")
			require.NoError(t, os.WriteFile(planFile, []byte("# Plan\n"), 0o600))
			cfg := Config{Mode: ModeFull, PlanFile: planFile, ReportEnabled: true}
			git := &checkpointGit{head: "before", branch: "feature", contains: true}
			r, review, external, _ := newCheckpointRunner(cfg, &checkpointMemoryStore{}, git)
			external.enabled = false
			store := &runRecordMemoryStore{found: stored, record: RunRecord{
				Version: runRecordVersion, Branch: "feature", StartedAt: time.Now().Add(-time.Hour),
				Tasks: TaskRunRecord{Iterations: 8}, InternalReview: InternalReviewRunRecord{LoopIterations: 9},
				PhaseDurations: map[string]Duration{"old": Duration(time.Hour)},
				Validation:     &ValidationRunRecord{Duration: Duration(time.Hour), Runs: 99},
			}}
			r.SetRunRecordStore(store)
			r.SetRunTimingsSource(func() (map[string]time.Duration, time.Duration, int) {
				return map[string]time.Duration{"task": 2 * time.Second}, time.Second, 2
			})
			var started time.Time
			r.phases.task.(*checkpointTask).onRun = func() {
				started = r.invocationStarted
				r.recorder.TaskIteration(true)
				r.recorder.TaskIteration(false)
				git.head = "after"
			}
			r.phases.report = testReportPhase{runFunc: func(_ context.Context, facts string) (string, error) {
				assert.Contains(t, facts, "- task iterations: 2\n- task failed retries: 1")
				assert.Contains(t, facts, "| task | 2000 |")
				assert.Contains(t, facts, "- duration_ms: 1000\n- runs: 2")
				assert.NotContains(t, facts, "finished: not recorded")
				return "", nil
			}}

			require.NoError(t, r.Run(t.Context()))
			assert.Equal(t, 1, review.first)
			assert.Equal(t, started, store.record.StartedAt)
			assert.Equal(t, TaskRunRecord{Iterations: 2, FailedRetries: 1}, store.record.Tasks)
			assert.Zero(t, store.record.InternalReview.LoopIterations, "stale review facts must be discarded")
			assert.Equal(t, map[string]Duration{"task": Duration(2 * time.Second)}, store.record.PhaseDurations)
			assert.Equal(t, &ValidationRunRecord{Duration: Duration(time.Second), Runs: 2}, store.record.Validation)
			assert.Contains(t, r.Report(), "| task | 2000 |")
			assert.Contains(t, r.Report(), "- duration_ms: 1000\n- runs: 2")
			assert.NotContains(t, r.Report(), "finished: not recorded")
		})
	}
}

func TestRunRecordPartialCheckpointRecovery(t *testing.T) {
	for _, mode := range []Mode{ModeFull, ModeReview, ModeCodexOnly} {
		for _, changedChain := range []bool{false, true} {
			t.Run(string(mode)+"/changed-chain="+map[bool]string{false: "false", true: "true"}[changedChain], func(t *testing.T) {
				if mode == ModeCodexOnly && changedChain {
					t.Skip("a changed chain has no resumable prefix in external-only mode")
				}
				keys := []string{"codex:first", "claude:second"}
				cfg := Config{Mode: mode, PlanFile: "plan.md", ExternalReviewers: []config.ReviewerSpec{
					{Provider: "codex", ModelSpec: "first"}, {Provider: "claude", ModelSpec: "second"},
				}}
				cp := ReviewCheckpoint{Version: reviewCheckpointVersion, Mode: mode, Branch: "feature", Plan: "plan.md", Reviewers: keys}
				if mode != ModeCodexOnly {
					cp.Stages = append(cp.Stages, ReviewStage{Stage: reviewStageInternal, Head: "retained"})
				}
				cp.Stages = append(cp.Stages,
					ReviewStage{Stage: reviewStageExternal, Index: 0, Reviewer: keys[0], Head: "retained"},
					ReviewStage{Stage: reviewStageExternal, Index: 1, Reviewer: keys[1], Head: "reverted"},
					ReviewStage{Stage: reviewStagePostReview, Head: "reverted"})
				if changedChain {
					cfg.ExternalReviewers = []config.ReviewerSpec{{Provider: "codex", ModelSpec: "replacement"}}
				}
				resume, err := resolveReviewResume(cp, reviewResumeInput{
					Mode: mode, Branch: "feature", Plan: "plan.md", Reviewers: reviewerKeys(cfg), Head: "retained",
				}, func(head string) (bool, error) { return head == "retained", nil })
				require.NoError(t, err)
				require.True(t, reviewResumeHasProgress(resume))
				store := &runRecordMemoryStore{found: true, record: RunRecord{
					Version: runRecordVersion, Branch: "feature", Tasks: TaskRunRecord{Iterations: 7},
					InternalReview: InternalReviewRunRecord{FirstRan: true, LoopIterations: 2},
					External: []ExternalReviewerRecord{
						{Key: keys[0], Iterations: []ExternalIterationRecord{{ReviewerOutput: "retained finding"}}},
						{Key: keys[1], Iterations: []ExternalIterationRecord{{EvaluatorResponse: "reverted fix"}}},
					},
					PostReview: PostReviewRunRecord{Ran: true, Iterations: 5}, Report: "stale report",
					PhaseDurations: map[string]Duration{"tasks": Duration(time.Second), "external review": Duration(time.Hour),
						"evaluation": Duration(time.Hour), "internal review": Duration(time.Hour), "other": Duration(time.Hour)},
				}}
				r := &Runner{cfg: cfg, log: newMockLogger(), git: &checkpointGit{branch: "feature"}, recordStore: store,
					resume: resume, resumeReady: mode != ModeFull}
				r.SetRunTimingsSource(func() (map[string]time.Duration, time.Duration, int) {
					return map[string]time.Duration{"tasks": 2 * time.Second}, 0, 0
				})
				r.startRunRecord()
				if mode == ModeFull {
					r.recorder.TaskIteration(false)
					r.adoptLoadedRunRecord()
				}
				r.snapshotRunTimings()
				assert.Equal(t, Duration(3*time.Second), r.record.PhaseDurations["tasks"])
				assert.NotContains(t, r.record.PhaseDurations, "external review")
				assert.NotContains(t, r.record.PhaseDurations, "evaluation")
				assert.NotContains(t, r.record.PhaseDurations, "internal review")
				assert.NotContains(t, r.record.PhaseDurations, "other")
				assert.Empty(t, r.record.PostReview)
				assert.Empty(t, r.Report())
				if changedChain {
					assert.Empty(t, r.record.External)
				} else {
					require.Len(t, r.record.External, 1)
					assert.Equal(t, "retained finding", r.record.External[0].Iterations[0].ReviewerOutput)
				}
				assert.Equal(t, mode != ModeCodexOnly, r.record.InternalReview.FirstRan)
				assert.Equal(t, cloneRunRecord(r.record).External, store.record.External, "reconciled outcomes must be persisted")
			})
		}
	}
}
