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

func TestLaunchLockNormalizesPlanPath(t *testing.T) {
	root := t.TempDir()
	const plan, branch = "docs/plans/x.md", "x"
	for _, spelling := range []string{"./docs/plans/x.md", "docs//plans/x.md", "docs/plans/../plans/x.md", "docs/plans/x.md/"} {
		assert.Equal(t, launchLockPath(root, plan, branch), launchLockPath(root, spelling, branch),
			"%q is the same plan", spelling)
	}
	sum := sha256.Sum256([]byte(plan + "\x00" + branch))
	assert.Equal(t, ".launch-"+hex.EncodeToString(sum[:])[:16]+".lock",
		filepath.Base(launchLockPath(root, "./"+plan, branch)), "the key hashes the cleaned path")
	assert.NotEqual(t, launchLockPath(root, plan, branch), launchLockPath(root, "docs/plans/y.md", branch))

	release, err := AcquireLaunchLock(root, "./"+plan, branch, time.Second)
	require.NoError(t, err)
	inProgress, err := LaunchInProgress(root, plan, branch)
	require.NoError(t, err)
	assert.True(t, inProgress, "a launch spelled ./docs/plans/x.md is in progress for docs/plans/x.md")
	_, err = AcquireLaunchLock(root, plan, branch, 50*time.Millisecond)
	require.ErrorIs(t, err, ErrLaunchBusy, "two spellings of one plan serialize")
	require.NoError(t, release())
}

func TestAcquireLaunchLockErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(root, []byte("x"), 0o600))

	_, err := AcquireLaunchLock(root, "plan.md", "plan", time.Second)
	require.ErrorContains(t, err, "create runs directory")
	assert.NotErrorIs(t, err, ErrLaunchBusy)
}
