package git

import (
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

// mockLogger implements Logger interface for testing.
type mockLogger struct {
	logs []string
}

func (m *mockLogger) Printf(format string, args ...any) (int, error) {
	m.logs = append(m.logs, fmt.Sprintf(format, args...))
	return 0, nil
}

// noopLogger returns a no-op logger.
func noopServiceLogger() Logger {
	return &mockLogger{}
}

func TestNewService(t *testing.T) {
	t.Run("opens valid repo", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		assert.NotNil(t, svc)

		// resolve symlinks for consistent path comparison (macOS /var -> /private/var)
		expected, err := filepath.EvalSymlinks(dir)
		require.NoError(t, err)
		assert.Equal(t, expected, svc.Root())
	})

	t.Run("fails on non-repo", func(t *testing.T) {
		dir := t.TempDir()
		_, err := NewService(dir, noopServiceLogger())
		assert.Error(t, err)
	})

	t.Run("accepts custom vcs command", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger(), "git")
		require.NoError(t, err)
		assert.NotNil(t, svc)

		// verify it works normally with explicit "git"
		branch, err := svc.CurrentBranch()
		require.NoError(t, err)
		assert.NotEmpty(t, branch)
	})

	t.Run("defaults to git when vcs command is empty", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger(), "")
		require.NoError(t, err)
		assert.NotNil(t, svc)
	})

	t.Run("defaults to git when no vcs command provided", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		assert.NotNil(t, svc)
	})

	t.Run("fails with invalid vcs command", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		_, err := NewService(dir, noopServiceLogger(), "nonexistent-vcs")
		require.Error(t, err)
	})
}

func TestService_IsDefaultBranch(t *testing.T) {
	t.Run("returns true for master with empty default", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		isDefault, err := svc.IsDefaultBranch("")
		require.NoError(t, err)
		assert.True(t, isDefault)
	})

	t.Run("returns true for main with empty default", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		err = svc.CreateBranch("main")
		require.NoError(t, err)

		isDefault, err := svc.IsDefaultBranch("")
		require.NoError(t, err)
		assert.True(t, isDefault)
	})

	t.Run("returns true for master with explicit default", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		isDefault, err := svc.IsDefaultBranch("master")
		require.NoError(t, err)
		assert.True(t, isDefault)
	})

	t.Run("returns true for develop branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		err = svc.CreateBranch("develop")
		require.NoError(t, err)

		isDefault, err := svc.IsDefaultBranch("develop")
		require.NoError(t, err)
		assert.True(t, isDefault)
	})

	t.Run("returns true for trunk branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		err = svc.CreateBranch("trunk")
		require.NoError(t, err)

		isDefault, err := svc.IsDefaultBranch("trunk")
		require.NoError(t, err)
		assert.True(t, isDefault)
	})

	t.Run("strips origin prefix from default branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// on master, default branch "origin/master" should match after stripping prefix
		isDefault, err := svc.IsDefaultBranch("origin/master")
		require.NoError(t, err)
		assert.True(t, isDefault)
	})

	t.Run("returns false for feature branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		err = svc.CreateBranch("feature-test")
		require.NoError(t, err)

		isDefault, err := svc.IsDefaultBranch("master")
		require.NoError(t, err)
		assert.False(t, isDefault)
	})

	t.Run("returns false for detached HEAD", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		hash, err := svc.HeadHash()
		require.NoError(t, err)

		// checkout commit directly via git CLI to create detached HEAD
		runGit(t, dir, "checkout", hash)

		// re-open service to pick up detached HEAD state
		svc, err = NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		isDefault, err := svc.IsDefaultBranch("")
		require.NoError(t, err)
		assert.False(t, isDefault)
	})
}

func TestService_ResolveBaseBranch(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, dir string)
		explicit string
		want     string
		errText  string
	}{
		{name: "explicit existing branch", explicit: "release", setup: func(t *testing.T, dir string) {
			runGit(t, dir, "branch", "release")
		}, want: "release"},
		{name: "explicit missing branch", explicit: "release", errText: `base branch "release" does not exist`},
		{name: "prefers main", setup: func(t *testing.T, dir string) {
			runGit(t, dir, "branch", "main")
		}, want: "main"},
		{name: "falls back to master", want: "master"},
		{name: "neither conventional branch exists", setup: func(t *testing.T, dir string) {
			runGit(t, dir, "branch", "-m", "unusual")
		}, errText: "base branch not found: neither main nor master exists"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupExternalTestRepo(t)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			svc, err := NewService(dir, noopServiceLogger())
			require.NoError(t, err)

			got, err := svc.ResolveBaseBranch(tt.explicit)
			if tt.errText != "" {
				require.EqualError(t, err, tt.errText)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestService_MergeBranch(t *testing.T) {
	t.Run("clean merge", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		featureHash := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
		runGit(t, dir, "checkout", "master")

		require.NoError(t, svc.MergeBranch("feature"))
		assert.FileExists(t, filepath.Join(dir, "feature.txt"))
		assert.Equal(t, featureHash, strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")))
	})

	t.Run("ignores base branch merge options", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")

		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600))
		runGit(t, dir, "add", "base.txt")
		runGit(t, dir, "commit", "-m", "advance base")
		runGit(t, dir, "config", "branch.master.mergeOptions", "-s ours")

		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		require.NoError(t, svc.MergeBranch("feature"))
		assert.FileExists(t, filepath.Join(dir, "feature.txt"),
			"configured branch merge options must not discard feature content")
	})

	t.Run("uses Git 2.30 compatible config override", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600))
		runGit(t, dir, "add", "base.txt")
		runGit(t, dir, "commit", "-m", "advance base")
		runGit(t, dir, "config", "branch.master.mergeOptions", "-s ours")
		t.Setenv("GIT_CONFIG_PARAMETERS", "")

		command := filepath.Join(t.TempDir(), "old-git")
		script := "#!/bin/sh\nif [ -n \"$GIT_CONFIG_PARAMETERS\" ]; then echo 'error: bogus format in GIT_CONFIG_PARAMETERS' >&2; exit 1; fi\nexec git \"$@\"\n"
		require.NoError(t, os.WriteFile(command, []byte(script), 0o755)) //nolint:gosec // executable test fixture
		svc, err := NewService(dir, noopServiceLogger(), command)
		require.NoError(t, err)

		require.NoError(t, svc.MergeBranch("feature"))
		assert.FileExists(t, filepath.Join(dir, "feature.txt"),
			"the compatibility-safe override must still neutralize configured merge options")
	})

	t.Run("branch wins over a same-named tag", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		runGit(t, dir, "tag", "feature")
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		featureHash := strings.TrimSpace(runGit(t, dir, "rev-parse", "refs/heads/feature"))
		runGit(t, dir, "checkout", "master")
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		require.NoError(t, svc.MergeBranch("feature"))
		assert.Equal(t, featureHash, strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")))
	})

	t.Run("conflict aborts and preserves branches", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("feature\n"), 0o600))
		runGit(t, dir, "commit", "-am", "feature change")
		featureHash := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("master\n"), 0o600))
		runGit(t, dir, "commit", "-am", "master change")
		masterHash := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

		err = svc.MergeBranch("feature")
		require.Error(t, err)
		require.ErrorIs(t, err, ErrMergeConflict)
		assert.Equal(t, masterHash, strings.TrimSpace(runGit(t, dir, "rev-parse", "master")))
		assert.Equal(t, featureHash, strings.TrimSpace(runGit(t, dir, "rev-parse", "feature")))
		assert.Equal(t, "master", strings.TrimSpace(runGit(t, dir, "branch", "--show-current")))
		content, readErr := os.ReadFile(filepath.Join(dir, "README.md")) //nolint:gosec // test repo path
		require.NoError(t, readErr)
		assert.Equal(t, "master\n", string(content))
		assert.Empty(t, strings.TrimSpace(runGit(t, dir, "status", "--porcelain")))
	})

	t.Run("non-conflict commit failure is not reported as a conflict", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")

		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600))
		runGit(t, dir, "add", "base.txt")
		runGit(t, dir, "commit", "-m", "advance base")
		masterHash := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

		hook := filepath.Join(dir, ".git", "hooks", "prepare-commit-msg")
		require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\necho hook rejected merge >&2\nexit 1\n"), 0o755)) //nolint:gosec // executable hook fixture

		err = svc.MergeBranch("feature")
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrMergeConflict)
		assert.Contains(t, err.Error(), "hook rejected merge")
		assert.Equal(t, masterHash, strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")))
		assert.Empty(t, strings.TrimSpace(runGit(t, dir, "status", "--porcelain")))
	})

	t.Run("failed incorporation check restores pre-merge state", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		masterHash := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

		runGit(t, dir, "checkout", "-b", "expected")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "expected.txt"), []byte("expected\n"), 0o600))
		runGit(t, dir, "add", "expected.txt")
		runGit(t, dir, "commit", "-m", "unrelated expected commit")
		expectedHash := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

		runGit(t, dir, "checkout", "master")
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		runGit(t, dir, "checkout", "master")

		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		err = svc.MergeBranchCommitContext(t.Context(), "feature", expectedHash)
		require.ErrorContains(t, err, "without incorporating expected commit")
		assert.Equal(t, masterHash, strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")))
		assert.Empty(t, strings.TrimSpace(runGit(t, dir, "status", "--porcelain")))
		assert.True(t, svc.BranchExists("feature"))
	})
}

func TestService_DeleteBranch(t *testing.T) {
	tests := []struct {
		name   string
		merged bool
	}{
		{name: "deletes merged branch", merged: true},
		{name: "refuses unmerged branch", merged: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupExternalTestRepo(t)
			svc, err := NewService(dir, noopServiceLogger())
			require.NoError(t, err)
			runGit(t, dir, "checkout", "-b", "feature")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
			runGit(t, dir, "add", "feature.txt")
			runGit(t, dir, "commit", "-m", "feature")
			runGit(t, dir, "checkout", "master")
			if tt.merged {
				runGit(t, dir, "merge", "feature")
			}

			err = svc.DeleteBranch("feature")
			if tt.merged {
				require.NoError(t, err)
				assert.False(t, svc.BranchExists("feature"))
				return
			}
			require.Error(t, err)
			assert.True(t, svc.BranchExists("feature"))
		})
	}
}

func TestService_DeleteBranchWhoseNameStartsWithDash(t *testing.T) {
	dir := setupExternalTestRepo(t)
	runGit(t, dir, "update-ref", "refs/heads/-feature", "HEAD")
	svc, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)

	require.NoError(t, svc.DeleteBranch("-feature"))
	assert.False(t, svc.BranchExists("-feature"))
}

