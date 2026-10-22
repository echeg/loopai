//go:build !windows

package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/runs"
)

const (
	// detachChildMainEnv makes the test binary run main() with the unit-separator-delimited
	// arguments it holds, as the launcher starts the detached child.
	detachChildMainEnv = "LOOPAI_TEST_DETACH_CHILD_MAIN"
	// detachChildWait bounds every wait on a child; spawns are slow under the race-enabled suite.
	detachChildWait = 30 * time.Second
	// detachChildID is the run the fixture's descriptor records.
	detachChildID = "child-20261022-120000-abcd"
)

// detachChildFakeClaude completes every session. While $DETACH_CHILD_GATE is set it first waits, at
// most a minute, for that file to exist, so a test can hold the run inside its first session.
const detachChildFakeClaude = `#!/bin/sh
cat >/dev/null
if [ -n "$DETACH_CHILD_GATE" ]; then
  i=0
  while [ ! -e "$DETACH_CHILD_GATE" ] && [ "$i" -lt 1200 ]; do sleep 0.05; i=$((i+1)); done
fi
printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:ALL_TASKS_DONE>>>"}}'
printf '%s\n' '{"type":"result","result":""}'
`

// detachFDProbe appends "$probe fd3=<open|closed> fd4=<open|closed>" for the shell running it to
// $DETACH_CHILD_FDS. Duplicating a closed descriptor fails; the subshell keeps that from ending the
// script.
const detachFDProbe = `fd3=closed; ( : >&3 ) 2>/dev/null && fd3=open
fd4=closed; ( : >&4 ) 2>/dev/null && fd4=open
printf '%s fd3=%s fd4=%s\n' "$probe" "$fd3" "$fd4" >> "$DETACH_CHILD_FDS"
`

// detachChildGitProbe is a vcs_command that records the descriptors Git inherits, then runs Git.
const detachChildGitProbe = "#!/bin/sh\nprobe=git\n" + detachFDProbe + "exec git \"$@\"\n"

// TestDetachChildHelper is the re-exec target of detachChildFixture.command, not a test of its own.
func TestDetachChildHelper(t *testing.T) {
	raw, ok := os.LookupEnv(detachChildMainEnv)
	if !ok {
		t.Skip("helper process only")
	}
	os.Args = append([]string{"loopai"}, strings.Split(raw, "\x1f")...)
	main()
	t.Fatal("main returned without exiting")
}

// detachChildFixture is what a launcher leaves before it starts the child: a repository holding a
// committed, finished plan and an ignored .loopai, and a run directory whose descriptor names the
// plan. env is the child's environment: home and config redirected, the fake claude first on PATH.
type detachChildFixture struct {
	repo    string
	dir     string // the run directory
	console string // the run's console log
	config  string // the global config file
	gate    string // the file a gated fake claude waits for
	fds     string // where descriptor probes are recorded
	env     []string
}

// newDetachChildFixture prepares a run of docs/plans/child.md. gated makes every session wait for
// the gate file.
func newDetachChildFixture(t *testing.T, gated bool) *detachChildFixture {
	t.Helper()
	f := &detachChildFixture{
		repo: setupTestRepo(t),
		gate: filepath.Join(t.TempDir(), "gate"),
		fds:  filepath.Join(t.TempDir(), "fds"),
	}
	runGit(t, f.repo, "config", "core.autocrlf", "false")
	planPath := filepath.Join(f.repo, "docs", "plans", "child.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o750))
	require.NoError(t, os.WriteFile(planPath, []byte(detachedFinishedPlan), 0o600))
	runGit(t, f.repo, "add", "docs/plans/child.md")
	runGit(t, f.repo, "commit", "-m", "add plan")
	svc, err := git.NewService(f.repo, noopLogger())
	require.NoError(t, err)
	require.NoError(t, svc.EnsureLocalGitignore())

	home := t.TempDir()
	cfgDir := filepath.Join(home, "config")
	require.NoError(t, os.MkdirAll(cfgDir, 0o750))
	f.config = filepath.Join(cfgDir, "config")
	f.addConfig(t, "claude_swap_enabled = false", "codex_enabled = false", "keep_awake = false",
		"iteration_delay_ms = 1", "task_retry_count = 0", "report_enabled = false")
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "claude"), detachChildFakeClaude)

	f.dir = runs.Dir(f.repo, detachChildID)
	require.NoError(t, os.MkdirAll(f.dir, 0o750))
	f.console = filepath.Join(f.dir, runs.ConsoleLogFile)
	desc := runs.Descriptor{
		ID: detachChildID, Attempt: 1, Executable: os.Args[0], Argv: []string{"--tasks-only", "docs/plans/child.md"},
		Cwd: f.repo, Mode: string(processor.ModeTasksOnly), Plan: "docs/plans/child.md", ConsoleLog: f.console,
		Origin: runs.Origin{Kind: runs.OriginCLI},
	}
	require.NoError(t, runs.SaveDescriptor(f.dir, &desc))

	f.env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home,
		"LOOPAI_CONFIG_DIR="+cfgDir, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "DETACH_CHILD_FDS="+f.fds)
	if gated {
		f.env = append(f.env, "DETACH_CHILD_GATE="+f.gate)
	}
	return f
}

