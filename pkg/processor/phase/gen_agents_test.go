package phase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/status"
)

func newGenAgentsPhase(t *testing.T, exec Executor, log *mockLogger) (*GenAgentsPhase, *status.PhaseHolder) {
	t.Helper()
	holder := &status.PhaseHolder{}
	cfg := Config{AppConfig: testAppConfig(t)}
	p := NewGenAgentsPhase(GenAgentsPhaseOpts{
		Cfg: cfg, Log: log, Exec: exec, Policy: newTestPolicy(cfg, log),
		Prompts: testPrompts{}, PhaseHolder: holder,
	})
	return p, holder
}

func TestGenAgentsPhase_RunsSingleSessionWithPrompt(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "wrote 2 agents"}})
	log := newMockLogger("progress.txt")
	p, holder := newGenAgentsPhase(t, exec, log)

	require.NoError(t, p.Run(t.Context()))

	require.Len(t, exec.RunCalls(), 1)
	assert.Equal(t, "gen agents prompt", exec.RunCalls()[0].Prompt)
	assert.Equal(t, status.PhasePlan, holder.Get())
	assertGenAgentsSectionPrinted(t, log)
	assertLogContains(t, log, "agent generation completed")
}

func TestGenAgentsPhase_FailedSignal(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "nope", Signal: status.Failed}})
	p, _ := newGenAgentsPhase(t, exec, newMockLogger("progress.txt"))

	err := p.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "FAILED signal")
}

func TestGenAgentsPhase_ExecutorErrorPropagates(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Error: errors.New("boom")}})
	p, _ := newGenAgentsPhase(t, exec, newMockLogger("progress.txt"))

	err := p.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestGenAgentsPhase_TimeoutIsAnError(t *testing.T) {
	log := newMockLogger("progress.txt")
	exec := newTaskPhaseMockExecutor(nil)
	cfg := Config{AppConfig: testAppConfig(t)}
	p := NewGenAgentsPhase(GenAgentsPhaseOpts{
		Cfg: cfg, Log: log, Exec: exec,
		Policy:  newScriptedTestPolicy(log, ExecutionResult{TimedOut: true}),
		Prompts: testPrompts{},
	})

	err := p.Run(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestGenAgentsPhase_ContextCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	exec := &executorMock{RunFunc: func(runCtx context.Context, _ string) executor.Result {
		return executor.Result{Error: runCtx.Err()}
	}}
	p, _ := newGenAgentsPhase(t, exec, newMockLogger("progress.txt"))

	err := p.Run(ctx)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func assertGenAgentsSectionPrinted(t *testing.T, log *mockLogger) {
	t.Helper()
	for _, call := range log.PrintSectionCalls() {
		if strings.Contains(call.Section.Label, "agent generation") {
			return
		}
	}
	t.Fatal("agent generation section was not printed")
}
