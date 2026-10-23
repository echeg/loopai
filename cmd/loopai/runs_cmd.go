package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/progress"
	"github.com/umputun/ralphex/pkg/runs"
)

const (
	// runsPoll is how often --attach reads a run's console log and status, and --stop its lifetime lock.
	runsPoll = 250 * time.Millisecond
	// consoleChunk is how much of a console log --attach copies between two checks for Ctrl+C.
	consoleChunk = 64 << 10
	// consoleReplayLimit bounds how much of a console log --attach replays before following it: a
	// run of many hours, every attempt included, would otherwise push its whole history through the
	// terminal on every attach, which the Mode 1 bridge repeats after each T3 Code restart.
	consoleReplayLimit = 1 << 20
	// runsTimeLayout is how --runs prints a run's start time, in local time.
	runsTimeLayout = "2006-01-02 15:04:05"
)

var (
	// stopWaitTimeout bounds how long --stop waits for a stopped run to release its lifetime lock.
	stopWaitTimeout = 15 * time.Second
	// signalRun asks a run's process to stop; tests replace it to record the pid instead.
	signalRun = signalTerm
)

// runCommandRequested reports whether the invocation is --attach, --runs, or --stop.
func runCommandRequested(o opts) bool { return o.Attach || o.Runs || o.Stop }

// validateRunCommandFlags keeps --attach, --runs, and --stop standalone: one of them at a time, with
// no execution, close-out, or utility flag, and with their positional argument as the run they mean.
// --attach takes at most one, --stop exactly one, and --runs none.
func validateRunCommandFlags(o opts) error {
	var requested []string
	for _, cmd := range []struct {
		flag string
		set  bool
	}{{"--attach", o.Attach}, {"--runs", o.Runs}, {"--stop", o.Stop}} {
		if cmd.set {
			requested = append(requested, cmd.flag)
		}
	}
	if len(requested) == 0 {
		return nil
	}
	flag := requested[0]
	if len(requested) > 1 {
		return fmt.Errorf("%s cannot be combined with %s", flag, requested[1])
	}
	// the positional argument names the run, so it does not count as an execution mode here
	bare := o
	bare.PlanFile = ""
	if hasExecutionMode(bare) || o.Clear || closeoutRequested(o) || o.detachedRunSet || o.DetachedRun != "" {
		return fmt.Errorf("%s cannot be combined with other mode flags", flag)
	}
	switch {
	case o.Runs && o.PlanFile != "":
		return fmt.Errorf("--runs takes no argument, got %d", len(o.extraArgs)+1)
	case len(o.extraArgs) > 0:
		return fmt.Errorf("%s accepts at most one run argument (a run id, plan, or branch), got %d", flag, len(o.extraArgs)+1)
	case o.Stop && strings.TrimSpace(o.PlanFile) == "":
		return errors.New("--stop requires the run to stop: a run id, plan, or branch (list them with loopai --runs)")
	}
	return nil
}

