//go:build !windows

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/runs"
)

// holdDetachedRun records a run of branch under root and holds its lifetime lock, as a live detached
// run does, until the returned release is called or the test ends.
func holdDetachedRun(t *testing.T, root, id, branch string) (release func()) {
	t.Helper()
	dir := runs.Dir(root, id)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	desc := runs.Descriptor{ID: id, Attempt: 1, Cwd: root, Mode: "full", Plan: "docs/plans/" + branch + ".md",
		Branch: branch, Origin: runs.Origin{Kind: runs.OriginCLI}}
	require.NoError(t, runs.SaveDescriptor(dir, &desc))
	unlock, err := runs.AcquireBounded(filepath.Join(dir, runs.LockFile), time.Second)
	require.NoError(t, err)
	released := false
	release = func() {
		if !released {
			released = true
			require.NoError(t, unlock())
		}
	}
	t.Cleanup(release)
	return release
}

// newGuardRepo returns a repository whose branch feature carries one commit, checked out on master,
// with the .loopai runtime paths ignored as a launch leaves them.
func newGuardRepo(t *testing.T) (string, *git.Service) {
	t.Helper()
	dir := setupTestRepo(t)
	runGit(t, dir, "checkout", "-b", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
	runGit(t, dir, "add", "feature.txt")
	runGit(t, dir, "commit", "-m", "feature")
	runGit(t, dir, "checkout", "master")
	svc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)
	require.NoError(t, svc.EnsureLocalGitignore())
	return dir, svc
}

func TestRefuseLiveDetachedRun(t *testing.T) {
	const id = "feature-20261022-120000-abcd"
	feature := closeoutTarget{identifier: "feature"}
	liveErr := "a detached run of feature is live: " + id + "; stop it with loopai --stop " + id

	t.Run("merge refuses while a live run of the branch holds its lock, then proceeds", func(t *testing.T) {
		dir, svc := newGuardRepo(t)
		release := holdDetachedRun(t, dir, id, "feature")
		master := gitOutput(t, dir, "rev-parse", "master")

		err := runMergeCommand(t.Context(), svc, "master", feature, &recordingStatusClearer{}, io.Discard)
		require.EqualError(t, err, liveErr)
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Equal(t, master, gitOutput(t, dir, "rev-parse", "master"), "nothing is merged")

		release()
		require.NoError(t, runMergeCommand(t.Context(), svc, "master", feature, &recordingStatusClearer{}, io.Discard))
		assert.False(t, branchExists(t, dir, "feature"))
		assert.FileExists(t, filepath.Join(dir, "feature.txt"))
	})

	t.Run("a finished run of the branch does not block", func(t *testing.T) {
		dir, svc := newGuardRepo(t)
		holdDetachedRun(t, dir, id, "feature")()
		require.NoError(t, runs.SaveOutcome(runs.Dir(dir, id),
			&runs.Outcome{RunID: id, Attempt: 1, Status: runs.StatusFailed}))
		holdDetachedRun(t, dir, "other-20261022-120000-abcd", "other")

		require.NoError(t, refuseLiveDetachedRun(t.Context(), svc, "feature"), "a live run of another branch is no concern")
	})

	t.Run("merge from the primary checkout refuses a run recorded in a second worktree", func(t *testing.T) {
		dir, svc := newGuardRepo(t)
		side := filepath.Join(t.TempDir(), "side")
		runGit(t, dir, "worktree", "add", "-b", "side", side)
		holdDetachedRun(t, side, id, "feature")

		err := runMergeCommand(t.Context(), svc, "master", feature, &recordingStatusClearer{}, io.Discard)
		require.EqualError(t, err, liveErr)
		assert.True(t, branchExists(t, dir, "feature"))
	})

	t.Run("merge refuses while only the feature worktree's run lock is held", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		require.NoError(t, svc.EnsureLocalGitignore())
		wtPath := filepath.Join(dir, ".loopai", "worktrees", "feature")
		runGit(t, dir, "worktree", "add", "-b", "feature", wtPath)
		require.NoError(t, os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, wtPath, "add", "feature.txt")
		runGit(t, wtPath, "commit", "-m", "feature")
		wtSvc, err := git.NewService(wtPath, noopLogger())
		require.NoError(t, err)
		unlock, err := wtSvc.AcquireWorktreeRunLock()
		require.NoError(t, err)
		require.NoDirExists(t, runs.RunsDir(dir), "no run directory anywhere")
		require.NoDirExists(t, runs.RunsDir(wtPath))

		err = runMergeCommand(t.Context(), svc, "master", feature, &recordingStatusClearer{}, io.Discard)
		require.ErrorContains(t, err, "a loopai run of feature is live: plan worktree "+wtPath+" is busy")
		require.ErrorContains(t, err, "stop it first (loopai --stop <run-id> for a detached run)")
		assert.DirExists(t, wtPath)
		assert.True(t, branchExists(t, dir, "feature"))

		require.NoError(t, unlock())
		require.NoError(t, runMergeCommand(t.Context(), svc, "master", feature, &recordingStatusClearer{}, io.Discard))
		assert.NoDirExists(t, wtPath)
		assert.False(t, branchExists(t, dir, "feature"))
	})

	t.Run("pr refuses while a live run of the branch holds its lock, then proceeds", func(t *testing.T) {
		dir, _, svc := setupPRFeatureWithRemote(t)
		require.NoError(t, svc.EnsureLocalGitignore())
		release := holdDetachedRun(t, dir, id, "feature")
		binDir := t.TempDir()
		argsLog := filepath.Join(binDir, "gh-args.log")
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nif [ \"$1\" = repo ]; then\n  printf '%s\\n' 'acme/repo'\n"+
			"  exit 0\nfi\nprintf '%s\\n' \"$@\" > \"$GH_ARGS_LOG\"\ncat > /dev/null\n"+
			"printf '%s\\n' 'https://github.com/acme/repo/pull/42'\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GH_ARGS_LOG", argsLog)

		err := runPRCommand(t.Context(), svc, "master", closeoutTarget{}, &recordingStatusClearer{}, io.Discard)
		require.EqualError(t, err, liveErr)
		assert.NoFileExists(t, argsLog, "no pull request is opened")

		release()
		var output bytes.Buffer
		require.NoError(t, runPRCommand(t.Context(), svc, "master", closeoutTarget{}, &recordingStatusClearer{}, &output))
		assert.Equal(t, "https://github.com/acme/repo/pull/42\n", output.String())
	})

	t.Run("a broken run directory is named with its fix", func(t *testing.T) {
		dir, svc := newGuardRepo(t)
		broken := runs.Dir(dir, "other-20261022-120000-abcd")
		require.NoError(t, os.MkdirAll(broken, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(broken, runs.DescriptorFile), []byte("{"), 0o600))

		err := refuseLiveDetachedRun(t.Context(), svc, "feature")
		require.ErrorContains(t, err, "check for a live detached run of feature: ")
		require.ErrorContains(t, err, "run other-20261022-120000-abcd cannot be read: ")
		require.ErrorContains(t, err, "check that no loopai process still uses "+broken+", then remove it")
	})
}
