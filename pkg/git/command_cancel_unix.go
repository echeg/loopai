//go:build !windows

package git

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// configureCommandCancellation gives Git and any credential/helper children their own process
// group. Context cancellation kills the complete group instead of orphaning descendants.
func configureCommandCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("kill Git process group: %w", err)
	}
	cmd.WaitDelay = 250 * time.Millisecond
}
