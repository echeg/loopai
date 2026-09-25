package t3

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/status"
)

type fakeDispatcher struct {
	mu        sync.Mutex
	shell     Shell
	shellErr  error
	dispErr   error
	gate      chan struct{} // when set, each Dispatch waits for one receive
	entered   chan struct{} // when set, each Dispatch announces itself before waiting on gate
	commands  []Command
	shellHits int
}

func (f *fakeDispatcher) Dispatch(_ context.Context, cmd Command) (int64, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dispErr != nil {
		return 0, f.dispErr
	}
	f.commands = append(f.commands, cmd)
	return int64(len(f.commands)), nil
}

func (f *fakeDispatcher) Shell(context.Context) (Shell, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shellHits++
	return f.shell, f.shellErr
}

func (f *fakeDispatcher) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, cmd := range f.commands {
		switch c := cmd.(type) {
		case *ThreadCreate:
			out = append(out, "create:"+c.Title)
		case *ThreadTitleUpdate:
			out = append(out, c.Title)
		}
	}
	return out
}

func projectShell(root string) Shell {
	return Shell{Projects: []Project{{ID: "p1", WorkspaceRoot: root}}}
}

func writePlan(t *testing.T, tasks int) string {
	t.Helper()
	var content strings.Builder
	content.WriteString("# Plan\n\n## Implementation Steps\n\n")
	for i := 1; i <= tasks; i++ {
		fmt.Fprintf(&content, "### Task %d: step\n- [ ] do it\n\n", i)
	}
	path := filepath.Join(t.TempDir(), "20260925-t3-demo.md")
	require.NoError(t, os.WriteFile(path, []byte(content.String()), 0o600))
	return path
}

func TestNewUnavailable(t *testing.T) {
	r, err := New(Options{}, envMap(nil))
	require.ErrorIs(t, err, ErrNoToken)
	assert.Nil(t, r)
}

func TestNewReadsThreadIDFromEnv(t *testing.T) {
	r, err := New(Options{}, envMap(map[string]string{
		EnvToken: "tok", EnvURL: "http://127.0.0.1:1", EnvThreadID: " th-env ",
	}))
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, "th-env", r.opts.ThreadID)
	close(r.quit) // stop the worker without publishing
	<-r.done
}

func TestReporterNilReceiver(t *testing.T) {
	var r *Reporter
	r.OnPhase("", status.PhaseTask)
	r.OnSection(status.NewTaskIterationSection(1))
	r.Finish(true)
	r.Stop()
	assert.True(t, r.WithInputWait(func() bool { return true }))
	logger := &recordingLogger{}
	assert.Same(t, logger, r.WrapLogger(logger))
}

func TestReporterCreatesThreadThenUpdatesTitles(t *testing.T) {
	api := &fakeDispatcher{shell: projectShell(`C:\Repo`)}
	planFile := writePlan(t, 3)
	r := NewWithDispatcher(api, Options{
		PlanFile: planFile, Executor: config.ExecutorCodex, Model: "gpt-5:high",
		RepoRoot: "c:/repo/", Branch: "t3-demo", WorktreePath: `C:\Repo`,
	})

	r.OnPhase("", status.PhaseTask)
	waitFor(t, api, 1)
	r.OnSection(status.NewTaskIterationSection(2))
	waitFor(t, api, 2)
	r.OnPhase(status.PhaseTask, status.PhaseReview)
	waitFor(t, api, 3)
	r.Finish(true)
	r.Stop()

	assert.Equal(t, []string{
		"create:t3-demo · task", "t3-demo · task 2/3", "t3-demo · review", "t3-demo · done",
	}, api.titles())

	create, ok := api.commands[0].(*ThreadCreate)
	require.True(t, ok)
	assert.Equal(t, "p1", create.ProjectID)
	assert.Equal(t, ModelSelection{InstanceID: InstanceCodex, Model: "gpt-5"}, create.ModelSelection)
	require.NotNil(t, create.Branch)
	assert.Equal(t, "t3-demo", *create.Branch)
	require.NotNil(t, create.WorktreePath)
	for _, cmd := range api.commands[1:] {
		update, ok := cmd.(*ThreadTitleUpdate)
		require.True(t, ok)
		assert.Equal(t, create.ThreadID, update.ThreadID)
	}
}

