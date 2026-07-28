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

	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/status"
)

// fakeRunner records argv of every call instead of spawning the real cmux binary.
type fakeRunner struct {
	mu     sync.Mutex
	calls  [][]string
	err    error          // returned from run when set
	block  time.Duration  // when set, run waits for it or for ctx cancellation
	ctxErr error          // context error observed by the last blocking call
	onCall func([]string) // when set, called with argv before run returns
}

func (f *fakeRunner) run(ctx context.Context, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	block := f.block
	err := f.err
	onCall := f.onCall
	f.mu.Unlock()

	if onCall != nil {
		onCall(args)
	}

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

// waitForCalls polls the fake runner until it records at least n calls or the deadline expires.
func (f *fakeRunner) waitForCalls(t *testing.T, n int) [][]string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls := f.recorded()
		if len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected at least %d calls, got %d: %v", n, len(calls), calls)
		}
		time.Sleep(time.Millisecond)
	}
}

// testReporter builds a reporter wired to a fake runner, bypassing environment detection.
func testReporter(t *testing.T, runner commandRunner) *Reporter {
	t.Helper()
	return &Reporter{runner: runner, timeout: time.Second, interval: time.Hour, lastDone: -1, lastTotal: -1}
}

type fakeLogger struct {
	sections []status.Section
}

func (l *fakeLogger) Print(string, ...any)                {}
func (l *fakeLogger) PrintRaw(string, ...any)             {}
func (l *fakeLogger) PrintAligned(string)                 {}
func (l *fakeLogger) LogQuestion(string, []string)        {}
func (l *fakeLogger) LogAnswer(string)                    {}
func (l *fakeLogger) LogDraftReview(string, string)       {}
func (l *fakeLogger) Path() string                        { return "progress.txt" }
func (l *fakeLogger) PrintSection(section status.Section) { l.sections = append(l.sections, section) }

