package phase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/status"
)

type externalReviewPhaseTestOpts struct {
	cfg       Config
	review    Executor
	reviewers []ExternalReviewer
	external  Executor
	custom    *executor.CustomExecutor
	log       *mockLogger
}

func externalReviewPhaseFromRunner(t *testing.T, opts externalReviewPhaseTestOpts) (*externalReviewPhase, *mockLogger) {
	t.Helper()
	if opts.cfg.AppConfig == nil {
		opts.cfg.AppConfig = testAppConfig(t)
	}
	if opts.log == nil {
		opts.log = newMockLogger("progress.txt")
	}
	if opts.review == nil {
		opts.review = newTaskPhaseMockExecutor(nil)
	}
	if opts.external == nil {
		opts.external = newTaskPhaseMockExecutor(nil)
	}
	reviewers := externalReviewersForTest(opts)
	r := newTestRunner(testRunnerOpts{cfg: opts.cfg, log: opts.log, execs: Executors{Task: opts.review, Externals: reviewers}, holder: &status.PhaseHolder{}})
	phase, ok := r.phases.external.(*externalReviewPhase)
	require.True(t, ok)
	return phase, opts.log
}

func externalReviewersForTest(opts externalReviewPhaseTestOpts) []ExternalReviewer {
	if opts.reviewers != nil {
		return opts.reviewers
	}
	tool := effectiveTestReviewerTool(opts.cfg)
	if tool == config.ExternalReviewToolNone {
		return nil
	}
	exec := opts.external
	if tool == config.ExternalReviewToolCustom {
		exec = nil
	}
	if opts.custom != nil {
		exec = opts.custom
	}
	return []ExternalReviewer{{Tool: tool, Exec: exec}}
}

func effectiveTestReviewerTool(cfg Config) string {
	if cfg.ExternalReviewTool != "" && cfg.ExternalReviewTool != config.ExternalReviewToolAuto {
		return cfg.ExternalReviewTool
	}
	if cfg.AppConfig != nil && cfg.AppConfig.ExternalReviewTool != "" && cfg.AppConfig.ExternalReviewTool != config.ExternalReviewToolAuto {
		return cfg.AppConfig.ExternalReviewTool
	}
	if cfg.CodexEnabled {
		return config.ExternalReviewToolCodex
	}
	return config.ExternalReviewToolNone
}

func TestExternalReviewPhaseEnabledAndLabel(t *testing.T) {
	tests := []struct {
		name        string
		cfg         Config
		wantEnabled bool
		wantLabel   string
	}{
		{name: "chain", cfg: Config{CodexEnabled: true, AppConfig: testAppConfig(t)}, wantEnabled: true, wantLabel: "codex"},
		{name: "disabled", cfg: Config{CodexEnabled: false, AppConfig: testAppConfig(t)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{cfg: tc.cfg})

			assert.Equal(t, tc.wantEnabled, phase.Enabled())
			assert.Equal(t, tc.wantLabel, phase.Label())
		})
	}
}

func TestExternalReviewPhaseRunsReviewersInOrderUntilClean(t *testing.T) {
	var order []string
	firstResults := []executor.Result{{Output: "first finding"}, {Output: "first clean"}}
	secondResults := []executor.Result{{Output: "second finding"}, {Output: "second clean"}}
	firstIndex, secondIndex := 0, 0
	first := &executorMock{RunFunc: func(context.Context, string) executor.Result {
		order = append(order, "codex")
		result := firstResults[firstIndex]
		firstIndex++
		return result
	}}
	second := &executorMock{RunFunc: func(context.Context, string) executor.Result {
		order = append(order, "claude")
		result := secondResults[secondIndex]
		secondIndex++
		return result
	}}
	evaluator := newTaskPhaseMockExecutor([]executor.Result{
		{Output: "fixed first"}, {Output: "done", Signal: status.ExternalReviewDone},
		{Output: "fixed second"}, {Output: "done", Signal: status.ExternalReviewDone},
	})
	phase, log := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg:    Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)},
		review: evaluator,
		reviewers: []ExternalReviewer{
			{Tool: config.ExternalReviewToolCodex, Exec: first},
			{Tool: config.ExternalReviewToolClaude, Exec: second},
		},
	})

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.True(t, outcome.HadFindings)
	assert.Equal(t, []string{"codex", "codex", "claude", "claude"}, order)
	assert.Len(t, evaluator.RunCalls(), 4)
	require.Len(t, second.RunCalls(), 2)
	assert.NotContains(t, second.RunCalls()[0].Prompt, "fixed first",
		"each reviewer must start with fresh loop context")
	sections := log.PrintSectionCalls()
	require.Len(t, sections, 10)
	assert.Equal(t, "external review (codex)", sections[0].Section.Label)
	assert.Equal(t, "external review (claude)", sections[5].Section.Label)
}