// attachDetachedRun is --attach: it validates the flags, loads config read-only for its colors, and
// follows the run o.PlanFile names, or the default run without one, until it ends. It returns an
// exitCodeError carrying the run's exit code when that is not 0, and nil after Ctrl+C.
func attachDetachedRun(ctx context.Context, o opts, stdout io.Writer) error {
	if err := validateFlags(o); err != nil {
		return err
	}
	cfg, err := config.LoadReadOnly(o.ConfigDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	root, err := runsRoot(cfg.VcsCommand)
	if err != nil {
		return err
	}
	run, err := resolveRun(root, o.PlanFile)
	if err != nil {
		return err
	}
	code, err := attachRun(ctx, root, run, stdout, progress.NewColors(cfg.Colors))
	if err != nil {
		return err
	}
	if code != 0 {
		return &exitCodeError{code: code}
	}
	return nil
}

// listDetachedRuns is --runs: one row per run of the checkout, newest first. vcsCommand is the
// configured vcs_command, which finds the checkout's root.
func listDetachedRuns(vcsCommand string, stdout io.Writer) error {
	root, err := runsRoot(vcsCommand)
	if err != nil {
		return err
	}
	all, err := runs.List(root)
	if err != nil {
		return fmt.Errorf("detached runs: %w", err)
	}
	return printRuns(stdout, root, all)
}

// stopDetachedRun is --stop: it stops the run ident names in the checkout vcsCommand finds through
// stopRun, then waits up to stopWaitTimeout for the run to end and prints the status it ended with, or,
// when it is still finishing, how to follow it.
func stopDetachedRun(ctx context.Context, vcsCommand, ident string, colors *progress.Colors, stdout io.Writer) error {
	root, err := runsRoot(vcsCommand)
	if err != nil {
		return err
	}
	run, err := resolveRun(root, ident)
	if err != nil {
		return err
	}
	if stopErr := stopRun(root, run.Descriptor); stopErr != nil {
		return stopErr
	}
	id := run.Descriptor.ID
	colors.Info().Fprintf(stdout, "stop requested: run %s\n", id)
	status, ended, err := awaitRunStop(ctx, root, run.Dir, stopWaitTimeout)
	if err != nil {
		return err
	}
	if !ended {
		fmt.Fprintf(stdout, "run %s is still finishing; follow it with loopai --attach %s\n", id, id)
		return nil
	}
	fmt.Fprintf(stdout, "run %s: %s\n", id, status)
	return nil
}

// runsRoot is the checkout whose runs --attach, --runs, and --stop see: the root of the repository or
// worktree holding the working directory, found with vcsCommand ("" is git), since the launcher runs
// from that root and records its runs there. A working directory outside a repository is refused.
func runsRoot(vcsCommand string) (string, error) {
	svc, err := git.NewService(".", nil, vcsCommand)
	if err != nil {
		return "", fmt.Errorf("detached runs are recorded per checkout; run this inside the repository: %w", err)
	}
	return svc.Root(), nil
}

// resolveRun finds the run ident names under root, a run id, plan, or branch, or without ident the
// default run: the single live one, else the newest.
func resolveRun(root, ident string) (runs.Run, error) {
	var run runs.Run
	var err error
	if ident == "" {
		run, err = runs.DefaultRun(root)
	} else {
		run, err = runs.Resolve(root, ident)
	}
	switch {
	case errors.Is(err, runs.ErrNotFound) && ident == "":
		return runs.Run{}, fmt.Errorf("%w in %s", err, root)
	case errors.Is(err, runs.ErrNotFound):
		return runs.Run{}, fmt.Errorf("%w in %s; list the runs with loopai --runs", err, root)
	case err != nil:
		return runs.Run{}, fmt.Errorf("find the detached run: %w", err)
	}
	return run, nil
}

// attachRun follows run under root on out: a header from its journal, then its console log replayed
// from the first byte, every attempt included, or only its last consoleReplayLimit bytes from a line
// start after a note naming the full log, and followed until the run is no longer live and the log is
// drained, then how the run ended. It returns the run's exit code: 0 succeeded, 2 stopped, and
// 1 failed or crashed. A canceled ctx detaches instead: the run is left untouched and the code is 0.
func attachRun(ctx context.Context, root string, run runs.Run, out io.Writer, colors *progress.Colors) (int, error) {
	id := run.Descriptor.ID
	snap, err := runSnapshot(run.Dir)
	if err != nil {
		return 0, fmt.Errorf("attach run %s: %w", id, err)
	}
	printAttachHeader(out, colors, run, snap)

	console := &consoleFollower{path: filepath.Join(run.Dir, runs.ConsoleLogFile), out: out}
	defer console.close()
	ticker := time.NewTicker(runsPoll)
	defer ticker.Stop()
	for {
		status, classifyErr := runs.Classify(root, run.Dir)
		if classifyErr != nil {
			return 0, fmt.Errorf("attach run %s: %w", id, classifyErr)
		}
		// read after the status: a run that is no longer live wrote all its output before it ended
		if copyErr := console.copy(ctx); copyErr != nil {
			return 0, fmt.Errorf("attach run %s: %w", id, copyErr)
		}
		if ctx.Err() == nil && !status.Live() {
			console.endLine()
			printAttachOutcome(out, colors, run, status)
			return detachExitCode(status), nil
		}
		select {
		case <-ctx.Done():
			console.endLine()
			fmt.Fprintf(out, "detached; the run continues: loopai --attach %s\n", id)
			return 0, nil
		case <-ticker.C:
		}
	}
}

// runSnapshot folds the journal of the run in dir.
func runSnapshot(dir string) (runs.Snapshot, error) {
	events, err := runs.ReadFrom(dir, 0)
	if err != nil {
		return runs.Snapshot{}, fmt.Errorf("read the run journal: %w", err)
	}
	return runs.Fold(events), nil
}

// printAttachHeader prints the run's id, attempt, and status, with the phase and task its journal
// recorded for the current attempt.
func printAttachHeader(out io.Writer, colors *progress.Colors, run runs.Run, snap runs.Snapshot) {
	desc := run.Descriptor
	parts := []string{string(run.Status)}
	if snap.Attempt == desc.Attempt {
		if snap.Phase != "" {
			parts = append(parts, "phase "+string(snap.Phase))
		}
		if snap.Task > 0 {
			parts = append(parts, fmt.Sprintf("task %d/%d", snap.Task, snap.TaskTotal))
		}
	}
	colors.Info().Fprintf(out, "loopai run %s, attempt %d: %s\n", desc.ID, desc.Attempt, strings.Join(parts, ", "))
}

// printAttachOutcome prints how the run's current attempt ended: its status, and the reason, branch,
// commit, report, pull request, and kept worktree its outcome records. Without an outcome, the reason
// comes from the attempt's finish event, the branch from the descriptor, and the kept worktree is the
// descriptor's when it is still in place.
func printAttachOutcome(out io.Writer, colors *progress.Colors, run runs.Run, status runs.Status) {
	desc := run.Descriptor
	if current, err := runs.LoadDescriptor(run.Dir); err == nil {
		desc = current
	}
	outcome, err := runs.LoadOutcome(run.Dir)
	if err != nil || outcome.Attempt != desc.Attempt {
		outcome = runs.Outcome{Branch: desc.Branch, Reason: missingOutcomeReason(run.Dir, desc.Attempt, status)}
	}
	statusColor(colors, status).Fprintf(out, "=== loopai run %s, attempt %d finished ===\n", desc.ID, desc.Attempt)
	for _, field := range []struct{ name, value string }{
		{"status", string(status)},
		{"reason", outcome.Reason},
		{"branch", outcome.Branch},
		{"commit", outcome.Commit},
		{"report", reportLocation(run.Dir, outcome.Branch, outcome.PlanMoved)},
		{"pr", outcome.Finalize.PRURL},
		{"worktree kept", runs.KeptWorktree(runs.Run{Dir: run.Dir, Descriptor: desc})},
	} {
		if field.value != "" {
			fmt.Fprintf(out, "%s: %s\n", field.name, field.value)
		}
	}
}

// missingOutcomeReason is the reason shown for an attempt that ended without an outcome: the one its
// finish event recorded, else what the status says about how it ended.
func missingOutcomeReason(dir string, attempt int, status runs.Status) string {
	if snap, err := runSnapshot(dir); err == nil && snap.Attempt == attempt && snap.Finish != nil && snap.Finish.Reason != "" {
		return snap.Finish.Reason
	}
	switch status {
	case runs.StatusStopped:
		return "stopped before the run recorded an outcome"
	case runs.StatusCrashed:
		return "the run process ended without recording an outcome"
	default:
		return ""
	}
}

// reportLocation is where the run's report can be read: report.md in its run directory and, once the
// plan was archived with its report sidecar on the run branch, loopai --report <branch>; "" when the
// run wrote no report.
func reportLocation(dir, branch string, planMoved bool) string {
	path := filepath.Join(dir, runs.ReportFile)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	if planMoved && branch != "" {
		return path + ", or loopai --report " + branch
	}
	return path
}

// statusColor is the color of a finished run's status line.
func statusColor(colors *progress.Colors, status runs.Status) *color.Color {
	switch status {
	case runs.StatusSucceeded:
		return colors.Info()
	case runs.StatusStopped:
		return colors.Warn()
	default:
		return colors.Error()
	}
}

// consoleFollower copies a run's console log to out as it grows: from its first byte on, or, for a log
// longer than consoleReplayLimit when first opened, from the first line start within its last
// consoleReplayLimit bytes, after a note that names the full log.
type consoleFollower struct {
	path    string
	out     io.Writer
	file    *os.File // nil until the log exists
	buf     []byte
	midLine bool // the last byte written was not a newline
}

// copy writes what the log gained since the last call, in chunks between which a canceled ctx stops
// it. A log that does not exist yet, as before the launcher writes the attempt's first line, has
// nothing to copy.
func (f *consoleFollower) copy(ctx context.Context) error {
	if f.file == nil {
		file, err := os.Open(f.path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("open console log: %w", err)
		}
		f.file, f.buf = file, make([]byte, consoleChunk)
		if err := f.skipToTail(); err != nil {
			return err
		}
	}
	for ctx.Err() == nil {
		n, err := f.file.Read(f.buf)
		if n > 0 {
			if _, writeErr := f.out.Write(f.buf[:n]); writeErr != nil {
				return fmt.Errorf("write console output: %w", writeErr)
			}
			f.midLine = f.buf[n-1] != '\n'
		}
		if errors.Is(err, io.EOF) || (err == nil && n == 0) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read console log: %w", err)
		}
	}
	return nil
}

