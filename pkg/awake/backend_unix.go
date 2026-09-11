//go:build darwin || linux

package awake

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
)

// platformBackend selects the system inhibitor for this platform. Both inhibitors are bound to
// this process id, so a crash releases the hold without any cleanup on loopai's side. Nil means
// no inhibitor is installed.
func platformBackend() Backend {
	pid := strconv.Itoa(os.Getpid())
	switch runtime.GOOS {
	case "darwin":
		// -i prevents idle sleep, -w exits when the watched process does
		return newCommandBackend("caffeinate", "-i", "-w", pid)
	case "linux":
		// systemd-inhibit holds the lock while its child runs; tail --pid ends with loopai
		if _, err := exec.LookPath("tail"); err != nil {
			return nil
		}
		return newCommandBackend("systemd-inhibit",
			"--what=idle:sleep", "--who=loopai", "--why=plan execution in progress", "--mode=block",
			"tail", "--pid="+pid, "-f", "/dev/null")
	default:
		return nil
	}
}

// commandBackend holds the inhibitor as a child process for the lifetime of the hold.
type commandBackend struct {
	path string
	args []string

	mu  sync.Mutex
	cmd *exec.Cmd
}

// newCommandBackend resolves name on PATH and returns nil when it cannot be executed.
func newCommandBackend(name string, args ...string) *commandBackend {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil
	}
	return &commandBackend{path: path, args: args}
}

// Acquire starts the inhibitor in its own process group so Release can end every process it
// spawned, not only the leader.
func (b *commandBackend) Acquire() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd != nil {
		return nil
	}
	// the hold has no deadline of its own: Release ends it, and the -w/--pid binding ends it on
	// process death, so the command is not tied to a cancellable context
	cmd := exec.CommandContext(context.Background(), b.path, b.args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", b.path, err)
	}
	b.cmd = cmd
	return nil
}

// Release terminates the inhibitor process group and reaps the leader.
func (b *commandBackend) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return
	}
	_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGTERM)
	_ = b.cmd.Wait()
	b.cmd = nil
}

// pid reports the running inhibitor's process id, or zero when nothing is held.
func (b *commandBackend) pid() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return 0
	}
	return b.cmd.Process.Pid
}