func TestExternalReviewPhaseAggregatesFindingsFromSecondReviewer(t *testing.T) {
	first := newTaskPhaseMockExecutor([]executor.Result{{Output: "clean"}})
	second := newTaskPhaseMockExecutor([]executor.Result{{Output: "finding"}, {Output: "clean"}})
	evaluator := newTaskPhaseMockExecutor([]executor.Result{
		{Output: "done", Signal: status.ExternalReviewDone},
		{Output: "fixed"},
		{Output: "done", Signal: status.ExternalReviewDone},
	})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, review: evaluator,
		reviewers: []ExternalReviewer{
			{Tool: config.ExternalReviewToolCodex, Exec: first},
			{Tool: config.ExternalReviewToolClaude, Exec: second},
		},
	})

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.True(t, outcome.HadFindings)
	assert.Len(t, first.RunCalls(), 1)
	assert.Len(t, second.RunCalls(), 2)
}

func TestExternalReviewPhaseBreakStopsRemainingReviewers(t *testing.T) {
	breakCh := make(chan struct{}, 1)
	first := &executorMock{RunFunc: func(ctx context.Context, _ string) executor.Result {
		breakCh <- struct{}{}
		<-ctx.Done()
		return executor.Result{Error: ctx.Err()}
	}}
	second := newTaskPhaseMockExecutor([]executor.Result{{Output: "must not run"}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)},
		reviewers: []ExternalReviewer{
			{Tool: config.ExternalReviewToolCodex, Exec: first},
			{Tool: config.ExternalReviewToolClaude, Exec: second},
		},
	})
	phase.breaks.deps.BreakCh = breakCh

	_, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.Empty(t, second.RunCalls())
}

func TestExternalReviewPhaseCancellationStopsRemainingReviewers(t *testing.T) {
	first := newTaskPhaseMockExecutor([]executor.Result{{Output: "must not run"}})
	second := newTaskPhaseMockExecutor([]executor.Result{{Output: "must not run"}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)},
		reviewers: []ExternalReviewer{
			{Tool: config.ExternalReviewToolCodex, Exec: first},
			{Tool: config.ExternalReviewToolClaude, Exec: second},
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := phase.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, first.RunCalls())
	assert.Empty(t, second.RunCalls())
}

func TestExternalReviewPhaseSecondReviewerErrorStopsChain(t *testing.T) {
	first := newTaskPhaseMockExecutor([]executor.Result{{Output: "clean"}})
	secondErr := errors.New("reviewer failed")
	second := newTaskPhaseMockExecutor([]executor.Result{{Error: secondErr}})
	third := newTaskPhaseMockExecutor([]executor.Result{{Output: "must not run"}})
	evaluator := newTaskPhaseMockExecutor([]executor.Result{{Output: "done", Signal: status.ExternalReviewDone}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, review: evaluator,
		reviewers: []ExternalReviewer{
			{Tool: config.ExternalReviewToolCodex, Exec: first},
			{Tool: config.ExternalReviewToolClaude, Exec: second},
			{Tool: config.ExternalReviewToolCodex, Exec: third},
		},
	})

	_, err := phase.Run(t.Context())

	require.Error(t, err)
	require.ErrorIs(t, err, secondErr)
	require.ErrorContains(t, err, "claude execution")
	assert.Empty(t, third.RunCalls())
}

