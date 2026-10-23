package main

import (
	"bytes"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
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
// With helperPIDEnv it also records its pid and the value of helperMarkerEnv in the named file.
const (
	helperEnv       = "LOOPAI_ACP_TEST_HELPER_ARGS"
	helperExitEnv   = "LOOPAI_ACP_TEST_HELPER_EXIT"
	helperPIDEnv    = "LOOPAI_ACP_TEST_HELPER_PID"
	helperMarkerEnv = "LOOPAI_ACP_TEST_HELPER_MARKER"
	helperAwaitTerm = "await-term"
	helperSelfKill  = "self-kill"
	helperTermCode  = 42
)

// authWords is the Go form of the pattern T3 Code uses to mark a provider unauthenticated; nothing the
// launcher prints may match it.
var authWords = regexp.MustCompile(`(?i)\b(authenticat(e|ed|es|ing|ion)|credentials?|log([\s-]+)?in)\b`)

func TestMain(m *testing.M) {
	if out := os.Getenv(helperEnv); out != "" {
		os.Exit(runHelper(out, os.Getenv(helperExitEnv)))
	}
	os.Exit(m.Run())
}

func runHelper(out, mode string) int {
	if pidFile := os.Getenv(helperPIDEnv); pidFile != "" {
		record := strconv.Itoa(os.Getpid()) + "\n" + os.Getenv(helperMarkerEnv)
		_ = os.WriteFile(pidFile, []byte(record), 0o600)
	}
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

// testEnv returns an env with a stderr buffer, no LOOPAI_ACP_LOOPAI, no sibling, a loopai on PATH,
// and a recording launcher returning code.
func testEnv(code int) (e env, stderr *bytes.Buffer, calls *[]launchCall) {
	stderr = &bytes.Buffer{}
	calls = &[]launchCall{}
	e = env{
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
	return e, stderr, calls
}

// TestRunLaunchesLoopaiACPForAnyArgv checks that the launcher takes no arguments of its own: empty
// argv, the old Grok driver's probes and session argv, and anything else a provider instance's
// arguments hold all start "loopai --acp", whose exit code the launcher returns.
func TestRunLaunchesLoopaiACPForAnyArgv(t *testing.T) {
	argvs := [][]string{
		nil,
		{},
		{"--version"},
		{"models"},
		{"inspect", "--json"},
		{"update"},
		{"agent", "stdio"},
		{"--permission-mode", "auto", "agent", "--always-approve", "stdio"},
		{"--acp", "docs/plans/x.md", "--weird=1"},
	}
	for _, argv := range argvs {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			e, stderr, calls := testEnv(7)
			assert.Equal(t, 7, run(argv, e), "the exit code is forwarded")
			require.Len(t, *calls, 1)
			assert.Equal(t, launchCall{path: filepath.Join("path", "loopai"), args: []string{"--acp"}}, (*calls)[0])
			assert.Empty(t, stderr.String())
		})
	}
}

func TestRunLaunchError(t *testing.T) {
	e, stderr, _ := testEnv(0)
	e.launch = func(string, []string) (int, error) { return 0, errors.New("boom") }
	assert.Equal(t, 1, run(nil, e))
	assert.Contains(t, stderr.String(), "boom")
}

func TestRunMissingLoopai(t *testing.T) {
	e, stderr, calls := testEnv(0)
	e.lookPath = func(string) (string, error) {
		return "", errors.New(`exec: "loopai": executable file not found in $PATH`)
	}
	assert.Equal(t, 1, run([]string{"agent", "stdio"}, e))
	assert.Empty(t, *calls)
	msg := stderr.String()
	assert.Contains(t, msg, loopaiEnv)
	assert.Contains(t, msg, "executable file not found")
	assert.False(t, authWords.MatchString(msg), "T3 Code would read %q as a sign-in failure", msg)
}

// TestLauncherTextHasNoAuthWords keeps every user-visible string of the launcher, the package doc
// included, free of the words T3 Code reads as a sign-in failure.
func TestLauncherTextHasNoAuthWords(t *testing.T) {
	for _, file := range []string{"main.go", "exec_unix.go", "exec_windows.go"} {
		data, err := os.ReadFile(file) //nolint:gosec // the package's own sources
		require.NoError(t, err)
		assert.False(t, authWords.Match(data), "%s matches %q", file, authWords.Find(data))
	}
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
			e, _, _ := testEnv(0)
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
	e, _, _ := testEnv(0)
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