// skipToTail moves past the start of a log longer than consoleReplayLimit, to the first line start
// within its last consoleReplayLimit bytes, and notes the omission with the full log's path. A tail
// holding no line start begins mid-line at the limit.
func (f *consoleFollower) skipToTail() error {
	info, err := f.file.Stat()
	if err != nil {
		return fmt.Errorf("read console log: %w", err)
	}
	if info.Size() <= consoleReplayLimit {
		return nil
	}
	// one byte before the window tells whether the window already starts a line
	from := info.Size() - consoleReplayLimit - 1
	window := make([]byte, consoleReplayLimit+1)
	n, err := f.file.ReadAt(window, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read console log: %w", err)
	}
	start := from + 1
	if i := bytes.IndexByte(window[:n], '\n'); i >= 0 && i < n-1 {
		start = from + int64(i) + 1
	}
	if _, err := f.file.Seek(start, io.SeekStart); err != nil {
		return fmt.Errorf("read console log: %w", err)
	}
	if _, err := fmt.Fprintf(f.out, "(earlier output omitted; full log: %s)\n", f.path); err != nil {
		return fmt.Errorf("write console output: %w", err)
	}
	return nil
}

// endLine ends a line the log left open, so what attach prints next starts on a line of its own.
func (f *consoleFollower) endLine() {
	if f.midLine {
		fmt.Fprintln(f.out)
		f.midLine = false
	}
}

