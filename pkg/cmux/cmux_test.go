package cmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/status"
)

// fakeRunner records argv of every call instead of spawning the real cmux binary.
type fakeRunner struct {
	mu     sync.Mutex
	calls  [][]string
	err    error         // returned from run when set
	block  time.Duration // when set, run waits for it or for ctx cancellation
	ctxErr error         // context error observed by the last blocking call
}

func (f *fakeRunner) run(ctx context.Context, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	block := f.block
	err := f.err
	f.mu.Unlock()

	if block > 0 {
		select {
		case <-time.After(block):
		case <-ctx.Done():
			f.mu.Lock()
			f.ctxErr = ctx.Err()
			f.mu.Unlock()
			return ctx.Err() //nolint:wrapcheck // test double
		}
	}
	return err
}

func (f *fakeRunner) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

// testReporter builds a reporter wired to a fake runner, bypassing environment detection.
func testReporter(t *testing.T, runner commandRunner) *Reporter {
	t.Helper()
	return &Reporter{runner: runner, timeout: time.Second}
}

// writeFakeBin creates an executable file in dir so exec.LookPath can find it.
func writeFakeBin(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755)) //nolint:gosec // test fixture must be executable
}

func TestNew(t *testing.T) {
	binDir := t.TempDir()
	writeFakeBin(t, binDir, binName)
	emptyDir := t.TempDir()

	tests := []struct {
		name      string
		workspace string
		path      string
		wantNil   bool
	}{
		{name: "workspace and binary present", workspace: "ws-1", path: binDir},
		{name: "no workspace id", workspace: "", path: binDir, wantNil: true},
		{name: "blank workspace id", workspace: "  \t ", path: binDir, wantNil: true},
		{name: "no binary in path", workspace: "ws-1", path: emptyDir, wantNil: true},
		{name: "no workspace and no binary", workspace: "", path: emptyDir, wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(workspaceEnv, tt.workspace)
			t.Setenv("PATH", tt.path)

			r := New("docs/plans/feature.md")
			if tt.wantNil {
				assert.Nil(t, r)
				return
			}
			require.NotNil(t, r)
			assert.Equal(t, "docs/plans/feature.md", r.planFile)
			assert.Equal(t, execTimeout, r.timeout)
			assert.NotNil(t, r.runner)
		})
	}
}

func TestNewUnsetWorkspaceEnv(t *testing.T) {
	binDir := t.TempDir()
	writeFakeBin(t, binDir, binName)
	t.Setenv("PATH", binDir)
	t.Setenv(workspaceEnv, "ws-1")
	require.NoError(t, os.Unsetenv(workspaceEnv))

	assert.Nil(t, New(""), "unset workspace env must disable the reporter")
}

func TestReporterExecNilSafe(t *testing.T) {
	var r *Reporter
	assert.NotPanics(t, func() { r.exec("workspace", "loading", "on") })
}

func TestReporterExecNoRunner(t *testing.T) {
	r := &Reporter{timeout: time.Second}
	assert.NotPanics(t, func() { r.exec("clear-status", "ralphex") })
}

func TestReporterExec(t *testing.T) {
	tests := []struct {
		name     string
		runErr   error
		args     []string
		wantArgs []string
	}{
		{name: "successful call", args: []string{"clear-progress"}, wantArgs: []string{"clear-progress"}},
		{
			name:     "runner error is swallowed",
			runErr:   errors.New("cmux exploded"),
			args:     []string{"set-status", "ralphex", "task"},
			wantArgs: []string{"set-status", "ralphex", "task"},
		},
		{name: "no args", args: nil, wantArgs: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{err: tt.runErr}
			r := testReporter(t, runner)

			assert.NotPanics(t, func() { r.exec(tt.args...) })

			calls := runner.recorded()
			require.Len(t, calls, 1)
			assert.Equal(t, tt.wantArgs, calls[0])
		})
	}
}

func TestReporterExecTimeout(t *testing.T) {
	runner := &fakeRunner{block: 5 * time.Second}
	r := &Reporter{runner: runner, timeout: 20 * time.Millisecond}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.exec("notify", "--title", "ralphex")
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("exec did not return after the per-call timeout expired")
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	assert.ErrorIs(t, runner.ctxErr, context.DeadlineExceeded, "runner must observe the deadline")
}