func TestExternalReviewPhaseIterationCapAdvancesToNextReviewer(t *testing.T) {
	first := newTaskPhaseMockExecutor([]executor.Result{{Output: "finding"}})
	second := newTaskPhaseMockExecutor([]executor.Result{{Output: "clean"}})
	evaluator := newTaskPhaseMockExecutor([]executor.Result{
		{Output: "fixed"},
		{Output: "done", Signal: status.ExternalReviewDone},
	})
	phase, log := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{
			MaxIterations: 50, MaxExternalIterations: 1, CodexEnabled: true,
			AppConfig: testAppConfig(t),
		},
		review: evaluator,
		reviewers: []ExternalReviewer{
			{Tool: config.ExternalReviewToolCodex, Exec: first},
			{Tool: config.ExternalReviewToolClaude, Exec: second},
		},
	})

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.True(t, outcome.HadFindings)
	assert.Len(t, first.RunCalls(), 1)
	assert.Len(t, second.RunCalls(), 1)
	assert.Contains(t, fmt.Sprint(log.PrintCalls()), "max %s iterations reached, continuing to next phase")
}

func TestExternalReviewPhaseStalematePatienceIsPerReviewer(t *testing.T) {
	first := newTaskPhaseMockExecutor([]executor.Result{{Output: "finding"}, {Output: "finding again"}})
	second := newTaskPhaseMockExecutor([]executor.Result{{Output: "finding"}, {Output: "finding again"}})
	evaluator := newTaskPhaseMockExecutor([]executor.Result{
		{Output: "not fixed"}, {Output: "still not fixed"},
		{Output: "not fixed"}, {Output: "still not fixed"},
	})
	phase, log := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg:    Config{MaxIterations: 50, MaxExternalIterations: 5, ReviewPatience: 2, CodexEnabled: true, AppConfig: testAppConfig(t)},
		review: evaluator,
		reviewers: []ExternalReviewer{
			{Tool: config.ExternalReviewToolCodex, Exec: first},
			{Tool: config.ExternalReviewToolClaude, Exec: second},
		},
	})
	phase.git.deps.Git = &gitCheckerMock{
		HeadHashFunc:        func() (string, error) { return "abc123def456abc123def456abc123def456abcd", nil },
		DiffFingerprintFunc: func() (string, error) { return "unchanged-diff", nil },
	}

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.True(t, outcome.HadFindings)
	assert.Len(t, first.RunCalls(), 2)
	assert.Len(t, second.RunCalls(), 2)
	stalemates := 0
	for _, call := range log.PrintCalls() {
		if strings.Contains(call.Format, "stalemate detected") {
			stalemates++
		}
	}
	assert.Equal(t, 2, stalemates)
}

func TestExternalReviewPhaseRunCodexNoFindings(t *testing.T) {
	review := newTaskPhaseMockExecutor([]executor.Result{{Output: "done", Signal: status.ExternalReviewDone}})
	external := newTaskPhaseMockExecutor([]executor.Result{{Output: "found issue"}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, review: review, external: external,
	})

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.False(t, outcome.HadFindings)
	assert.Len(t, external.RunCalls(), 1)
	assert.Len(t, review.RunCalls(), 1)
}

func TestExternalReviewPhaseRunCodexNilPhaseHolder(t *testing.T) {
	review := newTaskPhaseMockExecutor([]executor.Result{{Output: "done", Signal: status.CodexDone}})
	external := newTaskPhaseMockExecutor([]executor.Result{{Output: "found issue"}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, review: review, external: external,
	})
	phase.phaseHolder = nil

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.False(t, outcome.HadFindings)
}

func TestExternalReviewPhaseRunCodexFindingsThenEmptyRequiresEvaluation(t *testing.T) {
	review := newTaskPhaseMockExecutor([]executor.Result{{Output: "fixed"}, {Output: "done", Signal: status.ExternalReviewDone}})
	external := newTaskPhaseMockExecutor([]executor.Result{{Output: "found issue"}, {Output: ""}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, review: review, external: external,
	})

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.True(t, outcome.HadFindings)
	assert.Len(t, external.RunCalls(), 2)
	assert.Len(t, review.RunCalls(), 2)
}

