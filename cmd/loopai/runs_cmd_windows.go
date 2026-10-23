//go:build windows

package main

import (
	"context"
	"io"

	"github.com/umputun/ralphex/pkg/progress"
)

// runAttachCommand refuses: detached runs are not supported on Windows.
func runAttachCommand(context.Context, opts, io.Writer) error {
	return errDetachUnsupported
}

// runRunsCommand refuses: detached runs are not supported on Windows.
func runRunsCommand(string, io.Writer) error {
	return errDetachUnsupported
}

// runStopCommand refuses: detached runs are not supported on Windows.
func runStopCommand(context.Context, string, string, *progress.Colors, io.Writer) error {
	return errDetachUnsupported
}