func TestService_Push(t *testing.T) {
	t.Run("pushes ordinary branch and records upstream", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		remote := t.TempDir()
		runGit(t, remote, "init", "--bare")
		runGit(t, dir, "remote", "add", "origin", remote)
		runGit(t, dir, "checkout", "-b", "feature")

		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		require.NoError(t, svc.Push("feature"))

		assert.Equal(t, strings.TrimSpace(runGit(t, dir, "rev-parse", "feature")),
			strings.TrimSpace(runGit(t, remote, "rev-parse", "refs/heads/feature")))
		assert.Equal(t, "origin/feature", strings.TrimSpace(runGit(t, dir, "rev-parse", "--abbrev-ref", "feature@{upstream}")))
	})

	t.Run("leading plus remains part of branch name", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		remote := t.TempDir()
		runGit(t, remote, "init", "--bare")
		runGit(t, dir, "remote", "add", "origin", remote)
		runGit(t, dir, "update-ref", "refs/heads/+feature", "HEAD")

		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		require.NoError(t, svc.Push("+feature"))

		assert.Equal(t, strings.TrimSpace(runGit(t, dir, "rev-parse", "refs/heads/+feature")),
			strings.TrimSpace(runGit(t, remote, "rev-parse", "refs/heads/+feature")))
		wrongRef := exec.Command("git", "rev-parse", "--verify", "refs/heads/feature")
		wrongRef.Dir = remote
		require.Error(t, wrongRef.Run(), "the leading plus must not become force-refspec syntax")
	})
}

func TestService_WorktreeInspectionAndSafeRemoval(t *testing.T) {
	dir := setupExternalTestRepo(t)
	worktreePath := filepath.Join(dir, ".loopai", "worktrees", "feature")
	rootSvc, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)
	require.NoError(t, rootSvc.EnsureLocalGitignore())
	runGit(t, dir, "worktree", "add", worktreePath, "-b", "feature")
	svc, err := NewService(worktreePath, noopServiceLogger())
	require.NoError(t, err)
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	resolvedWorktreePath, err := filepath.EvalSymlinks(worktreePath)
	require.NoError(t, err)

	worktrees, err := svc.Worktrees()
	require.NoError(t, err)
	require.Len(t, worktrees, 2)
	assert.Equal(t, resolvedDir, worktrees[0].Path)
	assert.Equal(t, "master", worktrees[0].Branch)
	assert.Equal(t, resolvedWorktreePath, worktrees[1].Path)
	assert.Equal(t, "feature", worktrees[1].Branch)

	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "untracked.txt"), []byte("keep\n"), 0o600))
	dirty, err := svc.IsDirtyAll()
	require.NoError(t, err)
	assert.True(t, dirty)
	err = svc.RemoveWorktreeSafe(worktreePath)
	require.Error(t, err)
	assert.DirExists(t, worktreePath)

	require.NoError(t, os.Remove(filepath.Join(worktreePath, "untracked.txt")))
	mainSvc, err := svc.OpenWorktree(dir)
	require.NoError(t, err)
	require.NoError(t, mainSvc.RemoveWorktreeSafe(worktreePath))
	assert.NoDirExists(t, worktreePath)
}

func TestServiceOpenWorktreeRejectsForeignRepository(t *testing.T) {
	sourceDir := setupExternalTestRepo(t)
	foreignDir := setupExternalTestRepo(t)
	svc, err := NewService(sourceDir, noopServiceLogger())
	require.NoError(t, err)

	opened, err := svc.OpenWorktree(foreignDir)
	require.ErrorIs(t, err, ErrNotSameRepository)
	assert.Nil(t, opened)
}

func TestServiceWorktreePreparationMarkerLifecycle(t *testing.T) {
	dir := setupExternalTestRepo(t)
	svc, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)
	target := filepath.Join(dir, ".loopai", "worktrees", "marker-test")

	marked, err := svc.HasWorktreePreparationMarker(target)
	require.NoError(t, err)
	assert.False(t, marked)
	require.NoError(t, svc.MarkWorktreePreparation(target))
	marked, err = svc.HasWorktreePreparationMarker(target)
	require.NoError(t, err)
	assert.True(t, marked)
	require.Error(t, svc.MarkWorktreePreparation(target), "an active marker must not be overwritten")
	require.NoError(t, svc.ClearWorktreePreparation(target))
	require.NoError(t, svc.ClearWorktreePreparation(target), "clearing an absent marker is idempotent")
	marked, err = svc.HasWorktreePreparationMarker(target)
	require.NoError(t, err)
	assert.False(t, marked)
}

func TestServiceWorktreePreparationMarkerSyncsDirectoryEntries(t *testing.T) {
	dir := setupExternalTestRepo(t)
	svc, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)
	target := filepath.Join(dir, ".loopai", "worktrees", "durable-marker")
	markerPath, err := svc.worktreePreparationMarkerPath(target)
	require.NoError(t, err)

	var synced []string
	syncDir := func(path string) error {
		synced = append(synced, path)
		return nil
	}
	require.NoError(t, svc.markWorktreePreparation(target, syncDir))
	assert.Equal(t, []string{filepath.Dir(markerPath)}, synced)

	synced = nil
	require.NoError(t, svc.clearWorktreePreparation(target, syncDir))
	assert.Equal(t, []string{filepath.Dir(markerPath)}, synced)
}

func TestServiceWorktreePreparationMarkerDirectorySyncErrors(t *testing.T) {
	dir := setupExternalTestRepo(t)
	svc, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)
	target := filepath.Join(dir, ".loopai", "worktrees", "marker-sync-error")
	injected := errors.New("injected directory sync failure")

	err = svc.markWorktreePreparation(target, func(string) error { return injected })
	require.ErrorIs(t, err, injected)
	marked, inspectErr := svc.HasWorktreePreparationMarker(target)
	require.NoError(t, inspectErr)
	assert.False(t, marked, "a marker whose directory entry was not synced must be rolled back")

	require.NoError(t, svc.MarkWorktreePreparation(target))
	err = svc.clearWorktreePreparation(target, func(string) error { return injected })
	require.ErrorIs(t, err, injected)
	marked, inspectErr = svc.HasWorktreePreparationMarker(target)
	require.NoError(t, inspectErr)
	assert.False(t, marked, "the namespace deletion occurred even when its durability sync failed")
}

func TestService_RemoveWorktreeSafeAllowsIgnoredFiles(t *testing.T) {
	dir := setupExternalTestRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "feature")
	runGit(t, dir, "worktree", "add", worktreePath, "-b", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, ".gitignore"), []byte("secret.env\n"), 0o600))
	runGit(t, worktreePath, "add", ".gitignore")
	runGit(t, worktreePath, "commit", "-m", "ignore local secret")
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "secret.env"), []byte("keep me\n"), 0o600))

	mainSvc, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)
	require.NoError(t, mainSvc.RemoveWorktreeSafe(worktreePath))
	assert.NoDirExists(t, worktreePath)
}

func TestService_CreateBranchForPlan(t *testing.T) {
	t.Run("returns nil on feature branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create and switch to feature branch
		err = svc.CreateBranch("feature-test")
		require.NoError(t, err)

		log := &mockLogger{}
		svc.log = log

		err = svc.CreateBranchForPlan(filepath.Join(dir, "docs", "plans", "feature.md"), "master", "")
		require.NoError(t, err)

		// should not have logged anything (no branch created)
		assert.Empty(t, log.logs)

		// should still be on feature-test
		branch, err := svc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "feature-test", branch)
	})

	t.Run("creates branch from plan file name", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "add-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		err = svc.CreateBranchForPlan(planFile, "master", "")
		require.NoError(t, err)

		// should have created branch
		branch, err := svc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "add-feature", branch)

		// should have logged creation
		assert.Len(t, log.logs, 2) // creating branch + committing plan
	})

	t.Run("switches to existing branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create the branch first but stay on master
		err = svc.CreateBranch("existing-feature")
		require.NoError(t, err)
		err = svc.repo.checkoutBranch("master")
		require.NoError(t, err)

		log := &mockLogger{}
		svc.log = log

		// create plan file with matching name
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "existing-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		err = svc.CreateBranchForPlan(planFile, "master", "")
		require.NoError(t, err)

		// should have switched to existing branch
		branch, err := svc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "existing-feature", branch)

		// first log should mention "switching"
		assert.Contains(t, log.logs[0], "switching")
	})

	t.Run("fails with other uncommitted changes", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		// create another uncommitted file
		otherFile := filepath.Join(dir, "other.txt")
		require.NoError(t, os.WriteFile(otherFile, []byte("other content"), 0o600))

		err = svc.CreateBranchForPlan(planFile, "master", "")
		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf(
			"cannot create branch \"feature\": worktree has uncommitted changes\n\n"+
				"uncommitted files:\n  other.txt\n\n"+
				"loopai needs to create a feature branch from master to isolate plan work.\n\n"+
				"options:\n"+
				"  git stash && loopai %s && git stash pop   # stash changes temporarily\n"+
				"  git commit -am \"wip\"                       # commit changes first\n"+
				"  loopai --review                            # skip branch creation (review-only mode)",
			planFile), err.Error())
	})

	t.Run("auto-commits plan file if only dirty file", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create untracked plan file (the only dirty file)
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "new-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# New Feature Plan"), 0o600))

		err = svc.CreateBranchForPlan(planFile, "master", "")
		require.NoError(t, err)

		// should have created branch and committed plan
		assert.Len(t, log.logs, 2)
		assert.Contains(t, log.logs[1], "committing plan file")

		// verify plan was committed
		hasChanges, err := svc.repo.fileHasChanges(planFile)
		require.NoError(t, err)
		assert.False(t, hasChanges, "plan file should be committed")
	})

	t.Run("does not commit if plan already committed", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create and commit plan file while on master
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "committed-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))

		log := &mockLogger{}
		svc.log = log

		err = svc.CreateBranchForPlan(planFile, "master", "")
		require.NoError(t, err)

		// should only have one log (creating branch, no committing)
		assert.Len(t, log.logs, 1)
		assert.Contains(t, log.logs[0], "creating branch")
	})

	t.Run("strips date prefix from branch name", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create plan file with date prefix
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "2024-01-15-add-auth.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		err = svc.CreateBranchForPlan(planFile, "master", "")
		require.NoError(t, err)

		// branch name should not have date prefix
		branch, err := svc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "add-auth", branch)
	})

	t.Run("creates branch from develop as default branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// switch to develop branch (simulating gitflow default)
		require.NoError(t, svc.CreateBranch("develop"))

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "gitflow-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		log := &mockLogger{}
		svc.log = log

		err = svc.CreateBranchForPlan(planFile, "develop", "")
		require.NoError(t, err)

		branch, err := svc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "gitflow-feature", branch)
		assert.Len(t, log.logs, 2) // creating branch + committing plan
	})

	t.Run("skips branch creation with origin prefix default", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// switch to feature branch
		require.NoError(t, svc.CreateBranch("feature-x"))

		log := &mockLogger{}
		svc.log = log

		// default branch is "origin/master" but we're on feature-x, should skip
		err = svc.CreateBranchForPlan(filepath.Join(dir, "docs", "plans", "feature.md"), "origin/master", "")
		require.NoError(t, err)
		assert.Empty(t, log.logs) // no branch created
	})

	t.Run("commits plan file with case-mismatched path", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create plan file with specific case
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "Branch-Case.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Branch Case Plan"), 0o600))

		// call CreateBranchForPlan with lowercase path (different case)
		lowercasePlan := filepath.Join(plansDir, "branch-case.md")
		err = svc.CreateBranchForPlan(lowercasePlan, "master", "")
		require.NoError(t, err, "should succeed despite case mismatch in plan file path")

		// verify branch created (name derived from resolved on-disk case)
		branch, err := svc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "Branch-Case", branch)

		// verify plan was committed (no uncommitted changes)
		hasChanges, err := svc.repo.fileHasChanges(planFile)
		require.NoError(t, err)
		assert.False(t, hasChanges, "plan file should be committed")
	})

	t.Run("branch override used instead of plan filename", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, &mockLogger{})
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "2026-04-30-some-long-generated-name.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		err = svc.CreateBranchForPlan(planFile, "master", "my-custom-branch")
		require.NoError(t, err)

		branch, err := svc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "my-custom-branch", branch)
	})
}

