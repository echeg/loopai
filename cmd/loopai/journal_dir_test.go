package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/progress"
	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
)

// journalDirRepo creates a git repository whose .gitignore ignores ignored/, makes it the working
// directory, and returns its path.
func journalDirRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o600))
	t.Chdir(dir)
	return dir
}

func TestPrepareJournalDir(t *testing.T) {
	tests := []struct {
		name string
		// setup prepares the working directory and returns the --journal-dir argument and the
		// absolute path it must resolve to.
		setup   func(t *testing.T) (arg, want string)
		wantErr string
		// createdOnRefusal reports whether the directory exists after a refusal (it did before).
		createdOnRefusal bool
	}{
		{name: "relative path made absolute", setup: func(t *testing.T) (string, string) {
			cwd := t.TempDir()
			t.Chdir(cwd)
			return "journal", filepath.Join(cwd, "journal")
		}},
		{name: "missing nested directory created", setup: func(t *testing.T) (string, string) {
			dir := filepath.Join(t.TempDir(), "a", "b")
			return dir, dir
		}},
		{name: "existing empty directory accepted", setup: func(t *testing.T) (string, string) {
			dir := t.TempDir()
			return dir, dir
		}},
		{name: "existing events.jsonl refused", setup: func(t *testing.T) (string, string) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, runs.JournalFile), nil, 0o600))
			return dir, dir
		}, wantErr: "already holds events.jsonl", createdOnRefusal: true},
		{name: "existing outcome.json refused", setup: func(t *testing.T) (string, string) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, runs.OutcomeFile), []byte("{}\n"), 0o600))
			return dir, dir
		}, wantErr: "already holds outcome.json", createdOnRefusal: true},
		{name: "dangling events.jsonl symlink refused", setup: func(t *testing.T) (string, string) {
			dir := t.TempDir()
			require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, runs.JournalFile)))
			return dir, dir
		}, wantErr: "already holds events.jsonl", createdOnRefusal: true},
		{name: "regular file refused", setup: func(t *testing.T) (string, string) {
			path := filepath.Join(t.TempDir(), "file")
			require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
			return path, path
		}, wantErr: "is not a directory", createdOnRefusal: true},
		{name: "in-tree non-ignored directory refused", setup: func(t *testing.T) (string, string) {
			repo := journalDirRepo(t)
			return "journal", filepath.Join(repo, "journal")
		}, wantErr: "inside the repository work tree and git does not ignore events.jsonl"},
		{name: "in-tree non-ignored existing directory refused", setup: func(t *testing.T) (string, string) {
			repo := journalDirRepo(t)
			require.NoError(t, os.Mkdir(filepath.Join(repo, "journal"), 0o700))
			return filepath.Join(repo, "journal"), filepath.Join(repo, "journal")
		}, wantErr: "inside the repository work tree", createdOnRefusal: true},
		{name: "in-tree ignored directory accepted", setup: func(t *testing.T) (string, string) {
			repo := journalDirRepo(t)
			return filepath.Join("ignored", "run-1"), filepath.Join(repo, "ignored", "run-1")
		}},
		{name: "directory outside the repository accepted", setup: func(t *testing.T) (string, string) {
			journalDirRepo(t)
			dir := filepath.Join(t.TempDir(), "journal")
			return dir, dir
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arg, want := tc.setup(t)
			got, err := prepareJournalDir(arg)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, got)
				_, statErr := os.Stat(want)
				assert.Equal(t, tc.createdOnRefusal, statErr == nil, "a refusal must not create the directory")
				return
			}
			require.NoError(t, err)
			wantResolved, evalErr := filepath.EvalSymlinks(want)
			require.NoError(t, evalErr)
			gotResolved, evalErr := filepath.EvalSymlinks(got)
			require.NoError(t, evalErr)
			assert.True(t, filepath.IsAbs(got))
			assert.Equal(t, wantResolved, gotResolved)
			info, statErr := os.Stat(got)
			require.NoError(t, statErr)
			assert.True(t, info.IsDir())
			entries, readErr := os.ReadDir(got)
			require.NoError(t, readErr)
			assert.Empty(t, entries, "preparing the directory writes nothing into it")
		})
	}
}

func TestPrepareJournalDirCreatesWithMode0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	got, err := prepareJournalDir(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, got)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestPrepareJournalDirKeepsExistingMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	require.NoError(t, os.Mkdir(dir, 0o750))
	_, err := prepareJournalDir(dir)
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm())
}

