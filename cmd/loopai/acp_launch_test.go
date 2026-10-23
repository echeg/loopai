//go:build !windows

// The launch tests start the test binary as a launcher and, through it, the stub detached children of
// detach_launch_unix_test.go, so like that file they build on Unix only.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/runs"
)

const (
	// acpFakeLauncherEnv makes the test binary a fake launcher: it holds the mode and the dump path,
	// separated by a unit separator. ok prints a run id, refuse fails with a refusal on stderr, silent
	// fails printing nothing, and noid succeeds without printing a run id.
	acpFakeLauncherEnv = "LOOPAI_TEST_ACP_FAKE_LAUNCHER"
	// acpLauncherMainEnv makes the test binary run main() as the launcher with the arguments after --,
	// starting stub detached children.
	acpLauncherMainEnv = "LOOPAI_TEST_ACP_LAUNCHER_MAIN"
	// acpFakeRunID is the run id the fake launcher prints.
	acpFakeRunID = "feature-20261022-120000-abcd"
	// acpFakeRefusal is what the fake launcher prints when it refuses.
	acpFakeRefusal = "error: run " + acpFakeRunID + " of docs/plans/feature.md is already running; " +
		"follow it with loopai --attach " + acpFakeRunID
)

// acpLauncherDump is what the fake launcher records: its arguments after the selector, the environment
// it started with, and its working directory.
type acpLauncherDump struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
}

// TestACPFakeLauncherHelper is the fake launcher the launch tests start, not a test of its own.
func TestACPFakeLauncherHelper(t *testing.T) {
	raw, ok := os.LookupEnv(acpFakeLauncherEnv)
	if !ok {
		t.Skip("helper process only")
	}
	mode, dumpPath, _ := strings.Cut(raw, "\x1f")
	cwd, _ := os.Getwd()
	dump, _ := json.Marshal(acpLauncherDump{Args: os.Args[slices.Index(os.Args, "--")+1:], Env: detachStubEnviron, Cwd: cwd})
	if err := os.WriteFile(dumpPath, dump, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake launcher:", err)
		os.Exit(4)
	}
	switch mode {
	case "refuse":
		fmt.Println("loopai test")
		fmt.Fprintln(os.Stderr, acpFakeRefusal)
		os.Exit(1)
	case "silent":
		os.Exit(1)
	case "noid":
		fmt.Println("loopai test")
	default:
		fmt.Fprintln(os.Stderr, "warning: a warning the launch succeeded despite")
		fmt.Printf("loopai test\nrun: %s\nfollow: loopai --attach %s\n", acpFakeRunID, acpFakeRunID)
	}
	os.Exit(0)
}

// TestACPLauncherMainHelper runs main() as the launcher of a loopai thread's run, with the arguments
// after --, whose detached children are stubs; not a test of its own.
func TestACPLauncherMainHelper(t *testing.T) {
	if _, ok := os.LookupEnv(acpLauncherMainEnv); !ok {
		t.Skip("helper process only")
	}
	detachChildArgs = detachStubSelector
	os.Args = append([]string{"loopai"}, os.Args[slices.Index(os.Args, "--")+1:]...)
	main()
	os.Exit(0) // main returns after a successful launch
}

// useACPLauncher makes launchACPRun start the test binary as the launcher helper selected by test.
func useACPLauncher(t *testing.T, test string) {
	t.Helper()
	acpLauncherArgs = []string{"-test.run=^" + test + "$", "--"}
	t.Cleanup(func() { acpLauncherArgs = nil })
}

func TestACPLaunchLauncherArgs(t *testing.T) {
	plan := []string{"--task-model=codex", "docs/plans/x.md"}
	assert.Equal(t, []string{"--detach", "--acp-thread=thread:a%3Ab", "--acp-session=loopai-1", "--task-model=codex",
		"docs/plans/x.md"}, acpLaunch{threadID: "thread:a%3Ab", sessionID: "loopai-1", args: plan}.launcherArgs())
	assert.Equal(t, []string{"--detach", "--acp-session=loopai-1", "--task-model=codex", "docs/plans/x.md"},
		acpLaunch{sessionID: "loopai-1", args: plan}.launcherArgs(), "an unknown thread id is left out")
}

