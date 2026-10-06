package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
)

// acpMergeLockTimeout bounds the wait for another loopai process's repository lock before the
// merge is skipped.
var acpMergeLockTimeout = 2 * time.Minute

// acpMergeResult is the outcome of acpAutoMerge. Exactly one of merged and skipped is set.
type acpMergeResult struct {
	merged  bool
	kind    string // fast-forward, merge commit, or already up to date
	base    string
	feature string
	head    string // short SHA of the base after the merge
	skipped string // why no merge happened; the repository is unchanged
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
	skip := func(format string, args ...any) acpMergeResult {
		result.skipped = fmt.Sprintf(format, args...)
		return result
	}

	if reason := acpMergePolicySkip(cfg, outcome); reason != "" {
		return skip("%s", reason)
	}

	feature, err := gitSvc.CurrentBranch()
	if err != nil {
		return skip("cannot resolve the current branch: %v", err)
	}
	if feature == "" {
		return skip("HEAD is detached")
	}
	result.feature = feature
	if !gitSvc.BranchExists(base) {
		return skip("base branch %q does not exist locally", base)
	}
	if base == feature {
		return skip("the run is on the base branch itself")
	}

	if reason := acpCleanWorktreeSkip(gitSvc, "feature"); reason != "" {
		return skip("%s", reason)
	}
	featureHead, reason := acpCompletedTip(gitSvc, feature, outcome.branchTip)
	if reason != "" {
		return skip("%s", reason)
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

	mergeSvc, reason := acpBaseWorktree(gitSvc, base)
	if reason != "" {
		return skip("%s", reason)
	}

	merge, err := mergeIntoBase(ctx, mergeSvc, feature, base, featureHead, mergeSvc.MergeBranchTipContext)
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
	dirty, err := svc.IsDirtyAll()
	if err != nil {
		return fmt.Sprintf("cannot check the %s worktree: %v", role, err)
	}
	if dirty {
		return fmt.Sprintf("the %s worktree at %s has uncommitted changes", role, svc.Root())
	}
	return ""
}

func shortSHA(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}

// summary renders the outcome as one plain line for the run's human-readable output.
func (r acpMergeResult) summary() string {
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
	if r.merged {
		head := ""
		if r.head != "" {
			head = fmt.Sprintf(", %#q", r.head)
		}
		return fmt.Sprintf("## Merge\n\nMerged %#q into %#q (%s%s). Not pushed.", r.feature, r.base, r.kind, head)
	}
	return fmt.Sprintf("## Merge\n\nNot merged into %#q: %s.", r.base, strings.TrimRight(r.skipped, "."))
}