// addConfig appends lines to the global config file.
func (f *detachChildFixture) addConfig(t *testing.T, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(f.config, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString(strings.Join(lines, "\n") + "\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

// consoleLog returns the run's console log.
func (f *detachChildFixture) consoleLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.console)
	require.NoError(t, err)
	return string(data)
}

// probes returns the descriptor probe lines recorded so far.
func (f *detachChildFixture) probes(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.fds)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// descriptor returns the run's descriptor.
func (f *detachChildFixture) descriptor(t *testing.T) runs.Descriptor {
	t.Helper()
	desc, err := runs.LoadDescriptor(f.dir)
	require.NoError(t, err)
	return desc
}

// command returns the child for args after --detached-run, run in the repository with the fixture's
// environment and its output appended to the run's console log, as the launcher starts it.
func (f *detachChildFixture) command(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	console, err := os.OpenFile(f.console, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = console.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachChildHelper$")
	cmd.Dir = f.repo
	argv := append([]string{"--detached-run=" + detachChildID}, args...)
	cmd.Env = append(slices.Clone(f.env), detachChildMainEnv+"="+strings.Join(argv, "\x1f"))
	cmd.Stdout, cmd.Stderr = console, console
	return cmd
}

// detachChildProc is a started child with the launcher's ends of its handshake pipes.
type detachChildProc struct {
	cmd     *exec.Cmd
	secretW *os.File // the launcher's end of the secret pipe
	ackR    *os.File // the launcher's end of the ack pipe
	done    chan struct{}
	waitErr error
}

// spawn starts cmd and waits for it in the background. At cleanup the gate is opened and the child
// waited for, so no gated session outlives the test; it is killed only when it does not exit.
func (f *detachChildFixture) spawn(t *testing.T, cmd *exec.Cmd) *detachChildProc {
	t.Helper()
	require.NoError(t, cmd.Start())
	p := &detachChildProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(f.gate, nil, 0o600)
		select {
		case <-p.done:
		case <-time.After(detachChildWait):
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// start starts the child with the two handshake pipes as ExtraFiles, keeping only the launcher's
// ends, so EOF on the ack pipe can come from the child alone. The secret is not sent yet.
func (f *detachChildFixture) start(t *testing.T, args ...string) *detachChildProc {
	t.Helper()
	cmd := f.command(t, args...)
	secretR, secretW, err := os.Pipe()
	require.NoError(t, err)
	ackR, ackW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = secretW.Close()
		_ = ackR.Close()
	})
	cmd.ExtraFiles = []*os.File{secretR, ackW}
	p := f.spawn(t, cmd)
	require.NoError(t, secretR.Close())
	require.NoError(t, ackW.Close())
	p.secretW, p.ackR = secretW, ackR
	return p
}

// launch starts the child and sends the launcher's secret line, as a launcher without a T3 thread.
func (f *detachChildFixture) launch(t *testing.T, args ...string) *detachChildProc {
	t.Helper()
	p := f.start(t, args...)
	p.sendSecret(t)
	return p
}

// sendSecret writes the secret line and closes the launcher's end of the secret pipe.
func (p *detachChildProc) sendSecret(t *testing.T) {
	t.Helper()
	_, err := p.secretW.WriteString(`{"t3_token":""}` + "\n")
	require.NoError(t, err)
	require.NoError(t, p.secretW.Close())
}

// awaitAck reads the launcher's end of the ack pipe to EOF and returns what arrived.
func (p *detachChildProc) awaitAck(t *testing.T, f *detachChildFixture) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(p.ackR)
		got <- string(data)
	}()
	select {
	case ack := <-got:
		return ack
	case <-time.After(detachChildWait):
		t.Fatalf("no EOF on the ack pipe; console log:\n%s", f.consoleLog(t))
		return ""
	}
}

