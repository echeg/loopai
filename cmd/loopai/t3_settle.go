package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/t3"
)

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

func reportT3Settlement(stderr io.Writer, settled int, err error) {
	if settled > 0 {
		fmt.Fprintf(stderr, "settled %d T3 Code thread(s) after merge\n", settled)
	}
	// ordinary CLI users do not need T3 installed; cancellation means shutdown.
	if err != nil && !errors.Is(err, t3.ErrNoRuntime) &&
		!errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "warning: t3 thread settlement incomplete: %v\n", err)
	}
}