func TestACPLaunchEnv(t *testing.T) {
	environ := []string{"PATH=/bin", "T3_ACP_MCP_ENDPOINT=http://x", "T3_ACP_MCP_AUTHORIZATION=Bearer y",
		"LOOPAI_T3_TOKEN=secret", "CMUX_WORKSPACE_ID=ws", "LOOPAI_CONFIG_DIR=/now", "LOOPAI_WEB_HOST=0.0.0.0"}

	assert.Equal(t, []string{"PATH=/bin", "LOOPAI_CONFIG_DIR=/now", "LOOPAI_WEB_HOST=0.0.0.0"}, acpLaunchEnv(environ, nil),
		"a new run's launcher gets the scrubbed environment")

	env := acpLaunchEnv(environ, map[string]string{"LOOPAI_CONFIG_DIR": "/then", "LOOPAI_ORCA": "1", "LD_PRELOAD": "/evil.so"})
	assert.Equal(t, []string{"PATH=/bin", "LOOPAI_WEB_HOST=0.0.0.0", "LOOPAI_CONFIG_DIR=/then", "LOOPAI_ORCA=1"}, env,
		"a relaunch restores the recorded options over the environment's, and only option variables")
}

func TestLaunchedRunID(t *testing.T) {
	tests := []struct {
		name, stdout, want string
	}{
		{name: "after the banner", stdout: "loopai 1.0\nrun: " + acpFakeRunID + "\nfollow: loopai --attach x\n",
			want: acpFakeRunID},
		{name: "no run line", stdout: "loopai 1.0\n"},
		{name: "an invalid id", stdout: "run: ../escape\n"},
		{name: "nothing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, launchedRunID(tc.stdout))
		})
	}
}

func TestLaunchACPRun(t *testing.T) {
	useACPLauncher(t, "TestACPFakeLauncherHelper")
	t.Setenv("T3_ACP_MCP_ENDPOINT", "http://127.0.0.1:1/mcp")
	t.Setenv("T3_ACP_MCP_AUTHORIZATION", "Bearer mcp-secret")
	t.Setenv("LOOPAI_T3_TOKEN", "t3-secret")
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
	t.Setenv("LOOPAI_CONFIG_DIR", "/now")
	launch := func(t *testing.T, mode string, l acpLaunch) (string, acpLauncherDump, error) {
		t.Helper()
		dumpPath := filepath.Join(t.TempDir(), "dump.json")
		t.Setenv(acpFakeLauncherEnv, mode+"\x1f"+dumpPath)
		id, err := launchACPRun(t.Context(), l)
		data, readErr := os.ReadFile(dumpPath) //nolint:gosec // test temp file
		require.NoError(t, readErr)
		var dump acpLauncherDump
		require.NoError(t, json.Unmarshal(data, &dump))
		return id, dump, err
	}
	args := []string{"--task-model=codex", "docs/plans/feature.md"}

	t.Run("a new run", func(t *testing.T) {
		cwd := t.TempDir()
		id, dump, err := launch(t, "ok", acpLaunch{cwd: cwd, threadID: "thread-1", sessionID: "loopai-1", args: args})
		require.NoError(t, err)
		assert.Equal(t, acpFakeRunID, id)
		assert.Equal(t, []string{"--detach", "--acp-thread=thread-1", "--acp-session=loopai-1", "--task-model=codex",
			"docs/plans/feature.md"}, dump.Args)
		assert.True(t, sameProgressRoot(cwd, dump.Cwd), "the launcher runs in the session cwd, not %s", dump.Cwd)
		for _, kv := range dump.Env {
			key, _, _ := strings.Cut(kv, "=")
			assert.False(t, detachScrubbed(key), "%s reaches the launcher", key)
			assert.NotContains(t, kv, "mcp-secret")
			assert.NotContains(t, kv, "t3-secret")
		}
		assert.Contains(t, dump.Env, "LOOPAI_CONFIG_DIR=/now")
		assert.Contains(t, dump.Env, acpFakeLauncherEnv+"="+os.Getenv(acpFakeLauncherEnv), "the rest is passed on")
	})

	t.Run("a relaunch restores the recorded options", func(t *testing.T) {
		id, dump, err := launch(t, "ok", acpLaunch{cwd: t.TempDir(), sessionID: "loopai-1", args: args,
			envOptions: map[string]string{"LOOPAI_CONFIG_DIR": "/then", "LOOPAI_ORCA": "1"}})
		require.NoError(t, err)
		assert.Equal(t, acpFakeRunID, id)
		assert.Equal(t, []string{"--detach", "--acp-session=loopai-1", "--task-model=codex", "docs/plans/feature.md"},
			dump.Args, "no --acp-thread without a thread id")
		assert.Contains(t, dump.Env, "LOOPAI_CONFIG_DIR=/then")
		assert.NotContains(t, dump.Env, "LOOPAI_CONFIG_DIR=/now")
		assert.Contains(t, dump.Env, "LOOPAI_ORCA=1")
	})

	t.Run("the refusal is the error", func(t *testing.T) {
		_, _, err := launch(t, "refuse", acpLaunch{cwd: t.TempDir(), sessionID: "loopai-1", args: args})
		require.EqualError(t, err, acpFakeRefusal)
	})

	t.Run("a failure without a refusal", func(t *testing.T) {
		_, _, err := launch(t, "silent", acpLaunch{cwd: t.TempDir(), sessionID: "loopai-1", args: args})
		require.ErrorContains(t, err, "start the detached run: exit status 1")
	})

	t.Run("a success without a run id", func(t *testing.T) {
		_, _, err := launch(t, "noid", acpLaunch{cwd: t.TempDir(), sessionID: "loopai-1", args: args})
		require.EqualError(t, err, `the detached run launcher printed no run id: "loopai test"`)
	})

	t.Run("a session cwd that is gone", func(t *testing.T) {
		_, err := launchACPRun(t.Context(), acpLaunch{cwd: filepath.Join(t.TempDir(), "gone"), sessionID: "loopai-1",
			args: args})
		require.ErrorContains(t, err, "start the detached run")
	})
}

