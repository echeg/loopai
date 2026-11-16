package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/progress"
	"github.com/umputun/ralphex/pkg/runs"
)

// prepareJournalDir checks and creates the --journal-dir directory before anything else runs and
// returns its absolute path, so the journal and outcome land where the host expects them whatever
// the run does to its working directory later. It refuses a path that exists and is not a directory,
// a directory already holding events.jsonl or outcome.json (the files belong to one run, and a
// foreground run holds no lock that would keep a second one out), and a directory inside the
// repository work tree whose journal files git does not ignore: review stalemate detection
// fingerprints untracked file content, so journal writes there would read as code changes. A
// missing directory is created with mode 0700 once every check has passed.
func prepareJournalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve --journal-dir %s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	switch {
	case err == nil && !info.IsDir():
		return "", fmt.Errorf("--journal-dir %s is not a directory", abs)
	case err == nil:
		for _, name := range []string{runs.JournalFile, runs.OutcomeFile} {
			_, statErr := os.Lstat(filepath.Join(abs, name))
			if statErr == nil {
				return "", fmt.Errorf("--journal-dir %s already holds %s; use an empty directory for each run", abs, name)
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				return "", fmt.Errorf("inspect --journal-dir %s: %w", abs, statErr)
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("inspect --journal-dir %s: %w", abs, err)
	}
	if err := refuseUnignoredJournalDir(abs); err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("create --journal-dir %s: %w", abs, err)
	}
	return abs, nil
}

// refuseUnignoredJournalDir refuses an absolute journal directory inside the work tree of the
// repository holding the working directory unless git ignores both files the run writes there.
// The files are checked rather than the directory because a directory-only ignore rule matches a
// missing directory only once it exists, while a file below an ignored directory always matches.
// Outside a repository there is no work tree to protect. It runs before config loading, so it opens
// the repository with plain git: a repository a custom vcs_command reaches but git does not has no git
// work tree for the journal to pollute.
func refuseUnignoredJournalDir(abs string) error {
	svc, err := git.NewService(".", nil)
	if err != nil || !svc.WorkTreeContains(abs) {
		return nil //nolint:nilerr // no git work tree around the working directory, nothing to pollute
	}
	for _, name := range []string{runs.JournalFile, runs.OutcomeFile} {
		ignored, ignoreErr := svc.PathIgnored(filepath.Join(abs, name))
		if ignoreErr != nil {
			return fmt.Errorf("check --journal-dir %s: %w", abs, ignoreErr)
		}
		if !ignored {
			return fmt.Errorf("--journal-dir %s is inside the repository work tree and git does not ignore %s; "+
				"use a directory outside the checkout or one git ignores", abs, name)
		}
	}
	return nil
}

// executionStdin is the stdin the repository checks may prompt on: os.Stdin, or nil for a
// --journal-dir run, which its host spawned with nobody to answer, so an empty repository is an
// error there, as for a detached run, instead of a "create initial commit?" question.
func executionStdin(o opts) io.Reader {
	if o.JournalDir != "" {
		return nil
	}
	return os.Stdin
}

// foregroundJournal is the event journal and outcome of a foreground --journal-dir run: one attempt
// in the current checkout, owned by the process that spawned loopai. It is not a registered run, so
// it writes neither a descriptor nor a lock, a console log, or report.md. A nil *foregroundJournal,
// the run without the flag, does nothing.
type foregroundJournal struct {
	dir     string
	journal *runs.Journal
	hooks   *journalHooks
	outcome *planExecutionOutcome

	started bool   // the in-place attempt_start was recorded
	branch  string // the branch the attempt started on
}

// attachForegroundJournal creates the journal of a run with --journal-dir and sets its hooks on req:
// the outcome, the log decorator, the phase and usage observers, the in-place attempt hook, and
// NonInteractive, since a host-spawned run has nobody to answer a prompt. Everything else run()
// built stays, its cleanup holders, keep-awake hold, and output included. The journal is created
// exclusively, so a second process given the same directory fails here instead of appending to it.
// Without the flag req is returned unchanged with a nil journal.
func attachForegroundJournal(o opts, cfg *config.Config, req executePlanRequest) (*foregroundJournal, executePlanRequest, error) {
	if o.JournalDir == "" {
		return nil, req, nil
	}
	journal, err := runs.CreateJournal(o.JournalDir, 1)
	if err != nil {
		return nil, req, fmt.Errorf("--journal-dir %s: %w", o.JournalDir, err)
	}
	fj := &foregroundJournal{dir: o.JournalDir, journal: journal, hooks: newJournalHooks(journal, cfg),
		outcome: &planExecutionOutcome{}}
	req.Outcome = fj.outcome
	req.NonInteractive = true
	req.LogDecorator = fj.hooks.decorateLogger
	req.PhaseObserver = fj.hooks.observePhase
	req.UsageHandler = fj.hooks.observeUsage
	mode, sel := req.Mode, req.ExternalReview
	req.InPlaceAttemptStarted = func(start attemptStart) error {
		return fj.attemptStarted(start, attemptStartEvent(start, mode, cfg, sel, 0))
	}
	return fj, req, nil
}

