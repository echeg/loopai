//go:build windows

package git

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRepositoryLockContendsWithLegacyByteZeroLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "lock")
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	legacyRange := new(windows.Overlapped)
	require.NoError(t, windows.LockFileEx(windows.Handle(holder.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, legacyRange))
	t.Cleanup(func() {
		require.NoError(t, windows.UnlockFileEx(windows.Handle(holder.Fd()), 0, 1, 0, legacyRange))
		require.NoError(t, holder.Close())
	})

	contender, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, contender.Close()) })
	acquired, err := tryLockRepositoryFile(contender)
	require.NoError(t, err)
	assert.False(t, acquired, "new repository locks must contend with pre-upgrade byte-zero locks")
}

func TestWorktreeRunLockLeavesDiagnosticHeaderReadable(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "lock")
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	_, err = holder.WriteString("pid=123 started=2026-08-09T12:30:00Z\n")
	require.NoError(t, err)
	acquired, err := tryLockWorktreeRunFile(holder)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() {
		require.NoError(t, unlockWorktreeRunFile(holder))
		require.NoError(t, holder.Close())
	})

	contender, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, contender.Close()) })
	contents, err := io.ReadAll(contender)
	require.NoError(t, err)
	assert.Contains(t, string(contents), "pid=123")

	acquired, err = tryLockWorktreeRunFile(contender)
	require.NoError(t, err)
	assert.False(t, acquired)
}
