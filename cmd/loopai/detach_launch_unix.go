//go:build !windows

package main

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"

	"github.com/umputun/ralphex/pkg/config"
)

// runDetachCommand is the --detach launcher: it decides the plan's run, starts its detached child, and
// returns once the child answers.
func runDetachCommand(ctx context.Context, o opts, cfg *config.Config, dio detachIO) error {
	return launchDetached(ctx, o, cfg, dio)
}

// startDetachedProcess starts cmd in a session of its own, so neither the terminal nor the agent that
// ran the launcher can signal it or take it down with their process group.
func startDetachedProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the detached child: %w", err)
	}
	return nil
}
