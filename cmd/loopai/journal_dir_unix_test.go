//go:build !windows

package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
)

// newForegroundFixture prepares a foreground run of docs/plans/child.md with --journal-dir: the
// repository, config, and fake claude of the detached fixture, with dir a journal directory outside
// the repository that does not exist yet, and console the file the run's output goes to. No run
// directory and no descriptor are written. gated makes every session wait for the gate file.
func newForegroundFixture(t *testing.T, gated bool) *detachChildFixture {
	t.Helper()
	f := newChildRepoFixture(t, gated)
	f.dir = filepath.Join(t.TempDir(), "journal")
	f.console = filepath.Join(t.TempDir(), "output.log")
	return f
}

// foreground returns the foreground run for args, run in the repository with the fixture's
// environment and its output appended to the fixture's console file. Unlike command it passes no
// --detached-run and no handshake files, as a host that spawns loopai as a plain child.
func (f *detachChildFixture) foreground(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	console, err := os.OpenFile(f.console, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = console.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachChildHelper$")
	cmd.Dir = f.repo
	cmd.Env = append(slices.Clone(f.env), detachChildMainEnv+"="+strings.Join(args, "\x1f"), foregroundMainEnv+"=1")
	cmd.Stdout, cmd.Stderr = console, console
	return cmd
}

// runForeground runs the foreground run for args to its end and returns its exit status.
func (f *detachChildFixture) runForeground(t *testing.T, args ...string) int {
	t.Helper()
	return f.spawn(t, f.foreground(t, args...)).wait(t, f)
}

// assertChildUsage checks that ev is the usage event of a session of the fake claude run with
// $DETACH_CHILD_USAGE: its result record's tokens, cost, and single modelUsage key.
func assertChildUsage(t *testing.T, ev runs.Event) {
	t.Helper()
	assert.Equal(t, runs.EventUsage, ev.Type)
	assert.Equal(t, "claude", ev.Provider)
	assert.Equal(t, "claude-fake-1", ev.Model)
	assert.Equal(t, int64(120), ev.InputTokens)
	assert.Equal(t, int64(30), ev.CachedInputTokens)
	assert.Equal(t, int64(10), ev.CacheWriteInputTokens)
	assert.Equal(t, int64(45), ev.OutputTokens)
	require.NotNil(t, ev.CostUSD)
	assert.InDelta(t, 0.25, *ev.CostUSD, 1e-9)
}

// journalFilesUnder returns every events.jsonl and outcome.json below root.
func journalFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (d.Name() == runs.JournalFile || d.Name() == runs.OutcomeFile) {
			found = append(found, path)
		}
		return nil
	})
	require.NoError(t, err)
	return found
}

