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

// endingAttempt is a foreground-style attempt about to end: attempt 1 of branch feature, no run id,
// its journal open in its own directory and its attempt recorded over a plan whose first task is
// done.
type endingAttempt struct {
	dir   string
	hooks *journalHooks
	svc   *git.Service
	repo  string
}

func newEndingAttempt(t *testing.T) endingAttempt {
	t.Helper()
	repo := setupTestRepo(t)
	runGit(t, repo, "branch", "feature")
	svc, err := git.NewService(repo, noopLogger())
	require.NoError(t, err)

	dir := t.TempDir()
	j, err := runs.OpenJournal(dir, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = j.Close() })
	hooks := newJournalHooks(j, &config.Config{})
	hooks.warnOut = &bytes.Buffer{}

	planFile := filepath.Join(t.TempDir(), "feature.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# Feature\n\n### Task 1: One\n- [x] a\n\n### Task 2: Two\n- [ ] b\n"),
		0o600))
	_, err = hooks.startAttempt(attemptStartEvent(attemptStart{MainPlanFile: planFile, PlanFile: planFile, Branch: "feature"},
		processor.ModeFull, &config.Config{}, externalReviewSelection{}, 0))
	require.NoError(t, err)
	return endingAttempt{dir: dir, hooks: hooks, svc: svc, repo: repo}
}

// end returns the attemptEnd of this attempt for outcome and runErr.
func (a endingAttempt) end(outcome *planExecutionOutcome, runErr error) attemptEnd {
	return attemptEnd{hooks: a.hooks, attempt: 1, branch: "feature", svc: a.svc, outcome: outcome, runErr: runErr}
}

// closingEvents returns the events after attempt_start.
func (a endingAttempt) closingEvents(t *testing.T) []runs.Event {
	t.Helper()
	events, err := runs.ReadFrom(a.dir, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	require.Equal(t, runs.EventAttemptStart, events[0].Type)
	return events[1:]
}

func TestCloseAttempt(t *testing.T) {
	failure := errors.New("runner: task phase: task execution failed")
	canceled := fmt.Errorf("runner: task phase: %w", context.Canceled)
	terminated := errors.New("runner: task phase: claude: signal: terminated")
	report := "# Report: Feature\n\n## Summary\nDelivered.\n\n## Risk\n**Low** - small.\n"

	tests := []struct {
		name       string
		outcome    *planExecutionOutcome
		runErr     error
		stop       bool
		wantStatus runs.Status
		wantCode   int
		wantCause  error
		wantEvents []runs.EventType
	}{
		{name: "succeeded", outcome: &planExecutionOutcome{succeeded: true, branchTip: "abc", report: report,
			finalizeMode: config.FinalizeSync, finalizeStatusNote: "synced"},
			wantStatus: runs.StatusSucceeded, wantCode: 0,
			wantEvents: []runs.EventType{runs.EventFinalize, runs.EventReport, runs.EventFinish}},
		{name: "failed", outcome: &planExecutionOutcome{}, runErr: failure,
			wantStatus: runs.StatusFailed, wantCode: 1, wantCause: failure,
			wantEvents: []runs.EventType{runs.EventTasks, runs.EventFinish}},
		{name: "stopped by a canceled context", outcome: &planExecutionOutcome{}, runErr: canceled,
			wantStatus: runs.StatusStopped, wantCode: 2, wantCause: canceled,
			wantEvents: []runs.EventType{runs.EventTasks, runs.EventFinish}},
		{name: "stopped by a stop request", outcome: &planExecutionOutcome{}, runErr: terminated, stop: true,
			wantStatus: runs.StatusStopped, wantCode: 2, wantCause: terminated,
			wantEvents: []runs.EventType{runs.EventTasks, runs.EventFinish}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newEndingAttempt(t)
			end := a.end(tc.outcome, tc.runErr)
			end.stopRequested = tc.stop

			var rec runs.Outcome
			var cause error
			out := captureStderr(t, func() { rec, cause = closeAttempt(end) })

			assert.Empty(t, out, "nothing to warn about")
			assert.Equal(t, tc.wantCause, cause)
			assert.Empty(t, rec.RunID, "a foreground attempt has no run id")
			assert.Equal(t, 1, rec.Attempt)
			assert.Equal(t, tc.wantStatus, rec.Status)
			assert.Equal(t, tc.wantCode, rec.ExitCode)
			assert.Equal(t, "feature", rec.Branch)
			assert.Empty(t, rec.Notes)
			assert.True(t, rec.FinishedAt.IsZero(), "closeAttempt does not stamp the record")
			if tc.wantCause != nil {
				assert.Equal(t, tc.wantCause.Error(), rec.Reason)
				assert.Equal(t, strings.TrimSpace(gitOutput(t, a.repo, "rev-parse", "refs/heads/feature")), rec.Commit,
					"without a captured tip the commit is the branch head")
			} else {
				assert.Empty(t, rec.Reason)
				assert.Equal(t, "abc", rec.Commit)
				assert.Equal(t, report, rec.Report)
				assert.Equal(t, "Delivered.", rec.ReportSummary)
				assert.Equal(t, "low", rec.Risk)
				assert.Equal(t, runs.Finalize{Mode: config.FinalizeSync, StatusNote: "synced"}, rec.Finalize)
			}

			events := a.closingEvents(t)
			require.Equal(t, tc.wantEvents, eventTypes(events))
			finish := events[len(events)-1]
			assert.Equal(t, tc.wantStatus, finish.Status)
			assert.Equal(t, tc.wantCode, finish.ExitCode)
			assert.Equal(t, rec.Reason, finish.Reason)
			assert.Equal(t, 1, finish.Attempt)
			assert.NoFileExists(t, filepath.Join(a.dir, runs.OutcomeFile), "closeAttempt saves nothing")
			assert.NoFileExists(t, filepath.Join(a.dir, runs.ReportFile))
		})
	}
}

