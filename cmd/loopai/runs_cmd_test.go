//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fatih/color"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
)

const (
	// runsHolderEnv makes the test binary hold a run's lifetime lock as a live run does; it holds the
	// run directory and a scratch directory, separated by a unit separator.
	runsHolderEnv = "LOOPAI_TEST_RUNS_HOLDER"
	// runsWait bounds every wait on a helper process; spawns are slow under the race-enabled suite.
	runsWait = 30 * time.Second
)

// runsEpoch is the creation time the run fixtures count from.
var runsEpoch = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// TestRunsHolderHelper is the live run the --attach and --stop tests start, not a test of its own.
func TestRunsHolderHelper(t *testing.T) {
	raw, ok := os.LookupEnv(runsHolderEnv)
	if !ok {
		t.Skip("helper process only")
	}
	dir, scratch, _ := strings.Cut(raw, "\x1f")
	os.Exit(runRunsHolder(dir, scratch))
}

// runRunsHolder is a live run: it takes the lifetime lock of the run in dir and records its process in
// the descriptor, appends "holder: line 1" to the console log, and creates <scratch>/ready. Once
// <scratch>/gate exists it appends "holder: line 2", records a succeeded outcome, and exits 0. SIGTERM
// ends it at once with exit code 1 and no outcome, as the force exit ends a run. It returns the exit
// code.
func runRunsHolder(dir, scratch string) int {
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	release, err := runs.AcquireBounded(filepath.Join(dir, runs.LockFile), 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		return 4
	}
	defer func() { _ = release() }()
	desc, err := runs.LoadDescriptor(dir)
	if err == nil {
		desc.PID = os.Getpid()
		desc.PIDStart, err = runs.ProcessStartToken(desc.PID)
	}
	if err == nil {
		err = runs.SaveDescriptor(dir, &desc)
	}
	if err == nil {
		err = appendRunsConsole(dir, "holder: line 1\n")
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(scratch, "ready"), nil, 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		return 4
	}
	for range 1200 {
		if _, statErr := os.Stat(filepath.Join(scratch, "gate")); statErr == nil {
			break
		}
		select {
		case <-terms:
			return 1
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := appendRunsConsole(dir, "holder: line 2\n"); err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		return 4
	}
	outcome := runs.Outcome{RunID: desc.ID, Attempt: desc.Attempt, Status: runs.StatusSucceeded, Branch: desc.Branch,
		Commit: "c0ffee", FinishedAt: time.Now().UTC()}
	if err := runs.SaveOutcome(dir, &outcome); err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)
		return 4
	}
	return 0
}

// appendRunsConsole appends text to the console log of the run in dir.
func appendRunsConsole(dir, text string) error {
	f, err := os.OpenFile(filepath.Join(dir, runs.ConsoleLogFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // test path
	if err != nil {
		return fmt.Errorf("open console log: %w", err)
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return fmt.Errorf("write console log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close console log: %w", err)
	}
	return nil
}

// runsHolder is a started runRunsHolder process.
type runsHolder struct {
	cmd     *exec.Cmd
	scratch string
	done    chan struct{}
	waitErr error
}

// startRunsHolder starts a holder of the run in dir and waits until it holds the lock and wrote its
// first line. At cleanup its gate is opened and it is waited for; it is killed only when it does not exit.
func startRunsHolder(t *testing.T, dir string) *runsHolder {
	t.Helper()
	h := &runsHolder{scratch: t.TempDir(), done: make(chan struct{})}
	h.cmd = exec.Command(os.Args[0], "-test.run=^TestRunsHolderHelper$")
	h.cmd.Env = append(os.Environ(), runsHolderEnv+"="+dir+"\x1f"+h.scratch)
	var stderr bytes.Buffer
	h.cmd.Stderr = &stderr
	require.NoError(t, h.cmd.Start())
	go func() {
		h.waitErr = h.cmd.Wait()
		close(h.done)
	}()
	t.Cleanup(func() {
		h.open(t)
		select {
		case <-h.done:
		case <-time.After(runsWait):
			_ = h.cmd.Process.Kill()
			<-h.done
		}
	})
	deadline := time.Now().Add(runsWait)
	for {
		if _, err := os.Stat(filepath.Join(h.scratch, "ready")); err == nil {
			return h
		}
		select {
		case <-h.done:
			t.Fatalf("the holder exited before it was ready: %v; stderr:\n%s", h.waitErr, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// open lets the holder finish its run.
func (h *runsHolder) open(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(h.scratch, "gate"), nil, 0o600))
}

// alive reports whether the holder is still running.
func (h *runsHolder) alive() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// wait waits for the holder to exit and returns its exit code. A holder ended by a signal fails.
func (h *runsHolder) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(runsWait):
		t.Fatal("the holder did not exit")
	}
	if h.waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, h.waitErr, &exitErr)
	require.True(t, exitErr.Exited(), "the holder exited by itself: %v", h.waitErr)
	return exitErr.ExitCode()
}

// runsTestRepo returns an empty Git repository in a temp directory, the checkout --runs, --attach,
// and --stop resolve their runs root in.
func runsTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	return root
}

