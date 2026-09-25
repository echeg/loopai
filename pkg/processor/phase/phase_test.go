package phase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/status"
)

func TestConfigExecutorNamesFollowPhaseProvider(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		wantTask   string
		wantReview string
	}{
		{name: "unset providers default to claude", wantTask: "claude", wantReview: "claude"},
		{name: "both codex", cfg: Config{TaskProvider: "codex", ReviewProvider: "codex"}, wantTask: "codex", wantReview: "codex"},
		{name: "codex task with claude review", cfg: Config{TaskProvider: "codex", ReviewProvider: "claude"},
			wantTask: "codex", wantReview: "claude"},
		{name: "claude task with codex review", cfg: Config{TaskProvider: "claude", ReviewProvider: "codex"},
			wantTask: "claude", wantReview: "codex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantTask, tt.cfg.taskExecutorName())
			assert.Equal(t, tt.wantReview, tt.cfg.reviewExecutorName())
		})
	}
}

func TestPhasesRunUnderTheirOwnProvider(t *testing.T) {
	for _, tc := range []struct {
		name       string
		task       string
		review     string
		wantTask   string
		wantReview string
	}{
		{name: "codex task with claude review", task: "codex", review: "claude", wantTask: "codex", wantReview: "claude"},
		{name: "claude task with codex review", task: "claude", review: "codex", wantTask: "claude", wantReview: "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{MaxIterations: 10, FinalizeEnabled: true, ReportEnabled: true, PlanDescription: "add a feature",
				TaskProvider: tc.task, ReviewProvider: tc.review, AppConfig: testAppConfig(t)}
			log := newMockLogger("progress.txt")
			run := func(signal string) *executorMock {
				return newTaskPhaseMockExecutor([]executor.Result{{Output: "ok", Signal: signal}})
			}
			r := newTestRunner(testRunnerOpts{cfg: cfg, log: log, holder: &status.PhaseHolder{},
				planFile: writeTaskPhasePlan(t, "# Plan\n### Task 1: first\n- [ ] todo"),
				execs:    Executors{Task: run(status.Failed), Review: run(status.ReviewDone)}})
			r.SetInputCollector(newPlanInputCollector(nil))

			taskPolicy := newTestPolicy(cfg, log)
			task, ok := r.phases.task.(*taskPhase)
			require.True(t, ok)
			task.policy = taskPolicy
			_ = task.Run(t.Context())

			planPolicy := newTestPolicy(cfg, log)
			planCreation, ok := r.phases.planCreation.(*planCreationPhase)
			require.True(t, ok)
			planCreation.exec, planCreation.policy = run(status.Failed), planPolicy
			_ = planCreation.Run(t.Context())

			genPolicy := newTestPolicy(cfg, log)
			gen := NewGenAgentsPhase(GenAgentsPhaseOpts{Cfg: cfg, Log: log, Exec: run(""), Policy: genPolicy, Prompts: testPrompts{}})
			require.NoError(t, gen.Run(t.Context()))

			reviewPolicy := newTestPolicy(cfg, log)
			review, ok := r.phases.review.(*reviewPhase)
			require.True(t, ok)
			review.policy = reviewPolicy
			require.NoError(t, review.First(t.Context()))

			finalizePolicy := newTestPolicy(cfg, log)
			finalize, ok := r.phases.finalize.(*finalizePhase)
			require.True(t, ok)
			finalize.policy = finalizePolicy
			require.NoError(t, finalize.Run(t.Context()))

			reportPolicy := newTestPolicy(cfg, log)
			report := NewReportPhase(ReportPhaseOpts{Cfg: cfg, Log: log, Exec: run(""), Policy: reportPolicy, Prompts: testPrompts{}})
			_, err := report.Run(t.Context(), "facts")
			require.NoError(t, err)

			assertToolNames(t, tc.wantTask, taskPolicy, "task")
			assertToolNames(t, tc.wantTask, planPolicy, "plan creation runs the task-slot executor")
			assertToolNames(t, tc.wantTask, genPolicy, "gen agents")
			assertToolNames(t, tc.wantReview, reviewPolicy, "review")
			assertToolNames(t, tc.wantReview, finalizePolicy, "finalize")
			assertToolNames(t, tc.wantReview, reportPolicy, "report")
		})
	}
}

// assertToolNames checks that the phase ran at least once and every run named want.
func assertToolNames(t *testing.T, want string, policy *testPolicy, phase string) {
	t.Helper()
	require.NotEmpty(t, policy.toolNames, phase)
	for _, name := range policy.toolNames {
		assert.Equal(t, want, name, phase)
	}
}
