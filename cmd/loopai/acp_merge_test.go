package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
)

// acpMergeRepo is a primary checkout on master plus a linked worktree on the feature branch, the
// layout T3 Code produces for an agent-mode thread.
type acpMergeRepo struct {
	primary  string
	worktree string
	svc      *git.Service // opened at the feature worktree, as the ACP session cwd is
}

func setupACPMergeRepo(t *testing.T) acpMergeRepo {
	t.Helper()
	primary := setupTestRepo(t)
	worktree := filepath.Join(t.TempDir(), "feature-wt")
	runGit(t, primary, "worktree", "add", "-b", "feature", worktree)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("feature\n"), 0o600))
	runGit(t, worktree, "add", "feature.txt")
	runGit(t, worktree, "commit", "-m", "feature work")
	svc, err := git.NewService(worktree, noopLogger())
	require.NoError(t, err)
	return acpMergeRepo{primary: primary, worktree: worktree, svc: svc}
}

func acpRiskReport(risk string) string {
	return "# Report: feature\n\n## Summary\n\nDone.\n\n## Risk\n\n" + risk + "\n\n- details\n"
}

func acpMergeConfig() *config.Config {
	return &config.Config{ACPAutoMerge: true}
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(gitOutput(t, dir, "rev-parse", rev))
}

func TestACPAutoMergeMerges(t *testing.T) {
	for _, risk := range []string{"**low**", "Medium."} {
		t.Run(risk, func(t *testing.T) {
			repo := setupACPMergeRepo(t)
			featureHead := revParse(t, repo.worktree, "HEAD")

			res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskReport(risk))

			require.True(t, res.merged, "skipped: %s", res.skipped)
			assert.Empty(t, res.skipped)
			assert.Equal(t, "fast-forward", res.kind)
			assert.Equal(t, "feature", res.feature)
			assert.Equal(t, "master", res.base)
			assert.Equal(t, featureHead[:7], res.head)
			assert.Equal(t, featureHead, revParse(t, repo.primary, "refs/heads/master"))
			assert.Equal(t, "master", currentGitBranch(t, repo.primary))
			assert.FileExists(t, filepath.Join(repo.primary, "feature.txt"))
			// nothing is deleted: the branch and T3 Code's worktree survive the merge
			assert.True(t, branchExists(t, repo.primary, "feature"))
			assert.DirExists(t, repo.worktree)
			assert.Contains(t, gitOutput(t, repo.primary, "worktree", "list"), filepath.Base(repo.worktree))
		})
	}
}

func TestACPAutoMergeMergeCommitAndOriginPrefix(t *testing.T) {
	repo := setupACPMergeRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "base.txt"), []byte("base\n"), 0o600))
	runGit(t, repo.primary, "add", "base.txt")
	runGit(t, repo.primary, "commit", "-m", "base work")

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "origin/master", acpRiskReport("low"))

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, "merge commit", res.kind)
	assert.Equal(t, "master", res.base)
	assert.Equal(t, revParse(t, repo.primary, "HEAD")[:7], res.head)
	assert.FileExists(t, filepath.Join(repo.primary, "feature.txt"))
}

func TestACPAutoMergeAlreadyUpToDate(t *testing.T) {
	repo := setupACPMergeRepo(t)
	runGit(t, repo.primary, "merge", "--ff-only", "feature")

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskReport("low"))

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, "already up to date", res.kind)
}

func TestACPAutoMergeSkips(t *testing.T) {
	tests := []struct {
		name    string
		cfg     func() *config.Config
		base    string
		report  string
		prepare func(t *testing.T, repo acpMergeRepo)
		want    string
	}{
		{name: "high risk", report: acpRiskReport("**high**"), want: "Risk is high"},
		{name: "unknown risk", report: acpRiskReport("Moderate, see below"), want: "Risk level not stated"},
		{name: "fallback risk", report: acpRiskReport("_assessment unavailable_"), want: "Risk level not stated"},
		{name: "no risk section", report: "# Report: feature\n\n## Summary\n\nDone.\n", want: "Risk level not stated"},
		{name: "disabled", cfg: func() *config.Config { return &config.Config{} }, want: "acp_auto_merge is disabled"},
		{name: "nil config", cfg: func() *config.Config { return nil }, want: "acp_auto_merge is disabled"},
		{name: "finalize pr", cfg: func() *config.Config { return &config.Config{ACPAutoMerge: true, Finalize: config.FinalizePR} },
			want: "finalize = pr owns the close-out"},
		{name: "finalize merge",
			cfg:  func() *config.Config { return &config.Config{ACPAutoMerge: true, Finalize: config.FinalizeMerge} },
			want: "finalize = merge owns the close-out"},
		{name: "missing base", base: "trunk", want: `base branch "trunk" does not exist locally`},
		{name: "commit hash base", base: "0123456789abcdef", want: "does not exist locally"},
		{name: "detached head", prepare: func(t *testing.T, repo acpMergeRepo) {
			runGit(t, repo.worktree, "checkout", "--detach")
		}, want: "HEAD is detached"},
		{name: "feature equals base", base: "feature", want: "the run is on the base branch itself"},
		{name: "dirty feature", prepare: func(t *testing.T, repo acpMergeRepo) {
			require.NoError(t, os.WriteFile(filepath.Join(repo.worktree, "wip.txt"), []byte("wip\n"), 0o600))
		}, want: "the feature worktree at"},
		{name: "dirty base", prepare: func(t *testing.T, repo acpMergeRepo) {
			require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "README.md"), []byte("edited\n"), 0o600))
		}, want: "the base worktree at"},
		{name: "base checked out nowhere", prepare: func(t *testing.T, repo acpMergeRepo) {
			runGit(t, repo.primary, "checkout", "-b", "other")
		}, want: `base branch "master" is not checked out in any worktree`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupACPMergeRepo(t)
			if tc.prepare != nil {
				tc.prepare(t, repo)
			}
			cfg := acpMergeConfig()
			if tc.cfg != nil {
				cfg = tc.cfg()
			}
			base := tc.base
			if base == "" {
				base = "master"
			}
			report := tc.report
			if report == "" {
				report = acpRiskReport("low")
			}
			masterBefore := revParse(t, repo.primary, "refs/heads/master")
			primaryBranch := currentGitBranch(t, repo.primary)

			res := acpAutoMerge(t.Context(), repo.svc, cfg, base, report)

			assert.False(t, res.merged)
			assert.Contains(t, res.skipped, tc.want)
			assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
			assert.Equal(t, primaryBranch, currentGitBranch(t, repo.primary))
			assert.True(t, branchExists(t, repo.primary, "feature"))
		})
	}
}

