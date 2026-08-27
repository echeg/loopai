package orca

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/umputun/ralphex/pkg/status"
)

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