func TestCloseAttemptNotes(t *testing.T) {
	t.Run("the stop read failure is noted before the commit failure", func(t *testing.T) {
		a := newEndingAttempt(t)
		end := a.end(&planExecutionOutcome{failure: errors.New("boom")}, nil)
		end.branch = "missing"
		end.stopErr = errors.New("read the stop request: denied")

		var rec runs.Outcome
		out := captureStderr(t, func() { rec, _ = closeAttempt(end) })

		require.Len(t, rec.Notes, 2)
		assert.Equal(t, "read the stop request: denied", rec.Notes[0])
		assert.Contains(t, rec.Notes[1], "read the run's commit: ")
		assert.Contains(t, out, "warning: read the stop request: denied")
	})

	t.Run("the before-closing step runs before the closing events and can note", func(t *testing.T) {
		a := newEndingAttempt(t)
		end := a.end(&planExecutionOutcome{succeeded: true, branchTip: "abc", report: "# Report: X\n"}, nil)
		var seen []runs.EventType
		end.beforeClosing = func(rec *runs.Outcome) {
			seen = eventTypes(a.closingEvents(t))
			assert.Equal(t, "# Report: X\n", rec.Report, "the record is built when the step runs")
			noteOutcome(rec, errors.New("write report: denied"))
		}

		var rec runs.Outcome
		_ = captureStderr(t, func() { rec, _ = closeAttempt(end) })

		assert.Empty(t, seen, "no closing event is appended before the step")
		assert.Equal(t, []string{"write report: denied"}, rec.Notes)
		assert.Equal(t, []runs.EventType{runs.EventReport, runs.EventFinish}, eventTypes(a.closingEvents(t)))
	})

	t.Run("every closing event the journal cannot take is noted", func(t *testing.T) {
		a := newEndingAttempt(t)
		require.NoError(t, a.hooks.journal.Close())

		var rec runs.Outcome
		_ = captureStderr(t, func() {
			rec, _ = closeAttempt(a.end(&planExecutionOutcome{succeeded: true, branchTip: "abc", report: "# Report: X\n",
				finalizeMode: config.FinalizeSync}, nil))
		})

		assert.Equal(t, []string{
			"record the finalize event: append journal event: journal is closed",
			"record the report event: append journal event: journal is closed",
			"record the finish event: append journal event: journal is closed",
		}, rec.Notes)
	})
}

func TestSaveAttemptOutcome(t *testing.T) {
	t.Run("stamps and saves the record", func(t *testing.T) {
		dir := t.TempDir()
		rec := runs.Outcome{Attempt: 1, Status: runs.StatusFailed, Reason: "boom", ExitCode: 1, Branch: "feature",
			Notes: []string{"a note"}}

		saved, err := saveAttemptOutcome(dir, rec)

		require.NoError(t, err)
		assert.WithinDuration(t, time.Now(), saved.FinishedAt, time.Minute)
		assert.Equal(t, time.UTC, saved.FinishedAt.Location())
		loaded, err := runs.LoadOutcome(dir)
		require.NoError(t, err)
		assert.Empty(t, loaded.RunID)
		assert.Equal(t, runs.StatusFailed, loaded.Status)
		assert.Equal(t, "boom", loaded.Reason)
		assert.Equal(t, 1, loaded.ExitCode)
		assert.Equal(t, []string{"a note"}, loaded.Notes)
		assert.True(t, saved.FinishedAt.Equal(loaded.FinishedAt))
	})

	t.Run("a failed save returns the stamped record and the error", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, runs.OutcomeFile), 0o750))

		saved, err := saveAttemptOutcome(dir, runs.Outcome{Attempt: 1, Status: runs.StatusSucceeded})

		require.ErrorContains(t, err, "save run outcome")
		assert.False(t, saved.FinishedAt.IsZero())
		assert.Equal(t, runs.StatusSucceeded, saved.Status)
	})
}
