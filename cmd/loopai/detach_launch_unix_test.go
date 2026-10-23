//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/t3"
	"github.com/umputun/ralphex/pkg/t3/t3test"
)

const (
	// detachStubEnv makes the test binary act as a stub detached child, in the mode it holds: ok
	// answers ok, error answers {"ok":false,"error":"x"}, exit exits without an answer, and hang never
	// answers. ok and hang hold the run's lifetime lock until the gate file exists.
	detachStubEnv = "LOOPAI_TEST_DETACH_STUB"
	// detachStubGateEnv names the gate file a stub child waits for, at most a minute.
	detachStubGateEnv = "LOOPAI_TEST_DETACH_STUB_GATE"
	// detachStubDumpFile is what a stub child records in its run directory.
	detachStubDumpFile = "stub-dump.json"
	// detachLaunchMainEnv makes the test binary run main() with the unit-separator-delimited
	// arguments it holds, starting stub children.
	detachLaunchMainEnv = "LOOPAI_TEST_DETACH_LAUNCH_MAIN"
	// detachLaunchSandboxEnv holds a sandbox signal, KEY=value, that the main helper sets before main():
	// TestMain unsets the real signals in every test process, the re-executed helper included.
	detachLaunchSandboxEnv = "LOOPAI_TEST_DETACH_SANDBOX"
	// detachLaunchPlan is the plan every launcher test launches.
	detachLaunchPlan = "docs/plans/20261022-feature.md"
)

// detachStubSelector is the argv prefix that makes a re-executed test binary the stub child.
var detachStubSelector = []string{"-test.run=^TestDetachLaunchChildHelper$", "--"}

// detachRealChildSelector is the argv prefix that makes a re-executed test binary the real child, run
// through main() with the real T3 bridge; detachRealChildEnv names the file it writes the environment
// it started with into.
var detachRealChildSelector = []string{"-test.run=^TestDetachLaunchRealChildHelper$", "--"}

const detachRealChildEnv = "LOOPAI_TEST_DETACH_REAL_CHILD"

// detachStubEnviron is the environment the process started with. TestMain unsets the ambient T3 and
// cmux variables before any test runs, so a stub child records this copy to show what it received.
var detachStubEnviron = os.Environ()

// detachStubDump is what a stub child records: its arguments after the helper selector, its
// environment, the launcher's secret line, its pid, and its working directory.
type detachStubDump struct {
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Secret string   `json:"secret"`
	PID    int      `json:"pid"`
	Cwd    string   `json:"cwd"`
}

// TestDetachLaunchChildHelper is the stub child the launcher tests start, not a test of its own.
func TestDetachLaunchChildHelper(t *testing.T) {
	mode, ok := os.LookupEnv(detachStubEnv)
	if !ok {
		t.Skip("helper process only")
	}
	os.Exit(runDetachStub(mode))
}

// TestDetachLaunchMainHelper runs main() as a launcher whose children are stubs, not a test of its own.
func TestDetachLaunchMainHelper(t *testing.T) {
	raw, ok := os.LookupEnv(detachLaunchMainEnv)
	if !ok {
		t.Skip("helper process only")
	}
	if signal, set := os.LookupEnv(detachLaunchSandboxEnv); set {
		key, value, _ := strings.Cut(signal, "=")
		t.Setenv(key, value)
	}
	detachChildArgs = detachStubSelector
	os.Args = append([]string{"loopai"}, strings.Split(raw, "\x1f")...)
	main()
	os.Exit(0) // main returns after a successful launch, as for every command that succeeds
}

// TestDetachLaunchRealChildHelper is the real detached child a launcher test starts, not a test of its
// own: it records the environment it started with, which TestMain has not touched yet, and runs main()
// with the real T3 bridge.
func TestDetachLaunchRealChildHelper(t *testing.T) {
	dump, ok := os.LookupEnv(detachRealChildEnv)
	if !ok {
		t.Skip("helper process only")
	}
	if err := os.WriteFile(dump, []byte(strings.Join(detachStubEnviron, "\n")), 0o600); err != nil {
		t.Fatalf("record the environment: %v", err)
	}
	newT3Bridge = startT3Bridge
	args := os.Args[slices.Index(os.Args, "--")+1:]
	os.Args = append([]string{"loopai"}, args...)
	main()
	t.Fatal("main returned without exiting")
}

// runDetachStub is the stub child: it takes the handshake pipes and the run's lifetime lock as the
// real child does, records what it received, and answers per mode. It returns the exit code.
func runDetachStub(mode string) int {
	args := os.Args
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[i+1:]
	}
	if len(args) == 0 || !strings.HasPrefix(args[0], "--detached-run=") {
		fmt.Fprintf(os.Stderr, "stub: unexpected arguments %q\n", args)
		return 4
	}
	secretPipe, ackPipe, err := takeHandshakePipes(detachSecretFD, detachAckFD)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stub:", err)
		return 4
	}
	secret, _ := io.ReadAll(secretPipe)
	dir := runs.Dir(".", strings.TrimPrefix(args[0], "--detached-run="))
	desc, err := runs.LoadDescriptor(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stub:", err)
		return 4
	}
	release, err := runs.AcquireBounded(filepath.Join(dir, runs.LockFile), 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stub:", err)
		return 4
	}
	defer func() { _ = release() }()
	cwd, _ := os.Getwd()
	dump, _ := json.Marshal(detachStubDump{Args: args, Env: detachStubEnviron, Secret: string(secret),
		PID: os.Getpid(), Cwd: cwd})
	if err := os.WriteFile(filepath.Join(dir, detachStubDumpFile), dump, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "stub:", err)
		return 4
	}

	ack := &detachAck{w: ackPipe}
	switch mode {
	case "error":
		_ = ack.send(detachAckMessage{Error: "x"})
		return 1
	case "exit":
		fmt.Fprintln(os.Stderr, "stub: exiting without an answer")
		return 3
	case "hang":
	default:
		_ = ack.send(detachAckMessage{OK: true, RunID: desc.ID, Attempt: desc.Attempt})
	}
	gate := os.Getenv(detachStubGateEnv)
	for i := 0; gate != "" && i < 1200; i++ {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// detachLaunchFixture is a repository holding a committed plan, set up for launches whose children
// are stubs: the working directory is the repository, HOME is redirected, and the stub mode is ok.
type detachLaunchFixture struct {
	repo string
	gate string
}

func newDetachLaunchFixture(t *testing.T) *detachLaunchFixture {
	t.Helper()
	f := &detachLaunchFixture{repo: setupTestRepo(t), gate: filepath.Join(t.TempDir(), "gate")}
	runGit(t, f.repo, "config", "core.autocrlf", "false")
	plan := filepath.Join(f.repo, detachLaunchPlan)
	require.NoError(t, os.MkdirAll(filepath.Dir(plan), 0o750))
	require.NoError(t, os.WriteFile(plan, []byte("# Feature\n\n### Task 1: One\n- [ ] a\n"), 0o600))
	runGit(t, f.repo, "add", detachLaunchPlan)
	runGit(t, f.repo, "commit", "-m", "add plan")

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(detachStubEnv, "ok")
	t.Setenv(detachStubGateEnv, f.gate)
	for _, key := range cmuxEnvOptions {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	detachChildArgs = detachStubSelector
	t.Cleanup(func() { detachChildArgs = nil })
	t.Chdir(f.repo)
	t.Cleanup(func() { f.stopStubs(t) })
	return f
}

// launch runs the launcher in this process with args, as parsed from the command line, and returns
// what it printed and its error.
func (f *detachLaunchFixture) launch(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return f.launchContext(t.Context(), t, args...)
}

// launchContext is launch with ctx.
func (f *detachLaunchFixture) launchContext(ctx context.Context, t *testing.T, args ...string) (stdout, stderr string,
	err error) {
	t.Helper()
	o := parseTestOpts(t, args...)
	require.NoError(t, validateFlags(o))
	var out, errOut bytes.Buffer
	err = runDetachCommand(ctx, o, &config.Config{}, detachIO{args: args, stdout: &out, stderr: &errOut, colors: testColors()})
	return out.String(), errOut.String(), err
}

// runDirs returns the run directories under the repository; none when there is no runs directory.
func (f *detachLaunchFixture) runDirs() []string {
	entries, err := os.ReadDir(runs.RunsDir(f.repo))
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(runs.RunsDir(f.repo), entry.Name()))
		}
	}
	return dirs
}

