package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
)

// acpMergeLockTimeout bounds the wait for another loopai process's repository lock before the
// merge is skipped.
var acpMergeLockTimeout = 2 * time.Minute

// acpMergeResult is the outcome of acpAutoMerge or acpRequestedMerge. Exactly one of merged,
// upToDate, and skipped is set.
type acpMergeResult struct {
	merged   bool
	upToDate bool   // the base already contains the feature head, so nothing was merged
	kind     string // fast-forward, merge commit, or already up to date
	base     string
	feature  string
	head     string       // short SHA of the base after the merge
	added    []git.Commit // commits merged that the session's last run did not cover, newest first
	skipped  string       // why no merge happened; the repository is unchanged
}

// acpAutoMerge merges the plan branch checked out at gitSvc's root into the local base branch after
// a successful ACP run. It runs only when every precondition holds and otherwise reports why it
// skipped without touching the repository. The merge takes the commit the run completed on, happens
// in the worktree the base is already checked out in under the repository lock, is never pushed, and deletes neither the branch nor any worktree: the feature
// worktree belongs to T3 Code and is the process cwd.
func acpAutoMerge(ctx context.Context, gitSvc *git.Service, cfg *config.Config, defaultBranch string,
	outcome planExecutionOutcome) acpMergeResult {
	base := acpMergeBase(defaultBranch)
	result := acpMergeResult{base: base}
	skip := func(reason string) acpMergeResult {
		result.skipped = reason
		return result
	}

	if reason := acpMergePolicySkip(cfg, outcome); reason != "" {
		return skip(reason)
	}

	feature, reason := acpFeatureBranch(gitSvc, base)
	if reason != "" {
		return skip(reason)
	}
	result.feature = feature
	if reason = acpCleanWorktreeSkip(gitSvc, "feature"); reason != "" {
		return skip(reason)
	}
	featureHead, reason := acpCompletedTip(gitSvc, feature, outcome.branchTip)
	if reason != "" {
		return skip(reason)
	}
	return acpMergeIntoBase(ctx, gitSvc, result, featureHead)
}

// acpRequestedMerge merges the plan branch checked out at gitSvc's root into the local base branch
// because the user asked for it in the thread. A human asked, so none of acpAutoMerge's policy
// gates apply (acp_auto_merge, the finalize mode, the report's Risk), and the merge takes the
// branch's current head, including commits added after the run, which the result lists. Every
// repository gate and the repository lock still apply. A branch already contained in the base is
// a no-op. Like the auto-merge it never pushes, deletes no branch or worktree, and switches no
// checkout: the feature worktree belongs to T3 Code and is the process cwd.
func acpRequestedMerge(ctx context.Context, gitSvc *git.Service, base string, lastRun *acpLastRun) acpMergeResult {
	result := acpMergeResult{base: base}
	skip := func(format string, args ...any) acpMergeResult {
		result.skipped = fmt.Sprintf(format, args...)
		return result
	}

	feature, reason := acpFeatureBranch(gitSvc, base)
	if reason != "" {
		return skip("%s", reason)
	}
	result.feature = feature
	if reason = acpCleanWorktreeSkip(gitSvc, "feature"); reason != "" {
		return skip("%s", reason)
	}
	featureHead, err := gitSvc.BranchHash(feature)
	if err != nil {
		return skip("cannot read the feature branch head: %v", err)
	}
	contained, err := gitSvc.BranchContainsRevisionContext(ctx, base, featureHead)
	if err != nil {
		return skip("cannot check whether %q already contains %q: %v", base, feature, err)
	}
	if contained {
		result.upToDate = true
		return result
	}
	result.added = acpCommitsAfterRun(gitSvc, lastRun, feature, featureHead)
	return acpMergeIntoBase(ctx, gitSvc, result, featureHead)
}

// acpFeatureBranch returns the branch checked out at gitSvc's root, or why it cannot be merged into
// base or opened as a pull request against it.
func acpFeatureBranch(gitSvc *git.Service, base string) (string, string) {
	feature, err := gitSvc.CurrentBranch()
	if err != nil {
		return "", fmt.Sprintf("cannot resolve the current branch: %v", err)
	}
	if feature == "" {
		return "", "HEAD is detached"
	}
	if !gitSvc.BranchExists(base) {
		return "", fmt.Sprintf("base branch %q does not exist locally", base)
	}
	if base == feature {
		return "", "the run is on the base branch itself"
	}
	return feature, ""
}

