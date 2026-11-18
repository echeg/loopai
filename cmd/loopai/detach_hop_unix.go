//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// runDetachHop is the hop: it starts argv in a session of its own, handing on the handshake pipes it
// inherited as fds 3 and 4 together with its environment, working directory, stdout, and stderr, and
// returns as soon as the start succeeded, without waiting for the command. It returns the hop's exit
// code: 0 once argv started, 1 when the handshake descriptors are missing or argv could not start, 2
// for an empty argv, each failure described on stderr.
func runDetachHop(argv []string, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "loopai: the detached-run hop has no command to start")
		return 2
	}
	for _, fd := range []int{detachSecretFD, detachAckFD} {
		if err := checkHandshakeFD(fd); err != nil {
			fmt.Fprintln(stderr, "loopai:", err)
			return 1
		}
	}
	// the child outlives the hop, so no context may kill it
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:noctx // a context would kill the detached run
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{os.NewFile(detachSecretFD, "detach-secret"), os.NewFile(detachAckFD, "detach-ack")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "loopai: start the detached child: %v\n", err)
		return 1
	}
	return 0
}