func TestReporterSidebarCommands(t *testing.T) {
	tests := []struct {
		name string
		call func(r *Reporter)
		want [][]string
	}{
		{
			name: "loading on",
			call: func(r *Reporter) { r.LoadingOn() },
			want: [][]string{{"workspace", "loading", "on", "--id", "ralphex"}},
		},
		{
			name: "loading off",
			call: func(r *Reporter) { r.LoadingOff() },
			want: [][]string{{"workspace", "loading", "off", "--id", "ralphex"}},
		},
		{
			name: "status with icon and color",
			call: func(r *Reporter) { r.Status("task", "hammer", "#22c55e") },
			want: [][]string{{"set-status", "ralphex", "task", "--icon", "hammer", "--color", "#22c55e", "--priority", "90"}},
		},
		{
			name: "status without icon",
			call: func(r *Reporter) { r.Status("task", "", "#22c55e") },
			want: [][]string{{"set-status", "ralphex", "task", "--color", "#22c55e", "--priority", "90"}},
		},
		{
			name: "status without color",
			call: func(r *Reporter) { r.Status("task", "hammer", "") },
			want: [][]string{{"set-status", "ralphex", "task", "--icon", "hammer", "--priority", "90"}},
		},
		{
			name: "status without icon and color",
			call: func(r *Reporter) { r.Status("external review", "", "") },
			want: [][]string{{"set-status", "ralphex", "external review", "--priority", "90"}},
		},
		{
			name: "clear status",
			call: func(r *Reporter) { r.ClearStatus() },
			want: [][]string{{"clear-status", "ralphex"}},
		},
		{
			name: "clear progress",
			call: func(r *Reporter) { r.ClearProgress() },
			want: [][]string{{"clear-progress"}},
		},
		{
			name: "notify with all fields",
			call: func(r *Reporter) { r.Notify("ralphex", "нужен ответ", "which option?") },
			want: [][]string{{"notify", "--title", "ralphex", "--subtitle", "нужен ответ", "--body", "which option?"}},
		},
		{
			name: "notify without subtitle",
			call: func(r *Reporter) { r.Notify("ralphex", "", "done") },
			want: [][]string{{"notify", "--title", "ralphex", "--body", "done"}},
		},
		{
			name: "notify without body",
			call: func(r *Reporter) { r.Notify("ralphex", "run finished", "") },
			want: [][]string{{"notify", "--title", "ralphex", "--subtitle", "run finished"}},
		},
		{
			name: "notify title only",
			call: func(r *Reporter) { r.Notify("ralphex", "", "") },
			want: [][]string{{"notify", "--title", "ralphex"}},
		},
		{
			name: "notify body truncated to the rune limit",
			call: func(r *Reporter) { r.Notify("ralphex", "", strings.Repeat("я", notifyBodyLimit+50)) },
			want: [][]string{{"notify", "--title", "ralphex", "--body", strings.Repeat("я", notifyBodyLimit)}},
		},
		{
			name: "notify body at the rune limit is kept whole",
			call: func(r *Reporter) { r.Notify("ralphex", "", strings.Repeat("я", notifyBodyLimit)) },
			want: [][]string{{"notify", "--title", "ralphex", "--body", strings.Repeat("я", notifyBodyLimit)}},
		},
		{
			name: "clear sends all three commands",
			call: func(r *Reporter) { r.Clear() },
			want: [][]string{
				{"workspace", "loading", "off", "--id", "ralphex"},
				{"clear-status", "ralphex"},
				{"clear-progress"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			tt.call(testReporter(t, runner))
			assert.Equal(t, tt.want, runner.recorded())
		})
	}
}

func TestReporterProgress(t *testing.T) {
	tests := []struct {
		name        string
		done, total int
		label       string
		want        [][]string
	}{
		{name: "half done", done: 1, total: 2, label: "tasks", want: [][]string{{"set-progress", "0.50", "--label", "tasks"}}},
		{name: "none done", done: 0, total: 4, label: "tasks", want: [][]string{{"set-progress", "0.00", "--label", "tasks"}}},
		{name: "all done", done: 4, total: 4, label: "tasks", want: [][]string{{"set-progress", "1.00", "--label", "tasks"}}},
		{name: "rounded to two decimals", done: 1, total: 3, label: "tasks", want: [][]string{{"set-progress", "0.33", "--label", "tasks"}}},
		{name: "rounded up", done: 2, total: 3, label: "tasks", want: [][]string{{"set-progress", "0.67", "--label", "tasks"}}},
		{name: "empty label skips the flag", done: 1, total: 2, want: [][]string{{"set-progress", "0.50"}}},
		{name: "zero total is skipped", done: 3, total: 0, label: "tasks"},
		{name: "negative total is skipped", done: 3, total: -2, label: "tasks"},
		{name: "done above total is clamped", done: 9, total: 4, label: "tasks", want: [][]string{{"set-progress", "1.00", "--label", "tasks"}}},
		{name: "negative done is clamped", done: -3, total: 4, label: "tasks", want: [][]string{{"set-progress", "0.00", "--label", "tasks"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			testReporter(t, runner).Progress(tt.done, tt.total, tt.label)
			assert.Equal(t, tt.want, runner.recorded())
		})
	}
}

func TestReporterClearIdempotent(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	r.Clear()
	r.Clear()

	calls := runner.recorded()
	require.Len(t, calls, 6, "a repeated clear must send the same commands again, cmux ignores them")
	assert.Equal(t, calls[:3], calls[3:], "the second clear must be identical to the first")
}

func TestStyleForPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase status.Phase
		want  phaseStyle
	}{
		{name: "task", phase: status.PhaseTask, want: phaseStyle{text: "task", icon: "hammer", color: "#22c55e"}},
		{name: "review", phase: status.PhaseReview, want: phaseStyle{text: "review", icon: "magnifyingglass", color: "#06b6d4"}},
		{
			name:  "external review",
			phase: status.PhaseExternalReview,
			want:  phaseStyle{text: "external review", icon: "person.2", color: "#a855f7"},
		},
		{
			name:  "external eval",
			phase: status.PhaseExternalEval,
			want:  phaseStyle{text: "evaluating findings", icon: "checkmark.seal", color: "#a855f7"},
		},
		{name: "finalize", phase: status.PhaseFinalize, want: phaseStyle{text: "finalize", icon: "flag.checkered", color: "#22c55e"}},
		{name: "plan", phase: status.PhasePlan, want: phaseStyle{text: "planning", icon: "list.bullet.clipboard", color: "#3b82f6"}},
		// legacy phases are only parsed from historical progress files, they fall back to the raw value
		{name: "legacy codex", phase: status.PhaseCodex, want: phaseStyle{text: "codex", icon: unknownPhaseIcon}},
		{name: "legacy claude eval", phase: status.PhaseClaudeEval, want: phaseStyle{text: "claude-eval", icon: unknownPhaseIcon}},
		{name: "unknown phase", phase: status.Phase("brand-new"), want: phaseStyle{text: "brand-new", icon: unknownPhaseIcon}},
		{name: "empty phase", phase: status.Phase(""), want: phaseStyle{icon: unknownPhaseIcon}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotPanics(t, func() { styleForPhase(tt.phase) })
			assert.Equal(t, tt.want, styleForPhase(tt.phase))
		})
	}
}