// writePlan writes a plan file into a temp dir and returns its path.
func writePlan(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
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
			assert.Equal(t, pollInterval, r.interval)
			assert.Equal(t, -1, r.lastDone, "the first tick must always report")
			assert.Equal(t, -1, r.lastTotal)
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
	assert.NotPanics(t, func() { r.exec("clear-status", "loopai") })
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
			args:     []string{"set-status", "loopai", "task"},
			wantArgs: []string{"set-status", "loopai", "task"},
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
		r.exec("notify", "--title", "loopai")
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
			call: func(r *Reporter) { r.loadingOn() },
			want: [][]string{{"workspace", "loading", "on", "--id", "loopai"}},
		},
		{
			name: "loading off",
			call: func(r *Reporter) { r.loadingOff() },
			want: [][]string{{"workspace", "loading", "off", "--id", "loopai"}},
		},
		{
			name: "status with icon and color",
			call: func(r *Reporter) { r.setStatus("task", "hammer", "#22c55e") },
			want: [][]string{{"set-status", "loopai", "task", "--icon", "hammer", "--color", "#22c55e", "--priority", "90"}},
		},
		{
			name: "status without icon",
			call: func(r *Reporter) { r.setStatus("task", "", "#22c55e") },
			want: [][]string{{"set-status", "loopai", "task", "--color", "#22c55e", "--priority", "90"}},
		},
		{
			name: "status without color",
			call: func(r *Reporter) { r.setStatus("task", "hammer", "") },
			want: [][]string{{"set-status", "loopai", "task", "--icon", "hammer", "--priority", "90"}},
		},
		{
			name: "status without icon and color",
			call: func(r *Reporter) { r.setStatus("external review", "", "") },
			want: [][]string{{"set-status", "loopai", "external review", "--priority", "90"}},
		},
		{
			name: "clear status",
			call: func(r *Reporter) { r.clearStatus() },
			want: [][]string{{"clear-status", "loopai"}},
		},
		{
			name: "clear progress",
			call: func(r *Reporter) { r.clearProgress() },
			want: [][]string{{"clear-progress"}},
		},
		{
			name: "notify with all fields",
			call: func(r *Reporter) { r.Notify("нужен ответ", "which option?") },
			want: [][]string{{"notify", "--title", "loopai", "--subtitle", "нужен ответ", "--body", "which option?"}},
		},
		{
			name: "notify without subtitle",
			call: func(r *Reporter) { r.Notify("", "done") },
			want: [][]string{{"notify", "--title", "loopai", "--body", "done"}},
		},
		{
			name: "notify without body",
			call: func(r *Reporter) { r.Notify("run finished", "") },
			want: [][]string{{"notify", "--title", "loopai", "--subtitle", "run finished"}},
		},
		{
			name: "notify title only",
			call: func(r *Reporter) { r.Notify("", "") },
			want: [][]string{{"notify", "--title", "loopai"}},
		},
		{
			name: "notify body truncated to the rune limit",
			call: func(r *Reporter) { r.Notify("", strings.Repeat("я", notifyBodyLimit+50)) },
			want: [][]string{{"notify", "--title", "loopai", "--body", strings.Repeat("я", notifyBodyLimit)}},
		},
		{
			name: "notify body at the rune limit is kept whole",
			call: func(r *Reporter) { r.Notify("", strings.Repeat("я", notifyBodyLimit)) },
			want: [][]string{{"notify", "--title", "loopai", "--body", strings.Repeat("я", notifyBodyLimit)}},
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

func TestReporterSetProgress(t *testing.T) {
	tests := []struct {
		name  string
		ratio float64
		label string
		want  [][]string
	}{
		{name: "half done", ratio: 0.5, label: "tasks", want: [][]string{{"set-progress", "0.50", "--label", "tasks"}}},
		{name: "none done", ratio: 0, label: "tasks", want: [][]string{{"set-progress", "0.00", "--label", "tasks"}}},
		{name: "all done", ratio: 1, label: "tasks", want: [][]string{{"set-progress", "1.00", "--label", "tasks"}}},
		{name: "rounded to two decimals", ratio: 1.0 / 3, label: "tasks", want: [][]string{{"set-progress", "0.33", "--label", "tasks"}}},
		{name: "rounded up", ratio: 2.0 / 3, label: "tasks", want: [][]string{{"set-progress", "0.67", "--label", "tasks"}}},
		{name: "empty label skips the flag", ratio: 0.5, want: [][]string{{"set-progress", "0.50"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			testReporter(t, runner).setProgress(tt.ratio, tt.label)
			assert.Equal(t, tt.want, runner.recorded())
		})
	}
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
			want: []string{"set-status", "loopai", "review", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
		},
		{
			name: "unknown phase falls back to its raw value without color",
			cur:  status.Phase("mystery"),
			want: []string{"set-status", "loopai", "mystery", "--icon", "circle", "--priority", "90"},
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

func TestReporterOnPhaseIncludesEffectiveModel(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.models = Models{
		Task:           "gpt-5.6:high",
		Review:         "gpt-5.6:medium",
		ExternalReview: "opus:xhigh",
	}

	r.OnPhase("", status.PhaseTask)
	r.OnPhase(status.PhaseTask, status.PhaseReview)
	r.OnPhase(status.PhaseReview, status.PhaseExternalReview)
	r.OnPhase(status.PhaseExternalReview, status.PhaseExternalEval)

	assert.Equal(t, [][]string{
		{"set-status", "loopai", "task (gpt-5.6:high)", "--icon", "hammer", "--color", "#22c55e", "--priority", "90"},
		{"set-status", "loopai", "review (gpt-5.6:medium)", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
		{"set-status", "loopai", "external review (opus:xhigh)", "--icon", "person.2", "--color", "#a855f7", "--priority", "90"},
		{"set-status", "loopai", "evaluating findings (gpt-5.6:medium)", "--icon", "checkmark.seal", "--color", "#a855f7", "--priority", "90"},
	}, runner.recorded())
}

func TestReporterOnSectionShowsReviewIteration(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.models = Models{Review: "gpt-5.6:medium", ExternalReview: "opus:xhigh"}

	r.OnSection(status.NewInternalReviewSection(0, ": all findings"))
	r.OnSection(status.NewInternalReviewSection(3, ": critical/major"))
	r.OnSection(status.NewExternalReviewIterationSection("claude", 2))
	r.OnSection(status.NewGenericSection("ignored"))

	assert.Equal(t, [][]string{
		{"set-status", "loopai", "review (gpt-5.6:medium) · iteration 0", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
		{"set-status", "loopai", "review (gpt-5.6:medium) · iteration 3", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
		{"set-status", "loopai", "external review (opus:xhigh) · iteration 2", "--icon", "person.2", "--color", "#a855f7", "--priority", "90"},
	}, runner.recorded())
}

func TestReporterWrapLoggerObservesSections(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.models = Models{Review: "gpt-5.6:medium"}
	inner := &fakeLogger{}

	wrapped := r.WrapLogger(inner)
	section := status.NewInternalReviewSection(4, ": critical/major")
	wrapped.PrintSection(section)

	assert.Equal(t, []status.Section{section}, inner.sections)
	assert.Equal(t, [][]string{{
		"set-status", "loopai", "review (gpt-5.6:medium) · iteration 4",
		"--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90",
	}}, runner.recorded())

	var nilReporter *Reporter
	assert.Same(t, inner, nilReporter.WrapLogger(inner))
}

func TestReporterOnPhaseAsObserver(t *testing.T) {
	runner := &fakeRunner{}
	holder := &status.PhaseHolder{}
	holder.OnChange(testReporter(t, runner).OnPhase)

	holder.Set(status.PhaseTask)
	holder.Set(status.PhaseTask) // same phase must not fire the observer again
	holder.Set(status.PhaseFinalize)

	assert.Equal(t, [][]string{
		{"set-status", "loopai", "task", "--icon", "hammer", "--color", "#22c55e", "--priority", "90"},
		{"set-status", "loopai", "finalize", "--icon", "flag.checkered", "--color", "#22c55e", "--priority", "90"},
	}, runner.recorded())
}

func TestReporterOnPhaseAfterStop(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	r.Stop()
	r.OnPhase(status.PhaseTask, status.PhaseReview)

	assert.Equal(t, [][]string{
		{"workspace", "loading", "off", "--id", "loopai"},
		{"clear-status", "loopai"},
		{"clear-progress"},
	}, runner.recorded(), "a phase change after stop must not re-add the pill")
}

func TestReporterStopWaitsForPillUpdateInFlight(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	// park OnPhase inside its own set-status call, holding statusMu, and let Stop catch up on it
	inFlight, release := make(chan struct{}), make(chan struct{})
	runner.mu.Lock()
	runner.onCall = func(args []string) {
		if args[0] != "set-status" {
			return
		}
		close(inFlight)
		<-release
	}
	runner.mu.Unlock()

	go r.OnPhase(status.PhaseTask, status.PhaseReview)
	<-inFlight

	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()

	// the spinner clear takes no lock, so it lands; the pill clear is behind the update in flight
	runner.waitForCalls(t, 2)
	assert.NotContains(t, runner.recorded(), []string{"clear-status", "loopai"},
		"the pill clear must wait for the update in flight instead of racing it")

	close(release)
	<-stopped

	assert.Equal(t, [][]string{
		{"set-status", "loopai", "review", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
		{"workspace", "loading", "off", "--id", "loopai"},
		{"clear-status", "loopai"},
		{"clear-progress"},
	}, runner.recorded(), "the pill clear must land after the update it waited out")
}

func TestReporterOnPhaseConcurrentWithStop(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	// models the force-exit path: the execution goroutine still changes phase while Stop tears down
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { r.OnPhase(status.PhaseTask, status.PhaseReview) })
	}
	wg.Go(r.Stop)
	wg.Wait()

	cleared := false
	for _, call := range runner.recorded() {
		switch call[0] {
		case "clear-status":
			cleared = true
		case "set-status":
			assert.False(t, cleared, "the pill must never be set after the clear")
		}
	}
}

func TestReporterNotifyAfterStop(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	r.Stop()
	r.Notify("run failed", "boom")

	// a banner is transient state, so it is deliberately not gated by Stop: the plan-mode handoff
	// stops the reporter before delegating and still reports a failure through it
	assert.Equal(t, []string{"notify", "--title", "loopai", "--subtitle", "run failed", "--body", "boom"},
		runner.recorded()[3], "notify must still go out after stop")
}

func TestReporterNilReceiver(t *testing.T) {
	var r *Reporter

	tests := []struct {
		name string
		call func()
	}{
		{name: "loading on", call: func() { r.loadingOn() }},
		{name: "loading off", call: func() { r.loadingOff() }},
		{name: "status", call: func() { r.setStatus("task", "hammer", "#22c55e") }},
		{name: "clear status", call: func() { r.clearStatus() }},
		{name: "progress", call: func() { r.setProgress(0.5, "tasks") }},
		{name: "clear progress", call: func() { r.clearProgress() }},
		{name: "notify", call: func() { r.Notify("done", "all tasks complete") }},
		{name: "on phase", call: func() { r.OnPhase(status.PhaseTask, status.PhaseReview) }},
		{name: "start", call: func() { r.Start(context.Background()) }},
		{name: "stop", call: func() { r.Stop() }},
		{name: "wrap input", call: func() { r.WrapInput(&fakeCollector{}) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotPanics(t, tt.call)
		})
	}
}

func TestReporterReportProgress(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    [][]string
	}{
		{
			name:    "all tasks done",
			content: "# plan\n\n### Task 1: one\n\n- [x] a\n- [x] b\n\n### Task 2: two\n\n- [x] c\n",
			want:    [][]string{{"set-progress", "1.00", "--label", "2/2 tasks"}},
		},
		{
			name:    "partially done",
			content: "# plan\n\n### Task 1: one\n\n- [x] a\n\n### Task 2: two\n\n- [ ] b\n\n### Task 3: three\n\n- [ ] c\n",
			want:    [][]string{{"set-progress", "0.33", "--label", "1/3 tasks"}},
		},
		{
			name:    "task started but not finished counts as not done",
			content: "# plan\n\n### Task 1: one\n\n- [x] a\n- [ ] b\n\n### Task 2: two\n\n- [x] c\n",
			want:    [][]string{{"set-progress", "0.50", "--label", "1/2 tasks"}},
		},
		{
			name:    "no tasks done",
			content: "# plan\n\n### Task 1: one\n\n- [ ] a\n\n### Task 2: two\n\n- [ ] b\n",
			want:    [][]string{{"set-progress", "0.00", "--label", "0/2 tasks"}},
		},
		{
			name:    "plan without task sections is skipped",
			content: "# plan\n\n## Overview\n\nno tasks here\n\n- [ ] not a task checkbox\n",
		},
		{name: "empty plan is skipped", content: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			r := testReporter(t, runner)
			r.planFile = writePlan(t, tt.content)

			r.reportProgress()
			assert.Equal(t, tt.want, runner.recorded())
		})
	}
}

func TestReporterReportProgressSkipsTick(t *testing.T) {
	tests := []struct {
		name     string
		planFile func(t *testing.T) string
	}{
		{name: "empty plan path", planFile: func(*testing.T) string { return "" }},
		{
			name: "missing plan file, it may have moved to completed/",
			planFile: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "gone.md")
			},
		},
		{
			name: "unreadable plan file",
			planFile: func(t *testing.T) string {
				t.Helper()
				return t.TempDir() // a directory fails to read as a file
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			r := testReporter(t, runner)
			r.planFile = tt.planFile(t)

			assert.NotPanics(t, func() { r.reportProgress() })
			assert.Empty(t, runner.recorded(), "a skipped tick must not touch the sidebar")
		})
	}
}

func TestReporterReportProgressUnchangedPair(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n\n### Task 2: two\n\n- [ ] b\n")

	r.reportProgress()
	r.reportProgress()
	r.reportProgress()
	assert.Equal(t, [][]string{{"set-progress", "0.50", "--label", "1/2 tasks"}}, runner.recorded(),
		"an unchanged done/total pair must not be pushed again")

	require.NoError(t, os.WriteFile(r.planFile,
		[]byte("# plan\n\n### Task 1: one\n\n- [x] a\n\n### Task 2: two\n\n- [x] b\n"), 0o600))
	r.reportProgress()
	assert.Equal(t, [][]string{
		{"set-progress", "0.50", "--label", "1/2 tasks"},
		{"set-progress", "1.00", "--label", "2/2 tasks"},
	}, runner.recorded(), "a changed pair must be pushed")
}

func TestReporterStartPolls(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.interval = time.Millisecond
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n\n### Task 2: two\n\n- [ ] b\n")

	r.Start(t.Context())
	calls := runner.waitForCalls(t, 2)
	r.Stop()

	assert.Equal(t, []string{"workspace", "loading", "on", "--id", "loopai"}, calls[0], "start must show the spinner")
	assert.Equal(t, []string{"set-progress", "0.50", "--label", "1/2 tasks"}, calls[1])
}

func TestReporterStartReportsBeforeFirstTick(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner) // interval is an hour, so nothing here comes from a tick
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n\n### Task 2: two\n\n- [ ] b\n")

	r.Start(t.Context())
	defer r.Stop()

	assert.Equal(t, [][]string{
		{"workspace", "loading", "on", "--id", "loopai"},
		{"set-progress", "0.50", "--label", "1/2 tasks"},
	}, runner.recorded(), "the bar must be there from the start, not one poll interval later")
}

func TestReporterStartWithoutPlanFile(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.interval = time.Millisecond // a poller, if started, would tick many times below

	r.Start(t.Context())
	time.Sleep(20 * time.Millisecond)

	assert.Equal(t, [][]string{{"workspace", "loading", "on", "--id", "loopai"}}, runner.recorded(),
		"plan creation mode has nothing to poll, so only the spinner is reported")

	r.mu.Lock()
	pollDone := r.pollDone
	r.mu.Unlock()
	assert.Nil(t, pollDone, "no goroutine must be started when there is no plan file to poll")

	assert.NotPanics(t, r.Stop, "stop must handle a reporter that never started a poller")
}

func TestReporterStartSurvivesBrokenPlan(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.interval = time.Millisecond
	r.planFile = filepath.Join(t.TempDir(), "appears-later.md")

	r.Start(t.Context())
	runner.waitForCalls(t, 1) // only the spinner so far, the plan file does not exist yet

	require.NoError(t, os.WriteFile(r.planFile, []byte("# plan\n\n### Task 1: one\n\n- [x] a\n"), 0o600))
	calls := runner.waitForCalls(t, 2)
	r.Stop()

	assert.Equal(t, []string{"set-progress", "1.00", "--label", "1/1 tasks"}, calls[1],
		"the goroutine must survive failed ticks and report once the plan is readable")
}

func TestReporterStop(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.interval = time.Millisecond
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n")

	r.Start(t.Context())
	runner.waitForCalls(t, 2)

	r.Stop()
	afterFirst := runner.recorded()
	r.Stop()
	r.Stop()

	assert.Equal(t, afterFirst, runner.recorded(), "stop must be idempotent")
	require.GreaterOrEqual(t, len(afterFirst), 5)
	assert.Equal(t, [][]string{
		{"workspace", "loading", "off", "--id", "loopai"},
		{"clear-status", "loopai"},
		{"clear-progress"},
	}, afterFirst[len(afterFirst)-3:], "stop must clear every sidebar artifact")

	r.mu.Lock()
	done := r.pollDone
	r.mu.Unlock()
	select {
	case <-done:
	default:
		t.Fatal("stop must wait for the poll goroutine to exit")
	}
}

func TestReporterStopClearsSpinnerBeforeJoin(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.interval = time.Millisecond
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n")

	// observed from inside the loading-off call, so the ordering is pinned by the poll goroutine's
	// own state rather than by timing: a call still in flight there ignores the cancel and could
	// otherwise push the spinner clear past the interrupt handler's bounded cleanup
	pollAlive := make(chan bool, 1)
	runner.mu.Lock()
	runner.onCall = func(args []string) {
		if len(args) < 3 || args[0] != "workspace" || args[2] != "off" {
			return
		}
		r.mu.Lock()
		done := r.pollDone
		r.mu.Unlock()
		select {
		case <-done:
			pollAlive <- false
		default:
			pollAlive <- true
		}
	}
	runner.mu.Unlock()

	r.Start(t.Context())
	runner.waitForCalls(t, 2)
	r.Stop()

	require.Len(t, pollAlive, 1, "stop must clear the spinner")
	assert.True(t, <-pollAlive, "the spinner must be cleared before the poll goroutine is joined")
}

func TestReporterStopWithoutStart(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	assert.NotPanics(t, func() { r.Stop() })
	assert.Equal(t, [][]string{
		{"workspace", "loading", "off", "--id", "loopai"},
		{"clear-status", "loopai"},
		{"clear-progress"},
	}, runner.recorded(), "stop without start must still clear the sidebar")
}

func TestReporterStopConcurrent(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.interval = time.Millisecond
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n")

	r.Start(t.Context())
	runner.waitForCalls(t, 2)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { r.Stop() })
	}
	wg.Wait()

	calls := runner.recorded()
	assert.Equal(t, [][]string{
		{"workspace", "loading", "off", "--id", "loopai"},
		{"clear-status", "loopai"},
		{"clear-progress"},
	}, calls[len(calls)-3:], "concurrent stops must clear exactly once")
}

