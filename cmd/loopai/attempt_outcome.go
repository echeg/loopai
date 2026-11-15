package main

import (
	"fmt"
	"os"
	"time"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/runs"
)

// attemptEnd is a started attempt that returned, as closeAttempt completes it. A detached run and a
// foreground run with a journal directory both fill it; the steps only one of them takes stay with
// the caller.
type attemptEnd struct {
	hooks         *journalHooks         // the attempt's journal hooks; the closing events go to their journal
	runID         string                // the detached run's id; empty for a foreground run
	attempt       int                   // the attempt's number
	branch        string                // the run's branch; empty when it has none
	stopRequested bool                  // a stop was requested for the run
	stopErr       error                 // the failure to read the stop request, noted first; nil when none
	svc           *git.Service          // reads the branch head when the run captured no tip
	outcome       *planExecutionOutcome // what the run recorded about itself
	runErr        error                 // the error the run returned

	// beforeClosing, when set, runs once the record is built and before the closing events are
	// appended, so a file it writes exists before an observer sees the finish event; it notes its
	// own failures through noteOutcome
	beforeClosing func(rec *runs.Outcome)
}

// closeAttempt classifies the attempt, builds its outcome record, and appends the closing events:
// finalize when the close-out ran, report when the run produced one, and finish. A run that did not
// succeed first records every task's status, since its last session may have ticked tasks no event
// recorded. Every warning on the way is reported on stderr and kept in the record's notes. It
// returns the record, not yet stamped or saved, and the cause of a run that did not succeed.
func closeAttempt(end attemptEnd) (runs.Outcome, error) {
	status, cause := classifyDetachedRun(end.runErr, end.outcome, end.stopRequested)
	rec := runs.Outcome{
		RunID: end.runID, Attempt: end.attempt, Status: status, ExitCode: detachExitCode(status),
		Branch: end.branch, Report: end.outcome.report, Finalize: outcomeFinalize(end.outcome),
		PlanMoved: end.outcome.planMoved, ArchiveIncomplete: end.outcome.archiveIncomplete,
	}
	if cause != nil {
		rec.Reason = cause.Error()
	}
	noteOutcome(&rec, end.stopErr)

	commit, err := runCommit(end.svc, end.outcome, end.branch)
	rec.Commit = commit
	noteOutcome(&rec, err)
	var report runs.Event
	if rec.Report != "" {
		report = reportEvent(rec.Report)
		rec.ReportSummary, rec.Risk = report.Summary, report.Risk
	}
	if end.beforeClosing != nil {
		end.beforeClosing(&rec)
	}

	if status != runs.StatusSucceeded {
		end.hooks.recordStatuses()
	}
	if end.outcome.finalizeMode != "" {
		appendClosing(end.hooks.journal, &rec, finalizeEvent(rec.Finalize))
	}
	if rec.Report != "" {
		appendClosing(end.hooks.journal, &rec, report)
	}
	appendClosing(end.hooks.journal, &rec, finishEvent(rec.Status, rec.Reason, rec.ExitCode))
	return rec, cause
}

// saveAttemptOutcome stamps the record's finish time and saves it as dir's outcome.json. It returns
// the stamped record and the save error; a failed save changes nothing about the run.
func saveAttemptOutcome(dir string, rec runs.Outcome) (runs.Outcome, error) {
	rec.FinishedAt = time.Now().UTC()
	if err := runs.SaveOutcome(dir, &rec); err != nil {
		return rec, fmt.Errorf("record the attempt's outcome: %w", err)
	}
	return rec, nil
}

// appendClosing appends one of the attempt's closing events, finalize, report, or finish, straight
// to the journal. Unlike the hooks, which warn about the first failed append only, every failure is
// noted: observers end a run on its finish event, so a lost one must leave a trace.
func appendClosing(j *runs.Journal, rec *runs.Outcome, ev runs.Event) {
	if _, err := j.Append(ev); err != nil {
		noteOutcome(rec, fmt.Errorf("record the %s event: %w", ev.Type, err))
	}
}

// noteOutcome reports err as a warning on stderr, the console log of a detached run, and records it
// in the outcome's notes; a nil err notes nothing.
func noteOutcome(rec *runs.Outcome, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	rec.Notes = append(rec.Notes, err.Error())
}
