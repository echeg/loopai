package main

import (
	"bytes"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helperEnv makes the test binary act as a fake loopai: it records its argv to the named file and
// exits with the code in helperExitEnv. Two non-numeric values change what it does after recording:
// helperAwaitTerm waits for SIGTERM and exits with helperTermCode, and helperSelfKill kills itself.
const (
	helperEnv       = "LOOPAI_ACP_TEST_HELPER_ARGS"
	helperExitEnv   = "LOOPAI_ACP_TEST_HELPER_EXIT"
	helperAwaitTerm = "await-term"
	helperSelfKill  = "self-kill"
	helperTermCode  = 42
)

func TestMain(m *testing.M) {
	if out := os.Getenv(helperEnv); out != "" {
		os.Exit(runHelper(out, os.Getenv(helperExitEnv)))
	}
	os.Exit(m.Run())
}

func runHelper(out, mode string) int {
	switch mode {
	case helperAwaitTerm:
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		// the args file doubles as the ready marker, so it is written once the handler is in place
		_ = os.WriteFile(out, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
		select {
		case <-sigs:
			return helperTermCode
		case <-time.After(30 * time.Second):
			return 0
		}
	case helperSelfKill:
		_ = os.WriteFile(out, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Kill()
		}
		time.Sleep(30 * time.Second)
		return 0
	}
	_ = os.WriteFile(out, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	code, _ := strconv.Atoi(mode)
	return code
}

type launchCall struct {
	path string
	args []string
}

// testEnv returns an env with buffers, no LOOPAI_ACP_LOOPAI, no sibling, no PATH entry, and a
// recording launcher returning code.
func testEnv(code int) (e env, stdout, stderr *bytes.Buffer, calls *[]launchCall) {
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	calls = &[]launchCall{}
	e = env{
		stdout:     stdout,
		stderr:     stderr,
		getenv:     func(string) string { return "" },
		executable: func() (string, error) { return filepath.Join("launcher", "dir", "loopai-acp"), nil },
		lookPath:   func(string) (string, error) { return filepath.Join("path", "loopai"), nil },
		stat:       func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		launch: func(path string, args []string) (int, error) {
			*calls = append(*calls, launchCall{path: path, args: args})
			return code, nil
		},
	}
	return e, stdout, stderr, calls
}

func TestRunProbeCommands(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"--version"}, wantCode: 0, wantStdout: "loopai-acp "},
		{name: "version short alias", args: []string{"-v"}, wantCode: 0, wantStdout: "loopai-acp "},
		{name: "models", args: []string{"models"}, wantCode: 0, wantStdout: "* loopai (default)"},
		{name: "inspect", args: []string{"inspect", "--json"}, wantCode: 1, wantStderr: "inspect is not supported"},
		{name: "update", args: []string{"update"}, wantCode: 1, wantStderr: "update is not supported"},
		{name: "no args", args: nil, wantCode: exitUsage, wantStderr: "usage: loopai-acp"},
		{name: "unknown", args: []string{"chat"}, wantCode: exitUsage, wantStderr: "usage: loopai-acp"},
		{name: "inspect without json", args: []string{"inspect"}, wantCode: exitUsage, wantStderr: "usage: loopai-acp"},
		{name: "stdio without agent", args: []string{"stdio"}, wantCode: exitUsage, wantStderr: "usage: loopai-acp"},
		{name: "agent without stdio", args: []string{"agent"}, wantCode: exitUsage, wantStderr: "usage: loopai-acp"},
		{name: "stdio with trailing arg", args: []string{"agent", "stdio", "extra"}, wantCode: exitUsage, wantStderr: "usage: loopai-acp"},
		{name: "permission mode without value", args: []string{"--permission-mode"}, wantCode: exitUsage, wantStderr: "usage: loopai-acp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, stdout, stderr, calls := testEnv(0)
			assert.Equal(t, tc.wantCode, run(tc.args, e))
			if tc.wantStdout == "" {
				assert.Empty(t, stdout.String())
			} else {
				assert.Contains(t, stdout.String(), tc.wantStdout)
			}
			if tc.wantStderr == "" {
				assert.Empty(t, stderr.String())
			} else {
				assert.Contains(t, stderr.String(), tc.wantStderr)
			}
			assert.Empty(t, *calls, "probe commands must not start loopai")
		})
	}
}

func TestRunModelsHasNoLoginVerdict(t *testing.T) {
	e, stdout, _, _ := testEnv(0)
	require.Equal(t, 0, run([]string{"models"}, e))
	assert.NotContains(t, strings.ToLower(stdout.String()), "logged in")
	assert.NotContains(t, strings.ToLower(stdout.String()), "not authenticated")
}

func TestRunVersionUsesRevision(t *testing.T) {
	prev := revision
	revision = "v1.2.3"
	t.Cleanup(func() { revision = prev })
	e, stdout, _, _ := testEnv(0)
	require.Equal(t, 0, run([]string{"--version"}, e))
	assert.Equal(t, "loopai-acp v1.2.3\n", stdout.String())
}