// wait waits for the child to exit and returns its exit status. A child ended by a signal fails.
func (p *detachChildProc) wait(t *testing.T, f *detachChildFixture) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(detachChildWait):
		t.Fatalf("the child did not exit; console log:\n%s", f.consoleLog(t))
	}
	if p.waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, p.waitErr, &exitErr)
	require.True(t, exitErr.Exited(), "the child exited by itself: %v", p.waitErr)
	return exitErr.ExitCode()
}

// alive reports whether the child is still running.
func (p *detachChildProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func TestDetachedChildStartsAttempt(t *testing.T) {
	f := newDetachChildFixture(t, true)
	wtPath := filepath.Join(f.repo, ".loopai", "worktrees", "child")
	p := f.launch(t, "--tasks-only", "docs/plans/child.md")

	ack := p.awaitAck(t, f)
	ackAt := time.Now()
	// the journal and descriptor as the launcher finds them on the answer; the gated session keeps
	// the run inside its first task meanwhile
	events, err := runs.ReadFrom(f.dir, 0)
	require.NoError(t, err)
	desc := f.descriptor(t)
	require.True(t, p.alive(), "EOF follows the explicit ack, not the child's exit; console log:\n%s", f.consoleLog(t))

	t.Run("the ack follows attempt_start and carries the run id", func(t *testing.T) {
		assert.JSONEq(t, `{"ok":true,"run_id":"`+detachChildID+`","attempt":1}`, ack)
		assert.Equal(t, 1, strings.Count(ack, "\n"), "exactly one newline-terminated line before EOF")
		assert.True(t, strings.HasSuffix(ack, "\n"))
		require.NotEmpty(t, events)
		start := events[0]
		assert.Equal(t, runs.EventAttemptStart, start.Type)
		assert.False(t, start.Time.After(ackAt), "attempt_start is recorded before the ack is sent")
		assert.Equal(t, 1, start.Attempt)
		assert.Equal(t, string(processor.ModeTasksOnly), start.Mode)
		assert.Equal(t, "child", start.Branch)
		assert.Equal(t, wtPath, start.Worktree)
		assert.Equal(t, filepath.Join(wtPath, "docs", "plans", "child.md"), start.WorktreePlan)
		assert.Equal(t, []runs.TaskInfo{{N: 1, Title: "Done"}}, start.Tasks)
		assert.Positive(t, start.ConsoleOffset, "the offset skips the banner the child printed first")
	})

	t.Run("the descriptor gains the process and the attempt's paths", func(t *testing.T) {
		pid := p.cmd.Process.Pid
		token, err := runs.ProcessStartToken(pid)
		require.NoError(t, err)
		assert.Equal(t, pid, desc.PID)
		assert.Equal(t, token, desc.PIDStart)
		assert.Equal(t, "child", desc.Branch)
		assert.Equal(t, wtPath, desc.Worktree)
		assert.Equal(t, filepath.Join(f.repo, ".loopai", "progress"), filepath.Dir(desc.ProgressLog))
		assert.FileExists(t, desc.ProgressLog)
	})

	t.Run("a hangup does not end the run", func(t *testing.T) {
		require.NoError(t, syscall.Kill(p.cmd.Process.Pid, syscall.SIGHUP))
		select {
		case <-p.done:
			t.Fatalf("the child ended on SIGHUP: %v", p.waitErr)
		case <-time.After(500 * time.Millisecond):
		}
	})

	t.Run("a successful run exits 0 without an error line", func(t *testing.T) {
		require.NoError(t, os.WriteFile(f.gate, nil, 0o600))
		assert.Equal(t, 0, p.wait(t, f))
		assert.NotRegexp(t, `(?m)^error: `, f.consoleLog(t))
	})
}

func TestDetachedChildDescriptorsNotInherited(t *testing.T) {
	f := newDetachChildFixture(t, false)
	gitProbe := filepath.Join(t.TempDir(), "git-probe")
	writeExecutable(t, gitProbe, detachChildGitProbe)
	f.addConfig(t, "vcs_command = "+gitProbe)

	// the probe sees descriptors a process did inherit
	r, w, err := os.Pipe()
	require.NoError(t, err)
	control := exec.Command("sh", "-c", "probe=control\n"+detachFDProbe)
	control.Env = append(os.Environ(), "DETACH_CHILD_FDS="+f.fds)
	control.ExtraFiles = []*os.File{r, w}
	require.NoError(t, control.Run())
	require.NoError(t, r.Close())
	require.NoError(t, w.Close())
	require.Equal(t, []string{"control fd3=open fd4=open"}, f.probes(t))

	p := f.launch(t, "--tasks-only", "docs/plans/child.md")
	ack := p.awaitAck(t, f)
	atAck := f.probes(t)
	assert.Contains(t, ack, `"ok":true`)
	assert.Equal(t, 0, p.wait(t, f))

	require.Greater(t, len(atAck), 1, "Git ran while the ack pipe was still open")
	for _, line := range f.probes(t)[1:] {
		assert.Equal(t, "git fd3=closed fd4=closed", line)
	}
}

func TestDetachedChildStartupFailure(t *testing.T) {
	tests := []struct {
		name   string
		config string // a global config line to add
		plan   string
		want   string // part of the reported error
	}{
		{name: "missing plan", plan: "docs/plans/missing.md", want: "plan file not found: docs/plans/missing.md"},
		{name: "broken config", config: "finalize_enabled = true", plan: "docs/plans/child.md", want: "finalize_enabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newDetachChildFixture(t, false)
			if tc.config != "" {
				f.addConfig(t, tc.config)
			}
			p := f.launch(t, "--tasks-only", tc.plan)

			ack := p.awaitAck(t, f)
			var msg detachAckMessage
			require.NoError(t, json.Unmarshal([]byte(ack), &msg), ack)
			assert.False(t, msg.OK)
			assert.Contains(t, msg.Error, tc.want)
			assert.NotContains(t, ack, "run_id")
			assert.Equal(t, 1, p.wait(t, f))

			outcome, err := runs.LoadOutcome(f.dir)
			require.NoError(t, err)
			assert.Equal(t, detachChildID, outcome.RunID)
			assert.Equal(t, 1, outcome.Attempt)
			assert.Equal(t, runs.StatusFailed, outcome.Status)
			assert.Equal(t, "startup: "+msg.Error, outcome.Reason)
			assert.Equal(t, 1, outcome.ExitCode)
			assert.Contains(t, f.consoleLog(t), "error: startup: "+msg.Error)
			assert.Positive(t, f.descriptor(t).PID, "the child had taken the run")
			status, err := runs.Classify(f.repo, f.dir)
			require.NoError(t, err)
			assert.Equal(t, runs.StatusFailed, status)
		})
	}
}