func TestService_MovePlanToCompleted(t *testing.T) {
	t.Run("moves tracked file", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create and commit plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))

		log := &mockLogger{}
		svc.log = log

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		// original file should not exist
		_, err = os.Stat(planFile)
		assert.True(t, os.IsNotExist(err))

		// completed file should exist
		completedPath := filepath.Join(plansDir, "completed", "feature.md")
		_, err = os.Stat(completedPath)
		require.NoError(t, err)

		// should have logged the move
		assert.Len(t, log.logs, 1)
		assert.Contains(t, log.logs[0], "moved plan")
	})

	t.Run("moves untracked file", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create untracked plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "untracked-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		// original file should not exist
		_, err = os.Stat(planFile)
		assert.True(t, os.IsNotExist(err))

		// completed file should exist
		completedPath := filepath.Join(plansDir, "completed", "untracked-feature.md")
		_, err = os.Stat(completedPath)
		require.NoError(t, err)
	})

	t.Run("creates completed directory", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))

		// verify completed dir doesn't exist
		completedDir := filepath.Join(plansDir, "completed")
		_, err = os.Stat(completedDir)
		require.True(t, os.IsNotExist(err))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		// completed dir should now exist
		info, err := os.Stat(completedDir)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("returns nil if already moved to completed", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create completed directory with plan file already there (simulating prior move)
		plansDir := filepath.Join(dir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o750))
		completedPath := filepath.Join(completedDir, "already-moved.md")
		require.NoError(t, os.WriteFile(completedPath, []byte("# Plan"), 0o600))

		// source file does not exist
		planFile := filepath.Join(plansDir, "already-moved.md")
		_, err = os.Stat(planFile)
		require.True(t, os.IsNotExist(err))

		// should return nil (not error)
		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		// should have logged skip message
		require.Len(t, log.logs, 1)
		assert.Contains(t, log.logs[0], "already in completed")
	})

	t.Run("returns nil if renamed to compact date in completed", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// simulate prior move that also renamed dashed → compact date prefix
		plansDir := filepath.Join(dir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o750))
		completedPath := filepath.Join(completedDir, "20260512-foo.md")
		require.NoError(t, os.WriteFile(completedPath, []byte("# Plan"), 0o600))

		// caller still references the original dashed-format path
		planFile := filepath.Join(plansDir, "2026-05-12-foo.md")
		_, err = os.Stat(planFile)
		require.True(t, os.IsNotExist(err))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		require.Len(t, log.logs, 1)
		assert.Contains(t, log.logs[0], "already in completed")
		assert.Contains(t, log.logs[0], "renamed")
		assert.Contains(t, log.logs[0], "20260512-foo.md")
	})

	t.Run("returns nil if renamed to dashed date in completed", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// simulate prior move that also renamed compact → dashed date prefix
		plansDir := filepath.Join(dir, "docs", "plans")
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o750))
		completedPath := filepath.Join(completedDir, "2026-05-12-foo.md")
		require.NoError(t, os.WriteFile(completedPath, []byte("# Plan"), 0o600))

		// caller still references the original compact-format path
		planFile := filepath.Join(plansDir, "20260512-foo.md")
		_, err = os.Stat(planFile)
		require.True(t, os.IsNotExist(err))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		require.Len(t, log.logs, 1)
		assert.Contains(t, log.logs[0], "already in completed")
		assert.Contains(t, log.logs[0], "renamed")
		assert.Contains(t, log.logs[0], "2026-05-12-foo.md")
	})

	t.Run("moves file renamed in place to compact date", func(t *testing.T) {
		// caller references original dashed path, file was renamed in place to compact
		// e.g. git mv docs/plans/2026-05-12-foo.md docs/plans/20260512-foo.md
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		renamedPath := filepath.Join(plansDir, "20260512-foo.md")
		require.NoError(t, os.WriteFile(renamedPath, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(renamedPath))
		require.NoError(t, svc.repo.commit("add plan with renamed basename"))

		// caller still passes the original dashed path
		planFile := filepath.Join(plansDir, "2026-05-12-foo.md")
		_, err = os.Stat(planFile)
		require.True(t, os.IsNotExist(err))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		// renamed source should be gone
		_, err = os.Stat(renamedPath)
		assert.True(t, os.IsNotExist(err))

		// destination uses the renamed basename
		completedPath := filepath.Join(plansDir, "completed", "20260512-foo.md")
		_, err = os.Stat(completedPath)
		require.NoError(t, err)
	})

	t.Run("moves file renamed in place to dashed date", func(t *testing.T) {
		// mirror: caller references compact, file renamed in place to dashed
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		renamedPath := filepath.Join(plansDir, "2026-05-12-foo.md")
		require.NoError(t, os.WriteFile(renamedPath, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(renamedPath))
		require.NoError(t, svc.repo.commit("add plan with renamed basename"))

		planFile := filepath.Join(plansDir, "20260512-foo.md")
		_, err = os.Stat(planFile)
		require.True(t, os.IsNotExist(err))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		_, err = os.Stat(renamedPath)
		assert.True(t, os.IsNotExist(err))

		completedPath := filepath.Join(plansDir, "completed", "2026-05-12-foo.md")
		_, err = os.Stat(completedPath)
		require.NoError(t, err)
	})

	t.Run("in-place rename wins over stale completed copy", func(t *testing.T) {
		// caller passes original dashed path. file was renamed in place to compact AND a stale
		// completed/<original-basename> exists from a prior run. the in-place rename is the
		// active plan and must be moved; the stale completed/ copy must not short-circuit.
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))

		// active plan at compact basename (renamed in place)
		renamedPath := filepath.Join(plansDir, "20260512-foo.md")
		require.NoError(t, os.WriteFile(renamedPath, []byte("# Plan (current)"), 0o600))
		require.NoError(t, svc.repo.add(renamedPath))
		require.NoError(t, svc.repo.commit("add plan with renamed basename"))

		// stale completed copy at original (dashed) basename from a prior run
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o750))
		stalePath := filepath.Join(completedDir, "2026-05-12-foo.md")
		require.NoError(t, os.WriteFile(stalePath, []byte("# Plan (stale)"), 0o600))

		// caller still references the original dashed path
		planFile := filepath.Join(plansDir, "2026-05-12-foo.md")
		_, err = os.Stat(planFile)
		require.True(t, os.IsNotExist(err))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		// renamed source should be gone (was moved, not abandoned)
		_, err = os.Stat(renamedPath)
		assert.True(t, os.IsNotExist(err), "active in-place renamed file should have been moved")

		// destination uses the renamed basename
		movedPath := filepath.Join(completedDir, "20260512-foo.md")
		movedContent, err := os.ReadFile(movedPath) //nolint:gosec // test file
		require.NoError(t, err)
		assert.Equal(t, "# Plan (current)", string(movedContent), "moved file should contain current content")

		// stale completed copy is left in place (not our responsibility to clean up)
		staleContent, err := os.ReadFile(stalePath) //nolint:gosec // test file
		require.NoError(t, err)
		assert.Equal(t, "# Plan (stale)", string(staleContent))
	})

	t.Run("collision between in-place rename and stale completed/<altBase> is left untouched", func(t *testing.T) {
		// caller passes original dashed path. file was renamed in place to compact AND a stale
		// completed/<altBase> copy already exists with the same basename (e.g. same slug ran
		// twice on the same day). git mv would refuse, os.Rename fallback would clobber the
		// stale copy and leave the source's deletion unstaged. verify we surface this as
		// already-completed and preserve both files for manual resolution.
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))

		// active plan at compact basename (renamed in place, tracked)
		renamedPath := filepath.Join(plansDir, "20260512-foo.md")
		require.NoError(t, os.WriteFile(renamedPath, []byte("# Plan (current)"), 0o600))
		require.NoError(t, svc.repo.add(renamedPath))
		require.NoError(t, svc.repo.commit("add plan with renamed basename"))

		// stale completed copy at compact basename from a prior run with same slug+date
		completedDir := filepath.Join(plansDir, "completed")
		require.NoError(t, os.MkdirAll(completedDir, 0o750))
		stalePath := filepath.Join(completedDir, "20260512-foo.md")
		require.NoError(t, os.WriteFile(stalePath, []byte("# Plan (stale)"), 0o600))

		// caller references the original dashed path
		planFile := filepath.Join(plansDir, "2026-05-12-foo.md")
		_, err = os.Stat(planFile)
		require.True(t, os.IsNotExist(err))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		// active source must be preserved (NOT clobbered, NOT moved)
		activeContent, err := os.ReadFile(renamedPath) //nolint:gosec // test file
		require.NoError(t, err)
		assert.Equal(t, "# Plan (current)", string(activeContent), "active in-place renamed file must be preserved")

		// stale completed copy must also be preserved (not overwritten)
		staleContent, err := os.ReadFile(stalePath) //nolint:gosec // test file
		require.NoError(t, err)
		assert.Equal(t, "# Plan (stale)", string(staleContent), "stale completed copy must be preserved")

		// repo must be clean — no dangling deletion of the active source
		dirty, err := svc.repo.isDirty()
		require.NoError(t, err)
		assert.False(t, dirty, "repo must be clean after collision-skip")

		// should have logged that the move was skipped due to the collision
		require.Len(t, log.logs, 1)
		assert.Contains(t, log.logs[0], "already in completed")
		assert.Contains(t, log.logs[0], "20260512-foo.md")
		assert.Contains(t, log.logs[0], "manual cleanup")
	})
}

