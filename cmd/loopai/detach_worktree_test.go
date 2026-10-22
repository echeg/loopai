package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/progress"
	"github.com/umputun/ralphex/pkg/status"
)

// detachedFakeClaude answers each session by its prompt and by $DETACH_MODE: fail makes the task
// session leave leftover.txt and report TASK_FAILED, never gives it no signal at all, hang leaves
// wip.txt, touches $DETACH_MARKER and sleeps, and blocked makes the finalize session leave
// leftover.txt and report FINALIZE_BLOCKED. Every other session completes. Each session's kind is
// appended to $DETACH_SESSIONS.
const detachedFakeClaude = `#!/bin/sh
prompt=$(cat)
case "$prompt" in
*FINALIZE-PROMPT*) kind=finalize ;;
*TASK-PROMPT*) kind=task ;;
*) kind=review ;;
esac
printf '%s\n' "$kind" >> "$DETACH_SESSIONS"
case "$kind:$DETACH_MODE" in
task:fail) echo leftover > leftover.txt; signal='<<<RALPHEX:TASK_FAILED>>>' ;;
task:never) signal='' ;;
task:hang) echo wip > wip.txt; : > "$DETACH_MARKER"; sleep 30; signal='' ;;
task:*) signal='<<<RALPHEX:ALL_TASKS_DONE>>>' ;;
finalize:blocked) echo leftover > leftover.txt; signal='<<<RALPHEX:FINALIZE_BLOCKED>>>\nneeds a product decision' ;;
finalize:*) signal='<<<RALPHEX:FINALIZE_DONE>>>' ;;
*) signal='<<<RALPHEX:REVIEW_DONE>>>' ;;
esac
printf '{"type":"content_block_delta","delta":{"type":"text_delta","text":"%s"}}\n' "$signal"
printf '%s\n' '{"type":"result","result":""}'
`

const (
	detachedFinishedPlan = "# Detached\n\n### Task 1: Done\n- [x] complete\n"
	detachedOpenPlan     = "# Detached\n\n### Task 1: Do\n- [ ] do it\n"
)

// detachedRunCase selects what one non-interactive runWithWorktree call does.
type detachedRunCase struct {
	plan          string // plan body; empty is a finished plan
	mode          string // DETACH_MODE for the fake claude
	finalize      string
	keep, archive bool // KeepWorktreeOnFailure, ArchiveOnRunBranch
	report        bool
	noHook        bool                     // leave AttemptStarted unset
	hook          func(attemptStart) error // runs after the call is recorded; nil succeeds
	maxIterations int                      // 0 runs one iteration
	// before runs once the fixture exists and returns the context the run uses; nil uses t.Context()
	before func(t *testing.T, f finalizePRFixture, r *detachedRunResult) context.Context
}

// detachedRunResult is what the run left behind.
type detachedRunResult struct {
	f        finalizePRFixture
	err      error
	out      string // Out, where a non-interactive run writes its banner and stats
	stderr   string
	outcome  *planExecutionOutcome
	starts   []attemptStart
	hookCwd  []string
	wtPath   string
	planPath string
	preHead  string // invoking checkout's HEAD before the run
	preIndex string // invoking checkout's index before the run
	marker   string // $DETACH_MARKER
	sessLog  string // $DETACH_SESSIONS
}

// sessions lists the kinds of the executor sessions the run started.
func (r detachedRunResult) sessions(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.sessLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	return strings.Fields(string(data))
}