func TestDetachedChildAlreadyRunning(t *testing.T) {
	f := newDetachChildFixture(t, false)
	release, err := runs.AcquireBounded(filepath.Join(f.dir, runs.LockFile), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })

	p := f.launch(t, "--tasks-only", "docs/plans/child.md")
	ack := p.awaitAck(t, f)
	assert.JSONEq(t, `{"ok":false,"error":"run `+detachChildID+` is already running"}`, ack)
	assert.True(t, strings.HasSuffix(ack, "\n"))
	assert.Equal(t, 1, p.wait(t, f))

	assert.NoFileExists(t, filepath.Join(f.dir, runs.OutcomeFile), "a refused child records no outcome")
	assert.Zero(t, f.descriptor(t).PID, "the descriptor still belongs to the live attempt")
	assert.NoDirExists(t, filepath.Join(f.repo, ".loopai", "worktrees", "child"))
	assert.Contains(t, f.consoleLog(t), "error: run "+detachChildID+" is already running")
}

func TestDetachedChildOutlivesItsLauncher(t *testing.T) {
	f := newDetachChildFixture(t, false)
	p := f.start(t, "--tasks-only", "docs/plans/child.md")
	// the launcher stopped waiting before the child could answer
	require.NoError(t, p.ackR.Close())
	p.sendSecret(t)

	assert.Equal(t, 0, p.wait(t, f), "an answer nobody reads does not end the run")
	assert.Contains(t, f.consoleLog(t), "warning: run "+detachChildID+" started, but its launcher could not be told")
	events, err := runs.ReadFrom(f.dir, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Equal(t, runs.EventAttemptStart, events[0].Type)
}

func TestDetachedChildRequiresHandshake(t *testing.T) {
	f := newDetachChildFixture(t, false)
	p := f.spawn(t, f.command(t, "--tasks-only", "docs/plans/child.md"))

	assert.Equal(t, 1, p.wait(t, f))
	assert.Contains(t, f.consoleLog(t), "error: --detached-run is started only by loopai --detach: handshake descriptor 3")
	assert.Zero(t, f.descriptor(t).PID, "the run was never taken")
	assert.NoFileExists(t, filepath.Join(f.dir, runs.OutcomeFile))
}

func TestTakeHandshakePipes(t *testing.T) {
	t.Run("marks inherited descriptors close-on-exec", func(t *testing.T) {
		secretR, secretW, err := os.Pipe()
		require.NoError(t, err)
		defer secretR.Close()
		ackR, ackW, err := os.Pipe()
		require.NoError(t, err)
		defer ackR.Close()
		defer ackW.Close()

		// a process started while the duplicates lack FD_CLOEXEC would hold the ack pipe open
		var secretFD, ackFD int
		var secret, ack *os.File
		func() {
			syscall.ForkLock.RLock()
			defer syscall.ForkLock.RUnlock()
			secretFD, ackFD = inheritedFD(t, secretR), inheritedFD(t, ackW)
			secret, ack, err = takeHandshakePipes(secretFD, ackFD)
		}()
		require.NoError(t, err)
		assert.True(t, closeOnExec(t, secretFD))
		assert.True(t, closeOnExec(t, ackFD))

		// the files are the pipes: the launcher's line reads back, the answer reaches the launcher
		_, err = secretW.WriteString("secret\n")
		require.NoError(t, err)
		require.NoError(t, secretW.Close())
		got, err := io.ReadAll(secret)
		require.NoError(t, err)
		assert.Equal(t, "secret\n", string(got))
		require.NoError(t, secret.Close())
		_, err = ack.WriteString("ack\n")
		require.NoError(t, err)
		require.NoError(t, ack.Close())
		require.NoError(t, ackW.Close())
		got, err = io.ReadAll(ackR)
		require.NoError(t, err)
		assert.Equal(t, "ack\n", string(got))
	})

	t.Run("refuses a descriptor that is not a pipe and leaves it open", func(t *testing.T) {
		file, err := os.Create(filepath.Join(t.TempDir(), "plain"))
		require.NoError(t, err)
		defer file.Close()
		fd := inheritedFD(t, file)
		defer syscall.Close(fd)

		_, _, err = takeHandshakePipes(fd, fd)
		require.ErrorContains(t, err, "--detached-run is started only by loopai --detach: handshake descriptor")
		require.ErrorContains(t, err, "is not a pipe")
		var st syscall.Stat_t
		require.NoError(t, syscall.Fstat(fd, &st), "a refused descriptor is not closed")
	})

	t.Run("refuses a pipe the process opened itself and leaves it working", func(t *testing.T) {
		// the Go runtime's own signal pipe on Darwin looks like this on fds 3 and 4
		r, w, err := os.Pipe()
		require.NoError(t, err)
		defer r.Close()
		defer w.Close()

		_, _, err = takeHandshakePipes(int(r.Fd()), int(w.Fd()))
		require.ErrorContains(t, err, "was not inherited")
		_, err = w.WriteString("still open\n")
		require.NoError(t, err)
		require.NoError(t, w.Close())
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		assert.Equal(t, "still open\n", string(got))
	})

	t.Run("refuses an ack descriptor without wrapping the secret one", func(t *testing.T) {
		secretR, secretW, err := os.Pipe()
		require.NoError(t, err)
		defer secretR.Close()
		defer secretW.Close()
		file, err := os.Create(filepath.Join(t.TempDir(), "plain"))
		require.NoError(t, err)
		defer file.Close()
		var secretFD, fileFD int
		func() {
			syscall.ForkLock.RLock()
			defer syscall.ForkLock.RUnlock()
			secretFD, fileFD = inheritedFD(t, secretR), inheritedFD(t, file)
			_, _, err = takeHandshakePipes(secretFD, fileFD)
		}()
		defer syscall.Close(secretFD)
		defer syscall.Close(fileFD)

		require.ErrorContains(t, err, "is not a pipe")
		assert.False(t, closeOnExec(t, secretFD), "nothing is marked until both descriptors pass")
		var st syscall.Stat_t
		require.NoError(t, syscall.Fstat(secretFD, &st), "the secret descriptor is left open")
	})
}

// inheritedFD returns a duplicate of f's descriptor, which carries no FD_CLOEXEC, as a descriptor
// passed through ExtraFiles arrives in the child.
func inheritedFD(t *testing.T, f *os.File) int {
	t.Helper()
	fd, err := syscall.Dup(int(f.Fd()))
	require.NoError(t, err)
	require.False(t, closeOnExec(t, fd), "a duplicate starts without FD_CLOEXEC")
	return fd
}

// closeOnExec reports whether descriptor fd has FD_CLOEXEC set.
func closeOnExec(t *testing.T, fd int) bool {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	require.NoError(t, err)
	return flags&unix.FD_CLOEXEC != 0
}
