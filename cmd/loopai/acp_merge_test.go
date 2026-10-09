package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// acpRiskOutcome is a successful run that completed on the feature worktree's current HEAD.
func acpRiskOutcome(t *testing.T, repo acpMergeRepo, risk string) planExecutionOutcome {
	t.Helper()
	return planExecutionOutcome{succeeded: true, report: acpRiskReport(risk), branchTip: revParse(t, repo.worktree, "HEAD")}
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

			res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskOutcome(t, repo, risk))

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

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "origin/master", acpRiskOutcome(t, repo, "low"))

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, "merge commit", res.kind)
	assert.Equal(t, "master", res.base)
	assert.Equal(t, revParse(t, repo.primary, "HEAD")[:7], res.head)
	assert.FileExists(t, filepath.Join(repo.primary, "feature.txt"))
	// the merge takes the completed commit by hash but still names the branch
	assert.Equal(t, "Merge branch 'feature'", strings.TrimSpace(gitOutput(t, repo.primary, "log", "-1", "--format=%s")))
	assert.Equal(t, revParse(t, repo.worktree, "HEAD"), revParse(t, repo.primary, "HEAD^2"))
}

// TestACPAutoMergeWaitsForRepositoryLock covers a second loopai process holding the shared lock,
// as a concurrent ACP auto-merge into the same base does: the merge is skipped once the wait
// expires and the base is unchanged.
func TestACPAutoMergeWaitsForRepositoryLock(t *testing.T) {
	repo := setupACPMergeRepo(t)
	masterBefore := revParse(t, repo.primary, "refs/heads/master")
	other, err := git.NewService(repo.primary, noopLogger())
	require.NoError(t, err)
	release, err := other.AcquireWorktreeCreationLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, release()) })
	prev := acpMergeLockTimeout
	acpMergeLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { acpMergeLockTimeout = prev })

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskOutcome(t, repo, "low"))

	assert.False(t, res.merged)
	assert.Contains(t, res.skipped, "another loopai process holds the repository lock")
	assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
}

// writeACPOverrides leaves untracked .loopai overrides in dir, as --t3-launch carries them into the
// thread's worktree and the source checkout keeps its own copies.
func writeACPOverrides(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "prompts"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "agents"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "config"), []byte("task_model = claude\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "prompts", "task.txt"), []byte("task\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai", "agents", "custom.txt"), []byte("agent\n"), 0o600))
}

func TestACPAutoMergeIgnoresUntrackedLocalOverrides(t *testing.T) {
	repo := setupACPMergeRepo(t)
	writeACPOverrides(t, repo.worktree)
	writeACPOverrides(t, repo.primary)

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskOutcome(t, repo, "low"))

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, revParse(t, repo.worktree, "HEAD"), revParse(t, repo.primary, "refs/heads/master"))
	assert.FileExists(t, filepath.Join(repo.primary, ".loopai", "config"))
}

// TestACPAutoMergeRefusesToOverwriteUntrackedOverride covers a plan branch that committed a file the
// base worktree holds as an untracked override: git refuses the merge and the override survives.
func TestACPAutoMergeRefusesToOverwriteUntrackedOverride(t *testing.T) {
	repo := setupACPMergeRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo.worktree, ".loopai"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo.worktree, ".loopai", "config"), []byte("committed\n"), 0o600))
	runGit(t, repo.worktree, "add", ".loopai/config")
	runGit(t, repo.worktree, "commit", "-m", "commit override")
	writeACPOverrides(t, repo.primary)
	masterBefore := revParse(t, repo.primary, "refs/heads/master")

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskOutcome(t, repo, "low"))

	assert.False(t, res.merged)
	assert.NotEmpty(t, res.skipped)
	assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
	data, err := os.ReadFile(filepath.Join(repo.primary, ".loopai", "config"))
	require.NoError(t, err)
	assert.Equal(t, "task_model = claude\n", string(data))
}

func TestACPAutoMergeAlreadyUpToDate(t *testing.T) {
	repo := setupACPMergeRepo(t)
	runGit(t, repo.primary, "merge", "--ff-only", "feature")

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskOutcome(t, repo, "low"))

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, "already up to date", res.kind)
}