// onlyRun returns the single run directory's descriptor.
func (f *detachLaunchFixture) onlyRun(t *testing.T) runs.Descriptor {
	t.Helper()
	dirs := f.runDirs()
	require.Len(t, dirs, 1)
	desc, err := runs.LoadDescriptor(dirs[0])
	require.NoError(t, err)
	return desc
}

// dump returns what the stub child of run id recorded.
func (f *detachLaunchFixture) dump(t *testing.T, id string) detachStubDump {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runs.Dir(f.repo, id), detachStubDumpFile))
	require.NoError(t, err)
	var dump detachStubDump
	require.NoError(t, json.Unmarshal(data, &dump))
	return dump
}

// detachSeedID is the id of the previous run seedRun records.
const detachSeedID = "feature-20261022-120000-abcd"

// seedRun records the first attempt of a previous run detachSeedID of the plan on branch feature with
// origin and the recorded environment options, and returns its directory.
func (f *detachLaunchFixture) seedRun(t *testing.T, origin runs.Origin, envOptions map[string]string) string {
	t.Helper()
	id := detachSeedID
	dir := runs.Dir(f.repo, id)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	desc := runs.Descriptor{ID: id, Attempt: 1, Cwd: f.repo, Mode: "full", Plan: detachLaunchPlan,
		Branch: "feature", Argv: []string{"--old", detachLaunchPlan}, EnvOptions: envOptions, Origin: origin,
		ConsoleLog: filepath.Join(dir, runs.ConsoleLogFile), Worktree: filepath.Join(f.repo, ".loopai", "worktrees", "feature")}
	require.NoError(t, runs.SaveDescriptor(dir, &desc))
	return dir
}

// stopStubs opens the gate and waits until every stub child that recorded itself has exited, reaping
// those this process started.
func (f *detachLaunchFixture) stopStubs(t *testing.T) {
	t.Helper()
	_ = os.WriteFile(f.gate, nil, 0o600)
	for _, dir := range f.runDirs() {
		data, err := os.ReadFile(filepath.Join(dir, detachStubDumpFile)) //nolint:gosec // path built from t.TempDir
		if err != nil {
			continue
		}
		var dump detachStubDump
		if json.Unmarshal(data, &dump) != nil {
			continue
		}
		waitDetachStub(t, dump.PID, filepath.Join(dir, runs.LockFile))
	}
}

