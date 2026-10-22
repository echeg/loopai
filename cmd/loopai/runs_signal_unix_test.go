//go:build !windows

package main

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalTerm(t *testing.T) {
	// a shell leading its own process group, with a background child in that group
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	child, err := strconv.Atoi(strings.TrimSpace(line))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	require.NoError(t, signalTerm(cmd.Process.Pid))
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err = <-waited:
	case <-time.After(runsWait):
		t.Fatal("the signaled process did not exit")
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	assert.Equal(t, syscall.SIGTERM, ws.Signal(), "the process ends on SIGTERM")
	assert.NoError(t, syscall.Kill(child, 0), "its process group was not signaled: the background child still runs")
}

func TestSignalTermRefusesGroupPids(t *testing.T) {
	for _, pid := range []int{0, -1} {
		require.ErrorContains(t, signalTerm(pid), "invalid pid "+strconv.Itoa(pid))
	}
}