func TestExternalReviewPhaseRunClaudeFindingsEvaluatedByCodex(t *testing.T) {
	review := newTaskPhaseMockExecutor([]executor.Result{{Output: "dismissed finding"}, {Output: "done", Signal: status.ExternalReviewDone}})
	external := newTaskPhaseMockExecutor([]executor.Result{{Output: "issue in main.go:12"}, {Output: "NO ISSUES FOUND"}})
	appCfg := testAppConfig(t)
	appCfg.Executor = "codex"
	appCfg.ExternalReviewTool = "claude"
	phase, log := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: appCfg}, review: review, external: external,
	})

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.True(t, outcome.HadFindings)
	require.Len(t, external.RunCalls(), 2)
	assert.Contains(t, external.RunCalls()[0].Prompt, "claude review prompt")
	assert.Contains(t, external.RunCalls()[1].Prompt, "dismissed finding")
	require.Len(t, review.RunCalls(), 2)
	assert.Contains(t, review.RunCalls()[0].Prompt, "issue in main.go:12")
	sections := log.PrintSectionCalls()
	require.Len(t, sections, 5)
	assert.Equal(t, "external review (claude)", sections[0].Section.Label)
	assert.Equal(t, "claude external review iteration 1", sections[1].Section.Label)
	assert.Equal(t, "codex evaluating claude findings", sections[2].Section.Label)
	assert.Equal(t, "claude external review iteration 2", sections[3].Section.Label)
	assert.Equal(t, "codex evaluating claude findings", sections[4].Section.Label)
}

func TestExternalReviewPhaseRunCustomSuccess(t *testing.T) {
	review := newTaskPhaseMockExecutor([]executor.Result{{Output: "done", Signal: status.CodexDone}})
	custom := &executor.CustomExecutor{Script: "/path/to/script.sh"}
	custom.SetRunner(&mockCustomRunnerImpl{results: []executor.Result{{Output: "found issue in foo.go:10"}}})
	appCfg := testAppConfig(t)
	appCfg.ExternalReviewTool = "custom"
	appCfg.CustomReviewScript = custom.Script
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: appCfg}, review: review, custom: custom,
	})

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.False(t, outcome.HadFindings)
	assert.Len(t, review.RunCalls(), 1)
}

func TestExternalReviewPhaseRunCustomNoDuplicateOutput(t *testing.T) {
	log := newMockLogger("progress.txt")
	custom := &executor.CustomExecutor{Script: "/path/to/script.sh", OutputHandler: func(text string) { log.PrintAligned(text) }}
	custom.SetRunner(&mockCustomRunnerImpl{results: []executor.Result{{Output: "issue in foo.go:10\n"}}})
	appCfg := testAppConfig(t)
	appCfg.ExternalReviewTool = "custom"
	appCfg.CustomReviewScript = custom.Script
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg:    Config{MaxIterations: 50, CodexEnabled: true, AppConfig: appCfg},
		review: newTaskPhaseMockExecutor([]executor.Result{{Output: "done", Signal: status.CodexDone}}),
		custom: custom,
		log:    log,
	})

	_, err := phase.Run(t.Context())

	require.NoError(t, err)
	count := 0
	for _, call := range log.PrintAlignedCalls() {
		if strings.Contains(call.Text, "issue in foo.go:10") {
			count++
		}
	}
	assert.Equal(t, 1, count, "custom review output should be streamed once and not duplicated by summary")
}

func TestExternalReviewPhaseRunClaudeDoesNotDuplicateStreamedOutput(t *testing.T) {
	log := newMockLogger("progress.txt")
	appCfg := testAppConfig(t)
	appCfg.Executor = config.ExecutorCodex
	appCfg.ExternalReviewTool = config.ExternalReviewToolClaude
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{
			MaxIterations: 50, CodexEnabled: true,
			ExternalReviewTool: config.ExternalReviewToolClaude, AppConfig: appCfg,
		},
		review:   newTaskPhaseMockExecutor([]executor.Result{{Output: "done", Signal: status.ExternalReviewDone}}),
		external: newTaskPhaseMockExecutor([]executor.Result{{Output: "issue in foo.go:10"}}),
		log:      log,
	})

	_, err := phase.Run(t.Context())

	require.NoError(t, err)
	for _, call := range log.PrintAlignedCalls() {
		assert.NotContains(t, call.Text, "issue in foo.go:10",
			"Claude output is streamed by its executor and must not be repeated by the phase summary")
	}
}

