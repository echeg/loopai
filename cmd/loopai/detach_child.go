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
// launcher exactly once on the ack pipe, and records an error that ends the attempt before it
// starts as the attempt's failed outcome.
type detachedChild struct {
	ack    *detachAck
	secret detachSecret // the launcher's handshake secret, held in memory only

	dir           string
	desc          runs.Descriptor
	releaseLock   func() error // releases the lifetime lock; the process exit does it instead
	consoleOffset int64        // console.log size when the child took the run
	started       bool         // AttemptStarted succeeded: the launcher was answered ok
}

// run executes the attempt of run o.DetachedRun and returns the error the process ends with: an
// exitCodeError once the attempt has started, a plain error before.
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

	// set on the options before config loading, so base resolution and plan execution read worktree
	// mode from the loaded config exactly as for an explicit --worktree
	o.Worktree = true
	cfg, err := loadRunConfig(o)
	if err != nil {
		return c.startupFailure(err)
	}
	cfg.Orca = false // the console log is no terminal to title

	req, selector, release, err := prepareNonInteractiveRequest(ctx, o, cfg, os.Stdout)
	if err != nil {
		return c.startupFailure(err)
	}
	runErr := errors.Join(c.execute(ctx, o, cfg, req, selector), release())
	if !c.started {
		if runErr == nil {
			runErr = errors.New("the run ended before its attempt started")
		}
		return c.startupFailure(runErr)
	}
	if runErr == nil && req.Outcome.succeeded {
		return &exitCodeError{code: 0}
	}
	if runErr == nil {
		runErr = req.Outcome.failure
	}
	return &exitCodeError{code: 1, err: runErr}
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
	defer journal.Close()
	hooks := newJournalHooks(journal, cfg)

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
