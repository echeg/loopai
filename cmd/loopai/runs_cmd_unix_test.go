//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/runs"
)

// runsMainEnv makes the test binary run main() with the unit-separator-delimited arguments it holds.
const runsMainEnv = "LOOPAI_TEST_RUNS_MAIN"

// TestRunsMainHelper runs main() for the --attach, --runs, and --stop tests, not a test of its own.
func TestRunsMainHelper(t *testing.T) {
	raw, ok := os.LookupEnv(runsMainEnv)
	if !ok {
		t.Skip("helper process only")
	}
	os.Args = append([]string{"loopai"}, strings.Split(raw, "\x1f")...)
	main()
	os.Exit(0) // main returns when the command succeeds
}

// runsMainCommand returns main() with args as a child process in dir, with HOME, USERPROFILE, and the
// config directory redirected to a temp directory.
func runsMainCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunsMainHelper$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), runsMainEnv+"="+strings.Join(args, "\x1f"),
		"HOME="+home, "USERPROFILE="+home, "LOOPAI_CONFIG_DIR="+filepath.Join(home, "config"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	return cmd
}

// runRunsMain runs main() with args in dir and returns its output and exit code.
func runRunsMain(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := runsMainCommand(t, dir, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	code = exitCode(t, cmd.Run())
	return out.String(), errOut.String(), code
}

// exitCode is the exit code of a finished child; one ended by a signal fails the test.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.True(t, exitErr.Exited(), "the command exited by itself: %v", err)
	return exitErr.ExitCode()
}

func TestAttachCommandReplaysFinishedRun(t *testing.T) {
	const id = "feature-20261022-120000-abcd"
	const console = "=== loopai run " + id + ", attempt 1, started 2026-10-22T12:00:00Z ===\n" +
		"loopai v1\nattempt one: task 1 failed\n" +
		"=== loopai run " + id + ", attempt 2, started 2026-10-22T13:00:00Z ===\n" +
		"loopai v1\nattempt two: done\n"
	tests := []struct {
		status   runs.Status
		wantCode int
	}{
		{status: runs.StatusSucceeded, wantCode: 0},
		{status: runs.StatusFailed, wantCode: 1},
		{status: runs.StatusStopped, wantCode: 2},
		{status: runs.StatusCrashed, wantCode: 1},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			root := t.TempDir()
			dir := saveRunsTestRun(t, root, runs.Descriptor{ID: id, Attempt: 2, Plan: "docs/plans/feature.md",
				Branch: "feature"})
			require.NoError(t, os.WriteFile(filepath.Join(dir, runs.ConsoleLogFile), []byte(console), 0o600))
			if tc.status != runs.StatusCrashed {
				saveRunsTestOutcome(t, dir, runs.Outcome{Status: tc.status, ExitCode: tc.wantCode, Branch: "feature"})
			}

			for _, args := range [][]string{{"--attach", id}, {"--attach"}} {
				stdout, stderr, code := runRunsMain(t, root, args...)
				assert.Equal(t, tc.wantCode, code, "%v: stderr:\n%s", args, stderr)
				assert.Contains(t, stdout, "loopai run "+id+", attempt 2: "+string(tc.status)+"\n", args)
				assert.Contains(t, stdout, console, "%v replays every attempt from the first byte", args)
				assert.Contains(t, stdout, console+"=== loopai run "+id+", attempt 2 finished ===\nstatus: "+
					string(tc.status)+"\n", args)
				assert.NotContains(t, stderr, "error:", "an exit code is no error")
			}
		})
	}
}