func TestService_EnsureHasCommits(t *testing.T) {
	t.Run("returns nil when repo has commits", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		promptCalled := false
		promptFn := func() bool {
			promptCalled = true
			return true
		}

		err = svc.EnsureHasCommits(promptFn)
		require.NoError(t, err)

		// prompt should not have been called
		assert.False(t, promptCalled)
	})

	t.Run("creates initial commit when user accepts", func(t *testing.T) {
		// create empty repo (no commits)
		dir := t.TempDir()
		runGit(t, dir, "init")
		runGit(t, dir, "config", "user.email", "test@test.com")
		runGit(t, dir, "config", "user.name", "test")
		runGit(t, dir, "config", "commit.gpgsign", "false")

		// create a file to commit
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test"), 0o600))

		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		promptCalled := false
		promptFn := func() bool {
			promptCalled = true
			return true
		}

		err = svc.EnsureHasCommits(promptFn)
		require.NoError(t, err)

		// prompt should have been called
		assert.True(t, promptCalled)

		// repo should now have commits
		hasCommits, err := svc.HasCommits()
		require.NoError(t, err)
		assert.True(t, hasCommits)
	})

	t.Run("creates initial commit with trailer when configured", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init")
		runGit(t, dir, "config", "user.email", "test@test.com")
		runGit(t, dir, "config", "user.name", "test")
		runGit(t, dir, "config", "commit.gpgsign", "false")

		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test"), 0o600))

		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		svc.SetCommitTrailer("Co-authored-by: ralphex <noreply@ralphex.com>")

		err = svc.EnsureHasCommits(func() bool { return true })
		require.NoError(t, err)

		// verify trailer in commit message
		out := runGit(t, dir, "log", "-1", "--format=%B")
		assert.Contains(t, out, "Co-authored-by: ralphex <noreply@ralphex.com>")
	})

	t.Run("returns error when user declines", func(t *testing.T) {
		// create empty repo (no commits)
		dir := t.TempDir()
		runGit(t, dir, "init")
		runGit(t, dir, "config", "user.email", "test@test.com")
		runGit(t, dir, "config", "user.name", "test")
		runGit(t, dir, "config", "commit.gpgsign", "false")

		// create a file so we're not completely empty
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test"), 0o600))

		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		promptFn := func() bool { return false }

		err = svc.EnsureHasCommits(promptFn)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no commits")
	})

	t.Run("returns error when no files to commit", func(t *testing.T) {
		// create empty repo with no files
		dir := t.TempDir()
		runGit(t, dir, "init")
		runGit(t, dir, "config", "user.email", "test@test.com")
		runGit(t, dir, "config", "user.name", "test")
		runGit(t, dir, "config", "commit.gpgsign", "false")

		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		promptFn := func() bool { return true }

		err = svc.EnsureHasCommits(promptFn)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no files to commit")
	})
}

func TestService_EnsureLocalGitignore(t *testing.T) {
	t.Run("rejects symlinked .loopai directory", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, ".loopai")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		err = svc.EnsureLocalGitignore()
		require.ErrorContains(t, err, "symbolic link")
		assert.NoFileExists(t, filepath.Join(outside, ".gitignore"))
		assert.NoDirExists(t, filepath.Join(outside, "progress"))
		assert.NoDirExists(t, filepath.Join(outside, "worktrees"))
	})

	t.Run("creates .loopai/.gitignore", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		err = svc.EnsureLocalGitignore()
		require.NoError(t, err)
		assert.Len(t, log.logs, 1)
		assert.Contains(t, log.logs[0], ".loopai/.gitignore")

		gitignorePath := filepath.Join(dir, ".loopai", ".gitignore")
		content, err := os.ReadFile(gitignorePath) //nolint:gosec // test file
		require.NoError(t, err)
		assert.Equal(t, ".gitignore\nprogress/\nworktrees/\n", string(content))
	})

	t.Run("idempotent when content matches", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai"), 0o750))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, ".loopai", ".gitignore"),
			[]byte(".gitignore\nprogress/\nworktrees/\n"), 0o600))

		err = svc.EnsureLocalGitignore()
		require.NoError(t, err)
		assert.Empty(t, log.logs, "should not log when content already matches")
	})

	t.Run("preserves custom tracked content and installs local excludes", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai"), 0o750))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, ".loopai", ".gitignore"),
			[]byte("custom-rule\n"), 0o600))
		runGit(t, dir, "add", ".loopai/.gitignore")
		runGit(t, dir, "commit", "-m", "add custom loopai ignore")

		err = svc.EnsureLocalGitignore()
		require.NoError(t, err)
		assert.Len(t, log.logs, 1)
		assert.Contains(t, log.logs[0], "preserved custom")

		content, err := os.ReadFile(filepath.Join(dir, ".loopai", ".gitignore")) //nolint:gosec // test file
		require.NoError(t, err)
		assert.Equal(t, "custom-rule\n", string(content))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "progress"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "progress", "run.log"), []byte("log"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "worktrees", "feature"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "worktrees", "feature", "file"), []byte("data"), 0o600))
		assert.Empty(t, runGit(t, dir, "status", "--porcelain"))
	})

	t.Run("custom negations cannot expose runtime artifacts", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai"), 0o750))
		custom := ".gitignore\n!progress/\n!progress/**\n!worktrees/\n!worktrees/**\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", ".gitignore"), []byte(custom), 0o600))
		runGit(t, dir, "add", "-f", ".loopai/.gitignore")
		runGit(t, dir, "commit", "-m", "add custom loopai ignore")

		require.NoError(t, svc.EnsureLocalGitignore())
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "progress", "run.log"), []byte("secret\n"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "worktrees", "feature"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "worktrees", "feature", "file"), []byte("artifact\n"), 0o600))

		assert.Empty(t, runGit(t, dir, "status", "--porcelain"))
		content, readErr := os.ReadFile(filepath.Join(dir, ".loopai", ".gitignore")) //nolint:gosec // test file
		require.NoError(t, readErr)
		assert.Equal(t, custom, string(content))
	})

	t.Run("preserves tracked runtime directory ignore files", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "progress"), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "worktrees"), 0o750))
		customRoot := ".gitignore\n!progress/\n!progress/**\n!worktrees/\n!worktrees/**\n"
		progressRules := "*.keep\n"
		worktreeRules := "# project-owned\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", ".gitignore"), []byte(customRoot), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "progress", ".gitignore"), []byte(progressRules), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "worktrees", ".gitignore"), []byte(worktreeRules), 0o600))
		runGit(t, dir, "add", "-f", ".loopai/.gitignore", ".loopai/progress/.gitignore", ".loopai/worktrees/.gitignore")
		runGit(t, dir, "commit", "-m", "add project runtime ignore rules")

		require.NoError(t, svc.EnsureLocalGitignore())
		progressContent, err := os.ReadFile(filepath.Join(dir, ".loopai", "progress", ".gitignore")) //nolint:gosec // test file
		require.NoError(t, err)
		assert.Equal(t, progressRules, string(progressContent))
		worktreeContent, err := os.ReadFile(filepath.Join(dir, ".loopai", "worktrees", ".gitignore")) //nolint:gosec // test file
		require.NoError(t, err)
		assert.Equal(t, worktreeRules, string(worktreeContent))
		assert.Empty(t, runGit(t, dir, "status", "--porcelain"))
	})

	t.Run("creates .loopai dir if missing", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		_, err = os.Stat(filepath.Join(dir, ".loopai"))
		assert.True(t, os.IsNotExist(err))

		err = svc.EnsureLocalGitignore()
		require.NoError(t, err)

		info, err := os.Stat(filepath.Join(dir, ".loopai"))
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("does not modify root .gitignore", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		rootGitignore := filepath.Join(dir, ".gitignore")
		_, err = os.Stat(rootGitignore)
		rootExistedBefore := !os.IsNotExist(err)

		err = svc.EnsureLocalGitignore()
		require.NoError(t, err)

		if !rootExistedBefore {
			_, err = os.Stat(rootGitignore)
			assert.True(t, os.IsNotExist(err), "root .gitignore should not be created")
		}
	})
}

func TestService_GetDefaultBranch(t *testing.T) {
	t.Run("returns detected default branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		branch := svc.GetDefaultBranch()
		assert.Equal(t, "master", branch)
	})

	t.Run("returns main when main branch exists", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create main branch
		err = svc.CreateBranch("main")
		require.NoError(t, err)

		branch := svc.GetDefaultBranch()
		assert.Equal(t, "main", branch)
	})
}