// saveRunsTestRun records the run desc describes under root, with attempt 1, mode full, and origin cli
// unless desc sets them, and returns its directory.
func saveRunsTestRun(t *testing.T, root string, desc runs.Descriptor) string {
	t.Helper()
	dir := runs.Dir(root, desc.ID)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	if desc.Attempt == 0 {
		desc.Attempt = 1
	}
	if desc.Mode == "" {
		desc.Mode = "full"
	}
	if desc.Origin.Kind == "" {
		desc.Origin.Kind = runs.OriginCLI
	}
	desc.Cwd = root
	desc.ConsoleLog = filepath.Join(dir, runs.ConsoleLogFile)
	require.NoError(t, runs.SaveDescriptor(dir, &desc))
	return dir
}

// saveRunsTestOutcome records an outcome with status for the current attempt of the run in dir.
func saveRunsTestOutcome(t *testing.T, dir string, outcome runs.Outcome) {
	t.Helper()
	desc, err := runs.LoadDescriptor(dir)
	require.NoError(t, err)
	outcome.RunID, outcome.Attempt = desc.ID, desc.Attempt
	require.NoError(t, runs.SaveOutcome(dir, &outcome))
}

// appendRunsTestEvents appends events to the journal of the run in dir for attempt.
func appendRunsTestEvents(t *testing.T, dir string, attempt int, events ...runs.Event) {
	t.Helper()
	j, err := runs.OpenJournal(dir, attempt)
	require.NoError(t, err)
	for _, ev := range events {
		_, err := j.Append(ev)
		require.NoError(t, err)
	}
	require.NoError(t, j.Close())
}

// holdRunsTestLock holds the lifetime lock of the run in dir from this process until the returned
// release is called or the test ends; a probe from this process reads it as held too.
func holdRunsTestLock(t *testing.T, dir string) (release func()) {
	t.Helper()
	unlock, err := runs.AcquireBounded(filepath.Join(dir, runs.LockFile), time.Second)
	require.NoError(t, err)
	var once sync.Once
	release = func() { once.Do(func() { require.NoError(t, unlock()) }) }
	t.Cleanup(release)
	return release
}

// recordRunSignals replaces signalRun for the test with a recorder of the pids it is asked to signal.
func recordRunSignals(t *testing.T) *[]int {
	t.Helper()
	var pids []int
	signalRun = func(pid int) error {
		pids = append(pids, pid)
		return nil
	}
	t.Cleanup(func() { signalRun = signalTerm })
	return &pids
}

// plainColors turns color output off for the test, so printed lines can be compared exactly.
func plainColors(t *testing.T) {
	t.Helper()
	saved := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = saved })
}

