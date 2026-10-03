package phase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/status"
)

// finalizeRemote is the remote whose base branch finalize merges into the plan branch.
const finalizeRemote = "origin"

// finalizeRestoreTimeout bounds the rollback that runs after a rejected or canceled sync. It
// detaches from the run's context, because a canceled run must not leave a half-done merge in
// the checkout it resumes from.
const finalizeRestoreTimeout = time.Minute

// FinalizeStatus classifies the result of the finalize base sync.
type FinalizeStatus string

// finalize statuses reported by FinalizePhase.Run.
const (
	FinalizeSkipped  FinalizeStatus = "skipped"    // finalize is off, or not the last plan of a chain
	FinalizeUpToDate FinalizeStatus = "up_to_date" // the branch already contained the base; validation passed
	FinalizeMerged   FinalizeStatus = "merged"     // the base merged cleanly; validation passed
	FinalizeResolved FinalizeStatus = "resolved"   // the review provider resolved conflicts; validation passed
	FinalizeBlocked  FinalizeStatus = "blocked"    // the sync was rejected and the pre-merge HEAD restored
)

// FinalizeOutcome describes the finalize base sync. Files lists the paths the merge left
// conflicted, for both a resolved and a blocked conflicting merge.
type FinalizeOutcome struct {
	Status  FinalizeStatus `json:"status"`
	Reason  string         `json:"reason,omitempty"`
	Files   []string       `json:"files,omitempty"`
	Base    string         `json:"base,omitempty"`     // remote-tracking base, such as origin/master
	BaseSHA string         `json:"base_sha,omitempty"` // fetched base commit
	// Unrestored marks a blocked sync whose rollback failed: the checkout may still hold the merge
	// or sit on another branch, so nothing may commit to it until the user repairs it.
	Unrestored bool `json:"unrestored,omitempty"`
}

// Summary renders the outcome as one line for progress logs and status surfaces.
func (o FinalizeOutcome) Summary() string {
	switch o.Status {
	case FinalizeUpToDate:
		return "up to date with " + o.Base
	case FinalizeMerged:
		return "merged " + o.Base
	case FinalizeResolved:
		return fmt.Sprintf("merged %s (%d %s resolved)", o.Base, len(o.Files), plural(len(o.Files), "file", "files"))
	case FinalizeBlocked:
		return "blocked: " + o.Reason
	default:
		return "skipped"
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// FinalizeGit performs the repository operations of the finalize base sync; *git.Service implements it.
type FinalizeGit interface {
	HeadHash() (string, error)
	CurrentBranch() (string, error)
	IsDirty() (bool, error)
	OperationInProgress() (string, error)
	FetchContext(ctx context.Context, remote, branch string) (string, error)
	MergeRemoteNoCommitContext(ctx context.Context, rev string) (git.MergeResult, error)
	StageZeroSnapshot() (git.MergeSnapshot, error)
	CommitMergeContext(ctx context.Context, message string) error
	CommitParents(commit string) ([]string, error)
	ChangedOutside(snapshot git.MergeSnapshot, allowed []string, commit string) ([]string, error)
	MergeAbortContext(ctx context.Context, snapshot git.MergeSnapshot) error
	AbortCleanMergeContext(ctx context.Context) error
	RestoreHeadContext(ctx context.Context, sha string) error
}

// FinalizePhase merges the remote base into the plan branch and validates the result.
type FinalizePhase struct {
	cfg         Config
	log         FinalizeLogger
	exec        Executor
	policy      Policy
	prompts     FinalizePrompts
	deps        *Deps
	phaseHolder *status.PhaseHolder
}

// FinalizePhaseOpts contains dependencies for FinalizePhase.
type FinalizePhaseOpts struct {
	Cfg         Config
	Log         FinalizeLogger
	Exec        Executor
	Policy      Policy
	Prompts     FinalizePrompts
	Deps        *Deps
	PhaseHolder *status.PhaseHolder
}

// NewFinalizePhase creates a finalize phase engine.
func NewFinalizePhase(opts FinalizePhaseOpts) *FinalizePhase {
	return &FinalizePhase{
		cfg: opts.Cfg, log: opts.Log, exec: opts.Exec, policy: opts.Policy,
		prompts: opts.Prompts, deps: opts.Deps, phaseHolder: opts.PhaseHolder,
	}
}

// finalizeSync is the state of one base sync: the HEAD to restore and the merge it started.
type finalizeSync struct {
	git      FinalizeGit
	base     string // remote-tracking ref, such as origin/master
	branch   string // plan branch the sync started on; empty for a detached HEAD
	preHead  string
	merge    git.MergeResult
	snapshot git.MergeSnapshot // zero when the branch was already up to date
	snapped  bool              // snapshot was taken; without it only a just-started merge can be aborted
}

// Run merges origin/<base> into the plan branch and has the review provider resolve clear-cut
// conflicts and run the plan's validation commands. Every failure becomes a blocked outcome with
// the pre-merge HEAD restored, so finalize never fails the run; only context cancellation is
// returned as an error, after the same restore.
func (p *FinalizePhase) Run(ctx context.Context) (FinalizeOutcome, error) {
	if !p.cfg.FinalizeEnabled {
		return FinalizeOutcome{Status: FinalizeSkipped}, nil
	}

	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhaseFinalize)
	}
	p.log.PrintSection(status.NewGenericSection("finalize step"))

	branch := strings.TrimPrefix(p.cfg.FinalizeBase, finalizeRemote+"/")
	outcome, err := p.sync(ctx, branch)
	outcome.Base = finalizeRemote + "/" + branch
	if err != nil {
		return outcome, err
	}
	if outcome.Status == FinalizeBlocked {
		p.log.Print("finalize %s", outcome.Summary())
	} else {
		p.log.Print("finalize: %s", outcome.Summary())
	}
	return outcome, nil
}