func TestReporterContextCancelStopsPolling(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.interval = time.Millisecond
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n")

	ctx, cancel := context.WithCancel(t.Context())
	r.Start(ctx)
	runner.waitForCalls(t, 2)

	cancel()

	r.mu.Lock()
	done := r.pollDone
	r.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceling the context must stop the poll goroutine without Stop")
	}

	before := len(runner.recorded())
	time.Sleep(20 * time.Millisecond)
	assert.Len(t, runner.recorded(), before, "a stopped goroutine must not tick again")
	assert.NotContains(t, runner.recorded(), []string{"clear-progress"}, "cancel alone must not clear the sidebar")
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

	t.Run("returns while a grandchild still lives", func(t *testing.T) {
		// stdout/stderr must not be wired to a pipe: the copy goroutine cmd.Wait joins only sees EOF
		// once every inheritor of the write end is gone, so a surviving grandchild would block the
		// call well past its context deadline and stall the execution goroutine calling it
		r := &execRunner{bin: "sh"}
		done := make(chan error, 1)
		go func() { done <- r.run(t.Context(), "-c", "sleep 10 &") }()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("run must not wait for a grandchild holding the inherited output")
		}
	})
}

// fakeCollector records the arguments it receives and returns canned values.
type fakeCollector struct {
	question    string
	options     []string
	planContent string

	answer   string
	action   string
	feedback string
	err      error
}

