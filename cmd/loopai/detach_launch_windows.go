//go:build windows

package main

import (
	"context"
	"os/exec"

	"github.com/umputun/ralphex/pkg/config"
)

// runDetachCommand refuses: detached runs are not supported on Windows.
func runDetachCommand(context.Context, opts, *config.Config, detachIO) error {
	return errDetachUnsupported
}

// startDetachedProcess refuses: detached runs are not supported on Windows.
func startDetachedProcess(*exec.Cmd) error {
	return errDetachUnsupported
}
