//go:build !windows

package main

import (
	"context"
	"io"

	"github.com/umputun/ralphex/pkg/progress"
)

// runAttachCommand is --attach: it follows a detached run's console log until the run ends and
// returns its exit code; Ctrl+C detaches from the run and leaves it running.
func runAttachCommand(ctx context.Context, o opts, stdout io.Writer) error {
	return attachDetachedRun(ctx, o, stdout)
}

// runRunsCommand is --runs: it lists the checkout's detached runs.
func runRunsCommand(stdout io.Writer) error {
	return listDetachedRuns(stdout)
}

// runStopCommand is --stop: it stops the detached run ident names and waits a while for it to end.
func runStopCommand(ctx context.Context, ident string, colors *progress.Colors, stdout io.Writer) error {
	return stopDetachedRun(ctx, ident, colors, stdout)
}
