package runs

import "fmt"

// ProcessStartToken returns an opaque token identifying the start of process pid. Two tokens
// are only ever compared for equality: a recorded pid whose current token differs from the
// recorded one belongs to another process that reused the pid.
func ProcessStartToken(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("process start token: invalid pid %d", pid)
	}
	return processStartToken(pid)
}
