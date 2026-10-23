//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// platformLaunch replaces the launcher with loopai, so T3 Code's provider stays one process.
var platformLaunch = execReplace

// execReplace replaces the current process image with bin, keeping its pid, stdio, and environment.
// It returns only when the exec fails.
func execReplace(bin string, args []string) (int, error) {
	if err := syscall.Exec(bin, append([]string{bin}, args...), os.Environ()); err != nil {
		return 0, fmt.Errorf("exec: %w", err)
	}
	return 0, nil
}
