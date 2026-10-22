//go:build !windows

package runs

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helperTimeout bounds a re-executed helper process; spawns are slow under -race.
const helperTimeout = 30 * time.Second

func TestAcquireBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)

	release, err := AcquireBounded(path, time.Second)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	start := time.Now()
	again, err := AcquireBounded(path, 150*time.Millisecond)
	require.ErrorIs(t, err, ErrBusy)
	assert.Nil(t, again)
	assert.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond, "a busy lock is retried until the deadline")

	require.NoError(t, release())
	again, err = AcquireBounded(path, time.Second)
	require.NoError(t, err)
	require.NoError(t, again())
}

func TestAcquireBoundedWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	release, err := AcquireBounded(path, time.Second)
	require.NoError(t, err)

	released := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		released <- release()
	}()
	again, err := AcquireBounded(path, helperTimeout)
	require.NoError(t, err, "a lock released before the deadline is acquired")
	require.NoError(t, <-released)
	require.NoError(t, again())
}

func TestAcquireBoundedErrors(t *testing.T) {
	_, err := AcquireBounded(filepath.Join(t.TempDir(), "missing", LockFile), time.Second)
	require.ErrorContains(t, err, "open lock")
	assert.NotErrorIs(t, err, ErrBusy)
}

func TestProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)

	held, err := Probe(path)
	require.NoError(t, err)
	assert.False(t, held, "a missing lock file is not held")
	assert.NoFileExists(t, path, "a probe never creates the lock file")

	release, err := AcquireBounded(path, time.Second)
	require.NoError(t, err)
	held, err = Probe(path)
	require.NoError(t, err)
	assert.True(t, held)

	require.NoError(t, release())
	held, err = Probe(path)
	require.NoError(t, err)
	assert.False(t, held)
}

func TestFlockRetriesInterruptedCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	// every flock call is interrupted once by a signal before it goes through
	interrupted, interrupt := 0, true
	flockSyscall = func(fd, how int) error {
		if interrupt {
			interrupt = false
			interrupted++
			return syscall.EINTR
		}
		interrupt = true
		return syscall.Flock(fd, how)
	}
	t.Cleanup(func() { flockSyscall = syscall.Flock })

	held, err := Probe(path)
	require.NoError(t, err)
	assert.False(t, held, "an interrupted probe of a free lock is retried, not read as held")
	assert.Equal(t, 2, interrupted, "the probe's lock and unlock were both retried")

	release, err := AcquireBounded(path, time.Second)
	require.NoError(t, err)
	held, err = Probe(path)
	require.NoError(t, err)
	assert.True(t, held, "a held lock still reads as held after a retry")
	require.NoError(t, release())

	held, err = Probe(path)
	require.NoError(t, err)
	assert.False(t, held, "the retried unlock released the lock")
	assert.Equal(t, 7, interrupted)
}

func TestFlockErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	flockSyscall = func(int, int) error { return syscall.ENOLCK }
	t.Cleanup(func() { flockSyscall = syscall.Flock })

	_, err := Probe(path)
	require.ErrorIs(t, err, syscall.ENOLCK)
	require.ErrorContains(t, err, "probe lock")
	_, err = AcquireBounded(path, time.Second)
	require.ErrorIs(t, err, syscall.ENOLCK)
	assert.NotErrorIs(t, err, ErrBusy, "a failing flock is not a busy lock")
}

func TestProbesDoNotSeeEachOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	// another observer in the middle of its probe holds the shared lock
	f, err := os.Open(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB))

	held, err := Probe(path)
	require.NoError(t, err)
	assert.False(t, held, "a concurrent probe does not read as a holder")

	_, err = AcquireBounded(path, 50*time.Millisecond)
	require.ErrorIs(t, err, ErrBusy, "the run waits out a probe instead of sharing the lock")

	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
	release, err := AcquireBounded(path, time.Second)
	require.NoError(t, err)
	require.NoError(t, release())
}

func TestAcquireBoundedSucceedsWhileOthersProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	startLockHelper(t, "probe", path)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := Probe(path)
				assert.NoError(t, err)
			}
		})
	}
	for range 20 {
		release, err := AcquireBounded(path, 5*time.Second)
		require.NoError(t, err, "probing observers never make the run's acquire fail")
		require.NoError(t, release())
	}
	close(stop)
	wg.Wait()
}

func TestAcquireBoundedHelperProcessHoldsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	helper, _ := startLockHelper(t, "hold", path)

	_, err := AcquireBounded(path, 200*time.Millisecond)
	require.ErrorIs(t, err, ErrBusy)
	held, err := Probe(path)
	require.NoError(t, err)
	assert.True(t, held)

	require.NoError(t, helper.Process.Kill())
	_ = helper.Wait()

	release, err := AcquireBounded(path, helperTimeout)
	require.NoError(t, err, "the OS releases the lock when its holder dies without cleanup")
	require.NoError(t, release())
}

func TestAcquireBoundedLockNotInheritedByChildren(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	helper, grandchild := startLockHelper(t, "hold-spawn", path)
	require.Positive(t, grandchild)
	t.Cleanup(func() {
		if p, err := os.FindProcess(grandchild); err == nil {
			_ = p.Kill()
		}
	})

	require.NoError(t, helper.Process.Kill())
	_ = helper.Wait()

	release, err := AcquireBounded(path, helperTimeout)
	require.NoError(t, err, "a process the holder started must not keep the lock (O_CLOEXEC)")
	require.NoError(t, release())
}

// startLockHelper re-executes the test binary as a lock helper in mode, waits until it reports
// readiness, and stops it at cleanup. It returns the helper and, for hold-spawn, the pid of the
// process the helper started.
func startLockHelper(t *testing.T, mode, path string) (*exec.Cmd, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), helperTimeout)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), "LOOPAI_RUNS_LOCK_HELPER="+mode, "LOOPAI_RUNS_LOCK_PATH="+path)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "lock helper exited before reporting readiness")
	fields := strings.Fields(scanner.Text())
	require.NotEmpty(t, fields)
	require.Equal(t, "ready", fields[0])
	pid := 0
	if len(fields) > 1 {
		pid, err = strconv.Atoi(fields[1])
		require.NoError(t, err)
	}
	return cmd, pid
}

// TestLockHelperProcess is the body of startLockHelper's re-executed process.
func TestLockHelperProcess(t *testing.T) {
	mode := os.Getenv("LOOPAI_RUNS_LOCK_HELPER")
	if mode == "" {
		return
	}
	path := os.Getenv("LOOPAI_RUNS_LOCK_PATH")
	switch mode {
	case "hold":
		if _, err := AcquireBounded(path, helperTimeout); err != nil {
			t.Fatal(err)
		}
		fmt.Println("ready")
	case "hold-spawn":
		if _, err := AcquireBounded(path, helperTimeout); err != nil {
			t.Fatal(err)
		}
		// the child outlives this helper, so it is bound to no context of it
		child := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestLockHelperProcess$")
		child.Env = append(os.Environ(), "LOOPAI_RUNS_LOCK_HELPER=sleep")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Println("ready", child.Process.Pid)
	case "probe":
		fmt.Println("ready")
		for {
			if _, err := Probe(path); err != nil {
				t.Fatal(err)
			}
		}
	case "sleep":
		// longer than any wait of the test that started the helper, so a lock this process
		// wrongly inherited would make that test time out instead of passing late
		time.Sleep(2 * helperTimeout)
		return
	default:
		t.Fatalf("unknown lock helper mode %q", mode)
	}
	time.Sleep(time.Hour)
}
