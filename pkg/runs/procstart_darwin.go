package runs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStartToken renders the kernel's start time of pid (kinfo_proc p_starttime) as
// "<seconds>.<microseconds>".
func processStartToken(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("process start token of pid %d: %w", pid, err)
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", start.Sec, start.Usec), nil
}