// syncBuffer is a bytes.Buffer that a writer goroutine and a reading test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestValidateRunCommandFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string // empty when the flags are valid
	}{
		{name: "attach the default run", args: []string{"--attach"}},
		{name: "attach a run", args: []string{"--attach", "feature-20261022-120000-abcd"}},
		{name: "attach a plan", args: []string{"--attach", "docs/plans/x.md"}},
		{name: "attach with output flags", args: []string{"--attach", "--debug", "--no-color", "--config-dir", "d", "x"}},
		{name: "runs", args: []string{"--runs"}},
		{name: "stop a run", args: []string{"--stop", "feature"}},
		{name: "attach with detach", args: []string{"--attach", "--detach"},
			wantErr: "--attach cannot be combined with other mode flags"},
		{name: "attach with detach and a plan", args: []string{"--attach", "--detach", "docs/plans/x.md"},
			wantErr: "--attach cannot be combined with other mode flags"},
		{name: "runs with a plan", args: []string{"--runs", "plan.md"}, wantErr: "--runs takes no argument, got 1"},
		{name: "runs with two arguments", args: []string{"--runs", "a", "b"}, wantErr: "--runs takes no argument, got 2"},
		{name: "stop without a run", args: []string{"--stop"},
			wantErr: "--stop requires the run to stop: a run id, plan, or branch (list them with loopai --runs)"},
		{name: "stop with a blank run", args: []string{"--stop", " "}, wantErr: "--stop requires the run to stop"},
		{name: "attach with two runs", args: []string{"--attach", "a", "b"},
			wantErr: "--attach accepts at most one run argument (a run id, plan, or branch), got 2"},
		{name: "stop with two runs", args: []string{"--stop", "a", "b"}, wantErr: "--stop accepts at most one run argument"},
		{name: "attach and runs", args: []string{"--attach", "--runs"}, wantErr: "--attach cannot be combined with --runs"},
		{name: "runs and stop", args: []string{"--runs", "--stop", "x"}, wantErr: "--runs cannot be combined with --stop"},
		{name: "attach and stop", args: []string{"--stop", "x", "--attach"}, wantErr: "--attach cannot be combined with --stop"},
		{name: "attach with tasks-only", args: []string{"--attach", "--tasks-only"}, wantErr: "other mode flags"},
		{name: "runs with worktree", args: []string{"--runs", "--worktree"}, wantErr: "--runs cannot be combined with other mode flags"},
		{name: "stop with an explicit zero", args: []string{"--stop", "x", "--max-iterations=0"}, wantErr: "other mode flags"},
		{name: "stop with serve", args: []string{"--stop", "x", "--serve"}, wantErr: "other mode flags"},
		{name: "runs with init", args: []string{"--runs", "--init"}, wantErr: "other mode flags"},
		{name: "attach with merge", args: []string{"--attach", "--merge"}, wantErr: "--attach cannot be combined with other mode flags"},
		{name: "attach with report", args: []string{"--report", "--attach"}, wantErr: "--attach cannot be combined with other mode flags"},
		{name: "runs with clear", args: []string{"--runs", "--clear"}, wantErr: "--runs cannot be combined with other mode flags"},
		{name: "attach with the child's run id", args: []string{"--attach", "--detached-run=x-1"},
			wantErr: "--attach cannot be combined with other mode flags"},
		{name: "a removed flag names its replacement first", args: []string{"--attach", "--t3"}, wantErr: "--t3 was removed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFlags(parseTestOpts(t, tc.args...))
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestRunCommandIdentifiersAreNotSplit(t *testing.T) {
	for _, flag := range []string{"--attach", "--stop"} {
		t.Run(flag, func(t *testing.T) {
			o := parseTestOpts(t, flag, "feature,with-comma")
			assert.Equal(t, "feature,with-comma", o.PlanFile)
			assert.Nil(t, o.PlanFiles)
			require.NoError(t, validateFlags(o))
		})
	}
	t.Run("--runs rejects the argument whole", func(t *testing.T) {
		// split as a chain, the duplicate entry would fail parsing before validation could name the flag
		o := parseTestOpts(t, "--runs", "a.md,a.md")
		assert.Equal(t, "a.md,a.md", o.PlanFile)
		require.ErrorContains(t, validateFlags(o), "--runs takes no argument")
	})
}

func TestRunCommandsAreStandalone(t *testing.T) {
	for _, args := range [][]string{{"--attach"}, {"--runs"}, {"--stop", "x"}} {
		o := parseTestOpts(t, args...)
		assert.True(t, runCommandRequested(o), args)
		assert.True(t, isStandaloneCommand(o), "%v owns no cmux card and is never handed over", args)
		assert.False(t, o.executionModeSet, "%v is no execution flag", args)
	}
	assert.False(t, runCommandRequested(parseTestOpts(t, "docs/plans/x.md")))
}

func TestResolveRun(t *testing.T) {
	root := t.TempDir()
	older := saveRunsTestRun(t, root, runs.Descriptor{ID: "older-1", Plan: "docs/plans/older.md", Branch: "older",
		CreatedAt: runsEpoch})
	newer := saveRunsTestRun(t, root, runs.Descriptor{ID: "newer-1", Plan: "docs/plans/newer.md", Branch: "newer",
		CreatedAt: runsEpoch.Add(time.Hour)})
	resolvedID := func(ident string) string {
		t.Helper()
		run, err := resolveRun(root, ident)
		require.NoError(t, err)
		return run.Descriptor.ID
	}

	releaseOlder := holdRunsTestLock(t, older)
	assert.Equal(t, "older-1", resolvedID(""), "the single live run wins over a newer finished one")
	releaseNewer := holdRunsTestLock(t, newer)
	assert.Equal(t, "newer-1", resolvedID(""), "with two live runs, the newest")
	releaseOlder()
	releaseNewer()
	assert.Equal(t, "newer-1", resolvedID(""), "without a live run, the newest")
	assert.Equal(t, "older-1", resolvedID("older"), "an identifier names a run by id, plan, or branch")
	assert.Equal(t, "older-1", resolvedID("docs/plans/older.md"))

	_, err := resolveRun(root, "nope")
	require.ErrorIs(t, err, runs.ErrNotFound)
	require.EqualError(t, err, "no matching detached run: nope in "+root+"; list the runs with loopai --runs")

	empty := t.TempDir()
	_, err = resolveRun(empty, "")
	require.ErrorIs(t, err, runs.ErrNotFound)
	require.EqualError(t, err, "no matching detached run in "+empty)
}

func TestListDetachedRuns(t *testing.T) {
	root := runsTestRepo(t)
	t.Chdir(root)
	running := saveRunsTestRun(t, root, runs.Descriptor{ID: "a-running", Plan: "docs/plans/a.md", Branch: "a",
		CreatedAt: runsEpoch.Add(6 * time.Hour), Origin: runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: "thread:a%3Ab"}})
	holdRunsTestLock(t, running)
	saveRunsTestRun(t, root, runs.Descriptor{ID: "b-starting", Plan: "docs/plans/b.md", Branch: "b",
		CreatedAt: runsEpoch.Add(5 * time.Hour),
		Origin:    runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "8e8b69fc", ACPSessionID: "sess_1"}})
	releaseLaunch, err := runs.AcquireLaunchLock(root, "docs/plans/b.md", "b", time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = releaseLaunch() })
	succeeded := saveRunsTestRun(t, root, runs.Descriptor{ID: "c-succeeded", Plan: "docs/plans/c.md", Branch: "c",
		CreatedAt: runsEpoch.Add(4 * time.Hour)})
	saveRunsTestOutcome(t, succeeded, runs.Outcome{Status: runs.StatusSucceeded})
	failed := saveRunsTestRun(t, root, runs.Descriptor{ID: "d-failed", Attempt: 3, Plan: "docs/plans/d.md", Branch: "d",
		CreatedAt: runsEpoch.Add(3 * time.Hour)})
	saveRunsTestOutcome(t, failed, runs.Outcome{Status: runs.StatusFailed})
	stopped := saveRunsTestRun(t, root, runs.Descriptor{ID: "e-stopped", Plan: "docs/plans/e.md", Branch: "e",
		CreatedAt: runsEpoch.Add(2 * time.Hour)})
	require.NoError(t, runs.RequestStop(stopped))
	saveRunsTestRun(t, root, runs.Descriptor{ID: "f-crashed", Plan: "docs/plans/f.md", CreatedAt: runsEpoch.Add(time.Hour)})

	var out bytes.Buffer
	require.NoError(t, listDetachedRuns("", &out))
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	require.Len(t, lines, 7, out.String())
	assert.Equal(t, []string{"RUN", "STATUS", "ATTEMPT", "ORIGIN", "PLAN", "BRANCH", "STARTED"}, strings.Fields(lines[0]))
	started := func(offset time.Duration) []string {
		return strings.Fields(runsEpoch.Add(offset).Local().Format(runsTimeLayout))
	}
	want := [][]string{
		append([]string{"a-running", "running", "1", "t3-terminal:thread:a%3Ab", "docs/plans/a.md", "a"}, started(6*time.Hour)...),
		append([]string{"b-starting", "starting", "1", "t3-thread:8e8b69fc", "docs/plans/b.md", "b"}, started(5*time.Hour)...),
		append([]string{"c-succeeded", "succeeded", "1", "cli", "docs/plans/c.md", "c"}, started(4*time.Hour)...),
		append([]string{"d-failed", "failed", "3", "cli", "docs/plans/d.md", "d"}, started(3*time.Hour)...),
		append([]string{"e-stopped", "stopped", "1", "cli", "docs/plans/e.md", "e"}, started(2*time.Hour)...),
		append([]string{"f-crashed", "crashed", "1", "cli", "docs/plans/f.md", "-"}, started(time.Hour)...),
	}
	for i, row := range want {
		assert.Equal(t, row, strings.Fields(lines[i+1]), "row %d, newest first", i+1)
		assert.Equal(t, strings.Index(lines[0], " STATUS")+1, strings.Index(lines[i+1], " "+row[1])+1, "aligned columns")
	}

	t.Run("a subdirectory of the checkout", func(t *testing.T) {
		sub := filepath.Join(root, "docs", "plans")
		require.NoError(t, os.MkdirAll(sub, 0o750))
		t.Chdir(sub)
		var fromSub bytes.Buffer
		require.NoError(t, listDetachedRuns("", &fromSub))
		assert.Equal(t, out.String(), fromSub.String(), "the runs of the repository root")
	})

	t.Run("a checkout without runs", func(t *testing.T) {
		empty := runsTestRepo(t)
		t.Chdir(empty)
		var out bytes.Buffer
		require.NoError(t, listDetachedRuns("", &out))
		assert.Equal(t, "no detached runs in "+empty+"\n", out.String())
	})

	t.Run("outside a repository", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := listDetachedRuns("", io.Discard)
		require.ErrorContains(t, err, "detached runs are recorded per checkout; run this inside the repository")
	})

	t.Run("a vcs command that fails", func(t *testing.T) {
		t.Chdir(root)
		err := listDetachedRuns(filepath.Join(t.TempDir(), "no-such-vcs"), io.Discard)
		require.ErrorContains(t, err, "run this inside the repository")
	})

	t.Run("a run that cannot be read", func(t *testing.T) {
		broken := runsTestRepo(t)
		t.Chdir(broken)
		dir := runs.Dir(broken, "x-1")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, runs.DescriptorFile), []byte("{"), 0o600))
		require.ErrorContains(t, listDetachedRuns("", io.Discard), "run x-1 cannot be read")
	})
}

