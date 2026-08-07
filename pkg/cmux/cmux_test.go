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
	printFormat   string
	printArgs     []any
	rawFormat     string
	rawArgs       []any
	sections      []status.Section
	aligned       string
	question      string
	options       []string
	answer        string
	draftAction   string
	draftFeedback string
}

func (l *fakeLogger) Print(format string, args ...any) {
	l.printFormat, l.printArgs = format, args
}
func (l *fakeLogger) PrintRaw(format string, args ...any) {
	l.rawFormat, l.rawArgs = format, args
}
func (l *fakeLogger) PrintAligned(text string) { l.aligned = text }
func (l *fakeLogger) LogQuestion(question string, options []string) {
	l.question, l.options = question, options
}
func (l *fakeLogger) LogAnswer(answer string) { l.answer = answer }
func (l *fakeLogger) LogDraftReview(action, feedback string) {
	l.draftAction, l.draftFeedback = action, feedback
}
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

			models := Models{Plan: "opus:high", Task: "sonnet:medium"}
			r := New("docs/plans/feature.md", models)
			if tt.wantNil {
				assert.Nil(t, r)
				return
			}
			require.NotNil(t, r)
			assert.Equal(t, "docs/plans/feature.md", r.planFile)
			assert.Equal(t, models, r.models)
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

	assert.Nil(t, New("", Models{}), "unset workspace env must disable the reporter")
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
			call: func(r *Reporter) { r.loadingOnContext(context.Background()) },
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

func TestReporterFinish(t *testing.T) {
	tests := []struct {
		name    string
		success bool
		detail  string
		want    []string
	}{
		{
			name:    "success with elapsed time",
			success: true,
			detail:  "2h24m",
			want:    []string{"set-status", "loopai", "done in 2h24m", "--icon", "bolt", "--color", "#34c759", "--priority", "90"},
		},
		{
			name:   "failure with short detail",
			detail: "processor exited",
			want:   []string{"set-status", "loopai", "failed · processor exited", "--icon", "exclamationmark.triangle", "--color", "#ff3b30", "--priority", "90"},
		},
		{
			name:   "failure omits long detail",
			detail: strings.Repeat("x", failureDetailLimit+1),
			want:   []string{"set-status", "loopai", "failed", "--icon", "exclamationmark.triangle", "--color", "#ff3b30", "--priority", "90"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{}
			r := testReporter(t, runner)

			r.Finish(tt.success, tt.detail)

			assert.Equal(t, [][]string{tt.want}, runner.recorded())
			assert.True(t, r.finished)
		})
	}
}

func TestReporterStopAfterFinishPreservesPill(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	r.Finish(true, "12s")
	r.Stop()

	assert.Equal(t, [][]string{
		{"set-status", "loopai", "done in 12s", "--icon", "bolt", "--color", "#34c759", "--priority", "90"},
		{"workspace", "loading", "off", "--id", "loopai"},
		{"clear-progress"},
	}, runner.recorded())
}

func TestReporterFinishIsTerminalForStatusUpdates(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	r.Finish(true, "12s")
	r.OnPhase(status.PhaseTask, status.PhaseReview)
	r.OnLimitWait("10m")
	r.OnLimitRecovery("account switched")
	r.OnSection(status.NewInternalReviewSection(2, ": critical/major"))

	assert.Equal(t, [][]string{{
		"set-status", "loopai", "done in 12s", "--icon", "bolt", "--color", "#34c759", "--priority", "90",
	}}, runner.recorded(), "late callbacks must not overwrite the final pill")
}

func TestReporterFinishAfterStopNoOp(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.Stop()
	before := runner.recorded()

	r.Finish(false, "too late")

	assert.Equal(t, before, runner.recorded())
	assert.False(t, r.finished)
}

