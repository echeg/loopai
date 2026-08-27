package orca

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/status"
)

func TestNew(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		assert.Nil(t, newReporter(false, "plan.md", config.ExecutorClaude, io.Discard, func() bool { return true }))
	})

	t.Run("stdout is not terminal", func(t *testing.T) {
		assert.Nil(t, newReporter(true, "plan.md", config.ExecutorClaude, io.Discard, func() bool { return false }))
	})

	t.Run("enabled terminal", func(t *testing.T) {
		r := newReporter(true, "plan.md", config.ExecutorCodex, io.Discard, func() bool { return true })
		require.NotNil(t, r)
		assert.Equal(t, "plan.md", r.planFile)
		assert.Equal(t, "codex", r.executor)
	})

	assert.Nil(t, New(false, "plan.md", config.ExecutorClaude))
}

func TestReporterNilReceiver(t *testing.T) {
	var r *Reporter

	tests := []struct {
		name string
		call func()
	}{
		{name: "on phase", call: func() { r.OnPhase(status.PhaseTask, status.PhaseReview) }},
		{name: "on section", call: func() { r.OnSection(status.NewTaskIterationSection(1)) }},
		{name: "finish", call: func() { r.Finish(true) }},
		{name: "stop", call: func() { r.Stop() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotPanics(t, tt.call)
		})
	}
}

func TestReporterOnPhaseAsObserver(t *testing.T) {
	tests := []struct {
		name     string
		phase    status.Phase
		executor string
		want     string
	}{
		{name: "task", phase: status.PhaseTask, executor: config.ExecutorClaude, want: "\x1b]0;◐ loopai · task · claude\a"},
		{name: "review", phase: status.PhaseReview, executor: config.ExecutorClaude, want: "\x1b]0;◐ loopai · review · claude\a"},
		{name: "external review", phase: status.PhaseExternalReview, executor: config.ExecutorClaude, want: "\x1b]0;◐ loopai · external review · claude\a"},
		{name: "external evaluation", phase: status.PhaseExternalEval, executor: config.ExecutorCodex, want: "\x1b]0;◐ loopai · external eval · codex\a"},
		{name: "plan", phase: status.PhasePlan, executor: config.ExecutorClaude, want: "\x1b]0;◐ loopai · plan · claude\a"},
		{name: "finalize", phase: status.PhaseFinalize, executor: config.ExecutorCodex, want: "\x1b]0;◐ loopai · finalize · codex\a"},
		{name: "limit wait", phase: status.PhaseLimitWait, executor: config.ExecutorCodex, want: "\x1b]0;loopai · waiting for limit · codex\a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			r := requireReporter(t, &out, tt.executor)
			holder := &status.PhaseHolder{}
			holder.OnChange(r.OnPhase)

			holder.Set(tt.phase)
			holder.Set(tt.phase)

			assert.Equal(t, tt.want, out.String(), "a repeated phase must not emit another title")
		})
	}
}

func TestReporterFinish(t *testing.T) {
	tests := []struct {
		name    string
		success bool
		want    string
	}{
		{name: "success", success: true, want: "\x1b]0;✳ loopai · done\a"},
		{name: "failure", success: false, want: "\x1b]0;✳ loopai · failed\a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			r := requireReporter(t, &out, config.ExecutorClaude)

			r.Finish(tt.success)
			r.OnPhase(status.PhaseTask, status.PhaseReview)
			r.OnSection(status.NewInternalReviewSection(2, ""))
			r.Finish(!tt.success)

			assert.Equal(t, tt.want, out.String(), "the final title must freeze the reporter")
		})
	}
}

func TestReporterStop(t *testing.T) {
	t.Run("without finish", func(t *testing.T) {
		var out bytes.Buffer
		r := requireReporter(t, &out, config.ExecutorClaude)

		r.Stop()
		r.Stop()
		r.OnPhase(status.PhaseTask, status.PhaseReview)
		r.OnSection(status.NewInternalReviewSection(2, ""))

		assert.Equal(t, "\x1b]0;✳ loopai\a", out.String())
	})

	t.Run("after finish", func(t *testing.T) {
		var out bytes.Buffer
		r := requireReporter(t, &out, config.ExecutorClaude)

		r.Finish(true)
		r.Stop()
		r.Stop()

		assert.Equal(t, "\x1b]0;✳ loopai · done\a", out.String())
	})
}