func TestOriginLabel(t *testing.T) {
	assert.Equal(t, "cli", originLabel(runs.Origin{Kind: runs.OriginCLI}))
	assert.Equal(t, "t3-terminal:thread-1", originLabel(runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: "thread-1"}))
	assert.Equal(t, "t3-thread:thread-2", originLabel(runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread-2",
		ACPSessionID: "sess"}))
	assert.Equal(t, "-", originLabel(runs.Origin{}))
}

func TestStopRunRefuses(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, root string) runs.Descriptor
		wantErr string
	}{
		{name: "a finished run", setup: func(t *testing.T, root string) runs.Descriptor {
			dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "finished-1", Plan: "docs/plans/f.md", Branch: "f"})
			saveRunsTestOutcome(t, dir, runs.Outcome{Status: runs.StatusFailed})
			return runs.Descriptor{ID: "finished-1"}
		}, wantErr: "run finished-1 is not running (failed); there is nothing to stop"},
		{name: "a starting run", setup: func(t *testing.T, root string) runs.Descriptor {
			saveRunsTestRun(t, root, runs.Descriptor{ID: "starting-1", Plan: "docs/plans/s.md", Branch: "s"})
			release, err := runs.AcquireLaunchLock(root, "docs/plans/s.md", "s", time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = release() })
			return runs.Descriptor{ID: "starting-1"}
		}, wantErr: "run starting-1 is not running (starting)"},
		{name: "an unknown id", setup: func(*testing.T, string) runs.Descriptor {
			return runs.Descriptor{ID: "unknown-1"}
		}, wantErr: "stop run unknown-1: load run descriptor"},
		{name: "an id that names no run directory", setup: func(*testing.T, string) runs.Descriptor {
			return runs.Descriptor{ID: "../escape"}
		}, wantErr: `stop run: invalid run id "../escape"`},
		{name: "a reused pid", setup: func(t *testing.T, root string) runs.Descriptor {
			// the lock is held, but the recorded process started at another time than this one
			dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "reused-1", PID: os.Getpid(), PIDStart: "1.000000"})
			holdRunsTestLock(t, dir)
			return runs.Descriptor{ID: "reused-1"}
		}, wantErr: fmt.Sprintf("refusing to stop run reused-1: process %d is not the one the run recorded (the pid was reused)",
			os.Getpid())},
		{name: "a process not recorded yet", setup: func(t *testing.T, root string) runs.Descriptor {
			dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "fresh-1"})
			holdRunsTestLock(t, dir)
			return runs.Descriptor{ID: "fresh-1"}
		}, wantErr: "refusing to stop run fresh-1: its attempt has not recorded its process yet"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			signals := recordRunSignals(t)
			root := t.TempDir()
			desc := tc.setup(t, root)
			require.ErrorContains(t, stopRun(root, desc), tc.wantErr)
			assert.Empty(t, *signals, "no signal is sent")
			if runs.ValidID(desc.ID) {
				assert.NoFileExists(t, filepath.Join(runs.Dir(root, desc.ID), runs.StopFile), "no stop marker is written")
			}
		})
	}
}