func TestACPAutoMergeConflictAborts(t *testing.T) {
	repo := setupACPMergeRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "feature.txt"), []byte("base side\n"), 0o600))
	runGit(t, repo.primary, "add", "feature.txt")
	runGit(t, repo.primary, "commit", "-m", "conflicting base work")
	masterBefore := revParse(t, repo.primary, "refs/heads/master")

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskReport("low"))

	assert.False(t, res.merged)
	assert.Contains(t, res.skipped, "conflicted and was aborted")
	assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
	gitDir := strings.TrimSpace(gitOutput(t, repo.primary, "rev-parse", "--absolute-git-dir"))
	assert.NoFileExists(t, filepath.Join(gitDir, "MERGE_HEAD"))
	assert.Empty(t, strings.TrimSpace(gitOutput(t, repo.primary, "status", "--porcelain")))
	assert.True(t, branchExists(t, repo.primary, "feature"))
}

func TestACPMergeResultMessage(t *testing.T) {
	tests := []struct {
		name string
		res  acpMergeResult
		want string
	}{
		{name: "fast-forward",
			res:  acpMergeResult{merged: true, kind: "fast-forward", base: "master", feature: "feat", head: "abc1234"},
			want: "## Merge\n\nMerged `feat` into `master` (fast-forward, `abc1234`). Not pushed."},
		{name: "merge commit",
			res:  acpMergeResult{merged: true, kind: "merge commit", base: "main", feature: "feat", head: "def5678"},
			want: "## Merge\n\nMerged `feat` into `main` (merge commit, `def5678`). Not pushed."},
		{name: "already up to date",
			res:  acpMergeResult{merged: true, kind: "already up to date", base: "main", feature: "feat", head: "def5678"},
			want: "## Merge\n\nMerged `feat` into `main` (already up to date, `def5678`). Not pushed."},
		{name: "merged without head",
			res:  acpMergeResult{merged: true, kind: "fast-forward", base: "main", feature: "feat"},
			want: "## Merge\n\nMerged `feat` into `main` (fast-forward). Not pushed."},
		{name: "skipped",
			res:  acpMergeResult{base: "master", skipped: "Risk is high"},
			want: "## Merge\n\nNot merged into `master`: Risk is high."},
		{name: "skipped reason with trailing period",
			res:  acpMergeResult{base: "master", skipped: "merge failed."},
			want: "## Merge\n\nNot merged into `master`: merge failed."},
		{name: "skipped without base",
			res:  acpMergeResult{skipped: "acp_auto_merge is disabled"},
			want: "## Merge\n\nNot merged: acp_auto_merge is disabled."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.res.message())
		})
	}
}

func TestACPMergeResultSummary(t *testing.T) {
	tests := []struct {
		name string
		res  acpMergeResult
		want string
	}{
		{name: "merged",
			res:  acpMergeResult{merged: true, kind: "fast-forward", base: "master", feature: "feat", head: "abc1234"},
			want: "merged feat into master (fast-forward, abc1234), not pushed"},
		{name: "merged without head",
			res:  acpMergeResult{merged: true, kind: "merge commit", base: "main", feature: "feat"},
			want: "merged feat into main (merge commit), not pushed"},
		{name: "skipped",
			res:  acpMergeResult{base: "master", skipped: "merge failed."},
			want: "not merged into master: merge failed"},
		{name: "skipped without base",
			res:  acpMergeResult{skipped: "acp_auto_merge is disabled"},
			want: "not merged: acp_auto_merge is disabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.res.summary())
		})
	}
}
