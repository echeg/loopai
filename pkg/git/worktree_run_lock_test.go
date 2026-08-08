package git

import (
	"errors"
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
	require.True(t, errors.As(err, &busyErr))
	assert.Equal(t, os.Getpid(), busyErr.PID)
	assert.False(t, busyErr.Started.IsZero())
	assert.Contains(t, busyErr.Info(), "pid=")
	assert.Contains(t, busyErr.Info(), "started=")

	require.NoError(t, release())
	releaseAgain, err := AcquireWorktreeRunLock(wtPath)
	require.NoError(t, err)
	require.NoError(t, releaseAgain())
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
		lockFile, err := os.OpenFile(filepath.Join(gitDir, worktreeRunLockName), os.O_CREATE|os.O_RDWR, 0o600)
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

func setupRunLockWorktree(t *testing.T) string {
	t.Helper()
	repo := setupExternalTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "lock-worktree")
	runGit(t, repo, "worktree", "add", "-b", "run-lock-"+strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-"), wtPath)
	return wtPath
}
