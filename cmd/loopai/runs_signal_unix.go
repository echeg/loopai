//go:build !windows

package main

import (
	"fmt"
	"syscall"
)

// signalTerm sends SIGTERM to process pid alone. It never signals a process group: kill(2) reads a
// pid of 0 or below as a group, or as every process the caller may signal, so such a pid is refused.
func signalTerm(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("signal process: invalid pid %d", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM to process %d: %w", pid, err)
	}
	return nil
}