// close closes the log once it was opened.
func (f *consoleFollower) close() {
	if f.file != nil {
		_ = f.file.Close()
	}
}

// printRuns writes one row per run of root, newest first as runs.List returns them, or a line saying
// there are none.
func printRuns(out io.Writer, root string, all []runs.Run) error {
	if len(all) == 0 {
		fmt.Fprintf(out, "no detached runs in %s\n", root)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTATUS\tATTEMPT\tORIGIN\tPLAN\tBRANCH\tSTARTED")
	for _, run := range all {
		d := run.Descriptor
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", d.ID, run.Status, d.Attempt, originLabel(d.Origin),
			orDash(d.Plan), orDash(d.Branch), d.CreatedAt.Local().Format(runsTimeLayout))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write the runs: %w", err)
	}
	return nil
}

// originLabel is how --runs shows who started a run: cli, t3-terminal:<thread>, or t3-thread:<thread>.
func originLabel(o runs.Origin) string {
	if o.ThreadID == "" {
		return orDash(o.Kind)
	}
	return o.Kind + ":" + o.ThreadID
}

// orDash is s, or "-" in place of an empty table cell.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stopRun asks the live run desc names under root to stop. The run directory's descriptor is read
// again, since a caller's copy can predate the attempt now running. Only when the run's lifetime lock
// is held and the process the descriptor records still carries its recorded start token does stopRun
// write the stop marker and then send SIGTERM to that process alone, never its group. The marker comes
// first, so a run that its own force exit kills before it records an outcome classifies as stopped
// rather than crashed. A run that is not running, or whose pid was reused, is refused without a signal,
// and a signal that fails removes the marker again. stopRun does not wait for the run to end.
func stopRun(root string, desc runs.Descriptor) error {
	id := desc.ID
	if !runs.ValidID(id) {
		return fmt.Errorf("stop run: invalid run id %q", id)
	}
	dir := runs.Dir(root, id)
	current, err := runs.LoadDescriptor(dir)
	if err != nil {
		return fmt.Errorf("stop run %s: %w", id, err)
	}
	held, err := runs.Probe(filepath.Join(dir, runs.LockFile))
	if err != nil {
		return fmt.Errorf("stop run %s: %w", id, err)
	}
	if !held {
		return fmt.Errorf("run %s is not running (%s); there is nothing to stop", id, statusText(root, dir))
	}
	if processErr := checkRunProcess(current); processErr != nil {
		return processErr
	}
	if markErr := runs.RequestStop(dir); markErr != nil {
		return fmt.Errorf("stop run %s: %w", id, markErr)
	}
	if signalErr := signalRun(current.PID); signalErr != nil {
		// the run was not asked to stop, so the marker must not make a later failure or crash read as
		// stopped
		return fmt.Errorf("stop run %s: %w", id, errors.Join(signalErr, runs.ClearStop(dir)))
	}
	return nil
}