func TestStopDetachedRunUnknown(t *testing.T) {
	signals := recordRunSignals(t)
	root := runsTestRepo(t)
	t.Chdir(root)
	saveRunsTestRun(t, root, runs.Descriptor{ID: "other-1", Plan: "docs/plans/other.md", Branch: "other"})
	err := stopDetachedRun(t.Context(), "", "nope", testColors(), io.Discard)
	require.ErrorIs(t, err, runs.ErrNotFound)
	require.ErrorContains(t, err, "no matching detached run: nope in "+root)
	assert.Empty(t, *signals)
}

func TestStopDetachedRunMarksBeforeSignal(t *testing.T) {
	plainColors(t)
	root := runsTestRepo(t)
	t.Chdir(root)
	pid := os.Getpid()
	token, err := runs.ProcessStartToken(pid)
	require.NoError(t, err)
	// the run's lock is held and the descriptor records this very process, so every check passes
	dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "live-1", PID: pid, PIDStart: token})
	holdRunsTestLock(t, dir)
	var signaled []int
	markedFirst := false
	signalRun = func(p int) error {
		signaled = append(signaled, p)
		markedFirst, _ = runs.StopRequested(dir)
		return nil
	}
	t.Cleanup(func() { signalRun = signalTerm })
	saved := stopWaitTimeout
	t.Cleanup(func() { stopWaitTimeout = saved })
	const want = "stop requested: run live-1\nrun live-1 is still finishing; follow it with loopai --attach live-1\n"

	t.Run("a run that does not end within the wait", func(t *testing.T) {
		stopWaitTimeout = 3 * runsPoll
		var out bytes.Buffer
		require.NoError(t, stopDetachedRun(t.Context(), "", "live-1", testColors(), &out))
		assert.Equal(t, []int{pid}, signaled, "the recorded pid alone is signaled")
		assert.True(t, markedFirst, "the stop marker is written before the signal")
		assert.Equal(t, want, out.String())
	})

	t.Run("an interrupted wait", func(t *testing.T) {
		stopWaitTimeout = time.Minute
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var out bytes.Buffer
		start := time.Now()
		require.NoError(t, stopDetachedRun(ctx, "", "live-1", testColors(), &out))
		assert.Less(t, time.Since(start), 10*time.Second, "the wait ends with the context")
		assert.Equal(t, want, out.String())
	})
}

