package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/runs"
)

func TestReadDetachSecret(t *testing.T) {
	const token = "t3-secret-token-value"
	tests := []struct {
		name    string
		input   string
		want    detachSecret
		wantErr string
	}{
		{name: "line with a token", input: `{"t3_token":"` + token + `"}` + "\n", want: detachSecret{T3Token: token}},
		{name: "line without a token", input: `{"t3_token":""}` + "\n"},
		{name: "line ended by EOF", input: `{"t3_token":"x"}`, want: detachSecret{T3Token: "x"}},
		{name: "only the first line is read", input: `{"t3_token":"a"}` + "\n" + `{"t3_token":"b"}` + "\n",
			want: detachSecret{T3Token: "a"}},
		{name: "nothing before EOF", input: "", wantErr: "the launcher sent no message"},
		{name: "blank line", input: "\n", wantErr: "the launcher sent no message"},
		{name: "malformed line", input: `{"t3_token":"` + token + "\n", wantErr: "malformed message"},
		{name: "not an object", input: `"` + token + `"` + "\n", wantErr: "malformed message"},
		{name: "line over the cap", input: `{"t3_token":"` + strings.Repeat("x", maxDetachSecretLine) + `"}` + "\n",
			wantErr: "malformed message"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readDetachSecret(strings.NewReader(tc.input))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, err.Error(), token, "the secret never reaches an error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("read failure", func(t *testing.T) {
		_, err := readDetachSecret(&failingReader{err: errors.New("broken pipe")})
		require.ErrorContains(t, err, "read handshake secret: broken pipe")
	})
}

// failingReader fails every read with err.
type failingReader struct{ err error }

func (r *failingReader) Read([]byte) (int, error) { return 0, r.err }

// ackRecorder is an ack pipe that records what was written and whether it was closed.
type ackRecorder struct {
	buf      bytes.Buffer
	writeErr error
	closed   int
}

func (r *ackRecorder) Write(p []byte) (int, error) {
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	r.buf.Write(p) // a bytes.Buffer write always succeeds
	return len(p), nil
}

func (r *ackRecorder) Close() error {
	r.closed++
	return nil
}

func TestDetachAckSend(t *testing.T) {
	t.Run("ok carries the run id and attempt", func(t *testing.T) {
		rec := &ackRecorder{}
		ack := &detachAck{w: rec}
		require.NoError(t, ack.send(detachAckMessage{OK: true, RunID: "plan-20261022-120000-abcd", Attempt: 2}))
		assert.JSONEq(t, `{"ok":true,"run_id":"plan-20261022-120000-abcd","attempt":2}`, rec.buf.String())
		assert.Equal(t, 1, strings.Count(rec.buf.String(), "\n"), "one line")
		assert.True(t, strings.HasSuffix(rec.buf.String(), "\n"), "newline-terminated")
		assert.Equal(t, 1, rec.closed, "the pipe is closed right after the line")
	})

	t.Run("an error carries only the message", func(t *testing.T) {
		rec := &ackRecorder{}
		ack := &detachAck{w: rec}
		require.NoError(t, ack.send(detachAckMessage{Error: "run x is already running"}))
		assert.JSONEq(t, `{"ok":false,"error":"run x is already running"}`, rec.buf.String())
		assert.True(t, strings.HasSuffix(rec.buf.String(), "\n"), "newline-terminated")
	})

	t.Run("only the first message is sent", func(t *testing.T) {
		rec := &ackRecorder{}
		ack := &detachAck{w: rec}
		require.NoError(t, ack.send(detachAckMessage{OK: true, RunID: "a", Attempt: 1}))
		require.NoError(t, ack.send(detachAckMessage{Error: "later"}))
		assert.Equal(t, 1, strings.Count(rec.buf.String(), "\n"))
		assert.NotContains(t, rec.buf.String(), "later")
		assert.Equal(t, 1, rec.closed)
	})

	t.Run("a failed write is returned and the pipe still closed", func(t *testing.T) {
		rec := &ackRecorder{writeErr: errors.New("broken pipe")}
		ack := &detachAck{w: rec}
		require.ErrorContains(t, ack.send(detachAckMessage{OK: true, RunID: "a", Attempt: 1}), "write handshake ack: broken pipe")
		assert.Equal(t, 1, rec.closed)
		require.NoError(t, ack.send(detachAckMessage{Error: "again"}), "nothing is retried")
		assert.Equal(t, 1, rec.closed)
	})
}