// runDetachedWorktree runs a plan through runWithWorktree non-interactively, as the detached child
// will, on the finalize fixture's feature branch whose origin master moved ahead.
func runDetachedWorktree(t *testing.T, tc detachedRunCase) detachedRunResult {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on windows
	plan := tc.plan
	if plan == "" {
		plan = detachedFinishedPlan
	}
	f := setupFinalizeFixture(t, map[string]string{"docs/plans/detached.md": plan})
	f.advanceOriginBase(t, "base.txt", "one\ntwo\nthree\n")
	t.Chdir(f.dir)
	res := detachedRunResult{
		f: f, outcome: &planExecutionOutcome{},
		wtPath:   filepath.Join(f.dir, ".loopai", "worktrees", "detached"),
		planPath: filepath.Join(f.dir, "docs", "plans", "detached.md"),
		preHead:  strings.TrimSpace(gitOutput(t, f.dir, "rev-parse", "HEAD")),
		preIndex: gitOutput(t, f.dir, "ls-files", "--stage"),
		marker:   filepath.Join(t.TempDir(), "session-started"),
		sessLog:  filepath.Join(t.TempDir(), "sessions"),
	}
	t.Setenv("DETACH_MODE", tc.mode)
	t.Setenv("DETACH_SESSIONS", res.sessLog)
	t.Setenv("DETACH_MARKER", res.marker)
	fakeClaude := filepath.Join(t.TempDir(), "fake-claude")
	writeExecutable(t, fakeClaude, detachedFakeClaude)
	ctx := t.Context()
	if tc.before != nil {
		ctx = tc.before(t, f, &res)
	}

	var out bytes.Buffer
	req := executePlanRequest{
		PlanFile: res.planPath, Mode: processor.ModeFull, GitSvc: f.svc, BaseRef: "master",
		Config: &config.Config{
			ClaudeCommand: fakeClaude, Finalize: tc.finalize, MovePlanOnCompletion: true, WorktreeEnabled: true,
			FinalizeMergeMethod: "squash", FinalizeChecksTimeout: time.Minute,
			TaskPrompt: "TASK-PROMPT", ReviewFirstPrompt: "REVIEW-PROMPT", ReviewSecondPrompt: "REVIEW-PROMPT",
			FinalizePrompt: "FINALIZE-PROMPT", ReportEnabled: tc.report,
			IterationDelayMs: 1, TaskRetryCount: 0, TaskRetryCountSet: true,
		},
		Colors: testColors(), WtCleanup: &cleanupHolder{}, Outcome: res.outcome,
		Out: &out, NonInteractive: true,
		KeepWorktreeOnFailure: tc.keep, ArchiveOnRunBranch: tc.archive,
	}
	if !tc.noHook {
		req.AttemptStarted = func(start attemptStart) error {
			res.starts = append(res.starts, start)
			cwd, err := os.Getwd()
			require.NoError(t, err)
			res.hookCwd = append(res.hookCwd, cwd)
			if tc.hook != nil {
				return tc.hook(start)
			}
			return nil
		}
	}
	maxIterations := tc.maxIterations
	if maxIterations == 0 {
		maxIterations = 1
	}
	res.stderr = captureStderr(t, func() {
		res.err = runWithWorktree(ctx, opts{MaxIterations: maxIterations, NoColor: true}, req)
	})
	res.out = out.String()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, f.dir, cwd, "runWithWorktree restores cwd")
	return res
}

// requireRunLockFree checks that a kept worktree's run lock was released, so the next launch can
// resume it.
func requireRunLockFree(t *testing.T, r detachedRunResult) {
	t.Helper()
	wtSvc, err := r.f.svc.OpenWorktree(r.wtPath)
	require.NoError(t, err)
	release, err := wtSvc.AcquireWorktreeRunLock()
	require.NoError(t, err, "a kept worktree must release its run lock")
	require.NoError(t, release())
}

// requireKept checks that the run left its worktree in place, said so, and recorded it.
func requireKept(t *testing.T, r detachedRunResult) {
	t.Helper()
	require.DirExists(t, r.wtPath)
	assert.Equal(t, r.wtPath, r.outcome.worktreeKept)
	assert.Contains(t, r.stderr, "worktree preserved for resume: "+r.wtPath)
	requireRunLockFree(t, r)
}