func TestAttachCommandCtrlCDetaches(t *testing.T) {
	root := t.TempDir()
	dir := saveRunsTestRun(t, root, runs.Descriptor{ID: "live-1", Plan: "docs/plans/live.md", Branch: "live"})
	h := startRunsHolder(t, dir)

	cmd := runsMainCommand(t, root, "--attach", "live-1")
	var stdout, stderr syncBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	})
	require.Eventually(t, func() bool { return strings.Contains(stdout.String(), "holder: line 1\n") }, runsWait,
		20*time.Millisecond, "attach replays the console log")

	require.NoError(t, cmd.Process.Signal(syscall.SIGINT))
	start := time.Now()
	select {
	case <-done:
	case <-time.After(runsWait):
		t.Fatal("attach did not exit on Ctrl+C")
	}
	assert.Equal(t, 0, exitCode(t, waitErr), "stderr:\n%s", stderr.String())
	assert.Less(t, time.Since(start), 5*time.Second, "attach exits at once, not through the force exit")
	assert.Contains(t, stdout.String(), "holder: line 1\ndetached; the run continues: loopai --attach live-1\n")
	assert.NotContains(t, stderr.String(), "interrupting", "no interrupt watcher runs for --attach")

	assert.True(t, h.alive(), "the run is left running")
	stop, err := runs.StopRequested(dir)
	require.NoError(t, err)
	assert.False(t, stop)
	st, err := runs.Classify(root, dir)
	require.NoError(t, err)
	assert.Equal(t, runs.StatusRunning, st)
}

func TestRunCommandsThroughMain(t *testing.T) {
	root := t.TempDir()
	saveRunsTestRun(t, root, runs.Descriptor{ID: "feature-1", Plan: "docs/plans/feature.md", Branch: "feature",
		CreatedAt: runsEpoch})
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "runs lists the runs", args: []string{"--runs"}, wantStdout: "feature-1  crashed"},
		{name: "runs with a plan", args: []string{"--runs", "plan.md"}, wantCode: 1,
			wantStderr: "error: --runs takes no argument, got 1"},
		{name: "attach with detach", args: []string{"--attach", "--detach"}, wantCode: 1,
			wantStderr: "error: --attach cannot be combined with other mode flags"},
		{name: "stop without a run", args: []string{"--stop"}, wantCode: 1, wantStderr: "error: --stop requires the run to stop"},
		{name: "stop an unknown run", args: []string{"--stop", "nope"}, wantCode: 1,
			wantStderr: "error: no matching detached run: nope in "},
		{name: "stop a finished run", args: []string{"--stop", "feature-1"}, wantCode: 1,
			wantStderr: "error: run feature-1 is not running (crashed); there is nothing to stop"},
		{name: "attach an unknown run", args: []string{"--attach", "nope"}, wantCode: 1,
			wantStderr: "error: no matching detached run: nope in "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runRunsMain(t, root, tc.args...)
			assert.Equal(t, tc.wantCode, code, "stderr:\n%s", stderr)
			assert.Contains(t, stdout, tc.wantStdout)
			assert.Contains(t, stderr, tc.wantStderr)
		})
	}
}

func TestStopCommandStopsDetachedChild(t *testing.T) {
	plainColors(t)
	f := newDetachChildFixture(t, true)
	p := f.launch(t, "--tasks-only", "docs/plans/child.md")
	assert.Contains(t, p.awaitAck(t, f), `"ok":true`)
	// the run is inside a live session of its first task, not merely past its task event
	f.awaitSession(t)
	t.Chdir(f.repo)

	var out bytes.Buffer
	require.NoError(t, runStopCommand(t.Context(), detachChildID, testColors(), &out))
	assert.Equal(t, "stop requested: run "+detachChildID+"\nrun "+detachChildID+": stopped\n", out.String())

	assert.Equal(t, 2, p.wait(t, f), "the child exits on SIGTERM with the stopped code")
	stop, err := runs.StopRequested(f.dir)
	require.NoError(t, err)
	assert.True(t, stop, "the stop marker was written")
	st, err := runs.Classify(f.repo, f.dir)
	require.NoError(t, err)
	assert.Equal(t, runs.StatusStopped, st)
	rec := f.outcome(t)
	assert.Equal(t, runs.StatusStopped, rec.Status)
	assert.Equal(t, 2, rec.ExitCode)
	assert.Equal(t, f.worktreePath(), rec.WorktreeKept)
	assert.DirExists(t, f.worktreePath(), "a stopped run keeps its worktree")
}

func TestRunRunsCommand(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	saveRunsTestRun(t, root, runs.Descriptor{ID: "feature-1", Plan: "docs/plans/feature.md", Branch: "feature"})
	var out bytes.Buffer
	require.NoError(t, runRunsCommand(&out))
	assert.Contains(t, out.String(), "feature-1  crashed  1        cli")
}