// waitDetachStub waits for the stub child pid to exit: it reaps a child of this process, and for a
// child of another process it waits for the run lock to be free. One still running after the bound
// is killed.
func waitDetachStub(t *testing.T, pid int, lock string) {
	t.Helper()
	deadline := time.Now().Add(detachChildWait)
	for time.Now().Before(deadline) {
		var status syscall.WaitStatus
		got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if got == pid {
			return
		}
		if err != nil {
			if held, _ := runs.Probe(lock); !held {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("stub child %d did not exit", pid)
}

var detachRunIDRe = regexp.MustCompile(`^feature-\d{8}-\d{6}-[0-9a-f]{4}$`)

func TestDetachLaunchStartsRun(t *testing.T) {
	f := newDetachLaunchFixture(t)
	require.NoDirExists(t, filepath.Join(f.repo, ".loopai"))
	t.Setenv("T3_ACP_MCP_ENDPOINT", "http://127.0.0.1:1/mcp")
	t.Setenv("T3_ACP_MCP_AUTHORIZATION", "Bearer mcp-secret")
	t.Setenv("LOOPAI_T3_TOKEN", "t3-secret")
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
	cfgDir := filepath.Join(t.TempDir(), "config")
	t.Setenv("LOOPAI_CONFIG_DIR", cfgDir)

	stdout, stderr, err := f.launch(t, "--detach", "--external-reviewers=", "--tasks-only", detachLaunchPlan)
	require.NoError(t, err, stderr)
	desc := f.onlyRun(t)
	id := desc.ID

	t.Run("prints the run id and the follow command", func(t *testing.T) {
		assert.Regexp(t, detachRunIDRe, id)
		assert.Equal(t, "run: "+id+"\nfollow: loopai --attach "+id+"\n", stdout)
	})

	dump := f.dump(t, id)
	t.Run("the child gets the raw arguments without --detach", func(t *testing.T) {
		assert.Equal(t, []string{"--detached-run=" + id, "--external-reviewers=", "--tasks-only", detachLaunchPlan}, dump.Args)
		assert.Equal(t, f.repo, dump.Cwd, "the child runs in the invoking checkout")
		assert.JSONEq(t, `{"t3_token":""}`, dump.Secret)
		assert.True(t, strings.HasSuffix(dump.Secret, "\n"), "one line")
	})

	t.Run("the child's environment carries no credentials and no cmux workspace", func(t *testing.T) {
		for _, kv := range dump.Env {
			key, _, _ := strings.Cut(kv, "=")
			assert.NotContains(t, []string{"T3_ACP_MCP_ENDPOINT", "T3_ACP_MCP_AUTHORIZATION", "LOOPAI_T3_TOKEN",
				"CMUX_WORKSPACE_ID"}, key)
			assert.NotContains(t, kv, "mcp-secret")
			assert.NotContains(t, kv, "t3-secret")
		}
		assert.Contains(t, dump.Env, "LOOPAI_CONFIG_DIR="+cfgDir)
		assert.Contains(t, dump.Env, detachStubEnv+"=ok", "the rest of the environment is passed on")
	})

	t.Run("the descriptor records the launch", func(t *testing.T) {
		assert.Equal(t, 1, desc.Attempt)
		assert.Equal(t, runs.Origin{Kind: runs.OriginCLI}, desc.Origin)
		assert.Equal(t, []string{"--external-reviewers=", "--tasks-only", detachLaunchPlan}, desc.Argv)
		assert.Equal(t, map[string]string{"LOOPAI_CONFIG_DIR": cfgDir}, desc.EnvOptions)
		assert.Equal(t, f.repo, desc.Cwd)
		assert.Equal(t, "tasks-only", desc.Mode)
		assert.Equal(t, detachLaunchPlan, desc.Plan)
		assert.Equal(t, "feature", desc.Branch)
		exe, exeErr := os.Executable()
		require.NoError(t, exeErr)
		assert.Equal(t, exe, desc.Executable)
		assert.Equal(t, filepath.Join(runs.Dir(f.repo, id), runs.ConsoleLogFile), desc.ConsoleLog)
		console, readErr := os.ReadFile(desc.ConsoleLog)
		require.NoError(t, readErr)
		assert.True(t, strings.HasPrefix(string(console), "=== loopai run "+id+", attempt 1, started "), string(console))
	})

	t.Run("the run directory is ignored before it exists", func(t *testing.T) {
		assert.FileExists(t, filepath.Join(f.repo, ".loopai", ".gitignore"))
		runGit(t, f.repo, "check-ignore", "-q", runs.Dir(f.repo, id))
		assert.Empty(t, strings.TrimSpace(gitOutput(t, f.repo, "status", "--porcelain")))
	})

	t.Run("the child holds the run once the launcher returns", func(t *testing.T) {
		status, classifyErr := runs.Classify(f.repo, runs.Dir(f.repo, id))
		require.NoError(t, classifyErr)
		assert.Equal(t, runs.StatusRunning, status)
	})
}

func TestDetachLaunchT3Thread(t *testing.T) {
	const thread = "thread:provider:claudeAgent:native-thread:thread%3Aprovider%3AclaudeAgent%3Aagent%3A3"
	const token = "t3-bearer-token-6f1c" //nolint:gosec // a fake token no server accepts

	t.Run("the token goes over the secret pipe and nowhere else", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv("LOOPAI_T3_TOKEN", " "+token+"\n")

		stdout, stderr, err := f.launch(t, "--detach", "--t3-thread", thread, "--tasks-only", detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.Equal(t, "run: "+desc.ID+"\nfollow: loopai --attach "+desc.ID+"\n", stdout)
		assert.NotContains(t, stdout+stderr, token)

		dump := f.dump(t, desc.ID)
		assert.JSONEq(t, `{"t3_token":"`+token+`"}`, dump.Secret, "the trimmed token")
		assert.Equal(t, 1, strings.Count(dump.Secret, "\n"), "one line")
		assert.True(t, strings.HasSuffix(dump.Secret, "\n"))
		for _, kv := range dump.Env {
			assert.NotContains(t, kv, token, "the child's environment never holds the token")
		}
		assert.Equal(t, runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: thread}, desc.Origin)
		assert.Equal(t, []string{"--tasks-only", detachLaunchPlan}, desc.Argv, "the thread is not a run argument")
		for _, name := range []string{runs.DescriptorFile, runs.ConsoleLogFile} {
			data, readErr := os.ReadFile(filepath.Join(runs.Dir(f.repo, desc.ID), name)) //nolint:gosec // test temp path
			require.NoError(t, readErr)
			assert.NotContains(t, string(data), token, "%s never holds the token", name)
		}
	})

	t.Run("an attached thread id is stripped too", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv("LOOPAI_T3_TOKEN", token)
		_, stderr, err := f.launch(t, "--detach", "--t3-thread="+thread, detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.Equal(t, []string{detachLaunchPlan}, desc.Argv)
		assert.Equal(t, thread, desc.Origin.ThreadID)
	})

	t.Run("without a token the launch is refused before anything is created", func(t *testing.T) {
		for _, value := range []string{"", " \n"} {
			f := newDetachLaunchFixture(t)
			t.Setenv("LOOPAI_T3_TOKEN", value)
			_, _, err := f.launch(t, "--detach", "--t3-thread", thread, detachLaunchPlan)
			require.EqualError(t, err, "--t3-thread needs LOOPAI_T3_TOKEN")
			assert.Empty(t, f.runDirs())
			assert.NoDirExists(t, filepath.Join(f.repo, ".loopai"))
		}
	})

	t.Run("a token without a thread is never sent", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv("LOOPAI_T3_TOKEN", token)
		_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.JSONEq(t, `{"t3_token":""}`, f.dump(t, desc.ID).Secret)
		assert.Equal(t, runs.Origin{Kind: runs.OriginCLI}, desc.Origin)
	})

	t.Run("a resume naming a thread shows the run there", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv("LOOPAI_T3_TOKEN", token)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "other", ACPSessionID: "sess_1"}, nil)
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: detachSeedID, Attempt: 1, Status: runs.StatusFailed}))

		_, stderr, err := f.launch(t, "--detach", "--t3-thread", thread, detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.Equal(t, detachSeedID, desc.ID)
		assert.Equal(t, 2, desc.Attempt)
		assert.Equal(t, runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: thread}, desc.Origin)
		assert.JSONEq(t, `{"t3_token":"`+token+`"}`, f.dump(t, desc.ID).Secret)
	})

	t.Run("a resume from a plain shell keeps the thread without a token", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv("LOOPAI_T3_TOKEN", token)
		origin := runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: thread}
		dir := f.seedRun(t, origin, nil)
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: detachSeedID, Attempt: 1, Status: runs.StatusFailed}))

		_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.Equal(t, origin, desc.Origin)
		assert.JSONEq(t, `{"t3_token":""}`, f.dump(t, desc.ID).Secret, "only --t3-thread hands over a token")
	})
}

