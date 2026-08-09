package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireWorktreeRunLock(t *testing.T) {
	wtPath := setupRunLockWorktree(t)

	release, err := AcquireWorktreeRunLock(wtPath)
	require.NoError(t, err)
	require.NotNil(t, release)

	_, err = AcquireWorktreeRunLock(wtPath)
	require.Error(t, err)
	var busyErr *ErrWorktreeBusy
	require.ErrorAs(t, err, &busyErr)
	assert.Equal(t, os.Getpid(), busyErr.PID)
	assert.False(t, busyErr.Started.IsZero())
	assert.Contains(t, busyErr.Info(), "pid=")
	assert.Contains(t, busyErr.Info(), "started=")

	require.NoError(t, release())
	releaseAgain, err := AcquireWorktreeRunLock(wtPath)
	require.NoError(t, err)
	require.NoError(t, releaseAgain())
}

func TestAcquireWorktreeRunLockConcurrent(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	start := make(chan struct{})
	releaseWinner := make(chan struct{})
	results := make(chan error, 2)
	done := make(chan struct{}, 2)

	for range 2 {
		go func() {
			<-start
			release, err := AcquireWorktreeRunLock(wtPath)
			results <- err
			if err == nil {
				<-releaseWinner
				_ = release()
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
}

func TestErrWorktreeBusyError(t *testing.T) {
	t.Run("with diagnostics", func(t *testing.T) {
		err := &ErrWorktreeBusy{
			Path:    "/tmp/loopai-run.lock",
			PID:     123,
			Started: time.Date(2026, time.August, 9, 12, 30, 0, 0, time.UTC),
		}
		assert.Equal(t,
			"worktree run lock /tmp/loopai-run.lock is busy (pid=123 started=2026-08-09T12:30:00Z)",
			err.Error())
	})

	t.Run("without diagnostics", func(t *testing.T) {
		err := &ErrWorktreeBusy{Path: "/tmp/loopai-run.lock"}
		assert.Equal(t, "worktree run lock /tmp/loopai-run.lock is busy", err.Error())
		assert.Empty(t, err.Info())
	})
}

func TestProbeWorktreeRunLock(t *testing.T) {
	t.Run("free lock remains acquirable", func(t *testing.T) {
		wtPath := setupRunLockWorktree(t)
		gitDir, err := worktreeGitDir(wtPath)
		require.NoError(t, err)
		lockPath := filepath.Join(gitDir, worktreeRunLockName)
		assert.NoFileExists(t, lockPath)

		busy, info, err := ProbeWorktreeRunLock(wtPath)
		require.NoError(t, err)
		assert.False(t, busy)
		assert.Empty(t, info)
		assert.FileExists(t, lockPath)

		release, err := AcquireWorktreeRunLock(wtPath)
		require.NoError(t, err)
		require.NoError(t, release())
	})

	t.Run("held lock returns diagnostics", func(t *testing.T) {
		wtPath := setupRunLockWorktree(t)
		release, err := AcquireWorktreeRunLock(wtPath)
		require.NoError(t, err)
		defer func() { require.NoError(t, release()) }()

		busy, info, err := ProbeWorktreeRunLock(wtPath)
		require.NoError(t, err)
		assert.True(t, busy)
		assert.Contains(t, info, "pid=")
		assert.Contains(t, info, "started=")
	})

	t.Run("corrupt diagnostics do not affect contention", func(t *testing.T) {
		wtPath := setupRunLockWorktree(t)
		gitDir, err := worktreeGitDir(wtPath)
		require.NoError(t, err)
		lockFile, err := os.OpenFile(filepath.Join(gitDir, worktreeRunLockName), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // test-owned Git directory
		require.NoError(t, err)
		_, err = lockFile.WriteString("not valid lock metadata\n")
		require.NoError(t, err)
		acquired, err := tryLockRepositoryFile(lockFile)
		require.NoError(t, err)
		require.True(t, acquired)

		busy, info, err := ProbeWorktreeRunLock(wtPath)
		require.NoError(t, err)
		assert.True(t, busy)
		assert.Empty(t, info)

		require.NoError(t, unlockRepositoryFile(lockFile))
		require.NoError(t, lockFile.Close())
		release, err := AcquireWorktreeRunLock(wtPath)
		require.NoError(t, err)
		require.NoError(t, release())
	})
}

func TestWorktreeRunLockLocation(t *testing.T) {
	wtPath := setupRunLockWorktree(t)
	gitDir, err := worktreeGitDir(wtPath)
	require.NoError(t, err)

	release, err := AcquireWorktreeRunLock(wtPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, release()) }()

	lockPath := filepath.Join(gitDir, worktreeRunLockName)
	assert.FileExists(t, lockPath)
	assert.NoFileExists(t, filepath.Join(wtPath, worktreeRunLockName))
	assert.NotEqual(t, filepath.Clean(wtPath), filepath.Dir(lockPath))
	status := runGit(t, wtPath, "status", "--porcelain", "--untracked-files=all")
	assert.NotContains(t, status, worktreeRunLockName)
}

func TestWorktreeRunLockInvalidPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	release, err := AcquireWorktreeRunLock(missing)
	require.Error(t, err)
	assert.Nil(t, release)
	assert.Contains(t, err.Error(), "locate worktree Git directory")

	busy, info, err := ProbeWorktreeRunLock(missing)
	require.Error(t, err)
	assert.False(t, busy)
	assert.Empty(t, info)
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