func TestReporterFinishConcurrentWithStop(t *testing.T) {
	for range 100 {
		runner := &fakeRunner{}
		r := testReporter(t, runner)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r.Finish(true, "1s")
		}()
		go func() {
			defer wg.Done()
			<-start
			r.Stop()
		}()
		close(start)
		wg.Wait()

		lastSet, lastClear := -1, -1
		for idx, call := range runner.recorded() {
			if len(call) > 0 && call[0] == "set-status" {
				lastSet = idx
			}
			if len(call) > 0 && call[0] == "clear-status" {
				lastClear = idx
			}
		}
		if lastSet >= 0 {
			assert.Less(t, lastClear, lastSet, "a completed pill must not be cleared after Finish wins the race")
		} else {
			assert.GreaterOrEqual(t, lastClear, 0, "Stop-first must clear the unfinished pill")
		}
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
		{name: "limit wait", phase: status.PhaseLimitWait, want: phaseStyle{text: "rate limited", icon: "clock.arrow.circlepath", color: "#ef4444"}},
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
		Plan:           "haiku:low",
		Task:           "gpt-5.6:high",
		Review:         "gpt-5.6:medium",
		ExternalReview: "opus:xhigh",
	}

	r.OnPhase("", status.PhaseTask)
	r.OnPhase(status.PhaseTask, status.PhaseReview)
	r.OnPhase(status.PhaseReview, status.PhaseExternalReview)
	r.OnPhase(status.PhaseExternalReview, status.PhaseExternalEval)
	r.OnPhase(status.PhaseExternalEval, status.PhasePlan)

	assert.Equal(t, [][]string{
		{"set-status", "loopai", "task (gpt-5.6:high)", "--icon", "hammer", "--color", "#22c55e", "--priority", "90"},
		{"set-status", "loopai", "review (gpt-5.6:medium)", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
		{"set-status", "loopai", "external review (opus:xhigh)", "--icon", "person.2", "--color", "#a855f7", "--priority", "90"},
		{"set-status", "loopai", "evaluating findings (gpt-5.6:medium)", "--icon", "checkmark.seal", "--color", "#a855f7", "--priority", "90"},
		{"set-status", "loopai", "planning (haiku:low)", "--icon", "list.bullet.clipboard", "--color", "#3b82f6", "--priority", "90"},
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
		{"set-status", "loopai", "review (gpt-5.6:medium)", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
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

func TestReporterWrapLoggerForwardsAllMethods(t *testing.T) {
	inner := &fakeLogger{}
	wrapped := testReporter(t, &fakeRunner{}).WrapLogger(inner)

	wrapped.Print("value %d", 7)
	wrapped.PrintRaw("raw %s", "text")
	wrapped.PrintAligned("aligned")
	wrapped.LogQuestion("continue?", []string{"yes", "no"})
	wrapped.LogAnswer("yes")
	wrapped.LogDraftReview("accept", "looks good")

	assert.Equal(t, "value %d", inner.printFormat)
	assert.Equal(t, []any{7}, inner.printArgs)
	assert.Equal(t, "raw %s", inner.rawFormat)
	assert.Equal(t, []any{"text"}, inner.rawArgs)
	assert.Equal(t, "aligned", inner.aligned)
	assert.Equal(t, "continue?", inner.question)
	assert.Equal(t, []string{"yes", "no"}, inner.options)
	assert.Equal(t, "yes", inner.answer)
	assert.Equal(t, "accept", inner.draftAction)
	assert.Equal(t, "looks good", inner.draftFeedback)
	assert.Equal(t, "progress.txt", wrapped.Path())
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

func TestReporterLogLimitWaitUpdatesCmux(t *testing.T) {
	runner := &fakeRunner{}
	inner := &fakeLogger{}
	wrapped := testReporter(t, runner).WrapLogger(inner)
	logger, ok := wrapped.(interface {
		LogLimitWait(pattern, tool, waitLabel string)
	})
	require.True(t, ok)

	logger.LogLimitWait("You've hit your session limit", "claude", "10m")

	assert.Equal(t, "rate limit detected: %q in %s output, waiting %s before retry...", inner.printFormat)
	assert.Equal(t, []any{"You've hit your session limit", "claude", "10m"}, inner.printArgs)
	assert.Equal(t, [][]string{{
		"set-status", "loopai", "rate limited · retry in 10m",
		"--icon", "clock.arrow.circlepath", "--color", "#ef4444", "--priority", "90",
	}}, runner.recorded())
}

func TestReporterLogLimitRecoveryUpdatesCmux(t *testing.T) {
	runner := &fakeRunner{}
	inner := &fakeLogger{}
	wrapper := testReporter(t, runner).WrapLogger(inner)
	logger, ok := wrapper.(interface {
		LogLimitRecovery(statusText, message string)
	})
	require.True(t, ok)

	logger.LogLimitRecovery("account switched · retry in 35s", "switched slot 1 to slot 2")

	assert.Equal(t, "%s", inner.printFormat)
	assert.Equal(t, []any{"switched slot 1 to slot 2"}, inner.printArgs)
	assert.Equal(t, [][]string{{
		"set-status", "loopai", "account switched · retry in 35s",
		"--icon", "clock.arrow.circlepath", "--color", "#ef4444", "--priority", "90",
	}}, runner.recorded())
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

func TestReporterOnSectionAfterStop(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	r.Stop()
	r.OnSection(status.NewInternalReviewSection(2, ": critical/major"))

	assert.Equal(t, [][]string{
		{"workspace", "loading", "off", "--id", "loopai"},
		{"clear-status", "loopai"},
		{"clear-progress"},
	}, runner.recorded(), "a review section after stop must not re-add the pill")
}

func TestReporterStopWaitsForSectionUpdateInFlight(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)

	inFlight, release := make(chan struct{}), make(chan struct{})
	runner.onCall = func(args []string) {
		if args[0] == "set-status" {
			close(inFlight)
			<-release
		}
	}

	go r.OnSection(status.NewInternalReviewSection(2, ": critical/major"))
	<-inFlight

	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()

	runner.waitForCalls(t, 2)
	assert.NotContains(t, runner.recorded(), []string{"clear-status", "loopai"})

	close(release)
	<-stopped

	assert.Equal(t, [][]string{
		{"set-status", "loopai", "review · iteration 2", "--icon", "magnifyingglass", "--color", "#06b6d4", "--priority", "90"},
		{"workspace", "loading", "off", "--id", "loopai"},
		{"clear-status", "loopai"},
		{"clear-progress"},
	}, runner.recorded())
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
		{name: "loading on", call: func() { r.loadingOnContext(context.Background()) }},
		{name: "loading off", call: func() { r.loadingOff() }},
		{name: "status", call: func() { r.setStatus("task", "hammer", "#22c55e") }},
		{name: "clear status", call: func() { r.clearStatus() }},
		{name: "progress", call: func() { r.setProgress(0.5, "tasks") }},
		{name: "clear progress", call: func() { r.clearProgress() }},
		{name: "notify", call: func() { r.Notify("done", "all tasks complete") }},
		{name: "finish", call: func() { r.Finish(true, "1s") }},
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
	calls := runner.waitForCalls(t, 3)
	r.Stop()

	assert.Equal(t, []string{"clear-status", "loopai"}, calls[0], "start must remove a stale final pill")
	assert.Equal(t, []string{"workspace", "loading", "on", "--id", "loopai"}, calls[1], "start must show the spinner")
	assert.Equal(t, []string{"set-progress", "0.50", "--label", "1/2 tasks"}, calls[2])
}

func TestReporterStartReportsBeforeFirstTick(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner) // interval is an hour, so nothing here comes from a tick
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n\n### Task 2: two\n\n- [ ] b\n")

	r.Start(t.Context())
	defer r.Stop()

	assert.Equal(t, [][]string{
		{"clear-status", "loopai"},
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

	assert.Equal(t, [][]string{
		{"clear-status", "loopai"},
		{"workspace", "loading", "on", "--id", "loopai"},
	}, runner.recorded(), "plan creation mode clears a stale pill and reports only the spinner")

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
	runner.waitForCalls(t, 2) // stale-pill cleanup and spinner only; the plan file does not exist yet

	require.NoError(t, os.WriteFile(r.planFile, []byte("# plan\n\n### Task 1: one\n\n- [x] a\n"), 0o600))
	calls := runner.waitForCalls(t, 3)
	r.Stop()

	assert.Equal(t, []string{"set-progress", "1.00", "--label", "1/1 tasks"}, calls[2],
		"the goroutine must survive failed ticks and report once the plan is readable")
}

func TestReporterStop(t *testing.T) {
	runner := &fakeRunner{}
	r := testReporter(t, runner)
	r.interval = time.Millisecond
	r.planFile = writePlan(t, "# plan\n\n### Task 1: one\n\n- [x] a\n")

	r.Start(t.Context())
	runner.waitForCalls(t, 3)

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

func TestReporterStopClearsSpinnerWhileStoppingPoll(t *testing.T) {
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
	runner.waitForCalls(t, 3)
	r.Stop()

	require.Len(t, pollAlive, 1, "stop must clear the spinner")
	<-pollAlive // either state is valid; loading-off must run without waiting on a stuck poll command
}

func TestReporterStopCancelsStartBeforeCleanup(t *testing.T) {
	runner := &fakeRunner{block: time.Hour}
	r := testReporter(t, runner)
	r.timeout = 50 * time.Millisecond

	startReturned := make(chan struct{})
	go func() {
		r.Start(t.Context())
		close(startReturned)
	}()
	runner.waitForCalls(t, 1)

	r.Stop()
	select {
	case <-startReturned:
	case <-time.After(time.Second):
		t.Fatal("Stop must cancel and join synchronous Start setup")
	}

	calls := runner.recorded()
	require.GreaterOrEqual(t, len(calls), 4)
	assert.Equal(t, []string{"clear-status", "loopai"}, calls[0])
	assert.Equal(t, []string{"workspace", "loading", "off", "--id", "loopai"}, calls[1])
	assert.NotContains(t, calls[2:], []string{"workspace", "loading", "on", "--id", "loopai"},
		"Start must not recreate the spinner after Stop begins")
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
	runner.waitForCalls(t, 3)

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
	runner.waitForCalls(t, 3)

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

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain word", in: "loopai", want: `'loopai'`},
		{name: "empty string stays an argument", in: "", want: `''`},
		{name: "spaces", in: "docs/plans/my plan.md", want: `'docs/plans/my plan.md'`},
		{name: "single quote", in: "it's", want: `'it'\''s'`},
		{name: "only a single quote", in: "'", want: `''\'''`},
		{name: "double quotes and backslash", in: `a"b\c`, want: `'a"b\c'`},
		{name: "shell metacharacters stay literal", in: "$HOME; rm -rf *", want: `'$HOME; rm -rf *'`},
		{name: "unicode", in: "план ветка", want: `'план ветка'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shellQuote(tt.in))
		})
	}
}

func TestSpawnWorkspace(t *testing.T) {
	binDir := t.TempDir()
	writeFakeBin(t, binDir, binName)

	t.Run("no workspace env", func(t *testing.T) {
		t.Setenv("PATH", binDir)
		t.Setenv(workspaceEnv, "")

		err := SpawnWorkspace("feature", "/tmp", []string{"loopai", "plan.md"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotInCmux)
	})

	t.Run("blank workspace env", func(t *testing.T) {
		t.Setenv("PATH", binDir)
		t.Setenv(workspaceEnv, " \t ")

		assert.ErrorIs(t, SpawnWorkspace("feature", "/tmp", []string{"loopai"}), ErrNotInCmux)
	})

	t.Run("no binary in path", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		t.Setenv(workspaceEnv, "ws-1")

		err := SpawnWorkspace("feature", "/tmp", []string{"loopai"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotInCmux)
	})

	t.Run("availability detected, request reaches the binary", func(t *testing.T) {
		t.Setenv("PATH", binDir)
		t.Setenv(workspaceEnv, "ws-1")

		// only the availability verdict is asserted here, the argv is covered with a fake runner in
		// TestSpawnWorkspaceCommand.
		err := SpawnWorkspace("feature", "/tmp", []string{"loopai", "plan.md"})
		assert.NotErrorIs(t, err, ErrNotInCmux, "env and binary are both present")
	})
}

func TestSpawnWorkspaceCommand(t *testing.T) {
	t.Run("exact argv", func(t *testing.T) {
		runner := &fakeRunner{}
		require.NoError(t, spawnWorkspace(runner, spawnTimeout, "my-feature", "/work/repo", []string{"/bin/loopai", "docs/plans/a b.md", "--codex"}))

		calls := runner.recorded()
		require.Len(t, calls, 1)
		assert.Equal(t, []string{
			"new-workspace",
			"--name", "my-feature",
			"--cwd", "/work/repo",
			"--focus", "true",
			"--command", `'/bin/loopai' 'docs/plans/a b.md' '--codex'`,
		}, calls[0])
	})

	t.Run("quotes in arguments are escaped", func(t *testing.T) {
		runner := &fakeRunner{}
		require.NoError(t, spawnWorkspace(runner, spawnTimeout, "it's", "/work", []string{"loopai", "it's.md"}))

		calls := runner.recorded()
		require.Len(t, calls, 1)
		assert.Equal(t, `'loopai' 'it'\''s.md'`, calls[0][len(calls[0])-1])
		assert.Equal(t, "it's", calls[0][2], "the name is passed as a plain argument, not shell-quoted")
	})

	t.Run("runner failure is propagated", func(t *testing.T) {
		wantErr := errors.New("cmux refused")
		runner := &fakeRunner{err: wantErr}

		err := spawnWorkspace(runner, spawnTimeout, "feature", "/work", []string{"loopai"})
		require.Error(t, err)
		require.ErrorIs(t, err, wantErr, "the caller decides on the fallback, so the cause must survive")
		require.NotErrorIs(t, err, ErrNotInCmux, "a CLI failure is not an availability problem")
		assert.Contains(t, err.Error(), `create cmux workspace "feature"`)
	})

	t.Run("empty argv", func(t *testing.T) {
		runner := &fakeRunner{}

		require.Error(t, spawnWorkspace(runner, spawnTimeout, "feature", "/work", nil))
		assert.Empty(t, runner.recorded(), "nothing must be sent to cmux without a command")
	})

	t.Run("call is bounded by the timeout", func(t *testing.T) {
		runner := &fakeRunner{block: time.Minute}

		start := time.Now()
		err := spawnWorkspace(runner, 50*time.Millisecond, "feature", "/work", []string{"loopai"})
		require.Error(t, err)
		assert.Less(t, time.Since(start), 30*time.Second, "a hanging cmux call must not stall the hand-off")
	})

	t.Run("timeout is reported as ambiguous", func(t *testing.T) {
		runner := &fakeRunner{block: time.Minute}

		// killing the local client says nothing about the request cmux may already have acted on,
		// so the caller must be able to tell this apart from a refusal it can fall back from.
		err := spawnWorkspace(runner, 50*time.Millisecond, "feature", "/work", []string{"loopai"})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrSpawnAmbiguous)
		require.ErrorIs(t, err, context.DeadlineExceeded, "the cause survives alongside the sentinel")
	})

	t.Run("clean refusal is not ambiguous", func(t *testing.T) {
		runner := &fakeRunner{err: errors.New("cmux refused")}

		err := spawnWorkspace(runner, spawnTimeout, "feature", "/work", []string{"loopai"})
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrSpawnAmbiguous, "a rejected request created nothing to duplicate")
	})

	t.Run("spawn timeout outlasts the status timeout", func(t *testing.T) {
		// creating a workspace starts a terminal, and a premature kill leaves the caller unable to tell
		// a failed hand-off from a created workspace it would then duplicate with a local run.
		assert.Greater(t, spawnTimeout, execTimeout)
	})
}