func TestRunStdioSessionShapes(t *testing.T) {
	shapes := [][]string{
		{"agent", "stdio"},
		{"agent", "--always-approve", "stdio"},
		{"--permission-mode", "default", "agent", "stdio"},
		{"--permission-mode", "acceptEdits", "agent", "stdio"},
		{"--permission-mode", "auto", "agent", "stdio"},
		{"--permission-mode=auto", "agent", "stdio"},
	}
	for _, args := range shapes {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			e, stdout, stderr, calls := testEnv(7)
			assert.Equal(t, 7, run(args, e), "exit code is forwarded")
			require.Len(t, *calls, 1)
			assert.Equal(t, filepath.Join("path", "loopai"), (*calls)[0].path)
			assert.Equal(t, []string{"--acp"}, (*calls)[0].args)
			assert.Empty(t, stdout.String(), "stdout belongs to the protocol")
			assert.Empty(t, stderr.String())
		})
	}
}

func TestRunStdioLaunchError(t *testing.T) {
	e, stdout, stderr, _ := testEnv(0)
	e.launch = func(string, []string) (int, error) { return 0, errors.New("boom") }
	assert.Equal(t, 1, run([]string{"agent", "stdio"}, e))
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "boom")
}

func TestRunStdioMissingLoopai(t *testing.T) {
	e, stdout, stderr, calls := testEnv(0)
	e.lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }
	assert.Equal(t, 1, run([]string{"agent", "stdio"}, e))
	assert.Empty(t, *calls)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), loopaiEnv)
	assert.Contains(t, stderr.String(), "not on PATH")
}

func TestResolveLoopaiOrder(t *testing.T) {
	siblingName := "loopai"
	if runtime.GOOS == "windows" {
		siblingName += ".exe"
	}
	dir := t.TempDir()
	sibling := filepath.Join(dir, siblingName)
	require.NoError(t, os.WriteFile(sibling, []byte("bin"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))

	tests := []struct {
		name   string
		envVar string
		self   string
		selfOK bool
		want   string
	}{
		{name: "env wins over sibling", envVar: filepath.Join("custom", "loopai"), self: filepath.Join(dir, "loopai-acp"), selfOK: true, want: filepath.Join("custom", "loopai")},
		{name: "sibling wins over path", self: filepath.Join(dir, "loopai-acp"), selfOK: true, want: sibling},
		{name: "path when no sibling", self: filepath.Join(dir, "sub", "loopai-acp"), selfOK: true, want: filepath.Join("path", "loopai")},
		{name: "path when executable unknown", selfOK: false, want: filepath.Join("path", "loopai")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _, _ := testEnv(0)
			e.stat = os.Stat
			e.getenv = func(k string) string {
				if k == loopaiEnv {
					return tc.envVar
				}
				return ""
			}
			e.executable = func() (string, error) {
				if !tc.selfOK {
					return "", errors.New("unknown")
				}
				return tc.self, nil
			}
			got, err := resolveLoopai(e)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveLoopaiSiblingDirectoryIgnored(t *testing.T) {
	siblingName := "loopai"
	if runtime.GOOS == "windows" {
		siblingName += ".exe"
	}
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, siblingName), 0o700))
	e, _, _, _ := testEnv(0)
	e.stat = os.Stat
	e.executable = func() (string, error) { return filepath.Join(dir, "loopai-acp"), nil }
	got, err := resolveLoopai(e)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("path", "loopai"), got)
}

func TestLaunchInherited(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	tests := []struct {
		name string
		exit string
		want int
	}{
		{name: "success", exit: "0", want: 0},
		{name: "failure code forwarded", exit: "3", want: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := filepath.Join(t.TempDir(), "args")
			t.Setenv(helperEnv, argsFile)
			t.Setenv(helperExitEnv, tc.exit)
			code, err := launchInherited(self, []string{"--acp"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, code)
			got, err := os.ReadFile(argsFile) //nolint:gosec // test temp file
			require.NoError(t, err)
			assert.Equal(t, "--acp", string(got))
		})
	}
}

func TestLaunchInheritedSignalTermination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals cannot be forwarded to a child process on Windows")
	}
	self, err := os.Executable()
	require.NoError(t, err)

	t.Run("SIGTERM is forwarded to the child", func(t *testing.T) {
		argsFile := filepath.Join(t.TempDir(), "args")
		t.Setenv(helperEnv, argsFile)
		t.Setenv(helperExitEnv, helperAwaitTerm)
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := launchInherited(self, []string{"--acp"})
			done <- result{code, err}
		}()
		require.Eventually(t, func() bool { _, err := os.Stat(argsFile); return err == nil },
			10*time.Second, 10*time.Millisecond, "the child never became ready")
		p, err := os.FindProcess(os.Getpid())
		require.NoError(t, err)
		// the launcher has SIGTERM registered while the child runs, so this process survives it
		require.NoError(t, p.Signal(syscall.SIGTERM))
		select {
		case res := <-done:
			require.NoError(t, res.err)
			assert.Equal(t, helperTermCode, res.code, "the child received the forwarded SIGTERM")
		case <-time.After(20 * time.Second):
			t.Fatal("the launcher did not forward SIGTERM")
		}
	})

	t.Run("a signal-killed child exits 1", func(t *testing.T) {
		t.Setenv(helperEnv, filepath.Join(t.TempDir(), "args"))
		t.Setenv(helperExitEnv, helperSelfKill)
		code, err := launchInherited(self, []string{"--acp"})
		require.NoError(t, err)
		assert.Equal(t, 1, code)
	})
}

func TestLaunchInheritedStartError(t *testing.T) {
	_, err := launchInherited(filepath.Join(t.TempDir(), "missing-loopai"), []string{"--acp"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start")
}