func (p *FinalizePhase) sync(ctx context.Context, branch string) (FinalizeOutcome, error) {
	var g FinalizeGit
	if p.deps != nil {
		g = p.deps.FinalizeGit
	}
	if g == nil {
		return blocked("no git repository available"), nil
	}
	if branch == "" {
		return blocked("base branch is unknown"), nil
	}
	s := &finalizeSync{git: g, base: finalizeRemote + "/" + branch}

	dirty, err := g.IsDirty()
	if err != nil {
		return blocked("check working tree: %v", err), nil
	}
	if dirty {
		return blocked("working tree has uncommitted changes"), nil
	}
	current, err := g.CurrentBranch()
	if err != nil {
		return blocked("read current branch: %v", err), nil
	}
	if current == branch {
		return blocked("the checkout is on the base branch %s", branch), nil
	}
	s.branch = current
	if s.preHead, err = g.HeadHash(); err != nil {
		return blocked("read HEAD: %v", err), nil
	}
	if _, err = g.FetchContext(ctx, finalizeRemote, branch); err != nil {
		if isContextErr(err) {
			return FinalizeOutcome{}, fmt.Errorf("finalize fetch: %w", err)
		}
		return blocked("fetch %s: %v", s.base, err), nil
	}
	// a failed merge is already aborted by the git layer, so nothing is left to restore. The full
	// ref keeps a local branch or tag named origin/<base> from shadowing the fetched commit.
	if s.merge, err = g.MergeRemoteNoCommitContext(ctx, "refs/remotes/"+s.base); err != nil {
		if isContextErr(err) {
			return FinalizeOutcome{}, fmt.Errorf("finalize merge: %w", err)
		}
		return blocked("merge %s: %v", s.base, err), nil
	}

	outcome, err := p.syncMerge(ctx, s)
	outcome.BaseSHA = s.merge.Target
	return outcome, err
}