// TestACPAutoMergeAfterFinalizeSync covers finalize = sync: the sync merged the base into the plan
// branch first, so the auto-merge that follows fast-forwards the base to that merge commit.
func TestACPAutoMergeAfterFinalizeSync(t *testing.T) {
	repo := setupACPMergeRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "base.txt"), []byte("base\n"), 0o600))
	runGit(t, repo.primary, "add", "base.txt")
	runGit(t, repo.primary, "commit", "-m", "base work")
	runGit(t, repo.worktree, "merge", "--no-ff", "-m", "sync master", "master")
	syncedHead := revParse(t, repo.worktree, "HEAD")

	res := acpAutoMerge(t.Context(), repo.svc, &config.Config{ACPAutoMerge: true, Finalize: config.FinalizeSync},
		"master", acpRiskOutcome(t, repo, "low"))

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, "fast-forward", res.kind)
	assert.Equal(t, syncedHead, revParse(t, repo.primary, "refs/heads/master"))
	assert.True(t, branchExists(t, repo.primary, "feature"))
}

// TestACPAutoMergeCanceledContext covers a run canceled as the merge starts: the merge fails and
// is reported as skipped with the base, its checkout, and the feature branch unchanged.
func TestACPAutoMergeCanceledContext(t *testing.T) {
	repo := setupACPMergeRepo(t)
	masterBefore := revParse(t, repo.primary, "refs/heads/master")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := acpAutoMerge(ctx, repo.svc, acpMergeConfig(), "master", acpRiskOutcome(t, repo, "low"))

	assert.False(t, res.merged)
	assert.Contains(t, res.skipped, context.Canceled.Error())
	assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
	assert.Equal(t, "master", currentGitBranch(t, repo.primary))
	assert.Empty(t, strings.TrimSpace(gitOutput(t, repo.primary, "status", "--porcelain")))
	assert.True(t, branchExists(t, repo.primary, "feature"))
}

func TestACPAutoMergeSkips(t *testing.T) {
	tests := []struct {
		name    string
		cfg     func() *config.Config
		base    string
		report  string
		prepare func(t *testing.T, repo acpMergeRepo)
		outcome func(t *testing.T, repo acpMergeRepo, outcome *planExecutionOutcome)
		want    string

		finalizeIncomplete error
	}{
		{name: "high risk", report: acpRiskReport("**high**"), want: "Risk is high"},
		{name: "unknown risk", report: acpRiskReport("Moderate, see below"), want: "Risk level not stated"},
		{name: "fallback risk", report: acpRiskReport("_assessment unavailable_"), want: "Risk level not stated"},
		{name: "no risk section", report: "# Report: feature\n\n## Summary\n\nDone.\n", want: "Risk level not stated"},
		{name: "disabled", cfg: func() *config.Config { return &config.Config{} }, want: "acp_auto_merge is disabled"},
		{name: "finalize pr", cfg: func() *config.Config { return &config.Config{ACPAutoMerge: true, Finalize: config.FinalizePR} },
			want: "finalize = pr owns the close-out"},
		{name: "finalize merge",
			cfg:  func() *config.Config { return &config.Config{ACPAutoMerge: true, Finalize: config.FinalizeMerge} },
			want: "finalize = merge owns the close-out"},
		{name: "finalize sync incomplete",
			cfg:                func() *config.Config { return &config.Config{ACPAutoMerge: true, Finalize: config.FinalizeSync} },
			finalizeIncomplete: errors.New("base sync blocked: validation failed"),
			want:               "finalize incomplete: base sync blocked: validation failed"},
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
		{name: "untracked base file", prepare: func(t *testing.T, repo acpMergeRepo) {
			require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "notes.txt"), []byte("notes\n"), 0o600))
		}, want: "has untracked files"},
		{name: "untracked file beside overrides", prepare: func(t *testing.T, repo acpMergeRepo) {
			writeACPOverrides(t, repo.worktree)
			require.NoError(t, os.WriteFile(filepath.Join(repo.worktree, ".loopai", "notes.txt"), []byte("x\n"), 0o600))
		}, want: "the feature worktree at"},
		{name: "clean pending merge in base", prepare: func(t *testing.T, repo acpMergeRepo) {
			acpPendingMerge(t, repo.primary, "master", "side")
		}, want: "has a merge in progress"},
		{name: "clean pending merge in feature", prepare: func(t *testing.T, repo acpMergeRepo) {
			acpPendingMerge(t, repo.worktree, "feature", "feature-side")
		}, want: "the feature worktree at"},
		{name: "base checked out nowhere", prepare: func(t *testing.T, repo acpMergeRepo) {
			runGit(t, repo.primary, "checkout", "-b", "other")
		}, want: `base branch "master" is not checked out in any worktree`},
		{name: "completed tip not recorded", outcome: func(_ *testing.T, _ acpMergeRepo, outcome *planExecutionOutcome) {
			outcome.branchTip = ""
		}, want: "the run did not record the commit it completed on"},
		{name: "branch moved after completion", outcome: func(t *testing.T, repo acpMergeRepo, _ *planExecutionOutcome) {
			runGit(t, repo.worktree, "commit", "--allow-empty", "-m", "unreviewed follow-up")
		}, want: `branch "feature" moved from`},
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

			outcome := planExecutionOutcome{succeeded: true, report: report, finalizeIncomplete: tc.finalizeIncomplete,
				branchTip: revParse(t, repo.worktree, "HEAD")}
			if tc.outcome != nil {
				tc.outcome(t, repo, &outcome)
			}

			res := acpAutoMerge(t.Context(), repo.svc, cfg, base, outcome)

			assert.False(t, res.merged)
			assert.Contains(t, res.skipped, tc.want)
			assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
			assert.Equal(t, primaryBranch, currentGitBranch(t, repo.primary))
			assert.True(t, branchExists(t, repo.primary, "feature"))
		})
	}
}