func TestService_DiffStats(t *testing.T) {
	t.Run("returns zero stats when on same branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		stats, err := svc.DiffStats("master")
		require.NoError(t, err)
		assert.Equal(t, 0, stats.Files)
		assert.Equal(t, 0, stats.Additions)
		assert.Equal(t, 0, stats.Deletions)
	})

	t.Run("returns zero stats for nonexistent branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		stats, err := svc.DiffStats("nonexistent")
		require.NoError(t, err)
		assert.Equal(t, 0, stats.Files)
	})

	t.Run("returns stats for changes on feature branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create feature branch
		err = svc.CreateBranch("feature")
		require.NoError(t, err)

		// add a new file
		newFile := filepath.Join(dir, "feature.txt")
		require.NoError(t, os.WriteFile(newFile, []byte("line1\nline2\n"), 0o600))
		require.NoError(t, svc.repo.add("feature.txt"))
		require.NoError(t, svc.repo.commit("add feature file"))

		stats, err := svc.DiffStats("master")
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Files)
		assert.Equal(t, 2, stats.Additions)
		assert.Equal(t, 0, stats.Deletions)
	})

	t.Run("local branch wins over a same-named tag", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		require.NoError(t, svc.CreateBranch("feature"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("line1\nline2\n"), 0o600))
		require.NoError(t, svc.repo.add("feature.txt"))
		require.NoError(t, svc.repo.commit("add feature file"))
		runGit(t, dir, "tag", "master")

		stats, err := svc.DiffStats("master")
		require.NoError(t, err)
		assert.Equal(t, DiffStats{Files: 1, Additions: 2}, stats)
	})

	t.Run("returns stats using commit hash as base ref", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// get initial commit hash to use as base ref
		baseHash := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

		// create feature branch with changes
		err = svc.CreateBranch("feature")
		require.NoError(t, err)

		newFile := filepath.Join(dir, "feature.txt")
		require.NoError(t, os.WriteFile(newFile, []byte("line1\nline2\nline3\n"), 0o600))
		require.NoError(t, svc.repo.add("feature.txt"))
		require.NoError(t, svc.repo.commit("add feature file"))

		// use commit hash instead of branch name
		stats, err := svc.DiffStats(baseHash)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Files)
		assert.Equal(t, 3, stats.Additions)
		assert.Equal(t, 0, stats.Deletions)

		// also works with short hash (7 chars)
		shortHash := baseHash[:7]
		stats, err = svc.DiffStats(shortHash)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Files)
		assert.Equal(t, 3, stats.Additions)
		assert.Equal(t, 0, stats.Deletions)
	})
}