func TestRunWithWorktreeDetachedSuccess(t *testing.T) {
	t.Run("clean success archives on the run branch and removes the worktree", func(t *testing.T) {
		var wtExisted, logExisted bool
		r := runDetachedWorktree(t, detachedRunCase{
			finalize: config.FinalizePR, keep: true, archive: true, report: true,
			hook: func(start attemptStart) error {
				_, wtErr := os.Stat(start.WorktreePath)
				_, logErr := os.Stat(start.ProgressLog)
				wtExisted, logExisted = wtErr == nil, logErr == nil
				return nil
			},
		})
		require.NoError(t, r.err, r.out)
		require.True(t, r.outcome.succeeded)

		// the hook fires once, before the chdir, with absolute paths of the attempt
		require.Len(t, r.starts, 1)
		start := r.starts[0]
		assert.Equal(t, attemptStart{
			PlanFile:     filepath.Join(r.wtPath, "docs", "plans", "detached.md"),
			MainPlanFile: r.planPath, Branch: "detached", WorktreePath: r.wtPath,
			ProgressLog: start.ProgressLog,
		}, start)
		for _, path := range []string{start.PlanFile, start.MainPlanFile, start.WorktreePath, start.ProgressLog} {
			assert.True(t, filepath.IsAbs(path), "%q is absolute", path)
		}
		assert.Equal(t, filepath.Join(r.f.dir, ".loopai", "progress"), filepath.Dir(start.ProgressLog))
		assert.True(t, wtExisted, "the worktree exists when the hook fires")
		assert.True(t, logExisted, "the progress log exists when the hook fires")
		assert.Equal(t, []string{r.f.dir}, r.hookCwd, "the hook fires before the chdir into the worktree")

		// a clean success removes the worktree even under the knob
		assert.NoDirExists(t, r.wtPath)
		assert.Empty(t, r.outcome.worktreeKept)
		assert.NotContains(t, r.stderr, "worktree preserved")

		// the archive and its report land on the run branch, which the PR pushed
		runGit(t, r.f.dir, "cat-file", "-e", "refs/heads/detached:docs/plans/completed/detached.md")
		sidecar := gitOutput(t, r.f.dir, "show", "refs/heads/detached:docs/plans/completed/detached.report.md")
		assert.Equal(t, strings.TrimSpace(r.outcome.report), strings.TrimSpace(sidecar))
		assert.True(t, strings.HasPrefix(gitOutput(t, r.f.dir, "log", "-1", "--format=%s", "refs/heads/detached"),
			"move completed plan: detached.md"), "the run branch tip is the archive commit")
		runGit(t, r.f.remote, "cat-file", "-e", "refs/heads/detached:docs/plans/completed/detached.md")

		// the invoking checkout is untouched
		assert.Equal(t, r.preHead, strings.TrimSpace(gitOutput(t, r.f.dir, "rev-parse", "HEAD")))
		assert.Equal(t, r.preIndex, gitOutput(t, r.f.dir, "ls-files", "--stage"))
		assert.Empty(t, gitOutput(t, r.f.dir, "diff", "--cached", "--name-only"))
		assert.FileExists(t, r.planPath)
		assert.NoDirExists(t, filepath.Join(r.f.dir, "docs", "plans", "completed"))
		assert.Equal(t, "feature", currentGitBranch(t, r.f.dir))

		// the outcome carries the finalize, archive, and report results
		assert.Equal(t, config.FinalizePR, r.outcome.finalizeMode)
		assert.Equal(t, "PR opened", r.outcome.finalizeStatusNote)
		assert.Equal(t, "https://github.com/acme/repo/pull/7", r.outcome.prURL)
		assert.True(t, r.outcome.prOpened)
		assert.False(t, r.outcome.merged)
		require.NoError(t, r.outcome.finalizeIncomplete)
		assert.True(t, r.outcome.planMoved)
		assert.Empty(t, r.outcome.archiveIncomplete)
		assert.Contains(t, r.outcome.report, "## Summary")
		assert.Contains(t, r.out, "  PR: https://github.com/acme/repo/pull/7\n")
	})

	t.Run("knobs unset archive in the invoking checkout", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{finalize: config.FinalizeSync})
		require.NoError(t, r.err, r.out)
		require.True(t, r.outcome.succeeded)

		require.Len(t, r.starts, 1, "the hook alone still fires")
		assert.NoDirExists(t, r.wtPath)
		assert.FileExists(t, filepath.Join(r.f.dir, "docs", "plans", "completed", "detached.md"))
		assert.NoFileExists(t, r.planPath)
		assert.Equal(t, "move completed plan: detached.md",
			strings.TrimSpace(gitOutput(t, r.f.dir, "log", "-1", "--format=%s", "HEAD")))
		cmd := exec.Command("git", "cat-file", "-e", "refs/heads/detached:docs/plans/completed/detached.md")
		cmd.Dir = r.f.dir
		require.Error(t, cmd.Run(), "the run branch carries no archive")
		assert.Equal(t, "synced", r.outcome.finalizeStatusNote)
		assert.True(t, r.outcome.planMoved)
	})
}

