package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/git"
)

// detachSourceRepo is a repository with core.autocrlf off, its Git service, the plan path under
// docs/plans, and a state path under .loopai/progress/runs as a detached run uses it.
func detachSourceRepo(t *testing.T) (dir string, svc *git.Service, planPath, statePath string) {
	t.Helper()
	dir = setupTestRepo(t)
	runGit(t, dir, "config", "core.autocrlf", "false")
	planPath = filepath.Join(dir, "docs", "plans", "detached.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o750))
	svc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)
	statePath = filepath.Join(dir, ".loopai", "progress", "runs", "detached-1", "source-plans.json")
	return dir, svc, planPath, statePath
}

func TestDetachedSourcePlanReconcile(t *testing.T) {
	t.Run("removes an unchanged untracked plan", func(t *testing.T) {
		_, svc, planPath, statePath := detachSourceRepo(t)
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n"), 0o600))

		require.NoError(t, captureDetachedSourcePlan(svc, planPath, statePath))
		require.FileExists(t, statePath)
		require.NoError(t, reconcileDetachedSourcePlan(svc, statePath))

		assert.NoFileExists(t, planPath, "an unchanged untracked source plan is consumed by the run")
	})

	t.Run("restores an unchanged tracked plan to HEAD", func(t *testing.T) {
		dir, svc, planPath, statePath := detachSourceRepo(t)
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/detached.md")
		runGit(t, dir, "commit", "-m", "add plan")
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n\n- [ ] edited before launch\n"), 0o600))

		require.NoError(t, captureDetachedSourcePlan(svc, planPath, statePath))
		require.NoError(t, reconcileDetachedSourcePlan(svc, statePath))

		got, err := os.ReadFile(planPath) //nolint:gosec // test temp path
		require.NoError(t, err)
		assert.Equal(t, "# Detached\n", string(got))
		assert.Empty(t, gitOutput(t, dir, "status", "--porcelain", "--", "docs"), "the plan is merge-ready again")
	})

	t.Run("leaves a concurrently edited plan untouched", func(t *testing.T) {
		_, svc, planPath, statePath := detachSourceRepo(t)
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n"), 0o600))
		require.NoError(t, captureDetachedSourcePlan(svc, planPath, statePath))
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n\nedited during the run\n"), 0o600))

		err := reconcileDetachedSourcePlan(svc, statePath)

		require.EqualError(t, err, `reconcile the invoking checkout: source plan "docs/plans/detached.md" changed `+
			"during execution; left untouched", "a detached run's warning names no plan chain")
		got, readErr := os.ReadFile(planPath) //nolint:gosec // test temp path
		require.NoError(t, readErr)
		assert.Equal(t, "# Detached\n\nedited during the run\n", string(got))
	})

	t.Run("a second capture keeps the first", func(t *testing.T) {
		_, svc, planPath, statePath := detachSourceRepo(t)
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n"), 0o600))
		require.NoError(t, captureDetachedSourcePlan(svc, planPath, statePath))
		first, err := os.ReadFile(statePath) //nolint:gosec // test temp path
		require.NoError(t, err)

		// a resume sees the copy the user edited after the first attempt started
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n\nedited between attempts\n"), 0o600))
		require.NoError(t, captureDetachedSourcePlan(svc, planPath, statePath))

		second, err := os.ReadFile(statePath) //nolint:gosec // test temp path
		require.NoError(t, err)
		assert.Equal(t, string(first), string(second))
		require.ErrorContains(t, reconcileDetachedSourcePlan(svc, statePath), "left untouched",
			"the edit made after the first capture is never overwritten")
		assert.FileExists(t, planPath)
	})

	t.Run("a clean tracked plan records nothing to reconcile", func(t *testing.T) {
		dir, svc, planPath, statePath := detachSourceRepo(t)
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/detached.md")
		runGit(t, dir, "commit", "-m", "add plan")

		require.NoError(t, captureDetachedSourcePlan(svc, planPath, statePath))
		data, err := os.ReadFile(statePath) //nolint:gosec // test temp path
		require.NoError(t, err)
		assert.JSONEq(t, `{"source_plans":[]}`, string(data))
		require.NoError(t, reconcileDetachedSourcePlan(svc, statePath))
		assert.FileExists(t, planPath)
	})
}

func TestDetachedSourcePlanErrors(t *testing.T) {
	t.Run("missing state reconciles nothing", func(t *testing.T) {
		_, svc, planPath, statePath := detachSourceRepo(t)
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n"), 0o600))

		require.NoError(t, reconcileDetachedSourcePlan(svc, statePath))
		assert.FileExists(t, planPath)
	})

	t.Run("corrupt state is an error", func(t *testing.T) {
		_, svc, _, statePath := detachSourceRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o750))
		require.NoError(t, os.WriteFile(statePath, []byte("{not json"), 0o600))

		require.ErrorContains(t, reconcileDetachedSourcePlan(svc, statePath), "parse source plan state")
	})

	t.Run("unreadable state is an error", func(t *testing.T) {
		_, svc, _, statePath := detachSourceRepo(t)
		require.NoError(t, os.MkdirAll(statePath, 0o750))

		require.ErrorContains(t, reconcileDetachedSourcePlan(svc, statePath), "read source plan state")
	})

	t.Run("a plan outside the repository is not captured", func(t *testing.T) {
		_, svc, _, statePath := detachSourceRepo(t)
		outside := filepath.Join(t.TempDir(), "outside.md")
		require.NoError(t, os.WriteFile(outside, []byte("# Outside\n"), 0o600))

		err := captureDetachedSourcePlan(svc, outside, statePath)
		require.ErrorContains(t, err, "capture source plan: ")
		assert.NotContains(t, err.Error(), "chain", "a detached run's error names no plan chain")
		assert.NoFileExists(t, statePath)
	})

	t.Run("an uninspectable state path is not overwritten", func(t *testing.T) {
		dir, svc, planPath, _ := detachSourceRepo(t)
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached\n"), 0o600))
		blocker := filepath.Join(dir, ".loopai", "progress", "blocker")
		require.NoError(t, os.MkdirAll(filepath.Dir(blocker), 0o750))
		require.NoError(t, os.WriteFile(blocker, []byte("file"), 0o600))

		err := captureDetachedSourcePlan(svc, planPath, filepath.Join(blocker, "source-plans.json"))

		require.ErrorContains(t, err, "inspect source plan state")
	})
}