func TestExternalReviewPhaseRunCustomNotConfigured(t *testing.T) {
	appCfg := testAppConfig(t)
	appCfg.ExternalReviewTool = "custom"
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: appCfg},
	})

	_, err := phase.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom review script not configured")
}

func TestExternalReviewPhaseRunBreakChannelExitsEarly(t *testing.T) {
	breakCh := make(chan struct{}, 1)
	external := &executorMock{RunFunc: func(ctx context.Context, _ string) executor.Result {
		breakCh <- struct{}{}
		<-ctx.Done()
		return executor.Result{Error: ctx.Err()}
	}}
	phase, log := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, external: external,
	})
	phase.breaks.deps.BreakCh = breakCh

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.False(t, outcome.HadFindings)
	assertLogContains(t, log, "manual break requested")
}

func TestExternalReviewPhaseShowSummary(t *testing.T) {
	log := newMockLogger("")
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{log: log})

	phase.showSummary("codex", "line 1\n\nline 2\n```go\ncode")

	require.Len(t, log.PrintCalls(), 1)
	assert.Equal(t, "%s findings:", log.PrintCalls()[0].Format)
	assert.Equal(t, []any{"codex"}, log.PrintCalls()[0].Args)
	aligned := log.PrintAlignedCalls()
	require.Len(t, aligned, 2)
	assert.Equal(t, "  line 1", aligned[0].Text)
	assert.Equal(t, "  line 2", aligned[1].Text)
}

func TestExternalReviewPhaseRunPatternError(t *testing.T) {
	external := newTaskPhaseMockExecutor([]executor.Result{{Error: &executor.PatternMatchError{Pattern: "limit", HelpCmd: "usage"}}})
	phase, log := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, external: external,
	})

	_, err := phase.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit")
	assertLogContains(t, log, "detected")
}

func TestExternalReviewPhaseRunClaudeEvalError(t *testing.T) {
	review := newTaskPhaseMockExecutor([]executor.Result{{Error: errors.New("eval failed")}})
	external := newTaskPhaseMockExecutor([]executor.Result{{Output: "found issue"}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, review: review, external: external,
	})

	_, err := phase.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "claude execution")
}

type mockCustomRunnerImpl struct {
	results []executor.Result
	idx     int
}

func (m *mockCustomRunnerImpl) Run(_ context.Context, _, _ string) (io.Reader, func() error, error) {
	if m.idx >= len(m.results) {
		return nil, nil, errors.New("no more mock results")
	}
	result := m.results[m.idx]
	m.idx++
	return strings.NewReader(result.Output), func() error { return result.Error }, nil
}

func TestExternalReviewPhaseStalemateBreaksAfterPatience(t *testing.T) {
	log := newMockLogger("progress.txt")
	review := newTaskPhaseMockExecutor([]executor.Result{{Output: "rejected findings"}, {Output: "rejected findings again"}})
	external := newTaskPhaseMockExecutor([]executor.Result{{Output: "found issue"}, {Output: "found issue"}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg:    Config{MaxIterations: 50, MaxExternalIterations: 5, CodexEnabled: true, ReviewPatience: 2, AppConfig: testAppConfig(t)},
		review: review, external: external, log: log,
	})
	phase.git.deps.Git = &gitCheckerMock{
		HeadHashFunc:        func() (string, error) { return "abc123def456abc123def456abc123def456abcd", nil },
		DiffFingerprintFunc: func() (string, error) { return "unchanged-diff", nil },
	}

	outcome, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.True(t, outcome.HadFindings)
	assert.Len(t, external.RunCalls(), 2)
	assert.Len(t, review.RunCalls(), 2)
	assertLogContains(t, log, "stalemate detected")
}