func TestForegroundJournalSucceeds(t *testing.T) {
	f := newForegroundFixture(t, false)
	f.addLocalConfig(t, "report_enabled = true")
	// an in-place run branches off a clean checkout, so the local config and the open plan the task
	// session ticks are committed
	require.NoError(t, os.WriteFile(f.planPath(), []byte(detachedOpenPlan), 0o600))
	runGit(t, f.repo, "add", "-f", ".loopai/config", "docs/plans/child.md")
	runGit(t, f.repo, "commit", "-m", "enable the report, open the plan")
	f.env = append(f.env, "DETACH_CHILD_USAGE=1", "DETACH_CHILD_TICK=1")

	require.Equal(t, 0, f.runForeground(t, "--journal-dir="+f.dir, "docs/plans/child.md"), f.consoleLog(t))
	console := f.consoleLog(t)
	assert.NotRegexp(t, `(?m)^error: `, console)
	assert.Equal(t, "child", currentGitBranch(t, f.repo), "the run executed in the checkout, on the plan's branch")
	assert.NoDirExists(t, f.worktreePath(), "no worktree is created")
	assert.NoDirExists(t, filepath.Join(f.repo, ".loopai", "progress", "runs"), "the run is not registered")

	t.Run("the journal holds the run's events in order", func(t *testing.T) {
		events := f.events(t)
		assert.Equal(t, []runs.EventType{
			runs.EventAttemptStart,
			runs.EventPhase, runs.EventTask, runs.EventTasks, runs.EventUsage, // the task session
			runs.EventPhase, runs.EventTasks, // the review phase re-reads the ticked plan
			runs.EventReview, runs.EventUsage, runs.EventReview, runs.EventUsage, // two review sessions
			runs.EventPhase, runs.EventUsage, // the report session
			runs.EventFinalize, runs.EventReport, runs.EventFinish,
		}, eventTypes(events), "console:\n%s", console)

		start := events[0]
		assert.Equal(t, 1, start.Attempt)
		assert.Equal(t, f.planPath(), start.Plan)
		assert.Equal(t, f.planPath(), start.WorktreePlan)
		assert.Empty(t, start.Worktree)
		assert.Equal(t, "child", start.Branch)
		assert.True(t, filepath.IsAbs(start.ProgressLog), start.ProgressLog)
		assert.FileExists(t, start.ProgressLog)

		assert.Equal(t, "Do", events[2].Title, "the task title is read from the plan in the checkout")
		usagePhases := []status.Phase{}
		for _, ev := range events {
			if ev.Type == runs.EventUsage {
				assertChildUsage(t, ev)
				usagePhases = append(usagePhases, ev.Phase)
			}
		}
		assert.Equal(t, []status.Phase{status.PhaseTask, status.PhaseReview, status.PhaseReview, status.PhaseReport},
			usagePhases)

		finish := events[len(events)-1]
		assert.Equal(t, runs.StatusSucceeded, finish.Status)
		assert.Zero(t, finish.ExitCode)
		assert.Equal(t, "Delivered the child plan.", events[len(events)-2].Summary)
	})

	t.Run("outcome.json records the success without a run id", func(t *testing.T) {
		rec := f.outcome(t)
		assert.Empty(t, rec.RunID)
		assert.Equal(t, 1, rec.Attempt)
		assert.Equal(t, runs.StatusSucceeded, rec.Status)
		assert.Empty(t, rec.Reason)
		assert.Zero(t, rec.ExitCode)
		assert.Equal(t, "child", rec.Branch)
		assert.Equal(t, gitOutput(t, f.repo, "rev-parse", "refs/heads/child"), rec.Commit+"\n")
		assert.True(t, strings.HasPrefix(rec.Report, "# Report: Child\n"), rec.Report)
		assert.Equal(t, "Delivered the child plan.", rec.ReportSummary)
		assert.Equal(t, "low", rec.Risk)
		assert.True(t, rec.PlanMoved)
		assert.Empty(t, rec.WorktreeKept)
		assert.False(t, rec.FinishedAt.IsZero())
	})

	t.Run("only the two journal files are written to the directory", func(t *testing.T) {
		entries, err := os.ReadDir(f.dir)
		require.NoError(t, err)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.ElementsMatch(t, []string{runs.JournalFile, runs.OutcomeFile}, names)
	})
}

func TestForegroundJournalTasksOnlyFailed(t *testing.T) {
	f := newForegroundFixture(t, false)
	f.env = append(f.env, "DETACH_CHILD_MODE=fail")
	planPath := f.planPath()
	require.NoError(t, os.WriteFile(planPath, []byte(detachedOpenPlan), 0o600))
	runGit(t, f.repo, "commit", "-am", "open the plan")

	assert.Equal(t, 1, f.runForeground(t, "--journal-dir="+f.dir, "--tasks-only", "docs/plans/child.md"))
	assert.Regexp(t, `(?m)^error: runner: .*task execution failed`, f.consoleLog(t))

	rec := f.outcome(t)
	assert.Equal(t, runs.StatusFailed, rec.Status)
	assert.Equal(t, 1, rec.ExitCode)
	assert.Contains(t, rec.Reason, "task execution failed")
	assert.Empty(t, rec.RunID)
	assert.Empty(t, rec.WorktreeKept)
	assert.Empty(t, rec.Report)
	events := f.events(t)
	assert.Equal(t, runs.EventAttemptStart, events[0].Type)
	finish := events[len(events)-1]
	assert.Equal(t, runs.EventFinish, finish.Type)
	assert.Equal(t, runs.StatusFailed, finish.Status)
	assert.Equal(t, 1, finish.ExitCode)
}