// acpMergeIntoBase merges featureHead into result.base in the worktree the base is checked out in,
// under the repository lock, and fills in the outcome.
func acpMergeIntoBase(ctx context.Context, gitSvc *git.Service, result acpMergeResult, featureHead string) acpMergeResult {
	skip := func(format string, args ...any) acpMergeResult {
		result.skipped = fmt.Sprintf(format, args...)
		return result
	}

	// another ACP process can finish at the same time and merge into the same base checkout; a merge
	// failing on Git's index lock while the other succeeds would see HEAD move and reset it back,
	// so the base checks and the merge run under the repository's shared lock
	lockCtx, cancel := context.WithTimeout(ctx, acpMergeLockTimeout)
	release, err := gitSvc.AcquireWorktreeCreationLockContext(lockCtx)
	cancel()
	if err != nil {
		return skip("another loopai process holds the repository lock: %v", err)
	}
	defer func() { _ = release() }() // the OS drops the advisory lock with the process regardless

	mergeSvc, reason := acpBaseWorktree(gitSvc, result.base)
	if reason != "" {
		return skip("%s", reason)
	}

	merge, err := mergeIntoBase(ctx, mergeSvc, result.feature, result.base, featureHead, mergeSvc.MergeBranchTipContext)
	if err != nil {
		return skip("%v", err)
	}
	result.merged = true
	result.kind = merge.mergeType
	if head, headErr := mergeSvc.HeadHash(); headErr == nil {
		result.head = shortSHA(head)
	}
	return result
}

// acpCommitsAfterRun lists the commits on feature after the commit the session's last run
// completed on, newest first, so a requested merge names what its report did not cover. It is
// best-effort: no recorded run, a run on another branch, or an unreadable range lists nothing.
func acpCommitsAfterRun(gitSvc *git.Service, lastRun *acpLastRun, feature, featureHead string) []git.Commit {
	if lastRun == nil || lastRun.Tip == "" || lastRun.Tip == featureHead {
		return nil
	}
	if lastRun.Branch != "" && lastRun.Branch != feature {
		return nil
	}
	commits, err := gitSvc.CommitsBetween(lastRun.Tip, featureHead)
	if err != nil {
		return nil
	}
	return commits
}

// acpPRResult is the outcome of acpRequestedPR. Exactly one of url and skipped is set, except
// that gh may print no URL for a created pull request.
type acpPRResult struct {
	feature  string
	base     string
	url      string
	existing bool   // url names an open pull request that was already there
	skipped  string // why no pull request was opened
}

// acpRequestedPR pushes the plan branch checked out at gitSvc's root and opens a pull request
// against base, because the user asked for it in the thread. The body comes from the session's
// last run report, falling back to the body --pr builds; an open pull request for the branch is
// reported instead of a second one. Every failure is a refusal the thread shows, not a failed
// turn. The worktree, its checkout, and the branch are left as they are.
func acpRequestedPR(ctx context.Context, gitSvc *git.Service, cfg *config.Config, base string,
	lastRun *acpLastRun) acpPRResult {
	result := acpPRResult{base: base}
	skip := func(format string, args ...any) acpPRResult {
		result.skipped = fmt.Sprintf(format, args...)
		return result
	}

	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return skip("pr needs GitHub CLI (gh) in PATH; install it from https://cli.github.com/")
	}
	feature, reason := acpFeatureBranch(gitSvc, base)
	if reason != "" {
		return skip("%s", reason)
	}
	result.feature = feature
	if url := acpOpenPRURL(ctx, ghPath, gitSvc, feature); url != "" {
		result.url, result.existing = url, true
		return result
	}

	report := ""
	if lastRun != nil && (lastRun.Branch == "" || lastRun.Branch == feature) {
		report = lastRun.Report
	}
	url, err := createPullRequest(ctx, ghPath, gitSvc, feature, base, closeoutTarget{plansDir: cfg.PlansDir, report: report})
	if err != nil {
		return skip("%v", err)
	}
	result.url = url
	return result
}