// TestLaunchACPRunThroughTheLauncher launches a loopai thread's run through the real --detach
// launcher, whose child is a stub: the run records origin t3-thread with both ids, and a second
// launch of the plan while the run is live returns the launcher's refusal.
func TestLaunchACPRunThroughTheLauncher(t *testing.T) {
	const thread = "thread:provider:claudeAgent:native-thread:thread%3Aprovider%3AclaudeAgent%3Aagent%3A3"
	const session = "loopai-0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	f := newDetachLaunchFixture(t)
	cfgDir := filepath.Join(t.TempDir(), "config")
	t.Setenv("LOOPAI_CONFIG_DIR", cfgDir)
	t.Setenv("T3_ACP_MCP_AUTHORIZATION", "Bearer mcp-secret")
	t.Setenv(acpLauncherMainEnv, "1")
	useACPLauncher(t, "TestACPLauncherMainHelper")
	l := acpLaunch{cwd: f.repo, threadID: thread, sessionID: session, args: []string{"--task-model=codex:gpt-6-astra:high",
		detachLaunchPlan}}

	id, err := launchACPRun(t.Context(), l)
	require.NoError(t, err)
	desc := f.onlyRun(t)
	assert.Equal(t, desc.ID, id)
	assert.Equal(t, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: thread, ACPSessionID: session}, desc.Origin)
	assert.Equal(t, []string{"--task-model=codex:gpt-6-astra:high", detachLaunchPlan}, desc.Argv)
	assert.Equal(t, map[string]string{"LOOPAI_CONFIG_DIR": cfgDir}, desc.EnvOptions)
	for _, kv := range f.dump(t, id).Env {
		assert.NotContains(t, kv, "mcp-secret", "the provider's MCP credential never reaches the run")
	}
	status, err := runs.Classify(f.repo, runs.Dir(f.repo, id))
	require.NoError(t, err)
	assert.Equal(t, runs.StatusRunning, status)

	_, err = launchACPRun(t.Context(), l)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error: run "+id+" of "+detachLaunchPlan+" is already running; follow it with "+
		"loopai --attach "+id)
	assert.Len(t, f.runDirs(), 1, "the refused launch started nothing")
}
