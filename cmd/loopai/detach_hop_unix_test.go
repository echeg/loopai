//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// detachHopSelector is the argv prefix that makes a re-executed test binary the hop; TestMain sets
// detachHopArgs to it so every launcher test starts its child through the hop.
var detachHopSelector = []string{"-test.run=^TestDetachHopHelper$", "--"}

// TestDetachHopHelper is the hop the launcher tests start, not a test of its own.
func TestDetachHopHelper(t *testing.T) {
	i := slices.Index(os.Args, detachHopArg)
	if i < 0 {
		t.Skip("helper process only")
	}
	os.Exit(runDetachHop(os.Args[i+1:], os.Stderr))
}

// hopRun is one run of the hop helper: its exit code, its stderr, and the launcher's ends of the
// handshake pipes, nil when the hop got none.
type hopRun struct {
	code    int
	stderr  string
	secretW *os.File
	ackR    *os.File
}

// runHop runs the test binary as the hop with argv as the command it starts, handing it fresh
// handshake pipes as fds 3 and 4 when handshake is true, and waits for it.
func runHop(t *testing.T, argv []string, handshake bool) hopRun {
	t.Helper()
	args := append(slices.Clone(detachHopSelector), detachHopArg)
	cmd := exec.Command(os.Args[0], append(args, argv...)...)
	// a file, not a buffer: the command the hop starts inherits stderr and may outlive the hop, and
	// Run waits for a buffer's pipe to close, as the launcher would for anything but its console file
	stderrPath := filepath.Join(t.TempDir(), "stderr")
	stderr, err := os.Create(stderrPath)
	require.NoError(t, err)
	defer stderr.Close()
	cmd.Stderr = stderr
	var res hopRun
	if handshake {
		secretR, secretW, err := os.Pipe()
		require.NoError(t, err)
		ackR, ackW, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() { _ = secretW.Close(); _ = ackR.Close() })
		cmd.ExtraFiles = []*os.File{secretR, ackW}
		defer func() { _ = secretR.Close(); _ = ackW.Close() }()
		res.secretW, res.ackR = secretW, ackR
	}
	runErr := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		require.NoError(t, runErr)
	}
	data, err := os.ReadFile(stderrPath)
	require.NoError(t, err)
	res.stderr = string(data)
	return res
}

func TestRunDetachHopPassesHandshake(t *testing.T) {
	res := runHop(t, []string{"/bin/sh", "-c", `read l <&3; echo "got $l" >&4`}, true)
	require.Equal(t, 0, res.code, res.stderr)

	// the hop has exited: the command it started holds the handshake pipes on its own
	_, err := res.secretW.WriteString("x\n")
	require.NoError(t, err)
	require.NoError(t, res.secretW.Close())
	line, err := bufio.NewReader(res.ackR).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "got x\n", line)
}

func TestRunDetachHopStartFailure(t *testing.T) {
	res := runHop(t, []string{"/nonexistent/loopai"}, true)
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.stderr, "start the detached child:")
}

func TestRunDetachHopRequiresHandshake(t *testing.T) {
	res := runHop(t, []string{"/bin/sh", "-c", "exit 0"}, false)
	assert.Equal(t, 1, res.code)
	assert.Contains(t, res.stderr, "handshake descriptor 3")
}

func TestRunDetachHopRequiresCommand(t *testing.T) {
	res := runHop(t, nil, true)
	assert.Equal(t, 2, res.code)
	assert.Contains(t, res.stderr, "no command to start")
}

func TestDetachHopThroughMainPrintsNothing(t *testing.T) {
	secretR, secretW, err := os.Pipe()
	require.NoError(t, err)
	defer secretW.Close()
	ackR, ackW, err := os.Pipe()
	require.NoError(t, err)
	defer ackR.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachLaunchMainHelper$")
	cmd.Env = append(os.Environ(), detachLaunchMainEnv+"="+detachHopArg+"\x1f/bin/sh\x1f-c\x1fexit 0")
	cmd.ExtraFiles = []*os.File{secretR, ackW}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	_ = secretR.Close()
	_ = ackW.Close()
	require.NoError(t, runErr, "stderr: %s", stderr.String())
	assert.Empty(t, stdout.String(), "the hop's stdout is the console log: no version banner")
}