// acpOpenPRURL returns the URL of the open pull request whose head is branch in origin's GitHub
// repository, or "" when there is none or the lookup fails: creation then reports the real error.
func acpOpenPRURL(ctx context.Context, ghPath string, gitSvc *git.Service, branch string) string {
	originURL, err := gitSvc.OriginURL()
	if err != nil {
		return ""
	}
	repoSpec, err := githubRepoSpec(originURL)
	if err != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, ghPath, "pr", "view", branch, "--repo", repoSpec,
		"--json", "url,state", "--jq", `select(.state == "OPEN") | .url`)
	cmd.Dir = gitSvc.Root()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// message renders the outcome as a Markdown section for the thread.
func (r acpPRResult) message() string {
	switch {
	case r.skipped != "":
		return fmt.Sprintf("## Pull request\n\nNo pull request opened against %#q: %s.", r.base, strings.TrimRight(r.skipped, "."))
	case r.existing:
		return fmt.Sprintf("## Pull request\n\nA pull request for %#q is already open: %s", r.feature, r.url)
	case r.url == "":
		return fmt.Sprintf("## Pull request\n\nPushed %#q and opened a pull request against %#q.", r.feature, r.base)
	default:
		return fmt.Sprintf("## Pull request\n\nPushed %#q and opened a pull request against %#q: %s", r.feature, r.base, r.url)
	}
}

// acpActionBase names the local branch a requested merge or pull request targets: the session's
// last run base, otherwise the configured or detected default branch a run would use.
func acpActionBase(gitSvc *git.Service, cfg *config.Config, lastRun *acpLastRun) string {
	if lastRun != nil && lastRun.Base != "" {
		return lastRun.Base
	}
	return acpMergeBase(resolveDefaultBranch("", cfg.DefaultBranch, gitSvc.GetDefaultBranch()))
}

// acpCompletedTip returns the commit the run completed on, or why it cannot be merged. The merge
// takes that commit rather than the branch, and a branch that has moved since is refused: commits
// added after the run were not covered by its report's Risk assessment.
func acpCompletedTip(gitSvc *git.Service, feature, completedTip string) (string, string) {
	if completedTip == "" {
		return "", "the run did not record the commit it completed on"
	}
	head, err := gitSvc.BranchHash(feature)
	if err != nil {
		return "", fmt.Sprintf("cannot read the feature branch head: %v", err)
	}
	if head != completedTip {
		return "", fmt.Sprintf("branch %q moved from %s to %s after the run completed", feature,
			shortSHA(completedTip), shortSHA(head))
	}
	return completedTip, ""
}

// acpMergeBase names the local branch the plan branch merges into: the run's diff base without the
// remote prefix.
func acpMergeBase(defaultBranch string) string {
	return strings.TrimPrefix(strings.TrimSpace(defaultBranch), "origin/")
}

// acpMergePolicySkip returns why configuration, an incomplete finalize sync, or the report's Risk
// level rules the merge out, or "" when the merge may proceed. A sync that stopped keeps the run
// green, but it stopped because the base and the branch did not combine cleanly or failed
// validation together, or it left the checkout unrestored, so the base must not receive the branch.
func acpMergePolicySkip(cfg *config.Config, outcome planExecutionOutcome) string {
	if !cfg.ACPAutoMerge {
		return "acp_auto_merge is disabled"
	}
	if mode := cfg.EffectiveFinalize(); mode == config.FinalizePR || mode == config.FinalizeMerge {
		return fmt.Sprintf("finalize = %s owns the close-out", mode)
	}
	if outcome.finalizeIncomplete != nil {
		return fmt.Sprintf("finalize incomplete: %v", outcome.finalizeIncomplete)
	}
	switch risk := reportRiskLevel(outcome.report); risk {
	case "low", "medium":
		return ""
	case "":
		return "Risk level not stated in the report"
	default:
		return "Risk is " + risk
	}
}