// syncMerge runs the session for the merge state and accepts or rolls back its result.
func (p *FinalizePhase) syncMerge(ctx context.Context, s *finalizeSync) (FinalizeOutcome, error) {
	outcome := FinalizeOutcome{Status: FinalizeUpToDate}
	if s.merge.State != git.MergeUpToDate {
		snapshot, err := s.git.StageZeroSnapshot()
		if err != nil {
			return p.reject(ctx, s, FinalizeOutcome{}, fmt.Sprintf("snapshot merge index: %v", err)), nil
		}
		s.snapshot, s.snapped = snapshot, true
	}
	switch s.merge.State {
	case git.MergeUpToDate:
	case git.MergeClean:
		outcome.Status = FinalizeMerged
		if err := s.git.CommitMergeContext(ctx, "Merge remote-tracking branch '"+s.base+"'"); err != nil {
			if isContextErr(err) {
				return p.cancel(ctx, s, err)
			}
			return p.reject(ctx, s, outcome, fmt.Sprintf("commit merge: %v", err)), nil
		}
	case git.MergeConflicted:
		outcome.Status = FinalizeResolved
		outcome.Files = slices.Clone(s.merge.Conflicts)
	default:
		return p.reject(ctx, s, outcome, fmt.Sprintf("unexpected merge state %q", s.merge.State)), nil
	}

	reason, err := p.runSession(ctx, s.merge.Conflicts)
	if err != nil {
		return p.cancel(ctx, s, err)
	}
	if reason == "" {
		reason = p.verify(s)
	}
	if reason != "" {
		return p.reject(ctx, s, outcome, reason), nil
	}
	return outcome, nil
}

// runSession runs the finalize prompt and returns a non-empty reason unless the session signaled
// FINALIZE_DONE. Only parent context cancellation is returned as an error.
func (p *FinalizePhase) runSession(ctx context.Context, conflicts []string) (string, error) {
	execName := p.cfg.reviewExecutorName()
	execResult := p.policy.Run(ctx, p.exec.Run, p.prompts.FinalizePrompt(conflicts), execName)
	result := execResult.Result

	if execResult.TimedOut {
		return "finalize session timed out", nil
	}
	if result.Error != nil {
		if isContextErr(result.Error) {
			return "", fmt.Errorf("finalize session: %w", result.Error)
		}
		if patternErr := p.policy.HandlePatternMatchError(result.Error, execName); patternErr != nil {
			return fmt.Sprintf("%s session stopped: %v", execName, patternErr), nil
		}
		return fmt.Sprintf("%s session failed: %v", execName, result.Error), nil
	}
	// a later text block can overwrite an earlier signal, so a blocked signal anywhere in the
	// output wins over a done signal
	if IsFinalizeBlocked(result.Signal) || strings.Contains(result.Output, SignalFinalizeBlocked) {
		if reason := ParseFinalizeBlockedReason(result.Output); reason != "" {
			return reason, nil
		}
		return "session reported FINALIZE_BLOCKED without a reason", nil
	}
	if !IsFinalizeDone(result.Signal) {
		return "session ended without FINALIZE_DONE", nil
	}
	return "", nil
}

// verify checks the repository after a session that signaled FINALIZE_DONE and returns the reason
// the result is unacceptable, or an empty string. An up-to-date branch must be unchanged; a merge
// must be committed as exactly one commit whose parents are the pre-merge HEAD and the fetched base,
// and whose tree matches the merge's own result everywhere outside the conflicted paths.
func (p *FinalizePhase) verify(s *finalizeSync) string {
	op, err := s.git.OperationInProgress()
	if err != nil {
		return fmt.Sprintf("inspect repository state: %v", err)
	}
	if op == "merge" {
		return "merge left uncommitted"
	}
	if op != "" {
		return fmt.Sprintf("a %s is in progress", op)
	}
	dirty, err := s.git.IsDirty()
	if err != nil {
		return fmt.Sprintf("check working tree: %v", err)
	}
	if dirty {
		return "working tree has uncommitted changes after the session"
	}
	if reason := s.checkoutMoved(); reason != "" {
		return reason
	}
	head, err := s.git.HeadHash()
	if err != nil {
		return fmt.Sprintf("read HEAD: %v", err)
	}
	if s.merge.State == git.MergeUpToDate {
		if head != s.preHead {
			return "session committed changes to an up-to-date branch"
		}
		return ""
	}
	parents, err := s.git.CommitParents(head)
	if err != nil {
		return fmt.Sprintf("read merge commit: %v", err)
	}
	if !slices.Equal(parents, []string{s.preHead, s.merge.Target}) {
		return fmt.Sprintf("HEAD is not a single merge commit of %s into the plan branch", s.base)
	}
	changed, err := s.git.ChangedOutside(s.snapshot, s.merge.Conflicts, head)
	if err != nil {
		return fmt.Sprintf("compare merge result: %v", err)
	}
	if len(changed) > 0 {
		return "changed paths outside the conflicted set: " + strings.Join(changed, ", ")
	}
	return ""
}

