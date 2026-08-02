//go:build windows

package git

import (
	"os/exec"
	"time"
)

// Windows CommandContext kills the direct Git process. WaitDelay also bounds inherited pipe
// handles from helpers because native process-group termination is not available through os/exec.
func configureCommandCancellation(cmd *exec.Cmd) {
	cmd.WaitDelay = 250 * time.Millisecond
}
