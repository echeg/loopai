package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/runs"
)

// refuseLiveDetachedRun refuses a close-out of feature while a run of it is live, since merging or
// pushing the branch would race the run's commits. It checks two things independently:
//  1. the runs directory of every checkout progressRecordRoots returns, because a detached run records
//     in the checkout it was launched from, which can be any registered worktree;
//  2. the run lock of the worktree that has feature checked out, probed without blocking. It also
//     catches a foreground --worktree run and a run whose runs directory the first check cannot see.
//
// A platform without detached runs has no runs to find in the first check.
func refuseLiveDetachedRun(ctx context.Context, gitSvc *git.Service, feature string) error {
	roots, err := progressRecordRoots(gitSvc)
	if err != nil {
		return err
	}
	for _, root := range roots {
		run, live, liveErr := runs.LiveRunForBranch(root, feature)
		if errors.Is(liveErr, runs.ErrUnsupported) {
			continue
		}
		if liveErr != nil {
			return fmt.Errorf("check for a live detached run of %s: %w", feature, liveErr)
		}
		if live {
			id := run.Descriptor.ID
			return fmt.Errorf("a detached run of %s is live: %s; stop it with loopai --stop %s", feature, id, id)
		}
	}
	return refuseBusyFeatureWorktree(ctx, gitSvc, feature)
}

// refuseBusyFeatureWorktree refuses when another process holds the run lock of the registered
// worktree that has feature checked out. A lock this probe acquires is released at once. A worktree
// whose directory was deleted holds no run, and the close-out reports it itself.
func refuseBusyFeatureWorktree(ctx context.Context, gitSvc *git.Service, feature string) error {
	worktrees, err := gitSvc.Worktrees()
	if err != nil {
		return fmt.Errorf("check the worktree of %s: %w", feature, err)
	}
	i := slices.IndexFunc(worktrees, func(wt git.Worktree) bool { return wt.Branch == feature })
	if i < 0 {
		return nil
	}
	path := worktrees[i].Path
	if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
		return nil
	}
	wtSvc, err := gitSvc.OpenWorktree(path)
	if err != nil {
		return fmt.Errorf("check the worktree of %s: %w", feature, err)
	}
	release, err := wtSvc.AcquireWorktreeRunLockContext(ctx)
	var busy *git.ErrWorktreeBusy
	if errors.As(err, &busy) {
		return fmt.Errorf("a loopai run of %s is live: %w; stop it first (loopai --stop <run-id> for a detached run)",
			feature, busy)
	}
	if err != nil {
		return fmt.Errorf("check the worktree of %s: %w", feature, err)
	}
	if releaseErr := release(); releaseErr != nil {
		return fmt.Errorf("check the worktree of %s: %w", feature, releaseErr)
	}
	return nil
}