// reject restores the pre-merge state and returns a blocked outcome carrying reason. A failed
// restore is appended to the reason and logged, since the branch then still holds the merge.
func (p *FinalizePhase) reject(ctx context.Context, s *finalizeSync, outcome FinalizeOutcome, reason string) FinalizeOutcome {
	outcome.Status = FinalizeBlocked
	outcome.Reason = reason
	if err := p.restore(ctx, s); err != nil {
		p.log.Print("warning: finalize could not restore %s: %v", s.preHead, err)
		outcome.Reason = fmt.Sprintf("%s; restore failed: %v", reason, err)
		outcome.Unrestored = true
		return outcome
	}
	// restore keeps local changes, so edits the session left uncommitted are still in the tree
	if dirty, err := s.git.IsDirty(); err == nil && dirty {
		outcome.Reason = reason + "; the session's uncommitted changes were left in the working tree"
	}
	return outcome
}

// cancel restores the pre-merge state after context cancellation and returns cause.
func (p *FinalizePhase) cancel(ctx context.Context, s *finalizeSync, cause error) (FinalizeOutcome, error) {
	if err := p.restore(ctx, s); err != nil {
		p.log.Print("warning: finalize could not restore %s: %v", s.preHead, err)
	}
	return FinalizeOutcome{}, fmt.Errorf("finalize: %w", cause)
}

// restore aborts an uncommitted merge and moves the branch back to the pre-merge HEAD. Both steps
// refuse to discard changes the merge did not produce.
func (p *FinalizePhase) restore(ctx context.Context, s *finalizeSync) error {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeRestoreTimeout)
	defer cancel()

	op, err := s.git.OperationInProgress()
	if err != nil {
		return fmt.Errorf("inspect repository state: %w", err)
	}
	switch {
	case op == "merge" && s.merge.State != git.MergeUpToDate && !s.snapped:
		// the snapshot failed right after the merge started on a clean tree, before any session ran
		if abortErr := s.git.AbortCleanMergeContext(restoreCtx); abortErr != nil {
			return fmt.Errorf("abort merge: %w", abortErr)
		}
	case op == "merge" && s.merge.State != git.MergeUpToDate:
		if abortErr := s.git.MergeAbortContext(restoreCtx, s.snapshot); abortErr != nil {
			return fmt.Errorf("abort merge: %w", abortErr)
		}
	case op != "":
		return fmt.Errorf("a %s is in progress", op)
	}
	// resetting another branch would move it and still leave the plan branch as the session left it
	if reason := s.checkoutMoved(); reason != "" {
		return errors.New(reason)
	}
	head, err := s.git.HeadHash()
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	if head == s.preHead {
		return nil
	}
	if err := s.git.RestoreHeadContext(restoreCtx, s.preHead); err != nil {
		return fmt.Errorf("restore HEAD: %w", err)
	}
	return nil
}

// checkoutMoved returns a reason when the checkout is no longer on the plan branch the sync started
// on. A merge commit with the right parents on another branch would otherwise pass verify, and the
// pull request would then be opened from that branch.
func (s *finalizeSync) checkoutMoved() string {
	current, err := s.git.CurrentBranch()
	if err != nil {
		return fmt.Sprintf("read current branch: %v", err)
	}
	if current == s.branch {
		return ""
	}
	return fmt.Sprintf("the checkout moved from %s to %s", branchLabel(s.branch), branchLabel(current))
}

func branchLabel(branch string) string {
	if branch == "" {
		return "a detached HEAD"
	}
	return branch
}

func blocked(format string, args ...any) FinalizeOutcome {
	return FinalizeOutcome{Status: FinalizeBlocked, Reason: fmt.Sprintf(format, args...)}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