func TestRunWithWorktreeDetachedKeepsWorktree(t *testing.T) {
	t.Run("failing task keeps the fresh worktree with its uncommitted file", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{plan: detachedOpenPlan, mode: "fail", keep: true, archive: true,
			maxIterations: 3})
		require.ErrorContains(t, r.err, "task execution failed")
		assert.False(t, r.outcome.succeeded)
		requireKept(t, r)
		assert.FileExists(t, filepath.Join(r.wtPath, "leftover.txt"))
		assert.FileExists(t, r.planPath, "a failed run never archives")
	})

	t.Run("task-phase iteration cap keeps the worktree", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{plan: detachedOpenPlan, mode: "never", keep: true, archive: true})
		require.ErrorContains(t, r.err, "max iterations (1) reached without completion")
		requireKept(t, r)
	})

	t.Run("canceled run keeps the worktree", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{plan: detachedOpenPlan, mode: "hang", keep: true, archive: true,
			before: func(t *testing.T, _ finalizePRFixture, r *detachedRunResult) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				cancelWhenPathExists(cancel, r.marker)
				return ctx
			}})
		require.ErrorIs(t, r.err, context.Canceled)
		requireKept(t, r)
		assert.FileExists(t, filepath.Join(r.wtPath, "wip.txt"))
	})

	t.Run("blocked finalize sync with an uncommitted file keeps a successful run's worktree", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{mode: "blocked", finalize: config.FinalizeSync, keep: true, archive: true})
		require.NoError(t, r.err, r.out)
		require.True(t, r.outcome.succeeded)
		require.ErrorContains(t, r.outcome.finalizeIncomplete, "base sync blocked")
		requireKept(t, r)
		assert.FileExists(t, filepath.Join(r.wtPath, "leftover.txt"))
		assert.Contains(t, r.out, "finalize incomplete: base sync blocked")
	})

	t.Run("finalize incomplete keeps a clean worktree", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{finalize: config.FinalizePR, keep: true, archive: true,
			before: func(t *testing.T, _ finalizePRFixture, _ *detachedRunResult) context.Context {
				// a PATH with what the fake claude and the assertions run, but no gh
				bin := t.TempDir()
				for _, tool := range []string{"cat", "git"} {
					path, err := exec.LookPath(tool)
					require.NoError(t, err)
					require.NoError(t, os.Symlink(path, filepath.Join(bin, tool)))
				}
				t.Setenv("PATH", bin)
				return t.Context()
			}})
		require.NoError(t, r.err, r.out)
		require.True(t, r.outcome.succeeded)
		require.ErrorContains(t, r.outcome.finalizeIncomplete, "requires GitHub CLI (gh) in PATH")
		assert.Equal(t, "finalize incomplete", r.outcome.finalizeStatusNote)
		requireKept(t, r)
		assert.Empty(t, gitOutput(t, r.wtPath, "status", "--porcelain"), "only the close-out keeps this worktree")
		assert.True(t, r.outcome.planMoved)
	})

	t.Run("rejected run-branch archive keeps a successful run's worktree", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{keep: true, archive: true,
			before: func(t *testing.T, f finalizePRFixture, _ *detachedRunResult) context.Context {
				hook := filepath.Join(f.dir, ".git", "hooks", "commit-msg")
				writeExecutable(t, hook, "#!/bin/sh\ncase \"$(cat \"$1\")\" in\n"+
					"'move completed plan'*) echo rejected by hook >&2; exit 1 ;;\nesac\nexit 0\n")
				return t.Context()
			}})
		require.NoError(t, r.err, "a rejected archive is incomplete, not a failure: %s", r.out)
		require.True(t, r.outcome.succeeded)
		assert.False(t, r.outcome.planMoved)
		assert.Contains(t, r.outcome.archiveIncomplete, "move detached.md")
		requireKept(t, r)
		assert.NotEmpty(t, gitOutput(t, r.wtPath, "status", "--porcelain"), "the plan move is left staged")
		assert.Equal(t, r.preHead, strings.TrimSpace(gitOutput(t, r.f.dir, "rev-parse", "HEAD")))
		assert.Contains(t, r.out, "plan archive incomplete: move detached.md")
	})

	t.Run("dirty worktree keeps a successful run's worktree", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{keep: true, archive: true,
			hook: func(start attemptStart) error {
				return os.WriteFile(filepath.Join(start.WorktreePath, "scratch.txt"), []byte("untracked\n"), 0o600)
			}})
		require.NoError(t, r.err, r.out)
		require.True(t, r.outcome.succeeded)
		require.NoError(t, r.outcome.finalizeIncomplete)
		assert.Empty(t, r.outcome.archiveIncomplete)
		assert.True(t, r.outcome.planMoved)
		requireKept(t, r)
		assert.FileExists(t, filepath.Join(r.wtPath, "scratch.txt"))
	})
}