func TestDetachLaunchACPThread(t *testing.T) {
	const thread = "thread:provider:claudeAgent:native-thread:thread%3Aprovider%3AclaudeAgent%3Aagent%3A3"
	const session = "loopai-0f1e2d3c4b5a69788796a5b4c3d2e1f0"

	t.Run("a loopai thread's launch records the thread and its session", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv("LOOPAI_T3_TOKEN", "t3-bearer-token-6f1c")
		stdout, stderr, err := f.launch(t, "--detach", "--acp-thread="+thread, "--acp-session="+session,
			"--task-model=codex:gpt-6-astra:high", detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.Equal(t, "run: "+desc.ID+"\nfollow: loopai --attach "+desc.ID+"\n", stdout)
		assert.Equal(t, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: thread, ACPSessionID: session}, desc.Origin)
		assert.Equal(t, []string{"--task-model=codex:gpt-6-astra:high", detachLaunchPlan}, desc.Argv,
			"neither id is a run argument")
		assert.JSONEq(t, `{"t3_token":""}`, f.dump(t, desc.ID).Secret, "a loopai thread's run gets no T3 token")
	})

	t.Run("without a thread id the session alone names the thread", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		_, stderr, err := f.launch(t, "--detach", "--acp-session", session, detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.Equal(t, runs.Origin{Kind: runs.OriginT3Thread, ACPSessionID: session}, desc.Origin)
		assert.Equal(t, []string{detachLaunchPlan}, desc.Argv)
	})

	t.Run("a resume from a loopai thread makes the run that thread's", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: detachSeedID, Attempt: 1, Status: runs.StatusStopped}))

		_, stderr, err := f.launch(t, "--detach", "--acp-thread", thread, "--acp-session="+session, detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.Equal(t, detachSeedID, desc.ID)
		assert.Equal(t, 2, desc.Attempt)
		assert.Equal(t, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: thread, ACPSessionID: session}, desc.Origin)
	})

	t.Run("a resume naming only the session keeps the recorded thread", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: thread, ACPSessionID: "loopai-old"}, nil)
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: detachSeedID, Attempt: 1, Status: runs.StatusFailed}))

		_, stderr, err := f.launch(t, "--detach", "--acp-session="+session, detachLaunchPlan)
		require.NoError(t, err, stderr)
		desc := f.onlyRun(t)
		assert.Equal(t, 2, desc.Attempt)
		assert.Equal(t, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: thread, ACPSessionID: session}, desc.Origin,
			"the new session owns the run, and the thread it recorded stays")
	})

	t.Run("a session-only resume of a terminal run records no thread", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginT3Terminal, ThreadID: "terminal-thread"}, nil)
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: detachSeedID, Attempt: 1, Status: runs.StatusFailed}))

		_, stderr, err := f.launch(t, "--detach", "--acp-session="+session, detachLaunchPlan)
		require.NoError(t, err, stderr)
		assert.Equal(t, runs.Origin{Kind: runs.OriginT3Thread, ACPSessionID: session}, f.onlyRun(t).Origin,
			"a terminal's thread is not the loopai thread the session belongs to")
	})
}

func TestDetachT3TokenLeavesNoTrace(t *testing.T) {
	const thread = "8e8b69fc-3359-47c8-a5a3-6dd91c245a86"
	token := "t3-leak-probe-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	f := newDetachChildFixture(t, false)
	f.addLocalConfig(t, "report_enabled = true")
	require.NoError(t, os.RemoveAll(f.dir), "the launcher creates the run")
	s := t3test.NewServer(t, t3test.WithToken(token),
		t3test.WithThread(t3test.Thread{ID: thread, ProjectID: "p", Title: "My thread"}),
		t3test.WithProject(t3test.Project{ID: "p", WorkspaceRoot: f.repo}))
	t3Home := t.TempDir()
	require.NoError(t, s.WriteRuntime(t3Home))
	dumps := t.TempDir()
	childEnv, modelEnv := filepath.Join(dumps, "child-env"), filepath.Join(dumps, "model-env")

	// the launcher runs in this process; the child inherits its environment
	for _, kv := range f.env {
		key, value, _ := strings.Cut(kv, "=")
		if os.Getenv(key) != value {
			t.Setenv(key, value)
		}
	}
	t.Setenv(t3.EnvHome, t3Home)
	t.Setenv("LOOPAI_T3_TOKEN", token)
	t.Setenv(detachRealChildEnv, childEnv)
	t.Setenv("DETACH_CHILD_ENV_DUMP", modelEnv)
	t.Chdir(f.repo)
	detachChildArgs = detachRealChildSelector
	t.Cleanup(func() { detachChildArgs = nil })

	args := []string{"--detach", "--t3-thread", thread, "docs/plans/child.md"}
	o := parseTestOpts(t, args...)
	require.NoError(t, validateFlags(o))
	var stdout, stderr bytes.Buffer
	require.NoError(t, runDetachCommand(t.Context(), o, &config.Config{},
		detachIO{args: args, stdout: &stdout, stderr: &stderr, colors: testColors()}), stderr.String())
	dirs, err := os.ReadDir(runs.RunsDir(f.repo))
	require.NoError(t, err)
	var runDir string
	for _, entry := range dirs {
		if entry.IsDir() {
			runDir = filepath.Join(runs.RunsDir(f.repo), entry.Name())
		}
	}
	desc, err := runs.LoadDescriptor(runDir)
	require.NoError(t, err)
	waitDetachStub(t, desc.PID, filepath.Join(runDir, runs.LockFile))

	outcome, err := runs.LoadOutcome(runDir)
	require.NoError(t, err)
	require.Equal(t, runs.StatusSucceeded, outcome.Status, "console log:\n%s", readTestFile(t, desc.ConsoleLog))
	th, _ := s.Thread(thread)
	require.Equal(t, "[loopai: done] My thread", th.Title, "the bridge worked with the token")
	require.NotEmpty(t, s.Handshakes())
	assert.Equal(t, "Bearer "+token, s.Handshakes()[0].Authorization)

	// sources are named, never printed: an environment dump holds whatever the developer's shell exports
	scanned := map[string]string{"the child's environment": readTestFile(t, childEnv),
		"a model process's environment": readTestFile(t, modelEnv), "launcher stdout": stdout.String(),
		"launcher stderr": stderr.String()}
	for _, source := range []string{"the child's environment", "a model process's environment"} {
		//nolint:testifylint // assert.Contains would print the dump, which holds the developer's shell secrets
		assert.True(t, strings.Contains(scanned[source], "\nHOME="), "%s was recorded", source)
	}
	desc, err = runs.LoadDescriptor(runDir)
	require.NoError(t, err)
	for _, name := range []string{runs.DescriptorFile, runs.OutcomeFile, runs.JournalFile, runs.ReportFile,
		runs.ConsoleLogFile} {
		assert.FileExists(t, filepath.Join(runDir, name))
	}
	require.NoError(t, filepath.WalkDir(runDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		scanned["run directory file "+entry.Name()] = readTestFile(t, path)
		return nil
	}))
	scanned["the progress log"] = readTestFile(t, desc.ProgressLog)
	scanned["the launch history"] = readTestFile(t, filepath.Join(filepath.Dir(f.config), launchHistoryFile))
	for source, text := range scanned {
		//nolint:testifylint // assert.NotContains would print the text, which may hold the developer's shell secrets
		assert.False(t, strings.Contains(text, token), "%s holds the token", source)
	}
}