// acpPendingMerge leaves an uncommitted merge of a diverged side branch in dir whose index matches
// HEAD, so git status reports nothing while MERGE_HEAD exists.
func acpPendingMerge(t *testing.T, dir, branch, side string) {
	t.Helper()
	runGit(t, dir, "checkout", "-b", side)
	runGit(t, dir, "commit", "--allow-empty", "-m", "side work")
	runGit(t, dir, "checkout", branch)
	runGit(t, dir, "merge", "--no-commit", "-s", "ours", side)
	require.Empty(t, strings.TrimSpace(gitOutput(t, dir, "status", "--porcelain")))
}

func TestACPAutoMergeConflictAborts(t *testing.T) {
	repo := setupACPMergeRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "feature.txt"), []byte("base side\n"), 0o600))
	runGit(t, repo.primary, "add", "feature.txt")
	runGit(t, repo.primary, "commit", "-m", "conflicting base work")
	masterBefore := revParse(t, repo.primary, "refs/heads/master")

	res := acpAutoMerge(t.Context(), repo.svc, acpMergeConfig(), "master", acpRiskOutcome(t, repo, "low"))

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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.res.summary())
		})
	}
}

func TestACPRequestedMergeIgnoresPolicyGates(t *testing.T) {
	// a human asked, so a high-risk report merges; acpRequestedMerge reads no config at all
	repo := setupACPMergeRepo(t)
	featureHead := revParse(t, repo.worktree, "HEAD")
	lastRun := &acpLastRun{Branch: "feature", Base: "master", Tip: featureHead, Report: acpRiskReport("**high**")}

	res := acpRequestedMerge(t.Context(), repo.svc, "master", lastRun)

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, "fast-forward", res.kind)
	assert.Empty(t, res.added)
	assert.Equal(t, featureHead, revParse(t, repo.primary, "refs/heads/master"))
	assert.Equal(t, "master", currentGitBranch(t, repo.primary))
	assert.Equal(t, "feature", currentGitBranch(t, repo.worktree))
	assert.True(t, branchExists(t, repo.primary, "feature"))
	assert.DirExists(t, repo.worktree)
	assert.Contains(t, gitOutput(t, repo.primary, "worktree", "list"), filepath.Base(repo.worktree))
}