func TestRunWithWorktreeDetachedHookAndDefaults(t *testing.T) {
	t.Run("hook error removes a fresh worktree before execution", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{keep: true, archive: true,
			hook: func(attemptStart) error { return errors.New("descriptor write failed") }})
		require.EqualError(t, r.err, "start attempt: descriptor write failed")
		assert.NoDirExists(t, r.wtPath)
		assert.Empty(t, r.sessions(t), "no session runs after a failed hook")
		assert.Empty(t, r.outcome.worktreeKept)
		assert.False(t, r.outcome.succeeded)
		logs, err := filepath.Glob(filepath.Join(r.f.dir, ".loopai", "progress", "progress-*.txt"))
		require.NoError(t, err)
		require.Len(t, logs, 1)
		data, err := os.ReadFile(logs[0])
		require.NoError(t, err)
		assert.Contains(t, string(data), "start attempt: descriptor write failed")
	})

	t.Run("hook error keeps a resumed worktree", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{keep: true, archive: true,
			hook: func(attemptStart) error { return errors.New("journal append failed") },
			before: func(t *testing.T, f finalizePRFixture, r *detachedRunResult) context.Context {
				wtPath, _, err := f.svc.CreateWorktreeForPlan(r.planPath, "")
				require.NoError(t, err)
				require.Equal(t, r.wtPath, wtPath)
				require.NoError(t, os.WriteFile(filepath.Join(wtPath, "previous.txt"), []byte("work\n"), 0o600))
				return t.Context()
			}})
		require.EqualError(t, r.err, "start attempt: journal append failed")
		require.Len(t, r.starts, 1)
		assert.True(t, r.starts[0].Resumed)
		require.DirExists(t, r.wtPath)
		assert.FileExists(t, filepath.Join(r.wtPath, "previous.txt"), "a previous attempt's work survives")
		assert.Equal(t, r.wtPath, r.outcome.worktreeKept)
		assert.Empty(t, r.sessions(t))
		requireRunLockFree(t, r)
	})

	t.Run("knobs unset remove the worktree on failure", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{plan: detachedOpenPlan, mode: "fail", maxIterations: 3})
		require.ErrorContains(t, r.err, "task execution failed")
		require.Len(t, r.starts, 1)
		assert.NoDirExists(t, r.wtPath, "without KeepWorktreeOnFailure a fresh worktree is removed as before")
		assert.Empty(t, r.outcome.worktreeKept)
		assert.NotContains(t, r.stderr, "worktree preserved")
	})

	t.Run("no hook keeps today's behavior under the knob", func(t *testing.T) {
		r := runDetachedWorktree(t, detachedRunCase{plan: detachedOpenPlan, mode: "fail", keep: true, noHook: true,
			maxIterations: 3})
		require.ErrorContains(t, r.err, "task execution failed")
		assert.NoDirExists(t, r.wtPath, "the knob takes effect only after AttemptStarted")
	})
}

// fakeWorktreeState answers the keep decision's worktree checks and counts them.
type fakeWorktreeState struct {
	dirty    bool
	dirtyErr error
	op       string
	opErr    error
	calls    int
}

func (f *fakeWorktreeState) IsDirtyAll() (bool, error) {
	f.calls++
	return f.dirty, f.dirtyErr
}