// checkRunProcess verifies that the process desc records is the one the run started: a process whose
// start token differs, or that is gone, only reuses the pid.
func checkRunProcess(desc runs.Descriptor) error {
	if desc.PID <= 0 || desc.PIDStart == "" {
		return fmt.Errorf("refusing to stop run %s: its attempt has not recorded its process yet; try again in a moment", desc.ID)
	}
	if token, err := runs.ProcessStartToken(desc.PID); err != nil || token != desc.PIDStart {
		return fmt.Errorf("refusing to stop run %s: process %d is not the one the run recorded (the pid was reused)",
			desc.ID, desc.PID)
	}
	return nil
}

// statusText is the status of the run in dir for a message, or "status unknown" when it cannot be read.
func statusText(root, dir string) string {
	status, err := runs.Classify(root, dir)
	if err != nil {
		return "status unknown"
	}
	return string(status)
}

// awaitRunStop waits at most timeout, and no longer than ctx lives, for the run in dir to release its
// lifetime lock, and returns the status the run then has with ended set; ended is false when the lock
// is still held.
func awaitRunStop(ctx context.Context, root, dir string, timeout time.Duration) (runs.Status, bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(runsPoll)
	defer ticker.Stop()
	for {
		held, err := runs.Probe(filepath.Join(dir, runs.LockFile))
		if err != nil {
			return "", false, fmt.Errorf("the stop was requested, but the run's lock cannot be read: %w", err)
		}
		if !held {
			status, classifyErr := runs.Classify(root, dir)
			if classifyErr != nil {
				return "", false, fmt.Errorf("the stop was requested, but the run's status cannot be read: %w", classifyErr)
			}
			return status, true, nil
		}
		select {
		case <-ctx.Done():
			return "", false, nil
		case <-deadline.C:
			return "", false, nil
		case <-ticker.C:
		}
	}
}
