//go:build windows

package executor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestProcessGroupCleanup_WindowsTree(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		fallback, normalExit bool
	}{
		{name: "job"},
		{name: "taskkill fallback", fallback: true},
		{name: "normal exit reaps orphan", normalExit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOOPAI_TREE_ROLE", "parent")
			exitGate := filepath.Join(t.TempDir(), "exit")
			if tc.normalExit {
				t.Setenv("LOOPAI_TREE_EXIT", "1")
				t.Setenv("LOOPAI_TREE_EXIT_GATE", exitGate)
			}
			gate := filepath.Join(t.TempDir(), "start")
			t.Setenv("LOOPAI_TREE_GATE", gate)
			exe, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.Command("cmd", "/c", exe, "-test.run=^TestProcessTreeHelper$")
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			assign := windows.AssignProcessToJobObject
			if tc.fallback {
				assign = func(windows.Handle, windows.Handle) error { return windows.ERROR_ACCESS_DENIED }
			}
			pg, err := startProcessGroupWithAssign(cmd, ctx.Done(), assign, stdout)
			require.NoError(t, err)
			t.Cleanup(func() { cancel(); _ = pg.Wait() })
			if tc.fallback {
				require.Zero(t, pg.job)
			} else {
				require.NotZero(t, pg.job)
			}
			require.NoError(t, os.WriteFile(gate, nil, 0o600))
			pidCh := make(chan int, 1)
			go func() {
				line, _ := bufio.NewReader(stdout).ReadString('\n')
				pid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "CHILD_PID:")))
				pidCh <- pid
			}()
			var pid int
			select {
			case pid = <-pidCh:
			case <-time.After(10 * time.Second):
				t.Fatal("grandchild did not start")
			}
			require.Positive(t, pid)
			child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
			require.NoError(t, err)
			defer windows.CloseHandle(child)
			defer windows.TerminateProcess(child, 1) //nolint:errcheck // prevent leaks on assertion failures
			if tc.normalExit {
				// Acquire a child handle before the exit watcher can reap it.
				require.NoError(t, os.WriteFile(exitGate, nil, 0o600))
				require.NoError(t, pg.Wait())
			} else {
				cancel()
				require.Error(t, pg.Wait())
			}
			state, err := windows.WaitForSingleObject(child, 5000)
			require.NoError(t, err)
			require.Equal(t, uint32(windows.WAIT_OBJECT_0), state, "grandchild must exit with the launcher")
		})
	}
}

func TestProcessGroupCleanup_WindowsClosesJob(t *testing.T) {
	// Keep cmd alive until assignment, then let it exit successfully.
	cmd := exec.Command("cmd", "/c", "set /p value=")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	pg, err := startProcessGroup(cmd, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stdin.Close(); _ = pg.Wait() })
	job := pg.job
	require.NotZero(t, job)
	_, err = io.WriteString(stdin, "done\n")
	require.NoError(t, err)
	require.NoError(t, stdin.Close())
	require.NoError(t, pg.Wait())
	require.NoError(t, pg.Wait(), "Wait must be idempotent")
	require.Zero(t, pg.job)
	require.ErrorIs(t, windows.SetHandleInformation(job, 0, 0), windows.ERROR_INVALID_HANDLE)
}

func TestCreateProcessJob_AssignmentFailureClosesHandle(t *testing.T) {
	var acquired windows.Handle
	job, err := createProcessJob(os.Getpid(), func(job, _ windows.Handle) error {
		acquired = job
		return windows.ERROR_ACCESS_DENIED
	})
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	require.Zero(t, job)
	require.NotZero(t, acquired)
	require.ErrorIs(t, windows.SetHandleInformation(acquired, 0, 0), windows.ERROR_INVALID_HANDLE)
}

func TestProcessGroupCleanup_WindowsClosesRetainedPipe(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "set /p value=")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	defer stdin.Close()
	reader, writer := io.Pipe()
	defer writer.Close() // deliberately hold writer open beyond process lifetime
	ctx, cancel := context.WithCancel(t.Context())
	pg, err := startProcessGroup(cmd, ctx.Done(), reader)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); _ = pg.Wait() })
	done := make(chan struct{})
	go func() { _, _ = io.ReadAll(reader); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("kill left a blocked pipe reader")
	}
	require.Error(t, pg.Wait())
}

func TestWindowsExecutor_NormalExitWithRetainedPipes(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("LOOPAI_TREE_ROLE", "parent")
			t.Setenv("LOOPAI_TREE_EXIT", "1")
			exe, err := os.Executable()
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			var result Result
			if kind == "claude" {
				e := &ClaudeExecutor{Command: exe, Args: "-test.run=^TestProcessTreeHelper$", ArgsSet: true}
				result = e.Run(ctx, "")
			} else {
				e := &CodexExecutor{runner: codexHelperRunner{exe: exe}}
				result = e.Run(ctx, "")
			}
			require.NoError(t, ctx.Err(), "normal exit must not depend on cancellation")
			require.NoError(t, result.Error)
			pidText := strings.TrimSpace(strings.TrimPrefix(result.Output, "CHILD_PID:"))
			pid, err := strconv.Atoi(pidText)
			require.NoError(t, err)
			require.Positive(t, pid)
			child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				return // the process has already been reaped
			}
			require.NoError(t, err)
			defer windows.CloseHandle(child)
			state, err := windows.WaitForSingleObject(child, 1000)
			require.NoError(t, err)
			require.Equal(t, uint32(windows.WAIT_OBJECT_0), state)
		})
	}
}

type codexHelperRunner struct{ exe string }

func (r codexHelperRunner) Run(ctx context.Context, _ string, _ ...string) (CodexStreams, func() error, error) {
	return (&execCodexRunner{}).Run(ctx, r.exe, "-test.run=^TestProcessTreeHelper$")
}

func TestWindowsProcessAssignmentPrecedesExecution(t *testing.T) {
	t.Setenv("LOOPAI_TREE_ROLE", "parent")
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("LOOPAI_TREE_READY", ready)
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command("cmd", "/c", exe, "-test.run=^TestProcessTreeHelper$")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var beforeAssignment error
	pg, err := startProcessGroupWithAssign(cmd, ctx.Done(), func(job, process windows.Handle) error {
		// Deliberately widen the startup race: an unsuspended launcher has time
		// to spawn both helper generations before it joins the job.
		time.Sleep(200 * time.Millisecond)
		_, beforeAssignment = os.Stat(ready)
		return windows.AssignProcessToJobObject(job, process)
	}, stdout)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); _ = pg.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err, "the launcher must resume after assignment")
	pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "CHILD_PID:")))
	require.NoError(t, err)
	child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	require.NoError(t, err)
	defer windows.CloseHandle(child)
	defer windows.TerminateProcess(child, 1) //nolint:errcheck // clean up even if containment regresses
	require.True(t, os.IsNotExist(beforeAssignment), "launcher ran before job assignment")
	cancel()
	require.Error(t, pg.Wait())
	state, err := windows.WaitForSingleObject(child, 1000)
	require.NoError(t, err)
	require.Equal(t, uint32(windows.WAIT_OBJECT_0), state, "early descendants must belong to the job")
}

func TestResumeProcess_MissingProcess(t *testing.T) {
	require.Error(t, resumeProcess(-1))
}
