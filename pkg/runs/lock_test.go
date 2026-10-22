//go:build !windows

package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireLaunchLock(t *testing.T) {
	root := t.TempDir()
	plan, branch := "docs/plans/20261022-feature-x.md", "feature-x"

	inProgress, err := LaunchInProgress(root, plan, branch)
	require.NoError(t, err)
	assert.False(t, inProgress)
	assert.NoDirExists(t, RunsDir(root), "a probe creates nothing")

	release, err := AcquireLaunchLock(root, plan, branch, time.Second)
	require.NoError(t, err)

	sum := sha256.Sum256([]byte(plan + "\x00" + branch))
	lockName := ".launch-" + hex.EncodeToString(sum[:])[:16] + ".lock"
	assert.FileExists(t, filepath.Join(RunsDir(root), lockName))
	assert.False(t, ValidID(lockName), "a launch lock is never listed as a run")

	inProgress, err = LaunchInProgress(root, plan, branch)
	require.NoError(t, err)
	assert.True(t, inProgress)

	_, err = AcquireLaunchLock(root, plan, branch, 100*time.Millisecond)
	require.ErrorIs(t, err, ErrLaunchBusy)
	require.NotErrorIs(t, err, ErrBusy, "a busy launch lock has its own error")
	require.ErrorContains(t, err, plan)

	other, err := AcquireLaunchLock(root, plan, "feature-x-2", time.Second)
	require.NoError(t, err, "the same plan on another branch has its own launch lock")
	require.NoError(t, other())

	require.NoError(t, release())
	inProgress, err = LaunchInProgress(root, plan, branch)
	require.NoError(t, err)
	assert.False(t, inProgress)

	again, err := AcquireLaunchLock(root, plan, branch, time.Second)
	require.NoError(t, err)
	require.NoError(t, again())
}

func TestAcquireLaunchLockErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(root, []byte("x"), 0o600))

	_, err := AcquireLaunchLock(root, "plan.md", "plan", time.Second)
	require.ErrorContains(t, err, "create runs directory")
	assert.NotErrorIs(t, err, ErrLaunchBusy)
}