func TestForegroundJournalStopped(t *testing.T) {
	f := newForegroundFixture(t, true)
	p := f.spawn(t, f.foreground(t, "--journal-dir="+f.dir, "--tasks-only", "docs/plans/child.md"))
	// the gated session holds the run inside its first task
	f.awaitSession(t)
	require.NoError(t, p.cmd.Process.Signal(syscall.SIGTERM))

	assert.Equal(t, 1, p.wait(t, f), "a stopped foreground run exits as an interrupted run always has")
	rec := f.outcome(t)
	assert.Equal(t, runs.StatusStopped, rec.Status)
	assert.Equal(t, 2, rec.ExitCode, "the outcome records the code a stopped detached run exits with")
	assert.NotEmpty(t, rec.Reason)
	assert.Empty(t, rec.WorktreeKept)
	events := f.events(t)
	finish := events[len(events)-1]
	assert.Equal(t, runs.EventFinish, finish.Type)
	assert.Equal(t, runs.StatusStopped, finish.Status)
	assert.Equal(t, 2, finish.ExitCode)
}

func TestForegroundJournalRefusesUsedDirectory(t *testing.T) {
	f := newForegroundFixture(t, false)
	dump := filepath.Join(t.TempDir(), "env-dump")
	f.env = append(f.env, "DETACH_CHILD_ENV_DUMP="+dump)
	require.NoError(t, os.MkdirAll(f.dir, 0o700))
	existing := filepath.Join(f.dir, runs.JournalFile)
	require.NoError(t, os.WriteFile(existing, []byte("{}\n"), 0o600))

	assert.Equal(t, 1, f.runForeground(t, "--journal-dir="+f.dir, "docs/plans/child.md"))
	assert.Regexp(t, `(?m)^error: --journal-dir .* already holds events\.jsonl`, f.consoleLog(t))
	assert.NoFileExists(t, dump, "no provider session started")
	assert.NoFileExists(t, filepath.Join(f.dir, runs.OutcomeFile))
	data, err := os.ReadFile(existing) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "{}\n", string(data), "the existing journal is left alone")
	assert.Equal(t, "master", currentGitBranch(t, f.repo), "no branch was prepared")
}

func TestForegroundWithoutJournalDir(t *testing.T) {
	codes := map[bool]int{}
	for _, withFlag := range []bool{true, false} {
		t.Run(map[bool]string{true: "with the flag", false: "without the flag"}[withFlag], func(t *testing.T) {
			f := newForegroundFixture(t, false)
			f.env = append(f.env, "DETACH_CHILD_USAGE=1")
			args := []string{"docs/plans/child.md"}
			if withFlag {
				args = append([]string{"--journal-dir=" + f.dir}, args...)
			}
			codes[withFlag] = f.runForeground(t, args...)
			require.Zero(t, codes[withFlag], f.consoleLog(t))
			found := journalFilesUnder(t, filepath.Dir(f.repo))
			if withFlag {
				assert.ElementsMatch(t, []string{filepath.Join(f.dir, runs.JournalFile),
					filepath.Join(f.dir, runs.OutcomeFile)}, found)
				return
			}
			// every directory of this subtest, the repository and the config home included, shares
			// the subtest's temporary root
			assert.Empty(t, found, "a run without the flag writes no journal")
			assert.NoDirExists(t, f.dir)
		})
	}
	assert.Equal(t, codes[true], codes[false], "the flag does not change the exit code")
	assert.Zero(t, codes[false])
}