func TestService_CreateWorktreeForPlan(t *testing.T) {
	t.Run("creates worktree with new branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "add-worktree.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit, "untracked plan file should need commit")
		assert.Contains(t, wtPath, filepath.Join(".loopai", "worktrees", "add-worktree"))

		// verify worktree exists and is on the correct branch
		wtSvc, err := NewService(wtPath, noopServiceLogger())
		require.NoError(t, err)
		branch, err := wtSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "add-worktree", branch)

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})

	t.Run("creates worktree with existing branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create plan file with matching name
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "existing-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))

		// create the existing branch at the source HEAD, then return to master
		require.NoError(t, svc.CreateBranch("existing-feature"))
		branchHeadBefore, err := svc.repo.headHash()
		require.NoError(t, err)
		require.NoError(t, svc.repo.checkoutBranch("master"))

		log := &mockLogger{}
		svc.log = log

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.False(t, planNeedsCommit, "already-committed plan file should not need commit")

		// verify worktree uses existing branch
		wtSvc, err := NewService(wtPath, noopServiceLogger())
		require.NoError(t, err)
		branch, err := wtSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "existing-feature", branch)
		branchHeadAfter, err := wtSvc.repo.headHash()
		require.NoError(t, err)
		assert.Equal(t, branchHeadBefore, branchHeadAfter, "up-to-date reuse must not create a commit")

		assert.Contains(t, log.logs[0], "existing branch")

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})

	t.Run("existing branch worktree creation honors cancellation", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		planFile := filepath.Join(dir, "docs", "plans", "cancel-add.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan\n"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add cancellation plan"))
		require.NoError(t, svc.CreateBranch("cancel-add"))
		require.NoError(t, svc.repo.checkoutBranch("master"))

		command := filepath.Join(t.TempDir(), "slow-git")
		script := "#!/bin/sh\nif [ \"$1\" = worktree ] && [ \"$2\" = add ]; then git \"$@\" || exit $?; sleep 30; exit 0; fi\nexec git \"$@\"\n"
		require.NoError(t, os.WriteFile(command, []byte(script), 0o755)) //nolint:gosec // executable test fixture
		svc, err = NewService(dir, noopServiceLogger(), command)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		started := time.Now()
		_, _, err = svc.CreateWorktreeForPlanContext(ctx, planFile, "")

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 8*time.Second,
			"cancellation must stop well before the command's 30-second fixture delay")
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "cancel-add"))
	})

	t.Run("auto-commit preflight excludes tracked runtime changes", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		planFile := filepath.Join(dir, "docs", "plans", "runtime-conflict.md")
		runtimeFile := filepath.Join(dir, ".loopai", "progress", "run.log")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Dir(runtimeFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan\n"), 0o600))
		require.NoError(t, os.WriteFile(runtimeFile, []byte("base\n"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.add(runtimeFile))
		require.NoError(t, svc.repo.commit("add plan and tracked runtime file"))
		require.NoError(t, svc.CreateBranch("runtime-conflict"))
		require.NoError(t, os.WriteFile(runtimeFile, []byte("branch version\n"), 0o600))
		require.NoError(t, svc.repo.commitFiles("change runtime file on plan branch", runtimeFile))
		require.NoError(t, svc.repo.checkoutBranch("master"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "source.txt"), []byte("source\n"), 0o600))
		require.NoError(t, svc.repo.add("source.txt"))
		require.NoError(t, svc.repo.commit("advance source"))
		require.NoError(t, os.WriteFile(runtimeFile, []byte("dirty source version\n"), 0o600))

		require.NoError(t, svc.PreflightWorktreeForPlanAutoCommitContext(t.Context(), planFile, ""))
		committed, err := svc.AutoCommitAll("must ignore runtime-only changes")
		require.NoError(t, err)
		assert.False(t, committed)
		assert.Contains(t, runGit(t, dir, "status", "--porcelain"), ".loopai/progress/run.log")
	})

	t.Run("auto-commit conflict describes dirty source changes when branch contains HEAD", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		planFile := filepath.Join(dir, "docs", "plans", "ahead-conflict.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan\n"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))
		sourceHead, err := svc.repo.headHash()
		require.NoError(t, err)
		require.NoError(t, svc.CreateBranch("ahead-conflict"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("plan version\n"), 0o600))
		require.NoError(t, svc.repo.commitFiles("change README on plan branch", "README.md"))
		require.NoError(t, svc.repo.checkoutBranch("master"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty source version\n"), 0o600))

		err = svc.PreflightWorktreeForPlanAutoCommitContext(t.Context(), planFile, "")

		require.ErrorContains(t, err, "auto-committing the source changes")
		require.ErrorContains(t, err, "would conflict")
		assert.NotContains(t, err.Error(), "does not include current HEAD")
		runGit(t, dir, "merge-base", "--is-ancestor", sourceHead, "refs/heads/ahead-conflict")
		assert.Contains(t, runGit(t, dir, "status", "--porcelain"), "README.md",
			"preflight must leave dirty source changes untouched")
	})

	t.Run("clean auto-commit preflight does not require author identity", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		planFile := filepath.Join(dir, "docs", "plans", "identity-free.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan\n"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add identity-free plan"))
		require.NoError(t, svc.CreateBranch("identity-free"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "branch.txt"), []byte("branch\n"), 0o600))
		require.NoError(t, svc.repo.add("branch.txt"))
		require.NoError(t, svc.repo.commit("advance plan branch"))
		require.NoError(t, svc.repo.checkoutBranch("master"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "source.txt"), []byte("source\n"), 0o600))
		require.NoError(t, svc.repo.add("source.txt"))
		require.NoError(t, svc.repo.commit("advance source branch"))
		runGit(t, dir, "config", "--unset", "user.name")
		runGit(t, dir, "config", "--unset", "user.email")

		command := filepath.Join(t.TempDir(), "identity-free-git")
		script := "#!/bin/sh\nunset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL\nexport GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1\nexec git \"$@\"\n"
		require.NoError(t, os.WriteFile(command, []byte(script), 0o755)) //nolint:gosec // executable test fixture
		svc, err = NewService(dir, noopServiceLogger(), command)
		require.NoError(t, err)

		require.NoError(t, svc.PreflightWorktreeForPlanAutoCommitContext(t.Context(), planFile, ""))
	})

	t.Run("merges clean divergence into existing branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		const branchName = "stale=a'b"
		require.NoError(t, svc.CreateBranch(branchName))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "branch-only.txt"), []byte("branch work\n"), 0o600))
		require.NoError(t, svc.repo.add("branch-only.txt"))
		require.NoError(t, svc.repo.commit("add branch work"))
		branchHead, err := svc.repo.headHash()
		require.NoError(t, err)
		runGit(t, dir, "config", "branch."+branchName+".mergeOptions", "-s ours")
		require.NoError(t, svc.repo.checkoutBranch("master"))
		planFile := filepath.Join(dir, "docs", "plans", "stale-feature.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("advance source with plan"))
		sourceHead, err := svc.repo.headHash()
		require.NoError(t, err)

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, branchName)
		require.NoError(t, err)
		assert.False(t, planNeedsCommit)
		defer svc.RemoveWorktree(wtPath) //nolint:errcheck // test cleanup

		runGit(t, wtPath, "merge-base", "--is-ancestor", branchHead, "HEAD")
		runGit(t, wtPath, "merge-base", "--is-ancestor", sourceHead, "HEAD")
		assert.Equal(t, "branch work", strings.TrimSpace(runGit(t, wtPath, "show", "HEAD:branch-only.txt")))
		assert.Equal(t, "# Plan", strings.TrimSpace(runGit(t, wtPath, "show", "HEAD:docs/plans/stale-feature.md")),
			"configured branch merge options must not discard source changes")
		assert.Equal(t, branchHead, strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD^1")))
		assert.Equal(t, sourceHead, strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD^2")))
		assert.Contains(t, strings.Join(log.logs, ""),
			"merging source HEAD "+sourceHead[:7]+" into existing plan branch "+branchName)
	})

	t.Run("reuses stale branch when same-named tag contains current HEAD", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		require.NoError(t, svc.CreateBranch("ambiguous-feature"))
		require.NoError(t, svc.repo.checkoutBranch("master"))
		planFile := filepath.Join(dir, "docs", "plans", "ambiguous-feature.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("advance source with plan"))
		runGit(t, dir, "tag", "ambiguous-feature")
		sourceHead, err := svc.repo.headHash()
		require.NoError(t, err)

		wtPath, _, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		defer svc.RemoveWorktree(wtPath) //nolint:errcheck // test cleanup
		assert.Equal(t, sourceHead, strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD")))
		assert.Equal(t, sourceHead, strings.TrimSpace(runGit(t, dir, "rev-parse", "refs/heads/ambiguous-feature")))
	})

	t.Run("rejects conflicting divergence during preflight", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		planFile := filepath.Join(dir, "docs", "plans", "conflicting-feature.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))
		require.NoError(t, svc.CreateBranch("conflicting-feature"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("branch version\n"), 0o600))
		require.NoError(t, svc.repo.commitFiles("change README on plan branch", "README.md"))
		branchHead := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
		require.NoError(t, svc.repo.checkoutBranch("master"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("source version\n"), 0o600))
		require.NoError(t, svc.repo.commitFiles("change README on source", "README.md"))
		sourceHead := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

		err = svc.PreflightWorktreeForPlan(planFile, "")
		require.ErrorContains(t, err, "would conflict; merge or rebase the source changes into it")
		_, _, createErr := svc.CreateWorktreeForPlan(planFile, "")
		require.ErrorContains(t, createErr, "would conflict")
		assert.Equal(t, sourceHead, strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")))
		assert.Equal(t, branchHead, strings.TrimSpace(runGit(t, dir, "rev-parse", "conflicting-feature")))
		assert.Empty(t, strings.TrimSpace(runGit(t, dir, "status", "--porcelain")))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "conflicting-feature"))
	})

	t.Run("removes worktree when fallback merge finds a conflict", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		command := filepath.Join(t.TempDir(), "old-git")
		script := "#!/bin/sh\nif [ \"$1\" = \"merge-tree\" ]; then echo \"error: unknown option 'write-tree'\" >&2; exit 129; fi\nexec git \"$@\"\n"
		require.NoError(t, os.WriteFile(command, []byte(script), 0o755)) //nolint:gosec // executable test fixture
		svc, err := NewService(dir, noopServiceLogger(), command)
		require.NoError(t, err)

		planFile := filepath.Join(dir, "docs", "plans", "fallback-conflict.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))
		require.NoError(t, svc.CreateBranch("fallback-conflict"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("branch version\n"), 0o600))
		require.NoError(t, svc.repo.commitFiles("change README on plan branch", "README.md"))
		require.NoError(t, svc.repo.checkoutBranch("master"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("source version\n"), 0o600))
		require.NoError(t, svc.repo.commitFiles("change README on source", "README.md"))

		_, _, err = svc.CreateWorktreeForPlan(planFile, "")
		require.Error(t, err)
		require.ErrorIs(t, err, ErrMergeConflict)
		require.ErrorContains(t, err, "merge source HEAD into existing plan branch")
		wtPath := filepath.Join(dir, ".loopai", "worktrees", "fallback-conflict")
		assert.NoDirExists(t, wtPath)
		assert.NotContains(t, runGit(t, dir, "worktree", "list", "--porcelain"), wtPath)
	})

	t.Run("propagates ordinary merge-tree failures without creating a worktree", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		command := filepath.Join(t.TempDir(), "broken-git")
		script := "#!/bin/sh\nif [ \"$1\" = \"merge-tree\" ]; then echo \"fatal: bad revision\" >&2; exit 128; fi\nexec git \"$@\"\n"
		require.NoError(t, os.WriteFile(command, []byte(script), 0o755)) //nolint:gosec // executable test fixture
		svc, err := NewService(dir, noopServiceLogger(), command)
		require.NoError(t, err)

		planFile := filepath.Join(dir, "docs", "plans", "prediction-error.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))
		require.NoError(t, svc.CreateBranch("prediction-error"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "branch.txt"), []byte("branch\n"), 0o600))
		require.NoError(t, svc.repo.add("branch.txt"))
		require.NoError(t, svc.repo.commit("advance branch"))
		branchHead := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
		require.NoError(t, svc.repo.checkoutBranch("master"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "source.txt"), []byte("source\n"), 0o600))
		require.NoError(t, svc.repo.add("source.txt"))
		require.NoError(t, svc.repo.commit("advance source"))
		sourceHead := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
		statusBefore := runGit(t, dir, "status", "--porcelain")

		err = svc.PreflightWorktreeForPlan(planFile, "")
		require.ErrorContains(t, err, "predict source merge into existing plan branch")
		require.ErrorContains(t, err, "fatal: bad revision")
		_, _, err = svc.CreateWorktreeForPlan(planFile, "")
		require.ErrorContains(t, err, "predict source merge into existing plan branch")
		assert.Equal(t, sourceHead, strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD")))
		assert.Equal(t, branchHead, strings.TrimSpace(runGit(t, dir, "rev-parse", "prediction-error")))
		assert.Equal(t, statusBefore, runGit(t, dir, "status", "--porcelain"))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "prediction-error"))
	})

	t.Run("creates worktree from non-default branch HEAD", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "from-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))

		require.NoError(t, svc.CreateBranch("ch_main"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "source.txt"), []byte("source branch\n"), 0o600))
		require.NoError(t, svc.repo.add("source.txt"))
		require.NoError(t, svc.repo.commit("advance source branch"))
		sourceHead, err := svc.repo.headHash()
		require.NoError(t, err)

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		defer svc.RemoveWorktree(wtPath) //nolint:errcheck // test cleanup
		assert.False(t, planNeedsCommit)
		assert.Equal(t, sourceHead, strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD")))
		require.NotEmpty(t, log.logs)
		assert.Contains(t, log.logs[len(log.logs)-1], "creating worktree with new branch: from-feature (from ch_main)")
	})

	t.Run("fails when plan branch is already checked out", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		require.NoError(t, svc.CreateBranch("feature"))

		planFile := filepath.Join(dir, "docs", "plans", "feature.md")
		_, _, err = svc.CreateWorktreeForPlan(planFile, "")
		require.Error(t, err)
		assert.Equal(t, "plan branch \"feature\" is already checked out here; switch to the source branch or run without --worktree", err.Error())
	})

	t.Run("fails when plan branch differs from current branch only by case", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		require.NoError(t, svc.CreateBranch("Feature"))

		planFile := filepath.Join(dir, "docs", "plans", "feature.md")
		_, _, err = svc.CreateWorktreeForPlan(planFile, "")
		require.ErrorContains(t, err, "plan branch \"feature\" is already checked out here")
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "feature"))
	})

	t.Run("rejects symlinked worktree parent", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		outside := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai"), 0o750))
		if err := os.Symlink(outside, filepath.Join(dir, ".loopai", "worktrees")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		planFile := filepath.Join(dir, "docs", "plans", "escaped.md")
		_, _, err = svc.CreateWorktreeForPlan(planFile, "")
		require.ErrorContains(t, err, "symbolic link")
		assert.NoDirExists(t, filepath.Join(outside, "escaped"))
	})

	t.Run("fails when branch override is already checked out", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		require.NoError(t, svc.CreateBranch("custom-feature"))

		planFile := filepath.Join(dir, "docs", "plans", "different-name.md")
		_, _, err = svc.CreateWorktreeForPlan(planFile, "custom-feature")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "plan branch \"custom-feature\" is already checked out here")
	})

	t.Run("creates worktree from detached HEAD", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "detached-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))

		detachedHead := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
		runGit(t, dir, "checkout", "--detach", detachedHead)

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		defer svc.RemoveWorktree(wtPath) //nolint:errcheck // test cleanup
		assert.False(t, planNeedsCommit)
		assert.Equal(t, detachedHead, strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD")))
		require.NotEmpty(t, log.logs)
		assert.Contains(t, log.logs[len(log.logs)-1], "creating worktree with new branch: detached-feature (from "+detachedHead[:7]+")")
	})

	t.Run("succeeds from develop when develop is default", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// switch to develop branch
		require.NoError(t, svc.CreateBranch("develop"))

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "develop-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.Contains(t, wtPath, "develop-feature")
		assert.True(t, planNeedsCommit, "untracked plan file should need commit")

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})

	t.Run("fails with other uncommitted changes", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		// create another uncommitted file
		require.NoError(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other"), 0o600))

		_, _, err = svc.CreateWorktreeForPlan(planFile, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot create worktree")
		assert.Contains(t, err.Error(), "uncommitted changes")
		assert.Contains(t, err.Error(), "other.txt")
	})

	t.Run("fails when worktree already exists", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "dup-worktree.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		// create first worktree
		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit, "untracked plan file should need commit")

		// switch back to master for second attempt
		require.NoError(t, svc.repo.checkoutBranch("master"))

		// second attempt should fail
		_, _, err = svc.CreateWorktreeForPlan(planFile, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "worktree target already exists")

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})

	t.Run("auto-commits plan file", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create untracked plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "new-feature.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# New Feature"), 0o600))

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit, "untracked plan file should need commit")

		// verify plan file was copied into worktree
		wtPlanFile := filepath.Join(wtPath, "docs", "plans", "new-feature.md")
		_, statErr := os.Stat(wtPlanFile)
		assert.NoError(t, statErr, "plan file should exist in worktree")

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})

	t.Run("does not commit plan on main", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// record HEAD before creating worktree
		headBefore, err := svc.repo.headHash()
		require.NoError(t, err)

		// create untracked plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "no-commit-on-main.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Regression Test"), 0o600))

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit)

		// main repo HEAD must not have advanced (plan is NOT committed on main)
		headAfter, err := svc.repo.headHash()
		require.NoError(t, err)
		assert.Equal(t, headBefore, headAfter, "HEAD on main must not change after CreateWorktreeForPlan")

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})

	t.Run("fails when branch is checked out in another worktree", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create plan file and first worktree
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "branch-conflict.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit, "untracked plan file should need commit")
		defer svc.RemoveWorktree(wtPath) //nolint:errcheck // cleanup

		// try to create second worktree at different path but same branch.
		// use AddWorktree directly to bypass dir-exists check.
		secondPath := filepath.Join(dir, ".loopai", "worktrees", "branch-conflict-2")
		err = svc.repo.addWorktree(t.Context(), secondPath, "branch-conflict", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already used by worktree")
	})

	t.Run("strips date prefix from branch name", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "2024-01-15-add-auth.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit, "untracked plan file should need commit")
		assert.Contains(t, wtPath, "add-auth")

		// verify branch name
		wtSvc, err := NewService(wtPath, noopServiceLogger())
		require.NoError(t, err)
		branch, err := wtSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "add-auth", branch)

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})
}