func TestACPRequestedMergeListsCommitsAfterReport(t *testing.T) {
	repo := setupACPMergeRepo(t)
	reportTip := revParse(t, repo.worktree, "HEAD")
	runGit(t, repo.worktree, "commit", "--allow-empty", "-m", "fix after report")
	runGit(t, repo.worktree, "commit", "--allow-empty", "-m", "second fix")
	featureHead := revParse(t, repo.worktree, "HEAD")
	lastRun := &acpLastRun{Branch: "feature", Base: "master", Tip: reportTip, Report: acpRiskReport("low")}

	res := acpRequestedMerge(t.Context(), repo.svc, "master", lastRun)

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, featureHead, revParse(t, repo.primary, "refs/heads/master"), "the current head is merged, not the report tip")
	require.Len(t, res.added, 2)
	assert.Equal(t, "second fix", res.added[0].Subject)
	assert.Equal(t, "fix after report", res.added[1].Subject)
	msg := res.message()
	assert.Contains(t, msg, "Commits added after the report:\n- `"+featureHead[:7]+"` second fix\n- `")
	assert.Contains(t, msg, "` fix after report")
	assert.DirExists(t, repo.worktree)
}

func TestACPRequestedMergeMergeCommit(t *testing.T) {
	repo := setupACPMergeRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "base.txt"), []byte("base\n"), 0o600))
	runGit(t, repo.primary, "add", "base.txt")
	runGit(t, repo.primary, "commit", "-m", "base work")

	res := acpRequestedMerge(t.Context(), repo.svc, "master", nil)

	require.True(t, res.merged, "skipped: %s", res.skipped)
	assert.Equal(t, "merge commit", res.kind)
	assert.Empty(t, res.added, "no recorded run lists nothing")
	assert.Equal(t, "Merge branch 'feature'", strings.TrimSpace(gitOutput(t, repo.primary, "log", "-1", "--format=%s")))
	assert.Equal(t, revParse(t, repo.worktree, "HEAD"), revParse(t, repo.primary, "HEAD^2"))
}

// TestACPRequestedMergeAlreadyInBase covers a branch the base already contains: nothing is merged,
// and the base worktree need not even be clean or checked out.
func TestACPRequestedMergeAlreadyInBase(t *testing.T) {
	repo := setupACPMergeRepo(t)
	runGit(t, repo.primary, "merge", "--ff-only", "feature")
	runGit(t, repo.primary, "checkout", "-b", "other")
	require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "wip.txt"), []byte("wip\n"), 0o600))
	masterBefore := revParse(t, repo.primary, "refs/heads/master")

	res := acpRequestedMerge(t.Context(), repo.svc, "master", nil)

	assert.True(t, res.upToDate)
	assert.False(t, res.merged)
	assert.Empty(t, res.skipped)
	assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
	assert.Equal(t, "other", currentGitBranch(t, repo.primary))
	assert.Equal(t, "## Merge\n\n`feature` is already in `master`; nothing to merge.", res.message())
	assert.Equal(t, "feature is already in master", res.summary())
}

func TestACPRequestedMergeSkips(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		prepare func(t *testing.T, repo acpMergeRepo)
		want    string
	}{
		{name: "missing base", base: "trunk", want: `base branch "trunk" does not exist locally`},
		{name: "detached head", prepare: func(t *testing.T, repo acpMergeRepo) {
			runGit(t, repo.worktree, "checkout", "--detach")
		}, want: "HEAD is detached"},
		{name: "feature equals base", base: "feature", want: "the run is on the base branch itself"},
		{name: "dirty feature", prepare: func(t *testing.T, repo acpMergeRepo) {
			require.NoError(t, os.WriteFile(filepath.Join(repo.worktree, "feature.txt"), []byte("edited\n"), 0o600))
		}, want: "the feature worktree at"},
		{name: "untracked feature file", prepare: func(t *testing.T, repo acpMergeRepo) {
			require.NoError(t, os.WriteFile(filepath.Join(repo.worktree, "notes.txt"), []byte("x\n"), 0o600))
		}, want: "has untracked files"},
		{name: "dirty base", prepare: func(t *testing.T, repo acpMergeRepo) {
			require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "README.md"), []byte("edited\n"), 0o600))
		}, want: "the base worktree at"},
		{name: "operation in progress in base", prepare: func(t *testing.T, repo acpMergeRepo) {
			acpPendingMerge(t, repo.primary, "master", "side")
		}, want: "has a merge in progress"},
		{name: "operation in progress in feature", prepare: func(t *testing.T, repo acpMergeRepo) {
			acpPendingMerge(t, repo.worktree, "feature", "feature-side")
		}, want: "the feature worktree at"},
		{name: "base checked out nowhere", prepare: func(t *testing.T, repo acpMergeRepo) {
			runGit(t, repo.primary, "checkout", "-b", "other")
		}, want: `base branch "master" is not checked out in any worktree`},
		{name: "conflict aborted", prepare: func(t *testing.T, repo acpMergeRepo) {
			require.NoError(t, os.WriteFile(filepath.Join(repo.primary, "feature.txt"), []byte("base side\n"), 0o600))
			runGit(t, repo.primary, "add", "feature.txt")
			runGit(t, repo.primary, "commit", "-m", "conflicting base work")
		}, want: "conflicted and was aborted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := setupACPMergeRepo(t)
			if tc.prepare != nil {
				tc.prepare(t, repo)
			}
			base := tc.base
			if base == "" {
				base = "master"
			}
			masterBefore := revParse(t, repo.primary, "refs/heads/master")
			primaryBranch := currentGitBranch(t, repo.primary)

			res := acpRequestedMerge(t.Context(), repo.svc, base, &acpLastRun{Branch: "feature", Tip: "0123456"})

			assert.False(t, res.merged)
			assert.False(t, res.upToDate)
			assert.Contains(t, res.skipped, tc.want)
			assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
			assert.Equal(t, primaryBranch, currentGitBranch(t, repo.primary))
			assert.True(t, branchExists(t, repo.primary, "feature"))
			assert.DirExists(t, repo.worktree)
		})
	}
}