func TestClassifyDetachedRun(t *testing.T) {
	failure := errors.New("task phase: task execution failed")
	canceled := fmt.Errorf("runner: task phase: %w", context.Canceled)
	tests := []struct {
		name      string
		runErr    error
		outcome   planExecutionOutcome
		stop      bool
		want      runs.Status
		wantCause string
		wantCode  int
	}{
		{name: "success", outcome: planExecutionOutcome{succeeded: true}, want: runs.StatusSucceeded},
		{name: "a stop that arrived after the run completed", outcome: planExecutionOutcome{succeeded: true}, stop: true,
			want: runs.StatusSucceeded},
		{name: "an error after the recorded success", runErr: errors.New("release chain lock: boom"),
			outcome: planExecutionOutcome{succeeded: true}, want: runs.StatusFailed, wantCause: "release chain lock: boom",
			wantCode: 1},
		{name: "returned error", runErr: errors.New("runner: " + failure.Error()), want: runs.StatusFailed,
			wantCause: "runner: task phase: task execution failed", wantCode: 1},
		{name: "recorded failure without a returned error", outcome: planExecutionOutcome{failure: failure},
			want: runs.StatusFailed, wantCause: failure.Error(), wantCode: 1},
		{name: "the returned error comes before the recorded failure", runErr: errors.New("runner: boom"),
			outcome: planExecutionOutcome{failure: canceled}, want: runs.StatusFailed, wantCause: "runner: boom", wantCode: 1},
		{name: "cancellation", runErr: canceled, want: runs.StatusStopped, wantCause: canceled.Error(), wantCode: 2},
		{name: "recorded cancellation", outcome: planExecutionOutcome{failure: canceled}, want: runs.StatusStopped,
			wantCause: canceled.Error(), wantCode: 2},
		{name: "stop marker", runErr: errors.New("runner: claude: signal: terminated"), stop: true,
			want: runs.StatusStopped, wantCause: "runner: claude: signal: terminated", wantCode: 2},
		{name: "no error and no success", want: runs.StatusFailed, wantCause: "the run ended without completing", wantCode: 1},
		{name: "no error and no success with a stop marker", stop: true, want: runs.StatusStopped,
			wantCause: "the run ended without completing", wantCode: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, cause := classifyDetachedRun(tc.runErr, &tc.outcome, tc.stop)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantCode, detachExitCode(got))
			if tc.wantCause == "" {
				require.NoError(t, cause)
				return
			}
			require.EqualError(t, cause, tc.wantCause)
		})
	}
}

func TestLoadDetachedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	chdirTemp(t)
	cfgDir := filepath.Join(home, "config")
	require.NoError(t, os.MkdirAll(cfgDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"), []byte("orca = true\nuse_worktree = false\n"), 0o600))

	o, cfg, err := loadDetachedConfig(opts{ConfigDir: cfgDir, Orca: true})
	require.NoError(t, err)
	assert.True(t, o.Worktree, "the options carry worktree mode on to plan execution")
	assert.True(t, cfg.WorktreeEnabled, "a detached run always executes in its own worktree")
	assert.False(t, cfg.Orca, "the console log is no terminal to title")

	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"), []byte("finalize_enabled = true\n"), 0o600))
	_, _, err = loadDetachedConfig(opts{ConfigDir: cfgDir})
	require.ErrorContains(t, err, "finalize_enabled")
}

// completingChild is a started attempt about to complete: attempt 2 of a run of branch feature in
// a repository with that branch, its journal open and its attempt recorded over a worktree plan
// whose first task is done.
type completingChild struct {
	*detachedChild
	repo     string
	svc      *git.Service
	planFile string // the worktree plan the journal hooks re-read
}

func newCompletingChild(t *testing.T) completingChild {
	t.Helper()
	repo := setupTestRepo(t)
	runGit(t, repo, "config", "core.autocrlf", "false")
	runGit(t, repo, "branch", "feature")
	svc, err := git.NewService(repo, noopLogger())
	require.NoError(t, err)

	const id = "feature-20261022-120000-abcd"
	dir := runs.Dir(repo, id)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	j, err := runs.OpenJournal(dir, 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = j.Close() })
	hooks := newJournalHooks(j, &config.Config{})
	hooks.warnOut = &bytes.Buffer{}

	planFile := filepath.Join(t.TempDir(), "feature.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# Feature\n\n### Task 1: One\n- [x] a\n\n### Task 2: Two\n- [ ] b\n"),
		0o600))
	_, err = hooks.startAttempt(attemptStartEvent(attemptStart{PlanFile: planFile, Branch: "feature"},
		processor.ModeFull, &config.Config{}, externalReviewSelection{}, 0))
	require.NoError(t, err)

	c := &detachedChild{dir: dir, journal: j, hooks: hooks, started: true, desc: runs.Descriptor{
		ID: id, Attempt: 2, Branch: "feature", Worktree: filepath.Join(repo, ".loopai", "worktrees", "feature"),
	}}
	return completingChild{detachedChild: c, repo: repo, svc: svc, planFile: planFile}
}