// readTestFile returns the content of a file the test reads.
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path the test built
	require.NoError(t, err)
	return string(data)
}

func TestDetachLaunchThroughMain(t *testing.T) {
	f := newDetachLaunchFixture(t)
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachLaunchMainHelper$")
	cmd.Dir = f.repo
	cmd.Env = append(os.Environ(), detachLaunchMainEnv+"=--detach\x1f"+detachLaunchPlan,
		"HOME="+home, "USERPROFILE="+home, "LOOPAI_CONFIG_DIR="+filepath.Join(home, "config"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Run(), "stdout: %s\nstderr: %s", stdout.String(), stderr.String())

	desc := f.onlyRun(t)
	assert.Contains(t, stdout.String(), "\nrun: "+desc.ID+"\nfollow: loopai --attach "+desc.ID+"\n")
	assert.Equal(t, "full", desc.Mode)
	assert.Equal(t, []string{detachLaunchPlan}, desc.Argv)
	assert.Equal(t, map[string]string{"LOOPAI_CONFIG_DIR": filepath.Join(home, "config")}, desc.EnvOptions)
	assert.NoFileExists(t, filepath.Join(home, "config", launchHistoryFile),
		"the launcher records no launch history; its child does")
}

func TestDetachLaunchSandboxThroughMain(t *testing.T) {
	// launchInSandbox runs main() as the launcher in f's repository with SANDBOX_RUNTIME=1, HOME at
	// home, and LOOPAI_CONFIG_DIR at configDir when it is not empty, and checks that it refused.
	launchInSandbox := func(t *testing.T, f *detachLaunchFixture, home, configDir string) {
		t.Helper()
		env := []string{detachLaunchMainEnv + "=--detach\x1f" + detachLaunchPlan, detachLaunchSandboxEnv + "=SANDBOX_RUNTIME=1",
			"HOME=" + home, "USERPROFILE=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}
		if configDir != "" {
			env = append(env, "LOOPAI_CONFIG_DIR="+configDir)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestDetachLaunchMainHelper$")
		cmd.Dir = f.repo
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
			return strings.HasPrefix(kv, "LOOPAI_CONFIG_DIR=")
		}), env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "stdout: %s\nstderr: %s", stdout.String(), stderr.String())
		assert.Equal(t, 1, exitErr.ExitCode())
		assert.NotContains(t, stdout.String(), "run: ")
		assert.Contains(t, stderr.String(), "error: refusing to start a detached run inside an agent sandbox "+
			"(SANDBOX_RUNTIME=1): ")
		assert.Contains(t, stderr.String(), "(Mode 2 of the loopai-t3 skill)")
		assert.NotContains(t, stderr.String(), "load config")
		assert.NoDirExists(t, filepath.Join(f.repo, ".loopai"), "nothing is created in the checkout")
	}

	t.Run("an all-commented config in an unwritable directory is left alone", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		home := t.TempDir()
		configDir := filepath.Join(home, "config")
		for _, sub := range []string{"prompts", "agents"} {
			require.NoError(t, os.MkdirAll(filepath.Join(configDir, sub), 0o700))
		}
		// all-commented, so the defaults installer would rewrite it on every config load
		const commented = "# my notes\n"
		configFile := filepath.Join(configDir, "config")
		require.NoError(t, os.WriteFile(configFile, []byte(commented), 0o400))
		require.NoError(t, os.Chmod(configDir, 0o500))       //nolint:gosec // a directory without write permission
		t.Cleanup(func() { _ = os.Chmod(configDir, 0o700) }) //nolint:gosec // restore permissions for cleanup

		launchInSandbox(t, f, home, configDir)

		data, err := os.ReadFile(configFile) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Equal(t, commented, string(data), "the config is not rewritten")
		for _, sub := range []string{"prompts", "agents"} {
			entries, readErr := os.ReadDir(filepath.Join(configDir, sub))
			require.NoError(t, readErr)
			assert.Empty(t, entries, "no default %s are installed", sub)
		}
		assert.NoFileExists(t, filepath.Join(configDir, ".gitignore"))
	})

	t.Run("no config directory is created", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		home := t.TempDir()

		launchInSandbox(t, f, home, "")

		assert.NoDirExists(t, filepath.Join(home, ".config"), "the default config directory is not created")
		entries, err := os.ReadDir(home)
		require.NoError(t, err)
		assert.Empty(t, entries, "nothing is written under HOME")
	})
}

func TestDetachLaunchResumesFailedRun(t *testing.T) {
	f := newDetachLaunchFixture(t)
	const id = detachSeedID
	dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI},
		map[string]string{"LOOPAI_CONFIG_DIR": "/recorded", "LOOPAI_ORCA": "1"})
	require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: id, Attempt: 1, Status: runs.StatusFailed, Reason: "boom"}))
	require.NoError(t, runs.RequestStop(dir))
	t.Setenv("LOOPAI_CONFIG_DIR", "/explicit")

	stdout, stderr, err := f.launch(t, "--detach", "--tasks-only", detachLaunchPlan)
	require.NoError(t, err, stderr)

	assert.Equal(t, "run: "+id+"\nfollow: loopai --attach "+id+"\n", stdout, "the same id")
	desc := f.onlyRun(t)
	assert.Equal(t, 2, desc.Attempt)
	assert.Equal(t, []string{"--tasks-only", detachLaunchPlan}, desc.Argv, "the new arguments replace the recorded ones")
	assert.Equal(t, "tasks-only", desc.Mode)
	archived, err := os.ReadFile(filepath.Join(dir, "outcome-attempt-1.json")) //nolint:gosec // path built from t.TempDir
	require.NoError(t, err)
	assert.Contains(t, string(archived), `"reason": "boom"`)
	assert.NoFileExists(t, filepath.Join(dir, runs.OutcomeFile), "the new attempt has no outcome yet")
	stop, err := runs.StopRequested(dir)
	require.NoError(t, err)
	assert.False(t, stop, "the stop marker is cleared")

	dump := f.dump(t, id)
	assert.Equal(t, "--detached-run="+id, dump.Args[0])
	assert.Contains(t, dump.Env, "LOOPAI_CONFIG_DIR=/explicit", "an explicit option wins over the recorded one")
	assert.Contains(t, dump.Env, "LOOPAI_ORCA=1", "a recorded option the shell lacks is restored")
	assert.Equal(t, map[string]string{"LOOPAI_CONFIG_DIR": "/explicit", "LOOPAI_ORCA": "1"}, desc.EnvOptions)
	console, err := os.ReadFile(desc.ConsoleLog)
	require.NoError(t, err)
	assert.Contains(t, string(console), "=== loopai run "+id+", attempt 2, started ")
}

