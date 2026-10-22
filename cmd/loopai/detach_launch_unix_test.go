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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/runs"
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
	// detachLaunchPlan is the plan every launcher test launches.
	detachLaunchPlan = "docs/plans/20261022-feature.md"
)

// detachStubSelector is the argv prefix that makes a re-executed test binary the stub child.
var detachStubSelector = []string{"-test.run=^TestDetachLaunchChildHelper$", "--"}

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
	detachChildArgs = detachStubSelector
	os.Args = append([]string{"loopai"}, strings.Split(raw, "\x1f")...)
	main()
	os.Exit(0) // main returns after a successful launch, as for every command that succeeds
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

	t.Run("inside a sandbox, before anything is created", func(t *testing.T) {
		f := newDetachLaunchFixture(t)
		t.Setenv("SANDBOX_RUNTIME", "1")

		_, _, err := f.launch(t, "--detach", detachLaunchPlan)
		require.ErrorContains(t, err, "refusing to start a detached run inside an agent sandbox (SANDBOX_RUNTIME=1)")
		require.ErrorContains(t, err, "loopai-t3 skill")
		assert.NoDirExists(t, filepath.Join(f.repo, ".loopai"))
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