// attemptStarted is the run's InPlaceAttemptStarted hook: it appends ev, the attempt_start event,
// and remembers the branch the completion reads the run's commit from. It first repeats the ignore
// check prepareJournalDir made: that ran on the branch checked out at startup, and branch
// preparation may since have switched to an existing plan branch whose .gitignore does not ignore
// the journal files, which would then read as code changes for the whole run. A refusal or a failed
// append fails the hook, which ends the run before it executes.
func (f *foregroundJournal) attemptStarted(start attemptStart, ev runs.Event) error {
	if err := refuseUnignoredJournalDir(f.dir); err != nil {
		return fmt.Errorf("on branch %q: %w", start.Branch, err)
	}
	if _, err := f.hooks.startAttempt(ev); err != nil {
		return err
	}
	f.started, f.branch = true, start.Branch
	return nil
}

// complete ends the run after execution returned runErr and closes the journal. A started attempt
// is completed as a detached one is, through closeAttempt and saveAttemptOutcome, with a canceled
// context read as a stop request; a run that ended before its attempt started gets only a failed
// outcome, as a detached child's startup failure does. Neither changes the error the process exits
// with: every failure on the way is a warning on stderr.
func (f *foregroundJournal) complete(ctx context.Context, svc *git.Service, runErr error) {
	if f == nil {
		return
	}
	defer func() {
		if err := f.journal.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close the run journal: %v\n", err)
		}
	}()
	rec := startupOutcome(runErr)
	if f.started {
		rec, _ = closeAttempt(attemptEnd{hooks: f.hooks, attempt: 1, branch: f.branch,
			stopRequested: ctx.Err() != nil, svc: svc, outcome: f.outcome, runErr: runErr})
	}
	if _, err := saveAttemptOutcome(f.dir, rec); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	f.hooks.endProgressLog()
}

// startupOutcome is the failed outcome of a run that ended before its attempt started, with a
// reason beginning with "startup:", as a detached child records its startup failure. A nil runErr is
// still a failure: no attempt ran.
func startupOutcome(runErr error) runs.Outcome {
	cause := runErr
	if cause == nil {
		cause = errors.New("the run ended before its attempt started")
	}
	return runs.Outcome{Attempt: 1, Status: runs.StatusFailed, Reason: fmt.Errorf("startup: %w", cause).Error(),
		ExitCode: 1}
}

// failForegroundStartup records runErr as the failed startup outcome of a --journal-dir run that
// returned before its journal was attached: configuration loading, the dependency checks, or the
// repository checks failed after prepareJournalDir accepted the directory. It first claims the
// directory by creating the journal exclusively, as attaching does, so a directory another process
// took meanwhile is left alone; the journal stays empty, as for a run that failed after attaching
// and before its attempt started. A failure on the way is a warning on stderr.
func failForegroundStartup(dir string, runErr error) {
	journal, err := runs.CreateJournal(dir, 1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: --journal-dir %s: the startup failure is not recorded: %v\n", dir, err)
		return
	}
	defer func() {
		if closeErr := journal.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: close the run journal: %v\n", closeErr)
		}
	}()
	if _, err := saveAttemptOutcome(dir, startupOutcome(runErr)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
}

// fireInPlaceAttemptStarted calls req.InPlaceAttemptStarted for a run in the current checkout once
// its progress logger exists. The plan and the progress log are named by absolute path, and the plan
// stands for both the plan and the copy the run ticks, since an in-place run has no worktree. The
// branch is the one checked out, read again rather than taken from the progress header, whose
// "unknown" placeholder is no branch; it is empty when none is checked out.
func fireInPlaceAttemptStarted(req executePlanRequest, baseLog *progress.Logger) error {
	if req.InPlaceAttemptStarted == nil {
		return nil
	}
	branch, err := req.GitSvc.CurrentBranch()
	if err != nil {
		branch = ""
	}
	planFile := absPath(req.PlanFile)
	start := attemptStart{PlanFile: planFile, MainPlanFile: planFile, Branch: branch,
		ProgressLog: absPath(baseLog.Path())}
	if err := req.InPlaceAttemptStarted(start); err != nil {
		return fmt.Errorf("start attempt: %w", err)
	}
	return nil
}