func TestService_CommitPlanFile(t *testing.T) {
	t.Run("commits plan file in worktree", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create plan file
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "commit-test.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Commit Test Plan"), 0o600))

		// create worktree (plan is copied in)
		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit)

		// open worktree git service and commit plan (pass main repo root for path resolution)
		wtSvc, err := NewService(wtPath, log)
		require.NoError(t, err)
		err = wtSvc.CommitPlanFile(planFile, svc.Root())
		require.NoError(t, err)

		// verify plan was committed on the feature branch, not on main
		wtBranch, err := wtSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "commit-test", wtBranch)

		// main repo should still be clean (plan not committed there)
		mainHasChanges, err := svc.repo.fileHasChanges(planFile)
		require.NoError(t, err)
		assert.True(t, mainHasChanges, "plan file should still be uncommitted in main repo")

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})

	t.Run("existing branch with identical plan is a no-op", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "resume-test.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Resume Test Plan"), 0o600))

		// first setup attempt creates the feature branch and commits the plan there.
		// the plan deliberately remains untracked in the main worktree.
		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		require.True(t, planNeedsCommit)

		wtSvc, err := NewService(wtPath, log)
		require.NoError(t, err)
		require.NoError(t, wtSvc.CommitPlanFile(planFile, svc.Root()))
		require.NoError(t, svc.RemoveWorktree(wtPath))

		// a retry reuses the feature branch. CreateWorktreeForPlan still copies the
		// untracked main-worktree plan, but its content already matches the branch.
		retryPath, retryNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		require.True(t, retryNeedsCommit)
		defer svc.RemoveWorktree(retryPath) //nolint:errcheck // test cleanup

		retrySvc, err := NewService(retryPath, log)
		require.NoError(t, err)
		headBefore, err := retrySvc.repo.headHash()
		require.NoError(t, err)

		log.logs = nil
		require.NoError(t, retrySvc.CommitPlanFile(planFile, svc.Root()))

		headAfter, err := retrySvc.repo.headHash()
		require.NoError(t, err)
		assert.Equal(t, headBefore, headAfter, "identical plan must not create an empty commit")
		require.NotEmpty(t, log.logs)
		assert.Contains(t, log.logs[len(log.logs)-1], "plan file already committed")
	})

	t.Run("commits plan file with case-mismatched path", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create plan file with specific case on master
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "Case-Test.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Case Test Plan"), 0o600))

		// create worktree from master (plan is copied in with original case)
		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit)

		// open worktree git service and commit plan with lowercase path (different case)
		wtSvc, err := NewService(wtPath, log)
		require.NoError(t, err)
		lowercasePlan := filepath.Join(plansDir, "case-test.md")
		err = wtSvc.CommitPlanFile(lowercasePlan, svc.Root())
		require.NoError(t, err, "commit should succeed despite case mismatch")

		// verify commit succeeded on the feature branch (branch name derived from original-case plan file)
		wtBranch, err := wtSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "Case-Test", wtBranch)

		// cleanup
		require.NoError(t, svc.RemoveWorktree(wtPath))
	})

	t.Run("branch override used instead of plan filename", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, &mockLogger{})
		require.NoError(t, err)

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "2026-04-30-some-long-generated-name.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		wtPath, _, err := svc.CreateWorktreeForPlan(planFile, "my-custom-branch")
		require.NoError(t, err)
		defer svc.RemoveWorktree(wtPath) //nolint:errcheck // test cleanup, error irrelevant

		// worktree path should use the override, not the plan filename
		assert.Contains(t, wtPath, "my-custom-branch")
		assert.True(t, svc.repo.branchExists("my-custom-branch"))
		assert.False(t, svc.repo.branchExists("some-long-generated-name"))
	})
}

func TestService_RemoveWorktree(t *testing.T) {
	t.Run("removes existing worktree", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		// create plan and worktree
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "rm-test.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit)

		log.logs = nil // reset logs
		err = svc.RemoveWorktree(wtPath)
		require.NoError(t, err)

		// verify worktree removed
		_, err = os.Stat(wtPath)
		assert.True(t, os.IsNotExist(err))
		assert.Contains(t, log.logs[0], "removed worktree")
	})

	t.Run("no-op when path does not exist", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		log := &mockLogger{}
		svc, err := NewService(dir, log)
		require.NoError(t, err)

		err = svc.RemoveWorktree("/nonexistent/path")
		require.NoError(t, err)
		assert.Empty(t, log.logs) // nothing should be logged
	})

	t.Run("preserves branch after removal", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		// create worktree
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "preserve-branch.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		wtPath, planNeedsCommit, err := svc.CreateWorktreeForPlan(planFile, "")
		require.NoError(t, err)
		assert.True(t, planNeedsCommit)

		// remove worktree
		err = svc.RemoveWorktree(wtPath)
		require.NoError(t, err)

		// branch should still exist
		assert.True(t, svc.repo.branchExists("preserve-branch"))
	})
}

func TestService_FileHasChanges(t *testing.T) {
	t.Run("returns true for dirty file", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, &mockLogger{})
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("data"), 0o600))
		changed, err := svc.FileHasChanges("dirty.txt")
		require.NoError(t, err)
		assert.True(t, changed)
	})

	t.Run("returns false for clean file", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, &mockLogger{})
		require.NoError(t, err)

		// README.md was committed in setupExternalTestRepo
		changed, err := svc.FileHasChanges("README.md")
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("returns false for nonexistent file", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, &mockLogger{})
		require.NoError(t, err)

		changed, err := svc.FileHasChanges("nonexistent.txt")
		require.NoError(t, err)
		assert.False(t, changed)
	})
}

func TestService_BranchExists(t *testing.T) {
	t.Run("returns true for existing branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		require.NoError(t, svc.CreateBranch("release/13.0.0"))
		runGit(t, dir, "checkout", "master")

		assert.True(t, svc.BranchExists("release/13.0.0"))
	})

	t.Run("returns true for current branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		assert.True(t, svc.BranchExists("master"))
	})

	t.Run("returns false for unknown branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		assert.False(t, svc.BranchExists("no-such-branch"))
	})

	t.Run("returns false for commit hash", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		hash, err := svc.HeadHash()
		require.NoError(t, err)

		// a hash is a valid revision but not a branch, so it cannot serve as a branch base
		assert.False(t, svc.BranchExists(hash))
	})

	t.Run("returns false for empty name", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		assert.False(t, svc.BranchExists(""))
	})
}

func TestService_BranchHash(t *testing.T) {
	t.Run("returns head of a branch that is not checked out", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		require.NoError(t, svc.CreateBranch("feature"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		featureHead, err := svc.HeadHash()
		require.NoError(t, err)
		runGit(t, dir, "checkout", "master")

		masterHead, err := svc.HeadHash()
		require.NoError(t, err)
		got, err := svc.BranchHash("feature")
		require.NoError(t, err)
		assert.Equal(t, featureHead, got)
		assert.NotEqual(t, masterHead, got)
	})

	t.Run("returns head of the current branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		head, err := svc.HeadHash()
		require.NoError(t, err)
		got, err := svc.BranchHash("master")
		require.NoError(t, err)
		assert.Equal(t, head, got)
	})

	t.Run("fails for unknown branch", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		_, err = svc.BranchHash("no-such-branch")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no-such-branch")
	})

	t.Run("fails for empty name", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		_, err = svc.BranchHash("")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty branch name")
	})
}