func TestDetachLaunchResumeKeepsOrigin(t *testing.T) {
	f := newDetachLaunchFixture(t)
	const id = detachSeedID
	origin := runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "thread:provider:claudeAgent:native-thread:thread%3Ax",
		ACPSessionID: "sess_123"}
	f.seedRun(t, origin, nil) // crashed: no outcome, no lock

	_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
	require.NoError(t, err, stderr)

	desc := f.onlyRun(t)
	assert.Equal(t, id, desc.ID)
	assert.Equal(t, 2, desc.Attempt)
	assert.Equal(t, origin, desc.Origin, "a plain shell resumes the thread's run as the thread's run")
}

func TestDetachLaunchAfterSuccess(t *testing.T) {
	const oldID = detachSeedID

	t.Run("a succeeded run gets a new run with origin cli", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginT3Thread, ThreadID: "t", ACPSessionID: "s"}, nil)
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: oldID, Attempt: 1, Status: runs.StatusSucceeded}))

		stdout, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)

		require.Len(t, f.runDirs(), 2)
		id := strings.TrimSuffix(strings.TrimPrefix(strings.Split(stdout, "\n")[0], "run: "), "\n")
		assert.NotEqual(t, oldID, id)
		assert.Regexp(t, detachRunIDRe, id)
		desc, err := runs.LoadDescriptor(runs.Dir(f.repo, id))
		require.NoError(t, err)
		assert.Equal(t, 1, desc.Attempt)
		assert.Equal(t, runs.Origin{Kind: runs.OriginCLI}, desc.Origin)
		old, err := runs.LoadDescriptor(dir)
		require.NoError(t, err)
		assert.Equal(t, 1, old.Attempt, "a succeeded run is never resumed")
	})

	t.Run("a success whose outcome was lost is never resumed", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		j, err := runs.OpenJournal(dir, 1)
		require.NoError(t, err)
		_, err = j.Append(runs.Event{Type: runs.EventFinish, Status: runs.StatusSucceeded})
		require.NoError(t, err)
		require.NoError(t, j.Close())

		_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)

		require.Len(t, f.runDirs(), 2, "a new run, not a second attempt")
		old, err := runs.LoadDescriptor(dir)
		require.NoError(t, err)
		assert.Equal(t, 1, old.Attempt)
	})

	t.Run("a succeeded run that kept its worktree blocks a new launch until it is gone", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		kept := filepath.Join(f.repo, ".loopai", "worktrees", "feature")
		require.NoError(t, os.MkdirAll(kept, 0o750))
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: oldID, Attempt: 1, Status: runs.StatusSucceeded,
			WorktreeKept: kept}))

		_, _, err := f.launch(t, "--detach", detachLaunchPlan)
		require.ErrorContains(t, err, "run "+oldID+" of "+detachLaunchPlan+" succeeded but kept its worktree "+kept)
		require.ErrorContains(t, err, "loopai --merge feature")
		assert.Len(t, f.runDirs(), 1, "no run is created")

		require.NoError(t, os.RemoveAll(kept))
		_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)
		assert.Len(t, f.runDirs(), 2)
	})

	t.Run("a lost outcome with its worktree still in place blocks too", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		require.NoError(t, os.MkdirAll(filepath.Join(f.repo, ".loopai", "worktrees", "feature"), 0o750))
		j, err := runs.OpenJournal(dir, 1)
		require.NoError(t, err)
		_, err = j.Append(runs.Event{Type: runs.EventFinish, Status: runs.StatusSucceeded})
		require.NoError(t, err)
		require.NoError(t, j.Close())

		_, _, err = f.launch(t, "--detach", detachLaunchPlan)
		require.ErrorContains(t, err, "succeeded but kept its worktree")
	})

	t.Run("a registered worktree of the branch blocks a new launch until it is removed", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		// the outcome says the worktree was removed, yet the branch is still checked out in one
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: oldID, Attempt: 1, Status: runs.StatusSucceeded}))
		wt := filepath.Join(t.TempDir(), "elsewhere")
		runGit(t, f.repo, "worktree", "add", "-b", "feature", wt)

		_, _, err := f.launch(t, "--detach", detachLaunchPlan)
		require.ErrorContains(t, err, "run "+oldID+" of "+detachLaunchPlan+
			" succeeded, but its branch feature is still checked out in worktree "+wt+"; ")
		require.ErrorContains(t, err, "loopai --merge feature")
		assert.Len(t, f.runDirs(), 1, "no run is created")

		runGit(t, f.repo, "worktree", "remove", wt)
		_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)
		assert.Len(t, f.runDirs(), 2)
	})

	t.Run("the branch checked out in the invoking checkout does not block", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: oldID, Attempt: 1, Status: runs.StatusSucceeded}))
		runGit(t, f.repo, "checkout", "-b", "feature")

		_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)
		assert.Len(t, f.runDirs(), 2)
	})

	t.Run("a registered worktree whose directory is gone does not block", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: oldID, Attempt: 1, Status: runs.StatusSucceeded}))
		wt := filepath.Join(t.TempDir(), "gone")
		runGit(t, f.repo, "worktree", "add", "-b", "feature", wt)
		require.NoError(t, os.RemoveAll(wt))

		_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)
		assert.Len(t, f.runDirs(), 2)
	})
}