func TestReporterOnPhase(t *testing.T) {
	tests := []struct {
		name string
		old  status.Phase
		cur  status.Phase
		want []string
	}{
		{
			name: "mapped phase",
			old:  status.PhaseTask,
			cur:  status.PhaseReview,
			want: []string{"set-status", "ralphex", "review", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
		},
		{
			name: "unknown phase falls back to its raw value without color",
			cur:  status.Phase("mystery"),
			want: []string{"set-status", "ralphex", "mystery", "--icon", "circle", "--priority", "90"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			testReporter(t, runner).OnPhase(tt.old, tt.cur)

			calls := runner.recorded()
			require.Len(t, calls, 1)
			assert.Equal(t, tt.want, calls[0])
		})
	}
}

func TestReporterOnPhaseAsObserver(t *testing.T) {
	runner := &fakeRunner{}
	holder := &status.PhaseHolder{}
	holder.OnChange(testReporter(t, runner).OnPhase)

	holder.Set(status.PhaseTask)
	holder.Set(status.PhaseTask) // same phase must not fire the observer again
	holder.Set(status.PhaseFinalize)

	assert.Equal(t, [][]string{
		{"set-status", "ralphex", "task", "--icon", "hammer", "--color", "#22c55e", "--priority", "90"},
		{"set-status", "ralphex", "finalize", "--icon", "flag.checkered", "--color", "#22c55e", "--priority", "90"},
	}, runner.recorded())
}

func TestReporterNilReceiver(t *testing.T) {
	var r *Reporter

	tests := []struct {
		name string
		call func()
	}{
		{name: "loading on", call: func() { r.LoadingOn() }},
		{name: "loading off", call: func() { r.LoadingOff() }},
		{name: "status", call: func() { r.Status("task", "hammer", "#22c55e") }},
		{name: "clear status", call: func() { r.ClearStatus() }},
		{name: "progress", call: func() { r.Progress(1, 2, "tasks") }},
		{name: "progress with zero total", call: func() { r.Progress(1, 0, "tasks") }},
		{name: "clear progress", call: func() { r.ClearProgress() }},
		{name: "notify", call: func() { r.Notify("ralphex", "done", "all tasks complete") }},
		{name: "clear", call: func() { r.Clear() }},
		{name: "on phase", call: func() { r.OnPhase(status.PhaseTask, status.PhaseReview) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotPanics(t, tt.call)
		})
	}
}

func TestExecRunner(t *testing.T) {
	t.Run("command succeeds", func(t *testing.T) {
		r := &execRunner{bin: "true"}
		assert.NoError(t, r.run(t.Context()))
	})

	t.Run("command fails", func(t *testing.T) {
		r := &execRunner{bin: "false"}
		err := r.run(t.Context(), "set-status")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "set-status")
	})

	t.Run("binary missing", func(t *testing.T) {
		r := &execRunner{bin: filepath.Join(t.TempDir(), "no-such-binary")}
		assert.Error(t, r.run(t.Context()))
	})

	t.Run("context canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r := &execRunner{bin: "true"}
		assert.Error(t, r.run(ctx))
	})
}