func TestExternalReviewPhaseTimeoutRetriesNextIteration(t *testing.T) {
	tests := []struct {
		name        string
		firstOutput string
	}{
		{name: "empty output", firstOutput: ""},
		{name: "partial output", firstOutput: "partial findings before timeout..."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := newMockLogger("progress.txt")
			review := newTaskPhaseMockExecutor(nil)
			external := newTaskPhaseMockExecutor(nil)
			phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
				cfg:    Config{MaxIterations: 50, MaxExternalIterations: 3, CodexEnabled: true, AppConfig: testAppConfig(t)},
				review: review, external: external, log: log,
			})
			phase.policy = newScriptedTestPolicy(log,
				ExecutionResult{Result: executor.Result{Output: tc.firstOutput}, TimedOut: true},
				ExecutionResult{Result: executor.Result{Output: "found issue in foo.go:42"}},
				ExecutionResult{Result: executor.Result{Output: "done", Signal: status.CodexDone}},
			)

			_, err := phase.Run(t.Context())

			require.NoError(t, err)
			assert.Len(t, external.RunCalls(), 2)
			assert.Len(t, review.RunCalls(), 1)
			assertLogContains(t, log, "session timed out, retrying on next iteration")
		})
	}
}

func TestExternalReviewPhaseTimeoutSkipsStalemate(t *testing.T) {
	log := newMockLogger("progress.txt")
	review := newTaskPhaseMockExecutor(nil)
	external := newTaskPhaseMockExecutor(nil)
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg:    Config{MaxIterations: 50, MaxExternalIterations: 5, CodexEnabled: true, ReviewPatience: 2, AppConfig: testAppConfig(t)},
		review: review, external: external, log: log,
	})
	phase.policy = newScriptedTestPolicy(log,
		ExecutionResult{Result: executor.Result{Output: "found issue in foo.go:10"}},
		ExecutionResult{Result: executor.Result{Output: "partial output"}, TimedOut: true},
		ExecutionResult{Result: executor.Result{Output: "found issue in bar.go:20"}},
		ExecutionResult{Result: executor.Result{Output: "partial output"}, TimedOut: true},
		ExecutionResult{Result: executor.Result{Output: "no issues found"}},
		ExecutionResult{Result: executor.Result{Output: "done", Signal: status.CodexDone}},
	)
	phase.git.deps.Git = &gitCheckerMock{
		HeadHashFunc:        func() (string, error) { return "abc123def456abc123def456abc123def456abcd", nil },
		DiffFingerprintFunc: func() (string, error) { return "unchanged-diff", nil },
	}

	_, err := phase.Run(t.Context())

	require.NoError(t, err)
	assert.Len(t, external.RunCalls(), 3)
	assertLogContains(t, log, "eval session timed out")
	for _, call := range log.PrintCalls() {
		assert.NotContains(t, call.Format, "stalemate detected")
	}
}

func TestExternalReviewPhaseKeepsBranchDiffAfterTimeout(t *testing.T) {
	var prompts []string
	review := newTaskPhaseMockExecutor(nil)
	external := &executorMock{RunFunc: func(_ context.Context, prompt string) executor.Result {
		prompts = append(prompts, prompt)
		return executor.Result{}
	}}
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 50, CodexEnabled: true, AppConfig: testAppConfig(t)}, review: review, external: external,
	})
	phase.policy = newScriptedTestPolicy(newMockLogger(""),
		ExecutionResult{Result: executor.Result{Output: "found issue"}},
		ExecutionResult{TimedOut: true},
		ExecutionResult{Result: executor.Result{Output: "found issue"}},
		ExecutionResult{Result: executor.Result{Output: "done", Signal: status.CodexDone}},
	)

	_, err := phase.Run(t.Context())

	require.NoError(t, err)
	require.Len(t, prompts, 2)
	assert.Contains(t, prompts[0], "git diff")
	assert.Contains(t, prompts[1], "git diff")
	assert.NotContains(t, prompts[1], "PREVIOUS REVIEW CONTEXT")
}