// seedSucceededRun records a succeeded first attempt of run id of plan on branch, created at created,
// whose outcome records kept as the worktree it kept.
func (f *detachLaunchFixture) seedSucceededRun(t *testing.T, id, plan, branch string, created time.Time, kept string) {
	t.Helper()
	dir := runs.Dir(f.repo, id)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	desc := runs.Descriptor{ID: id, Attempt: 1, CreatedAt: created, Cwd: f.repo, Mode: "full", Plan: plan,
		Branch: branch, Origin: runs.Origin{Kind: runs.OriginCLI}}
	require.NoError(t, runs.SaveDescriptor(dir, &desc))
	require.NoError(t, runs.SaveOutcome(dir, &runs.Outcome{RunID: id, Attempt: 1, Status: runs.StatusSucceeded,
		WorktreeKept: kept}))
}

// seedSucceededOthers records n succeeded runs of another plan, oldest first, each created a minute
// after the previous one starting at from, and returns their ids in that order.
func (f *detachLaunchFixture) seedSucceededOthers(t *testing.T, n int, from time.Time) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		created := from.Add(time.Duration(i) * time.Minute)
		id := "other-" + created.Format("20060102-150405") + "-abcd"
		f.seedSucceededRun(t, id, "docs/plans/other.md", "other", created, "")
		ids = append(ids, id)
	}
	return ids
}

func TestDetachLaunchPrunesSucceededRuns(t *testing.T) {
	epoch := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	t.Run("a new run prunes succeeded runs beyond 20 and keeps a kept worktree's run", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		const keptID = "feature-20261001-110000-abcd"
		kept := filepath.Join(f.repo, ".loopai", "worktrees", "feature")
		require.NoError(t, os.MkdirAll(kept, 0o750))
		// the oldest run of all, so the first pruned if a kept worktree did not keep it
		f.seedSucceededRun(t, keptID, detachLaunchPlan, "feature", epoch.Add(-time.Hour), kept)
		others := f.seedSucceededOthers(t, detachKeepSucceeded+1, epoch)

		stdout, stderr, err := f.launch(t, "--detach", "--branch", "elsewhere", detachLaunchPlan)
		require.NoError(t, err, stderr)
		assert.Contains(t, stdout, "run: ")
		assert.NotContains(t, stderr, "warning:")

		assert.NoDirExists(t, runs.Dir(f.repo, others[0]), "the oldest succeeded run beyond 20 is pruned")
		for _, id := range others[1:] {
			assert.DirExists(t, runs.Dir(f.repo, id), "one of the newest 20 succeeded runs: %s", id)
		}
		assert.DirExists(t, runs.Dir(f.repo, keptID), "a succeeded run whose worktree is kept is never pruned")
		assert.Len(t, f.runDirs(), detachKeepSucceeded+2, "20 succeeded runs, the kept one, and the new run")

		_, _, err = f.launch(t, "--detach", detachLaunchPlan)
		require.ErrorContains(t, err, "run "+keptID+" of "+detachLaunchPlan+" succeeded but kept its worktree "+kept,
			"the retained run still stops a launch from resuming the kept worktree")
	})

	t.Run("a failed prune only warns", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		f := newDetachLaunchFixture(t)
		others := f.seedSucceededOthers(t, detachKeepSucceeded+1, epoch)
		// a directory the prune cannot empty
		locked := filepath.Join(runs.Dir(f.repo, others[0]), "locked")
		require.NoError(t, os.Mkdir(locked, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(locked, "file"), nil, 0o600))
		require.NoError(t, os.Chmod(locked, 0o500))       //nolint:gosec // a directory without write permission
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // restore permissions for cleanup

		stdout, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		require.NoError(t, err, stderr)
		assert.Contains(t, stdout, "run: ", "the launch proceeds")
		assert.Contains(t, stderr, "warning: prune run "+others[0]+": ")
		assert.Contains(t, stderr, "permission denied")
		assert.Equal(t, 1, strings.Count(stderr, "warning:"), "one warning")
		assert.DirExists(t, locked)
	})
}

func TestDetachLaunchRefusals(t *testing.T) {
	t.Run("a live run", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		const id = detachSeedID
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		release, err := runs.AcquireBounded(filepath.Join(dir, runs.LockFile), time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = release() })

		_, _, err = f.launch(t, "--detach", detachLaunchPlan)
		require.EqualError(t, err, "run "+id+" of "+detachLaunchPlan+" is already running; follow it with loopai --attach "+id)
		assert.Len(t, f.runDirs(), 1)
	})

	t.Run("a missing plan", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		_, _, err := f.launch(t, "--detach", "docs/plans/missing.md")
		require.EqualError(t, err, "plan file not found: docs/plans/missing.md")
		assert.NoDirExists(t, filepath.Join(f.repo, ".loopai"))
	})

	t.Run("an executable that is gone", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		detachExecutable = func() (string, error) { return "/nonexistent/loopai (deleted)", nil }
		t.Cleanup(func() { detachExecutable = os.Executable })
		_, _, err := f.launch(t, "--detach", detachLaunchPlan)
		require.EqualError(t, err, "executable not found: /nonexistent/loopai (deleted)")
	})

	t.Run("outside the repository root", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Chdir(filepath.Join(f.repo, "docs"))
		_, _, err := f.launch(t, "--detach", "plans/20261022-feature.md")
		require.ErrorContains(t, err, "must run from repository root")
		assert.NoDirExists(t, filepath.Join(f.repo, ".loopai"))
	})

	t.Run("no run directory before the ignore rules are written", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(f.repo, ".loopai"), []byte("not a directory"), 0o600))
		_, _, err := f.launch(t, "--detach", detachLaunchPlan)
		require.ErrorContains(t, err, "prepare .loopai: ")
		assert.NoFileExists(t, runs.RunsDir(f.repo))
		assert.NoDirExists(t, runs.RunsDir(f.repo))
	})

	t.Run("a broken run directory is named with its fix", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		broken := runs.Dir(f.repo, "other-20261022-120000-abcd")
		require.NoError(t, os.MkdirAll(broken, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(broken, runs.DescriptorFile), []byte("{"), 0o600))

		_, _, err := f.launch(t, "--detach", detachLaunchPlan)
		require.ErrorContains(t, err, "run other-20261022-120000-abcd cannot be read: ")
		require.ErrorContains(t, err, "check that no loopai process still uses "+broken+", then remove it")
		assert.Len(t, f.runDirs(), 1, "no run is created")
	})
}

