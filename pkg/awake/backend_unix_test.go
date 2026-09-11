//go:build darwin || linux

package awake

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubWait bounds the wait for a stub to start. Process creation on a loaded host is slow: under a
// concurrent race-enabled suite one spawn has been measured in the hundreds of milliseconds.
const stubWait = 30 * time.Second

// writeInhibitorStub installs a script named name at the front of PATH that records its arguments
// to argsFile and then blocks like a real inhibitor would. The rest of PATH is kept so the stub
// can find sleep; a stub that exits at once would make the liveness assertions vacuous.
func writeInhibitorStub(t *testing.T, name string) (argsFile string) {
	t.Helper()
	binDir := t.TempDir()
	argsFile = filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\nexec sleep 60\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o700)) //nolint:gosec // executable stub in t.TempDir
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestCommandBackend_AcquireStartsAndReleaseTerminates(t *testing.T) {
	argsFile := writeInhibitorStub(t, "stub-inhibit")
	b := newCommandBackend("stub-inhibit", "-x", "42")
	require.NotNil(t, b)

	require.NoError(t, b.Acquire())
	pid := b.pid()
	require.NotZero(t, pid)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(argsFile) //nolint:gosec // path built from t.TempDir
		return err == nil && string(data) == "-x\n42\n"
	}, stubWait, 10*time.Millisecond)
	assert.True(t, processAlive(pid), "stub must block like a real inhibitor")

	b.Release()
	assert.False(t, processAlive(pid), "inhibitor process must not outlive Release")
	assert.Zero(t, b.pid())
}

func TestCommandBackend_ReleaseWithoutAcquireIsNoop(t *testing.T) {
	writeInhibitorStub(t, "stub-inhibit")
	b := newCommandBackend("stub-inhibit")
	require.NotNil(t, b)

	b.Release()
	assert.Zero(t, b.pid())
}

func TestCommandBackend_ReacquireAfterRelease(t *testing.T) {
	writeInhibitorStub(t, "stub-inhibit")
	b := newCommandBackend("stub-inhibit")
	require.NotNil(t, b)

	require.NoError(t, b.Acquire())
	first := b.pid()
	b.Release()
	require.NoError(t, b.Acquire())
	second := b.pid()
	t.Cleanup(b.Release)

	assert.NotZero(t, second)
	assert.NotEqual(t, first, second)
	assert.True(t, processAlive(second))
}

func TestCommandBackend_AcquireFailsForBrokenBinary(t *testing.T) {
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "stub-inhibit"), []byte("not executable"), 0o600))
	t.Setenv("PATH", binDir)

	assert.Nil(t, newCommandBackend("stub-inhibit"), "a binary that cannot run is not a backend")
}

func TestNewCommandBackend_MissingBinaryReturnsNil(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	assert.Nil(t, newCommandBackend("stub-inhibit"))
}

func TestPlatformBackend(t *testing.T) {
	t.Run("no inhibitor on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		assert.Nil(t, platformBackend())
	})

	t.Run("uses the platform inhibitor bound to this process", func(t *testing.T) {
		var argsFile string
		var want string
		switch runtime.GOOS {
		case "darwin":
			argsFile = writeInhibitorStub(t, "caffeinate")
			want = "-i\n-w\n" + strconv.Itoa(os.Getpid()) + "\n"
		case "linux":
			argsFile = writeInhibitorStub(t, "systemd-inhibit")
			want = "--what=idle:sleep\n--who=loopai\n--why=plan execution in progress\n--mode=block\n" +
				"tail\n--pid=" + strconv.Itoa(os.Getpid()) + "\n-f\n/dev/null\n"
		default:
			t.Skip("no platform inhibitor")
		}

		b := platformBackend()
		require.NotNil(t, b)
		require.NoError(t, b.Acquire())
		t.Cleanup(b.Release)
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(argsFile) //nolint:gosec // path built from t.TempDir
			return err == nil && string(data) == want
		}, stubWait, 10*time.Millisecond)
	})
}
