package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/t3"
)

// settlement also runs in ACP, where phase-title reporting is deliberately disabled.
// keep the factory separate so tests cannot issue credentials or reach a developer's server.
var newT3SettleDispatcher = func(ctx context.Context, getenv func(string) string) (t3.Dispatcher, error) {
	ep, err := t3.ResolveSettleEndpoint(ctx, getenv)
	if err != nil {
		return nil, err //nolint:wrapcheck // t3 errors already carry operation context
	}
	return t3.NewClient(ep), nil
}

// t3MergeRoots must run before --merge removes the feature worktree: T3 may have registered
// that checkout itself as a project, and gitSvc may point at the directory being removed.
func t3MergeRoots(gitSvc *git.Service) []string {
	roots, err := progressRecordRoots(gitSvc)
	if err != nil {
		return []string{gitSvc.Root()}
	}
	return roots
}

func settleT3Threads(ctx context.Context, roots []string, branch string, stderr io.Writer) {
	api, err := newT3SettleDispatcher(ctx, os.Getenv)
	if err != nil {
		reportT3Settlement(stderr, 0, err)
		return
	}
	settled, err := t3.SettleMergedBranch(ctx, api, roots, branch)
	reportT3Settlement(stderr, settled, err)
}

// prepareT3MergedBranchSettlement captures the threads before the ACP reply. The callback runs
// after the reply and is canceled and joined before the server accepts another prompt's work.
// T3 still needs a short interval to consume the reply and mark its provider session idle.
func prepareT3MergedBranchSettlement(ctx context.Context, gitSvc *git.Service, branch, base string,
	stderr io.Writer) func(context.Context) {
	head, err := gitSvc.BranchHashContext(ctx, branch)
	if err != nil {
		reportT3Settlement(stderr, 0, fmt.Errorf("read merged branch: %w", err))
		return nil
	}
	api, err := newT3SettleDispatcher(ctx, os.Getenv)
	if err != nil {
		reportT3Settlement(stderr, 0, err)
		return nil
	}
	guarded := t3MergedDispatcher{Dispatcher: api, gitSvc: gitSvc, branch: branch, base: base, head: head}
	settlement, err := t3.PrepareBranchSettlement(ctx, guarded, t3MergeRoots(gitSvc), branch)
	if err != nil {
		reportT3Settlement(stderr, 0, err)
		return nil
	}
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		settled, settleErr := settlement.Settle(ctx)
		reportT3Settlement(stderr, settled, settleErr)
	}
}

// another process can edit the branch while T3 consumes the reply. Check the Git evidence at
// dispatch time, after the idle wait, so new unmerged commits never inherit the previous settlement.
type t3MergedDispatcher struct {
	t3.Dispatcher
	gitSvc             *git.Service
	branch, base, head string
}

func (d t3MergedDispatcher) Dispatch(ctx context.Context, cmd t3.Command) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err //nolint:wrapcheck // preserve cancellation for the best-effort reporter
	}
	current, err := d.gitSvc.BranchHashContext(ctx, d.branch)
	if err != nil {
		return 0, fmt.Errorf("recheck merged branch: %w", err)
	}
	if current != d.head {
		return 0, fmt.Errorf("branch %q changed after merge", d.branch)
	}
	contained, err := d.gitSvc.BranchContainsRevisionContext(ctx, d.base, d.head)
	if err != nil {
		return 0, fmt.Errorf("recheck merge into %q: %w", d.base, err)
	}
	if !contained {
		return 0, fmt.Errorf("branch %q is no longer merged into %q", d.branch, d.base)
	}
	return d.Dispatcher.Dispatch(ctx, cmd) //nolint:wrapcheck // t3 errors already identify the command
}

func reportT3Settlement(stderr io.Writer, settled int, err error) {
	if settled > 0 {
		fmt.Fprintf(stderr, "settled %d T3 Code thread(s) after merge\n", settled)
	}
	// ordinary CLI users do not need T3 installed; cancellation means a new ACP turn or shutdown.
	if err != nil && !errors.Is(err, t3.ErrNoRuntime) &&
		!errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "warning: t3 thread settlement incomplete: %v\n", err)
	}
}