func TestACPRequestedMergeWaitsForRepositoryLock(t *testing.T) {
	repo := setupACPMergeRepo(t)
	masterBefore := revParse(t, repo.primary, "refs/heads/master")
	other, err := git.NewService(repo.primary, noopLogger())
	require.NoError(t, err)
	release, err := other.AcquireWorktreeCreationLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, release()) })
	prev := acpMergeLockTimeout
	acpMergeLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { acpMergeLockTimeout = prev })

	res := acpRequestedMerge(t.Context(), repo.svc, "master", nil)

	assert.False(t, res.merged)
	assert.Contains(t, res.skipped, "another loopai process holds the repository lock")
	assert.Equal(t, masterBefore, revParse(t, repo.primary, "refs/heads/master"))
}

func TestACPCommitsAfterRun(t *testing.T) {
	repo := setupACPMergeRepo(t)
	tip := revParse(t, repo.worktree, "HEAD")
	runGit(t, repo.worktree, "commit", "--allow-empty", "-m", "later")
	head := revParse(t, repo.worktree, "HEAD")

	tests := []struct {
		name    string
		lastRun *acpLastRun
		want    []string
	}{
		{name: "no run"},
		{name: "no tip", lastRun: &acpLastRun{Branch: "feature"}},
		{name: "head unchanged", lastRun: &acpLastRun{Branch: "feature", Tip: head}},
		{name: "run on another branch", lastRun: &acpLastRun{Branch: "other", Tip: tip}},
		{name: "unknown tip", lastRun: &acpLastRun{Branch: "feature", Tip: "0123456789abcdef0123456789abcdef01234567"}},
		{name: "moved", lastRun: &acpLastRun{Branch: "feature", Tip: tip}, want: []string{"later"}},
		{name: "moved without recorded branch", lastRun: &acpLastRun{Tip: tip}, want: []string{"later"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var subjects []string
			for _, c := range acpCommitsAfterRun(repo.svc, tc.lastRun, "feature", head) {
				subjects = append(subjects, c.Subject)
			}
			assert.Equal(t, tc.want, subjects)
		})
	}
}

func TestACPAddedCommits(t *testing.T) {
	assert.Empty(t, acpAddedCommits(nil))
	commits := make([]git.Commit, acpAddedCommitsLimit+3)
	for i := range commits {
		commits[i] = git.Commit{Hash: "abcdef0123456789", Subject: "fix"}
	}
	got := acpAddedCommits(commits)
	assert.True(t, strings.HasPrefix(got, "\n\nCommits added after the report:\n- `abcdef0` fix\n"))
	assert.Equal(t, acpAddedCommitsLimit, strings.Count(got, "` fix"))
	assert.True(t, strings.HasSuffix(got, "\n- and 3 more"))
}

