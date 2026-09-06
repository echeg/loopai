package phase

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/status"
)

func TestReportPhase_RunSuccess(t *testing.T) {
	log := newMockLogger("progress.txt")
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "# Report: done"}})
	holder := &status.PhaseHolder{}
	phase := NewReportPhase(ReportPhaseOpts{
		Cfg: Config{ReportEnabled: true}, Log: log, Exec: exec,
		Policy: newTestPolicy(Config{}, log), Prompts: testPrompts{}, PhaseHolder: holder,
	})

	output, err := phase.Run(t.Context(), "facts")

	require.NoError(t, err)
	assert.Equal(t, "# Report: done", output)
	assert.Equal(t, status.PhaseReport, holder.Get())
	require.Len(t, exec.RunCalls(), 1)
	assert.Equal(t, "report prompt\nfacts", exec.RunCalls()[0].Prompt)
	require.Len(t, log.PrintSectionCalls(), 1)
	assert.Equal(t, "report step", log.PrintSectionCalls()[0].Section.Label)
}

func TestReportPhase_RunFailedSignalIsNonBlocking(t *testing.T) {
	log := newMockLogger("progress.txt")
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "failed", Signal: status.Failed}})
	phase := NewReportPhase(ReportPhaseOpts{
		Cfg: Config{ReportEnabled: true}, Log: log, Exec: exec,
		Policy: newTestPolicy(Config{}, log), Prompts: testPrompts{},
	})

	output, err := phase.Run(t.Context(), "facts")

	require.NoError(t, err)
	assert.Empty(t, output)
	assertLogContains(t, log, "report step reported failure")
}

func TestReportPhase_RunTimeoutIsNonBlocking(t *testing.T) {
	log := newMockLogger("progress.txt")
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "partial"}})
	phase := NewReportPhase(ReportPhaseOpts{
		Cfg: Config{ReportEnabled: true}, Log: log, Exec: exec,
		Policy: newScriptedTestPolicy(log, ExecutionResult{TimedOut: true}), Prompts: testPrompts{},
	})

	output, err := phase.Run(t.Context(), "facts")

	require.NoError(t, err)
	assert.Empty(t, output)
	assertLogContains(t, log, "report step timed out")
}

func TestReportPhase_RunExecutorErrors(t *testing.T) {
	t.Run("ordinary error is non-blocking", func(t *testing.T) {
		log := newMockLogger("progress.txt")
		exec := newTaskPhaseMockExecutor([]executor.Result{{Error: errors.New("report unavailable")}})
		phase := NewReportPhase(ReportPhaseOpts{
			Cfg: Config{ReportEnabled: true}, Log: log, Exec: exec,
			Policy: newTestPolicy(Config{}, log), Prompts: testPrompts{},
		})

		output, err := phase.Run(t.Context(), "facts")

		require.NoError(t, err)
		assert.Empty(t, output)
		assertLogContains(t, log, "report step failed: %v")
	})

	t.Run("context cancellation remains blocking", func(t *testing.T) {
		log := newMockLogger("progress.txt")
		exec := newTaskPhaseMockExecutor([]executor.Result{{Error: context.Canceled}})
		phase := NewReportPhase(ReportPhaseOpts{
			Cfg: Config{ReportEnabled: true}, Log: log, Exec: exec,
			Policy: newTestPolicy(Config{}, log), Prompts: testPrompts{},
		})

		output, err := phase.Run(t.Context(), "facts")

		assert.Empty(t, output)
		require.ErrorIs(t, err, context.Canceled)
		assert.ErrorContains(t, err, "report step")
	})
}

func TestReportPhase_RunDisabled(t *testing.T) {
	log := newMockLogger("progress.txt")
	exec := newTaskPhaseMockExecutor(nil)
	holder := &status.PhaseHolder{}
	phase := NewReportPhase(ReportPhaseOpts{
		Cfg: Config{ReportEnabled: false}, Log: log, Exec: exec,
		Policy: newTestPolicy(Config{}, log), Prompts: testPrompts{}, PhaseHolder: holder,
	})

	output, err := phase.Run(t.Context(), "facts")

	require.NoError(t, err)
	assert.Empty(t, output)
	assert.Empty(t, exec.RunCalls())
	assert.Empty(t, log.PrintSectionCalls())
	assert.Equal(t, status.Phase(""), holder.Get())
}
