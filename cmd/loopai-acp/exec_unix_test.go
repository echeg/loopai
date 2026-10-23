//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// execHelperEnv makes TestExecReplaceHelper exec the test binary through execReplace. It holds the
// file the helper records its pid in before the exec and, after a unit separator, the args file the
// exec'd fake loopai records its arguments in.
const execHelperEnv = "LOOPAI_ACP_TEST_EXEC"

// TestExecReplaceHelper is the process TestExecReplace starts, not a test of its own: it records its
// pid and execs the test binary as loopai with the arguments run passes. helperEnv is set only now, so
// TestMain's helper branch runs in the exec'd image and not in this one.
func TestExecReplaceHelper(t *testing.T) {
	raw, ok := os.LookupEnv(execHelperEnv)
	if !ok {
		t.Skip("helper process only")
	}
	pidFile, argsFile, _ := strings.Cut(raw, "\x1f")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(5)
	}
	self, err := os.Executable()
	if err != nil {
		os.Exit(6)
	}
	if setErr := os.Setenv(helperEnv, argsFile); setErr != nil {
		os.Exit(7)
	}
	_, err = execReplace(self, []string{"--acp"})
	t.Fatalf("execReplace returned: %v", err)
}

// TestExecReplace runs execReplace in a child process and checks that the process image is replaced
// in place: the same pid runs loopai with exactly "--acp" and the inherited environment, and the
// replaced program's exit code is the process's.
func TestExecReplace(t *testing.T) {
	dir := t.TempDir()
	beforeExec, afterExec, argsFile := filepath.Join(dir, "pid-before"), filepath.Join(dir, "pid-after"),
		filepath.Join(dir, "args")
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecReplaceHelper$")
	cmd.Env = append(os.Environ(), execHelperEnv+"="+beforeExec+"\x1f"+argsFile, helperExitEnv+"=3",
		helperPIDEnv+"="+afterExec, helperMarkerEnv+"=inherited-1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "stderr: %s", stderr.String())
	assert.Equal(t, 3, exitErr.ExitCode(), "the exec'd program's exit code ends the process")

	args, err := os.ReadFile(argsFile) //nolint:gosec // test temp file
	require.NoError(t, err)
	assert.Equal(t, "--acp", string(args), "loopai gets exactly --acp")
	before, err := os.ReadFile(beforeExec) //nolint:gosec // test temp file
	require.NoError(t, err)
	after, err := os.ReadFile(afterExec) //nolint:gosec // test temp file
	require.NoError(t, err)
	pid, marker, _ := strings.Cut(string(after), "\n")
	assert.Equal(t, string(before), pid, "exec keeps the pid: the provider stays one process")
	assert.Equal(t, strconv.Itoa(cmd.Process.Pid), pid)
	assert.Equal(t, "inherited-1", marker, "the environment is inherited")
}

func TestExecReplaceFailure(t *testing.T) {
	code, err := execReplace(filepath.Join(t.TempDir(), "missing-loopai"), []string{"--acp"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec")
	assert.Zero(t, code)
}

func TestDefaultEnvExecsOnUnix(t *testing.T) {
	assert.Equal(t, reflect.ValueOf(execReplace).Pointer(), reflect.ValueOf(defaultEnv().launch).Pointer(),
		"the production launch replaces the process")
}
