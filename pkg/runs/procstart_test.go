package runs

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTokenSupported reports whether ProcessStartToken is implemented on this platform.
func startTokenSupported() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux"
}

func TestProcessStartTokenIsStable(t *testing.T) {
	first, err := ProcessStartToken(os.Getpid())
	if !startTokenSupported() {
		require.ErrorIs(t, err, ErrUnsupported)
		return
	}
	require.NoError(t, err)
	require.NotEmpty(t, first)

	second, err := ProcessStartToken(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestProcessStartTokenDiffersForAnotherProcess(t *testing.T) {
	if !startTokenSupported() {
		t.Skip("process start tokens are not supported on " + runtime.GOOS)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessStartTokenHelperProcess$")
	cmd.Env = append(os.Environ(), "LOOPAI_RUNS_START_TOKEN_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	own, err := ProcessStartToken(os.Getpid())
	require.NoError(t, err)
	other, err := ProcessStartToken(cmd.Process.Pid)
	require.NoError(t, err)
	assert.NotEqual(t, own, other)
}

func TestProcessStartTokenErrors(t *testing.T) {
	for _, pid := range []int{0, -1} {
		_, err := ProcessStartToken(pid)
		require.ErrorContains(t, err, "invalid pid")
	}
	if !startTokenSupported() {
		return
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	require.NoError(t, cmd.Run())
	_, err := ProcessStartToken(cmd.Process.Pid)
	require.Error(t, err, "an exited and reaped process has no start token")
}

// TestProcessStartTokenHelperProcess stays alive until its stdin is closed.
func TestProcessStartTokenHelperProcess(t *testing.T) {
	if os.Getenv("LOOPAI_RUNS_START_TOKEN_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}