func (f *fakeWorktreeState) OperationInProgress() (string, error) {
	f.calls++
	return f.op, f.opErr
}

func TestKeepDetachedWorktree(t *testing.T) {
	tests := []struct {
		name      string
		knob      bool
		success   bool
		outcome   *planExecutionOutcome
		state     fakeWorktreeState
		want      bool
		wantCalls int
		wantWarn  string
	}{
		{name: "failure keeps without the knob", want: true},
		{name: "failure keeps with the knob", knob: true, want: true},
		{name: "success removes without the knob", success: true, state: fakeWorktreeState{dirty: true}},
		{name: "clean success removes", knob: true, success: true, outcome: &planExecutionOutcome{}, wantCalls: 2},
		{name: "clean success without an outcome removes", knob: true, success: true, wantCalls: 2},
		{name: "incomplete finalize keeps", knob: true, success: true,
			outcome: &planExecutionOutcome{finalizeIncomplete: errors.New("base sync blocked")}, want: true},
		{name: "incomplete archive keeps", knob: true, success: true,
			outcome: &planExecutionOutcome{archiveIncomplete: "move plan.md: rejected"}, want: true},
		{name: "dirty tree keeps", knob: true, success: true, state: fakeWorktreeState{dirty: true}, want: true, wantCalls: 1},
		{name: "failed dirty check keeps", knob: true, success: true,
			state: fakeWorktreeState{dirtyErr: errors.New("status failed")}, want: true, wantCalls: 1,
			wantWarn: "warning: keeping worktree, status failed"},
		{name: "operation in progress keeps", knob: true, success: true, state: fakeWorktreeState{op: "merge"},
			want: true, wantCalls: 2},
		{name: "failed operation check keeps", knob: true, success: true,
			state: fakeWorktreeState{opErr: errors.New("read MERGE_HEAD")}, want: true, wantCalls: 2,
			wantWarn: "warning: keeping worktree, read MERGE_HEAD"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.state
			var got bool
			stderr := captureStderr(t, func() {
				got = keepDetachedWorktree(executePlanRequest{KeepWorktreeOnFailure: tc.knob, Outcome: tc.outcome},
					&state, tc.success)
			})
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantCalls, state.calls)
			if tc.wantWarn == "" {
				assert.Empty(t, stderr)
				return
			}
			assert.Contains(t, stderr, tc.wantWarn)
		})
	}
}

func TestKeepWorktreeDecision(t *testing.T) {
	t.Run("without keep the worktree is never inspected", func(t *testing.T) {
		state := &fakeWorktreeState{dirty: true}
		decide := keepWorktreeDecision(false, executePlanRequest{KeepWorktreeOnFailure: true}, state)
		assert.False(t, decide(false))
		assert.Zero(t, state.calls)
	})

	t.Run("the first answer is memoized", func(t *testing.T) {
		state := &fakeWorktreeState{dirty: true}
		decide := keepWorktreeDecision(true, executePlanRequest{KeepWorktreeOnFailure: true}, state)
		assert.True(t, decide(true))
		calls := state.calls
		state.dirty = false
		assert.True(t, decide(true), "a removed worktree must not be inspected again")
		assert.True(t, decide(false))
		assert.Equal(t, calls, state.calls)
	})

	t.Run("a success decided first stays removed", func(t *testing.T) {
		state := &fakeWorktreeState{}
		decide := keepWorktreeDecision(true, executePlanRequest{KeepWorktreeOnFailure: true}, state)
		assert.False(t, decide(true))
		assert.False(t, decide(false), "the cleanup and the defer agree on one decision")
	})
}