func TestDetachLaunchConcurrent(t *testing.T) {
	f := newDetachLaunchFixture(t)
	args := []string{"--detach", detachLaunchPlan}
	o := parseTestOpts(t, args...)
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Go(func() {
			results[i] = runDetachCommand(t.Context(), o, &config.Config{},
				detachIO{args: args, stdout: io.Discard, stderr: io.Discard, colors: testColors()})
		})
	}
	wg.Wait()

	var started, refused int
	for _, err := range results {
		if err == nil {
			started++
			continue
		}
		refused++
		assert.Regexp(t, `is already running; follow it with loopai --attach|another launch of this plan is in progress`,
			err.Error())
	}
	assert.Equal(t, 1, started, "%v", results)
	assert.Equal(t, 1, refused, "%v", results)
	desc := f.onlyRun(t)
	assert.Equal(t, 1, desc.Attempt, "exactly one child")
}

func TestDetachLaunchNewIDCollision(t *testing.T) {
	f := newDetachLaunchFixture(t)
	const taken, free = "feature-20261022-120000-aaaa", "feature-20261022-120000-bbbb"
	require.NoError(t, os.MkdirAll(runs.Dir(f.repo, taken), 0o750))
	ids := []string{taken, free}
	newRunID = func(string, time.Time) string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	t.Cleanup(func() { newRunID = runs.NewID })

	stdout, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
	require.NoError(t, err, stderr)
	assert.Equal(t, "run: "+free+"\nfollow: loopai --attach "+free+"\n", stdout)
	entries, err := os.ReadDir(runs.Dir(f.repo, taken))
	require.NoError(t, err)
	assert.Empty(t, entries, "an existing directory is never reused")

	t.Run("gives up after a bounded number of ids", func(t *testing.T) {
		newRunID = func(string, time.Time) string { return taken }
		_, _, err := f.launch(t, "--detach", "--branch", "other", detachLaunchPlan)
		require.ErrorContains(t, err, "create run directory: 8 run ids drawn for "+detachLaunchPlan+" were all taken")
	})
}

func TestDetachLaunchChildFailures(t *testing.T) {
	t.Run("an error answer is reported", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv(detachStubEnv, "error")
		stdout, _, err := f.launch(t, "--detach", detachLaunchPlan)
		desc := f.onlyRun(t)
		require.EqualError(t, err, "detached run "+desc.ID+" did not start: x")
		assert.Empty(t, stdout)
	})

	t.Run("an exit without an answer shows the console log", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		// an earlier attempt's output is not part of the tail
		const id = detachSeedID
		dir := f.seedRun(t, runs.Origin{Kind: runs.OriginCLI}, nil)
		require.NoError(t, os.WriteFile(filepath.Join(dir, runs.ConsoleLogFile), []byte("an earlier attempt\n"), 0o600))
		t.Setenv(detachStubEnv, "exit")

		_, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
		consoleLog := filepath.Join(dir, runs.ConsoleLogFile)
		require.EqualError(t, err, "detached run "+id+" exited before it started; console log: "+consoleLog)
		assert.Contains(t, stderr, "the end of console log "+consoleLog+":\n")
		assert.Contains(t, stderr, "  === loopai run "+id+", attempt 2, started ")
		assert.Contains(t, stderr, "  stub: exiting without an answer\n")
		assert.NotContains(t, stderr, "an earlier attempt")
	})

	t.Run("no answer in time leaves the child running", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv(detachStubEnv, "hang")
		detachAckTimeout = 2 * time.Second
		t.Cleanup(func() { detachAckTimeout = 120 * time.Second })

		_, _, err := f.launch(t, "--detach", detachLaunchPlan)
		desc := f.onlyRun(t)
		require.ErrorContains(t, err, "detached run "+desc.ID+": no answer within 2s; it may still be starting: "+
			"follow it with loopai --attach "+desc.ID+" or read its console log "+desc.ConsoleLog)
		lock := filepath.Join(runs.Dir(f.repo, desc.ID), runs.LockFile)
		assert.Eventually(t, func() bool { held, _ := runs.Probe(lock); return held }, detachChildWait,
			20*time.Millisecond, "the child is not killed and takes the run")
	})

	t.Run("a canceled launcher leaves the child running", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv(detachStubEnv, "hang")
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			// cancel once the child holds the run
			for range 600 {
				if dirs := f.runDirs(); len(dirs) == 1 {
					if held, _ := runs.Probe(filepath.Join(dirs[0], runs.LockFile)); held {
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
			cancel()
		}()
		defer cancel()

		_, _, err := f.launchContext(ctx, t, "--detach", detachLaunchPlan)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "stopped waiting for its answer")
		held, probeErr := runs.Probe(filepath.Join(f.runDirs()[0], runs.LockFile))
		require.NoError(t, probeErr)
		assert.True(t, held, "the child is not killed")
	})
}

func TestDetachLaunchScope(t *testing.T) {
	tests := []struct {
		name        string
		prefix      func(t *testing.T) []string
		mode        string
		wantWarning string // part of the expected retry warning; empty when no retry may happen
		wantErr     string
	}{
		{name: "a scope that cannot start is retried without it", mode: "ok",
			prefix: func(t *testing.T) []string {
				return []string{filepath.Join(t.TempDir(), "systemd-run"), "--user", "--scope", "--"}
			},
			wantWarning: "warning: the run did not start in a systemd user scope (start the detached child: "},
		{name: "a scope that exits without an answer is retried without it", mode: "ok",
			prefix: func(*testing.T) []string { return []string{"/bin/sh", "-c", "exit 1", "scope"} },
			wantWarning: "warning: the run did not start in a systemd user scope (it exited before answering); " +
				"starting it without one\n"},
		{name: "a scope that execs the child keeps the handshake descriptors", mode: "ok",
			prefix: func(*testing.T) []string { return []string{"/bin/sh", "-c", `exec "$@"`, "scope"} }},
		{name: "an answered failure is not retried", mode: "error",
			prefix:  func(*testing.T) []string { return []string{"/bin/sh", "-c", `exec "$@"`, "scope"} },
			wantErr: "did not start: x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newDetachLaunchFixture(t)
			t.Setenv(detachStubEnv, tc.mode)
			prefix := tc.prefix(t)
			detachScopePrefix = func() []string { return prefix }
			t.Cleanup(func() { detachScopePrefix = func() []string { return nil } })

			stdout, stderr, err := f.launch(t, "--detach", detachLaunchPlan)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err, stderr)
				assert.Contains(t, stdout, "run: ")
			}
			if tc.wantWarning == "" {
				assert.NotContains(t, stderr, "warning:")
				return
			}
			assert.Contains(t, stderr, tc.wantWarning)
			assert.Equal(t, 1, strings.Count(stderr, "warning:"), "one warning")
		})
	}
}