func TestACPActionBase(t *testing.T) {
	repo := setupACPMergeRepo(t)
	tests := []struct {
		name    string
		cfg     *config.Config
		lastRun *acpLastRun
		want    string
	}{
		{name: "last run base wins", cfg: &config.Config{DefaultBranch: "develop"}, lastRun: &acpLastRun{Base: "release"}, want: "release"},
		{name: "configured default without run", cfg: &config.Config{DefaultBranch: "origin/develop"}, want: "develop"},
		{name: "run without base uses config", cfg: &config.Config{DefaultBranch: "develop"}, lastRun: &acpLastRun{}, want: "develop"},
		{name: "detected default", cfg: &config.Config{}, want: "master"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, acpActionBase(repo.svc, tc.cfg, tc.lastRun))
		})
	}
}

// acpPRRepo is a checkout on the feature branch whose GitHub origin is redirected to a local bare
// repository by a git wrapper, with a gh stub on PATH that records its calls.
type acpPRRepo struct {
	dir    string
	remote string
	svc    *git.Service
	calls  string // one gh argv per line
	body   string // the body piped to gh pr create
}

// setupACPPRRepo prepares the repository; existingPR, when set, is the URL gh pr view reports.
func setupACPPRRepo(t *testing.T, existingPR string) acpPRRepo {
	t.Helper()
	dir := setupTestRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, filepath.Dir(remote), "init", "--bare", remote)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/acme/repo.git")
	runGit(t, dir, "checkout", "-b", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
	runGit(t, dir, "add", "feature.txt")
	runGit(t, dir, "commit", "-m", "feature")
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	t.Setenv("PR_TEST_REAL_GIT", realGit)
	t.Setenv("PR_TEST_REMOTE", remote)
	gitWrapper := filepath.Join(t.TempDir(), "git-wrapper")
	writeExecutable(t, gitWrapper, "#!/bin/sh\nif [ \"$1\" = push ]; then\n  refspec=$4\n  \"$PR_TEST_REAL_GIT\" push \"$PR_TEST_REMOTE\" \"$refspec\" || exit $?\n  branch=${refspec#refs/heads/}\n  branch=${branch%%:*}\n  \"$PR_TEST_REAL_GIT\" update-ref \"refs/remotes/origin/$branch\" \"$branch\" || exit $?\n  exit 0\nfi\nexec \"$PR_TEST_REAL_GIT\" \"$@\"\n")
	svc, err := git.NewService(dir, noopLogger(), gitWrapper)
	require.NoError(t, err)

	binDir := t.TempDir()
	repo := acpPRRepo{dir: dir, remote: remote, svc: svc,
		calls: filepath.Join(binDir, "gh-calls.log"), body: filepath.Join(binDir, "gh-body.log")}
	writeExecutable(t, filepath.Join(binDir, "gh"), `#!/bin/sh
echo "$*" >> "$GH_CALLS"
case "$1 $2" in
"repo view") echo acme/repo ;;
"pr view")
  if [ -n "$GH_EXISTING" ]; then echo "$GH_EXISTING"; exit 0; fi
  echo 'no pull requests found for branch "feature"' >&2; exit 1 ;;
"pr create") cat > "$GH_BODY"; echo https://github.com/acme/repo/pull/7 ;;
*) exit 2 ;;
esac
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CALLS", repo.calls)
	t.Setenv("GH_BODY", repo.body)
	t.Setenv("GH_EXISTING", existingPR)
	return repo
}

func (r acpPRRepo) ghCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(r.calls)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

func TestACPRequestedPRCreates(t *testing.T) {
	repo := setupACPPRRepo(t, "")
	lastRun := &acpLastRun{Branch: "feature", Base: "master", Report: prReportFixture}

	res := acpRequestedPR(t.Context(), repo.svc, &config.Config{PlansDir: "docs/plans"}, "master", lastRun)

	require.Empty(t, res.skipped)
	assert.Equal(t, "https://github.com/acme/repo/pull/7", res.url)
	assert.False(t, res.existing)
	assert.Equal(t, "## Pull request\n\nPushed `feature` and opened a pull request against `master`: https://github.com/acme/repo/pull/7",
		res.message())
	calls := repo.ghCalls(t)
	assert.Contains(t, calls, "pr view feature --repo acme/repo --json url,state --jq select(.state == \"OPEN\") | .url\n")
	assert.Contains(t, calls, "pr create --repo acme/repo --base master --head feature")
	body, err := os.ReadFile(repo.body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "Delivered report-backed PRs.", "the body comes from the last run report")
	assert.Equal(t, revParse(t, repo.dir, "feature"), revParse(t, repo.remote, "refs/heads/feature"))
	assert.Equal(t, "feature", currentGitBranch(t, repo.dir))
}

// TestACPRequestedPRReportFromOtherBranch covers a last run recorded on another branch: its report
// does not describe this branch, so the body falls back to the legacy one.
func TestACPRequestedPRReportFromOtherBranch(t *testing.T) {
	repo := setupACPPRRepo(t, "")

	res := acpRequestedPR(t.Context(), repo.svc, &config.Config{PlansDir: "docs/plans"}, "master",
		&acpLastRun{Branch: "other", Report: prReportFixture})

	require.Empty(t, res.skipped)
	body, err := os.ReadFile(repo.body)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "Delivered report-backed PRs.")
	assert.Contains(t, string(body), "## Changes")
}

func TestACPRequestedPRExisting(t *testing.T) {
	repo := setupACPPRRepo(t, "https://github.com/acme/repo/pull/3")

	res := acpRequestedPR(t.Context(), repo.svc, &config.Config{PlansDir: "docs/plans"}, "master", nil)

	require.Empty(t, res.skipped)
	assert.True(t, res.existing)
	assert.Equal(t, "https://github.com/acme/repo/pull/3", res.url)
	assert.Equal(t, "## Pull request\n\nA pull request for `feature` is already open: https://github.com/acme/repo/pull/3",
		res.message())
	assert.NotContains(t, repo.ghCalls(t), "pr create")
	cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/feature")
	cmd.Dir = repo.remote
	assert.Error(t, cmd.Run(), "an existing pull request pushes nothing")
}

func TestACPRequestedPRRefusals(t *testing.T) {
	t.Run("gh missing", func(t *testing.T) {
		repo := setupACPMergeRepo(t)
		t.Setenv("PATH", t.TempDir())

		res := acpRequestedPR(t.Context(), repo.svc, &config.Config{PlansDir: "docs/plans"}, "master", nil)

		assert.Contains(t, res.skipped, "pr needs GitHub CLI (gh) in PATH")
		assert.Empty(t, res.url)
		assert.Equal(t, "## Pull request\n\nNo pull request opened against `master`: pr needs GitHub CLI (gh) in PATH; "+
			"install it from https://cli.github.com/.", res.message())
	})
	t.Run("detached head", func(t *testing.T) {
		repo := setupACPPRRepo(t, "")
		runGit(t, repo.dir, "checkout", "--detach")

		res := acpRequestedPR(t.Context(), repo.svc, &config.Config{PlansDir: "docs/plans"}, "master", nil)

		assert.Equal(t, "HEAD is detached", res.skipped)
		assert.Empty(t, repo.ghCalls(t))
	})
	t.Run("missing base", func(t *testing.T) {
		repo := setupACPPRRepo(t, "")

		res := acpRequestedPR(t.Context(), repo.svc, &config.Config{PlansDir: "docs/plans"}, "trunk", nil)

		assert.Contains(t, res.skipped, `base branch "trunk" does not exist locally`)
	})
	t.Run("gh create failure", func(t *testing.T) {
		repo := setupACPPRRepo(t, "")
		binDir := t.TempDir()
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nif [ \"$1\" = repo ]; then echo acme/repo; exit 0; fi\n"+
			"if [ \"$2\" = view ]; then exit 1; fi\necho 'authentication required' >&2\nexit 1\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		res := acpRequestedPR(t.Context(), repo.svc, &config.Config{PlansDir: "docs/plans"}, "master", nil)

		assert.Contains(t, res.skipped, "authentication required")
		assert.Empty(t, res.url)
	})
}

func TestACPPRResultMessageWithoutURL(t *testing.T) {
	res := acpPRResult{feature: "feat", base: "main"}
	assert.Equal(t, "## Pull request\n\nPushed `feat` and opened a pull request against `main`.", res.message())
}