// acpBaseWorktree opens the clean worktree the base branch is checked out in, or returns why it
// cannot be used. The base is merged only where it is already checked out: switching a checkout to
// the base on the user's behalf would change a working tree they did not hand to this run.
func acpBaseWorktree(gitSvc *git.Service, base string) (*git.Service, string) {
	worktrees, err := gitSvc.Worktrees()
	if err != nil {
		return nil, fmt.Sprintf("cannot list worktrees: %v", err)
	}
	basePath := worktreePathForBranch(worktrees, base)
	if basePath == "" {
		return nil, fmt.Sprintf("base branch %q is not checked out in any worktree", base)
	}
	mergeSvc, err := openMergeWorktree(gitSvc, basePath)
	if err != nil {
		return nil, err.Error()
	}
	if reason := acpCleanWorktreeSkip(mergeSvc, "base"); reason != "" {
		return nil, reason
	}
	return mergeSvc, ""
}

// acpCleanWorktreeSkip returns why the worktree at svc's root cannot take part in the merge, or "".
// The operation check comes first: a pending merge whose index matches HEAD reads as clean, and the
// merge's failure path would then abort it with git merge --abort.
func acpCleanWorktreeSkip(svc *git.Service, role string) string {
	op, err := svc.OperationInProgress()
	if err != nil {
		return fmt.Sprintf("cannot check the %s worktree: %v", role, err)
	}
	if op != "" {
		return fmt.Sprintf("the %s worktree at %s has a %s in progress", role, svc.Root(), op)
	}
	dirty, err := svc.IsDirty()
	if err != nil {
		return fmt.Sprintf("cannot check the %s worktree: %v", role, err)
	}
	if dirty {
		return fmt.Sprintf("the %s worktree at %s has uncommitted changes", role, svc.Root())
	}
	untracked, err := svc.UntrackedFiles()
	if err != nil {
		return fmt.Sprintf("cannot check the %s worktree: %v", role, err)
	}
	for _, path := range untracked {
		if !isLocalOverridePath(path) {
			return fmt.Sprintf("the %s worktree at %s has untracked files", role, svc.Root())
		}
	}
	return ""
}

// isLocalOverridePath reports whether an untracked path is a project-local .loopai override.
// --t3-launch copies those into the thread's worktree uncommitted, and the source checkout that
// normally holds the base keeps its own copies, so counting them would skip every merge for a
// project with local overrides. Ignoring them is safe: git merge refuses to overwrite an untracked
// file, and the failed merge leaves the base unchanged.
func isLocalOverridePath(path string) bool {
	return path == ".loopai/config" || strings.HasPrefix(path, ".loopai/prompts/") ||
		strings.HasPrefix(path, ".loopai/agents/")
}

func shortSHA(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}

// summary renders the outcome as one plain line for the run's human-readable output.
func (r acpMergeResult) summary() string {
	if r.upToDate {
		return fmt.Sprintf("%s is already in %s", r.feature, r.base)
	}
	if r.merged {
		head := ""
		if r.head != "" {
			head = ", " + r.head
		}
		return fmt.Sprintf("merged %s into %s (%s%s), not pushed", r.feature, r.base, r.kind, head)
	}
	return fmt.Sprintf("not merged into %s: %s", r.base, strings.TrimRight(r.skipped, "."))
}

// message renders the outcome as a Markdown section for the final thread message.
func (r acpMergeResult) message() string {
	if r.upToDate {
		return fmt.Sprintf("## Merge\n\n%#q is already in %#q; nothing to merge.", r.feature, r.base)
	}
	if r.merged {
		head := ""
		if r.head != "" {
			head = fmt.Sprintf(", %#q", r.head)
		}
		msg := fmt.Sprintf("## Merge\n\nMerged %#q into %#q (%s%s). Not pushed.", r.feature, r.base, r.kind, head)
		return msg + acpAddedCommits(r.added)
	}
	return fmt.Sprintf("## Merge\n\nNot merged into %#q: %s.", r.base, strings.TrimRight(r.skipped, "."))
}

// acpAddedCommitsLimit caps the commits a merge message lists after the report.
const acpAddedCommitsLimit = 20

// acpAddedCommits renders the commits merged after the report as a Markdown list, or "".
func acpAddedCommits(commits []git.Commit) string {
	if len(commits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nCommits added after the report:\n")
	for i, c := range commits {
		if i == acpAddedCommitsLimit {
			fmt.Fprintf(&b, "\n- and %d more", len(commits)-i)
			break
		}
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- %#q %s", shortSHA(c.Hash), c.Subject)
	}
	return b.String()
}