func (f *fakeCollector) AskQuestion(_ context.Context, question string, options []string) (string, error) {
	f.question, f.options = question, options
	return f.answer, f.err
}

func (f *fakeCollector) AskDraftReview(_ context.Context, question, planContent string) (action, feedback string, err error) {
	f.question, f.planContent = question, planContent
	return f.action, f.feedback, f.err
}

// compile-time checks that the mirrored interface stays compatible with processor.InputCollector
// in both directions: the real collector must be accepted by WrapInput, and what WrapInput returns
// must be accepted by processor's SetInputCollector — the second is the one cmd/loopai relies on.
var (
	_ inputCollector           = processor.InputCollector(nil)
	_ processor.InputCollector = (*notifyingCollector)(nil)
)

func TestReporterWrapInputNilReporter(t *testing.T) {
	var r *Reporter
	inner := &fakeCollector{answer: "yes"}

	wrapped := r.WrapInput(inner)
	assert.Same(t, inner, wrapped, "outside cmux the original collector must be returned as is")
}

func TestReporterWrapInputAskQuestion(t *testing.T) {
	t.Run("notifies and passes values through", func(t *testing.T) {
		runner := &fakeRunner{}
		inner := &fakeCollector{answer: "option b"}
		wrapped := testReporter(t, runner).WrapInput(inner)

		answer, err := wrapped.AskQuestion(t.Context(), "which storage?", []string{"a", "b"})
		require.NoError(t, err)
		assert.Equal(t, "option b", answer)
		assert.Equal(t, "which storage?", inner.question)
		assert.Equal(t, []string{"a", "b"}, inner.options)
		assert.Equal(t, [][]string{
			{"notify", "--title", "loopai", "--subtitle", "input needed", "--body", "which storage?"},
		}, runner.recorded())
	})

	t.Run("passes the inner error through unchanged", func(t *testing.T) {
		runner := &fakeRunner{}
		wantErr := errors.New("collector failed")
		inner := &fakeCollector{err: wantErr}
		wrapped := testReporter(t, runner).WrapInput(inner)

		answer, err := wrapped.AskQuestion(t.Context(), "which storage?", nil)
		assert.Same(t, wantErr, err, "the error must not be wrapped or replaced")
		assert.Empty(t, answer)
		assert.Len(t, runner.recorded(), 1, "the notification is sent before the call, not after")
	})

	t.Run("runner failure does not surface", func(t *testing.T) {
		runner := &fakeRunner{err: errors.New("cmux is gone")}
		inner := &fakeCollector{answer: "ok"}
		wrapped := testReporter(t, runner).WrapInput(inner)

		answer, err := wrapped.AskQuestion(t.Context(), "which storage?", nil)
		require.NoError(t, err)
		assert.Equal(t, "ok", answer)
	})
}