func TestRunRefusesJournalDirBeforeConfig(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	journal := filepath.Join(t.TempDir(), "journal")
	require.NoError(t, os.Mkdir(journal, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(journal, runs.OutcomeFile), []byte("{}\n"), 0o600))
	configDir := filepath.Join(t.TempDir(), "config")

	err := run(t.Context(), opts{PlanFile: "p.md", JournalDir: journal, journalDirSet: true, ConfigDir: configDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already holds outcome.json")
	_, statErr := os.Stat(configDir)
	assert.True(t, os.IsNotExist(statErr), "the refusal comes before config loading installs defaults")
}

func TestAttachForegroundJournal(t *testing.T) {
	cfg := &config.Config{}
	base := executePlanRequest{PlanFile: "p.md", Mode: processor.ModeFull, WtCleanup: &cleanupHolder{}}

	t.Run("without the flag the request is unchanged", func(t *testing.T) {
		fj, req, err := attachForegroundJournal(opts{}, cfg, base)
		require.NoError(t, err)
		assert.Nil(t, fj)
		assert.Nil(t, req.Outcome)
		assert.Nil(t, req.LogDecorator)
		assert.Nil(t, req.PhaseObserver)
		assert.Nil(t, req.UsageHandler)
		assert.Nil(t, req.InPlaceAttemptStarted)
		assert.False(t, req.NonInteractive)
		assert.Same(t, base.WtCleanup, req.WtCleanup)
		// a nil journal completes as a no-op
		fj.complete(t.Context(), nil, errors.New("boom"))
	})

	t.Run("with the flag the hooks are set and the journal is created", func(t *testing.T) {
		dir := t.TempDir()
		fj, req, err := attachForegroundJournal(opts{JournalDir: dir}, cfg, base)
		require.NoError(t, err)
		require.NotNil(t, fj)
		t.Cleanup(func() { _ = fj.journal.Close() })
		assert.Same(t, fj.outcome, req.Outcome)
		assert.NotNil(t, req.LogDecorator)
		assert.NotNil(t, req.PhaseObserver)
		assert.NotNil(t, req.UsageHandler)
		assert.NotNil(t, req.InPlaceAttemptStarted)
		assert.Nil(t, req.AttemptStarted, "a worktree hook is never set: the run executes in place")
		assert.True(t, req.NonInteractive)
		assert.Same(t, base.WtCleanup, req.WtCleanup, "run()'s own cleanup holder is kept")
		assert.FileExists(t, filepath.Join(dir, runs.JournalFile))
	})

	t.Run("an existing journal is refused", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, runs.JournalFile), []byte("x\n"), 0o600))
		fj, _, err := attachForegroundJournal(opts{JournalDir: dir}, cfg, base)
		require.ErrorIs(t, err, os.ErrExist)
		assert.Nil(t, fj)
		data, readErr := os.ReadFile(filepath.Join(dir, runs.JournalFile)) //nolint:gosec // test temp path
		require.NoError(t, readErr)
		assert.Equal(t, "x\n", string(data), "the existing journal is left alone")
	})
}

func TestFireInPlaceAttemptStarted(t *testing.T) {
	repo := setupTestRepo(t)
	t.Chdir(repo)
	svc, err := git.NewService(repo, noopLogger())
	require.NoError(t, err)
	baseLog, err := progress.NewLogger(progress.Config{PlanFile: "docs/plans/p.md", Mode: string(processor.ModeFull),
		Branch: "master", NoColor: true, Stdout: &discardWriter{}}, testColors(), &status.PhaseHolder{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = baseLog.Close() })

	t.Run("no hook does nothing", func(t *testing.T) {
		require.NoError(t, fireInPlaceAttemptStarted(executePlanRequest{GitSvc: svc}, baseLog))
	})

	t.Run("the hook receives absolute paths and the checked-out branch", func(t *testing.T) {
		var got attemptStart
		req := executePlanRequest{PlanFile: "docs/plans/p.md", GitSvc: svc,
			InPlaceAttemptStarted: func(start attemptStart) error { got = start; return nil }}
		require.NoError(t, fireInPlaceAttemptStarted(req, baseLog))
		want := filepath.Join(repo, "docs", "plans", "p.md")
		assert.Equal(t, want, got.PlanFile)
		assert.Equal(t, want, got.MainPlanFile)
		assert.Equal(t, "master", got.Branch)
		assert.Empty(t, got.WorktreePath)
		assert.True(t, filepath.IsAbs(got.ProgressLog), got.ProgressLog)
		assert.Equal(t, absPath(baseLog.Path()), got.ProgressLog)
		assert.False(t, got.Resumed)
	})

	t.Run("a hook error is wrapped", func(t *testing.T) {
		req := executePlanRequest{PlanFile: "p.md", GitSvc: svc,
			InPlaceAttemptStarted: func(attemptStart) error { return errors.New("journal full") }}
		err := fireInPlaceAttemptStarted(req, baseLog)
		require.Error(t, err)
		assert.Equal(t, "start attempt: journal full", err.Error())
	})
}

