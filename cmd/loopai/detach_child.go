package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/runs"
)

const (
	// detachLockTimeout bounds the wait for a run's lifetime lock. An observer's probe holds it
	// only for a moment; a holder still there after this long is a live attempt of the same run.
	detachLockTimeout = 2 * time.Second
	// maxDetachSecretLine caps the launcher's handshake line.
	maxDetachSecretLine = 64 << 10
)

// detachSecret is the launcher's handshake line on the child's secret pipe. T3Token is empty unless
// the run reports to a T3 thread; it is held in memory only and never written anywhere.
type detachSecret struct {
	T3Token string `json:"t3_token"`
}

// detachAckMessage is the child's single handshake line on the ack pipe: ok with the run id and
// attempt once the attempt has started, or the error that ended it before.
type detachAckMessage struct {
	OK      bool   `json:"ok"`
	RunID   string `json:"run_id,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Error   string `json:"error,omitempty"`
}

// readDetachSecret reads the launcher's one JSON line from r. A missing line, a line that does not
// decode, and one longer than maxDetachSecretLine are errors. The line never reaches an error
// message, since it may carry a token.
func readDetachSecret(r io.Reader) (detachSecret, error) {
	line, err := bufio.NewReader(io.LimitReader(r, maxDetachSecretLine)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return detachSecret{}, fmt.Errorf("read handshake secret: %w", err)
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return detachSecret{}, errors.New("read handshake secret: the launcher sent no message")
	}
	var secret detachSecret
	if json.Unmarshal(line, &secret) != nil {
		// the decode error can quote a character of the line, so it is not passed on
		return detachSecret{}, errors.New("read handshake secret: malformed message")
	}
	return secret, nil
}

// detachAck is the child's end of the ack pipe. It carries exactly one line and is closed right
// after it, so the launcher reads EOF as soon as the line is sent, not when the run ends.
type detachAck struct {
	w io.WriteCloser // nil once the line was sent
}

// send writes msg as the single ack line and closes the pipe; later calls do nothing. A failed
// write is returned for the caller to report, and the pipe is closed either way.
func (a *detachAck) send(msg detachAckMessage) error {
	if a.w == nil {
		return nil
	}
	w := a.w
	a.w = nil
	data, _ := json.Marshal(msg) // a bool, two strings, and an int always encode
	_, writeErr := w.Write(append(data, '\n'))
	closeErr := w.Close()
	if writeErr != nil {
		return fmt.Errorf("write handshake ack: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close handshake ack: %w", closeErr)
	}
	return nil
}

// detachedChild is one attempt of a detached run, executed by the hidden --detached-run child in the
// invoking checkout. It owns the run directory while it holds the run's lifetime lock, answers the
// launcher exactly once on the ack pipe, records an error that ends the attempt before it starts as
// the attempt's failed outcome, and completes a started attempt with its journal's closing events
// and its outcome.
type detachedChild struct {
	ack    *detachAck
	secret detachSecret // the launcher's handshake secret, held in memory only

	dir           string
	desc          runs.Descriptor
	releaseLock   func() error  // releases the lifetime lock; the process exit does it instead
	consoleOffset int64         // console.log size when the child took the run
	started       bool          // AttemptStarted succeeded: the launcher was answered ok
	journal       *runs.Journal // the attempt's event journal; nil until execute opens it
	hooks         *journalHooks // record the run in journal; set with it

	// beforeExit runs once the attempt's finish event and outcome are written, right before the
	// process exits and releases the lifetime lock. It must return promptly; nil runs nothing.
	beforeExit func()
}

// run executes the attempt of run o.DetachedRun and returns the error the process ends with: an
// exitCodeError carrying the outcome's exit code once the attempt has started, a plain error before.
func (c *detachedChild) run(ctx context.Context, o opts) error {
	if err := c.lock(o.DetachedRun); err != nil {
		return c.refuse(err)
	}
	// the lock is held until the process exits, after everything the attempt writes; keeping its
	// file reachable stops a finalizer from closing it, which would release the lock early
	defer runtime.KeepAlive(c.releaseLock)
	if err := c.recordProcess(); err != nil {
		return c.startupFailure(err)
	}

	o, cfg, err := loadDetachedConfig(o)
	if err != nil {
		return c.startupFailure(err)
	}
	req, selector, release, err := prepareNonInteractiveRequest(ctx, o, cfg, os.Stdout)
	if err != nil {
		return c.startupFailure(err)
	}
	recordLaunchHistory(o, cfg, req.ExternalReview, launcherForOrigin(c.desc.Origin.Kind), os.Stderr)

	defer c.closeJournal()
	runErr := errors.Join(c.execute(ctx, o, cfg, req, selector), release())
	if !c.started {
		if runErr == nil {
			runErr = errors.New("the run ended before its attempt started")
		}
		return c.startupFailure(runErr)
	}
	return c.complete(req.GitSvc, req.Outcome, runErr)
}

// loadDetachedConfig loads the run's config in worktree mode, which every detached run executes in.
// The option is set on o before loading, so base resolution and plan execution read worktree mode
// from the loaded config exactly as for an explicit --worktree; o is returned with it. Orca titles
// are turned off: the console log is no terminal to title.
func loadDetachedConfig(o opts) (opts, *config.Config, error) {
	o.Worktree = true
	cfg, err := loadRunConfig(o)
	if err != nil {
		return o, nil, err
	}
	cfg.Orca = false
	return o, cfg, nil
}

// lock loads the descriptor of run id from the invoking checkout's runs directory and takes the
// run's lifetime lock, which the process then holds until it exits.
func (c *detachedChild) lock(id string) error {
	if !runs.ValidID(id) {
		return fmt.Errorf("invalid run id %q", id)
	}
	c.dir = runs.Dir(".", id)
	desc, err := runs.LoadDescriptor(c.dir)
	if err != nil {
		return fmt.Errorf("run %s: %w", id, err)
	}
	c.desc = desc
	c.consoleOffset = consoleLogSize(filepath.Join(c.dir, runs.ConsoleLogFile))
	release, err := runs.AcquireBounded(filepath.Join(c.dir, runs.LockFile), detachLockTimeout)
	if errors.Is(err, runs.ErrBusy) {
		return fmt.Errorf("run %s is already running", id)
	}
	if err != nil {
		return fmt.Errorf("run %s: %w", id, err)
	}
	c.releaseLock = release
	return nil
}

// recordProcess records this process and its start token in the descriptor, so observers can tell
// it from a later process that reuses the pid.
func (c *detachedChild) recordProcess() error {
	pid := os.Getpid()
	token, err := runs.ProcessStartToken(pid)
	if err != nil {
		return fmt.Errorf("record run process: %w", err)
	}
	c.desc.PID, c.desc.PIDStart = pid, token
	if err := runs.SaveDescriptor(c.dir, &c.desc); err != nil {
		return fmt.Errorf("record run process: %w", err)
	}
	return nil
}

// execute runs the plan with the detached-run knobs: a keep-awake hold of its own, the worktree kept
// on failure, the archive on the run branch, the event journal, and the attempt hook. The invoking
// checkout's plan is captured first, so a successful run can make that checkout merge-ready again.
func (c *detachedChild) execute(
	ctx context.Context, o opts, cfg *config.Config, req executePlanRequest, selector *plan.Selector,
) error {
	keepAwake := newAwakeHolder(cfg.KeepAwake)
	defer keepAwake.Stop()
	req.KeepAwake = keepAwake

	journal, err := runs.OpenJournal(c.dir, c.desc.Attempt)
	if err != nil {
		return fmt.Errorf("run journal: %w", err)
	}
	hooks := newJournalHooks(journal, cfg)
	c.journal, c.hooks = journal, hooks

	req.KeepWorktreeOnFailure = true
	req.ArchiveOnRunBranch = true
	req.LogDecorator = hooks.decorateLogger
	req.PhaseObserver = hooks.observePhase
	mode, sel := req.Mode, req.ExternalReview
	req.AttemptStarted = func(start attemptStart) error {
		return c.attemptStarted(hooks, start, attemptStartEvent(start, mode, cfg, sel, c.consoleOffset))
	}

	if err := captureDetachedSourcePlan(req.GitSvc, o.PlanFile, filepath.Join(c.dir, runs.SourcePlansFile)); err != nil {
		return err
	}
	return selectAndExecutePlan(ctx, o, req, selector)
}

// attemptStarted is the run's AttemptStarted hook: it records the attempt's branch, worktree, and
// progress log in the descriptor, appends ev, the attempt_start event, and only then answers the
// launcher ok. A failed descriptor or journal write fails the hook, which ends the attempt before it
// executes. A failed answer does not: a launcher that stopped waiting must not end the run, so it is
// only reported on the console log.
func (c *detachedChild) attemptStarted(hooks *journalHooks, start attemptStart, ev runs.Event) error {
	c.desc.Branch, c.desc.Worktree, c.desc.ProgressLog = start.Branch, start.WorktreePath, start.ProgressLog
	if err := runs.SaveDescriptor(c.dir, &c.desc); err != nil {
		return fmt.Errorf("record attempt: %w", err)
	}
	if _, err := hooks.startAttempt(ev); err != nil {
		return err
	}
	c.started = true
	if err := c.ack.send(detachAckMessage{OK: true, RunID: c.desc.ID, Attempt: c.desc.Attempt}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: run %s started, but its launcher could not be told: %v\n", c.desc.ID, err)
	}
	return nil
}

// complete ends a started attempt after the run returned with runErr. It classifies the run, then
// in this order fills the commit, writes report.md, appends the closing events, reconciles the
// invoking checkout's plan after a success, and writes outcome.json; every warning on the way
// reaches the console log and the outcome's notes. The before-exit hook runs last. The returned
// exitCodeError carries the outcome's exit code; the lifetime lock is released only by the process
// exit that follows.
func (c *detachedChild) complete(svc *git.Service, outcome *planExecutionOutcome, runErr error) error {
	stop, stopErr := runs.StopRequested(c.dir)
	status, cause := classifyDetachedRun(runErr, outcome, stop)
	rec := runs.Outcome{
		RunID: c.desc.ID, Attempt: c.desc.Attempt, Status: status, ExitCode: detachExitCode(status),
		Branch: c.desc.Branch, Report: outcome.report, Finalize: outcomeFinalize(outcome),
		PlanMoved: outcome.planMoved, ArchiveIncomplete: outcome.archiveIncomplete,
	}
	if cause != nil {
		rec.Reason = cause.Error()
	}
	c.note(&rec, stopErr)

	commit, err := runCommit(svc, outcome, c.desc.Branch)
	rec.Commit = commit
	c.note(&rec, err)
	var report runs.Event
	if rec.Report != "" {
		report = reportEvent(rec.Report)
		rec.ReportSummary, rec.Risk = report.Summary, report.Risk
		c.note(&rec, runs.WriteReport(c.dir, rec.Report))
	}

	if status != runs.StatusSucceeded {
		c.hooks.recordStatuses()
	}
	if outcome.finalizeMode != "" {
		c.appendClosing(&rec, finalizeEvent(rec.Finalize))
	}
	if rec.Report != "" {
		c.appendClosing(&rec, report)
	}
	c.appendClosing(&rec, finishEvent(rec.Status, rec.Reason, rec.ExitCode))

	rec.WorktreeKept = c.keptWorktree(&rec, outcome.worktreeKept)
	if status == runs.StatusSucceeded {
		c.note(&rec, reconcileDetachedSourcePlan(svc, filepath.Join(c.dir, runs.SourcePlansFile)))
	}
	rec.FinishedAt = time.Now().UTC()
	saveErr := runs.SaveOutcome(c.dir, &rec)
	if c.beforeExit != nil {
		c.beforeExit()
	}
	return &exitCodeError{code: rec.ExitCode, err: errors.Join(cause, saveErr)}
}

// appendClosing appends one of the attempt's closing events, finalize, report, or finish, straight
// to the journal. Unlike the hooks, which warn about the first failed append only, every failure is
// noted: observers end a run on its finish event, so a lost one must leave a trace.
func (c *detachedChild) appendClosing(rec *runs.Outcome, ev runs.Event) {
	if _, err := c.journal.Append(ev); err != nil {
		c.note(rec, fmt.Errorf("record the %s event: %w", ev.Type, err))
	}
}

// note reports err as a warning on the console log and records it in the outcome's notes; a nil
// err notes nothing.
func (c *detachedChild) note(rec *runs.Outcome, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	rec.Notes = append(rec.Notes, err.Error())
}

// keptWorktree returns the worktree the run left in place: the one it recorded keeping or, when it
// meant to remove the worktree and the removal failed, which only warns, the worktree that is still
// there. The latter is also noted, since the run's own record says nothing about it.
func (c *detachedChild) keptWorktree(rec *runs.Outcome, recorded string) string {
	if recorded != "" || c.desc.Worktree == "" {
		return recorded
	}
	if _, err := os.Lstat(c.desc.Worktree); err != nil {
		return ""
	}
	c.note(rec, fmt.Errorf("worktree %s was not removed and is kept", c.desc.Worktree))
	return c.desc.Worktree
}

// closeJournal closes the run's journal once nothing appends to it any more; a journal that was
// never opened is nothing to close.
func (c *detachedChild) closeJournal() {
	if c.journal != nil {
		_ = c.journal.Close()
	}
}

// classifyDetachedRun classifies an attempt that started and returned runErr. Only a run that
// returned no error and recorded its success succeeded. Otherwise the cause is the returned error,
// else the failure the outcome recorded: the run stopped when the stop marker exists or the cause
// is a cancellation, and failed in every other case.
func classifyDetachedRun(runErr error, outcome *planExecutionOutcome, stopRequested bool) (runs.Status, error) {
	if runErr == nil && outcome.succeeded {
		return runs.StatusSucceeded, nil
	}
	cause := runErr
	if cause == nil {
		cause = outcome.failure
	}
	if cause == nil {
		cause = errors.New("the run ended without completing")
	}
	if stopRequested || errors.Is(cause, context.Canceled) {
		return runs.StatusStopped, cause
	}
	return runs.StatusFailed, cause
}

// detachExitCode is the exit code of a finished detached run: 0 succeeded, 2 stopped, 1 failed.
func detachExitCode(status runs.Status) int {
	switch status {
	case runs.StatusSucceeded:
		return 0
	case runs.StatusStopped:
		return 2
	default:
		return 1
	}
}

// runCommit returns the commit the run left its branch at: the tip it captured after a success,
// else the branch's current head. A run without a branch has no commit.
func runCommit(svc *git.Service, outcome *planExecutionOutcome, branch string) (string, error) {
	if outcome.branchTip != "" || branch == "" {
		return outcome.branchTip, nil
	}
	commit, err := svc.BranchHash(branch)
	if err != nil {
		return "", fmt.Errorf("read the run's commit: %w", err)
	}
	return commit, nil
}

// outcomeFinalize is the finalize close-out result as outcome recorded it; empty when the
// close-out did not run.
func outcomeFinalize(o *planExecutionOutcome) runs.Finalize {
	f := runs.Finalize{Mode: o.finalizeMode, StatusNote: o.finalizeStatusNote, PRURL: o.prURL, Merged: o.merged}
	if o.finalizeIncomplete != nil {
		f.Incomplete = o.finalizeIncomplete.Error()
	}
	return f
}

// refuse answers the launcher with err for an attempt that never took the run: without its
// lifetime lock nothing is recorded, since the run directory may belong to a live attempt.
func (c *detachedChild) refuse(err error) error {
	c.answerError(err)
	return err
}

// startupFailure ends an attempt that failed before it started: it answers the launcher with err
// and records the attempt's failed outcome, whose reason is err prefixed with "startup: ".
func (c *detachedChild) startupFailure(err error) error {
	c.answerError(err)
	failure := fmt.Errorf("startup: %w", err)
	outcome := runs.Outcome{
		RunID: c.desc.ID, Attempt: c.desc.Attempt, Status: runs.StatusFailed, Reason: failure.Error(),
		ExitCode: 1, Branch: c.desc.Branch, FinishedAt: time.Now().UTC(),
	}
	if saveErr := runs.SaveOutcome(c.dir, &outcome); saveErr != nil {
		return errors.Join(failure, saveErr)
	}
	return failure
}

// answerError sends err to the launcher. A launcher that is gone cannot be told; that is only
// reported, and err still ends the attempt.
func (c *detachedChild) answerError(err error) {
	if ackErr := c.ack.send(detachAckMessage{Error: err.Error()}); ackErr != nil {
		fmt.Fprintf(os.Stderr, "warning: the run's launcher could not be told why it stopped: %v\n", ackErr)
	}
}

// consoleLogSize returns the size of the console log at path, 0 when it cannot be read.
func consoleLogSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