func TestReporterWrapInputAskDraftReview(t *testing.T) {
	t.Run("notifies and passes values through", func(t *testing.T) {
		runner := &fakeRunner{}
		inner := &fakeCollector{action: "accept", feedback: "looks good"}
		wrapped := testReporter(t, runner).WrapInput(inner)

		action, feedback, err := wrapped.AskDraftReview(t.Context(), "review the draft", "# plan\n")
		require.NoError(t, err)
		assert.Equal(t, "accept", action)
		assert.Equal(t, "looks good", feedback)
		assert.Equal(t, "review the draft", inner.question)
		assert.Equal(t, "# plan\n", inner.planContent)
		assert.Equal(t, [][]string{
			{"notify", "--title", "loopai", "--subtitle", "plan draft ready", "--body", "review the draft"},
		}, runner.recorded())
	})

	t.Run("passes the inner error through unchanged", func(t *testing.T) {
		runner := &fakeRunner{}
		wantErr := errors.New("editor failed")
		inner := &fakeCollector{err: wantErr}
		wrapped := testReporter(t, runner).WrapInput(inner)

		action, feedback, err := wrapped.AskDraftReview(t.Context(), "review the draft", "# plan\n")
		assert.Same(t, wantErr, err, "the error must not be wrapped or replaced")
		assert.Empty(t, action)
		assert.Empty(t, feedback)
	})

	t.Run("long question is truncated in the notification body", func(t *testing.T) {
		runner := &fakeRunner{}
		wrapped := testReporter(t, runner).WrapInput(&fakeCollector{})

		question := strings.Repeat("я", notifyBodyLimit+50)
		_, _, err := wrapped.AskDraftReview(t.Context(), question, "# plan\n")
		require.NoError(t, err)

		calls := runner.recorded()
		require.Len(t, calls, 1)
		assert.Equal(t, strings.Repeat("я", notifyBodyLimit), calls[0][len(calls[0])-1])
	})
}