func TestReporterWorktreeRunRecordsNullPath(t *testing.T) {
	api := &fakeDispatcher{shell: projectShell("/repo")}
	r := NewWithDispatcher(api, Options{RepoRoot: "/repo", Branch: "feat"})
	r.OnPhase("", status.PhaseReview)
	waitFor(t, api, 1)
	r.Stop()
	create, ok := api.commands[0].(*ThreadCreate)
	require.True(t, ok)
	assert.Nil(t, create.WorktreePath)
	assert.Equal(t, ModelSelection{InstanceID: InstanceClaude, Model: "default"}, create.ModelSelection)
	assert.Equal(t, []string{"create:loopai · review", "loopai · stopped"}, api.titles())
}

func TestReporterExistingThread(t *testing.T) {
	api := &fakeDispatcher{}
	r := NewWithDispatcher(api, Options{ThreadID: "th-1"})
	r.OnPhase("", status.PhaseExternalEval)
	waitFor(t, api, 1)
	r.Finish(false)
	r.Stop()
	assert.Equal(t, 0, api.shellHits)
	assert.Equal(t, []string{"loopai · external eval", "loopai · failed"}, api.titles())
	for _, cmd := range api.commands {
		assert.Equal(t, "th-1", cmd.(*ThreadTitleUpdate).ThreadID)
	}
}

func TestReporterSkipsUnchangedAndCoalesces(t *testing.T) {
	gate, entered := make(chan struct{}), make(chan struct{}, 4)
	api := &fakeDispatcher{gate: gate, entered: entered}
	r := NewWithDispatcher(api, Options{ThreadID: "th"})

	r.OnPhase("", status.PhaseTask)
	<-entered // the worker is now blocked sending the first title

	// a burst while the worker is busy collapses to the latest title
	r.OnSection(status.NewInternalReviewSection(1, "review"))
	r.OnSection(status.NewInternalReviewSection(2, "review"))
	r.OnSection(status.NewInternalReviewSection(3, "review"))
	gate <- struct{}{} // first title goes out
	<-entered          // the worker picked up the coalesced title
	gate <- struct{}{}
	waitFor(t, api, 2)

	// the same title is not re-sent; the final title is
	r.OnSection(status.NewInternalReviewSection(3, "review"))
	r.Finish(true)
	<-entered
	gate <- struct{}{}
	r.Stop()

	assert.Equal(t, []string{"loopai · task", "loopai · review · iteration 3", "loopai · done"}, api.titles())
}

func TestReporterDisablesAfterError(t *testing.T) {
	tests := []struct {
		name    string
		api     *fakeDispatcher
		opts    Options
		wantErr string
	}{
		{"shell error", &fakeDispatcher{shellErr: errors.New("refused")}, Options{RepoRoot: "/r"}, "refused"},
		{"no project", &fakeDispatcher{shell: projectShell("/other")}, Options{RepoRoot: "/r"}, "no T3 Code project for /r"},
		{"create error", &fakeDispatcher{shell: projectShell("/r"), dispErr: &APIError{Status: 401}}, Options{RepoRoot: "/r"}, "create thread"},
		{"update error", &fakeDispatcher{dispErr: errors.New("boom")}, Options{ThreadID: "th"}, "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var warnings []string
			tc.opts.Warn = func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				warnings = append(warnings, fmt.Sprintf(format, args...))
			}
			r := NewWithDispatcher(tc.api, tc.opts)
			r.OnPhase("", status.PhaseTask)
			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(warnings) == 1
			}, time.Second, 5*time.Millisecond)
			// later updates neither block nor warn again
			r.OnSection(status.NewTaskIterationSection(2))
			r.Finish(true)
			r.Stop()
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, warnings, 1)
			assert.Contains(t, warnings[0], "t3 status disabled")
			assert.Contains(t, warnings[0], tc.wantErr)
		})
	}
}

func TestReporterStopIsBounded(t *testing.T) {
	api := &fakeDispatcher{gate: make(chan struct{})} // never released
	r := NewWithDispatcher(api, Options{ThreadID: "th"})
	r.OnPhase("", status.PhaseTask)
	start := time.Now()
	r.Stop()
	assert.Less(t, time.Since(start), stopTimeout+time.Second)
	r.Stop() // idempotent
}