func TestStopForceExitedRunIsStopped(t *testing.T) {
	plainColors(t)
	root := runsTestRepo(t)
	t.Chdir(root)
	dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "holder-1", Plan: "docs/plans/h.md", Branch: "h"})
	h := startRunsHolder(t, dir)

	var out bytes.Buffer
	require.NoError(t, stopDetachedRun(t.Context(), "", "holder-1", testColors(), &out))
	assert.Equal(t, "stop requested: run holder-1\nrun holder-1: stopped\n", out.String())
	assert.Equal(t, 1, h.wait(t), "the holder exits on SIGTERM the way the force exit ends a run")
	assert.NoFileExists(t, filepath.Join(dir, runs.OutcomeFile), "the run recorded no outcome")
	st, err := runs.Classify(root, dir)
	require.NoError(t, err)
	assert.Equal(t, runs.StatusStopped, st, "the stop marker makes the run stopped, not crashed")
}

// attachResult is what an attachRun goroutine returned.
type attachResult struct {
	code int
	err  error
}

// startAttach runs attachRun on run id under root in a goroutine, writing to out.
func startAttach(ctx context.Context, t *testing.T, root, id string, out *syncBuffer) <-chan attachResult {
	t.Helper()
	run, err := runs.Resolve(root, id)
	require.NoError(t, err)
	done := make(chan attachResult, 1)
	go func() {
		code, err := attachRun(ctx, root, run, out, testColors())
		done <- attachResult{code, err}
	}()
	return done
}

