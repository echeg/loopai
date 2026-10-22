//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// the launcher hands the child two pipe ends as ExtraFiles, which the child sees as fds 3 and 4.
const (
	detachSecretFD = 3 // the secret pipe's read end: one JSON line from the launcher, then EOF
	detachAckFD    = 4 // the ack pipe's write end: the child's one JSON answer
)

// runDetachedChild is the hidden --detached-run entry point: the detached child of run o.DetachedRun,
// started by the launcher in the invoking checkout. It takes over the handshake pipes, reads the
// launcher's secret, and survives hangups before it runs the attempt.
func runDetachedChild(ctx context.Context, o opts) error {
	secretPipe, ackPipe, err := takeHandshakePipes(detachSecretFD, detachAckFD)
	if err != nil {
		return err
	}
	child := &detachedChild{ack: &detachAck{w: ackPipe}}
	child.secret, err = readDetachSecret(secretPipe)
	_ = secretPipe.Close()
	if err != nil {
		return child.refuse(err)
	}

	// a hangup of the terminal or agent that started the run must not end it. Notify rather than
	// Ignore: an ignored disposition is inherited by every process the run starts
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)

	return child.run(ctx, o)
}

// takeHandshakePipes checks the handshake descriptors, marks them close-on-exec, and returns them as
// files. Descriptors passed through ExtraFiles arrive without FD_CLOEXEC, which is also how they are
// told apart from the process's own: nothing can start a process between the check and the marking,
// and once marked no provider or Git process can hold the launcher's pipes open after the child
// closes them. Nothing is wrapped until both pass, so a refused descriptor stays untouched.
func takeHandshakePipes(secretFD, ackFD int) (secret, ack *os.File, err error) {
	for _, fd := range []int{secretFD, ackFD} {
		if err := checkHandshakeFD(fd); err != nil {
			return nil, nil, err
		}
	}
	syscall.CloseOnExec(secretFD)
	syscall.CloseOnExec(ackFD)
	return os.NewFile(uintptr(secretFD), "detach-secret"), os.NewFile(uintptr(ackFD), "detach-ack"), nil
}

// checkHandshakeFD verifies that fd is a pipe this process inherited, as the launcher passes it.
// Everything a Go process opens itself carries FD_CLOEXEC, among it the pipe the runtime creates on
// Darwin for signal delivery, which lands on fds 3 and 4 when no launcher occupied them; reading
// or closing that one would break the process's signal handling.
func checkHandshakeFD(fd int) error {
	const refusal = "--detached-run is started only by loopai --detach: handshake descriptor %d"
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf(refusal+": %w", fd, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFIFO {
		return fmt.Errorf(refusal+" is not a pipe", fd)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf(refusal+": %w", fd, err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		return fmt.Errorf(refusal+" was not inherited", fd)
	}
	return nil
}