func TestReporterInputWaitAndLimit(t *testing.T) {
	api := &fakeDispatcher{}
	r := NewWithDispatcher(api, Options{ThreadID: "th"})
	r.OnPhase("", status.PhaseTask)
	waitFor(t, api, 1)
	assert.True(t, r.WithInputWait(func() bool {
		waitFor(t, api, 2)
		return true
	}))
	waitFor(t, api, 3)
	r.OnPhase(status.PhaseTask, status.PhaseLimitWait)
	waitFor(t, api, 4)
	r.OnPhase(status.PhaseLimitWait, status.PhaseTask)
	waitFor(t, api, 5)
	r.Stop()
	assert.Equal(t, []string{
		"loopai · task", "loopai · waiting for input", "loopai · task", "loopai · waiting for limit",
		"loopai · task", "loopai · stopped",
	}, api.titles())
}

func TestReporterFrozenAfterFinish(t *testing.T) {
	api := &fakeDispatcher{}
	r := NewWithDispatcher(api, Options{ThreadID: "th"})
	r.Finish(true)
	r.OnPhase("", status.PhaseTask)
	r.OnSection(status.NewTaskIterationSection(1))
	assert.True(t, r.WithInputWait(func() bool { return true }))
	r.Finish(false)
	r.Stop()
	assert.Equal(t, []string{"loopai · done"}, api.titles())
}

func TestReporterWrapLogger(t *testing.T) {
	api := &fakeDispatcher{}
	r := NewWithDispatcher(api, Options{ThreadID: "th"})
	inner := &recordingLogger{}
	logger := r.WrapLogger(inner)
	logger.PrintSection(status.NewExternalReviewIterationSection("codex", 2))
	logger.Print("ignored")
	waitFor(t, api, 1)
	r.Stop()
	assert.Equal(t, 1, inner.sections)
	assert.Equal(t, "loopai · external review · iteration 2", api.titles()[0])
}

func TestRunName(t *testing.T) {
	assert.Equal(t, "loopai", runName(""))
	assert.Equal(t, "t3-code-integration", runName("docs/plans/20260925-t3-code-integration.md"))
	assert.Equal(t, "fix-bug", runName(`C:\plans\fix-bug.md`))
	assert.Equal(t, "2026", runName("2026.md"))
}

func TestPhaseLabels(t *testing.T) {
	r := &Reporter{name: "p"}
	tests := []struct {
		s    state
		want string
	}{
		{state{phase: status.PhaseTask}, "p · task"},
		{state{phase: status.PhaseTask, task: 2}, "p · task 2"},
		{state{phase: status.PhaseTask, task: 2, total: 4}, "p · task 2/4"},
		{state{phase: status.PhaseReview, iteration: 1}, "p · review · iteration 1"},
		{state{phase: status.PhaseExternalReview}, "p · external review"},
		{state{phase: status.PhasePlan, iteration: 2}, "p · plan · iteration 2"},
		{state{phase: status.PhaseFinalize}, "p · finalize"},
		{state{phase: status.PhaseReport}, "p · report"},
		{state{phase: status.Phase("unknown")}, ""},
		{state{phase: status.PhaseTask, waiting: waitingInput}, "p · waiting for input"},
		{state{final: finalFailed, waiting: waitingInput}, "p · failed"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, r.titleFor(tc.s))
	}
}

func TestPlanTaskTotalMissingFile(t *testing.T) {
	assert.Equal(t, 0, planTaskTotal(filepath.Join(t.TempDir(), "missing.md")))
}

func waitFor(t *testing.T, api *fakeDispatcher, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.commands) >= n
	}, 2*time.Second, 2*time.Millisecond)
}

type recordingLogger struct {
	sections int
}

func (l *recordingLogger) Print(string, ...any)          { _ = l }
func (l *recordingLogger) PrintRaw(string, ...any)       { _ = l }
func (l *recordingLogger) PrintSection(status.Section)   { l.sections++ }
func (l *recordingLogger) PrintAligned(string)           { _ = l }
func (l *recordingLogger) LogQuestion(string, []string)  { _ = l }
func (l *recordingLogger) LogAnswer(string)              { _ = l }
func (l *recordingLogger) LogDraftReview(string, string) { _ = l }
func (l *recordingLogger) Path() string                  { return "" }