// complete runs complete and returns the process exit code it chose, the error line it carries, and
// the console log output it wrote.
func (c completingChild) complete(t *testing.T, outcome *planExecutionOutcome, runErr error) (code int, errLine, out string) {
	t.Helper()
	var err error
	out = captureStderr(t, func() { err = c.detachedChild.complete(c.svc, outcome, runErr) })
	var exitErr *exitCodeError
	require.ErrorAs(t, err, &exitErr)
	if exitErr.err != nil {
		errLine = exitErr.err.Error()
	}
	return exitErr.code, errLine, out
}

// closingEvents returns the events after attempt_start.
func (c completingChild) closingEvents(t *testing.T) []runs.Event {
	t.Helper()
	events, err := runs.ReadFrom(c.dir, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	require.Equal(t, runs.EventAttemptStart, events[0].Type)
	return events[1:]
}

// eventTypes lists the type of each event.
func eventTypes(events []runs.Event) []runs.EventType {
	types := make([]runs.EventType, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	return types
}

// captureSourcePlan writes an untracked plan into the repository and captures it as the run's
// source plan, as the first attempt does.
func (c completingChild) captureSourcePlan(t *testing.T) string {
	t.Helper()
	planPath := filepath.Join(c.repo, "docs", "plans", "feature.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o750))
	require.NoError(t, os.WriteFile(planPath, []byte("# Feature\n"), 0o600))
	require.NoError(t, captureDetachedSourcePlan(c.svc, planPath, filepath.Join(c.dir, runs.SourcePlansFile)))
	return planPath
}

func TestDetachedChildCompleteSuccess(t *testing.T) {
	c := newCompletingChild(t)
	sourcePlan := c.captureSourcePlan(t)
	// a report above the outcome's cap whose last rune straddles it
	report := "# Report: Feature\n\n## Summary\nDelivered the feature.\n\n## Risk\n**Medium** - touches config.\n\n" +
		"## Validation\n" + strings.Repeat("v", 300<<10) + "€"
	outcome := &planExecutionOutcome{succeeded: true, branchTip: "0123456789abcdef", report: report,
		finalizeMode: config.FinalizePR, finalizeStatusNote: "PR opened", prURL: "https://github.com/acme/repo/pull/7",
		prOpened: true, planMoved: true}
	var atExit struct {
		outcome bool
		events  []runs.EventType
	}
	c.beforeExit = func() {
		_, err := os.Stat(filepath.Join(c.dir, runs.OutcomeFile))
		atExit.outcome = err == nil
		events, err := runs.ReadFrom(c.dir, 0)
		require.NoError(t, err)
		atExit.events = eventTypes(events)
	}

	code, errLine, out := c.complete(t, outcome, nil)

	assert.Equal(t, 0, code)
	assert.Empty(t, errLine, "a success exits without an error line")
	assert.Empty(t, out, "nothing to warn about")
	rec, err := runs.LoadOutcome(c.dir)
	require.NoError(t, err)
	assert.Equal(t, "feature-20261022-120000-abcd", rec.RunID)
	assert.Equal(t, 2, rec.Attempt)
	assert.Equal(t, runs.StatusSucceeded, rec.Status)
	assert.Empty(t, rec.Reason)
	assert.Equal(t, 0, rec.ExitCode)
	assert.Equal(t, "feature", rec.Branch)
	assert.Equal(t, "0123456789abcdef", rec.Commit, "the tip captured after the success")
	assert.Empty(t, rec.WorktreeKept)
	assert.Equal(t, runs.Finalize{Mode: config.FinalizePR, StatusNote: "PR opened",
		PRURL: "https://github.com/acme/repo/pull/7"}, rec.Finalize)
	assert.True(t, rec.PlanMoved)
	assert.Empty(t, rec.ArchiveIncomplete)
	assert.Empty(t, rec.Notes)
	assert.WithinDuration(t, time.Now(), rec.FinishedAt, time.Minute)

	t.Run("the outcome caps the report while report.md keeps it whole", func(t *testing.T) {
		assert.Len(t, rec.Report, 256<<10)
		assert.True(t, strings.HasPrefix(report, rec.Report))
		full, err := runs.ReadReport(c.dir)
		require.NoError(t, err)
		assert.Equal(t, report, full)
		assert.Equal(t, "Delivered the feature.", rec.ReportSummary)
		assert.Equal(t, "medium", rec.Risk)
	})

	t.Run("the closing events end with finish", func(t *testing.T) {
		events := c.closingEvents(t)
		require.Equal(t, []runs.EventType{runs.EventFinalize, runs.EventReport, runs.EventFinish}, eventTypes(events),
			"a success records no final task statuses")
		assert.Equal(t, config.FinalizePR, events[0].Mode)
		assert.Equal(t, "PR opened", events[0].StatusNote)
		assert.Equal(t, "https://github.com/acme/repo/pull/7", events[0].PRURL)
		assert.Equal(t, "Delivered the feature.", events[1].Summary)
		assert.Equal(t, "medium", events[1].Risk)
		assert.Equal(t, runs.StatusSucceeded, events[2].Status)
		assert.Zero(t, events[2].ExitCode)
		assert.Equal(t, 2, events[2].Attempt)
	})

	t.Run("the source plan is reconciled and the hook runs after the outcome", func(t *testing.T) {
		assert.NoFileExists(t, sourcePlan, "an unchanged untracked source plan is consumed by the run")
		assert.True(t, atExit.outcome, "outcome.json exists when the before-exit hook runs")
		assert.Equal(t, runs.EventFinish, atExit.events[len(atExit.events)-1])
	})
}

func TestDetachedChildCompleteFailure(t *testing.T) {
	c := newCompletingChild(t)
	sourcePlan := c.captureSourcePlan(t)
	require.NoError(t, os.MkdirAll(c.desc.Worktree, 0o750))
	failure := errors.New("runner: task phase: task execution failed")
	outcome := &planExecutionOutcome{failure: errors.New("task phase: task execution failed"),
		worktreeKept: c.desc.Worktree}

	code, errLine, out := c.complete(t, outcome, failure)

	assert.Equal(t, 1, code)
	assert.Equal(t, failure.Error(), errLine, "the reason becomes the error line")
	assert.Empty(t, out)
	rec, err := runs.LoadOutcome(c.dir)
	require.NoError(t, err)
	assert.Equal(t, runs.StatusFailed, rec.Status)
	assert.Equal(t, failure.Error(), rec.Reason)
	assert.Equal(t, 1, rec.ExitCode)
	assert.Equal(t, strings.TrimSpace(gitOutput(t, c.repo, "rev-parse", "refs/heads/feature")), rec.Commit,
		"without a captured tip the commit is the branch head")
	assert.Equal(t, c.desc.Worktree, rec.WorktreeKept)
	assert.Empty(t, rec.Report)
	assert.Equal(t, runs.Finalize{}, rec.Finalize, "the close-out never ran")
	assert.NoFileExists(t, filepath.Join(c.dir, runs.ReportFile), "a run without a report writes no report.md")
	assert.FileExists(t, sourcePlan, "a failed run does not reconcile the source plan")

	events := c.closingEvents(t)
	require.Equal(t, []runs.EventType{runs.EventTasks, runs.EventFinish}, eventTypes(events),
		"the plan is re-read before finish; no finalize or report events")
	assert.Equal(t, []runs.TaskStatus{{N: 1, Status: runs.ProgressDone}, {N: 2, Status: runs.ProgressPending}},
		events[0].Statuses)
	assert.Equal(t, runs.Event{Type: runs.EventFinish, Status: runs.StatusFailed, Reason: failure.Error(), ExitCode: 1},
		runs.Event{Type: events[1].Type, Status: events[1].Status, Reason: events[1].Reason, ExitCode: events[1].ExitCode})
}

func TestDetachedChildCompleteStopped(t *testing.T) {
	c := newCompletingChild(t)
	require.NoError(t, runs.RequestStop(c.dir))
	runErr := errors.New("runner: task phase: claude: signal: terminated")

	code, errLine, _ := c.complete(t, &planExecutionOutcome{}, runErr)

	assert.Equal(t, 2, code)
	assert.Equal(t, runErr.Error(), errLine)
	rec, err := runs.LoadOutcome(c.dir)
	require.NoError(t, err)
	assert.Equal(t, runs.StatusStopped, rec.Status)
	assert.Equal(t, 2, rec.ExitCode)
	events := c.closingEvents(t)
	require.Equal(t, []runs.EventType{runs.EventTasks, runs.EventFinish}, eventTypes(events))
	assert.Equal(t, runs.StatusStopped, events[1].Status)
	assert.Equal(t, 2, events[1].ExitCode)
}

func TestDetachedChildCompleteNotes(t *testing.T) {
	t.Run("a worktree left after a removal failed is kept and noted", func(t *testing.T) {
		c := newCompletingChild(t)
		require.NoError(t, os.MkdirAll(c.desc.Worktree, 0o750))

		code, _, out := c.complete(t, &planExecutionOutcome{succeeded: true, branchTip: "abc"}, nil)

		assert.Equal(t, 0, code)
		rec, err := runs.LoadOutcome(c.dir)
		require.NoError(t, err)
		assert.Equal(t, c.desc.Worktree, rec.WorktreeKept)
		want := "worktree " + c.desc.Worktree + " was not removed and is kept"
		assert.Equal(t, []string{want}, rec.Notes)
		assert.Contains(t, out, "warning: "+want)
	})

	t.Run("a source plan edited during the run is left and noted", func(t *testing.T) {
		c := newCompletingChild(t)
		sourcePlan := c.captureSourcePlan(t)
		require.NoError(t, os.WriteFile(sourcePlan, []byte("# Feature\n\nedited during the run\n"), 0o600))

		code, _, out := c.complete(t, &planExecutionOutcome{succeeded: true, branchTip: "abc"}, nil)

		assert.Equal(t, 0, code, "a reconcile warning does not fail the run")
		rec, err := runs.LoadOutcome(c.dir)
		require.NoError(t, err)
		want := `reconcile the invoking checkout: source plan "docs/plans/feature.md" changed during execution; left untouched`
		assert.Equal(t, []string{want}, rec.Notes)
		assert.Contains(t, out, "warning: "+want)
		assert.NotContains(t, out, "chain", "a detached run's warning names no plan chain")
		assert.FileExists(t, sourcePlan)
	})

	t.Run("an unreadable branch head is noted", func(t *testing.T) {
		c := newCompletingChild(t)
		c.desc.Branch = "missing"

		code, _, out := c.complete(t, &planExecutionOutcome{failure: errors.New("boom")}, nil)

		assert.Equal(t, 1, code)
		rec, err := runs.LoadOutcome(c.dir)
		require.NoError(t, err)
		assert.Empty(t, rec.Commit)
		require.Len(t, rec.Notes, 1)
		assert.Contains(t, rec.Notes[0], "read the run's commit: ")
		assert.Contains(t, out, "warning: read the run's commit: ")
	})

	t.Run("an outcome that cannot be saved keeps the exit code and is reported", func(t *testing.T) {
		c := newCompletingChild(t)
		require.NoError(t, os.Mkdir(filepath.Join(c.dir, runs.OutcomeFile), 0o750))
		called := false
		c.beforeExit = func() { called = true }

		code, errLine, _ := c.complete(t, &planExecutionOutcome{succeeded: true, branchTip: "abc"}, nil)

		assert.Equal(t, 0, code)
		assert.Contains(t, errLine, "save run outcome")
		assert.True(t, called, "the before-exit hook still runs")
		events := c.closingEvents(t)
		assert.Equal(t, runs.EventFinish, events[len(events)-1].Type)
	})
}