func requireReporter(t *testing.T, w io.Writer, executor string) *Reporter {
	t.Helper()
	r := newReporter(true, "plan.md", executor, w, func() bool { return true })
	require.NotNil(t, r)
	return r
}

func TestTitleFor(t *testing.T) {
	tests := []struct {
		name     string
		state    state
		executor string
		want     string
	}{
		{name: "task with total", state: state{phase: status.PhaseTask, task: 3, total: 7}, executor: "claude", want: "◐ loopai · task 3/7 · claude"},
		{name: "task without total", state: state{phase: status.PhaseTask, task: 3}, executor: "claude", want: "◐ loopai · task 3 · claude"},
		{name: "task phase", state: state{phase: status.PhaseTask}, executor: "claude", want: "◐ loopai · task · claude"},
		{name: "review phase", state: state{phase: status.PhaseReview}, executor: "claude", want: "◐ loopai · review · claude"},
		{name: "review iteration", state: state{phase: status.PhaseReview, iteration: 2}, executor: "claude", want: "◐ loopai · review · iteration 2 · claude"},
		{name: "external review phase", state: state{phase: status.PhaseExternalReview}, executor: "claude", want: "◐ loopai · external review · claude"},
		{name: "external review iteration", state: state{phase: status.PhaseExternalReview, iteration: 1}, executor: "claude", want: "◐ loopai · external review · iteration 1 · claude"},
		{name: "external evaluation", state: state{phase: status.PhaseExternalEval}, executor: "claude", want: "◐ loopai · external eval · claude"},
		{name: "plan phase", state: state{phase: status.PhasePlan}, executor: "claude", want: "◐ loopai · plan · claude"},
		{name: "plan iteration", state: state{phase: status.PhasePlan, iteration: 2}, executor: "claude", want: "◐ loopai · plan · iteration 2 · claude"},
		{name: "finalize", state: state{phase: status.PhaseFinalize}, executor: "codex", want: "◐ loopai · finalize · codex"},
		{name: "waiting for input", state: state{waiting: waitingInput}, executor: "claude", want: "loopai · waiting for input · claude"},
		{name: "waiting for limit", state: state{waiting: waitingLimit}, executor: "codex", want: "loopai · waiting for limit · codex"},
		{name: "done", state: state{final: finalDone}, executor: "claude", want: "✳ loopai · done"},
		{name: "failed", state: state{final: finalFailed}, executor: "claude", want: "✳ loopai · failed"},
		{name: "stopped", state: state{final: finalStopped}, executor: "claude", want: "✳ loopai"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, titleFor(tt.state, tt.executor))
		})
	}
}

func TestTitleForUnknownState(t *testing.T) {
	assert.Empty(t, titleFor(state{}, "claude"))
	assert.Empty(t, titleFor(state{phase: status.Phase("unknown")}, "claude"))
}

func TestWriteTitle(t *testing.T) {
	w := &countingWriter{}
	writeTitle(w, "◐ loopai · task 3/7 · claude")

	assert.Equal(t, 1, w.calls)
	assert.Equal(t, "\x1b]0;◐ loopai · task 3/7 · claude\a", w.String())
}

func TestWriteTitleSwallowsWriterError(t *testing.T) {
	w := &countingWriter{err: errors.New("write failed")}

	assert.NotPanics(t, func() {
		writeTitle(w, "✳ loopai · failed")
	})
	assert.Equal(t, 1, w.calls)
}

type countingWriter struct {
	bytes.Buffer
	calls int
	err   error
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.err != nil {
		return 0, w.err
	}
	return w.Buffer.Write(p) //nolint:wrapcheck // test writer deliberately preserves the inner result
}