func TestAttachRunFollowsLiveRun(t *testing.T) {
	plainColors(t)
	root := t.TempDir()
	dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "live-1", Plan: "docs/plans/live.md", Branch: "live"})
	h := startRunsHolder(t, dir)

	var out syncBuffer
	done := startAttach(t.Context(), t, root, "live-1", &out)
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "holder: line 1\n") }, runsWait,
		20*time.Millisecond, "the console log is replayed")
	select {
	case res := <-done:
		t.Fatalf("attach returned while the run was live: %+v", res)
	case <-time.After(3 * runsPoll):
	}

	h.open(t)
	select {
	case res := <-done:
		require.NoError(t, res.err)
		assert.Equal(t, 0, res.code)
	case <-time.After(runsWait):
		t.Fatal("attach did not return after the run ended")
	}
	assert.Equal(t, 0, h.wait(t))
	assert.Equal(t, "loopai run live-1, attempt 1: running\n"+
		"holder: line 1\nholder: line 2\n"+
		"=== loopai run live-1, attempt 1 finished ===\nstatus: succeeded\nbranch: live\ncommit: c0ffee\n", out.String(),
		"the line appended after attaching is followed, and the outcome follows the drained log")
}

func TestAttachRunCanceled(t *testing.T) {
	plainColors(t)
	root := t.TempDir()
	dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "live-1", Plan: "docs/plans/live.md", Branch: "live"})
	h := startRunsHolder(t, dir)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out syncBuffer
	done := startAttach(ctx, t, root, "live-1", &out)
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "holder: line 1\n") }, runsWait,
		20*time.Millisecond)

	cancel()
	start := time.Now()
	select {
	case res := <-done:
		require.NoError(t, res.err)
		assert.Equal(t, 0, res.code)
	case <-time.After(runsWait):
		t.Fatal("attach did not return on cancel")
	}
	assert.Less(t, time.Since(start), 3*time.Second, "attach detaches at once")
	assert.True(t, strings.HasSuffix(out.String(), "holder: line 1\ndetached; the run continues: loopai --attach live-1\n"),
		out.String())

	assert.True(t, h.alive(), "the run is left running")
	stop, err := runs.StopRequested(dir)
	require.NoError(t, err)
	assert.False(t, stop, "no stop is requested")
	st, err := runs.Classify(root, dir)
	require.NoError(t, err)
	assert.Equal(t, runs.StatusRunning, st)
}

func TestAttachRunFinishedRun(t *testing.T) {
	plainColors(t)
	const id = "feature-1"
	kept := t.TempDir()
	tests := []struct {
		name     string
		setup    func(t *testing.T, dir string)
		wantCode int
		want     string // what follows the console log
	}{
		{name: "failed with a full outcome", setup: func(t *testing.T, dir string) {
			saveRunsTestOutcome(t, dir, runs.Outcome{Status: runs.StatusFailed, ExitCode: 1, Reason: "runner: task 2: boom",
				Branch: "feature", Commit: "abc123", WorktreeKept: kept, PlanMoved: true,
				Finalize: runs.Finalize{PRURL: "https://github.com/o/r/pull/7"}})
			require.NoError(t, runs.WriteReport(dir, "# Report: Feature\n"))
		}, wantCode: 1, want: "status: failed\nreason: runner: task 2: boom\nbranch: feature\ncommit: abc123\n" +
			"report: {dir}/report.md, or loopai --report feature\npr: https://github.com/o/r/pull/7\nworktree kept: {kept}\n"},
		{name: "succeeded with a report that is not on the branch", setup: func(t *testing.T, dir string) {
			saveRunsTestOutcome(t, dir, runs.Outcome{Status: runs.StatusSucceeded, Branch: "feature", Commit: "def456"})
			require.NoError(t, runs.WriteReport(dir, "# Report: Feature\n"))
		}, wantCode: 0, want: "status: succeeded\nbranch: feature\ncommit: def456\nreport: {dir}/report.md\n"},
		{name: "stopped without an outcome", setup: func(t *testing.T, dir string) {
			require.NoError(t, runs.RequestStop(dir))
		}, wantCode: 2, want: "status: stopped\nreason: stopped before the run recorded an outcome\nbranch: feature\n" +
			"worktree kept: {kept}\n"},
		{name: "crashed after its finish event", setup: func(t *testing.T, dir string) {
			appendRunsTestEvents(t, dir, 2, runs.Event{Type: runs.EventFinish, Status: runs.StatusFailed,
				Reason: "runner: task 2: boom", ExitCode: 1})
		}, wantCode: 1, want: "status: crashed\nreason: runner: task 2: boom\nbranch: feature\nworktree kept: {kept}\n"},
		{name: "crashed with nothing recorded", setup: func(*testing.T, string) {},
			wantCode: 1, want: "status: crashed\nreason: the run process ended without recording an outcome\n" +
				"branch: feature\nworktree kept: {kept}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := saveRunsTestRun(t, root, runs.Descriptor{ID: id, Attempt: 2, Plan: "docs/plans/feature.md",
				Branch: "feature", Worktree: kept})
			// the log ends without a newline: the outcome still starts on a line of its own
			require.NoError(t, appendRunsConsole(dir, "attempt one\n=== attempt two ===\nlast line"))
			appendRunsTestEvents(t, dir, 2, runs.Event{Type: runs.EventAttemptStart, TaskTotal: 3},
				runs.Event{Type: runs.EventPhase, Phase: status.PhaseTask}, runs.Event{Type: runs.EventTask, N: 2, M: 3})
			tc.setup(t, dir)
			run, err := runs.Resolve(root, id)
			require.NoError(t, err)

			var out bytes.Buffer
			code, err := attachRun(t.Context(), root, run, &out, testColors())
			require.NoError(t, err)
			assert.Equal(t, tc.wantCode, code)
			want := strings.NewReplacer("{dir}", dir, "{kept}", kept).Replace(tc.want)
			assert.Equal(t, fmt.Sprintf("loopai run %s, attempt 2: %s, phase task, task 2/3\n", id, run.Status)+
				"attempt one\n=== attempt two ===\nlast line\n"+
				"=== loopai run "+id+", attempt 2 finished ===\n"+want, out.String())
		})
	}
}

