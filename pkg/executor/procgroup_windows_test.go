//go:build windows

package executor

import (
	"bufio"
	"context"
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
			if tc.normalExit {
				t.Setenv("LOOPAI_TREE_EXIT", "1")
			}
			gate := filepath.Join(t.TempDir(), "start")
			t.Setenv("LOOPAI_TREE_GATE", gate)
			exe, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.Command("cmd", "/c", exe, "-test.run=^TestProcessTreeHelper$")
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			ctx, cancel := context.WithCancel(t.Context())
			assign := windows.AssignProcessToJobObject
			if tc.fallback {
				assign = func(windows.Handle, windows.Handle) error { return windows.ERROR_ACCESS_DENIED }
			}
			pg := newProcessGroupCleanupWithAssign(cmd, ctx.Done(), assign, stdout)
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
	require.NoError(t, cmd.Start())
	pg := newProcessGroupCleanup(cmd, nil)
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
	require.NoError(t, cmd.Start())
	defer stdin.Close()
	reader, writer := io.Pipe()
	defer writer.Close() // deliberately hold writer open beyond process lifetime
	ctx, cancel := context.WithCancel(t.Context())
	pg := newProcessGroupCleanup(cmd, ctx.Done(), reader)
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
