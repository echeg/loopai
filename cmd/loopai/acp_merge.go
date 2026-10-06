package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
)

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
// skipped without touching the repository. The merge happens in the worktree the base is already
// checked out in, is never pushed, and deletes neither the branch nor any worktree: the feature
// worktree belongs to T3 Code and is the process cwd.
func acpAutoMerge(ctx context.Context, gitSvc *git.Service, cfg *config.Config, defaultBranch, report string) acpMergeResult {
	base := strings.TrimPrefix(strings.TrimSpace(defaultBranch), "origin/")
	if base == "" {
		base = strings.TrimPrefix(gitSvc.GetDefaultBranch(), "origin/")
	}
	result := acpMergeResult{base: base}
	skip := func(format string, args ...any) acpMergeResult {
		result.skipped = fmt.Sprintf(format, args...)
		return result
	}

	if reason := acpMergePolicySkip(cfg, report); reason != "" {
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
	if base == "" || !gitSvc.BranchExists(base) {
		return skip("base branch %q does not exist locally", base)
	}
	if base == feature {
		return skip("the run is on the base branch itself")
	}

	dirty, err := gitSvc.IsDirtyAll()
	if err != nil {
		return skip("cannot check the feature worktree: %v", err)
	}
	if dirty {
		return skip("the feature worktree at %s has uncommitted changes", gitSvc.Root())
	}

	mergeSvc, reason := acpBaseWorktree(gitSvc, base)
	if reason != "" {
		return skip("%s", reason)
	}

	featureHead, err := gitSvc.BranchHash(feature)
	if err != nil {
		return skip("cannot read the feature branch head: %v", err)
	}
	merge, err := mergeForCloseout(ctx, mergeSvc, feature, base, featureHead)
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

// acpMergePolicySkip returns why configuration or the report's Risk level rules the merge out, or ""
// when the merge may proceed.
func acpMergePolicySkip(cfg *config.Config, report string) string {
	if cfg == nil || !cfg.ACPAutoMerge {
		return "acp_auto_merge is disabled"
	}
	if mode := cfg.EffectiveFinalize(); mode == config.FinalizePR || mode == config.FinalizeMerge {
		return fmt.Sprintf("finalize = %s owns the close-out", mode)
	}
	switch risk := reportRiskLevel(report); risk {
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
	dirty, err := mergeSvc.IsDirtyAll()
	if err != nil {
		return nil, fmt.Sprintf("cannot check the base worktree: %v", err)
	}
	if dirty {
		return nil, fmt.Sprintf("the base worktree at %s has uncommitted changes", mergeSvc.Root())
	}
	return mergeSvc, ""
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
	reason := strings.TrimRight(r.skipped, ".")
	if r.base == "" {
		return "not merged: " + reason
	}
	return fmt.Sprintf("not merged into %s: %s", r.base, reason)
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
	reason := strings.TrimRight(r.skipped, ".")
	if r.base == "" {
		return fmt.Sprintf("## Merge\n\nNot merged: %s.", reason)
	}
	return fmt.Sprintf("## Merge\n\nNot merged into %#q: %s.", r.base, reason)
}