// discardWriter is an io.Writer that drops everything.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// foregroundJournalRepo makes a repository with a one-task open plan the working directory and
// returns the repository, the plan's path, and a git service for it.
func foregroundJournalRepo(t *testing.T) (repo, planPath string, svc *git.Service) {
	t.Helper()
	repo = setupTestRepo(t)
	t.Chdir(repo)
	planPath = filepath.Join(repo, "docs", "plans", "fg.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o750))
	require.NoError(t, os.WriteFile(planPath, []byte("# Plan\n\n### Task 1: Build it\n\n- [ ] do it\n"), 0o600))
	svc, err := git.NewService(repo, noopLogger())
	require.NoError(t, err)
	return repo, planPath, svc
}

func TestExecutePlanRecordsForegroundJournal(t *testing.T) {
	repo, planPath, svc := foregroundJournalRepo(t)
	fakeClaude := filepath.Join(t.TempDir(), "fake-claude")
	writeExecutable(t, fakeClaude, `#!/bin/sh
cat >/dev/null
printf '# Plan\n\n### Task 1: Build it\n\n- [x] do it\n' > "$FG_JOURNAL_PLAN"
printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:ALL_TASKS_DONE>>>"}}'
printf '%s\n' '{"type":"result","result":"","usage":{"input_tokens":10,"output_tokens":5},"total_cost_usd":0.25}'
`)
	t.Setenv("FG_JOURNAL_PLAN", planPath)
	dir := t.TempDir()
	cfg := &config.Config{ClaudeCommand: fakeClaude}
	o := opts{TasksOnly: true, MaxIterations: 1, NoColor: true, JournalDir: dir}
	fj, req, err := attachForegroundJournal(o, cfg, executePlanRequest{
		PlanFile: planPath, Mode: processor.ModeTasksOnly, GitSvc: svc, Config: cfg, Colors: testColors(),
		BaseRef: "master",
	})
	require.NoError(t, err)

	runErr := executePlan(t.Context(), o, req)
	require.NoError(t, runErr)
	fj.complete(t.Context(), svc, runErr)

	events, err := runs.ReadFrom(dir, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)

	t.Run("attempt_start names the plan in place", func(t *testing.T) {
		start := events[0]
		require.Equal(t, runs.EventAttemptStart, start.Type)
		assert.Equal(t, 1, start.Attempt)
		assert.Equal(t, string(processor.ModeTasksOnly), start.Mode)
		assert.Equal(t, planPath, start.Plan)
		assert.Equal(t, planPath, start.WorktreePlan)
		assert.Empty(t, start.Worktree)
		assert.Zero(t, start.ConsoleOffset)
		assert.Equal(t, "master", start.Branch)
		assert.True(t, filepath.IsAbs(start.ProgressLog), start.ProgressLog)
		assert.FileExists(t, start.ProgressLog)
		assert.Equal(t, []runs.TaskInfo{{N: 1, Title: "Build it"}}, start.Tasks)
	})

	t.Run("task events carry the plan's titles", func(t *testing.T) {
		var titles []string
		for _, ev := range events {
			if ev.Type == runs.EventTask {
				titles = append(titles, ev.Title)
			}
		}
		require.NotEmpty(t, titles)
		assert.Equal(t, "Build it", titles[0])
	})

	t.Run("the session's usage is recorded", func(t *testing.T) {
		var usage []runs.Event
		for _, ev := range events {
			if ev.Type == runs.EventUsage {
				usage = append(usage, ev)
			}
		}
		require.Len(t, usage, 1)
		assert.Equal(t, "claude", usage[0].Provider)
		assert.Equal(t, int64(10), usage[0].InputTokens)
		assert.Equal(t, int64(5), usage[0].OutputTokens)
		require.NotNil(t, usage[0].CostUSD)
		assert.InDelta(t, 0.25, *usage[0].CostUSD, 1e-9)
	})

	t.Run("the outcome records the success without a run id", func(t *testing.T) {
		assert.Equal(t, runs.EventFinish, events[len(events)-1].Type)
		rec, err := runs.LoadOutcome(dir)
		require.NoError(t, err)
		assert.Equal(t, runs.StatusSucceeded, rec.Status)
		assert.Empty(t, rec.RunID)
		assert.Equal(t, 1, rec.Attempt)
		assert.Zero(t, rec.ExitCode)
		assert.Equal(t, "master", rec.Branch)
		assert.Equal(t, gitOutput(t, repo, "rev-parse", "HEAD"), rec.Commit+"\n")
		assert.Empty(t, rec.WorktreeKept)
		assert.False(t, rec.FinishedAt.IsZero())
		assert.NoFileExists(t, filepath.Join(dir, runs.ReportFile), "a foreground run writes no report.md")
	})
}

func TestExecutePlanInPlaceHookErrorStopsBeforeRunner(t *testing.T) {
	_, planPath, svc := foregroundJournalRepo(t)
	marker := filepath.Join(t.TempDir(), "provider-ran")
	fakeClaude := filepath.Join(t.TempDir(), "fake-claude")
	writeExecutable(t, fakeClaude, "#!/bin/sh\ntouch '"+marker+"'\ncat >/dev/null\n")
	cfg := &config.Config{ClaudeCommand: fakeClaude}
	outcome := &planExecutionOutcome{}

	err := executePlan(t.Context(), opts{TasksOnly: true, MaxIterations: 1, NoColor: true}, executePlanRequest{
		PlanFile: planPath, Mode: processor.ModeTasksOnly, GitSvc: svc, Config: cfg, Colors: testColors(),
		BaseRef: "master", Outcome: outcome, NonInteractive: true,
		InPlaceAttemptStarted: func(attemptStart) error { return errors.New("journal full") },
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start attempt: journal full")
	assert.NoFileExists(t, marker, "no provider process started")
	assert.False(t, outcome.succeeded)
	require.Error(t, outcome.failure)
}

func TestForegroundJournalComplete(t *testing.T) {
	cfg := &config.Config{}

	t.Run("a run that never started gets only a failed outcome", func(t *testing.T) {
		dir := t.TempDir()
		fj, _, err := attachForegroundJournal(opts{JournalDir: dir}, cfg, executePlanRequest{})
		require.NoError(t, err)
		stderr := captureStderr(t, func() { fj.complete(t.Context(), nil, errors.New("dirty tree")) })
		assert.Empty(t, stderr)

		rec, err := runs.LoadOutcome(dir)
		require.NoError(t, err)
		assert.Equal(t, runs.StatusFailed, rec.Status)
		assert.Equal(t, "startup: dirty tree", rec.Reason)
		assert.Equal(t, 1, rec.ExitCode)
		assert.Equal(t, 1, rec.Attempt)
		assert.Empty(t, rec.RunID)
		events, err := runs.ReadFrom(dir, 0)
		require.NoError(t, err)
		assert.Empty(t, events, "no attempt started, so there is nothing to close")
	})

	t.Run("a run that returned nothing before starting is still a failure", func(t *testing.T) {
		dir := t.TempDir()
		fj, _, err := attachForegroundJournal(opts{JournalDir: dir}, cfg, executePlanRequest{})
		require.NoError(t, err)
		fj.complete(t.Context(), nil, nil)
		rec, err := runs.LoadOutcome(dir)
		require.NoError(t, err)
		assert.Equal(t, runs.StatusFailed, rec.Status)
		assert.Equal(t, "startup: the run ended before its attempt started", rec.Reason)
	})

	t.Run("a canceled run is recorded as stopped", func(t *testing.T) {
		dir := t.TempDir()
		fj, req, err := attachForegroundJournal(opts{JournalDir: dir}, cfg, executePlanRequest{Mode: processor.ModeFull})
		require.NoError(t, err)
		require.NoError(t, req.InPlaceAttemptStarted(attemptStart{PlanFile: "/nowhere/p.md", MainPlanFile: "/nowhere/p.md"}))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		fj.complete(ctx, nil, errors.New("runner: interrupted"))

		rec, err := runs.LoadOutcome(dir)
		require.NoError(t, err)
		assert.Equal(t, runs.StatusStopped, rec.Status)
		assert.Equal(t, 2, rec.ExitCode)
		assert.Equal(t, "runner: interrupted", rec.Reason)
		events, err := runs.ReadFrom(dir, 0)
		require.NoError(t, err)
		require.NotEmpty(t, events)
		finish := events[len(events)-1]
		assert.Equal(t, runs.EventFinish, finish.Type)
		assert.Equal(t, runs.StatusStopped, finish.Status)
		assert.Equal(t, 2, finish.ExitCode)
	})

	t.Run("a failed outcome save is only a warning", func(t *testing.T) {
		dir := t.TempDir()
		fj, _, err := attachForegroundJournal(opts{JournalDir: dir}, cfg, executePlanRequest{})
		require.NoError(t, err)
		require.NoError(t, os.Mkdir(filepath.Join(dir, runs.OutcomeFile), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, runs.OutcomeFile, "x"), nil, 0o600))
		stderr := captureStderr(t, func() { fj.complete(t.Context(), nil, errors.New("boom")) })
		assert.Contains(t, stderr, "warning: record the attempt's outcome:")
	})
}