func TestAttachRunHeaderOfAnAttemptNotStartedYet(t *testing.T) {
	plainColors(t)
	root := t.TempDir()
	// attempt 2 is recorded, but the journal still ends with attempt 1, whose phase and task are not shown
	dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "feature-1", Attempt: 2, Plan: "docs/plans/f.md", Branch: "f"})
	appendRunsTestEvents(t, dir, 1, runs.Event{Type: runs.EventAttemptStart, TaskTotal: 3},
		runs.Event{Type: runs.EventPhase, Phase: status.PhaseTask}, runs.Event{Type: runs.EventTask, N: 1, M: 3})
	run, err := runs.Resolve(root, "feature-1")
	require.NoError(t, err)
	var out bytes.Buffer
	printAttachHeader(&out, testColors(), run, mustRunSnapshot(t, dir))
	assert.Equal(t, "loopai run feature-1, attempt 2: crashed\n", out.String())
}

// mustRunSnapshot is runSnapshot of the run in dir.
func mustRunSnapshot(t *testing.T, dir string) runs.Snapshot {
	t.Helper()
	snap, err := runSnapshot(dir)
	require.NoError(t, err)
	return snap
}

func TestConsoleFollower(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	f := &consoleFollower{path: filepath.Join(dir, runs.ConsoleLogFile), out: &out}
	defer f.close()

	require.NoError(t, f.copy(t.Context()), "a log that does not exist yet has nothing to copy")
	assert.Empty(t, out.String())

	require.NoError(t, appendRunsConsole(dir, "one\n"))
	require.NoError(t, f.copy(t.Context()))
	require.NoError(t, appendRunsConsole(dir, "two"))
	require.NoError(t, f.copy(t.Context()))
	assert.Equal(t, "one\ntwo", out.String(), "each copy writes only what the log gained")
	f.endLine()
	f.endLine()
	assert.Equal(t, "one\ntwo\n", out.String(), "an open line is ended once")

	require.NoError(t, appendRunsConsole(dir, "three\n"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, f.copy(ctx))
	assert.Equal(t, "one\ntwo\n", out.String(), "a canceled copy writes nothing")
	require.NoError(t, f.copy(t.Context()))
	assert.Equal(t, "one\ntwo\nthree\n", out.String())

	t.Run("a write that fails", func(t *testing.T) {
		failing := &consoleFollower{path: filepath.Join(dir, runs.ConsoleLogFile), out: failingWriter{}}
		defer failing.close()
		require.ErrorContains(t, failing.copy(t.Context()), "write console output")
	})
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
