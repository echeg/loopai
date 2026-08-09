package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireWorktreeRunLock(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	svc := openRunLockService(t, wtPath)

	release, err := svc.AcquireWorktreeRunLock()
	require.NoError(t, err)
	require.NotNil(t, release)

	_, err = openRunLockService(t, wtPath).AcquireWorktreeRunLock()
	require.Error(t, err)
	var busyErr *ErrWorktreeBusy
	require.ErrorAs(t, err, &busyErr)
	assert.Equal(t, svc.Root(), busyErr.Path)
	assert.Equal(t, os.Getpid(), busyErr.PID)
	assert.False(t, busyErr.Started.IsZero())
	assert.Contains(t, busyErr.Info(), "pid=")
	assert.Contains(t, busyErr.Info(), "started=")

	require.NoError(t, release())
	releaseAgain, err := openRunLockService(t, wtPath).AcquireWorktreeRunLock()
	require.NoError(t, err)
	require.NoError(t, releaseAgain())
}

func TestAcquireWorktreeRunLockConcurrent(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	services := []*Service{openRunLockService(t, wtPath), openRunLockService(t, wtPath)}
	start := make(chan struct{})
	releaseWinner := make(chan struct{})
	results := make(chan error, 2)
	releaseResults := make(chan error, 1)
	done := make(chan struct{}, 2)

	for _, svc := range services {
		go func() {
			<-start
			release, err := svc.AcquireWorktreeRunLock()
			results <- err
			if err == nil {
				<-releaseWinner
				releaseResults <- release()
			}
			done <- struct{}{}
		}()
	}
	close(start)

	var acquired, busy int
	for range 2 {
		err := <-results
		if err == nil {
			acquired++
			continue
		}
		var busyErr *ErrWorktreeBusy
		require.ErrorAs(t, err, &busyErr)
		busy++
	}
	close(releaseWinner)
	<-done
	<-done
	assert.Equal(t, 1, acquired)
	assert.Equal(t, 1, busy)
	require.NoError(t, <-releaseResults)

	release, err := openRunLockService(t, wtPath).AcquireWorktreeRunLock()
	require.NoError(t, err)
	require.NoError(t, release())
}

func TestWorktreeRunLockReleasedAfterProcessKill(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	helperCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(helperCtx, os.Args[0], "-test.run=^TestWorktreeRunLockHelperProcess$")
	cmd.Env = append(os.Environ(), "LOOPAI_RUN_LOCK_HELPER=1", "LOOPAI_RUN_LOCK_WORKTREE="+wtPath)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "lock helper exited before reporting readiness")
	require.Equal(t, "ready", scanner.Text())

	_, err = openRunLockService(t, wtPath).AcquireWorktreeRunLock()
	var busyErr *ErrWorktreeBusy
	require.ErrorAs(t, err, &busyErr)
	require.NoError(t, cmd.Process.Kill())
	require.Error(t, cmd.Wait())

	release, err := openRunLockService(t, wtPath).AcquireWorktreeRunLock()
	require.NoError(t, err, "the OS must release the lock when the holder dies without cleanup")
	require.NoError(t, release())
}

func TestWorktreeRunLockHelperProcess(t *testing.T) {
	if os.Getenv("LOOPAI_RUN_LOCK_HELPER") != "1" {
		return
	}
	svc, err := NewService(os.Getenv("LOOPAI_RUN_LOCK_WORKTREE"), noopServiceLogger())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	release, err := svc.AcquireWorktreeRunLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_ = release
	fmt.Println("ready")
	for {
		time.Sleep(time.Hour)
	}
}

func TestErrWorktreeBusyError(t *testing.T) {
	t.Run("with diagnostics", func(t *testing.T) {
		err := &ErrWorktreeBusy{
			Path:    "/tmp/worktree",
			PID:     123,
			Started: time.Date(2026, time.August, 9, 12, 30, 0, 0, time.UTC),
		}
		assert.Equal(t,
			"plan worktree /tmp/worktree is busy: loopai is already running in it (pid=123 started=2026-08-09T12:30:00Z)",
			err.Error())
	})

	t.Run("without diagnostics", func(t *testing.T) {
		err := &ErrWorktreeBusy{Path: "/tmp/worktree"}
		assert.Equal(t,
			"plan worktree /tmp/worktree is busy: loopai is already running in it (holder details unavailable)",
			err.Error())
		assert.Empty(t, err.Info())
	})
}

func TestAcquireWorktreeRunLockCorruptDiagnostics(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	svc := openRunLockService(t, wtPath)
	lockFile, err := svc.openWorktreeRunLock(t.Context())
	require.NoError(t, err)
	_, err = lockFile.WriteString("not valid lock metadata\n")
	require.NoError(t, err)
	acquired, err := tryLockWorktreeRunFile(lockFile)
	require.NoError(t, err)
	require.True(t, acquired)

	_, err = openRunLockService(t, wtPath).AcquireWorktreeRunLock()
	var busyErr *ErrWorktreeBusy
	require.ErrorAs(t, err, &busyErr)
	assert.Empty(t, busyErr.Info())

	require.NoError(t, unlockWorktreeRunFile(lockFile))
	require.NoError(t, lockFile.Close())
	release, err := openRunLockService(t, wtPath).AcquireWorktreeRunLock()
	require.NoError(t, err)
	require.NoError(t, release())
}

