package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/umputun/ralphex/pkg/progress"
)

// attemptStart describes the attempt a worktree run is about to execute, as AttemptStarted receives
// it. Every path is absolute.
type attemptStart struct {
	PlanFile     string // the worktree's plan copy, which the run ticks
	MainPlanFile string // the plan in the invoking checkout
	Branch       string
	WorktreePath string
	ProgressLog  string
	Resumed      bool // the worktree held a previous attempt
}

// fireAttemptStarted calls req.AttemptStarted once the worktree and its progress log exist and
// before execution starts, and returns keep: whether a run that does not succeed leaves the
// worktree in place. Without the hook, or when it fails, keep is wt.resumed, so a fresh worktree is
// removed as it always was and a resumed one, which holds a previous attempt's work, never is.
// After a successful hook KeepWorktreeOnFailure also keeps a fresh worktree.
func fireAttemptStarted(req executePlanRequest, wt worktreeRun, branch string, baseLog *progress.Logger) (keep bool, err error) {
	if req.AttemptStarted == nil {
		return wt.resumed, nil
	}
	start := attemptStart{
		PlanFile:     absUnder(wt.path, wt.planFile),
		MainPlanFile: absPath(req.PlanFile),
		Branch:       branch,
		WorktreePath: absPath(wt.path),
		ProgressLog:  absPath(baseLog.Path()),
		Resumed:      wt.resumed,
	}
	if hookErr := req.AttemptStarted(start); hookErr != nil {
		return wt.resumed, fmt.Errorf("start attempt: %w", hookErr)
	}
	return wt.resumed || req.KeepWorktreeOnFailure, nil
}

// absUnder resolves a relative path against base. A relative plan path names the same file from the
// repository root and, once the run changes into it, from the worktree root.
func absUnder(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

// absPath makes path absolute against the current directory, keeping it unchanged when that
// cannot be resolved.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// worktreeCleanliness is the part of the worktree's Git service the keep decision reads.
type worktreeCleanliness interface {
	IsDirtyAll() (bool, error)
	OperationInProgress() (string, error)
}

// keepWorktreeDecision returns the end-of-run keep decision for a worktree. keep is what
// fireAttemptStarted returned: without it the worktree is always removed. The decision is taken on
// the first call and memoized, because the finish callback and runWithWorktree's defer both ask and
// the worktree must not be inspected again after the first answer removed it.
func keepWorktreeDecision(keep bool, req executePlanRequest, wtSvc worktreeCleanliness) func(success bool) bool {
	var once sync.Once
	var kept bool
	return func(success bool) bool {
		once.Do(func() {
			kept = keep && keepDetachedWorktree(req, wtSvc, success)
		})
		return kept
	}
}

// keepDetachedWorktree decides whether a worktree fireAttemptStarted marked as kept stays in place.
// A run that did not succeed always keeps it, so the next launch resumes there. A success removes
// it, unless KeepWorktreeOnFailure is set and removal could lose work: the finalize close-out or
// the plan archive stopped short, or the tree is dirty or has a Git operation in progress, since
// removal is git worktree remove --force. A check that fails keeps the worktree too.
func keepDetachedWorktree(req executePlanRequest, wtSvc worktreeCleanliness, success bool) bool {
	if !success {
		return true
	}
	if !req.KeepWorktreeOnFailure {
		return false
	}
	return closeoutIncomplete(req.Outcome) || worktreeUnsettled(wtSvc)
}

// closeoutIncomplete reports a finalize close-out or plan archive that stopped short.
func closeoutIncomplete(o *planExecutionOutcome) bool {
	return o != nil && (o.finalizeIncomplete != nil || o.archiveIncomplete != "")
}

// worktreeUnsettled reports a worktree with uncommitted changes, untracked files included, or an
// unfinished Git operation. An error counts as unsettled: the worktree is kept rather than lost.
func worktreeUnsettled(wtSvc worktreeCleanliness) bool {
	dirty, err := wtSvc.IsDirtyAll()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: keeping worktree, %v\n", err)
		return true
	}
	if dirty {
		return true
	}
	op, err := wtSvc.OperationInProgress()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: keeping worktree, %v\n", err)
		return true
	}
	return op != ""
}

// recordArchive records the plan-archive result: whether the plan reached completed/ and, when the
// archive stopped short, why.
func (o *planExecutionOutcome) recordArchive(moved bool, incomplete error) {
	if o == nil {
		return
	}
	o.planMoved = moved
	o.archiveIncomplete = ""
	if incomplete != nil {
		o.archiveIncomplete = incomplete.Error()
	}
}

// recordFinalize records the finalize close-out, including why it stopped short.
func (o *planExecutionOutcome) recordFinalize(fin finalizeResult) {
	if o == nil {
		return
	}
	o.finalizeMode = fin.mode
	o.finalizeStatusNote = fin.statusNote()
	o.prURL, o.prOpened, o.merged = fin.prURL, fin.prOpened, fin.merged
	o.finalizeIncomplete = fin.incomplete
}

// recordWorktreeKept records the path of a worktree the run left in place.
func (o *planExecutionOutcome) recordWorktreeKept(path string) {
	if o == nil {
		return
	}
	o.worktreeKept = path
}
