package t3

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	pinErr    error         // returned for thread.pin only, so a wrongly sent unpin is still recorded
	titleErr  error         // returned for thread.meta.update only
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
	if pin, ok := cmd.(*ThreadPinToggle); ok && pin.Type == "thread.pin" && f.pinErr != nil {
		return 0, f.pinErr
	}
	if _, ok := cmd.(*ThreadTitleUpdate); ok && f.titleErr != nil {
		return 0, f.titleErr
	}
	f.commands = append(f.commands, cmd)
	if update, ok := cmd.(*ThreadTitleUpdate); ok {
		for i := range f.shell.Threads {
			if f.shell.Threads[i].ID == update.ThreadID {
				f.shell.Threads[i].Title = update.Title
			}
		}
	}
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

// pinOps lists the pin and unpin commands in dispatch order as "<type>:<thread id>".
func (f *fakeDispatcher) pinOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, cmd := range f.commands {
		if c, ok := cmd.(*ThreadPinToggle); ok {
			out = append(out, c.Type+":"+c.ThreadID)
		}
	}
	return out
}

// titleUpdates returns the title-only meta updates in dispatch order.
func (f *fakeDispatcher) titleUpdates() []*ThreadTitleUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ThreadTitleUpdate
	for _, cmd := range f.commands {
		if c, ok := cmd.(*ThreadTitleUpdate); ok {
			out = append(out, c)
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

func TestNewTitleOnlyRequiresExplicitThreadID(t *testing.T) {
	for _, id := range []string{"", " \t"} {
		r, err := New(Options{TitleOnly: true, ThreadID: id}, func(string) string {
			t.Fatal("missing explicit thread ID must fail before inspecting credentials or the environment")
			return "environment-thread"
		})
		require.ErrorContains(t, err, "requires an explicit thread ID")
		assert.Nil(t, r)
	}

	r, err := New(Options{TitleOnly: true, ThreadID: " th-explicit "}, envMap(map[string]string{
		EnvToken: "tok", EnvURL: "http://127.0.0.1:1", EnvThreadID: "th-env",
	}))
	require.NoError(t, err)
	assert.Equal(t, "th-explicit", r.opts.ThreadID)
	close(r.quit) // do not publish to the test endpoint
	<-r.done
}

func TestReporterTitleOnlyProgress(t *testing.T) {
	const title = "[Draft] Устройство проекта"
	api := &fakeDispatcher{shell: Shell{Threads: []Thread{
		{ID: "other", Title: "Other thread"},
		{ID: "current", Title: title, PinnedAt: new("already pinned"), Branch: new("main"),
			WorktreePath: new("/original"), Session: &ThreadSession{Status: "running"}},
	}}}
	r := NewWithDispatcher(api, Options{
		TitleOnly: true, ThreadID: "current", PlanFile: writePlan(t, 3),
		RepoRoot: "/different", Branch: "different", WorktreePath: "/different", Model: "different",
	})
	r.OnPhase("", status.PhaseTask)
	waitFor(t, api, 1)
	r.OnSection(status.NewTaskIterationSection(2))
	waitFor(t, api, 2)
	assert.True(t, r.WithInputWait(func() bool {
		waitFor(t, api, 3)
		return true
	}))
	waitFor(t, api, 4)
	r.OnPhase(status.PhaseTask, status.PhaseLimitWait)
	waitFor(t, api, 5)
	r.OnPhase(status.PhaseLimitWait, status.PhaseTask)
	waitFor(t, api, 6)
	r.OnSection(status.NewInternalReviewSection(2, "review"))
	waitFor(t, api, 7)
	r.OnSection(status.NewExternalReviewIterationSection("codex", 3))
	waitFor(t, api, 8)
	r.OnPhase(status.PhaseExternalReview, status.PhaseExternalEval)
	waitFor(t, api, 9)
	r.OnPhase(status.PhaseExternalEval, status.PhaseFinalize)
	waitFor(t, api, 10)
	r.OnPhase(status.PhaseFinalize, status.PhaseReport)
	waitFor(t, api, 11)
	r.Finish(true)
	r.OnPhase(status.PhaseReport, status.PhaseTask) // the final outcome is frozen
	r.Stop()

	labels := []string{"task", "task 2/3", "waiting for input", "task 2/3", "waiting for limit", "task 2/3",
		"review · iteration 2", "external review · iteration 3", "external eval", "finalize", "report", "done"}
	want := make([]string, len(labels))
	for i, label := range labels {
		want[i] = "[loopai: " + label + "] " + title
	}
	assert.Equal(t, want, api.titles())
	assert.Equal(t, len(want), api.shellHits, "each changed title revalidates the target and rereads the user title")
	assertTitleOnlyCommands(t, api)
	assert.Equal(t, "Other thread", api.shell.Threads[0].Title)
	assert.Equal(t, "main", *api.shell.Threads[1].Branch)
	assert.Equal(t, "/original", *api.shell.Threads[1].WorktreePath)
	assert.Equal(t, "running", api.shell.Threads[1].Session.Status)
}

func TestReporterTitleOnlyPreservesNames(t *testing.T) {
	tests := []struct {
		name, title, base string
	}{
		{"repeat run", "[loopai: done] Original", "Original"},
		{"repeat with outcome", "[loopai: done · PR merged] Original", "Original"},
		{"interrupted run", "[loopai: review · iteration 2] Original", "Original"},
		{"arbitrary brackets", "[review] Original", "[review] Original"},
		{"unknown namespace value", "[loopai: special] Original", "[loopai: special] Original"},
		{"prefix in middle", "Original [loopai: task 2/3]", "Original [loopai: task 2/3]"},
		{"only one prefix", "[loopai: task] [loopai: review] Original", "[loopai: review] Original"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeDispatcher{shell: Shell{Threads: []Thread{{ID: "current", Title: tc.title}}}}
			r := NewWithDispatcher(api, Options{TitleOnly: true, ThreadID: "current"})
			r.OnPhase("", status.PhaseTask)
			waitFor(t, api, 1)
			r.Stop()
			assert.Equal(t, []string{"[loopai: task] " + tc.base, "[loopai: stopped] " + tc.base}, api.titles())
			assertTitleOnlyCommands(t, api)
		})
	}
}

func TestReporterTitleOnlyKeepsManualRename(t *testing.T) {
	for _, rename := range []string{"User renamed", "[loopai: task] User renamed"} {
		api := &fakeDispatcher{shell: Shell{Threads: []Thread{{ID: "current", Title: "Original"}}}}
		r := NewWithDispatcher(api, Options{TitleOnly: true, ThreadID: "current"})
		r.OnPhase("", status.PhaseTask)
		waitFor(t, api, 1)
		api.mu.Lock()
		api.shell.Threads[0].Title = rename
		api.mu.Unlock()
		r.OnPhase(status.PhaseTask, status.PhaseReview)
		waitFor(t, api, 2)
		r.Finish(false)
		r.Stop()
		assert.Equal(t, []string{
			"[loopai: task] Original", "[loopai: review] User renamed", "[loopai: failed] User renamed",
		}, api.titles())
		assertTitleOnlyCommands(t, api)
	}
}

func TestReporterTitleOnlyDisablesWithoutFallback(t *testing.T) {
	tests := []struct {
		name, threadID, wantErr string
		api                     *fakeDispatcher
	}{
		{"missing ID", "", "explicit thread ID", &fakeDispatcher{shell: projectShell("/repo")}},
		{"missing thread", "current", "not found", &fakeDispatcher{shell: Shell{
			Projects: []Project{{ID: "p", WorkspaceRoot: "/repo"}}, Threads: []Thread{{ID: "other", Title: "Other"}},
		}}},
		{"archived thread", "current", "archived", &fakeDispatcher{shell: Shell{Threads: []Thread{
			{ID: "current", Title: "Original", ArchivedAt: new("yesterday")},
		}}}},
		{"shell failure", "current", "disconnected", &fakeDispatcher{shellErr: errors.New("disconnected")}},
		{"dispatch failure", "current", "denied", &fakeDispatcher{
			shell: Shell{Threads: []Thread{{ID: "current", Title: "Original"}}}, dispErr: errors.New("denied"),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warnings := make(chan string, 8)
			r := NewWithDispatcher(tc.api, Options{TitleOnly: true, ThreadID: tc.threadID, RepoRoot: "/repo",
				Warn: func(format string, args ...any) { warnings <- fmt.Sprintf(format, args...) },
			})
			r.OnPhase("", status.PhaseTask)
			select {
			case warning := <-warnings:
				assert.Contains(t, warning, tc.wantErr)
			case <-time.After(2 * time.Second):
				t.Fatal("expected best-effort reporting warning")
			}
			r.OnSection(status.NewTaskIterationSection(2))
			r.Finish(true)
			r.Stop()
			assert.Empty(t, tc.api.commands, "no thread is created, pinned, or otherwise changed on invalid target or API failure")
			assert.Empty(t, warnings, "failure only warns once")
			if tc.threadID == "" {
				assert.Zero(t, tc.api.shellHits, "an explicit ID is required before any server call")
			}
		})
	}
}

func TestReporterTitleOnlyRevalidatesTarget(t *testing.T) {
	for _, archive := range []bool{false, true} {
		api := &fakeDispatcher{shell: Shell{Threads: []Thread{{ID: "current", Title: "Original"}}}}
		var warnings atomic.Int32
		r := NewWithDispatcher(api, Options{TitleOnly: true, ThreadID: "current",
			Warn: func(string, ...any) { warnings.Add(1) },
		})
		r.OnPhase("", status.PhaseTask)
		waitFor(t, api, 1)
		api.mu.Lock()
		if archive {
			api.shell.Threads[0].ArchivedAt = new("now")
		} else {
			api.shell.Threads = nil
		}
		api.mu.Unlock()
		r.OnPhase(status.PhaseTask, status.PhaseReview)
		require.Eventually(t, func() bool { return warnings.Load() == 1 }, time.Second, time.Millisecond)
		r.Finish(true)
		r.Stop()
		assert.Equal(t, []string{"[loopai: task] Original"}, api.titles())
		assert.EqualValues(t, 1, warnings.Load())
		assertTitleOnlyCommands(t, api)
	}
}

func TestReporterTitleOnlyFinishNote(t *testing.T) {
	api := &fakeDispatcher{shell: Shell{Threads: []Thread{{ID: "current", Title: "Original"}}}}
	r := NewWithDispatcher(api, Options{TitleOnly: true, ThreadID: "current"})
	r.SetFinishNote("finalize incomplete [retry]\nneeded")
	r.Finish(true)
	r.Stop()
	assert.Equal(t, []string{"[loopai: done · finalize incomplete (retry) needed] Original"}, api.titles())
	// The emitted prefix must remain replaceable on the next run.
	r = NewWithDispatcher(api, Options{TitleOnly: true, ThreadID: "current"})
	r.Stop()
	assert.Equal(t, "[loopai: stopped] Original", api.titles()[1])
	assertTitleOnlyCommands(t, api)
}

func assertTitleOnlyCommands(t *testing.T, api *fakeDispatcher) {
	t.Helper()
	for _, cmd := range api.commands {
		update, ok := cmd.(*ThreadTitleUpdate)
		require.True(t, ok, "title-only reporting may dispatch only a title update, got %T", cmd)
		assert.Equal(t, "thread.meta.update", update.Type)
		assert.Equal(t, "current", update.ThreadID)
	}
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
	for _, update := range api.titleUpdates() {
		assert.Equal(t, create.ThreadID, update.ThreadID)
	}
	assert.Equal(t, []string{"thread.pin:" + create.ThreadID, "thread.unpin:" + create.ThreadID}, api.pinOps())
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
	// Binding publishes the title before checking the pin. Wait for that check so Stop
	// cannot intentionally skip it while the worker is still completing the bind.
	require.Eventually(t, func() bool { return len(api.pinOps()) == 1 }, 2*time.Second, 2*time.Millisecond)
	r.Finish(false)
	r.Stop()
	assert.Equal(t, 1, api.shellHits, "only the pin check reads the shell for a bound thread")
	assert.Equal(t, []string{"loopai · external eval", "loopai · failed"}, api.titles())
	for _, update := range api.titleUpdates() {
		assert.Equal(t, "th-1", update.ThreadID)
	}
	assert.Equal(t, []string{"thread.pin:th-1", "thread.unpin:th-1"}, api.pinOps())
}

func TestReporterKeepsUserPin(t *testing.T) {
	pinnedAt := "2026-10-01T00:00:00Z"
	api := &fakeDispatcher{shell: Shell{Threads: []Thread{{ID: "th-1", PinnedAt: &pinnedAt}}}}
	r := NewWithDispatcher(api, Options{ThreadID: "th-1"})
	r.OnPhase("", status.PhaseTask)
	waitFor(t, api, 1)
	r.Finish(true)
	r.Stop()
	assert.Equal(t, []string{"loopai · task", "loopai · done"}, api.titles())
	assert.Empty(t, api.pinOps(), "a pin the user set is neither re-sent nor released")
}

func TestReporterReleasesPinAfterTitleFailure(t *testing.T) {
	api := &fakeDispatcher{}
	var warned atomic.Bool
	r := NewWithDispatcher(api, Options{ThreadID: "th", Warn: func(string, ...any) { warned.Store(true) }})
	r.OnPhase("", status.PhaseTask)
	require.Eventually(t, func() bool { return len(api.pinOps()) == 1 }, 2*time.Second, 2*time.Millisecond)

	api.mu.Lock()
	api.titleErr = errors.New("server gone")
	api.mu.Unlock()
	r.OnPhase(status.PhaseTask, status.PhaseReview)
	require.Eventually(t, warned.Load, 2*time.Second, 2*time.Millisecond, "the failed title disables reporting")
	r.Finish(true)
	r.Stop()

	assert.Equal(t, []string{"thread.pin:th", "thread.unpin:th"}, api.pinOps(),
		"the pin is released even after title reporting was disabled")
}

func TestReporterSkipsPinWhenStoppedDuringBind(t *testing.T) {
	gate, entered := make(chan struct{}), make(chan struct{}, 4)
	api := &fakeDispatcher{gate: gate, entered: entered}
	r := NewWithDispatcher(api, Options{ThreadID: "th"})
	r.OnPhase("", status.PhaseTask)
	<-entered // the binding title is in flight

	stopped := make(chan struct{})
	go func() {
		r.Stop()
		close(stopped)
	}()
	require.Eventually(t, func() bool {
		select {
		case <-r.quit:
			return true
		default:
			return false
		}
	}, 2*time.Second, 2*time.Millisecond)
	gate <- struct{}{} // the binding title completes after Stop
	<-entered          // the final title
	gate <- struct{}{}
	<-stopped

	assert.Equal(t, []string{"loopai · task", "loopai · stopped"}, api.titles())
	assert.Empty(t, api.pinOps(), "a run stopped during the bind takes no pin it may not have time to release")
}

func TestReporterPinFailureKeepsTitles(t *testing.T) {
	tests := []struct {
		name string
		api  *fakeDispatcher
	}{
		{"pin rejected", &fakeDispatcher{shell: projectShell("/r"), pinErr: &APIError{Status: 400}}},
		{"shell unavailable for bound thread", &fakeDispatcher{shellErr: errors.New("refused")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{RepoRoot: "/r"}
			if tc.api.shellErr != nil {
				opts = Options{ThreadID: "th"}
			}
			var warned atomic.Bool
			opts.Warn = func(string, ...any) { warned.Store(true) }
			r := NewWithDispatcher(tc.api, opts)
			r.OnPhase("", status.PhaseTask)
			waitFor(t, tc.api, 1)
			r.OnPhase(status.PhaseTask, status.PhaseReview)
			waitFor(t, tc.api, 2)
			r.Finish(true)
			r.Stop()
			assert.False(t, warned.Load())
			assert.Len(t, tc.api.titles(), 3)
			assert.Empty(t, tc.api.pinOps(), "no pin was taken, so none is released")
		})
	}
}

func TestReporterSkipsUnchangedAndCoalesces(t *testing.T) {
	gate, entered := make(chan struct{}), make(chan struct{}, 4)
	pinnedAt := "2026-10-01T00:00:00Z" // already pinned, so only titles pass through the gate
	api := &fakeDispatcher{gate: gate, entered: entered, shell: Shell{Threads: []Thread{{ID: "th", PinnedAt: &pinnedAt}}}}
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
	assert.Equal(t, "fix-bug", runName(filepath.FromSlash("C:/plans/fix-bug.md")))
	assert.Equal(t, "fix-bug", runName(filepath.Join("plans", "fix-bug.md")))
	if runtime.GOOS == "windows" {
		assert.Equal(t, "fix-bug", runName(`C:\plans\fix-bug.md`))
	}
	assert.Equal(t, "fix-bug", runName(filepath.Join(string(filepath.Separator)+"plans", "fix-bug.md")))
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
		return len(api.titles()) >= n
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

func TestReporterFinishNote(t *testing.T) {
	api := &fakeDispatcher{}
	r := NewWithDispatcher(api, Options{ThreadID: "th"})
	r.SetFinishNote("PR merged")
	r.Finish(true)
	r.Stop()
	assert.Equal(t, []string{"loopai · done · PR merged"}, api.titles())

	failed := &fakeDispatcher{}
	r = NewWithDispatcher(failed, Options{ThreadID: "th"})
	r.SetFinishNote("PR merged")
	r.Finish(false)
	r.Stop()
	assert.Equal(t, []string{"loopai · failed"}, failed.titles())

	var nilReporter *Reporter
	assert.NotPanics(t, func() { nilReporter.SetFinishNote("PR merged") })
}