func TestWorktreeRunLockLocation(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	svc := openRunLockService(t, wtPath)
	gitDir, err := svc.repo.gitDir(t.Context())
	require.NoError(t, err)

	release, err := svc.AcquireWorktreeRunLock()
	require.NoError(t, err)
	defer func() { require.NoError(t, release()) }()

	lockPath := filepath.Join(gitDir, worktreeRunLockName)
	assert.FileExists(t, lockPath)
	assert.NoFileExists(t, filepath.Join(wtPath, worktreeRunLockName))
	assert.NotEqual(t, filepath.Clean(wtPath), filepath.Dir(lockPath))
	status := runGit(t, wtPath, "status", "--porcelain", "--untracked-files=all")
	assert.NotContains(t, status, worktreeRunLockName)
}

func TestWorktreeRunLockUsesConfiguredVCSCommand(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	gitPath, err = filepath.Abs(gitPath)
	require.NoError(t, err)
	svc, err := NewService(wtPath, noopServiceLogger(), gitPath)
	require.NoError(t, err)
	t.Setenv("PATH", t.TempDir())

	release, err := svc.AcquireWorktreeRunLockContext(t.Context())
	require.NoError(t, err, "lock lookup must use the configured absolute Git command")
	require.NoError(t, release())
}

func TestWorktreeRunLockLookupHonorsCancellation(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	svc := openRunLockService(t, wtPath)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	release, err := svc.AcquireWorktreeRunLockContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, release)
}

func TestAcquireWorktreeRunLockMetadataFailureReleasesLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), worktreeRunLockName)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	injected := errors.New("injected metadata failure")
	release, err := acquireWorktreeRunLockFile(lockFile, "/tmp/worktree", func(*os.File, string) error {
		return injected
	})
	require.ErrorIs(t, err, injected)
	assert.Nil(t, release)

	secondFile, err := os.OpenFile(lockPath, os.O_RDWR, 0o600) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	acquired, err := tryLockWorktreeRunFile(secondFile)
	require.NoError(t, err)
	assert.True(t, acquired, "failed metadata writes must roll back the acquired OS lock")
	require.NoError(t, unlockWorktreeRunFile(secondFile))
	require.NoError(t, secondFile.Close())
}

func TestReadWorktreeBusyError(t *testing.T) {
	started := "2026-08-09T12:30:00Z"
	tests := []struct {
		name        string
		contents    string
		wantPID     int
		wantStarted bool
	}{
		{name: "valid", contents: "pid=123 started=" + started, wantPID: 123, wantStarted: true},
		{name: "missing field", contents: "pid=123"},
		{name: "wrong keys", contents: "process=123 time=" + started},
		{name: "invalid pid", contents: "pid=nope started=" + started},
		{name: "non-positive pid", contents: "pid=0 started=" + started},
		{name: "invalid time", contents: "pid=123 started=yesterday"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), worktreeRunLockName)
			lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // test-owned temporary path
			require.NoError(t, err)
			_, err = lockFile.WriteString(tc.contents)
			require.NoError(t, err)

			busyErr := readWorktreeBusyError(lockFile, lockPath)
			assert.Equal(t, lockPath, busyErr.Path)
			assert.Equal(t, tc.wantPID, busyErr.PID)
			assert.Equal(t, tc.wantStarted, !busyErr.Started.IsZero())
			require.NoError(t, lockFile.Close())
		})
	}

	t.Run("unreadable descriptor leaves diagnostics empty", func(t *testing.T) {
		lockPath := filepath.Join(t.TempDir(), worktreeRunLockName)
		lockFile, err := os.Create(lockPath) //nolint:gosec // test-owned temporary path
		require.NoError(t, err)
		require.NoError(t, lockFile.Close())

		busyErr := readWorktreeBusyError(lockFile, lockPath)
		assert.Equal(t, lockPath, busyErr.Path)
		assert.Empty(t, busyErr.Info())
	})

	t.Run("read failure leaves diagnostics empty", func(t *testing.T) {
		dir := t.TempDir()
		dirFile, err := os.Open(dir) //nolint:gosec // test-owned temporary path
		require.NoError(t, err)

		busyErr := readWorktreeBusyError(dirFile, dir)
		assert.Equal(t, dir, busyErr.Path)
		assert.Empty(t, busyErr.Info())
		require.NoError(t, dirFile.Close())
	})
}

func TestReplaceLockFileContentsClosedFile(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), worktreeRunLockName)
	lockFile, err := os.Create(lockPath) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	require.NoError(t, lockFile.Close())

	err = replaceLockFileContents(lockFile, "metadata")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncate lock file")
}

func setupRunLockWorktree(t *testing.T) string {
	t.Helper()
	repo := setupExternalTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "lock-worktree")
	runGit(t, repo, "worktree", "add", "-b", "run-lock-"+strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-"), wtPath)
	return wtPath
}

func openRunLockService(t *testing.T, wtPath string) *Service {
	t.Helper()
	svc, err := NewService(wtPath, noopServiceLogger())
	require.NoError(t, err)
	return svc
}