func TestFireAttemptStarted(t *testing.T) {
	chdirTemp(t)
	baseLog, err := progress.NewLogger(progress.Config{PlanFile: "plan.md", Mode: "full", Branch: "b", NoColor: true},
		testColors(), &status.PhaseHolder{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = baseLog.Close() })
	cwd, err := os.Getwd()
	require.NoError(t, err)
	wtPath := filepath.Join(cwd, ".loopai", "worktrees", "b")
	hookErr := errors.New("boom")

	tests := []struct {
		name     string
		resumed  bool
		knob     bool
		noHook   bool
		hookErr  error
		wantKeep bool
		wantErr  string
	}{
		{name: "no hook fresh", noHook: true, knob: true},
		{name: "no hook resumed", noHook: true, resumed: true, wantKeep: true},
		{name: "hook fresh without knob", wantKeep: false},
		{name: "hook fresh with knob", knob: true, wantKeep: true},
		{name: "hook resumed without knob", resumed: true, wantKeep: true},
		{name: "hook error fresh", knob: true, hookErr: hookErr, wantErr: "start attempt: boom"},
		{name: "hook error resumed", knob: true, resumed: true, hookErr: hookErr, wantKeep: true,
			wantErr: "start attempt: boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var starts []attemptStart
			req := executePlanRequest{PlanFile: filepath.Join("docs", "plans", "plan.md"), KeepWorktreeOnFailure: tc.knob}
			if !tc.noHook {
				req.AttemptStarted = func(start attemptStart) error {
					starts = append(starts, start)
					return tc.hookErr
				}
			}
			wt := worktreeRun{path: wtPath, planFile: filepath.Join("docs", "plans", "plan.md"), resumed: tc.resumed}

			keep, err := fireAttemptStarted(req, wt, "b", baseLog)

			assert.Equal(t, tc.wantKeep, keep)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				require.ErrorIs(t, err, hookErr)
			} else {
				require.NoError(t, err)
			}
			if tc.noHook {
				assert.Empty(t, starts)
				return
			}
			require.Len(t, starts, 1)
			assert.Equal(t, attemptStart{
				PlanFile:     filepath.Join(wtPath, "docs", "plans", "plan.md"),
				MainPlanFile: filepath.Join(cwd, "docs", "plans", "plan.md"),
				Branch:       "b", WorktreePath: wtPath, ProgressLog: baseLog.Path(), Resumed: tc.resumed,
			}, starts[0], "relative plan paths reach the hook absolute")
		})
	}
}

func TestPlanExecutionOutcomeRecorders(t *testing.T) {
	t.Run("a nil outcome accepts every record", func(t *testing.T) {
		var outcome *planExecutionOutcome
		assert.NotPanics(t, func() {
			outcome.recordArchive(true, errors.New("boom"))
			outcome.recordFinalize(finalizeResult{mode: config.FinalizePR, incomplete: errors.New("boom")})
			outcome.recordWorktreeKept("/repo/.loopai/worktrees/b")
		})
	})

	t.Run("archive", func(t *testing.T) {
		outcome := &planExecutionOutcome{}
		outcome.recordArchive(false, errors.New("move plan.md: rejected by hook"))
		assert.False(t, outcome.planMoved)
		assert.Equal(t, "move plan.md: rejected by hook", outcome.archiveIncomplete)

		outcome.recordArchive(true, nil)
		assert.True(t, outcome.planMoved)
		assert.Empty(t, outcome.archiveIncomplete)
	})

	t.Run("finalize", func(t *testing.T) {
		outcome := &planExecutionOutcome{}
		incomplete := errors.New("PR checks failed")
		outcome.recordFinalize(finalizeResult{mode: config.FinalizeMerge, prURL: "https://github.com/acme/repo/pull/7",
			prOpened: true, incomplete: incomplete})
		assert.Equal(t, config.FinalizeMerge, outcome.finalizeMode)
		assert.Equal(t, "finalize incomplete", outcome.finalizeStatusNote)
		assert.Equal(t, "https://github.com/acme/repo/pull/7", outcome.prURL)
		assert.True(t, outcome.prOpened)
		assert.False(t, outcome.merged)
		assert.Same(t, incomplete, outcome.finalizeIncomplete)

		outcome.recordFinalize(finalizeResult{mode: config.FinalizeMerge, prOpened: true, merged: true})
		assert.Equal(t, "PR merged", outcome.finalizeStatusNote)
		assert.True(t, outcome.merged)
		require.NoError(t, outcome.finalizeIncomplete)
	})

	t.Run("worktree kept", func(t *testing.T) {
		outcome := &planExecutionOutcome{}
		outcome.recordWorktreeKept("/repo/.loopai/worktrees/b")
		assert.Equal(t, "/repo/.loopai/worktrees/b", outcome.worktreeKept)
	})
}