func TestService_BranchDiffStats(t *testing.T) {
	setupFeature := func(t *testing.T) *Service {
		t.Helper()
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		require.NoError(t, svc.CreateBranch("feature"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("one\ntwo\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		runGit(t, dir, "checkout", "master")
		return svc
	}

	t.Run("reports stats for a branch that is not checked out", func(t *testing.T) {
		svc := setupFeature(t)

		stats, err := svc.BranchDiffStats("master", "feature")
		require.NoError(t, err)
		assert.Equal(t, DiffStats{Files: 1, Additions: 2}, stats)
	})

	t.Run("HEAD-based stats stay empty on the base branch", func(t *testing.T) {
		svc := setupFeature(t)

		stats, err := svc.DiffStats("master")
		require.NoError(t, err)
		assert.Equal(t, DiffStats{}, stats)
	})

	t.Run("returns zero stats when branch equals base", func(t *testing.T) {
		svc := setupFeature(t)

		stats, err := svc.BranchDiffStats("master", "master")
		require.NoError(t, err)
		assert.Equal(t, DiffStats{}, stats)
	})

	t.Run("fails for empty branch name", func(t *testing.T) {
		svc := setupFeature(t)

		_, err := svc.BranchDiffStats("master", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty branch name")
	})

	t.Run("returns zero stats for unknown branch", func(t *testing.T) {
		svc := setupFeature(t)

		stats, err := svc.BranchDiffStats("master", "no-such-branch")
		require.NoError(t, err)
		assert.Equal(t, DiffStats{}, stats)
	})

	t.Run("a same-named tag does not shadow the branch", func(t *testing.T) {
		svc := setupFeature(t)
		// git resolves a bare "feature" to refs/tags/feature before refs/heads/feature
		runGit(t, svc.repo.root(), "tag", "feature", "master")

		stats, err := svc.BranchDiffStats("master", "feature")
		require.NoError(t, err)
		assert.Equal(t, DiffStats{Files: 1, Additions: 2}, stats)
	})
}

func TestService_formatDirtyFiles(t *testing.T) {
	svc := &Service{}

	t.Run("single file", func(t *testing.T) {
		result := svc.formatDirtyFiles([]string{"file.txt"})
		assert.Equal(t, "  file.txt", result)
	})

	t.Run("few files", func(t *testing.T) {
		result := svc.formatDirtyFiles([]string{"a.txt", "b.txt", "c.txt"})
		assert.Equal(t, "  a.txt\n  b.txt\n  c.txt", result)
	})

	t.Run("exactly 10 files", func(t *testing.T) {
		files := make([]string, 10)
		for i := range files {
			files[i] = fmt.Sprintf("file%d.txt", i)
		}
		result := svc.formatDirtyFiles(files)
		assert.NotContains(t, result, "more")
		assert.Contains(t, result, "file9.txt")
	})

	t.Run("11 files truncates with and-more suffix", func(t *testing.T) {
		files := make([]string, 11)
		for i := range files {
			files[i] = fmt.Sprintf("file%d.txt", i)
		}
		result := svc.formatDirtyFiles(files)
		assert.Contains(t, result, "file9.txt")
		assert.NotContains(t, result, "file10.txt")
		assert.Contains(t, result, "... and 1 more")
	})

	t.Run("15 files shows 10 plus count", func(t *testing.T) {
		files := make([]string, 15)
		for i := range files {
			files[i] = fmt.Sprintf("file%d.txt", i)
		}
		result := svc.formatDirtyFiles(files)
		assert.Contains(t, result, "... and 5 more")
	})

	t.Run("empty input", func(t *testing.T) {
		assert.Empty(t, svc.formatDirtyFiles(nil))
		assert.Empty(t, svc.formatDirtyFiles([]string{}))
	})
}

func TestService_SetCommitTrailer(t *testing.T) {
	t.Run("stores trailer value", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		svc.SetCommitTrailer("Co-authored-by: test <test@example.com>")
		assert.Equal(t, "Co-authored-by: test <test@example.com>", svc.trailer)
	})

	t.Run("clears trailer with empty string", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		svc.SetCommitTrailer("something")
		svc.SetCommitTrailer("")
		assert.Empty(t, svc.trailer)
	})
}

func TestService_appendTrailer(t *testing.T) {
	svc := &Service{}

	t.Run("returns message unchanged when trailer is empty", func(t *testing.T) {
		assert.Equal(t, "some commit msg", svc.appendTrailer("some commit msg"))
	})

	t.Run("appends trailer with blank line", func(t *testing.T) {
		svc.trailer = "Co-authored-by: bot <bot@example.com>"
		result := svc.appendTrailer("feat: add feature")
		assert.Equal(t, "feat: add feature\n\nCo-authored-by: bot <bot@example.com>", result)
	})

	t.Run("appends trailer to multi-line message", func(t *testing.T) {
		svc.trailer = "Signed-off-by: user"
		result := svc.appendTrailer("fix: bug\n\ndetailed description")
		assert.Equal(t, "fix: bug\n\ndetailed description\n\nSigned-off-by: user", result)
	})
}

func TestService_AutoCommitAll(t *testing.T) {
	t.Run("commits modified and untracked files and respects gitignore", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		svc.SetCommitTrailer("Loopai-Test: auto-commit")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "remove.txt"), []byte("remove me\n"), 0o600))
		require.NoError(t, svc.repo.add("remove.txt"))
		require.NoError(t, svc.repo.commit("add removable fixture"))

		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Modified\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.log"), []byte("ignored\n"), 0o600))
		require.NoError(t, os.Remove(filepath.Join(dir, "remove.txt")))

		committed, err := svc.AutoCommitAll("save working tree")
		require.NoError(t, err)
		assert.True(t, committed)
		assert.Empty(t, runGit(t, dir, "status", "--porcelain"))

		files := runGit(t, dir, "show", "--pretty=format:", "--name-only", "HEAD")
		assert.Contains(t, files, "README.md")
		assert.Contains(t, files, "untracked.txt")
		assert.Contains(t, files, ".gitignore")
		assert.NotContains(t, files, "ignored.log")
		assert.NotContains(t, runGit(t, dir, "ls-tree", "--name-only", "HEAD", "remove.txt"), "remove.txt")
		assert.Contains(t, runGit(t, dir, "show", "--pretty=format:", "--name-status", "HEAD"), "D\tremove.txt")
		message := runGit(t, dir, "log", "-1", "--pretty=%B")
		assert.Equal(t, "save working tree\n\nLoopai-Test: auto-commit", strings.TrimSpace(message))
	})

	t.Run("never commits runtime artifacts that were already staged", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "progress"), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "worktrees", "feature"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "progress", "run.log"), []byte("secret\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "worktrees", "feature", "file"), []byte("artifact\n"), 0o600))
		runGit(t, dir, "add", "-f", ".loopai/progress/run.log", ".loopai/worktrees/feature/file")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("commit me\n"), 0o600))

		committed, err := svc.AutoCommitAll("save without runtime artifacts")
		require.NoError(t, err)
		assert.True(t, committed)
		files := runGit(t, dir, "show", "--pretty=format:", "--name-only", "HEAD")
		assert.Contains(t, files, "dirty.txt")
		assert.NotContains(t, files, ".loopai/progress")
		assert.NotContains(t, files, ".loopai/worktrees")
		assert.NotContains(t, runGit(t, dir, "diff", "--cached", "--name-only"), ".loopai/")
	})

	t.Run("succeeds when gitignored runtime artifacts exist on disk", func(t *testing.T) {
		// regression: git add dies with "paths are ignored by one of your .gitignore files"
		// when a command-line pathspec (even an :(exclude) one) names an existing ignored path
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		require.NoError(t, svc.EnsureLocalGitignore())
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "progress"), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "worktrees", "feature"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "progress", "run.log"), []byte("log\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "worktrees", "feature", "file"), []byte("artifact\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("commit me\n"), 0o600))

		committed, err := svc.AutoCommitAll("save with runtime artifacts present")
		require.NoError(t, err)
		assert.True(t, committed)
		files := runGit(t, dir, "show", "--pretty=format:", "--name-only", "HEAD")
		assert.Contains(t, files, "dirty.txt")
		assert.NotContains(t, files, ".loopai/progress")
		assert.NotContains(t, files, ".loopai/worktrees")
	})

	t.Run("clean tree is a no-op", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)

		before, err := svc.HeadHash()
		require.NoError(t, err)
		committed, err := svc.AutoCommitAll("unused")
		require.NoError(t, err)
		assert.False(t, committed)
		after, err := svc.HeadHash()
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	t.Run("returns commit errors", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		hooksDir := filepath.Join(dir, ".git", "hooks")
		hook := filepath.Join(hooksDir, "pre-commit")
		require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755)) //nolint:gosec // executable test fixture
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blocked.txt"), []byte("change\n"), 0o600))

		committed, err := svc.AutoCommitAll("blocked")
		require.Error(t, err)
		assert.False(t, committed)
		assert.Contains(t, err.Error(), "auto-commit all: commit")
	})
}

func TestService_AcquireWorktreeCreationLock(t *testing.T) {
	dir := setupExternalTestRepo(t)
	first, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)
	second, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)

	releaseFirst, err := first.AcquireWorktreeCreationLock()
	require.NoError(t, err)
	type lockResult struct {
		release func() error
		err     error
	}
	acquired := make(chan lockResult, 1)
	go func() {
		release, lockErr := second.AcquireWorktreeCreationLock()
		acquired <- lockResult{release: release, err: lockErr}
	}()

	select {
	case <-acquired:
		t.Fatal("second repository lock acquired before the first was released")
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, releaseFirst())
	select {
	case result := <-acquired:
		require.NoError(t, result.err)
		require.NotNil(t, result.release)
		require.NoError(t, result.release())
	case <-time.After(5 * time.Second):
		t.Fatal("second repository lock did not acquire after release")
	}
}

func TestService_AcquireWorktreeCreationLockContext_Canceled(t *testing.T) {
	dir := setupExternalTestRepo(t)
	first, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)
	second, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)

	releaseFirst, err := first.AcquireWorktreeCreationLock()
	require.NoError(t, err)
	defer func() { require.NoError(t, releaseFirst()) }()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	release, err := second.AcquireWorktreeCreationLockContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, release)
}

func TestService_CommitWithTrailer(t *testing.T) {
	t.Run("trailer appears in commit log", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		svc.SetCommitTrailer("Co-authored-by: ralphex <noreply@ralphex.com>")

		// create plan file and switch to feature branch (mirrors real worktree flow)
		plansDir := filepath.Join(svc.Root(), "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "trailer-test.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.CreateBranch("trailer-test"))

		err = svc.CommitPlanFile(planFile, svc.Root())
		require.NoError(t, err)

		// verify trailer in commit message; branch name comes from current branch
		out := runGit(t, svc.Root(), "log", "-1", "--format=%B")
		assert.Contains(t, out, "add plan: trailer-test")
		assert.Contains(t, out, "Co-authored-by: ralphex <noreply@ralphex.com>")
	})

	t.Run("no trailer when not configured", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		// no SetCommitTrailer call

		plansDir := filepath.Join(svc.Root(), "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "no-trailer.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.CreateBranch("no-trailer"))

		err = svc.CommitPlanFile(planFile, svc.Root())
		require.NoError(t, err)

		out := runGit(t, svc.Root(), "log", "-1", "--format=%B")
		assert.Contains(t, out, "add plan: no-trailer")
		assert.NotContains(t, out, "Co-authored-by")
	})

	t.Run("trailer in CreateBranchForPlan", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		svc.SetCommitTrailer("Co-authored-by: ralphex <noreply@ralphex.com>")

		// create an untracked plan file so CreateBranchForPlan auto-commits it
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "branch-trailer.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))

		err = svc.CreateBranchForPlan(planFile, "master", "")
		require.NoError(t, err)

		out := runGit(t, dir, "log", "-1", "--format=%B")
		assert.Contains(t, out, "add plan: branch-trailer")
		assert.Contains(t, out, "Co-authored-by: ralphex <noreply@ralphex.com>")
	})

	t.Run("trailer in MovePlanToCompleted", func(t *testing.T) {
		dir := setupExternalTestRepo(t)
		svc, err := NewService(dir, noopServiceLogger())
		require.NoError(t, err)
		svc.SetCommitTrailer("Signed-off-by: test")

		// create and commit a plan file first
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "move-trailer.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# Plan"), 0o600))
		require.NoError(t, svc.repo.add(planFile))
		require.NoError(t, svc.repo.commit("add plan"))

		err = svc.MovePlanToCompleted(planFile)
		require.NoError(t, err)

		out := runGit(t, dir, "log", "-1", "--format=%B")
		assert.Contains(t, out, "move completed plan: move-trailer.md")
		assert.Contains(t, out, "Signed-off-by: test")
	})
}

func TestService_resolveFilesystemCase(t *testing.T) {
	dir := setupExternalTestRepo(t)
	svc, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)

	tests := []struct {
		name     string
		setup    func(t *testing.T, dir string) string // returns input path
		wantBase string                                // expected basename in result
	}{
		{name: "returns actual case when basename differs", setup: func(t *testing.T, dir string) string {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "Foo-Bar.md"), []byte("x"), 0o600))
			return filepath.Join(dir, "foo-bar.md") // different case
		}, wantBase: "Foo-Bar.md"},
		{name: "returns original path when no match", setup: func(_ *testing.T, dir string) string {
			return filepath.Join(dir, "nonexistent.md")
		}, wantBase: "nonexistent.md"},
		{name: "returns original path when file matches exactly", setup: func(t *testing.T, dir string) string {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "exact.md"), []byte("x"), 0o600))
			return filepath.Join(dir, "exact.md")
		}, wantBase: "exact.md"},
		{name: "returns original path when directory unreadable", setup: func(_ *testing.T, _ string) string {
			return "/nonexistent-dir-abc123/file.md"
		}, wantBase: "file.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			input := tt.setup(t, tmpDir)
			got := svc.resolveFilesystemCase(input)
			assert.Equal(t, tt.wantBase, filepath.Base(got))
		})
	}
}
