//go:build windows

package git

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryLockLeavesDiagnosticHeaderReadable(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "lock")
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	_, err = holder.WriteString("pid=123 started=2026-08-09T12:30:00Z\n")
	require.NoError(t, err)
	acquired, err := tryLockRepositoryFile(holder)
	require.NoError(t, err)
	require.True(t, acquired)
	t.Cleanup(func() {
		require.NoError(t, unlockRepositoryFile(holder))
		require.NoError(t, holder.Close())
	})

	contender, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, contender.Close()) })
	contents, err := io.ReadAll(contender)
	require.NoError(t, err)
	assert.Contains(t, string(contents), "pid=123")

	acquired, err = tryLockRepositoryFile(contender)
	require.NoError(t, err)
	assert.False(t, acquired)
}
