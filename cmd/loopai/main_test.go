package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatih/color"
	flags "github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/cmux"
	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	gitmocks "github.com/umputun/ralphex/pkg/git/mocks"
	"github.com/umputun/ralphex/pkg/notify"
	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/progress"
	"github.com/umputun/ralphex/pkg/status"
	"github.com/umputun/ralphex/pkg/web"
)

var (
	_ processor.Logger = (*progress.SectionTimer)(nil)
	_ cmux.Logger      = (*progress.SectionTimer)(nil)
)

type runnerLoggerRecorder struct {
	calls []string
}

func (l *runnerLoggerRecorder) Print(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf("print: "+format, args...))
}
func (l *runnerLoggerRecorder) PrintRaw(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf("raw: "+format, args...))
}
func (l *runnerLoggerRecorder) PrintSection(section status.Section) {
	l.calls = append(l.calls, "section: "+section.Label)
}
func (l *runnerLoggerRecorder) PrintAligned(text string) {
	l.calls = append(l.calls, "aligned: "+text)
}
func (l *runnerLoggerRecorder) LogQuestion(question string, _ []string) {
	l.calls = append(l.calls, "question: "+question)
}
func (l *runnerLoggerRecorder) LogAnswer(answer string) {
	l.calls = append(l.calls, "answer: "+answer)
}
func (l *runnerLoggerRecorder) LogDraftReview(action, _ string) {
	l.calls = append(l.calls, "draft review: "+action)
}
func (l *runnerLoggerRecorder) Path() string { return "progress.log" }

// TestMain isolates the suite from a live cmux terminal. cmux.New reads CMUX_WORKSPACE_ID from the
// ambient environment, so running these tests inside cmux would drive the developer's real sidebar
// and fire real notification banners — the same class of leak as touching the user's config dir.
// tests that need a reporter set the variable themselves via t.Setenv, which restores this unset
// state afterwards, so the deliberate cases keep working.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("CMUX_WORKSPACE_ID"); err != nil {
		fmt.Fprintf(os.Stderr, "unset CMUX_WORKSPACE_ID: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestBuildRunnerLoggerRecordsSectionsInOrder(t *testing.T) {
	inner := &runnerLoggerRecorder{}
	out, timer := buildRunnerLogger(nil, inner)

	out.PrintSection(status.NewTaskIterationSection(1))
	out.PrintSection(status.NewInternalReviewSection(1, ""))
	timer.FinishRun()

	require.Len(t, inner.calls, 5)
	assert.Equal(t, "section: task iteration 1", inner.calls[0])
	assert.Regexp(t, `^print: task iteration 1 took .+$`, inner.calls[1])
	assert.Equal(t, "section: review 1", inner.calls[2])
	assert.Regexp(t, `^print: review 1 took .+$`, inner.calls[3])
	assert.Regexp(t, `^print: phase durations: tasks .+ \(1\), internal review .+ \(1\)$`, inner.calls[4])
}

func TestBuildRunnerLoggerKeepsCmuxOutermost(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
	rep := cmux.New("plan.md", cmux.Models{})
	require.NotNil(t, rep)

	out, _ := buildRunnerLogger(rep, &runnerLoggerRecorder{})
	_, ok := out.(interface {
		LogLimitWait(pattern, tool, waitLabel string)
	})
	assert.True(t, ok)
}

func TestBuildRunnerLoggerWithoutReporterReturnsTimer(t *testing.T) {
	out, timer := buildRunnerLogger(nil, &runnerLoggerRecorder{})

	assert.Same(t, timer, out)
}

func TestRunWithSectionTimingFinishesBeforeReturning(t *testing.T) {
	tests := []struct {
		name   string
		runErr error
	}{
		{name: "success"},
		{name: "failure", runErr: errors.New("runner failed")},
		{name: "user abort", runErr: processor.ErrUserAborted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &runnerLoggerRecorder{}
			timer := progress.NewSectionTimer(inner, nil)
			run := func(context.Context) error {
				timer.PrintSection(status.NewTaskIterationSection(1))
				return tt.runErr
			}

			gotErr := runWithSectionTiming(t.Context(), run, timer)
			inner.calls = append(inner.calls, "downstream result handling")

			if tt.runErr == nil {
				require.NoError(t, gotErr)
			} else {
				require.ErrorIs(t, gotErr, tt.runErr)
			}
			require.Len(t, inner.calls, 4)
			assert.Equal(t, "section: task iteration 1", inner.calls[0])
			assert.Regexp(t, `^print: task iteration 1 took .+$`, inner.calls[1])
			assert.Regexp(t, `^print: phase durations: tasks .+ \(1\)$`, inner.calls[2])
			assert.Equal(t, "downstream result handling", inner.calls[3])
		})
	}
}

// captureStdout runs fn while redirecting os.Stdout (and the fatih/color Output
// target, which many progress prints use) to a pipe and returns the captured output.
// uses defer to restore global state even if fn panics or calls t.FailNow, preventing
// leaked redirections from breaking later tests.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	origStdout := os.Stdout
	origColorOutput := color.Output
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	color.Output = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()

	// ensure pipe is closed and globals are restored even if fn panics or t.FailNow is called;
	// closing w unblocks the reader goroutine so the pipe FDs are released, and closing r
	// releases the read-end FD rather than waiting for GC finalization.
	var closed bool
	closePipe := func() {
		if !closed {
			_ = w.Close()
			closed = true
		}
	}
	defer func() {
		closePipe()
		_ = r.Close()
		os.Stdout = origStdout
		color.Output = origColorOutput
	}()

	fn()

	closePipe()
	return <-done
}

// testColors returns a Colors instance for testing.
func testColors() *progress.Colors {
	return progress.NewColors(config.ColorConfig{
		Task:       "0,255,0",
		Review:     "0,255,255",
		Codex:      "255,0,255",
		ClaudeEval: "100,200,255",
		Warn:       "255,255,0",
		Error:      "255,0,0",
		Signal:     "255,100,100",
		Timestamp:  "138,138,138",
		Info:       "180,180,180",
	})
}

// skipIfClaudeNotAvailable loads config (read-only) and skips test if configured claude command is not in PATH.
// uses LoadReadOnly to avoid installing defaults to real user config directory during tests.
func skipIfClaudeNotAvailable(t *testing.T) {
	t.Helper()
	cfg, err := config.LoadReadOnly("")
	if err != nil {
		t.Skipf("failed to load config: %v", err)
	}
	claudeCmd := cfg.ClaudeCommand
	if claudeCmd == "" {
		claudeCmd = "claude"
	}
	if _, err := exec.LookPath(claudeCmd); err != nil {
		t.Skipf("%s not installed", claudeCmd)
	}
}

// parseTestOpts parses command-line args and marks explicitly set flags.
func parseTestOpts(t *testing.T, args ...string) opts {
	t.Helper()
	var o opts
	parser := flags.NewParser(&o, flags.Default)
	remaining, err := parser.ParseArgs(args)
	require.NoError(t, err)
	if len(remaining) > 0 {
		o.PlanFile = remaining[0]
	}
	o.markFlagsSet(parser)
	return o
}

func TestLoopaiEnvironmentOptions(t *testing.T) {
	t.Setenv("LOOPAI_WEB_HOST", "0.0.0.0")
	t.Setenv("LOOPAI_CONFIG_DIR", "/tmp/loopai-config")

	o := parseTestOpts(t)

	assert.Equal(t, "0.0.0.0", o.Host)
	assert.Equal(t, "/tmp/loopai-config", o.ConfigDir)
}

func TestClearFlagParsing(t *testing.T) {
	o := parseTestOpts(t, "--clear")
	assert.True(t, o.Clear)
}

func TestCommitFlagParsing(t *testing.T) {
	t.Run("short flag sets commit", func(t *testing.T) {
		o := parseTestOpts(t, "-c")
		assert.True(t, o.Commit)
		assert.False(t, o.CodexOnly)
	})

	t.Run("long flag sets commit", func(t *testing.T) {
		o := parseTestOpts(t, "--commit")
		assert.True(t, o.Commit)
		assert.False(t, o.CodexOnly)
	})

	t.Run("codex only keeps long form", func(t *testing.T) {
		o := parseTestOpts(t, "--codex-only")
		assert.True(t, o.CodexOnly)
		assert.False(t, o.Commit)
	})
}

func TestMergeFlagParsing(t *testing.T) {
	t.Run("bare flag auto detects base", func(t *testing.T) {
		o := parseTestOpts(t, "--merge")
		assert.True(t, o.mergeSet)
		assert.Empty(t, o.Merge)
	})

	t.Run("explicit base", func(t *testing.T) {
		o := parseTestOpts(t, "--merge=develop")
		assert.True(t, o.mergeSet)
		assert.Equal(t, "develop", o.Merge)
	})

	t.Run("bare flag does not swallow conflicting execution flag", func(t *testing.T) {
		o := parseTestOpts(t, "--merge", "--worktree")
		assert.True(t, o.mergeSet)
		assert.True(t, o.Worktree)
		require.Error(t, validateFlags(o))
	})

	// the whole feature argument rests on optional-value leaving the next token positional; were
	// it consumed as the base, "--merge myfeature" would silently merge the current branch into a
	// branch named myfeature instead of closing out myfeature
	t.Run("bare flag leaves the feature argument positional", func(t *testing.T) {
		o := parseTestOpts(t, "--merge", "myfeature")
		assert.True(t, o.mergeSet)
		assert.Empty(t, o.Merge)
		assert.Equal(t, "myfeature", o.PlanFile)
		require.NoError(t, validateFlags(o))
	})

	t.Run("explicit base keeps the feature argument positional", func(t *testing.T) {
		o := parseTestOpts(t, "--merge=develop", "myfeature")
		assert.Equal(t, "develop", o.Merge)
		assert.Equal(t, "myfeature", o.PlanFile)
		require.NoError(t, validateFlags(o))
	})
}

func TestPRFlagParsing(t *testing.T) {
	t.Run("bare flag auto detects base", func(t *testing.T) {
		o := parseTestOpts(t, "--pr")
		assert.True(t, o.prSet)
		assert.Empty(t, o.PR)
	})

	t.Run("explicit base", func(t *testing.T) {
		o := parseTestOpts(t, "--pr=develop")
		assert.True(t, o.prSet)
		assert.Equal(t, "develop", o.PR)
	})

	t.Run("bare flag does not swallow conflicting execution flag", func(t *testing.T) {
		o := parseTestOpts(t, "--pr", "--codex")
		assert.True(t, o.prSet)
		assert.True(t, o.Codex)
		require.Error(t, validateFlags(o))
	})

	t.Run("bare flag leaves the feature argument positional", func(t *testing.T) {
		o := parseTestOpts(t, "--pr", "myfeature")
		assert.True(t, o.prSet)
		assert.Empty(t, o.PR)
		assert.Equal(t, "myfeature", o.PlanFile)
		require.NoError(t, validateFlags(o))
	})
}

func TestCloseoutRejectsExplicitZeroOrEmptyExecutionFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "clear with zero max iterations", args: []string{"--clear", "--max-iterations=0"}},
		{name: "merge with zero review patience", args: []string{"--merge", "--review-patience=0"}},
		{name: "pr with empty claude command", args: []string{"--pr", "--claude-command="}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := parseTestOpts(t, tt.args...)
			assert.True(t, o.executionModeSet)
			require.Error(t, validateFlags(o))
		})
	}
}

func TestPromptPlanDescription(t *testing.T) {
	colors := testColors()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "normal_input", input: "add user authentication\n", expected: "add user authentication"},
		{name: "input_with_whitespace", input: "  add caching  \n", expected: "add caching"},
		{name: "empty_input", input: "\n", expected: ""},
		{name: "only_whitespace", input: "   \n", expected: ""},
		{name: "multiword_description", input: "implement health check endpoint with metrics\n", expected: "implement health check endpoint with metrics"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reader := strings.NewReader(tc.input)
			result := plan.PromptDescription(t.Context(), reader, colors)
			assert.Equal(t, tc.expected, result)
		})
	}

	t.Run("eof_returns_empty", func(t *testing.T) {
		// empty reader simulates EOF (Ctrl+D)
		reader := strings.NewReader("")
		result := plan.PromptDescription(t.Context(), reader, colors)
		assert.Empty(t, result)
	})

	t.Run("context_canceled_returns_empty", func(t *testing.T) {
		// canceled context simulates Ctrl+C
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // cancel immediately
		reader := strings.NewReader("some input\n")
		result := plan.PromptDescription(ctx, reader, colors)
		assert.Empty(t, result)
	})
}

func TestDetermineMode(t *testing.T) {
	tests := []struct {
		name     string
		opts     opts
		expected processor.Mode
	}{
		{name: "default_is_full", opts: opts{}, expected: processor.ModeFull},
		{name: "review_flag", opts: opts{Review: true}, expected: processor.ModeReview},
		{name: "codex_only_flag", opts: opts{CodexOnly: true}, expected: processor.ModeCodexOnly},
		{name: "external_only_flag", opts: opts{ExternalOnly: true}, expected: processor.ModeCodexOnly},
		{name: "both_external_and_codex_flags", opts: opts{ExternalOnly: true, CodexOnly: true}, expected: processor.ModeCodexOnly},
		{name: "codex_only_takes_precedence_over_review", opts: opts{Review: true, CodexOnly: true}, expected: processor.ModeCodexOnly},
		{name: "external_only_takes_precedence_over_review", opts: opts{Review: true, ExternalOnly: true}, expected: processor.ModeCodexOnly},
		{name: "tasks_only_flag", opts: opts{TasksOnly: true}, expected: processor.ModeTasksOnly},
		{name: "tasks_only_takes_precedence_over_codex", opts: opts{TasksOnly: true, CodexOnly: true}, expected: processor.ModeTasksOnly},
		{name: "tasks_only_takes_precedence_over_external", opts: opts{TasksOnly: true, ExternalOnly: true}, expected: processor.ModeTasksOnly},
		{name: "tasks_only_takes_precedence_over_review", opts: opts{TasksOnly: true, Review: true}, expected: processor.ModeTasksOnly},
		{name: "plan_flag", opts: opts{PlanDescription: "add caching"}, expected: processor.ModePlan},
		{name: "plan_takes_precedence_over_review", opts: opts{PlanDescription: "add caching", Review: true}, expected: processor.ModePlan},
		{name: "plan_takes_precedence_over_codex", opts: opts{PlanDescription: "add caching", CodexOnly: true}, expected: processor.ModePlan},
		{name: "plan_takes_precedence_over_external", opts: opts{PlanDescription: "add caching", ExternalOnly: true}, expected: processor.ModePlan},
		{name: "plan_takes_precedence_over_tasks_only", opts: opts{PlanDescription: "add caching", TasksOnly: true}, expected: processor.ModePlan},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := determineMode(tc.opts)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestIsWatchOnlyMode(t *testing.T) {
	tests := []struct {
		name            string
		opts            opts
		configWatchDirs []string
		expected        bool
	}{
		{name: "serve_with_watch_and_no_plan", opts: opts{Serve: true, Watch: []string{"/tmp"}}, configWatchDirs: nil, expected: true},
		{name: "serve_with_config_watch_and_no_plan", opts: opts{Serve: true}, configWatchDirs: []string{"/home"}, expected: true},
		{name: "serve_without_watch", opts: opts{Serve: true}, configWatchDirs: nil, expected: false},
		{name: "no_serve_with_watch", opts: opts{Watch: []string{"/tmp"}}, configWatchDirs: nil, expected: false},
		{name: "serve_with_plan_file", opts: opts{Serve: true, Watch: []string{"/tmp"}, PlanFile: "plan.md"}, configWatchDirs: nil, expected: false},
		{name: "serve_with_plan_description", opts: opts{Serve: true, Watch: []string{"/tmp"}, PlanDescription: "add feature"}, configWatchDirs: nil, expected: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := isWatchOnlyMode(tc.opts, tc.configWatchDirs)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestPlanFlagConflict(t *testing.T) {
	t.Run("returns_error_when_plan_and_planfile_both_set", func(t *testing.T) {
		o := opts{
			PlanDescription: "add caching",
			PlanFile:        "docs/plans/some-plan.md",
		}
		err := run(t.Context(), o)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--plan flag conflicts")
	})

	t.Run("no_error_when_only_plan_flag_set", func(t *testing.T) {
		// this test will fail at a later point (missing git repo etc), but not at validation
		o := opts{PlanDescription: "add caching"}
		err := run(t.Context(), o)
		// should fail at git repo check, not at validation
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "--plan flag conflicts")
	})

	t.Run("no_error_when_only_planfile_set", func(t *testing.T) {
		// this test will fail at a later point (file not found etc), but not at validation
		o := opts{PlanFile: "nonexistent-plan.md"}
		err := run(t.Context(), o)
		// should fail at git repo check, not at validation
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "--plan flag conflicts")
	})
}

func TestPlanModeIntegration(t *testing.T) {
	t.Run("plan_mode_requires_git_repo", func(t *testing.T) {
		// skip if configured claude command is not installed
		skipIfClaudeNotAvailable(t)

		// run from a non-git directory
		tmpDir := t.TempDir()
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(tmpDir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		o := opts{PlanDescription: "add caching feature", ConfigDir: t.TempDir()}
		err = run(t.Context(), o)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no .git directory")
	})

	t.Run("plan_mode_runs_from_git_repo", func(t *testing.T) {
		// create a test git repo
		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// run in plan mode - will fail at claude execution but should pass validation and setup
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // cancel immediately to stop execution

		o := opts{PlanDescription: "add caching feature", MaxIterations: 1, ConfigDir: t.TempDir()}
		err = run(ctx, o)

		// should fail with context canceled, not validation errors
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "--plan flag conflicts")
		assert.NotContains(t, err.Error(), "no .git directory")
	})

	t.Run("plan_mode_clears_setup_before_execution", func(t *testing.T) {
		// skip if configured claude command is not installed
		skipIfClaudeNotAvailable(t)

		// run() checks the context after flag validation, config load, external review
		// resolution and dependency checks, so a pre-canceled context stops exactly there.
		// reaching that point is what proves plan mode got through setup — the plan creation
		// loop itself is covered by the runner tests, it is never entered with a dead context
		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create docs/plans directory to avoid config loading errors
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))

		// run with immediate cancel - should fail at executor, not validation
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		o := opts{PlanDescription: "test plan description", MaxIterations: 1, ConfigDir: t.TempDir()}
		err = run(ctx, o)

		// error must be the cancellation, not a config or validation failure
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context canceled")
		assert.NotContains(t, err.Error(), "--plan flag conflicts")
		assert.NotContains(t, err.Error(), "load config")
		assert.NotContains(t, err.Error(), "no .git directory")
	})
}

func TestAutoPlanModeDetection(t *testing.T) {
	t.Run("feature_branch_with_no_plans_still_errors", func(t *testing.T) {
		// skip if configured claude command is not installed
		skipIfClaudeNotAvailable(t)

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create empty plans dir
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))

		// create and switch to a feature branch
		gitSvc, err := git.NewService(".", testColors().Info())
		require.NoError(t, err)
		require.NoError(t, gitSvc.CreateBranch("feature-test"))

		// run without arguments - should error because we're on feature branch
		o := opts{MaxIterations: 1, ConfigDir: t.TempDir()}
		err = run(t.Context(), o)
		require.Error(t, err)
		// should still get the no plans found error, not auto-plan-mode
		assert.ErrorIs(t, err, plan.ErrNoPlansFound, "should return ErrNoPlansFound on feature branch")
	})

	t.Run("review_mode_skips_auto_plan_mode", func(t *testing.T) {
		// skip if configured claude command is not installed
		skipIfClaudeNotAvailable(t)

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create empty plans dir
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))

		// run in review mode with canceled context - should not trigger auto-plan-mode
		// plan is optional in review mode, so it proceeds (then fails on canceled context)
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // cancel immediately to avoid actual execution

		o := opts{Review: true, MaxIterations: 1, ConfigDir: t.TempDir()}
		err = run(ctx, o)
		// error should be from context cancellation or runner, not "no plans found"
		// this verifies auto-plan-mode is skipped for --review flag
		require.Error(t, err)
		assert.NotErrorIs(t, err, plan.ErrNoPlansFound, "review mode should skip auto-plan-mode")
	})

	t.Run("codex_only_mode_skips_auto_plan_mode", func(t *testing.T) {
		// skip if configured claude command is not installed
		skipIfClaudeNotAvailable(t)

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create empty plans dir
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))

		// run in codex-only mode with canceled context - should not trigger auto-plan-mode
		// plan is optional in codex-only mode, so it proceeds (then fails on canceled context)
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // cancel immediately to avoid actual execution

		o := opts{CodexOnly: true, MaxIterations: 1, ConfigDir: t.TempDir()}
		err = run(ctx, o)
		// error should be from context cancellation or runner, not "no plans found"
		// this verifies auto-plan-mode is skipped for --codex-only flag
		require.Error(t, err)
		assert.NotErrorIs(t, err, plan.ErrNoPlansFound, "codex-only mode should skip auto-plan-mode")
	})

	t.Run("external_only_mode_skips_auto_plan_mode", func(t *testing.T) {
		// skip if configured claude command is not installed
		skipIfClaudeNotAvailable(t)

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create empty plans dir
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))

		// run in external-only mode with canceled context - should not trigger auto-plan-mode
		// plan is optional in external-only mode, so it proceeds (then fails on canceled context)
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // cancel immediately to avoid actual execution

		o := opts{ExternalOnly: true, MaxIterations: 1, ConfigDir: t.TempDir()}
		err = run(ctx, o)
		// error should be from context cancellation or runner, not "no plans found"
		// this verifies auto-plan-mode is skipped for --external-only flag
		require.Error(t, err)
		assert.NotErrorIs(t, err, plan.ErrNoPlansFound, "external-only mode should skip auto-plan-mode")
	})
}

func TestTryAutoPlanMode(t *testing.T) {
	newReq := func(t *testing.T, branch string) (executePlanRequest, *plan.Selector) {
		t.Helper()
		dir := setupTestRepo(t)
		gitSvc, err := git.NewService(dir, testColors().Info())
		require.NoError(t, err)
		if branch != "master" {
			require.NoError(t, gitSvc.CreateBranch(branch))
		}
		selector := plan.NewSelector(filepath.Join(dir, "docs", "plans"), testColors())
		req := executePlanRequest{GitSvc: gitSvc, DefaultBranch: "master", Colors: testColors()}
		return req, selector
	}

	t.Run("non_default_branch_refuses_with_reason", func(t *testing.T) {
		req, selector := newReq(t, "feature-x")
		handled, err := tryAutoPlanMode(t.Context(), plan.ErrNoPlansFound, opts{}, req, selector)
		assert.True(t, handled, "missing-plans error on feature branch is handled")
		require.Error(t, err)
		require.ErrorIs(t, err, plan.ErrNoPlansFound, "wrapped sentinel preserved for callers")
		assert.Contains(t, err.Error(), "default branch")
		assert.Contains(t, err.Error(), "feature-x")
		assert.Contains(t, err.Error(), "--plan")
	})

	t.Run("origin_prefixed_default_branch_is_normalized_for_display", func(t *testing.T) {
		req, selector := newReq(t, "master")
		req.DefaultBranch = "origin/main"
		handled, err := tryAutoPlanMode(t.Context(), plan.ErrNoPlansFound, opts{}, req, selector)
		assert.True(t, handled)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"main"`, "origin/ prefix stripped for display")
		assert.NotContains(t, err.Error(), "origin/main", "raw remote-tracking ref not shown to user")
	})

	t.Run("tasks_only_mode_refuses_with_reason", func(t *testing.T) {
		req, selector := newReq(t, "master")
		handled, err := tryAutoPlanMode(t.Context(), plan.ErrNoPlansFound, opts{TasksOnly: true}, req, selector)
		assert.True(t, handled)
		require.Error(t, err)
		require.ErrorIs(t, err, plan.ErrNoPlansFound)
		assert.Contains(t, err.Error(), "not available in this mode")
	})

	t.Run("unrelated_error_is_not_handled", func(t *testing.T) {
		req, selector := newReq(t, "master")
		handled, err := tryAutoPlanMode(t.Context(), errors.New("fzf not found"), opts{}, req, selector)
		assert.False(t, handled, "non-missing-plans error propagates to caller untouched")
		require.NoError(t, err)
	})
}

func TestCheckClaudeDep(t *testing.T) {
	t.Run("uses_configured_command", func(t *testing.T) {
		cfg := &config.Config{ClaudeCommand: "nonexistent-command-12345"}
		err := checkClaudeDep(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nonexistent-command-12345")
	})

	t.Run("falls_back_to_claude_when_empty", func(t *testing.T) {
		// force PATH lookup to fail deterministically so the assertion runs on dev machines too
		t.Setenv("PATH", "")
		cfg := &config.Config{ClaudeCommand: ""}
		err := checkClaudeDep(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "claude")
	})
}

func TestCheckCodexDep(t *testing.T) {
	t.Run("uses_configured_command", func(t *testing.T) {
		cfg := &config.Config{CodexCommand: "nonexistent-codex-12345"}
		err := checkCodexDep(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nonexistent-codex-12345")
		assert.Contains(t, err.Error(), "not found in PATH")
	})

	t.Run("falls_back_to_codex_when_empty", func(t *testing.T) {
		// force PATH lookup to fail deterministically so the assertion runs on dev machines too
		t.Setenv("PATH", "")
		cfg := &config.Config{CodexCommand: ""}
		err := checkCodexDep(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "codex")
	})
}

func TestCreateRunner(t *testing.T) {
	t.Run("creates_runner_without_panic", func(t *testing.T) {
		tmpDir := t.TempDir()
		oldWd, wdErr := os.Getwd()
		require.NoError(t, wdErr)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { _ = os.Chdir(oldWd) })

		cfg := &config.Config{IterationDelayMs: 5000, TaskRetryCount: 3, CodexEnabled: false}
		o := opts{MaxIterations: 100, Debug: true, NoColor: true}

		colors := testColors()
		holder := &status.PhaseHolder{}
		log, err := progress.NewLogger(progress.Config{PlanFile: "", Mode: "full", Branch: "test", NoColor: true}, colors, holder)
		require.NoError(t, err)
		defer log.Close()

		req := executePlanRequest{PlanFile: "/path/to/plan.md", Mode: processor.ModeFull, Config: cfg, DefaultBranch: "master"}
		runner := createRunner(req, o, log, holder)
		assert.NotNil(t, runner)
	})

	t.Run("codex_only_mode_creates_runner_without_panic", func(t *testing.T) {
		tmpDir := t.TempDir()
		oldWd, wdErr := os.Getwd()
		require.NoError(t, wdErr)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { _ = os.Chdir(oldWd) })

		cfg := &config.Config{CodexEnabled: false} // explicitly disabled in config
		o := opts{MaxIterations: 50}

		colors := testColors()
		holder := &status.PhaseHolder{}
		log, err := progress.NewLogger(progress.Config{PlanFile: "", Mode: "codex", Branch: "test", NoColor: true}, colors, holder)
		require.NoError(t, err)
		defer log.Close()

		// tests that codex-only mode code path runs without panic
		req := executePlanRequest{Mode: processor.ModeCodexOnly, Config: cfg, DefaultBranch: "main"}
		runner := createRunner(req, o, log, holder)
		assert.NotNil(t, runner)
	})

	t.Run("max_external_iterations_cli_overrides_config", func(t *testing.T) {
		tmpDir := t.TempDir()
		oldWd, wdErr := os.Getwd()
		require.NoError(t, wdErr)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { _ = os.Chdir(oldWd) })

		cfg := &config.Config{MaxExternalIterations: 10}       // config says 10
		o := opts{MaxIterations: 50, MaxExternalIterations: 5} // CLI says 5

		colors := testColors()
		holder := &status.PhaseHolder{}
		log, err := progress.NewLogger(progress.Config{Mode: "full", Branch: "test", NoColor: true}, colors, holder)
		require.NoError(t, err)
		defer log.Close()

		// verify the resolution logic: CLI=5 should win over config=10
		// the resolve logic: maxExtIter = config(10), then CLI > 0 so maxExtIter = 5
		req := executePlanRequest{Mode: processor.ModeFull, Config: cfg, DefaultBranch: "main"}
		runner := createRunner(req, o, log, holder)
		assert.NotNil(t, runner)
		// can't inspect Runner.cfg directly, but the wiring code is exercised
		// behavioral verification is in runner_test.go (TestRunner_MaxExternalIterations_ExplicitLimit)
	})

	t.Run("review_patience_cli_overrides_config", func(t *testing.T) {
		tmpDir := t.TempDir()
		oldWd, wdErr := os.Getwd()
		require.NoError(t, wdErr)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { _ = os.Chdir(oldWd) })

		cfg := &config.Config{ReviewPatience: 5}        // config says 5
		o := opts{MaxIterations: 50, ReviewPatience: 3} // CLI says 3

		colors := testColors()
		holder := &status.PhaseHolder{}
		log, err := progress.NewLogger(progress.Config{Mode: "full", Branch: "test", NoColor: true}, colors, holder)
		require.NoError(t, err)
		defer log.Close()

		// verify the resolution logic: CLI=3 should win over config=5
		req := executePlanRequest{Mode: processor.ModeFull, Config: cfg, DefaultBranch: "main"}
		runner := createRunner(req, o, log, holder)
		assert.NotNil(t, runner)
		// behavioral verification is in runner_test.go
	})
}

func TestResolveDefaultBranch(t *testing.T) {
	tests := []struct {
		name         string
		cliRef       string
		configBranch string
		autoDetect   string
		expected     string
	}{
		{name: "cli_flag_wins", cliRef: "abc1234", configBranch: "develop", autoDetect: "main", expected: "abc1234"},
		{name: "config_when_no_flag", cliRef: "", configBranch: "develop", autoDetect: "main", expected: "develop"},
		{name: "auto_detect_when_nothing_set", cliRef: "", configBranch: "", autoDetect: "main", expected: "main"},
		{name: "cli_flag_commit_hash", cliRef: "deadbeef", configBranch: "", autoDetect: "master", expected: "deadbeef"},
		{name: "all_empty", cliRef: "", configBranch: "", autoDetect: "", expected: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := resolveDefaultBranch(tc.cliRef, tc.configBranch, tc.autoDetect)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestResolveBranchBase(t *testing.T) {
	tests := []struct {
		name          string
		cliRef        string
		cliRefBranch  string
		defaultBranch string
		currentBranch string
		expected      string
		expectedErr   string
	}{
		{
			name: "no_cli_ref_keeps_default", cliRef: "", defaultBranch: "main", currentBranch: "main",
			expected: "main",
		},
		{
			name: "branch_ref_becomes_base_when_checked_out", cliRef: "release/13.0.0", cliRefBranch: "release/13.0.0",
			defaultBranch: "main", currentBranch: "release/13.0.0", expected: "release/13.0.0",
		},
		{
			name: "remote_tracking_ref_resolves_to_local_branch", cliRef: "origin/main", cliRefBranch: "main",
			defaultBranch: "main", currentBranch: "main", expected: "main",
		},
		{
			name: "branch_ref_from_another_feature_branch_is_kept", cliRef: "release/13.0.0", cliRefBranch: "release/13.0.0",
			defaultBranch: "main", currentBranch: "some-feature", expected: "release/13.0.0",
		},
		{
			name: "branch_ref_from_the_default_branch_fails", cliRef: "release/13.0.0", cliRefBranch: "release/13.0.0",
			defaultBranch: "main", currentBranch: "main",
			expectedErr: `--base-ref "release/13.0.0" names a branch but the checkout is on "main"`,
		},
		{
			name: "branch_ref_from_the_remote_form_of_the_default_branch_fails", cliRef: "release/13.0.0",
			cliRefBranch: "release/13.0.0", defaultBranch: "origin/main", currentBranch: "main",
			expectedErr: `run "git checkout release/13.0.0"`,
		},
		{
			name: "commit_hash_keeps_default_outside_worktree_mode", cliRef: "abc1234", defaultBranch: "main",
			currentBranch: "main", expected: "main",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := resolveBranchBase(tc.cliRef, tc.cliRefBranch, tc.defaultBranch, tc.currentBranch)
			if tc.expectedErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestResolveBaseRefs(t *testing.T) {
	t.Run("branch_base_ref_is_diff_only_in_worktree_mode", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "checkout", "-b", "release/13.0.0")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		branchBase, diffBase, err := resolveBaseRefs(gitSvc, "release/13.0.0", "", true, true)
		require.NoError(t, err)
		assert.Equal(t, "master", branchBase, "worktree creation ignores the resolved branch base and uses current HEAD")
		assert.Equal(t, "release/13.0.0", diffBase, "review diffs honor the requested base")
	})

	t.Run("branch_base_ref_rejected_from_the_default_branch", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "branch", "release/13.0.0")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		// on master with a release base: honoring it would make CreateBranchForPlan read the
		// mismatch as "already on a feature branch" and leave the run committing onto master
		_, _, err = resolveBaseRefs(gitSvc, "release/13.0.0", "", true, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `run "git checkout release/13.0.0"`)
	})

	t.Run("branch_base_ref_ignored_by_modes_that_create_no_branch", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "branch", "release/13.0.0")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		// review modes never create a branch, so --base-ref is a pure diff base there
		branchBase, diffBase, err := resolveBaseRefs(gitSvc, "release/13.0.0", "", false, false)
		require.NoError(t, err)
		assert.Equal(t, "master", branchBase)
		assert.Equal(t, "release/13.0.0", diffBase)
	})

	t.Run("remote_tracking_base_ref_accepted_in_worktree_mode", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "remote", "add", "origin", dir)
		runGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		branchBase, diffBase, err := resolveBaseRefs(gitSvc, "origin/master", "", true, true)
		require.NoError(t, err)
		assert.Equal(t, "master", branchBase, "worktree creation keeps the default branch metadata")
		assert.Equal(t, "origin/master", diffBase, "diffs keep the requested revision as written")
	})

	t.Run("commit_hash_base_ref_is_diff_only_in_worktree_mode", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		hash, err := gitSvc.HeadHash()
		require.NoError(t, err)

		branchBase, diffBase, err := resolveBaseRefs(gitSvc, hash, "", true, true)
		require.NoError(t, err)
		assert.Equal(t, "master", branchBase)
		assert.Equal(t, hash, diffBase)
	})

	t.Run("commit_hash_base_ref_kept_for_diffs_without_worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		hash, err := gitSvc.HeadHash()
		require.NoError(t, err)

		branchBase, diffBase, err := resolveBaseRefs(gitSvc, hash, "", true, false)
		require.NoError(t, err)
		assert.Equal(t, "master", branchBase, "a hash cannot be a branch base, auto-detected default stays")
		assert.Equal(t, hash, diffBase, "diffs still honor the requested revision")
	})

	t.Run("config_branch_used_when_no_cli_ref", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		branchBase, diffBase, err := resolveBaseRefs(gitSvc, "", "develop", true, true)
		require.NoError(t, err)
		assert.Equal(t, "develop", branchBase)
		assert.Equal(t, "develop", diffBase)
	})
}

func TestLocalBranchRef(t *testing.T) {
	dir := setupTestRepo(t)
	runGit(t, dir, "branch", "release/13.0.0")
	runGit(t, dir, "remote", "add", "origin", dir)
	runGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")

	gitSvc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)

	hash, err := gitSvc.HeadHash()
	require.NoError(t, err)

	tests := []struct {
		name     string
		ref      string
		expected string
	}{
		{name: "local branch", ref: "release/13.0.0", expected: "release/13.0.0"},
		{name: "remote tracking form", ref: "origin/master", expected: "master"},
		{name: "remote tracking form without a local branch", ref: "origin/nope", expected: ""},
		{name: "commit hash", ref: hash, expected: ""},
		{name: "empty ref", ref: "", expected: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, localBranchRef(gitSvc, tc.ref))
		})
	}
}

func TestForceExitCleanup(t *testing.T) {
	t.Run("waits for every holder", func(t *testing.T) {
		var restored, first, second atomic.Bool
		fast, slow := &cleanupHolder{}, &cleanupHolder{}
		fast.set(func() { first.Store(true) })
		// the sidebar reset shells out to cmux, so it is slower than an unset worktree holder;
		// a fire-and-forget goroutine here would lose the race against os.Exit
		slow.set(func() {
			time.Sleep(50 * time.Millisecond)
			second.Store(true)
		})

		forceExitCleanup(func() { restored.Store(true) }, fast, slow)()

		assert.True(t, restored.Load(), "the terminal must be restored")
		assert.True(t, first.Load())
		assert.True(t, second.Load(), "cleanup must not return before a slow holder finished")
	})

	t.Run("unset holders are skipped", func(t *testing.T) {
		assert.NotPanics(t, forceExitCleanup(func() {}, &cleanupHolder{}, &cleanupHolder{}))
	})
}

func TestResolveMaxIterations(t *testing.T) {
	tests := []struct {
		name     string
		cliValue int
		cfg      *config.Config
		expected int
	}{
		{name: "cli_explicitly_set", cliValue: 25, cfg: &config.Config{MaxIterations: 100, MaxIterationsSet: true}, expected: 25},
		{name: "cli_explicitly_50", cliValue: 50, cfg: &config.Config{MaxIterations: 30, MaxIterationsSet: true}, expected: 50},
		{name: "config_when_cli_not_set", cliValue: 0, cfg: &config.Config{MaxIterations: 100, MaxIterationsSet: true}, expected: 100},
		{name: "default_when_nothing_set", cliValue: 0, cfg: &config.Config{}, expected: 50},
		{name: "cli_value_no_config", cliValue: 10, cfg: &config.Config{}, expected: 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := resolveMaxIterations(tc.cliValue, tc.cfg)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestSkipFinalizeFlag(t *testing.T) {
	t.Run("skip_finalize_disables_in_runner", func(t *testing.T) {
		tmpDir := t.TempDir()
		oldWd, wdErr := os.Getwd()
		require.NoError(t, wdErr)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { _ = os.Chdir(oldWd) })

		cfg := &config.Config{FinalizeEnabled: true}
		o := opts{SkipFinalize: true, MaxIterations: 50}

		// apply the same override as run() does
		if o.SkipFinalize {
			cfg.FinalizeEnabled = false
		}

		colors := testColors()
		holder := &status.PhaseHolder{}
		log, err := progress.NewLogger(progress.Config{Mode: "full", Branch: "test", NoColor: true}, colors, holder)
		require.NoError(t, err)
		defer log.Close()

		// verify createRunner receives the overridden config
		req := executePlanRequest{Mode: processor.ModeFull, Config: cfg, DefaultBranch: "main"}
		runner := createRunner(req, o, log, holder)
		assert.NotNil(t, runner)
		assert.False(t, cfg.FinalizeEnabled, "skip-finalize should override config")
	})

	t.Run("no_skip_finalize_preserves_config", func(t *testing.T) {
		cfg := &config.Config{FinalizeEnabled: true}
		o := opts{SkipFinalize: false}
		if o.SkipFinalize {
			cfg.FinalizeEnabled = false
		}
		assert.True(t, cfg.FinalizeEnabled, "config should be preserved when skip-finalize not set")
	})
}

func TestPreserveAnthropicAPIKeyFlag(t *testing.T) {
	t.Run("flag enables when config disabled", func(t *testing.T) {
		cfg := &config.Config{PreserveAnthropicAPIKey: false}
		o := parseTestOpts(t, "--preserve-anthropic-api-key")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.True(t, cfg.PreserveAnthropicAPIKey, "CLI flag should enable preserve in config")
	})

	t.Run("absent flag preserves config true", func(t *testing.T) {
		cfg := &config.Config{PreserveAnthropicAPIKey: true}
		o := parseTestOpts(t)

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.True(t, cfg.PreserveAnthropicAPIKey, "config-set true should be preserved when flag absent")
	})

	t.Run("absent flag preserves config false", func(t *testing.T) {
		cfg := &config.Config{PreserveAnthropicAPIKey: false}
		o := parseTestOpts(t)

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.False(t, cfg.PreserveAnthropicAPIKey)
	})
}

func TestProviderOverrideFlags(t *testing.T) {
	t.Run("external_reviewers_flag_is_tracked_and_overrides_config", func(t *testing.T) {
		cfg := &config.Config{ExternalReviewers: "claude:sonnet", ExternalReviewersSet: true}
		o := parseTestOpts(t, "--external-reviewers", "codex:gpt-5.6:xhigh,claude:fable:max")

		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.True(t, o.externalReviewersSet)
		assert.Equal(t, "codex:gpt-5.6:xhigh,claude:fable:max", cfg.ExternalReviewers)
		assert.True(t, cfg.ExternalReviewersSet)
	})

	t.Run("external_reviewers_conflicts_with_legacy_flags", func(t *testing.T) {
		for _, legacy := range []string{"--external-review-tool=codex", "--external-review-model=gpt-5.6"} {
			o := parseTestOpts(t, "--external-reviewers=codex", legacy)
			require.ErrorContains(t, validateFlags(o), "cannot be combined")
			require.ErrorContains(t, applyCLIOverrides(o, &config.Config{}), "cannot be combined")
		}
	})

	t.Run("external_review_tool_accepts_auto_and_claude", func(t *testing.T) {
		for _, tool := range []string{config.ExternalReviewToolAuto, config.ExternalReviewToolClaude} {
			o := parseTestOpts(t, "--external-review-tool="+tool)
			assert.Equal(t, tool, o.ExternalReviewTool)
			assert.True(t, o.externalReviewToolSet)
		}
	})

	t.Run("claude_command_overrides_config", func(t *testing.T) {
		cfg := &config.Config{ClaudeCommand: "configured-claude"}
		o := parseTestOpts(t, "--claude-command", "/tmp/run-claude")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Equal(t, "/tmp/run-claude", cfg.ClaudeCommand)
	})

	t.Run("claude_args_overrides_config", func(t *testing.T) {
		cfg := &config.Config{ClaudeArgs: "--configured"}
		o := parseTestOpts(t, "--claude-args=--wrapper --stream")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Equal(t, "--wrapper --stream", cfg.ClaudeArgs)
	})

	t.Run("empty_claude_args_clears_config", func(t *testing.T) {
		cfg := &config.Config{ClaudeArgs: "--configured --args"}
		o := parseTestOpts(t, "--claude-args=")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Empty(t, cfg.ClaudeArgs)
		assert.True(t, cfg.ClaudeArgsSet)
	})

	t.Run("external_review_tool_overrides_config", func(t *testing.T) {
		cfg := &config.Config{ExternalReviewTool: "codex"}
		o := parseTestOpts(t, "--external-review-tool", "custom")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Equal(t, "custom", cfg.ExternalReviewTool)
	})

	t.Run("external_review_model_overrides_config", func(t *testing.T) {
		cfg := &config.Config{ExternalReviewModel: "gpt-5.5:low"}
		o := parseTestOpts(t, "--external-review-model", "opus:xhigh")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Equal(t, "opus:xhigh", cfg.ExternalReviewModel)
		assert.True(t, cfg.ExternalReviewModelSet)
	})

	t.Run("empty_external_review_model_clears_config", func(t *testing.T) {
		cfg := &config.Config{ExternalReviewModel: "gpt-5.5:low", ExternalReviewModelSet: true}
		o := parseTestOpts(t, "--external-review-model=")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Empty(t, cfg.ExternalReviewModel)
		assert.True(t, cfg.ExternalReviewModelSet)
	})

	t.Run("custom_review_script_overrides_config", func(t *testing.T) {
		cfg := &config.Config{CustomReviewScript: "/configured/review.sh"}
		o := parseTestOpts(t, "--custom-review-script", "/tmp/review.sh")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Equal(t, "/tmp/review.sh", cfg.CustomReviewScript)
	})

	t.Run("external_review_tool_cli_override_does_not_mutate_codex_enabled", func(t *testing.T) {
		// Explicit providers bypass the legacy CodexEnabled auto-selection gate,
		// so applyCLIOverrides does not need to flip CodexEnabled.
		cfg := &config.Config{
			CodexEnabled:       false,
			CodexEnabledSet:    true,
			ExternalReviewTool: "none",
		}
		o := parseTestOpts(t, "--external-review-tool", "custom")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Equal(t, "custom", cfg.ExternalReviewTool)
		assert.False(t, cfg.CodexEnabled)
		assert.True(t, cfg.CodexEnabledSet)
	})

	t.Run("external_review_tool_none_keeps_review_disabled", func(t *testing.T) {
		cfg := &config.Config{CodexEnabled: false, CodexEnabledSet: true, ExternalReviewTool: "codex"}
		o := parseTestOpts(t, "--external-review-tool", "none")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Equal(t, "none", cfg.ExternalReviewTool)
		assert.False(t, cfg.CodexEnabled)
		assert.True(t, cfg.CodexEnabledSet)
	})
}

func TestExternalReviewCLIConfigPrecedence(t *testing.T) {
	tmp := t.TempDir()
	globalDir := filepath.Join(tmp, "global")
	localDir := filepath.Join(tmp, "project", ".loopai")
	require.NoError(t, os.MkdirAll(globalDir, 0o750))
	require.NoError(t, os.MkdirAll(localDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "config"), []byte(
		"external_review_tool = codex\nexternal_review_model = gpt-global:low\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "config"), []byte(
		"external_review_tool = claude\nexternal_review_model = opus:high\n"), 0o600))

	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(filepath.Dir(localDir)))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	cfg, err := config.LoadReadOnly(globalDir)
	require.NoError(t, err)
	assert.Equal(t, config.ExternalReviewToolClaude, cfg.ExternalReviewTool, "local config overrides global")
	assert.Equal(t, "opus:high", cfg.ExternalReviewModel)

	o := parseTestOpts(t, "--external-review-tool=codex", "--external-review-model=gpt-cli:xhigh")
	require.NoError(t, applyCLIOverrides(o, cfg))
	assert.Equal(t, config.ExternalReviewToolCodex, cfg.ExternalReviewTool, "CLI overrides local config")
	assert.Equal(t, "gpt-cli:xhigh", cfg.ExternalReviewModel)
}

func TestExternalReviewersEmptyLocalValueDisablesGlobalChain(t *testing.T) {
	tmp := t.TempDir()
	globalDir := filepath.Join(tmp, "global")
	projectDir := filepath.Join(tmp, "project")
	localDir := filepath.Join(projectDir, ".loopai")
	require.NoError(t, os.MkdirAll(globalDir, 0o750))
	require.NoError(t, os.MkdirAll(localDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "config"), []byte("external_reviewers = codex\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "config"), []byte("external_reviewers =\n"), 0o600))

	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(projectDir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	cfg, err := config.LoadReadOnly(globalDir)
	require.NoError(t, err)
	assert.True(t, cfg.ExternalReviewersSet)
	assert.Empty(t, cfg.ExternalReviewers)

	selection, err := resolveExternalReviewSelection(opts{}, cfg, processor.ModeFull)
	require.NoError(t, err)
	assert.True(t, selection.Resolved)
	assert.True(t, selection.Explicit)
	assert.Empty(t, selection.Reviewers)
}

func TestResolveExternalReviewSelection(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.Config
		mode       processor.Mode
		wantTool   string
		wantModel  string
		wantEffort string
		wantAuto   bool
		wantMax    bool
		wantErr    string
	}{
		{name: "claude primary auto selects codex", cfg: config.Config{CodexEnabled: true, ExternalReviewTool: "auto", CodexModel: "gpt-5.5", CodexReasoningEffort: "high"}, mode: processor.ModeFull, wantTool: "codex", wantModel: "gpt-5.5", wantEffort: "high", wantAuto: true},
		{name: "codex primary auto selects claude dynamic default", cfg: config.Config{Executor: config.ExecutorCodex, CodexEnabled: true, ExternalReviewTool: "auto"}, mode: processor.ModeFull, wantTool: "claude", wantModel: "opus", wantEffort: "xhigh", wantAuto: true},
		{name: "codex enabled false disables ordinary auto", cfg: config.Config{ExternalReviewTool: "auto"}, mode: processor.ModeFull, wantTool: "none", wantAuto: true},
		{name: "external only forces auto despite legacy gate", cfg: config.Config{Executor: config.ExecutorCodex, ExternalReviewTool: "auto"}, mode: processor.ModeCodexOnly, wantTool: "claude", wantModel: "opus", wantEffort: "xhigh", wantAuto: true},
		{name: "explicit provider ignores legacy gate", cfg: config.Config{ExternalReviewTool: "codex", CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}, mode: processor.ModeFull, wantTool: "codex", wantModel: "gpt-5.5", wantEffort: "xhigh"},
		{name: "tasks only requires no external provider", cfg: config.Config{ExternalReviewTool: "claude", ExternalReviewModel: "opus:xhigh", ExternalReviewModelSet: true}, mode: processor.ModeTasksOnly, wantTool: "none"},
		{name: "explicit claude model overrides both defaults", cfg: config.Config{ExternalReviewTool: "claude", ExternalReviewModel: "sonnet:high", ExternalReviewModelSet: true}, mode: processor.ModeFull, wantTool: "claude", wantModel: "sonnet", wantEffort: "high"},
		{name: "claude effort-only keeps opus", cfg: config.Config{ExternalReviewTool: "claude", ExternalReviewModel: ":max", ExternalReviewModelSet: true}, mode: processor.ModeFull, wantTool: "claude", wantModel: "opus", wantEffort: "max"},
		{name: "codex explicit model overlays provider defaults", cfg: config.Config{ExternalReviewTool: "codex", ExternalReviewModel: "gpt-5.6:low", ExternalReviewModelSet: true, CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}, mode: processor.ModeFull, wantTool: "codex", wantModel: "gpt-5.6", wantEffort: "low"},
		{name: "codex max is dropped with warning signal", cfg: config.Config{ExternalReviewTool: "codex", ExternalReviewModel: "gpt-5.6:max", ExternalReviewModelSet: true, CodexModel: "gpt-5.5", CodexReasoningEffort: "high"}, mode: processor.ModeFull, wantTool: "codex", wantModel: "gpt-5.6", wantEffort: "high", wantMax: true},
		{name: "explicit none", cfg: config.Config{ExternalReviewTool: "none"}, mode: processor.ModeFull, wantTool: "none"},
		{name: "custom with model rejected", cfg: config.Config{ExternalReviewTool: "custom", ExternalReviewModel: "anything", ExternalReviewModelSet: true}, mode: processor.ModeFull, wantErr: "cannot be used"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveExternalReviewSelection(opts{}, &tc.cfg, tc.mode)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, got.Resolved)
			reviewer, ok := got.firstReviewer()
			if tc.wantTool == config.ExternalReviewToolNone {
				assert.False(t, ok)
			} else {
				require.True(t, ok)
				assert.Equal(t, tc.wantTool, reviewer.Provider)
				assert.Equal(t, tc.wantModel, reviewer.Model)
				assert.Equal(t, tc.wantEffort, reviewer.Effort)
				assert.Equal(t, tc.wantMax, reviewer.MaxDropped)
			}
			assert.Equal(t, tc.wantAuto, got.AutoSelected)
		})
	}
}

func TestResolveExternalReviewerChain(t *testing.T) {
	cfg := &config.Config{
		ExternalReviewers:    "codex, claude:fable:max, custom",
		ExternalReviewersSet: true,
		CodexModel:           "gpt-5.5",
		CodexReasoningEffort: "high",
	}

	got, err := resolveExternalReviewSelection(opts{}, cfg, processor.ModeFull)
	require.NoError(t, err)
	assert.True(t, got.Explicit)
	assert.Equal(t, []resolvedReviewer{
		{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "high"},
		{Provider: config.ExternalReviewToolClaude, Model: "fable", Effort: "max"},
		{Provider: config.ExternalReviewToolCustom},
	}, got.Reviewers)

	tasksOnly, err := resolveExternalReviewSelection(opts{}, cfg, processor.ModeTasksOnly)
	require.NoError(t, err)
	assert.Empty(t, tasksOnly.Reviewers)

	bad := &config.Config{ExternalReviewers: "codex,", ExternalReviewersSet: true}
	_, err = resolveExternalReviewSelection(opts{}, bad, processor.ModeFull)
	require.ErrorContains(t, err, "parse external_reviewers")

	empty := &config.Config{ExternalReviewersSet: true}
	disabled, err := resolveExternalReviewSelection(opts{}, empty, processor.ModeFull)
	require.NoError(t, err)
	assert.True(t, disabled.Resolved)
	assert.True(t, disabled.Explicit)
	assert.Empty(t, disabled.Reviewers)

	normalized := &config.Config{ExternalReviewers: "codex : gpt-5.6 : high", ExternalReviewersSet: true}
	resolved, err := resolveExternalReviewSelection(opts{}, normalized, processor.ModeFull)
	require.NoError(t, err)
	require.Len(t, resolved.Reviewers, 1)
	assert.Equal(t, "gpt-5.6", resolved.Reviewers[0].Model)
	assert.Equal(t, "high", resolved.Reviewers[0].Effort)
}

func TestExternalReviewWarnings(t *testing.T) {
	t.Run("explicit same provider warns", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex, ExternalReviewTool: config.ExternalReviewToolCodex}
		selection, err := resolveExternalReviewSelection(opts{}, cfg, processor.ModeFull)
		require.NoError(t, err)
		var buf bytes.Buffer
		printExternalReviewWarnings(opts{}, selection, cfg, &buf)
		assert.Contains(t, buf.String(), "matches the primary executor")
	})

	t.Run("auto cross provider does not warn", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex, CodexEnabled: true, ExternalReviewTool: config.ExternalReviewToolAuto}
		selection, err := resolveExternalReviewSelection(opts{}, cfg, processor.ModeFull)
		require.NoError(t, err)
		var buf bytes.Buffer
		printExternalReviewWarnings(opts{}, selection, cfg, &buf)
		assert.Empty(t, buf.String())
	})

	t.Run("configured chain warns when legacy CLI flag is ignored", func(t *testing.T) {
		cfg := &config.Config{ExternalReviewers: "codex", ExternalReviewersSet: true}
		o := parseTestOpts(t, "--external-review-tool=none")
		require.NoError(t, applyCLIOverrides(o, cfg))
		selection, err := resolveExternalReviewSelection(o, cfg, processor.ModeFull)
		require.NoError(t, err)

		var buf bytes.Buffer
		printExternalReviewWarnings(o, selection, cfg, &buf)
		assert.Contains(t, buf.String(), "legacy external-review CLI flags are ignored")
		assert.Contains(t, buf.String(), "--external-reviewers=")
	})

	t.Run("configured chain warns when legacy config key is ignored", func(t *testing.T) {
		cfg := &config.Config{
			ExternalReviewTool:    config.ExternalReviewToolNone,
			ExternalReviewToolSet: true,
			ExternalReviewers:     "codex",
			ExternalReviewersSet:  true,
		}
		selection, err := resolveExternalReviewSelection(opts{}, cfg, processor.ModeFull)
		require.NoError(t, err)

		var buf bytes.Buffer
		printExternalReviewWarnings(opts{}, selection, cfg, &buf)
		assert.Contains(t, buf.String(), "legacy external_review_tool and external_review_model config keys are ignored")
		assert.Contains(t, buf.String(), "set external_reviewers =")
	})

	t.Run("repeated providers emit each warning once", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex}
		selection := externalReviewSelection{Explicit: true, Reviewers: []resolvedReviewer{
			{Provider: config.ExternalReviewToolCodex, MaxDropped: true},
			{Provider: config.ExternalReviewToolCodex, MaxDropped: true},
		}}

		var buf bytes.Buffer
		printExternalReviewWarnings(opts{}, selection, cfg, &buf)
		assert.Equal(t, 1, strings.Count(buf.String(), "matches the primary executor"))
		assert.Equal(t, 1, strings.Count(buf.String(), "does not support 'max' reasoning effort"))
	})
}

func TestCheckExecutionDeps(t *testing.T) {
	fakeClaude := filepath.Join(t.TempDir(), "claude-ok")
	writeExecutable(t, fakeClaude, "#!/bin/sh\nexit 0\n")
	fakeCodex := filepath.Join(t.TempDir(), "codex-ok")
	writeExecutable(t, fakeCodex, "#!/bin/sh\nexit 0\n")
	missingClaude := filepath.Join(t.TempDir(), "missing-claude")
	missingCodex := filepath.Join(t.TempDir(), "missing-codex")

	tests := []struct {
		name        string
		cfg         config.Config
		selection   externalReviewSelection
		wantTool    string
		wantErr     string
		wantWarning bool
	}{
		{name: "both providers present", cfg: config.Config{ClaudeCommand: fakeClaude, CodexCommand: fakeCodex}, selection: externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: "codex"}}, AutoSelected: true}, wantTool: "codex"},
		{name: "automatic external missing degrades", cfg: config.Config{ClaudeCommand: fakeClaude, CodexCommand: missingCodex}, selection: externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: "codex"}}, AutoSelected: true}, wantTool: "none", wantWarning: true},
		{name: "explicit external missing fails", cfg: config.Config{ClaudeCommand: fakeClaude, CodexCommand: missingCodex}, selection: externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: "codex"}}, Explicit: true}, wantTool: "codex", wantErr: "install the codex CLI"},
		{name: "codex primary automatic claude missing degrades", cfg: config.Config{Executor: config.ExecutorCodex, ClaudeCommand: missingClaude, CodexCommand: fakeCodex}, selection: externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: "claude"}}, AutoSelected: true}, wantTool: "none", wantWarning: true},
		{name: "primary missing always fails", cfg: config.Config{ClaudeCommand: missingClaude, CodexCommand: fakeCodex}, selection: externalReviewSelection{}, wantTool: "none", wantErr: "install Claude Code"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var warnings bytes.Buffer
			got, err := checkExecutionDeps(&tc.cfg, tc.selection, &warnings)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			reviewer, ok := got.firstReviewer()
			if tc.wantTool == config.ExternalReviewToolNone {
				assert.False(t, ok)
			} else {
				require.True(t, ok)
				assert.Equal(t, tc.wantTool, reviewer.Provider)
			}
			assert.Equal(t, tc.wantWarning, warnings.Len() > 0)
		})
	}
}

func TestCheckExecutionDepsChain(t *testing.T) {
	fakeClaude := filepath.Join(t.TempDir(), "claude-ok")
	writeExecutable(t, fakeClaude, "#!/bin/sh\nexit 0\n")
	missingCodex := filepath.Join(t.TempDir(), "missing-codex")

	t.Run("explicit chain missing binary fails", func(t *testing.T) {
		cfg := &config.Config{ClaudeCommand: fakeClaude, CodexCommand: missingCodex}
		selection := externalReviewSelection{Explicit: true, Reviewers: []resolvedReviewer{
			{Provider: config.ExternalReviewToolClaude},
			{Provider: config.ExternalReviewToolCodex},
			{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.6"},
		}}
		_, err := checkExecutionDeps(cfg, selection, io.Discard)
		assert.ErrorContains(t, err, "install the codex CLI")
	})

	t.Run("custom reviewer requires script", func(t *testing.T) {
		cfg := &config.Config{ClaudeCommand: fakeClaude}
		selection := externalReviewSelection{Explicit: true, Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCustom}}}
		_, err := checkExecutionDeps(cfg, selection, io.Discard)
		require.EqualError(t, err, "custom external reviewer requires custom_review_script")

		cfg.CustomReviewScript = "/tmp/review.sh"
		_, err = checkExecutionDeps(cfg, selection, io.Discard)
		require.NoError(t, err)
	})
}

func TestRunAppliesClaudeCommandOverrideBeforeDependencyCheck(t *testing.T) {
	tmpDir := t.TempDir()
	cfgDir := filepath.Join(tmpDir, "config")
	require.NoError(t, os.MkdirAll(cfgDir, 0o750))

	missingCommand := "missing-ralphex-claude-command"
	configData := []byte("claude_command = " + missingCommand + "\n")
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"), configData, 0o600))

	fakeClaude := filepath.Join(tmpDir, "fake-claude")
	writeExecutable(t, fakeClaude, "#!/bin/sh\nexit 0\n")

	workDir := filepath.Join(tmpDir, "work")
	require.NoError(t, os.MkdirAll(workDir, 0o750))
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	o := parseTestOpts(t, "--config-dir", cfgDir, "--claude-command", fakeClaude)

	err = run(t.Context(), o)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must run from repository root")
	assert.NotContains(t, err.Error(), missingCommand)
}

func TestWaitFlag(t *testing.T) {
	t.Run("wait_cli_overrides_config", func(t *testing.T) {
		cfg := &config.Config{WaitOnLimit: 10 * time.Minute, WaitOnLimitSet: true}
		o := opts{Wait: 2 * time.Hour}
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 2*time.Hour, cfg.WaitOnLimit)
		assert.True(t, cfg.WaitOnLimitSet)
	})

	t.Run("wait_zero_preserves_config", func(t *testing.T) {
		cfg := &config.Config{WaitOnLimit: 30 * time.Minute, WaitOnLimitSet: true}
		o := opts{Wait: 0} // not set
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 30*time.Minute, cfg.WaitOnLimit, "config value should be preserved when CLI not set")
		assert.True(t, cfg.WaitOnLimitSet)
	})

	t.Run("wait_cli_sets_unset_config", func(t *testing.T) {
		cfg := &config.Config{} // wait_on_limit not set
		o := opts{Wait: 1 * time.Hour}
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 1*time.Hour, cfg.WaitOnLimit)
		assert.True(t, cfg.WaitOnLimitSet)
	})
}

func TestDetectClaudeSwapRecovery(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	cfg := &config.Config{ClaudeSwapEnabled: true, ClaudeCommand: "claude"}

	assert.Nil(t, detectClaudeSwapRecovery(opts{}, cfg, externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCodex}}}),
		"missing claude-swap keeps the optional integration disabled")
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "claude-swap"), []byte("#!/bin/sh\n"), 0o755)) //nolint:gosec // executable fixture for LookPath
	assert.NotNil(t, detectClaudeSwapRecovery(opts{}, cfg, externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCodex}}}))
	assert.Nil(t, detectClaudeSwapRecovery(opts{NoClaudeSwap: true}, cfg, externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCodex}}}))

	cfg.ClaudeSwapEnabled = false
	assert.Nil(t, detectClaudeSwapRecovery(opts{}, cfg, externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCodex}}}))
	cfg.ClaudeSwapEnabled = true
	cfg.ClaudeCommand = "claude-wrapper"
	assert.Nil(t, detectClaudeSwapRecovery(opts{}, cfg, externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCodex}}}),
		"custom stream-json wrappers must not mutate Claude Code credentials")

	cfg.ClaudeCommand = "claude"
	cfg.Executor = config.ExecutorCodex
	assert.Nil(t, detectClaudeSwapRecovery(opts{}, cfg, externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCodex}}}))
	assert.NotNil(t, detectClaudeSwapRecovery(opts{}, cfg, externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCodex}, {Provider: config.ExternalReviewToolClaude}}}),
		"a native external Claude reviewer uses the same failover integration")
}

func TestSessionTimeoutFlag(t *testing.T) {
	t.Run("cli_overrides_config", func(t *testing.T) {
		cfg := &config.Config{SessionTimeout: 10 * time.Minute, SessionTimeoutSet: true}
		o := opts{SessionTimeout: 2 * time.Hour}
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 2*time.Hour, cfg.SessionTimeout)
		assert.True(t, cfg.SessionTimeoutSet)
	})

	t.Run("zero_preserves_config", func(t *testing.T) {
		cfg := &config.Config{SessionTimeout: 30 * time.Minute, SessionTimeoutSet: true}
		o := opts{SessionTimeout: 0} // not set
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 30*time.Minute, cfg.SessionTimeout, "config value should be preserved when CLI not set")
		assert.True(t, cfg.SessionTimeoutSet)
	})

	t.Run("cli_sets_unset_config", func(t *testing.T) {
		cfg := &config.Config{} // session_timeout not set
		o := opts{SessionTimeout: 1 * time.Hour}
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 1*time.Hour, cfg.SessionTimeout)
		assert.True(t, cfg.SessionTimeoutSet)
	})
}

func TestIdleTimeoutFlag(t *testing.T) {
	t.Run("cli_overrides_config", func(t *testing.T) {
		cfg := &config.Config{IdleTimeout: 10 * time.Minute, IdleTimeoutSet: true}
		o := opts{IdleTimeout: 5 * time.Minute}
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 5*time.Minute, cfg.IdleTimeout)
		assert.True(t, cfg.IdleTimeoutSet)
	})

	t.Run("zero_preserves_config", func(t *testing.T) {
		cfg := &config.Config{IdleTimeout: 10 * time.Minute, IdleTimeoutSet: true}
		o := opts{IdleTimeout: 0} // not set
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 10*time.Minute, cfg.IdleTimeout, "config value should be preserved when CLI not set")
		assert.True(t, cfg.IdleTimeoutSet)
	})

	t.Run("cli_sets_unset_config", func(t *testing.T) {
		cfg := &config.Config{} // idle_timeout not set
		o := opts{IdleTimeout: 5 * time.Minute}
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, 5*time.Minute, cfg.IdleTimeout)
		assert.True(t, cfg.IdleTimeoutSet)
	})
}

func TestExplicitZeroOverridesConfig(t *testing.T) {
	// verify that --flag 0 on the command line overrides a non-zero config value.
	// uses markFlagsSet with a real go-flags parser to populate the *Set bools.
	makeOpts := func(flagName string) opts {
		var o opts
		p := flags.NewParser(&o, flags.Default)
		_, err := p.ParseArgs([]string{"--" + flagName, "0"})
		require.NoError(t, err)
		o.markFlagsSet(p)
		return o
	}

	t.Run("idle_timeout_zero_overrides_config", func(t *testing.T) {
		cfg := &config.Config{IdleTimeout: 5 * time.Minute, IdleTimeoutSet: true}
		o := makeOpts("idle-timeout")
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, time.Duration(0), cfg.IdleTimeout)
		assert.True(t, cfg.IdleTimeoutSet)
	})

	t.Run("session_timeout_zero_overrides_config", func(t *testing.T) {
		cfg := &config.Config{SessionTimeout: 30 * time.Minute, SessionTimeoutSet: true}
		o := makeOpts("session-timeout")
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, time.Duration(0), cfg.SessionTimeout)
		assert.True(t, cfg.SessionTimeoutSet)
	})

	t.Run("wait_zero_overrides_config", func(t *testing.T) {
		cfg := &config.Config{WaitOnLimit: 1 * time.Hour, WaitOnLimitSet: true}
		o := makeOpts("wait")
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, time.Duration(0), cfg.WaitOnLimit)
		assert.True(t, cfg.WaitOnLimitSet)
	})
}

func TestGetCurrentBranch(t *testing.T) {
	t.Run("returns_branch_name", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitSvc, err := git.NewService(dir, testColors().Info())
		require.NoError(t, err)

		branch := getCurrentBranch(gitSvc)
		assert.Equal(t, "master", branch)
	})

	t.Run("returns_unknown_on_error", func(t *testing.T) {
		// create a repo but then break it by removing .git
		dir := setupTestRepo(t)
		gitSvc, err := git.NewService(dir, testColors().Info())
		require.NoError(t, err)

		// close and remove git dir to simulate error
		require.NoError(t, os.RemoveAll(filepath.Join(dir, ".git")))

		// getCurrentBranch should return "unknown" on error
		branch := getCurrentBranch(gitSvc)
		assert.Equal(t, "unknown", branch)
	})
}

func TestValidateFlags(t *testing.T) {
	tests := []struct {
		name    string
		opts    opts
		wantErr bool
		errMsg  string
	}{
		{name: "no_flags_is_valid", opts: opts{}, wantErr: false},
		{name: "plan_flag_only_is_valid", opts: opts{PlanDescription: "add feature"}, wantErr: false},
		{name: "plan_file_only_is_valid", opts: opts{PlanFile: "docs/plans/test.md"}, wantErr: false},
		{name: "clear_only_is_valid", opts: opts{Clear: true}, wantErr: false},
		{name: "clear_with_plan_file_conflicts", opts: opts{Clear: true, PlanFile: "docs/plans/test.md"}, wantErr: true, errMsg: "--clear cannot be combined"},
		{name: "clear_with_plan_mode_conflicts", opts: opts{Clear: true, PlanDescription: "add feature"}, wantErr: true, errMsg: "other mode flags"},
		{name: "clear_with_review_mode_conflicts", opts: opts{Clear: true, Review: true}, wantErr: true, errMsg: "other mode flags"},
		{name: "clear_with_worktree_conflicts", opts: opts{Clear: true, Worktree: true}, wantErr: true, errMsg: "other mode flags"},
		{name: "clear_with_init_mode_conflicts", opts: opts{Clear: true, Init: true}, wantErr: true, errMsg: "other mode flags"},
		{name: "merge_only_is_valid", opts: opts{mergeSet: true}, wantErr: false},
		{name: "merge_with_explicit_base_is_valid", opts: opts{Merge: "develop"}, wantErr: false},
		{name: "merge_with_feature_argument_is_valid", opts: opts{mergeSet: true, PlanFile: "docs/plans/test.md"}, wantErr: false},
		{name: "merge_with_review_conflicts", opts: opts{mergeSet: true, Review: true}, wantErr: true, errMsg: "other mode flags"},
		{name: "merge_with_branch_conflicts", opts: opts{mergeSet: true, Branch: "feature"}, wantErr: true, errMsg: "other mode flags"},
		{name: "merge_with_executor_option_conflicts", opts: opts{mergeSet: true, Codex: true}, wantErr: true, errMsg: "other mode flags"},
		{name: "merge_with_clear_conflicts", opts: opts{mergeSet: true, Clear: true}, wantErr: true, errMsg: "--clear cannot be combined"},
		{name: "pr_only_is_valid", opts: opts{prSet: true}, wantErr: false},
		{name: "pr_with_explicit_base_is_valid", opts: opts{PR: "develop"}, wantErr: false},
		{name: "pr_with_feature_argument_is_valid", opts: opts{prSet: true, PlanFile: "docs/plans/test.md"}, wantErr: false},
		{name: "pr_with_review_conflicts", opts: opts{prSet: true, Review: true}, wantErr: true, errMsg: "other mode flags"},
		{name: "pr_with_base_ref_conflicts", opts: opts{prSet: true, BaseRef: "develop"}, wantErr: true, errMsg: "other mode flags"},
		{name: "pr_with_model_conflicts", opts: opts{prSet: true, TaskModel: "opus"}, wantErr: true, errMsg: "other mode flags"},
		{name: "pr_with_merge_conflicts", opts: opts{prSet: true, mergeSet: true}, wantErr: true, errMsg: "--pr cannot be combined"},
		{name: "pr_with_clear_conflicts", opts: opts{prSet: true, Clear: true}, wantErr: true, errMsg: "--clear cannot be combined"},
		{name: "both_plan_and_planfile_conflicts", opts: opts{PlanDescription: "add feature", PlanFile: "docs/plans/test.md"}, wantErr: true, errMsg: "conflicts"},
		{name: "negative_wait_is_invalid", opts: opts{Wait: -30 * time.Minute}, wantErr: true, errMsg: "non-negative"},
		{name: "positive_wait_is_valid", opts: opts{Wait: time.Hour}, wantErr: false},
		{name: "zero_wait_is_valid", opts: opts{Wait: 0}, wantErr: false},
		{name: "negative_session_timeout_is_invalid", opts: opts{SessionTimeout: -10 * time.Minute}, wantErr: true, errMsg: "non-negative"},
		{name: "positive_session_timeout_is_valid", opts: opts{SessionTimeout: 30 * time.Minute}, wantErr: false},
		{name: "zero_session_timeout_is_valid", opts: opts{SessionTimeout: 0}, wantErr: false},
		{name: "negative_idle_timeout_is_invalid", opts: opts{IdleTimeout: -5 * time.Minute}, wantErr: true, errMsg: "non-negative"},
		{name: "positive_idle_timeout_is_valid", opts: opts{IdleTimeout: 5 * time.Minute}, wantErr: false},
		{name: "zero_idle_timeout_is_valid", opts: opts{IdleTimeout: 0}, wantErr: false},
		{name: "codex_alone_is_valid", opts: opts{Codex: true}, wantErr: false},
		{name: "codex_with_pass_claude_md_is_valid", opts: opts{Codex: true, PassClaudeMd: true}, wantErr: false},
		{name: "commit_with_worktree_is_valid", opts: opts{Commit: true, Worktree: true}, wantErr: false},
		{name: "commit_without_worktree_is_deferred_until_config_merge", opts: opts{Commit: true}, wantErr: false},
		{name: "commit_with_resume_is_inert", opts: opts{Commit: true, Worktree: true, ResumeWorktree: true}, wantErr: false},
		{name: "commit_with_implicit_resume_is_inert", opts: opts{Commit: true, ResumeWorktree: true}, wantErr: false},
		{name: "commit_with_review_is_invalid", opts: opts{Commit: true, Worktree: true, Review: true}, wantErr: true, errMsg: "only supported for full"},
		{name: "commit_with_external_only_is_invalid", opts: opts{Commit: true, Worktree: true, ExternalOnly: true}, wantErr: true, errMsg: "only supported for full"},
		{name: "resume_worktree_is_valid", opts: opts{ResumeWorktree: true}, wantErr: false},
		{name: "resume_worktree_with_tasks_only_is_valid", opts: opts{ResumeWorktree: true, TasksOnly: true}, wantErr: false},
		{name: "resume_worktree_with_plan_conflicts", opts: opts{ResumeWorktree: true, PlanDescription: "add feature"}, wantErr: true, errMsg: "--plan"},
		{name: "resume_worktree_with_review_conflicts", opts: opts{ResumeWorktree: true, Review: true}, wantErr: true, errMsg: "full or --tasks-only"},
		{name: "resume_worktree_with_external_only_conflicts", opts: opts{ResumeWorktree: true, ExternalOnly: true}, wantErr: true, errMsg: "full or --tasks-only"},
		// the --codex / --external-only / --codex-only / --external-review-tool / --pass-claude-md
		// mutex checks moved to applyCodexOverrides so config-file executor=codex is also enforced;
		// validateFlags accepts those combos at CLI parse time and the post-merge gate rejects them.
		{name: "codex_with_external_only_accepted_at_cli_stage", opts: opts{Codex: true, ExternalOnly: true}, wantErr: false},
		{name: "codex_with_codex_only_accepted_at_cli_stage", opts: opts{Codex: true, CodexOnly: true}, wantErr: false},
		{name: "pass_claude_md_without_codex_is_valid_at_cli_stage", opts: opts{PassClaudeMd: true}, wantErr: false},
	}

	// parser-based cases: opts built through a real go-flags parse so tag defaults
	// (e.g. max-external-iterations, review-patience default:"0") are applied exactly
	// as in production. hand-built opts above never see defaults and cannot catch a
	// default being mistaken for an explicit CLI value.
	t.Run("parsed_standalone_closeout_flags_are_valid", func(t *testing.T) {
		for _, args := range [][]string{{"--clear"}, {"--merge"}, {"--pr"}} {
			var o opts
			p := flags.NewParser(&o, flags.Default)
			_, err := p.ParseArgs(args)
			require.NoError(t, err)
			o.markFlagsSet(p)
			assert.NoError(t, validateFlags(o), "bare %v must be valid", args)
		}
	})

	// "--merge <base> <feature>" is the documented "--merge=<base> <feature>" form with the "="
	// forgotten. go-flags leaves --merge empty and hands both tokens back as positionals, so the
	// base would be closed out as the feature. reject it end to end, from a real parse.
	t.Run("parsed_closeout_rejects_surplus_positional", func(t *testing.T) {
		for _, tc := range []struct{ flag, args string }{{"--merge", "--merge"}, {"--pr", "--pr"}} {
			var o opts
			p := flags.NewParser(&o, flags.Default)
			args, err := p.ParseArgs([]string{tc.args, "release/13", "feature"})
			require.NoError(t, err)
			o.markFlagsSet(p)
			o.applyPositionalArgs(args)
			assert.Equal(t, "release/13", o.PlanFile)
			assert.Equal(t, []string{"feature"}, o.extraArgs)
			require.ErrorContains(t, validateFlags(o), tc.flag+" accepts at most one feature argument")
		}
	})

	t.Run("parsed_closeout_accepts_single_positional", func(t *testing.T) {
		var o opts
		p := flags.NewParser(&o, flags.Default)
		args, err := p.ParseArgs([]string{"--merge", "feature"})
		require.NoError(t, err)
		o.markFlagsSet(p)
		o.applyPositionalArgs(args)
		assert.Equal(t, "feature", o.PlanFile)
		assert.Empty(t, o.extraArgs)
		assert.NoError(t, validateFlags(o))
	})

	t.Run("parsed_explicit_zero_execution_flag_still_conflicts", func(t *testing.T) {
		var o opts
		p := flags.NewParser(&o, flags.Default)
		_, err := p.ParseArgs([]string{"--clear", "--max-external-iterations", "0"})
		require.NoError(t, err)
		o.markFlagsSet(p)
		require.ErrorContains(t, validateFlags(o), "--clear cannot be combined")
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFlags(tc.opts)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestApplyCodexOverrides_AllowsSymmetricExternalReview(t *testing.T) {
	t.Run("cli_codex_plus_external_only_allowed", func(t *testing.T) {
		cfg := &config.Config{}
		o := parseTestOpts(t, "--codex", "--external-only")
		var warnBuf bytes.Buffer
		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))
		assert.Equal(t, config.ExecutorCodex, cfg.Executor)
	})

	t.Run("config_executor_codex_plus_cli_external_only_allowed", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex}
		o := parseTestOpts(t, "--external-only")
		var warnBuf bytes.Buffer
		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))
	})

	t.Run("config_executor_codex_plus_cli_codex_only_allowed", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex}
		o := parseTestOpts(t, "--codex-only")
		var warnBuf bytes.Buffer
		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))
	})

	t.Run("cli_codex_plus_external_review_tool_codex_allowed", func(t *testing.T) {
		cfg := &config.Config{}
		o := parseTestOpts(t, "--codex", "--external-review-tool", "codex")
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, config.ExternalReviewToolCodex, cfg.ExternalReviewTool)
	})

	t.Run("cli_codex_plus_external_review_tool_custom_allowed", func(t *testing.T) {
		cfg := &config.Config{}
		o := parseTestOpts(t, "--codex", "--external-review-tool", "custom")
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, config.ExternalReviewToolCustom, cfg.ExternalReviewTool)
	})

	t.Run("config_executor_codex_plus_cli_external_review_tool_custom_allowed", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex}
		o := parseTestOpts(t, "--external-review-tool", "custom")
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, config.ExternalReviewToolCustom, cfg.ExternalReviewTool)
	})

	t.Run("cli_codex_plus_external_review_tool_none_allowed", func(t *testing.T) {
		cfg := &config.Config{}
		o := parseTestOpts(t, "--codex", "--external-review-tool", "none")
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, "none", cfg.ExternalReviewTool)
	})

	t.Run("config_executor_codex_plus_cli_external_review_tool_none_allowed", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex}
		o := parseTestOpts(t, "--external-review-tool", "none")
		require.NoError(t, applyCLIOverrides(o, cfg))
		assert.Equal(t, "none", cfg.ExternalReviewTool)
	})

	t.Run("non_codex_executor_does_not_reject_external_only", func(t *testing.T) {
		// when executor is not codex, --external-only is fine; the codex mutex gate
		// must not over-reach.
		cfg := &config.Config{}
		o := parseTestOpts(t, "--external-only")
		var warnBuf bytes.Buffer
		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))
	})
}

func TestApplyCLIOverrides_ResumeWorktreeImpliesWorktree(t *testing.T) {
	cfg := &config.Config{}
	require.NoError(t, applyCLIOverrides(opts{ResumeWorktree: true}, cfg))
	assert.True(t, cfg.WorktreeEnabled)
}

func TestApplyCLIOverrides_CommitRequiresEffectiveWorktree(t *testing.T) {
	t.Run("accepts config enabled worktree", func(t *testing.T) {
		cfg := &config.Config{WorktreeEnabled: true}
		require.NoError(t, applyCLIOverrides(opts{Commit: true}, cfg))
		assert.True(t, cfg.WorktreeEnabled)
	})

	t.Run("accepts CLI enabled worktree", func(t *testing.T) {
		cfg := &config.Config{}
		require.NoError(t, applyCLIOverrides(opts{Commit: true, Worktree: true}, cfg))
		assert.True(t, cfg.WorktreeEnabled)
	})

	t.Run("accepts resume implied worktree", func(t *testing.T) {
		cfg := &config.Config{}
		require.NoError(t, applyCLIOverrides(opts{Commit: true, ResumeWorktree: true}, cfg))
		assert.True(t, cfg.WorktreeEnabled)
	})

	t.Run("rejects when worktree is disabled", func(t *testing.T) {
		require.ErrorContains(t, applyCLIOverrides(opts{Commit: true}, &config.Config{}), "--commit requires --worktree")
	})
}

func TestCodexFlag_ApplyCLIOverrides(t *testing.T) {
	t.Run("codex_flag_sets_executor_without_overriding_external_review", func(t *testing.T) {
		cfg := &config.Config{ExternalReviewTool: "codex"}
		o := parseTestOpts(t, "--codex")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Equal(t, config.ExecutorCodex, cfg.Executor)
		assert.Equal(t, "codex", cfg.ExternalReviewTool)
	})

	t.Run("pass_claude_md_flag_sets_pass_claude_md", func(t *testing.T) {
		cfg := &config.Config{}
		o := parseTestOpts(t, "--codex", "--pass-claude-md")

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.True(t, cfg.PassClaudeMd)
	})

	t.Run("absent_codex_flag_does_not_touch_executor", func(t *testing.T) {
		cfg := &config.Config{Executor: "", ExternalReviewTool: "codex"}
		o := parseTestOpts(t)

		require.NoError(t, applyCLIOverrides(o, cfg))

		assert.Empty(t, cfg.Executor)
		assert.Equal(t, "codex", cfg.ExternalReviewTool)
	})

	t.Run("config_executor_codex_preserves_user_external_review_tool", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex, ExternalReviewTool: "codex", ExternalReviewToolSet: true}
		o := parseTestOpts(t)
		var warnBuf bytes.Buffer

		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))

		assert.Equal(t, "codex", cfg.ExternalReviewTool)
		assert.Empty(t, warnBuf.String())
	})

	t.Run("config_executor_codex_embedded_default_does_not_warn", func(t *testing.T) {
		// user did NOT set external_review_tool — the value is just the embedded default.
		// no warning should fire (this was the spurious-warning bug on vanilla --codex runs).
		cfg := &config.Config{Executor: config.ExecutorCodex, ExternalReviewTool: "codex", ExternalReviewToolSet: false}
		o := parseTestOpts(t)
		var warnBuf bytes.Buffer

		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))

		assert.Equal(t, "codex", cfg.ExternalReviewTool)
		assert.Empty(t, warnBuf.String(), "no warning expected when external_review_tool is from embedded default")
	})

	t.Run("config_executor_codex_with_external_review_none_no_warning", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex, ExternalReviewTool: "none", ExternalReviewToolSet: true}
		o := parseTestOpts(t)
		var warnBuf bytes.Buffer

		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))

		assert.Equal(t, "none", cfg.ExternalReviewTool)
		assert.Empty(t, warnBuf.String())
	})

	t.Run("cli_external_review_tool_explicit_does_not_emit_warning", func(t *testing.T) {
		// validateFlags now accepts --codex with --external-review-tool=none at the CLI
		// stage (see TestValidateFlags), so applyCodexOverrides runs after it. this guards
		// the no-warning branch: a user explicitly setting the flag to "none" should not
		// see the codex-override warning.
		cfg := &config.Config{Executor: config.ExecutorCodex, ExternalReviewTool: "none"}
		o := parseTestOpts(t, "--external-review-tool", "none")
		var warnBuf bytes.Buffer

		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))

		assert.Equal(t, "none", cfg.ExternalReviewTool)
		assert.Empty(t, warnBuf.String())
	})

	t.Run("config_executor_codex_plus_cli_pass_claude_md_succeeds", func(t *testing.T) {
		// post-merge gate: --pass-claude-md is acceptable when executor=codex
		// comes from config file, even without --codex on the CLI.
		cfg := &config.Config{Executor: config.ExecutorCodex, ExternalReviewTool: "none"}
		o := parseTestOpts(t, "--pass-claude-md")
		var warnBuf bytes.Buffer

		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))

		assert.True(t, cfg.PassClaudeMd)
		assert.Equal(t, config.ExecutorCodex, cfg.Executor)
		assert.Empty(t, warnBuf.String())
	})

	t.Run("cli_pass_claude_md_without_any_codex_fails_post_merge", func(t *testing.T) {
		// post-merge gate: --pass-claude-md without codex executor (neither CLI nor config)
		// is rejected with a clear error message.
		cfg := &config.Config{Executor: "", ExternalReviewTool: "none"}
		o := parseTestOpts(t, "--pass-claude-md")
		var warnBuf bytes.Buffer

		err := applyCodexOverrides(o, cfg, &warnBuf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--pass-claude-md requires --codex")
		assert.Contains(t, err.Error(), "executor = codex in config")
	})

	t.Run("cli_codex_plus_pass_claude_md_succeeds_post_merge", func(t *testing.T) {
		// redundant but valid: both flags on CLI.
		cfg := &config.Config{Executor: "", ExternalReviewTool: "none"}
		o := parseTestOpts(t, "--codex", "--pass-claude-md")
		var warnBuf bytes.Buffer

		require.NoError(t, applyCodexOverrides(o, cfg, &warnBuf))

		assert.True(t, cfg.PassClaudeMd)
		assert.Equal(t, config.ExecutorCodex, cfg.Executor)
	})
}

func TestResolveModelSpecs(t *testing.T) {
	t.Run("resolve_spec_prefers_cli", func(t *testing.T) {
		assert.Equal(t, "cli-model:high", resolveSpec("cli-model:high", "cfg-model:low"))
		assert.Equal(t, "cfg-model:low", resolveSpec("", "cfg-model:low"))
	})

	t.Run("plan_spec_precedence", func(t *testing.T) {
		cfg := &config.Config{PlanModel: "cfg-plan:medium", TaskModel: "cfg-task:low"}

		assert.Equal(t, "cli-plan:high", resolvePlanSpec(opts{PlanModel: "cli-plan:high", TaskModel: "cli-task:xhigh"}, cfg))
		assert.Equal(t, "cfg-plan:medium", resolvePlanSpec(opts{TaskModel: "cli-task:xhigh"}, cfg))
		assert.Equal(t, "cli-task:xhigh", resolvePlanSpec(opts{TaskModel: "cli-task:xhigh"}, &config.Config{TaskModel: "cfg-task:low"}))
		assert.Equal(t, "cfg-task:low", resolvePlanSpec(opts{}, &config.Config{TaskModel: "cfg-task:low"}))
	})

	t.Run("review_spec_precedence", func(t *testing.T) {
		cfg := &config.Config{ReviewModel: "cfg-review:medium", TaskModel: "cfg-task:low"}

		assert.Equal(t, "cli-review:high", resolveReviewSpec(opts{ReviewModel: "cli-review:high", TaskModel: "cli-task:xhigh"}, cfg))
		assert.Equal(t, "cfg-review:medium", resolveReviewSpec(opts{TaskModel: "cli-task:xhigh"}, cfg))
		assert.Equal(t, "cli-task:xhigh", resolveReviewSpec(opts{TaskModel: "cli-task:xhigh"}, &config.Config{TaskModel: "cfg-task:low"}))
		assert.Equal(t, "cfg-task:low", resolveReviewSpec(opts{}, &config.Config{TaskModel: "cfg-task:low"}))
	})
}

func TestExternalReviewChainLabel(t *testing.T) {
	tests := []struct {
		name      string
		selection externalReviewSelection
		want      string
	}{
		{
			name: "single reviewer",
			selection: externalReviewSelection{Reviewers: []resolvedReviewer{
				{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "xhigh"},
			}},
			want: "codex (gpt-5.5:xhigh)",
		},
		{
			name: "reviewer chain",
			selection: externalReviewSelection{Reviewers: []resolvedReviewer{
				{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "xhigh"},
				{Provider: config.ExternalReviewToolClaude, Model: "fable", Effort: "max"},
			}},
			want: "codex (gpt-5.5:xhigh) → claude (fable:max)",
		},
		{
			name: "custom reviewer",
			selection: externalReviewSelection{Reviewers: []resolvedReviewer{
				{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "high"},
				{Provider: config.ExternalReviewToolCustom},
			}},
			want: "codex (gpt-5.5:high) → custom",
		},
		{name: "none", want: config.ExternalReviewToolNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.selection.chainLabel())
		})
	}
}

func TestCmuxRunModels(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		assert.Equal(t, cmux.Models{}, cmuxRunModels(opts{}, nil, externalReviewSelection{}))
	})

	t.Run("claude models use configured review and resolved external model", func(t *testing.T) {
		cfg := &config.Config{ReviewModel: "sonnet:high"}
		externalReview := externalReviewSelection{
			Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.6", Effort: "xhigh"}},
		}

		assert.Equal(t, cmux.Models{
			Plan:           "opus:medium",
			Task:           "opus:medium",
			Review:         "sonnet:high",
			ExternalReview: "gpt-5.6:xhigh",
		}, cmuxRunModels(opts{TaskModel: "opus:medium"}, cfg, externalReview))
	})

	t.Run("external reviewer chain uses joined provider and model labels", func(t *testing.T) {
		externalReview := externalReviewSelection{Reviewers: []resolvedReviewer{
			{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "xhigh"},
			{Provider: config.ExternalReviewToolClaude, Model: "fable", Effort: "max"},
		}}

		got := cmuxRunModels(opts{}, &config.Config{}, externalReview)
		assert.Equal(t, "codex (gpt-5.5:xhigh) → claude (fable:max)", got.ExternalReview)
	})

	t.Run("claude defaults are explicit when no model is configured", func(t *testing.T) {
		assert.Equal(t, cmux.Models{
			Plan:   "claude default",
			Task:   "claude default",
			Review: "claude default",
		}, cmuxRunModels(opts{}, &config.Config{}, externalReviewSelection{}))
	})

	t.Run("codex models match effective executor resolution", func(t *testing.T) {
		cfg := &config.Config{
			Executor:             config.ExecutorCodex,
			CodexModel:           "gpt-5.5",
			CodexReasoningEffort: "xhigh",
			ReviewModel:          "gpt-5.6:medium",
		}

		assert.Equal(t, cmux.Models{
			Plan:   "gpt-5.5:xhigh",
			Task:   "gpt-5.5:xhigh",
			Review: "gpt-5.6:medium",
		}, cmuxRunModels(opts{}, cfg, externalReviewSelection{}))
	})

	t.Run("effort-only spec names the inherited model source", func(t *testing.T) {
		assert.Equal(t, cmux.Models{
			Plan:   "claude default:high",
			Task:   "claude default:high",
			Review: "claude default:high",
		}, cmuxRunModels(opts{}, &config.Config{TaskModel: ":high"}, externalReviewSelection{}))
	})

	t.Run("plan model is resolved independently", func(t *testing.T) {
		cfg := &config.Config{
			PlanModel:   "haiku:low",
			TaskModel:   "opus:high",
			ReviewModel: "sonnet:medium",
		}
		assert.Equal(t, "haiku:low", cmuxRunModels(opts{}, cfg, externalReviewSelection{}).Plan)
	})
}

func TestRunHeaderParams(t *testing.T) {
	t.Run("nil config returns empty params", func(t *testing.T) {
		got := runHeaderParams(opts{}, nil, processor.ModeFull)
		assert.Equal(t, progress.RunParams{}, got)
	})

	t.Run("nothing set returns empty params", func(t *testing.T) {
		got := runHeaderParams(parseTestOpts(t), &config.Config{}, processor.ModeFull)
		assert.Equal(t, progress.RunParams{}, got)
	})

	t.Run("cli task and review models", func(t *testing.T) {
		got := runHeaderParams(parseTestOpts(t, "--task-model", "opus:high", "--review-model", "sonnet:low"), &config.Config{}, processor.ModeFull)
		assert.Equal(t, progress.RunParams{TaskModel: "opus:high", ReviewModel: "sonnet:low"}, got)
	})

	t.Run("cli flags override config values", func(t *testing.T) {
		cfg := &config.Config{TaskModel: "sonnet", ReviewModel: "haiku"}
		got := runHeaderParams(parseTestOpts(t, "--task-model", "opus"), cfg, processor.ModeFull)
		assert.Equal(t, progress.RunParams{TaskModel: "opus", ReviewModel: "haiku"}, got)
	})

	t.Run("review model fallback to task is not recorded", func(t *testing.T) {
		got := runHeaderParams(parseTestOpts(t, "--task-model", "opus"), &config.Config{}, processor.ModeFull)
		assert.Equal(t, progress.RunParams{TaskModel: "opus"}, got, "review inherits task implicitly, no separate header line")
	})

	t.Run("codex executor recorded", func(t *testing.T) {
		cfg := &config.Config{Executor: config.ExecutorCodex}
		got := runHeaderParams(parseTestOpts(t, "--codex"), cfg, processor.ModeFull)
		assert.Equal(t, progress.RunParams{Executor: "codex"}, got)
	})

	t.Run("plan mode records effective plan model", func(t *testing.T) {
		got := runHeaderParams(parseTestOpts(t, "--plan-model", "opus:high"), &config.Config{}, processor.ModePlan)
		assert.Equal(t, progress.RunParams{PlanModel: "opus:high"}, got)
	})

	t.Run("plan mode falls back to task model", func(t *testing.T) {
		got := runHeaderParams(parseTestOpts(t, "--task-model", "opus"), &config.Config{}, processor.ModePlan)
		assert.Equal(t, progress.RunParams{PlanModel: "opus"}, got, "plan_model falls back to task_model by design")
	})

	t.Run("external model is distinct from primary review model", func(t *testing.T) {
		external := externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolClaude, Model: "opus", Effort: "xhigh"}}, AutoSelected: true}
		got := runHeaderParams(parseTestOpts(t, "--review-model", "gpt-5.5:low"),
			&config.Config{Executor: config.ExecutorCodex}, processor.ModeFull, external)
		assert.Equal(t, "gpt-5.5:low", got.ReviewModel)
		assert.Equal(t, "claude (auto-selected)", got.ExternalReview)
		assert.Equal(t, "opus:xhigh", got.ExternalReviewModel)
	})

	t.Run("external reviewer chain is recorded as one label", func(t *testing.T) {
		external := externalReviewSelection{Reviewers: []resolvedReviewer{
			{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "xhigh"},
			{Provider: config.ExternalReviewToolClaude, Model: "fable", Effort: "max"},
		}}

		got := runHeaderParams(opts{}, &config.Config{}, processor.ModeFull, external)
		assert.Equal(t, "codex (gpt-5.5:xhigh) → claude (fable:max)", got.ExternalReview)
		assert.Empty(t, got.ExternalReviewModel)
		assert.Equal(t,
			"external codex (gpt-5.5:xhigh) → claude (fable:max)",
			web.FormatRunParams(got.Executor, got.PlanModel, got.TaskModel, got.ReviewModel, got.ExternalReview, got.ExternalReviewModel),
		)
	})

	t.Run("resolved disabled review is recorded", func(t *testing.T) {
		selection, err := resolveExternalReviewSelection(opts{}, &config.Config{ExternalReviewTool: config.ExternalReviewToolNone}, processor.ModeFull)
		require.NoError(t, err)

		got := runHeaderParams(opts{}, &config.Config{}, processor.ModeFull, selection)
		assert.Equal(t, config.ExternalReviewToolNone, got.ExternalReview)
		assert.Empty(t, got.ExternalReviewModel)
	})

	t.Run("tasks only records disabled review", func(t *testing.T) {
		selection, err := resolveExternalReviewSelection(opts{}, &config.Config{ExternalReviewTool: config.ExternalReviewToolClaude}, processor.ModeTasksOnly)
		require.NoError(t, err)

		got := runHeaderParams(opts{}, &config.Config{}, processor.ModeTasksOnly, selection)
		assert.Equal(t, config.ExternalReviewToolNone, got.ExternalReview)
	})
}

func TestCodexModelBanner(t *testing.T) {
	t.Run("task_model_sets_task_and_review", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexModelBanner(parseTestOpts(t, "--codex", "--task-model", "gpt-5.6"), cfg)

		assert.Equal(t, "gpt-5.6", got.taskModel)
		assert.Equal(t, "xhigh", got.taskEffort, "effort inherits config when spec has no effort part")
		assert.Equal(t, "gpt-5.6", got.reviewModel, "review falls back to task when no --review-model")
		assert.Equal(t, "xhigh", got.reviewEffort)
		assert.False(t, got.maxDropped)
	})

	t.Run("task_model_with_effort", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexModelBanner(parseTestOpts(t, "--codex", "--task-model", "gpt-5.6:high"), cfg)

		assert.Equal(t, "gpt-5.6", got.taskModel)
		assert.Equal(t, "high", got.taskEffort)
		assert.Equal(t, "gpt-5.6", got.reviewModel)
		assert.Equal(t, "high", got.reviewEffort)
	})

	t.Run("effort_only_task_spec", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexModelBanner(parseTestOpts(t, "--codex", "--task-model", ":medium"), cfg)

		assert.Equal(t, "gpt-5.5", got.taskModel, "model inherits config for effort-only spec")
		assert.Equal(t, "medium", got.taskEffort)
	})

	t.Run("separate_review_model_differs_from_task", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexModelBanner(parseTestOpts(t, "--codex", "--task-model", "gpt-5.6:high", "--review-model", "gpt-5.5:low"), cfg)

		assert.Equal(t, "gpt-5.6", got.taskModel)
		assert.Equal(t, "high", got.taskEffort)
		assert.Equal(t, "gpt-5.5", got.reviewModel)
		assert.Equal(t, "low", got.reviewEffort)
	})

	t.Run("review_model_only_leaves_task_at_config_default", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexModelBanner(parseTestOpts(t, "--codex", "--review-model", "gpt-5.6:low"), cfg)

		assert.Equal(t, "gpt-5.5", got.taskModel, "task untouched by --review-model")
		assert.Equal(t, "xhigh", got.taskEffort)
		assert.Equal(t, "gpt-5.6", got.reviewModel)
		assert.Equal(t, "low", got.reviewEffort)
	})

	t.Run("max_effort_sets_max_dropped", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexModelBanner(parseTestOpts(t, "--codex", "--task-model", "gpt-5.6:max"), cfg)

		assert.Equal(t, "gpt-5.6", got.taskModel, "model still applied")
		assert.Equal(t, "xhigh", got.taskEffort, "max effort not applied")
		assert.True(t, got.maxDropped)
	})

	t.Run("max_in_review_model_sets_max_dropped", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexModelBanner(parseTestOpts(t, "--codex", "--task-model", "gpt-5.6:high", "--review-model", ":max"), cfg)

		assert.Equal(t, "high", got.taskEffort)
		assert.Equal(t, "xhigh", got.reviewEffort, "max effort not applied to review")
		assert.True(t, got.maxDropped)
	})

	t.Run("no_flags_uses_codex_config_defaults", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexModelBanner(parseTestOpts(t, "--codex"), cfg)

		assert.Equal(t, "gpt-5.5", got.taskModel)
		assert.Equal(t, "xhigh", got.taskEffort)
		assert.Equal(t, "gpt-5.5", got.reviewModel)
		assert.Equal(t, "xhigh", got.reviewEffort)
		assert.False(t, got.maxDropped)
	})

	t.Run("config_task_model_used_without_cli_flag", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh", TaskModel: "gpt-5.6:low"}
		got := codexModelBanner(parseTestOpts(t, "--codex"), cfg)

		assert.Equal(t, "gpt-5.6", got.taskModel)
		assert.Equal(t, "low", got.taskEffort)
	})

	t.Run("config_review_model_used_without_cli_flag", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh", ReviewModel: "gpt-5.6:low"}
		got := codexModelBanner(parseTestOpts(t, "--codex"), cfg)

		assert.Equal(t, "gpt-5.5", got.taskModel, "task untouched by review_model")
		assert.Equal(t, "gpt-5.6", got.reviewModel)
		assert.Equal(t, "low", got.reviewEffort)
	})

	t.Run("cli_task_model_overrides_config", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh", TaskModel: "gpt-5.6:low"}
		got := codexModelBanner(parseTestOpts(t, "--codex", "--task-model", "gpt-5.7:high"), cfg)

		assert.Equal(t, "gpt-5.7", got.taskModel)
		assert.Equal(t, "high", got.taskEffort)
	})
}

func TestCodexPlanBanner(t *testing.T) {
	t.Run("plan_model_sets_plan_executor", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh", PlanModel: "gpt-5.6:high", TaskModel: "gpt-5.5:low"}
		got := codexPlanBanner(parseTestOpts(t, "--codex"), cfg)

		assert.Equal(t, "gpt-5.6", got.taskModel)
		assert.Equal(t, "high", got.taskEffort)
		assert.Equal(t, got.taskModel, got.reviewModel)
		assert.Equal(t, got.taskEffort, got.reviewEffort)
		assert.False(t, got.maxDropped)
	})

	t.Run("plan_model_falls_back_to_task_model", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh", TaskModel: "gpt-5.6:low"}
		got := codexPlanBanner(parseTestOpts(t, "--codex"), cfg)

		assert.Equal(t, "gpt-5.6", got.taskModel)
		assert.Equal(t, "low", got.taskEffort)
	})

	t.Run("plan_model_falls_back_to_cli_task_model", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexPlanBanner(parseTestOpts(t, "--codex", "--task-model", "gpt-5.7:high"), cfg)

		assert.Equal(t, "gpt-5.7", got.taskModel)
		assert.Equal(t, "high", got.taskEffort)
	})

	t.Run("cli_plan_model_overrides_config_and_task_model", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh", PlanModel: "gpt-5.6:high", TaskModel: "gpt-5.5:low"}
		got := codexPlanBanner(parseTestOpts(t, "--codex", "--plan-model", "gpt-5.7:medium"), cfg)

		assert.Equal(t, "gpt-5.7", got.taskModel)
		assert.Equal(t, "medium", got.taskEffort)
	})

	t.Run("max_effort_sets_max_dropped", func(t *testing.T) {
		cfg := &config.Config{CodexModel: "gpt-5.5", CodexReasoningEffort: "xhigh"}
		got := codexPlanBanner(parseTestOpts(t, "--codex", "--plan-model", "gpt-5.6:max"), cfg)

		assert.Equal(t, "gpt-5.6", got.taskModel)
		assert.Equal(t, "xhigh", got.taskEffort)
		assert.True(t, got.maxDropped)
	})
}

func TestPrintStartupInfo(t *testing.T) {
	colors := testColors()

	t.Run("prints_plan_info_for_full_mode", func(t *testing.T) {
		info := startupInfo{
			PlanFile:      "/path/to/plan.md",
			Branch:        "feature-branch",
			Mode:          processor.ModeFull,
			MaxIterations: 50,
			ProgressPath:  "progress.txt",
		}
		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.Contains(t, out, "starting loopai loop")
		assert.NotContains(t, out, "starting ralphex loop")
	})

	t.Run("prints_no_plan_for_review_mode", func(t *testing.T) {
		info := startupInfo{
			PlanFile:      "",
			Branch:        "test-branch",
			Mode:          processor.ModeReview,
			MaxIterations: 50,
			ProgressPath:  "progress-review.txt",
		}
		// verify it doesn't panic with empty plan
		printStartupInfo(info, colors)
	})

	t.Run("shows auth passthrough line when preserve enabled", func(t *testing.T) {
		info := startupInfo{
			PlanFile:                "/path/to/plan.md",
			Branch:                  "feature-branch",
			Mode:                    processor.ModeFull,
			MaxIterations:           50,
			ProgressPath:            "progress.txt",
			PreserveAnthropicAPIKey: true,
		}
		out := captureStdout(t, func() {
			printStartupInfo(info, colors)
		})
		assert.Contains(t, out, "ANTHROPIC_API_KEY passthrough enabled",
			"banner must surface API key passthrough so users notice wrong-context runs")
	})

	t.Run("hides auth line when preserve disabled", func(t *testing.T) {
		info := startupInfo{
			PlanFile:                "/path/to/plan.md",
			Branch:                  "feature-branch",
			Mode:                    processor.ModeFull,
			MaxIterations:           50,
			ProgressPath:            "progress.txt",
			PreserveAnthropicAPIKey: false,
		}
		out := captureStdout(t, func() {
			printStartupInfo(info, colors)
		})
		assert.NotContains(t, out, "passthrough", "no auth line when default-strip behavior")
	})

	t.Run("shows auth passthrough line in plan mode when preserve enabled", func(t *testing.T) {
		// plan mode has its own early-return branch in printStartupInfo; the auth
		// line must surface there too because passthrough is the only safety
		// signal once the run is on the wrong account.
		info := startupInfo{
			PlanDescription:         "add health endpoint",
			Branch:                  "plan-branch",
			Mode:                    processor.ModePlan,
			MaxIterations:           50,
			ProgressPath:            "progress.txt",
			PreserveAnthropicAPIKey: true,
		}
		out := captureStdout(t, func() {
			printStartupInfo(info, colors)
		})
		assert.Contains(t, out, "ANTHROPIC_API_KEY passthrough enabled",
			"plan mode banner must surface API key passthrough")
	})

	t.Run("shows codex executor line when enabled", func(t *testing.T) {
		info := startupInfo{
			PlanFile:      "/path/to/plan.md",
			Branch:        "feature-branch",
			Mode:          processor.ModeFull,
			MaxIterations: 50,
			ProgressPath:  "progress.txt",
			Executor:      config.ExecutorCodex,
		}
		out := captureStdout(t, func() {
			printStartupInfo(info, colors)
		})
		assert.Contains(t, out, "executor: codex")
		assert.NotContains(t, out, "external review skipped")
	})

	t.Run("shows effective auto-selected external provider and model", func(t *testing.T) {
		info := startupInfo{
			PlanFile:      "/path/to/plan.md",
			Branch:        "feature-branch",
			Mode:          processor.ModeFull,
			MaxIterations: 50,
			ProgressPath:  "progress.txt",
			Executor:      config.ExecutorCodex,
			ExternalReview: externalReviewSelection{
				Reviewers: []resolvedReviewer{{Provider: config.ExternalReviewToolClaude, Model: "opus", Effort: "xhigh"}}, AutoSelected: true,
			},
		}
		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.Contains(t, out, "external review: claude (auto-selected)")
		assert.Contains(t, out, "model: opus")
		assert.Contains(t, out, "reasoning effort: xhigh")
	})

	t.Run("shows external reviewer chain as one label", func(t *testing.T) {
		info := startupInfo{
			PlanFile:      "/path/to/plan.md",
			Branch:        "feature-branch",
			Mode:          processor.ModeFull,
			MaxIterations: 50,
			ProgressPath:  "progress.txt",
			ExternalReview: externalReviewSelection{Reviewers: []resolvedReviewer{
				{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "xhigh"},
				{Provider: config.ExternalReviewToolClaude, Model: "fable", Effort: "max"},
			}},
		}
		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.Contains(t, out, "external review: codex (gpt-5.5:xhigh) → claude (fable:max)")
		assert.NotContains(t, out, "\n  model:")
		assert.NotContains(t, out, "\n  reasoning effort:")
	})

	t.Run("shows resolved disabled external review", func(t *testing.T) {
		info := startupInfo{
			Mode:           processor.ModeTasksOnly,
			MaxIterations:  50,
			ExternalReview: externalReviewSelection{Resolved: true},
		}

		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.Contains(t, out, "external review: none")
	})

	t.Run("shows claude md passthrough line when enabled", func(t *testing.T) {
		info := startupInfo{
			PlanFile:      "/path/to/plan.md",
			Branch:        "feature-branch",
			Mode:          processor.ModeFull,
			MaxIterations: 50,
			ProgressPath:  "progress.txt",
			Executor:      config.ExecutorCodex,
			PassClaudeMd:  true,
		}
		out := captureStdout(t, func() {
			printStartupInfo(info, colors)
		})
		assert.Contains(t, out, "claude.md: project CLAUDE.md passthrough enabled")
	})

	t.Run("hides executor line for default claude", func(t *testing.T) {
		info := startupInfo{
			PlanFile:      "/path/to/plan.md",
			Branch:        "feature-branch",
			Mode:          processor.ModeFull,
			MaxIterations: 50,
			ProgressPath:  "progress.txt",
		}
		out := captureStdout(t, func() {
			printStartupInfo(info, colors)
		})
		assert.NotContains(t, out, "executor:")
	})

	t.Run("shows codex detail lines when config fields are set", func(t *testing.T) {
		info := startupInfo{
			PlanFile:      "/path/to/plan.md",
			Branch:        "feature-branch",
			Mode:          processor.ModeFull,
			MaxIterations: 50,
			ProgressPath:  "progress.txt",
			Executor:      config.ExecutorCodex,
			CodexModel:    "gpt-5.5",
			CodexSandbox:  "danger-full-access",
			CodexEffort:   "xhigh",
		}
		out := captureStdout(t, func() {
			printStartupInfo(info, colors)
		})
		assert.Contains(t, out, "model: gpt-5.5")
		assert.Contains(t, out, "sandbox: danger-full-access")
		assert.Contains(t, out, "reasoning effort: xhigh")
	})

	t.Run("omits empty codex detail lines so codex resolves them itself", func(t *testing.T) {
		// empty CodexModel/CodexEffort mean ralphex did not override them — the
		// banner must stay silent so codex's own resolved header surfaces them.
		info := startupInfo{
			PlanFile:      "/path/to/plan.md",
			Branch:        "feature-branch",
			Mode:          processor.ModeFull,
			MaxIterations: 50,
			ProgressPath:  "progress.txt",
			Executor:      config.ExecutorCodex,
			CodexSandbox:  "read-only",
		}
		out := captureStdout(t, func() {
			printStartupInfo(info, colors)
		})
		assert.NotContains(t, out, "model:")
		assert.NotContains(t, out, "reasoning effort:")
		assert.Contains(t, out, "sandbox: read-only", "sandbox is always resolved, so it is always shown")
	})

	t.Run("shows review model and effort lines when they differ from task", func(t *testing.T) {
		info := startupInfo{
			PlanFile: "/path/to/plan.md", Branch: "feature-branch", Mode: processor.ModeFull, MaxIterations: 50,
			ProgressPath: "progress.txt", Executor: config.ExecutorCodex, CodexSandbox: "danger-full-access",
			CodexModel: "gpt-5.6", CodexEffort: "high", CodexReviewModel: "gpt-5.5", CodexReviewEffort: "low",
		}
		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.Contains(t, out, "model: gpt-5.6")
		assert.Contains(t, out, "reasoning effort: high")
		assert.Contains(t, out, "review model: gpt-5.5")
		assert.Contains(t, out, "review reasoning effort: low")
	})

	t.Run("omits review lines when review matches task", func(t *testing.T) {
		info := startupInfo{
			PlanFile: "/path/to/plan.md", Branch: "feature-branch", Mode: processor.ModeFull, MaxIterations: 50,
			ProgressPath: "progress.txt", Executor: config.ExecutorCodex, CodexSandbox: "danger-full-access",
			CodexModel: "gpt-5.5", CodexEffort: "xhigh", CodexReviewModel: "gpt-5.5", CodexReviewEffort: "xhigh",
		}
		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.NotContains(t, out, "review model:")
		assert.NotContains(t, out, "review reasoning effort:")
	})

	t.Run("shows only review effort line when review model matches but effort differs", func(t *testing.T) {
		info := startupInfo{
			PlanFile: "/path/to/plan.md", Branch: "feature-branch", Mode: processor.ModeFull, MaxIterations: 50,
			ProgressPath: "progress.txt", Executor: config.ExecutorCodex, CodexSandbox: "danger-full-access",
			CodexModel: "gpt-5.5", CodexEffort: "high", CodexReviewModel: "gpt-5.5", CodexReviewEffort: "low",
		}
		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.NotContains(t, out, "review model:", "review model line omitted when model matches task")
		assert.Contains(t, out, "review reasoning effort: low")
	})

	t.Run("shows only review model line when model differs but effort matches", func(t *testing.T) {
		info := startupInfo{
			PlanFile: "/path/to/plan.md", Branch: "feature-branch", Mode: processor.ModeFull, MaxIterations: 50,
			ProgressPath: "progress.txt", Executor: config.ExecutorCodex, CodexSandbox: "danger-full-access",
			CodexModel: "gpt-5.6", CodexEffort: "xhigh", CodexReviewModel: "gpt-5.5", CodexReviewEffort: "xhigh",
		}
		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.Contains(t, out, "review model: gpt-5.5")
		assert.NotContains(t, out, "review reasoning effort:", "review effort line omitted when effort matches task")
	})

	t.Run("labels empty review value that differs from a set task value", func(t *testing.T) {
		// review effort empty (inherits ~/.codex/config.toml) but task effort set:
		// the line must render explicitly so the banner does not imply review reuses task.
		info := startupInfo{
			PlanFile: "/path/to/plan.md", Branch: "feature-branch", Mode: processor.ModeFull, MaxIterations: 50,
			ProgressPath: "progress.txt", Executor: config.ExecutorCodex, CodexSandbox: "danger-full-access",
			CodexModel: "gpt-5.6", CodexEffort: "high", CodexReviewModel: "gpt-5.6", CodexReviewEffort: "",
		}
		out := captureStdout(t, func() { printStartupInfo(info, colors) })
		assert.Contains(t, out, "review reasoning effort: (inherits ~/.codex/config.toml)")
		assert.NotContains(t, out, "review model:", "review model line omitted when model matches task")
	})
}

func TestToRelPath(t *testing.T) {
	// toRelPath uses filepath.Rel with resolved symlinks, so we need real paths.
	// use t.TempDir, chdir into it, then build absolute paths using Getwd
	// (same way plan.Select uses filepath.Abs which calls Getwd).
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(tmpDir))
	t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

	// use Getwd to get the resolved cwd (same as filepath.Abs would)
	cwd, err := os.Getwd()
	require.NoError(t, err)

	t.Run("converts_absolute_to_relative", func(t *testing.T) {
		absPath := filepath.Join(cwd, "docs", "plans", "feature.md")
		result := toRelPath(absPath)
		assert.Equal(t, filepath.Join("docs", "plans", "feature.md"), result)
		assert.False(t, filepath.IsAbs(result), "path should be relative, got: %s", result)
	})

	t.Run("converts_absolute_completed_path", func(t *testing.T) {
		absPath := filepath.Join(cwd, "docs", "plans", "completed", "feature.md")
		result := toRelPath(absPath)
		assert.Equal(t, filepath.Join("docs", "plans", "completed", "feature.md"), result)
		assert.False(t, filepath.IsAbs(result), "path should be relative, got: %s", result)
	})

	t.Run("keeps_relative_path_as_is", func(t *testing.T) {
		result := toRelPath("docs/plans/feature.md")
		assert.Equal(t, "docs/plans/feature.md", result)
	})

	t.Run("handles_path_outside_cwd", func(t *testing.T) {
		result := toRelPath("/some/other/project/plan.md")
		assert.NotEmpty(t, result)
	})
}

// noopLogger returns a no-op git.Logger for tests using moq-generated mock.
func noopLogger() *gitmocks.LoggerMock {
	return &gitmocks.LoggerMock{
		PrintfFunc: func(string, ...any) (int, error) { return 0, nil },
	}
}

func TestEnsureRepoHasCommits(t *testing.T) {
	t.Run("returns nil for repo with commits", func(t *testing.T) {
		dir := setupTestRepo(t)
		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		var stdout bytes.Buffer
		err = ensureRepoHasCommits(t.Context(), gitSvc, strings.NewReader(""), &stdout)
		assert.NoError(t, err)
	})

	t.Run("creates commit when user answers yes", func(t *testing.T) {
		dir := initEmptyRepo(t)

		// create a file so there's something to commit
		err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o600)
		require.NoError(t, err)

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		// verify no commits before
		hasCommits, err := gitSvc.HasCommits()
		require.NoError(t, err)
		assert.False(t, hasCommits)

		var stdout bytes.Buffer
		err = ensureRepoHasCommits(t.Context(), gitSvc, strings.NewReader("y\n"), &stdout)
		require.NoError(t, err)

		// verify commit was created
		hasCommits, err = gitSvc.HasCommits()
		require.NoError(t, err)
		assert.True(t, hasCommits)

		// verify output
		assert.Contains(t, stdout.String(), "repository has no commits")
		assert.Contains(t, stdout.String(), "loopai needs at least one commit")
		assert.Contains(t, stdout.String(), "created initial commit")
	})

	t.Run("returns error when user answers no", func(t *testing.T) {
		dir := initEmptyRepo(t)

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		var stdout bytes.Buffer
		err = ensureRepoHasCommits(t.Context(), gitSvc, strings.NewReader("n\n"), &stdout)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no commits - please create initial commit manually")
	})

	t.Run("returns error on EOF", func(t *testing.T) {
		dir := initEmptyRepo(t)

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		var stdout bytes.Buffer
		err = ensureRepoHasCommits(t.Context(), gitSvc, strings.NewReader(""), &stdout)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no commits - please create initial commit manually")
	})

	t.Run("returns error when no files to commit", func(t *testing.T) {
		dir := initEmptyRepo(t)

		// no files created - empty repo

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		var stdout bytes.Buffer
		err = ensureRepoHasCommits(t.Context(), gitSvc, strings.NewReader("y\n"), &stdout)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create initial commit")
	})

	t.Run("returns error when context canceled", func(t *testing.T) {
		dir := initEmptyRepo(t)

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		cancel() // cancel immediately

		var stdout bytes.Buffer
		err = ensureRepoHasCommits(ctx, gitSvc, strings.NewReader("y\n"), &stdout)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestTasksOnlyModeBranchCreation(t *testing.T) {
	t.Run("tasks_only_creates_branch_for_plan", func(t *testing.T) {
		skipIfClaudeNotAvailable(t)
		configDir := t.TempDir() // isolate from global config

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create plans dir and plan file, then commit them
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "test-plan.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Test Plan\n\n## Tasks\n\n- [ ] task 1\n"), 0o600))

		// commit the plan file so branch creation doesn't fail due to uncommitted changes
		runGit(t, dir, "add", "docs/plans/test-plan.md")
		runGit(t, dir, "commit", "-m", "add test plan")

		// run with tasks-only mode in background
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			o := opts{TasksOnly: true, PlanFile: planPath, MaxIterations: 1, ConfigDir: configDir}
			_ = run(ctx, o)
		}()

		// verify branch was created (branch name derived from plan filename)
		require.Eventually(t, func() bool {
			gitSvc, err := git.NewService(dir, testColors().Info())
			if err != nil {
				return false
			}
			branch, err := gitSvc.CurrentBranch()
			return err == nil && branch == "test-plan"
		}, 3*time.Second, 100*time.Millisecond, "tasks-only mode should create branch for plan")

		cancel()
		<-done
	})

	t.Run("review_mode_does_not_create_branch", func(t *testing.T) {
		skipIfClaudeNotAvailable(t)
		configDir := t.TempDir() // isolate from global config

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create plans dir and plan file, then commit them
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "review-plan.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Review Plan\n"), 0o600))

		runGit(t, dir, "add", "docs/plans/review-plan.md")
		runGit(t, dir, "commit", "-m", "add review plan")

		// run with review mode, cancel immediately and wait for exit
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		o := opts{Review: true, PlanFile: planPath, MaxIterations: 1, ConfigDir: configDir}
		_ = run(ctx, o)

		// verify branch was NOT created (still on master) 24d70519 (fix: isolate TestTasksOnlyModeBranchCreation from global config)
		gitSvc, err := git.NewService(dir, testColors().Info())
		require.NoError(t, err)
		branch, err := gitSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "master", branch, "review mode should not create branch")
	})

	t.Run("codex_only_mode_does_not_create_branch", func(t *testing.T) {
		skipIfClaudeNotAvailable(t)
		configDir := t.TempDir() // isolate from global config

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create plans dir and plan file, then commit them
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "codex-plan.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Codex Plan\n"), 0o600))

		runGit(t, dir, "add", "docs/plans/codex-plan.md")
		runGit(t, dir, "commit", "-m", "add codex plan")

		// run with codex-only mode, cancel immediately and wait for exit
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		o := opts{CodexOnly: true, PlanFile: planPath, MaxIterations: 1, ConfigDir: configDir}
		_ = run(ctx, o)

		// verify branch was NOT created (still on master) 24d70519 (fix: isolate TestTasksOnlyModeBranchCreation from global config)
		gitSvc, err := git.NewService(dir, testColors().Info())
		require.NoError(t, err)
		branch, err := gitSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "master", branch, "codex-only mode should not create branch")
	})

	t.Run("external_only_mode_does_not_create_branch", func(t *testing.T) {
		skipIfClaudeNotAvailable(t)
		configDir := t.TempDir() // isolate from global config

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		err = os.Chdir(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create plans dir and plan file, then commit them
		require.NoError(t, os.MkdirAll("docs/plans", 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "external-plan.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# External Plan\n"), 0o600))

		runGit(t, dir, "add", "docs/plans/external-plan.md")
		runGit(t, dir, "commit", "-m", "add external plan")

		// run with external-only mode, cancel immediately and wait for exit
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		o := opts{ExternalOnly: true, PlanFile: planPath, MaxIterations: 1, ConfigDir: configDir}
		_ = run(ctx, o)

		// verify branch was NOT created (still on master) 24d70519 (fix: isolate TestTasksOnlyModeBranchCreation from global config)
		gitSvc, err := git.NewService(dir, testColors().Info())
		require.NoError(t, err)
		branch, err := gitSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "master", branch, "external-only mode should not create branch")
	})
}

func TestModeRequiresBranch(t *testing.T) {
	// tests the modeRequiresBranch helper function used for both branch creation and plan-move
	tests := []struct {
		mode     processor.Mode
		expected bool
	}{
		{processor.ModeFull, true},
		{processor.ModeTasksOnly, true},
		{processor.ModeReview, false},
		{processor.ModeCodexOnly, false},
		{processor.ModePlan, false},
	}

	for _, tc := range tests {
		t.Run(string(tc.mode), func(t *testing.T) {
			result := modeRequiresBranch(tc.mode)
			assert.Equal(t, tc.expected, result, "mode %s should return %v", tc.mode, tc.expected)
		})
	}
}

func TestModeCreatesBranch(t *testing.T) {
	// modeCreatesBranch gates whether --base-ref is validated as a branch base. it differs from
	// modeRequiresBranch on plan mode only, which is the whole point: plan creation runs in place
	// but hands off to an implementation run that does create a branch.
	tests := []struct {
		mode     processor.Mode
		expected bool
	}{
		{processor.ModeFull, true},
		{processor.ModeTasksOnly, true},
		{processor.ModePlan, true},
		{processor.ModeReview, false},
		{processor.ModeCodexOnly, false},
	}

	for _, tc := range tests {
		t.Run(string(tc.mode), func(t *testing.T) {
			assert.Equal(t, tc.expected, modeCreatesBranch(tc.mode), "mode %s should return %v", tc.mode, tc.expected)
		})
	}

	assert.True(t, modeCreatesBranch(processor.ModePlan) && !modeRequiresBranch(processor.ModePlan),
		"plan mode is the case that separates the two predicates")
}

func TestShouldMovePlan(t *testing.T) {
	// tests the shouldMovePlan predicate used to guard the plan move call.
	// all three conditions must be true: non-empty plan file, mode requires branch, and config opts in.
	tests := []struct {
		name     string
		req      executePlanRequest
		expected bool
	}{
		{
			name: "empty_plan_file",
			req: executePlanRequest{
				PlanFile: "",
				Mode:     processor.ModeFull,
				Config:   &config.Config{MovePlanOnCompletion: true},
			},
			expected: false,
		},
		{
			name: "mode_does_not_require_branch",
			req: executePlanRequest{
				PlanFile: "docs/plans/x.md",
				Mode:     processor.ModeReview,
				Config:   &config.Config{MovePlanOnCompletion: true},
			},
			expected: false,
		},
		{
			name: "move_plan_on_completion_false",
			req: executePlanRequest{
				PlanFile: "docs/plans/x.md",
				Mode:     processor.ModeFull,
				Config:   &config.Config{MovePlanOnCompletion: false},
			},
			expected: false,
		},
		{
			name: "all_conditions_true_full_mode",
			req: executePlanRequest{
				PlanFile: "docs/plans/x.md",
				Mode:     processor.ModeFull,
				Config:   &config.Config{MovePlanOnCompletion: true},
			},
			expected: true,
		},
		{
			name: "all_conditions_true_tasks_only",
			req: executePlanRequest{
				PlanFile: "docs/plans/x.md",
				Mode:     processor.ModeTasksOnly,
				Config:   &config.Config{MovePlanOnCompletion: true},
			},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := shouldMovePlan(tc.req)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestStderrLog(t *testing.T) {
	// verify stderrLog has Print method with correct signature
	var log stderrLog
	log.Print("test %s %d", "message", 42)
}

func TestNotificationServiceCreation(t *testing.T) {
	t.Run("nil_service_when_no_channels", func(t *testing.T) {
		// run() creates notify service from config.NotifyParams.
		// with default config (no channels), notifySvc should be nil.
		// this is tested indirectly - existing tests call run() which now creates notifySvc.
		// nil service is nil-safe on Send(), so existing tests pass without changes.
		svc, err := notify.New(notify.Params{}, stderrLog{})
		require.NoError(t, err)
		assert.Nil(t, svc)
	})

	t.Run("error_on_misconfigured_channel", func(t *testing.T) {
		// missing required fields should return error (fail fast at startup)
		svc, err := notify.New(notify.Params{
			Channels: []string{"telegram"},
			// missing TelegramToken and TelegramChat
		}, stderrLog{})
		require.Error(t, err)
		assert.Nil(t, svc)
		assert.Contains(t, err.Error(), "telegram")
	})

	t.Run("nil_service_send_is_noop", func(t *testing.T) {
		// verify nil-safe Send doesn't panic
		var svc *notify.Service
		svc.Send(t.Context(), notify.Result{Status: "success"})
	})
}

func TestExecutePlanRequestHasNotifySvc(t *testing.T) {
	// verify the struct has NotifySvc field and it works with nil
	req := executePlanRequest{
		NotifySvc: nil,
	}
	assert.Nil(t, req.NotifySvc)

	// verify nil-safe call through the struct
	req.NotifySvc.Send(t.Context(), notify.Result{Status: "success"})
}

// writeExecutable writes content to path and makes it executable.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	err := os.WriteFile(path, []byte(content), 0o700) //nolint:gosec // test helper needs executable scripts
	require.NoError(t, err)
}

// runGit executes a git command in the given directory and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, out)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, out)
	return string(out)
}

// setupTestRepo creates a test git repository with an initial commit.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	runGit(t, dir, "init")
	runGit(t, dir, "checkout", "-B", "master")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "commit.gpgsign", "false")

	readme := filepath.Join(dir, "README.md")
	err := os.WriteFile(readme, []byte("# Test\n"), 0o600)
	require.NoError(t, err)

	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "initial commit")

	return dir
}

// initEmptyRepo creates a git repo with no commits (for testing ensureRepoHasCommits).
func initEmptyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "checkout", "-B", "master")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func TestConfigDirCustomPath(t *testing.T) {
	t.Run("custom_config_dir_installs_defaults", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgDir := filepath.Join(tmpDir, "custom-config")

		cfg, err := config.Load(cfgDir)
		require.NoError(t, err)
		assert.NotNil(t, cfg)

		// verify defaults were installed to the custom directory
		assert.FileExists(t, filepath.Join(cfgDir, "config"))
		assert.DirExists(t, filepath.Join(cfgDir, "prompts"))
		assert.DirExists(t, filepath.Join(cfgDir, "agents"))
	})

	t.Run("reset_with_custom_dir", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgDir := filepath.Join(tmpDir, "reset-config")

		// first load to install defaults
		_, err := config.Load(cfgDir)
		require.NoError(t, err)
		assert.FileExists(t, filepath.Join(cfgDir, "config"))

		// reset with "y" answers to all prompts
		stdin := strings.NewReader("y\ny\ny\n")
		var stdout bytes.Buffer
		_, err = config.Reset(cfgDir, stdin, &stdout)
		require.NoError(t, err)
		// freshly installed defaults are skipped (already match), verify reset ran against custom dir
		assert.Contains(t, stdout.String(), cfgDir)
		assert.FileExists(t, filepath.Join(cfgDir, "config"))
		assert.DirExists(t, filepath.Join(cfgDir, "prompts"))
		assert.DirExists(t, filepath.Join(cfgDir, "agents"))
	})

	t.Run("run_reset_with_custom_dir", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgDir := filepath.Join(tmpDir, "run-reset-config")

		// first load to install defaults
		_, err := config.Load(cfgDir)
		require.NoError(t, err)

		// exercise runReset directly with mock stdin/stdout
		stdin := strings.NewReader("y\ny\ny\n")
		var stdout bytes.Buffer
		err = runReset(cfgDir, stdin, &stdout)
		require.NoError(t, err)
		assert.FileExists(t, filepath.Join(cfgDir, "config"))
		assert.DirExists(t, filepath.Join(cfgDir, "prompts"))
		assert.DirExists(t, filepath.Join(cfgDir, "agents"))
	})
}

func TestDumpDefaults(t *testing.T) {
	t.Run("extracts_files_to_target_dir", func(t *testing.T) {
		tmpDir := filepath.Join(t.TempDir(), "defaults")
		err := dumpDefaults(tmpDir)
		require.NoError(t, err)

		// verify config exists
		assert.FileExists(t, filepath.Join(tmpDir, "config"))

		// verify specific prompt file exists
		assert.FileExists(t, filepath.Join(tmpDir, "prompts", "task.txt"))
		assert.FileExists(t, filepath.Join(tmpDir, "prompts", "external_claude_review.txt"))
		assert.FileExists(t, filepath.Join(tmpDir, "prompts", "external_claude_eval.txt"))

		// verify specific agent file exists
		assert.FileExists(t, filepath.Join(tmpDir, "agents", "quality.txt"))
	})

	t.Run("config_has_raw_content", func(t *testing.T) {
		tmpDir := filepath.Join(t.TempDir(), "defaults")
		require.NoError(t, dumpDefaults(tmpDir))

		data, err := os.ReadFile(filepath.Join(tmpDir, "config")) //nolint:gosec // test
		require.NoError(t, err)
		assert.Contains(t, string(data), "claude_command")
		// raw content should have uncommented lines
		hasUncommented := false
		for line := range strings.SplitSeq(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				hasUncommented = true
				break
			}
		}
		assert.True(t, hasUncommented, "config should have raw (uncommented) content")
	})

	t.Run("error_on_invalid_path", func(t *testing.T) {
		tmpDir := t.TempDir()
		blockingFile := filepath.Join(tmpDir, "blocker")
		require.NoError(t, os.WriteFile(blockingFile, []byte("file"), 0o600))

		err := dumpDefaults(filepath.Join(blockingFile, "sub"))
		require.Error(t, err)
	})
}

type recordingStatusClearer struct{ calls int }

func (r *recordingStatusClearer) Clear() { r.calls++ }

func TestRunMergeCommand(t *testing.T) {
	makeFeature := func(t *testing.T, dir string) *git.Service {
		t.Helper()
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		return svc
	}

	t.Run("happy path without worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		clearer := &recordingStatusClearer{}
		var output bytes.Buffer

		require.NoError(t, runMergeCommand(t.Context(), svc, "", closeoutTarget{}, clearer, &output))
		assert.Equal(t, "master", currentGitBranch(t, dir))
		assert.False(t, branchExists(t, dir, "feature"))
		assert.FileExists(t, filepath.Join(dir, "feature.txt"))
		assert.Equal(t, 1, clearer.calls)
		assert.Contains(t, output.String(), "feature into master (fast-forward)")
	})

	t.Run("command overrides configured squash and no-commit merge options", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600))
		runGit(t, dir, "add", "base.txt")
		runGit(t, dir, "commit", "-m", "advance base")
		runGit(t, dir, "config", "branch.master.mergeOptions", "--squash --no-commit")
		runGit(t, dir, "checkout", "feature")

		require.NoError(t, runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, &recordingStatusClearer{}, io.Discard))
		assert.Equal(t, "feature\n", gitOutput(t, dir, "show", "master:feature.txt"))
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "status", "--porcelain")))
		assert.False(t, branchExists(t, dir, "feature"))
	})

	t.Run("happy path removes actual feature worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "checkout", "master")
		worktreePath := filepath.Join(dir, ".loopai", "worktrees", "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		svc, err := git.NewService(worktreePath, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		require.NoError(t, runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard))
		assert.NoDirExists(t, worktreePath)
		assert.False(t, branchExists(t, dir, "feature"))
		assert.FileExists(t, filepath.Join(dir, "feature.txt"))
		assert.Equal(t, 1, clearer.calls)
	})

	t.Run("ignored files do not block linked worktree cleanup", func(t *testing.T) {
		dir := setupTestRepo(t)
		makeFeature(t, dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("artifact.tmp\n"), 0o600))
		runGit(t, dir, "checkout", "master")
		worktreePath := filepath.Join(t.TempDir(), "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "artifact.tmp"), []byte("generated\n"), 0o600))
		featureSvc, err := git.NewService(worktreePath, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		require.NoError(t, runMergeCommand(t.Context(), featureSvc, "master", closeoutTarget{}, clearer, io.Discard))
		assert.NoDirExists(t, worktreePath)
		assert.False(t, branchExists(t, dir, "feature"))
		assert.Equal(t, 1, clearer.calls)
	})

	t.Run("restores a third branch borrowed for linked worktree merge", func(t *testing.T) {
		dir := setupTestRepo(t)
		makeFeature(t, dir)
		runGit(t, dir, "checkout", "master")
		runGit(t, dir, "checkout", "-b", "develop")
		worktreePath := filepath.Join(t.TempDir(), "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		featureSvc, err := git.NewService(worktreePath, noopLogger())
		require.NoError(t, err)

		require.NoError(t, runMergeCommand(t.Context(), featureSvc, "master", closeoutTarget{}, &recordingStatusClearer{}, io.Discard))
		assert.Equal(t, "develop", currentGitBranch(t, dir))
		assert.NoDirExists(t, worktreePath)
		assert.False(t, branchExists(t, dir, "feature"))
		assert.Equal(t, "feature\n", gitOutput(t, dir, "show", "master:feature.txt"))
	})

	t.Run("restores detached HEAD borrowed for linked worktree merge", func(t *testing.T) {
		dir := setupTestRepo(t)
		makeFeature(t, dir)
		runGit(t, dir, "checkout", "--detach", "master")
		originalHead := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		worktreePath := filepath.Join(t.TempDir(), "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		featureSvc, err := git.NewService(worktreePath, noopLogger())
		require.NoError(t, err)

		require.NoError(t, runMergeCommand(t.Context(), featureSvc, "master", closeoutTarget{}, &recordingStatusClearer{}, io.Discard))
		assert.Empty(t, currentGitBranch(t, dir))
		assert.Equal(t, originalHead, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.NoDirExists(t, worktreePath)
		assert.False(t, branchExists(t, dir, "feature"))
		assert.Equal(t, "feature\n", gitOutput(t, dir, "show", "master:feature.txt"))
	})

	t.Run("reports an already merged branch accurately", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		runGit(t, dir, "checkout", "master")
		runGit(t, dir, "merge", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600))
		runGit(t, dir, "add", "base.txt")
		runGit(t, dir, "commit", "-m", "advance base")
		runGit(t, dir, "checkout", "feature")
		var output bytes.Buffer

		require.NoError(t, runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, &recordingStatusClearer{}, &output))
		assert.Contains(t, output.String(), "feature into master (already up to date)")
		assert.False(t, branchExists(t, dir, "feature"))
	})

	t.Run("unrelated worktree at conventional path is preserved", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		require.NoError(t, svc.EnsureLocalGitignore())
		worktreePath := filepath.Join(dir, ".loopai", "worktrees", "feature")
		runGit(t, dir, "branch", "unrelated", "master")
		runGit(t, dir, "worktree", "add", worktreePath, "unrelated")
		t.Cleanup(func() { _ = svc.RemoveWorktree(worktreePath) })

		require.NoError(t, runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, &recordingStatusClearer{}, io.Discard))
		assert.DirExists(t, worktreePath)
		assert.Equal(t, "unrelated", currentGitBranch(t, worktreePath))
	})

	t.Run("dirty base worktree is refused before merge", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "checkout", "master")
		worktreePath := filepath.Join(dir, ".loopai", "worktrees", "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		t.Cleanup(func() { _ = mainSvc.RemoveWorktree(worktreePath) })
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty base\n"), 0o600))
		featureSvc, err := git.NewService(worktreePath, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), featureSvc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clean base worktree")
		assert.Equal(t, "master", currentGitBranch(t, dir))
		assert.Equal(t, "feature", currentGitBranch(t, worktreePath))
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("ignored base file that merge would overwrite is preserved", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.env\n"), 0o600))
		runGit(t, dir, "add", ".gitignore")
		runGit(t, dir, "commit", "-m", "ignore local secret")
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.env"), []byte("feature version\n"), 0o600))
		runGit(t, dir, "add", "-f", "secret.env")
		runGit(t, dir, "commit", "-m", "feature adds secret path")
		mainSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.env"), []byte("local ignored data\n"), 0o600))
		worktreePath := filepath.Join(dir, ".loopai", "worktrees", "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		t.Cleanup(func() { _ = mainSvc.RemoveWorktree(worktreePath) })
		featureSvc, err := git.NewService(worktreePath, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), featureSvc, "master", closeoutTarget{}, clearer, io.Discard)
		require.ErrorContains(t, err, "merge \"feature\" into \"master\" failed")
		content, readErr := os.ReadFile(filepath.Join(dir, "secret.env")) //nolint:gosec // test fixture
		require.NoError(t, readErr)
		assert.Equal(t, "local ignored data\n", string(content))
		assert.Equal(t, "master", currentGitBranch(t, dir))
		assert.Equal(t, "feature", currentGitBranch(t, worktreePath))
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("dirty tree is refused", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o600))
		clearer := &recordingStatusClearer{}

		err := runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clean working tree")
		assert.Equal(t, "feature", currentGitBranch(t, dir))
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("untracked files make tree dirty", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("keep\n"), 0o600))
		clearer := &recordingStatusClearer{}

		err := runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clean working tree")
		assert.Equal(t, "feature", currentGitBranch(t, dir))
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("ignored file that checkout would overwrite is refused", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.env\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.env"), []byte("base version\n"), 0o600))
		runGit(t, dir, "add", ".gitignore")
		runGit(t, dir, "add", "-f", "secret.env")
		runGit(t, dir, "commit", "-m", "base secret fixture")
		runGit(t, dir, "checkout", "-b", "feature")
		runGit(t, dir, "rm", "secret.env")
		runGit(t, dir, "commit", "-m", "remove secret from feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.env"), []byte("local ignored data\n"), 0o600))
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.ErrorContains(t, err, "check out base branch")
		assert.Equal(t, "feature", currentGitBranch(t, dir))
		content, readErr := os.ReadFile(filepath.Join(dir, "secret.env")) //nolint:gosec // test fixture
		require.NoError(t, readErr)
		assert.Equal(t, "local ignored data\n", string(content))
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("current branch equal to base is refused", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already the base branch")
		assert.Equal(t, "master", currentGitBranch(t, dir))
		assert.Zero(t, clearer.calls)
	})

	t.Run("conflict restores feature and keeps pill", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("feature\n"), 0o600))
		runGit(t, dir, "commit", "-am", "feature change")
		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o600))
		runGit(t, dir, "commit", "-am", "base change")
		runGit(t, dir, "checkout", "feature")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		require.ErrorIs(t, err, git.ErrMergeConflict)
		assert.Contains(t, err.Error(), "conflicted and was aborted")
		assert.Equal(t, "feature", currentGitBranch(t, dir))
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
		cmd := exec.Command("git", "status", "--porcelain")
		cmd.Dir = dir
		statusOutput, statusErr := cmd.Output()
		require.NoError(t, statusErr)
		assert.Empty(t, strings.TrimSpace(string(statusOutput)))
	})
}

func TestBuildPRTitleBody(t *testing.T) {
	writePlan := func(t *testing.T, root, name, content string) string {
		t.Helper()
		dir := filepath.Join(root, "docs", "plans", "completed")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	t.Run("plan found", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "20260802-feature.md", "# A useful feature\n\n## Overview\n\nAdds the useful behavior.\n\n## Context\n\nDetails.\n")

		title, body, err := buildPRTitleBody(root, "docs/plans", "feature", git.DiffStats{Files: 3, Additions: 12, Deletions: 4})
		require.NoError(t, err)
		assert.Equal(t, "A useful feature", title)
		assert.Contains(t, body, "Adds the useful behavior.")
		assert.Contains(t, body, "- Files changed: 3")
		assert.Contains(t, body, "- Additions: 12")
		assert.Contains(t, body, "- Deletions: 4")
		assert.NotContains(t, body, "Details.")
	})

	t.Run("configured plans dir is used", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "plans", "completed")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "20260802-feature.md"),
			[]byte("# Custom dir feature\n\n## Overview\n\nLives outside docs/plans.\n"), 0o600))

		title, body, err := buildPRTitleBody(root, "plans", "feature", git.DiffStats{Files: 2})
		require.NoError(t, err)
		assert.Equal(t, "Custom dir feature", title)
		assert.Contains(t, body, "Lives outside docs/plans.")
	})

	t.Run("empty plans dir falls back to the default", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "20260802-feature.md", "# Default dir feature\n\n## Overview\n\nStill found.\n")

		title, body, err := buildPRTitleBody(root, "", "feature", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "Default dir feature", title)
		assert.Contains(t, body, "Still found.")
	})

	t.Run("plan missing", func(t *testing.T) {
		title, body, err := buildPRTitleBody(t.TempDir(), "docs/plans", "feature/missing", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "feature/missing", title)
		assert.Equal(t, "## Changes\n\n- Files changed: 0\n- Additions: 0\n- Deletions: 0", body)
	})

	t.Run("plans dir outside the repository degrades to stats only", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "repo")
		require.NoError(t, os.MkdirAll(root, 0o750))
		outside := filepath.Join(parent, "shared-plans", "completed")
		require.NoError(t, os.MkdirAll(outside, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(outside, "20260802-feature.md"),
			[]byte("# Out of tree feature\n\n## Overview\n\nUnreadable from the repository.\n"), 0o600))

		for _, plansDir := range []string{filepath.Join("..", "shared-plans"), filepath.Join(parent, "shared-plans")} {
			title, body, err := buildPRTitleBody(root, plansDir, "feature", git.DiffStats{Files: 2})
			require.NoError(t, err, plansDir)
			assert.Equal(t, "feature", title, plansDir)
			assert.Equal(t, "## Changes\n\n- Files changed: 2\n- Additions: 0\n- Deletions: 0", body, plansDir)
		}
	})

	t.Run("no Overview section", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "feature.md", "# Feature without overview\n\n## Context\n\nOnly context.\n")

		title, body, err := buildPRTitleBody(root, "docs/plans", "feature", git.DiffStats{Files: 1})
		require.NoError(t, err)
		assert.Equal(t, "Feature without overview", title)
		assert.NotContains(t, body, "Only context.")
		assert.Contains(t, body, "- Files changed: 1")
	})

	t.Run("newest plan mentioning branch is fallback", func(t *testing.T) {
		root := t.TempDir()
		oldPath := writePlan(t, root, "old.md", "# Old plan\n\nReferences special-branch.\n")
		newPath := writePlan(t, root, "new.md", "# New plan\n\nReferences special-branch.\n")
		oldTime := time.Now().Add(-time.Hour)
		require.NoError(t, os.Chtimes(oldPath, oldTime, oldTime))
		newTime := time.Now()
		require.NoError(t, os.Chtimes(newPath, newTime, newTime))

		title, _, err := buildPRTitleBody(root, "docs/plans", "special-branch", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "New plan", title)
	})

	t.Run("fallback requires a delimited branch name", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "unrelated.md", "# Unrelated plan\n\nReferences prefix behavior, fix.release, fixfix, and authentication.\n")

		title, _, err := buildPRTitleBody(root, "docs/plans", "fix", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "fix", title)
	})

	t.Run("associated symlinked plan is rejected without reading its target", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.md")
		require.NoError(t, os.WriteFile(outside, []byte("# Secret title\n\n## Overview\n\nSecret body.\n"), 0o600))
		plansDir := filepath.Join(root, "docs", "plans", "completed")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		require.NoError(t, os.Symlink(outside, filepath.Join(plansDir, "20260802-feature.md")))

		title, body, err := buildPRTitleBody(root, "docs/plans", "feature", git.DiffStats{})
		require.ErrorContains(t, err, "symlink")
		assert.Empty(t, title)
		assert.Empty(t, body)
	})

	t.Run("oversized plan is rejected", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "20260802-feature.md", strings.Repeat("x", int(maxPRPlanSize)+1))

		_, _, err := buildPRTitleBody(root, "docs/plans", "feature", git.DiffStats{})
		require.ErrorContains(t, err, "size limit")
	})

	t.Run("active exact plan beats completed textual fallback", func(t *testing.T) {
		root := t.TempDir()
		exactDir := filepath.Join(root, "docs", "plans")
		require.NoError(t, os.MkdirAll(exactDir, 0o750))
		exactPath := filepath.Join(exactDir, "20260802-special-branch.md")
		require.NoError(t, os.WriteFile(exactPath, []byte("# Exact active plan\n\n## Overview\n\nExact.\n"), 0o600))
		fallbackPath := writePlan(t, root, "newer.md", "# Newer fallback\n\nReferences special-branch.\n")
		oldTime := time.Now().Add(-time.Hour)
		require.NoError(t, os.Chtimes(exactPath, oldTime, oldTime))
		newTime := time.Now()
		require.NoError(t, os.Chtimes(fallbackPath, newTime, newTime))

		title, body, err := buildPRTitleBody(root, "docs/plans", "special-branch", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "Exact active plan", title)
		assert.Contains(t, body, "Exact.")
	})

	t.Run("directory matching exact plan name is ignored", func(t *testing.T) {
		root := t.TempDir()
		completedDir := filepath.Join(root, "docs", "plans", "completed")
		require.NoError(t, os.MkdirAll(filepath.Join(completedDir, "20260802-feature.md"), 0o750))
		activeDir := filepath.Join(root, "docs", "plans")
		require.NoError(t, os.WriteFile(filepath.Join(activeDir, "20260802-feature.md"),
			[]byte("# Active feature plan\n\n## Overview\n\nSelected.\n"), 0o600))

		title, body, err := buildPRTitleBody(root, "docs/plans", "feature", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "Active feature plan", title)
		assert.Contains(t, body, "Selected.")
	})

	t.Run("unrelated invalid plans do not block exact association", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "20260802-feature.md", "# Feature plan\n\n## Overview\n\nSelected.\n")
		outside := filepath.Join(t.TempDir(), "outside.md")
		require.NoError(t, os.WriteFile(outside, []byte("# Secret\n"), 0o600))
		require.NoError(t, os.Symlink(outside,
			filepath.Join(root, "docs", "plans", "completed", "unrelated.md")))

		title, body, err := buildPRTitleBody(root, "docs/plans", "feature", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "Feature plan", title)
		assert.Contains(t, body, "Selected.")
	})

	t.Run("unrelated invalid plans are skipped during textual fallback", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "valid.md", "# Valid fallback\n\nReferences special-branch.\n")
		outside := filepath.Join(t.TempDir(), "outside.md")
		require.NoError(t, os.WriteFile(outside, []byte("# Secret\n"), 0o600))
		require.NoError(t, os.Symlink(outside,
			filepath.Join(root, "docs", "plans", "completed", "unrelated.md")))

		title, _, err := buildPRTitleBody(root, "docs/plans", "special-branch", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "Valid fallback", title)
	})

	t.Run("newest exact plan wins", func(t *testing.T) {
		root := t.TempDir()
		oldPath := writePlan(t, root, "20260801-special-branch.md", "# Old exact\n")
		newPath := writePlan(t, root, "20260802-special-branch.md", "# New exact\n")
		oldTime := time.Now().Add(-time.Hour)
		require.NoError(t, os.Chtimes(oldPath, oldTime, oldTime))
		newTime := time.Now()
		require.NoError(t, os.Chtimes(newPath, newTime, newTime))

		title, _, err := buildPRTitleBody(root, "docs/plans", "special-branch", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "New exact", title)
	})

	t.Run("progress header associates arbitrary branch override", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "20260802-original-plan.md", "# Override plan\n\n## Overview\n\nExact association.\n")
		progressDir := filepath.Join(root, ".loopai", "progress")
		require.NoError(t, os.MkdirAll(progressDir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(progressDir, "progress-team-custom.txt"), []byte(
			"# Loopai Progress Log\nPlan: docs/plans/completed/20260802-original-plan.md\nBranch: team/custom\nMode: full\n"), 0o600))

		title, body, err := buildPRTitleBody(root, "docs/plans", "team/custom", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "Override plan", title)
		assert.Contains(t, body, "Exact association.")
	})

	t.Run("progress association prefers completed copy over active copy", func(t *testing.T) {
		root := t.TempDir()
		activeDir := filepath.Join(root, "docs", "plans")
		require.NoError(t, os.MkdirAll(activeDir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(activeDir, "original-plan.md"),
			[]byte("# Active copy\n"), 0o600))
		writePlan(t, root, "original-plan.md", "# Completed copy\n\n## Overview\n\nFinished.\n")
		progressDir := filepath.Join(root, ".loopai", "progress")
		require.NoError(t, os.MkdirAll(progressDir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(progressDir, "progress-team-custom.txt"), []byte(
			"# Loopai Progress Log\nPlan: docs/plans/original-plan.md\nBranch: team/custom\nMode: full\n"), 0o600))

		title, body, err := buildPRTitleBody(root, "docs/plans", "team/custom", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "Completed copy", title)
		assert.Contains(t, body, "Finished.")
	})

	t.Run("progress association outside the repository degrades to stats only", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "repo")
		require.NoError(t, os.MkdirAll(root, 0o750))
		outside := filepath.Join(parent, "shared-plans")
		require.NoError(t, os.MkdirAll(outside, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(outside, "20260802-original-plan.md"),
			[]byte("# Out of tree feature\n\n## Overview\n\nUnreadable from the repository.\n"), 0o600))
		progressDir := filepath.Join(root, ".loopai", "progress")
		require.NoError(t, os.MkdirAll(progressDir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(progressDir, "progress-team-custom.txt"), []byte(
			"# Loopai Progress Log\nPlan: "+filepath.Join(outside, "20260802-original-plan.md")+
				"\nBranch: team/custom\nMode: full\n"), 0o600))

		// the branch lookup accepts this record, but PR metadata must still not read outside the repo
		title, body, err := buildPRTitleBody(root, "docs/plans", "team/custom", git.DiffStats{Files: 2})
		require.NoError(t, err)
		assert.Equal(t, "team/custom", title)
		assert.Equal(t, "## Changes\n\n- Files changed: 2\n- Additions: 0\n- Deletions: 0", body)
	})

	t.Run("branch basename does not select unrelated plan", func(t *testing.T) {
		root := t.TempDir()
		writePlan(t, root, "20260802-foo.md", "# Unrelated foo plan\n\n## Overview\n\nWrong plan.\n")

		title, body, err := buildPRTitleBody(root, "docs/plans", "feature/foo", git.DiffStats{})
		require.NoError(t, err)
		assert.Equal(t, "feature/foo", title)
		assert.NotContains(t, body, "Wrong plan.")
	})
}

func TestExecutePlan_DashboardStartupFailureDoesNotPersistPill(t *testing.T) {
	dir := setupTestRepo(t)
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	planPath := filepath.Join(dir, "docs", "plans", "dashboard-startup.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o750))
	require.NoError(t, os.WriteFile(planPath, []byte("# Dashboard startup\n\n### Task 1: test\n\n- [ ] test\n"), 0o600))
	gitSvc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "argv.log")
	writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
	t.Setenv("CMUX_ARGV_LOG", argvLog)

	err = executePlan(t.Context(), opts{Serve: true, Host: "127.0.0.1", Port: port, NoColor: true}, executePlanRequest{
		PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc,
		Config: &config.Config{}, Colors: testColors(), BaseRef: "master",
	})
	require.ErrorContains(t, err, "start dashboard")

	recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
	require.NoError(t, readErr)
	assert.Contains(t, string(recorded), "notify --title loopai")
	assert.NotContains(t, string(recorded), "set-status loopai failed")
	assert.Contains(t, string(recorded), "clear-status loopai")
}

func TestRunPRCommand(t *testing.T) {
	setupFeatureWithRemote := func(t *testing.T) (string, string, *git.Service) {
		t.Helper()
		dir := setupTestRepo(t)
		remote := filepath.Join(t.TempDir(), "origin.git")
		runGit(t, filepath.Dir(remote), "init", "--bare", remote)
		originURL := "https://github.com/acme/repo.git"
		runGit(t, dir, "remote", "add", "origin", originURL)
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		plansDir := filepath.Join(dir, "docs", "plans", "completed")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(plansDir, "20260802-feature.md"), []byte("# Feature PR\n\n## Overview\n\nImplements feature.\n"), 0o600))
		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		t.Setenv("PR_TEST_REAL_GIT", realGit)
		t.Setenv("PR_TEST_REMOTE", remote)
		gitWrapper := filepath.Join(t.TempDir(), "git-wrapper")
		writeExecutable(t, gitWrapper, "#!/bin/sh\nif [ \"$1\" = push ]; then\n  refspec=$4\n  \"$PR_TEST_REAL_GIT\" push \"$PR_TEST_REMOTE\" \"$refspec\" || exit $?\n  branch=${refspec#refs/heads/}\n  branch=${branch%%:*}\n  \"$PR_TEST_REAL_GIT\" update-ref \"refs/remotes/origin/$branch\" \"$branch\" || exit $?\n  \"$PR_TEST_REAL_GIT\" config \"branch.$branch.remote\" origin || exit $?\n  \"$PR_TEST_REAL_GIT\" config \"branch.$branch.merge\" \"refs/heads/$branch\" || exit $?\n  exit 0\nfi\nexec \"$PR_TEST_REAL_GIT\" \"$@\"\n")
		svc, err := git.NewService(dir, noopLogger(), gitWrapper)
		require.NoError(t, err)
		return dir, remote, svc
	}

	t.Run("success prints only URL and clears pill", func(t *testing.T) {
		dir, remote, svc := setupFeatureWithRemote(t)
		binDir := t.TempDir()
		argsLog := filepath.Join(binDir, "gh-args.log")
		bodyLog := filepath.Join(binDir, "gh-body.log")
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nif [ \"$1\" = repo ]; then\n  printf '%s\\n' 'acme/repo'\n  exit 0\nfi\nprintf '%s\\n' \"$@\" > \"$GH_ARGS_LOG\"\ncat > \"$GH_BODY_LOG\"\nprintf '%s\\n' 'warning: 1 uncommitted change' >&2\nprintf '%s\\n' 'https://github.com/acme/repo/pull/42'\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GH_ARGS_LOG", argsLog)
		t.Setenv("GH_BODY_LOG", bodyLog)
		clearer := &recordingStatusClearer{}
		var output bytes.Buffer

		require.NoError(t, runPRCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, &output))
		assert.Equal(t, "https://github.com/acme/repo/pull/42\n", output.String())
		assert.Equal(t, 1, clearer.calls)
		assert.Equal(t, "feature", currentGitBranch(t, dir))
		assert.True(t, branchExists(t, dir, "feature"))
		args, err := os.ReadFile(argsLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Contains(t, string(args), "pr\ncreate\n--repo\nacme/repo\n--base\nmaster\n--head\nfeature\n--title\nFeature PR\n--body-file\n-\n")
		assert.NotContains(t, string(args), "Implements feature.", "the body must not be exposed in argv")
		body, readBodyErr := os.ReadFile(bodyLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readBodyErr)
		assert.Contains(t, string(body), "Implements feature.")
		localHead := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "feature"))
		assert.Equal(t, localHead, strings.TrimSpace(gitOutput(t, remote, "rev-parse", "refs/heads/feature")))
		assert.Equal(t, "origin/feature", strings.TrimSpace(gitOutput(t, dir, "rev-parse", "--abbrev-ref", "feature@{upstream}")))
	})

	t.Run("gh failure keeps pill", func(t *testing.T) {
		_, _, svc := setupFeatureWithRemote(t)
		binDir := t.TempDir()
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nprintf '%s\\n' 'authentication required'\nexit 1\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		clearer := &recordingStatusClearer{}

		err := runPRCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "authentication required")
		assert.Zero(t, clearer.calls)
	})

	t.Run("oversized metadata fails before push", func(t *testing.T) {
		dir, remote, svc := setupFeatureWithRemote(t)
		planPath := filepath.Join(dir, "docs", "plans", "completed", "20260802-feature.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Feature PR\n\n## Overview\n\n"+
			strings.Repeat("x", maxPRBodyRunes)), 0o600))
		binDir := t.TempDir()
		invoked := filepath.Join(binDir, "invoked")
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\ntouch \"$GH_INVOKED\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GH_INVOKED", invoked)
		clearer := &recordingStatusClearer{}

		err := runPRCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.ErrorContains(t, err, "PR body exceeds")
		assert.NoFileExists(t, invoked)
		cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/feature")
		cmd.Dir = remote
		require.Error(t, cmd.Run(), "validation must happen before the branch is pushed")
		assert.Zero(t, clearer.calls)
	})

	t.Run("push failure does not invoke gh and keeps pill", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "checkout", "-b", "feature")
		originURL := "https://github.com/acme/repo.git"
		runGit(t, dir, "remote", "add", "origin", originURL)
		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		t.Setenv("PR_TEST_REAL_GIT", realGit)
		gitWrapper := filepath.Join(t.TempDir(), "git-wrapper")
		writeExecutable(t, gitWrapper, "#!/bin/sh\nif [ \"$1\" = push ]; then printf '%s\\n' 'simulated push failure' >&2; exit 1; fi\nexec \"$PR_TEST_REAL_GIT\" \"$@\"\n")
		svc, err := git.NewService(dir, noopLogger(), gitWrapper)
		require.NoError(t, err)
		binDir := t.TempDir()
		invoked := filepath.Join(binDir, "invoked")
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nif [ \"$1\" = repo ]; then\n  printf '%s\\n' 'acme/repo'\n  exit 0\nfi\ntouch \"$GH_INVOKED\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GH_INVOKED", invoked)
		clearer := &recordingStatusClearer{}

		err = runPRCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "push PR branch")
		assert.NoFileExists(t, invoked)
		assert.Zero(t, clearer.calls)
	})

	for _, tc := range []struct {
		name      string
		configure func(t *testing.T, dir string)
	}{
		{name: "different pushurl is rejected", configure: func(t *testing.T, dir string) {
			runGit(t, dir, "remote", "set-url", "--add", "--push", "origin", "https://github.com/other/repo.git")
		}},
		{name: "different pushInsteadOf destination is rejected", configure: func(t *testing.T, dir string) {
			runGit(t, dir, "config", "url.https://github.com/other/repo.git.pushInsteadOf", "https://github.com/acme/repo.git")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			runGit(t, dir, "remote", "add", "origin", "https://github.com/acme/repo.git")
			runGit(t, dir, "checkout", "-b", "feature")
			tc.configure(t, dir)
			svc, err := git.NewService(dir, noopLogger())
			require.NoError(t, err)
			binDir := t.TempDir()
			invoked := filepath.Join(binDir, "invoked")
			writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\ntouch \"$GH_INVOKED\"\n")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GH_INVOKED", invoked)

			err = runPRCommand(t.Context(), svc, "master", closeoutTarget{}, &recordingStatusClearer{}, io.Discard)
			require.ErrorContains(t, err, "does not match PR repository")
			assert.NoFileExists(t, invoked)
		})
	}

	t.Run("invalid credentialed push URL is redacted", func(t *testing.T) {
		const credential = "secret-access-token"
		dir := setupTestRepo(t)
		runGit(t, dir, "remote", "add", "origin", "https://github.com/acme/repo.git")
		runGit(t, dir, "remote", "set-url", "--add", "--push", "origin",
			"https://"+credential+"@github.com/owner")
		runGit(t, dir, "checkout", "-b", "feature")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		binDir := t.TempDir()
		invoked := filepath.Join(binDir, "invoked")
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\ntouch \"$GH_INVOKED\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GH_INVOKED", invoked)

		err = runPRCommand(t.Context(), svc, "master", closeoutTarget{}, &recordingStatusClearer{}, io.Discard)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), credential)
		assert.NoFileExists(t, invoked)
	})

	t.Run("non-GitHub origin is rejected before gh or push", func(t *testing.T) {
		dir := setupTestRepo(t)
		remote := filepath.Join(t.TempDir(), "origin.git")
		runGit(t, filepath.Dir(remote), "init", "--bare", remote)
		runGit(t, dir, "remote", "add", "origin", remote)
		runGit(t, dir, "checkout", "-b", "feature")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		binDir := t.TempDir()
		invoked := filepath.Join(binDir, "invoked")
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\ntouch \"$GH_INVOKED\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GH_INVOKED", invoked)

		err = runPRCommand(t.Context(), svc, "master", closeoutTarget{}, &recordingStatusClearer{}, io.Discard)
		require.ErrorContains(t, err, "not a GitHub repository URL")
		assert.NoFileExists(t, invoked)
		cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/feature")
		cmd.Dir = remote
		require.Error(t, cmd.Run(), "branch must not be pushed before origin validation")
	})

	t.Run("missing gh has install hint", func(t *testing.T) {
		_, _, svc := setupFeatureWithRemote(t)
		t.Setenv("PATH", t.TempDir())
		clearer := &recordingStatusClearer{}

		err := runPRCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "install")
		assert.Zero(t, clearer.calls)
	})
}

func TestGitHubRepoSpec(t *testing.T) {
	tests := []struct {
		name, remote, want string
		wantErr            bool
	}{
		{name: "https", remote: "https://github.com/acme/repo.git", want: "acme/repo"},
		{name: "ssh shorthand", remote: "git@github.com:acme/repo.git", want: "acme/repo"},
		{name: "enterprise ssh", remote: "ssh://git@git.example.com/acme/repo.git", want: "git.example.com/acme/repo"},
		{name: "local path", remote: "/tmp/repo.git", wantErr: true},
		{name: "missing owner", remote: "https://github.com/repo.git", wantErr: true},
		{name: "nested path", remote: "https://gitlab.com/group/subgroup/repo.git", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := githubRepoSpec(tc.remote)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestGitHubRepoSpecErrorsDoNotExposeCredentials(t *testing.T) {
	const credential = "secret-access-token"
	_, err := githubRepoSpec("https://" + credential + "@github.com/owner")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), credential)
}

func currentGitBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func TestRunDispatchesMergeCloseoutBeforeExecutionDependencies(t *testing.T) {
	dir := setupTestRepo(t)
	runGit(t, dir, "checkout", "-b", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
	runGit(t, dir, "add", "feature.txt")
	runGit(t, dir, "commit", "-m", "feature")
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })
	t.Setenv("CMUX_WORKSPACE_ID", "")

	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config"),
		[]byte("pass_claude_md = true\n"), 0o600))
	err = run(t.Context(), opts{mergeSet: true, ConfigDir: configDir, NoColor: true})
	require.NoError(t, err)
	assert.Equal(t, "master", currentGitBranch(t, dir))
	assert.False(t, branchExists(t, dir, "feature"))
	assert.FileExists(t, filepath.Join(dir, "feature.txt"))
}

func TestRunClearsStaleCmuxStatusBeforeConfigFailure(t *testing.T) {
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "cmux-argv.log")
	writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
	t.Setenv("CMUX_ARGV_LOG", argvLog)
	badConfigDir := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(badConfigDir, []byte("x"), 0o600))

	err := run(t.Context(), opts{ConfigDir: badConfigDir})
	require.ErrorContains(t, err, "load config")
	recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "clear-status loopai\n", string(recorded))
}

func TestRunClearsStaleCmuxStatusBeforeFlagValidationFailure(t *testing.T) {
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "cmux-argv.log")
	writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
	t.Setenv("CMUX_ARGV_LOG", argvLog)

	err := run(t.Context(), opts{Wait: -time.Second})
	require.ErrorContains(t, err, "--wait must be non-negative")
	recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "clear-status loopai\n", string(recorded))
}

func TestRunWatchOnlyPreservesStaleCmuxStatus(t *testing.T) {
	tests := []struct {
		name           string
		configureWatch bool
	}{
		{name: "CLI watch directory"},
		{name: "configured watch directory", configureWatch: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			argvLog := filepath.Join(binDir, "cmux-argv.log")
			writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
			t.Setenv("CMUX_ARGV_LOG", argvLog)

			watchDir := t.TempDir()
			configDir := t.TempDir()
			o := opts{Serve: true, Port: 0, ConfigDir: configDir, NoColor: true}
			if tc.configureWatch {
				require.NoError(t, os.WriteFile(filepath.Join(configDir, "config"),
					[]byte("watch_dirs = "+watchDir+"\n"), 0o600))
			} else {
				o.Watch = []string{watchDir}
			}

			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()
			require.NoError(t, run(ctx, o))

			_, err := os.Stat(argvLog)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestClearStaleCmuxStatusSkipsConfigUtilities(t *testing.T) {
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "cmux-argv.log")
	writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
	t.Setenv("CMUX_ARGV_LOG", argvLog)

	for _, o := range []opts{
		{Init: true},
		{Reset: true},
		{DumpDefaults: filepath.Join(t.TempDir(), "defaults")},
	} {
		clearStaleCmuxStatus(o)
	}
	_, err := os.Stat(argvLog)
	require.ErrorIs(t, err, os.ErrNotExist)

	clearStaleCmuxStatus(opts{Reset: true, PlanFile: "plan.md"})
	recorded, err := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
	require.NoError(t, err)
	assert.Equal(t, "clear-status loopai\n", string(recorded))
}

func TestHandleEarlyFlags(t *testing.T) {
	t.Run("no_flags_continues", func(t *testing.T) {
		done, err := handleEarlyFlags(opts{})
		require.NoError(t, err)
		assert.False(t, done)
	})

	t.Run("clear_outside_cmux_is_successful_noop", func(t *testing.T) {
		t.Setenv("CMUX_WORKSPACE_ID", "")
		output := captureStdout(t, func() {
			done, err := handleEarlyFlags(opts{Clear: true})
			require.NoError(t, err)
			assert.True(t, done)
		})
		assert.Contains(t, output, "not running inside cmux")
	})

	t.Run("clear_inside_cmux_runs_expected_command", func(t *testing.T) {
		binDir := t.TempDir()
		argvLog := filepath.Join(binDir, "argv.log")
		writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
		t.Setenv("CMUX_ARGV_LOG", argvLog)

		done, err := handleEarlyFlags(opts{Clear: true})
		require.NoError(t, err)
		assert.True(t, done)
		recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readErr)
		assert.Equal(t, "clear-status loopai\n", string(recorded))
	})

	t.Run("dump_defaults_exits", func(t *testing.T) {
		tmpDir := filepath.Join(t.TempDir(), "defaults")
		done, err := handleEarlyFlags(opts{DumpDefaults: tmpDir})
		require.NoError(t, err)
		assert.True(t, done)
		assert.FileExists(t, filepath.Join(tmpDir, "config"))
	})

	t.Run("dump_defaults_error", func(t *testing.T) {
		tmpDir := t.TempDir()
		blocker := filepath.Join(tmpDir, "blocker")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

		done, err := handleEarlyFlags(opts{DumpDefaults: filepath.Join(blocker, "sub")})
		require.Error(t, err)
		assert.True(t, done)
	})

	t.Run("init_error", func(t *testing.T) {
		tmpDir := t.TempDir()

		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

		// create .git so repo root check passes
		require.NoError(t, os.Mkdir(filepath.Join(tmpDir, ".git"), 0o700))

		// make .loopai point to a file so MkdirAll fails
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".loopai"), []byte("x"), 0o600))

		done, err := handleEarlyFlags(opts{Init: true})
		require.Error(t, err)
		assert.True(t, done)
	})

	t.Run("init_creates_local_config", func(t *testing.T) {
		tmpDir := t.TempDir()
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

		// create .git so repo root check passes
		require.NoError(t, os.Mkdir(filepath.Join(tmpDir, ".git"), 0o700))

		// legacy config must remain untouched and must not replace the new local config.
		legacyDir := filepath.Join(tmpDir, ".ralphex")
		require.NoError(t, os.Mkdir(legacyDir, 0o700))
		legacyConfig := []byte("claude_command = legacy\n")
		require.NoError(t, os.WriteFile(filepath.Join(legacyDir, "config"), legacyConfig, 0o600))

		done, err := handleEarlyFlags(opts{Init: true})
		require.NoError(t, err)
		assert.True(t, done)
		loopaiDir := filepath.Join(tmpDir, ".loopai")
		assert.DirExists(t, loopaiDir)
		assert.FileExists(t, filepath.Join(loopaiDir, "config"))
		assert.DirExists(t, filepath.Join(loopaiDir, "prompts"))
		assert.DirExists(t, filepath.Join(loopaiDir, "agents"))

		gotLegacyConfig, readErr := os.ReadFile(filepath.Join(legacyDir, "config")) //nolint:gosec // test file
		require.NoError(t, readErr)
		assert.Equal(t, legacyConfig, gotLegacyConfig)
	})

	t.Run("init_fails_outside_repo_root", func(t *testing.T) {
		tmpDir := t.TempDir()
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

		// no .git - should fail
		done, err := handleEarlyFlags(opts{Init: true})
		require.Error(t, err)
		assert.True(t, done)
		assert.Contains(t, err.Error(), "must run from repository root")
	})

	t.Run("init_rejects_hg_repo_without_custom_backend", func(t *testing.T) {
		tmpDir := t.TempDir()
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

		// a bare .hg marker is no longer a supported repository root.
		require.NoError(t, os.Mkdir(filepath.Join(tmpDir, ".hg"), 0o700))

		done, err := handleEarlyFlags(opts{Init: true})
		require.Error(t, err)
		assert.True(t, done)
		assert.Contains(t, err.Error(), "no .git directory found")
		assert.NoDirExists(t, filepath.Join(tmpDir, ".loopai"))
	})

	t.Run("init_works_with_custom_vcs_backend", func(t *testing.T) {
		// simulate custom VCS backend with a script that returns cwd as repo root.
		// no .git directory — validation goes through validateRepoRoot.
		tmpDir := t.TempDir()
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

		// create a fake VCS script that outputs tmpDir as repo root
		fakeVCS := filepath.Join(t.TempDir(), "fake-vcs.sh")
		// resolve symlinks for consistent comparison (macOS /var -> /private/var)
		resolvedTmpDir, resolveErr := filepath.EvalSymlinks(tmpDir)
		require.NoError(t, resolveErr)
		writeExecutable(t, fakeVCS, "#!/bin/sh\necho "+resolvedTmpDir+"\n")

		cfgDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"),
			[]byte("vcs_command = "+fakeVCS), 0o600))

		done, err := handleEarlyFlags(opts{Init: true, ConfigDir: cfgDir})
		require.NoError(t, err)
		assert.True(t, done)
		assert.DirExists(t, filepath.Join(tmpDir, ".loopai"))
	})

	t.Run("init_fails_with_custom_vcs_in_arbitrary_dir", func(t *testing.T) {
		// custom VCS backend configured but command fails — must reject
		tmpDir := t.TempDir()
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

		// create a fake VCS script that exits with error (not a repo)
		fakeVCS := filepath.Join(t.TempDir(), "fake-vcs.sh")
		writeExecutable(t, fakeVCS, "#!/bin/sh\nexit 1\n")

		cfgDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"),
			[]byte("vcs_command = "+fakeVCS), 0o600))

		done, err := handleEarlyFlags(opts{Init: true, ConfigDir: cfgDir})
		assert.True(t, done)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must run from repository root")
	})

	t.Run("init_fails_with_custom_vcs_empty_root", func(t *testing.T) {
		// custom VCS returns empty string — must reject
		tmpDir := t.TempDir()
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(tmpDir))
		t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

		// create a fake VCS script that outputs empty string
		fakeVCS := filepath.Join(t.TempDir(), "fake-vcs.sh")
		writeExecutable(t, fakeVCS, "#!/bin/sh\necho\n")

		cfgDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"),
			[]byte("vcs_command = "+fakeVCS), 0o600))

		done, err := handleEarlyFlags(opts{Init: true, ConfigDir: cfgDir})
		assert.True(t, done)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must run from repository root")
	})

	t.Run("init_fails_with_custom_vcs_in_subdirectory", func(t *testing.T) {
		// custom VCS returns parent as root, but cwd is a subdirectory — must reject
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "sub")
		require.NoError(t, os.Mkdir(subDir, 0o700))
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(subDir))
		t.Cleanup(func() { require.NoError(t, os.Chdir(origDir)) })

		// create a fake VCS script that returns parent dir as root
		fakeVCS := filepath.Join(t.TempDir(), "fake-vcs.sh")
		resolvedTmpDir, resolveErr := filepath.EvalSymlinks(tmpDir)
		require.NoError(t, resolveErr)
		writeExecutable(t, fakeVCS, "#!/bin/sh\necho "+resolvedTmpDir+"\n")

		cfgDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"),
			[]byte("vcs_command = "+fakeVCS), 0o600))

		done, err := handleEarlyFlags(opts{Init: true, ConfigDir: cfgDir})
		assert.True(t, done)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must run from repository root")
	})
}

func TestIsResetOnly(t *testing.T) {
	t.Run("reset_only", func(t *testing.T) {
		assert.True(t, isResetOnly(opts{Reset: true}))
	})

	t.Run("reset_with_plan_file", func(t *testing.T) {
		assert.False(t, isResetOnly(opts{Reset: true, PlanFile: "plan.md"}))
	})

	t.Run("reset_with_dump_defaults", func(t *testing.T) {
		assert.False(t, isResetOnly(opts{Reset: true, DumpDefaults: "/tmp/dir"}))
	})

	t.Run("reset_with_review", func(t *testing.T) {
		assert.False(t, isResetOnly(opts{Reset: true, Review: true}))
	})

	t.Run("reset_with_init", func(t *testing.T) {
		assert.False(t, isResetOnly(opts{Reset: true, Init: true}))
	})

	t.Run("reset_with_resume_worktree", func(t *testing.T) {
		assert.False(t, isResetOnly(opts{Reset: true, ResumeWorktree: true}))
	})
}

func TestResolveVersion(t *testing.T) {
	t.Run("ldflags_set", func(t *testing.T) {
		orig := revision
		t.Cleanup(func() { revision = orig })
		revision = "v1.2.3-abc1234"
		assert.Equal(t, "v1.2.3-abc1234", resolveVersion())
	})

	t.Run("fallback_to_build_info", func(t *testing.T) {
		orig := revision
		t.Cleanup(func() { revision = orig })
		revision = "unknown"
		// in test context, debug.ReadBuildInfo returns (devel) module version
		// but VCS info should be available from the git repo
		v := resolveVersion()
		assert.NotEmpty(t, v)
	})
}

func TestPrepareWorktreeRunAutoCommit(t *testing.T) {
	t.Run("invalid_branch_is_rejected_before_source_mutation", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "valid.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Valid\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/valid.md")
		runGit(t, dir, "commit", "-m", "add valid plan")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		statusBefore := gitOutput(t, dir, "status", "--porcelain")
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
			BranchOverride: "bad branch",
		}, "bad branch")

		require.ErrorContains(t, err, `invalid plan branch "bad branch"`)
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Equal(t, statusBefore, gitOutput(t, dir, "status", "--porcelain"))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai"))
	})

	t.Run("reflog_shorthand_branch_is_rejected_before_source_mutation", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "valid.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Valid\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/valid.md")
		runGit(t, dir, "commit", "-m", "add valid plan")
		runGit(t, dir, "switch", "-c", "previous")
		runGit(t, dir, "switch", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		statusBefore := gitOutput(t, dir, "status", "--porcelain")
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
			BranchOverride: "@{-1}",
		}, "@{-1}")

		require.ErrorContains(t, err, `invalid plan branch "@{-1}"`)
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Equal(t, statusBefore, gitOutput(t, dir, "status", "--porcelain"))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai"))
	})

	t.Run("plan_branch_collision_is_rejected_before_source_mutation", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "collision.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Collision\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/collision.md")
		runGit(t, dir, "commit", "-m", "add collision plan")
		runGit(t, dir, "checkout", "-b", "collision")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "collision")

		require.ErrorContains(t, err, "plan branch \"collision\" is already checked out here")
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Contains(t, gitOutput(t, dir, "status", "--porcelain"), "README.md")
		assert.NoDirExists(t, filepath.Join(dir, ".loopai"))
	})

	t.Run("dirty_source_is_committed_and_merged_into_existing_plan_branch", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "existing-reuse.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Existing Reuse\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/existing-reuse.md")
		runGit(t, dir, "commit", "-m", "add existing reuse plan")
		runGit(t, dir, "switch", "-c", "existing-reuse")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "branch-only.txt"), []byte("preserve branch work\n"), 0o600))
		runGit(t, dir, "add", "branch-only.txt")
		runGit(t, dir, "commit", "-m", "existing plan work")
		runGit(t, dir, "switch", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		wt, err := prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "existing-reuse")

		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wt.path) })
		sourceHead := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		assert.NotEqual(t, headBefore, sourceHead)
		runGit(t, dir, "merge-base", "--is-ancestor", sourceHead, "existing-reuse")
		assert.Equal(t, "preserve branch work", strings.TrimSpace(gitOutput(t, wt.path, "show", "HEAD:branch-only.txt")))
		assert.Equal(t, "# Dirty", strings.TrimSpace(gitOutput(t, wt.path, "show", "HEAD:README.md")))
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "status", "--porcelain")))
	})

	t.Run("existing_plan_branch_merge_conflict_is_aborted_and_worktree_removed", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "existing-conflict.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Existing Conflict\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/existing-conflict.md")
		runGit(t, dir, "commit", "-m", "add conflict plan")
		runGit(t, dir, "switch", "-c", "existing-conflict")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("branch version\n"), 0o600))
		runGit(t, dir, "commit", "-am", "change README on plan branch")
		branchBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		runGit(t, dir, "switch", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("source version\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		sourceBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "existing-conflict")

		require.ErrorContains(t, err, "merge auto-committed source into existing plan branch")
		assert.NotEqual(t, sourceBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Equal(t, branchBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "existing-conflict")))
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "status", "--porcelain")))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "existing-conflict"))
	})

	t.Run("case_mismatched_plan_path_reuses_existing_branch_after_auto_commit", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		actualPlanPath := filepath.Join(dir, "docs", "plans", "Add-Auth.md")
		require.NoError(t, os.WriteFile(actualPlanPath, []byte("# Add Auth\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/Add-Auth.md")
		runGit(t, dir, "commit", "-m", "add mixed-case plan")
		runGit(t, dir, "branch", "Add-Auth")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		userPlanPath := filepath.Join(dir, "docs", "plans", "add-auth.md")
		wt, err := prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: userPlanPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "add-auth")

		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wt.path) })
		sourceHead := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		assert.NotEqual(t, headBefore, sourceHead)
		runGit(t, dir, "merge-base", "--is-ancestor", sourceHead, "Add-Auth")
		assert.Equal(t, "# Dirty", strings.TrimSpace(gitOutput(t, wt.path, "show", "HEAD:README.md")))
	})

	t.Run("clean_source_is_no_op_and_still_creates_worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "clean-source.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Clean Source\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/clean-source.md")
		runGit(t, dir, "commit", "-m", "add clean plan")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		wt, err := prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "clean-source")
		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wt.path) })
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "clean-source")))
		assert.DirExists(t, wt.path)
	})

	t.Run("commits_dirty_source_and_includes_plan_in_new_worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "auto-commit.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Auto Commit\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		req := executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}

		wt, err := prepareWorktreeRun(opts{Commit: true, Worktree: true}, req, "auto-commit")
		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wt.path) })
		assert.False(t, wt.planNeedsCommit, "auto-committed plan must not be copied and recommitted")

		headAfter := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "master"))
		assert.NotEqual(t, headBefore, headAfter)
		assert.Equal(t, headAfter, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "auto-commit")))
		assert.Equal(t, "auto-commit working tree before plan: auto-commit",
			strings.TrimSpace(gitOutput(t, dir, "log", "-1", "--format=%B", "master")))
		readme, readErr := os.ReadFile(filepath.Join(wt.path, "README.md"))
		require.NoError(t, readErr)
		assert.Equal(t, "# Updated\n", string(readme))
		planContents, readErr := os.ReadFile(wt.planFile)
		require.NoError(t, readErr)
		assert.Equal(t, "# Auto Commit\n", string(planContents))
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "status", "--porcelain")))
	})

	t.Run("commits_dirty_detached_head_and_creates_worktree_from_it", func(t *testing.T) {
		dir := setupTestRepo(t)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		runGit(t, dir, "checkout", "--detach", headBefore)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "detached-auto-commit.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Detached Auto Commit\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Detached update\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		wt, err := prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "detached-auto-commit")
		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wt.path) })

		detachedHead := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		assert.NotEqual(t, headBefore, detachedHead)
		assert.Equal(t, "HEAD", strings.TrimSpace(gitOutput(t, dir, "rev-parse", "--abbrev-ref", "HEAD")))
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD^")))
		assert.Equal(t, detachedHead, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "detached-auto-commit")))
		assert.False(t, wt.planNeedsCommit)
		assert.Equal(t, "# Detached update", strings.TrimSpace(gitOutput(t, wt.path, "show", "HEAD:README.md")))
		assert.Equal(t, "# Detached Auto Commit", strings.TrimSpace(gitOutput(t, wt.path, "show", "HEAD:docs/plans/detached-auto-commit.md")))
	})

	t.Run("invalid_worktree_parent_preserves_source_and_creates_nothing", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "ignore-failure.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Ignore Failure\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/ignore-failure.md")
		runGit(t, dir, "commit", "-m", "add ignore failure plan")
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".loopai"), []byte("blocks directory\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "ignore-failure")
		require.ErrorContains(t, err, "preflight worktree creation")
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Contains(t, gitOutput(t, dir, "status", "--porcelain"), "README.md")
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "branch", "--list", "ignore-failure")))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "ignore-failure"))
	})

	t.Run("case_only_plan_branch_collision_is_rejected_before_auto_commit", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "feature.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Feature\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/feature.md")
		runGit(t, dir, "commit", "-m", "add feature plan")
		runGit(t, dir, "branch", "-m", "Feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "feature")

		require.ErrorContains(t, err, "plan branch \"feature\" is already checked out here")
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Contains(t, gitOutput(t, dir, "status", "--porcelain"), "README.md")
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "feature"))
	})

	t.Run("outside_plan_is_rejected_before_auto_commit", func(t *testing.T) {
		dir := setupTestRepo(t)
		outsideDir := t.TempDir()
		planPath := filepath.Join(outsideDir, "outside-plan.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Outside Plan\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("keep staged\n"), 0o600))
		runGit(t, dir, "add", "staged.txt")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		indexBefore := gitOutput(t, dir, "diff", "--cached", "--name-status")
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "outside-plan")
		require.ErrorContains(t, err, "outside repository")
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Equal(t, indexBefore, gitOutput(t, dir, "diff", "--cached", "--name-status"))
		assert.Contains(t, gitOutput(t, dir, "status", "--porcelain"), "README.md")
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "branch", "--list", "outside-plan")))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "outside-plan"))
	})

	t.Run("ignored_plan_is_rejected_before_auto_commit", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("docs/plans/ignored-plan.md\n"), 0o600))
		runGit(t, dir, "add", ".gitignore")
		runGit(t, dir, "commit", "-m", "ignore plan fixture")
		planPath := filepath.Join(dir, "docs", "plans", "ignored-plan.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Ignored Plan\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "ignored-plan")
		require.ErrorContains(t, err, "ignored or otherwise unavailable to Git")
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Contains(t, gitOutput(t, dir, "status", "--porcelain"), "README.md")
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "branch", "--list", "ignored-plan")))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "ignored-plan"))
	})

	t.Run("runtime_plan_is_rejected_after_ignore_setup_before_auto_commit", func(t *testing.T) {
		dir := setupTestRepo(t)
		progressDir := filepath.Join(dir, ".loopai", "progress")
		require.NoError(t, os.MkdirAll(progressDir, 0o750))
		planPath := filepath.Join(progressDir, "runtime-plan.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Runtime Plan\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "runtime-plan")

		require.ErrorContains(t, err, "preflight worktree creation after ignore setup")
		require.ErrorContains(t, err, "ignored or otherwise unavailable to Git")
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Contains(t, gitOutput(t, dir, "status", "--porcelain"), "README.md")
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "branch", "--list", "runtime-plan")))
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "runtime-plan"))
	})

	t.Run("dangling_worktree_target_is_rejected_before_auto_commit", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "dangling-target.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Dangling Target\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/dangling-target.md")
		runGit(t, dir, "commit", "-m", "add dangling target plan")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))
		worktreesDir := filepath.Join(dir, ".loopai", "worktrees")
		require.NoError(t, os.MkdirAll(worktreesDir, 0o750))
		targetPath := filepath.Join(worktreesDir, "dangling-target")
		require.NoError(t, os.Symlink(filepath.Join(dir, "missing-worktree"), targetPath))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "dangling-target")

		require.ErrorContains(t, err, "preflight worktree creation")
		require.ErrorContains(t, err, "worktree already exists")
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Contains(t, gitOutput(t, dir, "status", "--porcelain"), "README.md")
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "branch", "--list", "dangling-target")))
	})

	t.Run("concurrent_fresh_runs_ignore_each_others_worktrees", func(t *testing.T) {
		dir := setupTestRepo(t)
		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		firstPlan := filepath.Join(plansDir, "parallel-one.md")
		secondPlan := filepath.Join(plansDir, "parallel-two.md")
		require.NoError(t, os.WriteFile(firstPlan, []byte("# Parallel One\n"), 0o600))
		require.NoError(t, os.WriteFile(secondPlan, []byte("# Parallel Two\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/parallel-one.md", "docs/plans/parallel-two.md")
		runGit(t, dir, "commit", "-m", "add parallel plans")

		firstSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		secondSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		type result struct {
			wt  worktreeRun
			err error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		launch := func(svc *git.Service, planPath, branch string) {
			<-start
			wt, prepareErr := prepareWorktreeRun(opts{Worktree: true}, executePlanRequest{
				PlanFile: planPath, GitSvc: svc, Config: &config.Config{},
				Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
			}, branch)
			results <- result{wt: wt, err: prepareErr}
		}
		go launch(firstSvc, firstPlan, "parallel-one")
		go launch(secondSvc, secondPlan, "parallel-two")
		close(start)

		for range 2 {
			res := <-results
			require.NoError(t, res.err)
			t.Cleanup(func() { _ = firstSvc.RemoveWorktree(res.wt.path) })
			assert.DirExists(t, res.wt.path)
		}
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "status", "--porcelain")))
	})

	t.Run("returns_commit_error_without_creating_worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "blocked.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Blocked\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/blocked.md")
		runGit(t, dir, "commit", "-m", "add blocked plan")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Dirty\n"), 0o600))
		writeExecutable(t, filepath.Join(dir, ".git", "hooks", "pre-commit"), "#!/bin/sh\nexit 1\n")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		_, err = prepareWorktreeRun(opts{Commit: true, Worktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "blocked")
		require.ErrorContains(t, err, "auto-commit working tree")
		assert.NoDirExists(t, filepath.Join(dir, ".loopai", "worktrees", "blocked"))
	})

	t.Run("resume_does_not_auto_commit_source", func(t *testing.T) {
		dir := setupTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "resume-no-commit.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Resume\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/resume-no-commit.md")
		runGit(t, dir, "commit", "-m", "add resume plan")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		wtPath, _, err := gitSvc.CreateWorktreeForPlan(planPath, "")
		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wtPath) })
		headBefore := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Still dirty\n"), 0o600))

		wt, err := prepareWorktreeRun(opts{Commit: true, ResumeWorktree: true}, executePlanRequest{
			PlanFile: planPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		}, "resume-no-commit")
		require.NoError(t, err)
		assert.True(t, wt.resumed)
		assert.Equal(t, headBefore, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
		assert.Contains(t, gitOutput(t, dir, "status", "--porcelain"), "README.md")
	})
}

func TestRunWithWorktree(t *testing.T) {
	t.Run("creates_worktree_and_restores_cwd", func(t *testing.T) {
		skipIfClaudeNotAvailable(t)

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// resolve dir through symlinks (macOS /var → /private/var)
		resolvedDir, err := filepath.EvalSymlinks(dir)
		require.NoError(t, err)

		// create and commit plan file
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "wt-test.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# WT Test\n\n### Task 1: cancel\n\n- [ ] task 1\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/wt-test.md")
		runGit(t, dir, "commit", "-m", "add wt test plan")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		colors := testColors()
		cfg := &config.Config{WorktreeEnabled: true}
		wtCleanup := &cleanupHolder{}
		binDir := t.TempDir()
		argvLog := filepath.Join(binDir, "cmux-argv.log")
		writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
		t.Setenv("CMUX_ARGV_LOG", argvLog)

		// cancel context immediately to stop executePlan fast
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err = runWithWorktree(ctx, opts{MaxIterations: 1, NoColor: true}, executePlanRequest{
			PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc, Config: cfg,
			Colors: colors, DefaultBranch: "master", WtCleanup: wtCleanup,
		})
		// should fail with context canceled from the runner
		require.Error(t, err)

		// verify CWD restored to original (compare resolved paths due to macOS symlinks)
		cwd, cwdErr := os.Getwd()
		require.NoError(t, cwdErr)
		assert.Equal(t, resolvedDir, cwd, "cwd should be restored after runWithWorktree")

		// verify worktree directory cleaned up
		wtPath := filepath.Join(dir, ".loopai", "worktrees", "wt-test")
		assert.NoDirExists(t, wtPath, "worktree should be removed after runWithWorktree")

		// verify branch was preserved (worktree creates the branch)
		assert.True(t, branchExists(t, dir, "wt-test"), "branch should be preserved after worktree removal")
		recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readErr)
		output := string(recorded)
		assert.NotContains(t, output, "set-status loopai done")
		assert.NotContains(t, output, "set-status loopai failed")
		assert.Contains(t, output, "clear-status loopai", "an aborted run must perform full pill cleanup")
	})

	t.Run("populates_worktree_cleanup_ptr", func(t *testing.T) {
		skipIfClaudeNotAvailable(t)

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create and commit plan file
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "wt-ptr.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# WT Ptr\n\n- [ ] task 1\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/wt-ptr.md")
		runGit(t, dir, "commit", "-m", "add wt ptr plan")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		colors := testColors()
		cfg := &config.Config{WorktreeEnabled: true}

		called := false
		wtCleanup := &cleanupHolder{fn: func() { called = true }}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_ = runWithWorktree(ctx, opts{MaxIterations: 1, NoColor: true}, executePlanRequest{
			PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc, Config: cfg,
			Colors: colors, DefaultBranch: "master", WtCleanup: wtCleanup,
		})

		// the cleanup fn should have been overwritten by runWithWorktree
		assert.False(t, called, "original cleanup should not have been called (replaced by runWithWorktree)")
	})

	t.Run("worktree_creates_branch", func(t *testing.T) {
		skipIfClaudeNotAvailable(t)

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		// create and commit plan file
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "wt-branch.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# WT Branch\n\n- [ ] task 1\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/wt-branch.md")
		runGit(t, dir, "commit", "-m", "add wt branch plan")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		colors := testColors()
		cfg := &config.Config{WorktreeEnabled: true}
		wtCleanup := &cleanupHolder{}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_ = runWithWorktree(ctx, opts{MaxIterations: 1, NoColor: true}, executePlanRequest{
			PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc, Config: cfg,
			Colors: colors, DefaultBranch: "master", WtCleanup: wtCleanup,
		})

		// branch should be preserved after worktree cleanup
		assert.True(t, branchExists(t, dir, "wt-branch"), "branch should exist after worktree removal")
	})
}

func TestRunWithWorktreeResume(t *testing.T) {
	t.Run("accepts_case_mismatched_plan_path", func(t *testing.T) {
		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		actualPlanPath := filepath.Join(plansDir, "Resume-Mixed-Case.md")
		require.NoError(t, os.WriteFile(actualPlanPath, []byte("# Resume Mixed Case\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/Resume-Mixed-Case.md")
		runGit(t, dir, "commit", "-m", "add mixed-case resume plan")

		requestedPlanPath := filepath.Join(plansDir, "resume-mixed-case.md")
		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		wtPath, _, err := gitSvc.CreateWorktreeForPlan(requestedPlanPath, "")
		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wtPath) })

		branch := gitSvc.EffectiveBranchName(requestedPlanPath, "")
		assert.Equal(t, "Resume-Mixed-Case", branch)
		wt, err := prepareWorktreeRun(opts{ResumeWorktree: true}, executePlanRequest{
			PlanFile: requestedPlanPath, GitSvc: gitSvc, Config: &config.Config{},
			Colors: testColors(), WtCleanup: &cleanupHolder{},
		}, branch)
		require.NoError(t, err)
		assert.Equal(t, wtPath, wt.path)
		currentBranch, err := wt.gitSvc.CurrentBranch()
		require.NoError(t, err)
		assert.Equal(t, "Resume-Mixed-Case", currentBranch)
	})

	t.Run("rejects_unregistered_directory_without_removing_it", func(t *testing.T) {
		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "resume-stale.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Resume Stale\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/resume-stale.md")
		runGit(t, dir, "commit", "-m", "add resume stale plan")

		wtPath := filepath.Join(dir, ".loopai", "worktrees", "resume-stale")
		require.NoError(t, os.MkdirAll(wtPath, 0o750))

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		err = runWithWorktree(t.Context(), opts{ResumeWorktree: true, NoColor: true}, executePlanRequest{
			PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc,
			Config: &config.Config{WorktreeEnabled: true}, Colors: testColors(),
			DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a registered git worktree")
		assert.DirExists(t, wtPath, "invalid resume targets must never be removed")
	})

	t.Run("rejects_wrong_branch_without_removing_worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "resume-branch.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Resume Branch\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/resume-branch.md")
		runGit(t, dir, "commit", "-m", "add resume branch plan")

		wtPath := filepath.Join(dir, ".loopai", "worktrees", "resume-branch")
		runGit(t, dir, "worktree", "add", wtPath, "-b", "different-branch")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wtPath) })
		err = runWithWorktree(t.Context(), opts{ResumeWorktree: true, NoColor: true}, executePlanRequest{
			PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc,
			Config: &config.Config{WorktreeEnabled: true}, Colors: testColors(),
			DefaultBranch: "master", WtCleanup: &cleanupHolder{},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `expected branch "resume-branch", found "different-branch"`)
		assert.DirExists(t, wtPath, "branch mismatch must not remove the existing worktree")
	})

	t.Run("preserves_dirty_worktree_on_failure", func(t *testing.T) {
		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "resume-preserve.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# Resume Preserve\n\n- [ ] no task section\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/resume-preserve.md")
		runGit(t, dir, "commit", "-m", "add resume preserve plan")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		wtPath, _, err := gitSvc.CreateWorktreeForPlan(planPath, "")
		require.NoError(t, err)
		t.Cleanup(func() { _ = gitSvc.RemoveWorktree(wtPath) })

		dirtyPath := filepath.Join(wtPath, "unfinished.txt")
		require.NoError(t, os.WriteFile(dirtyPath, []byte("keep me"), 0o600))

		err = runWithWorktree(t.Context(), opts{
			ResumeWorktree: true, TasksOnly: true, MaxIterations: 1, NoColor: true,
		}, executePlanRequest{
			PlanFile: planPath, Mode: processor.ModeTasksOnly, GitSvc: gitSvc,
			Config: &config.Config{WorktreeEnabled: true}, Colors: testColors(),
			DefaultBranch: "master", BaseRef: "master", WtCleanup: &cleanupHolder{},
		})
		require.Error(t, err)
		assert.DirExists(t, wtPath)
		assert.FileExists(t, dirtyPath, "failed resume must preserve unfinished changes")
	})

	t.Run("removes_worktree_after_success", func(t *testing.T) {
		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "resume-complete.md")
		require.NoError(t, os.WriteFile(planPath, []byte(
			"# Resume Complete\n\n### Task 1: Done\n\n- [x] already complete\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/resume-complete.md")
		runGit(t, dir, "commit", "-m", "add resume complete plan")

		gitSvc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		wtPath, _, err := gitSvc.CreateWorktreeForPlan(planPath, "")
		require.NoError(t, err)

		fakeClaude := filepath.Join(t.TempDir(), "fake-claude")
		writeExecutable(t, fakeClaude, `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:ALL_TASKS_DONE>>>"}}'
printf '%s\n' '{"type":"result","result":""}'
`)
		binDir := t.TempDir()
		argvLog := filepath.Join(binDir, "cmux-argv.log")
		writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
		t.Setenv("CMUX_ARGV_LOG", argvLog)

		err = runWithWorktree(t.Context(), opts{
			ResumeWorktree: true, TasksOnly: true, MaxIterations: 1, NoColor: true,
		}, executePlanRequest{
			PlanFile: planPath, Mode: processor.ModeTasksOnly, GitSvc: gitSvc,
			Config: &config.Config{WorktreeEnabled: true, ClaudeCommand: fakeClaude}, Colors: testColors(),
			DefaultBranch: "master", BaseRef: "master", WtCleanup: &cleanupHolder{},
		})
		require.NoError(t, err)
		assert.NoDirExists(t, wtPath)
		assert.True(t, branchExists(t, dir, "resume-complete"), "successful resume preserves the feature branch")
		recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readErr)
		output := string(recorded)
		statusPos := strings.LastIndex(output, "set-status loopai done in")
		clearPos := strings.LastIndex(output, "clear-status loopai")
		assert.Greater(t, statusPos, clearPos, "the final success pill must survive Stop: %s", output)
		assert.Contains(t, output, "workspace loading off --id loopai")
		assert.Contains(t, output, "clear-progress")
	})
}

func TestResolveWorktreePlanFile(t *testing.T) {
	mainRoot := t.TempDir()
	worktreeRoot := t.TempDir()
	planPath := filepath.Join(mainRoot, "docs", "plans", "feature.md")

	assert.Equal(t, filepath.Join(worktreeRoot, "docs", "plans", "feature.md"),
		resolveWorktreePlanFile(planPath, mainRoot, worktreeRoot))
	assert.Equal(t, "docs/plans/feature.md",
		resolveWorktreePlanFile("docs/plans/feature.md", mainRoot, worktreeRoot))

	outside := filepath.Join(filepath.Dir(mainRoot), "outside.md")
	assert.Equal(t, outside, resolveWorktreePlanFile(outside, mainRoot, worktreeRoot))
}

func TestWorktreeMode_SkippedForNonBranchModes(t *testing.T) {
	// worktree mode guard: cfg.WorktreeEnabled && planFile != "" && modeRequiresBranch(mode)
	// for modes that don't require a branch, worktree should not be activated.
	// this is tested via modeRequiresBranch which already has coverage.
	// here we verify the guard condition explicitly.

	t.Run("worktree_skipped_for_review_mode", func(t *testing.T) {
		skipIfClaudeNotAvailable(t)

		dir := setupTestRepo(t)
		origDir, err := os.Getwd()
		require.NoError(t, err)
		require.NoError(t, os.Chdir(dir))
		t.Cleanup(func() { _ = os.Chdir(origDir) })

		require.NoError(t, os.MkdirAll("docs/plans", 0o750))
		planPath := filepath.Join(dir, "docs", "plans", "wt-skip.md")
		require.NoError(t, os.WriteFile(planPath, []byte("# WT Skip\n"), 0o600))
		runGit(t, dir, "add", "docs/plans/wt-skip.md")
		runGit(t, dir, "commit", "-m", "add wt skip plan")

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		o := opts{Worktree: true, Review: true, PlanFile: planPath, MaxIterations: 1, NoColor: true, ConfigDir: t.TempDir()}
		_ = run(ctx, o)

		// no worktree directory should exist
		wtPath := filepath.Join(dir, ".loopai", "worktrees", "wt-skip")
		assert.NoDirExists(t, wtPath, "review mode should not create worktree")

		// should stay on master
		gitSvc, gitErr := git.NewService(dir, noopLogger())
		require.NoError(t, gitErr)
		branch, brErr := gitSvc.CurrentBranch()
		require.NoError(t, brErr)
		assert.Equal(t, "master", branch, "review mode should stay on master")
	})
}

func TestRunWithWorktree_UntrackedPlan(t *testing.T) {
	skipIfClaudeNotAvailable(t)

	dir := setupTestRepo(t)
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	// create plan file but do NOT commit it (untracked)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
	planPath := filepath.Join(dir, "docs", "plans", "wt-untracked.md")
	require.NoError(t, os.WriteFile(planPath, []byte("# WT Untracked\n\n- [ ] task 1\n"), 0o600))

	gitSvc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)

	colors := testColors()
	cfg := &config.Config{WorktreeEnabled: true}
	wtCleanup := &cleanupHolder{}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = runWithWorktree(ctx, opts{MaxIterations: 1, NoColor: true}, executePlanRequest{
		PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc, Config: cfg,
		Colors: colors, DefaultBranch: "master", WtCleanup: wtCleanup,
	})
	// should fail with context canceled from the runner, but plan should be committed on branch
	require.Error(t, err)

	// verify CWD restored
	cwd, cwdErr := os.Getwd()
	require.NoError(t, cwdErr)
	assert.Equal(t, resolvedDir, cwd, "cwd should be restored after runWithWorktree")

	// verify branch was created and plan was committed there
	assert.True(t, branchExists(t, dir, "wt-untracked"), "branch should exist")

	// verify worktree cleaned up
	wtPath := filepath.Join(dir, ".loopai", "worktrees", "wt-untracked")
	assert.NoDirExists(t, wtPath, "worktree should be removed")
}

func TestRunWithWorktree_CreateWorktreeError(t *testing.T) {
	dir := setupTestRepo(t)
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	// create plan file and commit it
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
	planPath := filepath.Join(dir, "docs", "plans", "wt-fail.md")
	require.NoError(t, os.WriteFile(planPath, []byte("# WT Fail\n"), 0o600))
	runGit(t, dir, "add", "docs/plans/wt-fail.md")
	runGit(t, dir, "commit", "-m", "add wt fail plan")

	gitSvc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)

	// pre-create worktree dir to force "already exists" error
	wtPath := filepath.Join(dir, ".loopai", "worktrees", "wt-fail")
	require.NoError(t, os.MkdirAll(wtPath, 0o750))

	colors := testColors()
	cfg := &config.Config{WorktreeEnabled: true}
	wtCleanup := &cleanupHolder{}

	err = runWithWorktree(t.Context(), opts{MaxIterations: 1, NoColor: true}, executePlanRequest{
		PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc, Config: cfg,
		Colors: colors, DefaultBranch: "master", WtCleanup: wtCleanup,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preflight worktree creation")
}

func TestRunWithWorktree_HandsOffFailureNotification(t *testing.T) {
	skipIfClaudeNotAvailable(t)

	// once setup succeeds and executePlan is entered, the downstream funnel owns the failure
	// banner. the handedOff gate is what stops runWithWorktree from raising a second one, so the
	// assertion is the notify *count* — with the gate broken every worktree failure double-banners.
	dir := setupTestRepo(t)
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	// plan with a checkbox but no "### Task N:" section parses to zero tasks, so executePlan
	// fails deterministically inside validation without ever reaching the executor
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
	planPath := filepath.Join(dir, "docs", "plans", "wt-once.md")
	require.NoError(t, os.WriteFile(planPath, []byte("# WT Once\n\n- [ ] task 1\n"), 0o600))
	runGit(t, dir, "add", "docs/plans/wt-once.md")
	runGit(t, dir, "commit", "-m", "add wt once plan")

	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argvLog + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "cmux"), []byte(script), 0o755)) //nolint:gosec // test fixture must be executable
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

	gitSvc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)

	err = runWithWorktree(t.Context(), opts{MaxIterations: 1, NoColor: true}, executePlanRequest{
		PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc, Config: &config.Config{WorktreeEnabled: true},
		Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
	})
	require.Error(t, err, "executePlan must fail so there is a failure to report")

	recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
	require.NoError(t, readErr, "the handed-off run must still reach the cmux CLI")
	assert.Equal(t, 1, strings.Count(string(recorded), "notify --title"),
		"exactly one funnel may banner a failure, got:\n%s", recorded)
	assert.Equal(t, 1, strings.Count(string(recorded), "set-status loopai failed"),
		"the handed-off failure must leave exactly one persistent failure pill, got:\n%s", recorded)
}

func TestRunWithWorktree_NotifiesSetupFailure(t *testing.T) {
	// worktree setup runs before executePlan creates its own reporter, so runWithWorktree must
	// raise the cmux banner for its own failures: the plan-mode handoff already stopped its
	// reporter and a direct run never had one, so nothing downstream would report this.
	dir := setupTestRepo(t)
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "plans"), 0o750))
	planPath := filepath.Join(dir, "docs", "plans", "wt-notify.md")
	require.NoError(t, os.WriteFile(planPath, []byte("# WT Notify\n"), 0o600))
	runGit(t, dir, "add", "docs/plans/wt-notify.md")
	runGit(t, dir, "commit", "-m", "add wt notify plan")

	// fake cmux binary recording argv, so the best-effort notify call becomes observable.
	// prepended to PATH rather than replacing it, git is still needed by the service below.
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argvLog + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "cmux"), []byte(script), 0o755)) //nolint:gosec // test fixture must be executable
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

	gitSvc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)

	// pre-create the worktree dir to force an "already exists" failure during setup
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".loopai", "worktrees", "wt-notify"), 0o750))

	err = runWithWorktree(t.Context(), opts{MaxIterations: 1, NoColor: true}, executePlanRequest{
		PlanFile: planPath, Mode: processor.ModeFull, GitSvc: gitSvc, Config: &config.Config{WorktreeEnabled: true},
		Colors: testColors(), DefaultBranch: "master", WtCleanup: &cleanupHolder{},
	})
	require.Error(t, err)

	recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
	require.NoError(t, readErr, "setup failure must reach the cmux CLI")
	assert.Contains(t, string(recorded), "notify")
	assert.Contains(t, string(recorded), "run failed")
	assert.NotContains(t, string(recorded), "set-status loopai failed",
		"setup failure occurs before execution and must not leave a persistent failure pill")
	assert.Contains(t, string(recorded), "wt-notify.md", "the notification body names the run")
}

// chdirTemp changes to a temporary directory and restores the original on cleanup.
func chdirTemp(t *testing.T) {
	t.Helper()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { _ = os.Chdir(origDir) })
}

func TestSetupProgressLogger(t *testing.T) {
	t.Run("creates_new_logger_when_not_provided", func(t *testing.T) {
		chdirTemp(t)

		colors := testColors()
		req := executePlanRequest{PlanFile: "test-plan.md", Mode: processor.ModeFull, Colors: colors}
		plr, err := setupProgressLogger(opts{NoColor: true}, req, "test-branch")
		require.NoError(t, err)
		defer plr.closeLog()

		assert.NotNil(t, plr.holder)
		assert.NotNil(t, plr.baseLog)
		assert.NotNil(t, plr.closeLog)
		assert.NotEmpty(t, plr.baseLog.Path())
	})

	t.Run("uses_provided_logger_and_holder", func(t *testing.T) {
		chdirTemp(t)

		colors := testColors()
		existingHolder := &status.PhaseHolder{}
		existingLog, err := progress.NewLogger(progress.Config{
			PlanFile: "pre-created.md", Mode: "full", Branch: "main", NoColor: true,
		}, colors, existingHolder)
		require.NoError(t, err)
		defer func() { _ = existingLog.Close() }()

		req := executePlanRequest{
			PlanFile:    "test-plan.md",
			Mode:        processor.ModeFull,
			Colors:      colors,
			ProgressLog: existingLog,
			PhaseHolder: existingHolder,
		}
		plr, err := setupProgressLogger(opts{NoColor: true}, req, "test-branch")
		require.NoError(t, err)

		assert.Equal(t, existingHolder, plr.holder, "should reuse provided holder")
		assert.Equal(t, existingLog, plr.baseLog, "should reuse provided logger")

		// closeLog should be a no-op (externally-owned logger)
		plr.closeLog()
	})

	t.Run("creates_holder_when_not_provided", func(t *testing.T) {
		chdirTemp(t)

		colors := testColors()
		req := executePlanRequest{PlanFile: "holder-test.md", Mode: processor.ModeReview, Colors: colors}
		plr, err := setupProgressLogger(opts{NoColor: true}, req, "main")
		require.NoError(t, err)
		defer plr.closeLog()

		assert.NotNil(t, plr.holder, "should create new holder when not provided")
	})

	t.Run("close_is_idempotent", func(t *testing.T) {
		chdirTemp(t)

		colors := testColors()
		req := executePlanRequest{PlanFile: "idempotent.md", Mode: processor.ModeFull, Colors: colors}
		plr, err := setupProgressLogger(opts{NoColor: true}, req, "main")
		require.NoError(t, err)

		// calling closeLog multiple times should not panic
		plr.closeLog()
		plr.closeLog()
	})
}

func TestSendNotification(t *testing.T) {
	t.Run("nil_service_is_noop", func(t *testing.T) {
		req := executePlanRequest{Mode: processor.ModeFull, PlanFile: "test.md"}
		// should not panic with nil NotifySvc
		sendNotification(req, "main", "5s", git.DiffStats{}, nil)
		sendNotification(req, "main", "5s", git.DiffStats{}, errors.New("test error"))
	})
}

func TestBuildNotifyResult(t *testing.T) {
	t.Run("success_result", func(t *testing.T) {
		req := executePlanRequest{
			Mode: processor.ModeFull, PlanFile: "plan.md",
			ExternalReview: externalReviewSelection{Resolved: true, Reviewers: []resolvedReviewer{
				{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "xhigh"},
				{Provider: config.ExternalReviewToolClaude, Model: "fable", Effort: "max"},
			}},
		}
		stats := git.DiffStats{Files: 3, Additions: 100, Deletions: 20}
		result := buildNotifyResult(req, "feature-branch", "1m30s", stats, nil)

		assert.Equal(t, "success", result.Status)
		assert.Equal(t, "full", result.Mode)
		assert.Equal(t, "plan.md", result.PlanFile)
		assert.Equal(t, "feature-branch", result.Branch)
		assert.Equal(t, "1m30s", result.Duration)
		assert.Equal(t, "codex (gpt-5.5:xhigh) → claude (fable:max)", result.ExternalReview)
		assert.Equal(t, 3, result.Files)
		assert.Equal(t, 100, result.Additions)
		assert.Equal(t, 20, result.Deletions)
		assert.Empty(t, result.Error)
	})

	t.Run("failure_result", func(t *testing.T) {
		req := executePlanRequest{Mode: processor.ModeReview, PlanFile: "review.md", ExternalReview: externalReviewSelection{Resolved: true}}
		result := buildNotifyResult(req, "main", "45s", git.DiffStats{}, errors.New("runner failed"))

		assert.Equal(t, "failure", result.Status)
		assert.Equal(t, "review", result.Mode)
		assert.Equal(t, "review.md", result.PlanFile)
		assert.Equal(t, "main", result.Branch)
		assert.Equal(t, "45s", result.Duration)
		assert.Equal(t, "runner failed", result.Error)
		assert.Empty(t, result.ExternalReview)
		assert.Zero(t, result.Files)
		assert.Zero(t, result.Additions)
		assert.Zero(t, result.Deletions)
	})

	t.Run("legacy_single_reviewer_is_omitted", func(t *testing.T) {
		req := executePlanRequest{
			Mode: processor.ModeFull,
			ExternalReview: externalReviewSelection{Resolved: true, Reviewers: []resolvedReviewer{{
				Provider: config.ExternalReviewToolCodex,
				Model:    "gpt-5.5",
				Effort:   "xhigh",
			}}},
		}

		result := buildNotifyResult(req, "main", "45s", git.DiffStats{}, nil)

		assert.Empty(t, result.ExternalReview)
	})
}

func TestDisplayStats(t *testing.T) {
	t.Run("with_diff_stats", func(t *testing.T) {
		chdirTemp(t)

		colors := testColors()
		holder := &status.PhaseHolder{}
		baseLog, err := progress.NewLogger(progress.Config{
			PlanFile: "stats-test.md", Mode: "full", Branch: "main", NoColor: true,
		}, colors, holder)
		require.NoError(t, err)
		defer func() { _ = baseLog.Close() }()

		req := executePlanRequest{PlanFile: "docs/plans/feature.md", Colors: colors}
		stats := git.DiffStats{Files: 5, Additions: 200, Deletions: 50}
		displayStats(req, baseLog, stats, "2m15s", "feature-branch", false)
	})

	t.Run("without_diff_stats", func(t *testing.T) {
		chdirTemp(t)

		colors := testColors()
		holder := &status.PhaseHolder{}
		baseLog, err := progress.NewLogger(progress.Config{
			PlanFile: "no-stats.md", Mode: "full", Branch: "main", NoColor: true,
		}, colors, holder)
		require.NoError(t, err)
		defer func() { _ = baseLog.Close() }()

		req := executePlanRequest{Colors: colors}
		displayStats(req, baseLog, git.DiffStats{}, "30s", "main", false)
	})

	t.Run("with_main_plan_file", func(t *testing.T) {
		chdirTemp(t)

		colors := testColors()
		holder := &status.PhaseHolder{}
		baseLog, err := progress.NewLogger(progress.Config{
			PlanFile: "main-plan.md", Mode: "full", Branch: "main", NoColor: true,
		}, colors, holder)
		require.NoError(t, err)
		defer func() { _ = baseLog.Close() }()

		req := executePlanRequest{
			PlanFile:     "worktree/docs/plans/feature.md",
			MainPlanFile: "docs/plans/feature.md",
			Colors:       colors,
		}
		displayStats(req, baseLog, git.DiffStats{Files: 1, Additions: 10, Deletions: 5}, "10s", "feature-wt", false)
	})

	// plan-path display must reflect the actual location of the plan file:
	// completed/ path only when the move succeeded, original path when the move was
	// skipped or failed. The caller (executePlan) passes planMoved=true only after
	// a successful MovePlanToCompleted call, so this test drives the flag directly.
	t.Run("plan_path_reflects_plan_moved_flag", func(t *testing.T) {
		tests := []struct {
			name      string
			req       executePlanRequest
			planMoved bool
			wantPath  string
		}{
			{
				name: "moved_shows_completed_path",
				req: executePlanRequest{
					PlanFile: "docs/plans/feature.md",
					Mode:     processor.ModeFull,
					Config:   &config.Config{MovePlanOnCompletion: true},
				},
				planMoved: true,
				wantPath:  filepath.Join("docs", "plans", "completed", "feature.md"),
			},
			{
				name: "not_moved_shows_original_path",
				req: executePlanRequest{
					PlanFile: "docs/plans/feature.md",
					Mode:     processor.ModeFull,
					Config:   &config.Config{MovePlanOnCompletion: false},
				},
				planMoved: false,
				wantPath:  "docs/plans/feature.md",
			},
			{
				name: "move_failed_shows_original_path",
				req: executePlanRequest{
					PlanFile: "docs/plans/feature.md",
					Mode:     processor.ModeFull,
					Config:   &config.Config{MovePlanOnCompletion: true},
				},
				planMoved: false,
				wantPath:  "docs/plans/feature.md",
			},
			{
				name: "review_mode_not_moved_shows_original_path",
				req: executePlanRequest{
					PlanFile: "docs/plans/feature.md",
					Mode:     processor.ModeReview,
					Config:   &config.Config{MovePlanOnCompletion: true},
				},
				planMoved: false,
				wantPath:  "docs/plans/feature.md",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				chdirTemp(t)
				colors := testColors()
				holder := &status.PhaseHolder{}
				baseLog, err := progress.NewLogger(progress.Config{
					PlanFile: "x.md", Mode: "full", Branch: "main", NoColor: true,
				}, colors, holder)
				require.NoError(t, err)
				defer func() { _ = baseLog.Close() }()

				req := tc.req
				req.Colors = colors

				output := captureStdout(t, func() {
					displayStats(req, baseLog, git.DiffStats{}, "1s", "main", tc.planMoved)
				})
				assert.Contains(t, output, "  plan: "+tc.wantPath+"\n")
			})
		}
	})
}

func TestDisplayMeta(t *testing.T) {
	tests := []struct {
		name, planFile, branch, progressPath string
		indent                               int
		wantContains                         []string
		wantNotContains                      []string
	}{
		{name: "no_indent_with_plan", indent: 0, planFile: "docs/plans/feature.md", branch: "feature-branch",
			progressPath: ".loopai/progress/progress-feature.txt",
			wantContains: []string{"plan: docs/plans/feature.md", "branch: feature-branch", "progress log: .loopai/progress/progress-feature.txt"}},
		{name: "indented_with_plan", indent: 2, planFile: "docs/plans/feature.md", branch: "main",
			progressPath: ".loopai/progress/progress-feature.txt",
			wantContains: []string{"  plan: docs/plans/feature.md", "  branch: main", "  progress log: .loopai/progress/progress-feature.txt"}},
		{name: "no_plan_file", indent: 0, planFile: "", branch: "develop",
			progressPath:    ".loopai/progress/progress.txt",
			wantContains:    []string{"branch: develop", "progress log: .loopai/progress/progress.txt"},
			wantNotContains: []string{"plan:"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			colors := testColors()
			var buf bytes.Buffer
			origOutput := color.Output
			color.Output = &buf
			t.Cleanup(func() { color.Output = origOutput })

			displayMeta(colors, tc.indent, tc.planFile, tc.branch, tc.progressPath)

			out := buf.String()
			for _, want := range tc.wantContains {
				assert.Contains(t, out, want)
			}
			for _, notWant := range tc.wantNotContains {
				assert.NotContains(t, out, notWant)
			}
		})
	}
}

func TestKeepDashboardAlive(t *testing.T) {
	t.Run("noop_when_serve_disabled", func(t *testing.T) {
		colors := testColors()
		req := executePlanRequest{Colors: colors}
		closeCalled := false
		closeLog := func() { closeCalled = true }

		keepDashboardAlive(t.Context(), opts{Serve: false}, req, closeLog)
		assert.False(t, closeCalled, "closeLog should not be called when serve is disabled")
	})

	t.Run("blocks_until_context_canceled", func(t *testing.T) {
		colors := testColors()
		req := executePlanRequest{Colors: colors}
		closeCalled := false
		closeLog := func() { closeCalled = true }

		ctx, cancel := context.WithCancel(t.Context())
		cancel() // cancel immediately

		keepDashboardAlive(ctx, opts{Serve: true, Port: 9999, Host: "127.0.0.1"}, req, closeLog)
		assert.True(t, closeCalled, "closeLog should be called when serve is enabled")
	})
}

func TestMakePauseHandler_EnterResumes(t *testing.T) {
	stdin := bytes.NewReader([]byte("\n"))
	var stdout bytes.Buffer
	handler := makePauseHandler(stdin, &stdout)
	result := handler(context.Background())
	assert.True(t, result, "handler should return true on Enter")
	assert.Contains(t, stdout.String(), "session interrupted")
}

func TestMakePauseHandler_ContextCancelAborts(t *testing.T) {
	// stdin that blocks forever (never returns)
	r, w := io.Pipe()
	defer w.Close()
	defer r.Close()
	var stdout bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	handler := makePauseHandler(r, &stdout)
	result := handler(ctx)
	assert.False(t, result, "handler should return false on context cancel")
}

func TestMakePauseHandler_EOFAborts(t *testing.T) {
	// empty reader returns EOF immediately, treated as abort (safe default for piped stdin)
	stdin := bytes.NewReader(nil)
	var stdout bytes.Buffer
	handler := makePauseHandler(stdin, &stdout)
	result := handler(context.Background())
	assert.False(t, result, "handler should return false on EOF (stdin closed = abort)")
}

func TestCleanupHolder(t *testing.T) {
	t.Run("call before set is a no-op", func(t *testing.T) {
		h := &cleanupHolder{}
		assert.NotPanics(t, h.call)
	})

	t.Run("call runs the registered function", func(t *testing.T) {
		h := &cleanupHolder{}
		calls := 0
		h.set(func() { calls++ })
		h.call()
		assert.Equal(t, 1, calls)
	})

	t.Run("double call is safe and runs the function again", func(t *testing.T) {
		h := &cleanupHolder{}
		calls := 0
		h.set(func() { calls++ })
		h.call()
		h.call()
		assert.Equal(t, 2, calls, "the holder does not deduplicate, callers use sync.Once for that")
	})

	t.Run("set replaces the previous function", func(t *testing.T) {
		h := &cleanupHolder{}
		first, second := false, false
		h.set(func() { first = true })
		h.set(func() { second = true })
		h.call()
		assert.False(t, first, "the replaced function must not run")
		assert.True(t, second)
	})

	t.Run("concurrent set and call are race-free", func(t *testing.T) {
		h := &cleanupHolder{}
		var calls atomic.Int64
		var wg sync.WaitGroup
		for range 50 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				h.set(func() { calls.Add(1) })
			}()
			go func() {
				defer wg.Done()
				h.call()
			}()
		}
		wg.Wait()
		h.call()
		assert.Positive(t, calls.Load(), "the last registered function must still be callable")
	})
}

func TestCmuxCompletionNotice(t *testing.T) {
	tests := []struct {
		name             string
		planFile         string
		branch           string
		elapsed          string
		runErr           error
		wantOK           bool
		wantSubtitle     string
		wantBodyContains string
	}{
		{
			name: "success names the plan", planFile: "/repo/docs/plans/20260728-feature.md", branch: "feature",
			elapsed: "12m30s", wantOK: true, wantSubtitle: "run completed",
			wantBodyContains: "20260728-feature.md in 12m30s",
		},
		{
			name: "success without plan falls back to branch", branch: "review-branch", elapsed: "2m",
			wantOK: true, wantSubtitle: "run completed", wantBodyContains: "review-branch in 2m",
		},
		{
			name: "failure carries the error", planFile: "/repo/docs/plans/feature.md", branch: "feature",
			elapsed: "1m", runErr: errors.New("boom"), wantOK: true, wantSubtitle: "run failed",
			wantBodyContains: "feature.md: boom",
		},
		{
			name: "wrapped failure carries the error", planFile: "feature.md", branch: "feature", elapsed: "1m",
			runErr: fmt.Errorf("runner: %w", errors.New("inner")), wantOK: true, wantSubtitle: "run failed",
			wantBodyContains: "feature.md: runner: inner",
		},
		{
			name: "user abort is not announced", planFile: "feature.md", branch: "feature", elapsed: "1m",
			runErr: processor.ErrUserAborted, wantOK: false,
		},
		{
			name: "wrapped user abort is not announced", planFile: "feature.md", branch: "feature", elapsed: "1m",
			runErr: fmt.Errorf("runner: %w", processor.ErrUserAborted), wantOK: false,
		},
		{
			name: "ctrl-c cancellation is not announced", planFile: "feature.md", branch: "feature", elapsed: "1m",
			runErr: context.Canceled, wantOK: false,
		},
		{
			// SIGINT reaches executePlan as this shape, not as ErrUserAborted
			name: "wrapped ctrl-c cancellation is not announced", planFile: "feature.md", branch: "feature",
			elapsed: "1m", runErr: fmt.Errorf("runner: task phase: %w", context.Canceled), wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subtitle, body, ok := cmuxCompletionNotice(tt.planFile, tt.branch, tt.elapsed, tt.runErr)
			assert.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				assert.Empty(t, subtitle)
				assert.Empty(t, body)
				return
			}
			assert.Equal(t, tt.wantSubtitle, subtitle)
			assert.Contains(t, body, tt.wantBodyContains)
		})
	}
}

func TestNotifyCmuxCompletion_NilReporter(t *testing.T) {
	// outside cmux the reporter is nil; the helper must stay a no-op for every outcome
	assert.NotPanics(t, func() {
		notifyCmuxCompletion(nil, "plan.md", "branch", "1m", nil)
		notifyCmuxCompletion(nil, "plan.md", "branch", "1m", errors.New("boom"))
		notifyCmuxCompletion(nil, "", "branch", "1m", processor.ErrUserAborted)
	})
}

func TestFinishCmuxCompletion(t *testing.T) {
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "cmux"), []byte(script), 0o755)) //nolint:gosec // test fixture must be executable
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
	t.Setenv("CMUX_ARGV_LOG", argvLog)

	tests := []struct {
		name       string
		runErr     error
		wantStatus string
		wantNotify bool
	}{
		{
			name:       "success finishes with elapsed time",
			wantStatus: "set-status loopai done in 12m30s --icon bolt --color #34c759 --priority 90",
			wantNotify: true,
		},
		{
			name:       "run error finishes with error detail",
			runErr:     errors.New("boom"),
			wantStatus: "set-status loopai failed · boom --icon exclamationmark.triangle --color #ff3b30 --priority 90",
			wantNotify: true,
		},
		{name: "user abort does not finish", runErr: processor.ErrUserAborted},
		{name: "context cancellation does not finish", runErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.RemoveAll(argvLog))
			rep := cmux.New("plan.md", cmux.Models{})
			require.NotNil(t, rep)

			finishCmuxCompletion(rep, "plan.md", "branch", "12m30s", tt.runErr)

			recorded, err := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
			if !tt.wantNotify {
				assert.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			output := string(recorded)
			assert.Contains(t, output, "notify --title loopai")
			assert.Contains(t, output, tt.wantStatus)
		})
	}
}

func TestFinishCmuxCompletion_NilReporter(t *testing.T) {
	assert.NotPanics(t, func() {
		finishCmuxCompletion(nil, "plan.md", "branch", "1m", nil)
		finishCmuxCompletion(nil, "plan.md", "branch", "1m", errors.New("boom"))
		finishCmuxCompletion(nil, "plan.md", "branch", "1m", context.Canceled)
	})
}

func TestRunCleanupBounded(t *testing.T) {
	tests := []struct {
		name        string
		nilCleanup  bool
		stuck       bool
		maxDuration time.Duration
	}{
		{name: "nil cleanup returns immediately", nilCleanup: true, maxDuration: 50 * time.Millisecond},
		{name: "fast cleanup runs to completion", maxDuration: 50 * time.Millisecond},
		{name: "stuck cleanup returns after timeout instead of blocking forever", stuck: true, maxDuration: 500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unblock := make(chan struct{})
			ran := make(chan struct{})
			defer close(unblock) // release a stuck cleanup goroutine at test end

			var cleanup func()
			switch {
			case tt.nilCleanup:
				cleanup = nil
			case tt.stuck:
				cleanup = func() { <-unblock } // models a hung Once.Do (git worktree remove stuck)
			default:
				cleanup = func() { close(ran) }
			}

			start := time.Now()
			runCleanupBounded(cleanup, 100*time.Millisecond)
			elapsed := time.Since(start)

			assert.Less(t, elapsed, tt.maxDuration, "runCleanupBounded must not block past its timeout")
			if !tt.nilCleanup && !tt.stuck {
				select {
				case <-ran:
				default:
					t.Fatal("cleanup should have run to completion")
				}
			}
		})
	}
}

// branchExists checks if a branch exists in the given git repository.
func branchExists(t *testing.T, dir, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "branch", "--list", branch)
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out)) != ""
}

// fakeBranchChecker reports branch existence from a fixed set and derives branch names the way
// git.Service does for an exact-case plan path.
type fakeBranchChecker struct{ branches []string }

func (f fakeBranchChecker) BranchExists(name string) bool {
	return slices.Contains(f.branches, name)
}

func (f fakeBranchChecker) EffectiveBranchName(planFile, branchOverride string) string {
	if branchOverride != "" {
		return branchOverride
	}
	return plan.ExtractBranchName(planFile)
}

func TestResolveFeatureBranch(t *testing.T) {
	plansDir := t.TempDir()
	completedDir := filepath.Join(plansDir, "completed")
	require.NoError(t, os.MkdirAll(completedDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(plansDir, "20260806-dynamic-review-agents.md"), []byte("# plan\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(completedDir, "20260805-section-duration-logging.md"), []byte("# plan\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(plansDir, "20260801-already-merged.md"), []byte("# plan\n"), 0o600))
	// plan file whose derived branch collides with an unrelated existing branch name
	require.NoError(t, os.WriteFile(filepath.Join(plansDir, "20260807-feature.md"), []byte("# plan\n"), 0o600))

	branches := []string{"master", "dynamic-review-agents", "section-duration-logging", "feature", "20260807-feature"}

	tests := []struct {
		name     string
		arg      string
		want     string
		wantErr  string
		errFrags []string
	}{
		{name: "existing branch name", arg: "dynamic-review-agents", want: "dynamic-review-agents"},
		{name: "plan basename with extension", arg: "20260806-dynamic-review-agents.md", want: "dynamic-review-agents"},
		{name: "plan basename without extension", arg: "20260806-dynamic-review-agents", want: "dynamic-review-agents"},
		{name: "plan in completed dir", arg: "20260805-section-duration-logging", want: "section-duration-logging"},
		{
			name: "plan path",
			arg:  filepath.Join(plansDir, "20260806-dynamic-review-agents.md"),
			want: "dynamic-review-agents",
		},
		{
			name: "plan path in completed dir",
			arg:  filepath.Join(completedDir, "20260805-section-duration-logging.md"),
			want: "section-duration-logging",
		},
		{
			name: "stale plan path falls back to completed lookup",
			arg:  filepath.Join(plansDir, "20260805-section-duration-logging.md"),
			want: "section-duration-logging",
		},
		{name: "branch match wins over plan match", arg: "20260807-feature", want: "20260807-feature"},
		{
			// a namespaced branch name must never be reduced to its last segment: doing so would
			// resolve 20260806-dynamic-review-agents.md and close out an unnamed branch
			name:     "missing namespaced branch does not fall back to plan basename",
			arg:      "feature/20260806-dynamic-review-agents",
			errFrags: []string{"feature/20260806-dynamic-review-agents", "no local branch with this name"},
		},
		{
			name:     "missing worktree path does not fall back to plan basename",
			arg:      filepath.Join(".loopai", "worktrees", "20260806-dynamic-review-agents"),
			errFrags: []string{"20260806-dynamic-review-agents", "no local branch with this name"},
		},
		{name: "whitespace is trimmed", arg: "  dynamic-review-agents  ", want: "dynamic-review-agents"},
		{name: "empty identifier", arg: "   ", wantErr: "empty feature identifier"},
		{
			name:     "unknown identifier lists searched locations",
			arg:      "no-such-thing",
			errFrags: []string{"no-such-thing", plansDir, completedDir},
		},
		{
			name:     "plan resolves to missing branch",
			arg:      "20260801-already-merged",
			errFrags: []string{"already-merged", "already merged"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFeatureBranch(fakeBranchChecker{branches: branches}, []string{t.TempDir()}, plansDir, tt.arg)
			if tt.wantErr != "" || len(tt.errFrags) > 0 {
				require.Error(t, err)
				assert.Empty(t, got)
				if tt.wantErr != "" {
					assert.Contains(t, err.Error(), tt.wantErr)
				}
				for _, frag := range tt.errFrags {
					assert.Contains(t, err.Error(), frag)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveFeatureBranchRelativePlansDir(t *testing.T) {
	repo := t.TempDir()
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(plansDir, "20260807-relative-plan.md"), []byte("# plan\n"), 0o600))

	t.Chdir(repo)

	checker := fakeBranchChecker{branches: []string{"relative-plan"}}
	got, err := resolveFeatureBranch(checker, []string{repo}, filepath.Join("docs", "plans"), "20260807-relative-plan")
	require.NoError(t, err)
	assert.Equal(t, "relative-plan", got)

	got, err = resolveFeatureBranch(checker, []string{repo}, filepath.Join("docs", "plans"),
		filepath.Join("docs", "plans", "20260807-relative-plan.md"))
	require.NoError(t, err)
	assert.Equal(t, "relative-plan", got)
}

func TestResolveFeatureBranchMissingPlansDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := resolveFeatureBranch(fakeBranchChecker{}, []string{t.TempDir()}, missing, "whatever")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whatever")
}

func TestResolveFeatureBranchCaseInsensitiveFilesystem(t *testing.T) {
	repo := setupTestRepo(t)
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(plansDir, "20260807-MixedCase.md"), []byte("# plan\n"), 0o600))
	if _, err := os.Stat(filepath.Join(plansDir, "20260807-mixedcase.md")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	runGit(t, repo, "branch", "MixedCase")
	svc, err := git.NewService(repo, noopLogger())
	require.NoError(t, err)

	// the branch must come from the on-disk plan name, not from the case the caller typed
	got, err := resolveFeatureBranch(svc, []string{repo}, plansDir, "20260807-mixedcase")
	require.NoError(t, err)
	assert.Equal(t, "MixedCase", got)
}

func TestResolveFeatureBranchPrefersRecordedBranch(t *testing.T) {
	repo := setupTestRepo(t)
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	planFile := filepath.Join(plansDir, "20260806-login.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
	writeProgressRecord(t, repo, "progress-login.txt", planFile, "fix/login", 1)

	// "login" exists but is unrelated: the run recorded fix/login via --branch, so resolving the
	// plan must never hand --merge the collision victim
	checker := fakeBranchChecker{branches: []string{"master", "login", "fix/login"}}
	got, err := resolveFeatureBranch(checker, []string{repo}, plansDir, "20260806-login")
	require.NoError(t, err)
	assert.Equal(t, "fix/login", got)

	t.Run("recorded branch deleted reports the recorded name", func(t *testing.T) {
		_, err := resolveFeatureBranch(fakeBranchChecker{branches: []string{"master", "login"}}, []string{repo}, plansDir,
			"20260806-login")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fix/login")
		assert.Contains(t, err.Error(), "already merged")
	})

	t.Run("unrecorded plan still derives from the filename", func(t *testing.T) {
		other := filepath.Join(plansDir, "20260807-signup.md")
		require.NoError(t, os.WriteFile(other, []byte("# plan\n"), 0o600))
		got, err := resolveFeatureBranch(fakeBranchChecker{branches: []string{"signup"}}, []string{repo}, plansDir,
			"20260807-signup")
		require.NoError(t, err)
		assert.Equal(t, "signup", got)
	})

	t.Run("newest record wins", func(t *testing.T) {
		writeProgressRecord(t, repo, "progress-login-rerun.txt", planFile, "fix/login-v2", 2)
		got, err := resolveFeatureBranch(fakeBranchChecker{branches: []string{"fix/login", "fix/login-v2"}},
			[]string{repo}, plansDir, "20260806-login")
		require.NoError(t, err)
		assert.Equal(t, "fix/login-v2", got)
	})
}

// TestResolveFeatureBranchIgnoresNonBranchCreatingRecords covers the review, codex-only, and plan
// runs that write their own record for the same plan, in the same directory and with a later
// mtime. their Branch header names whatever was checked out at the time, so honoring it would
// resolve the close-out to an unrelated branch that --merge then merges into base and deletes.
func TestResolveFeatureBranchIgnoresNonBranchCreatingRecords(t *testing.T) {
	branches := []string{"master", "fix/login", "unrelated"}

	for _, mode := range []string{"review", "codex-only", "plan"} {
		t.Run(mode+" record does not override the implementation run", func(t *testing.T) {
			repo := setupTestRepo(t)
			plansDir := filepath.Join(repo, "docs", "plans")
			require.NoError(t, os.MkdirAll(plansDir, 0o750))
			planFile := filepath.Join(plansDir, "20260806-login.md")
			require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
			writeProgressRecord(t, repo, "progress-20260806-login.txt", planFile, "fix/login", 1)
			writeProgressRecordMode(t, repo, "progress-20260806-login-"+mode+".txt", planFile, "unrelated", mode, 2)

			got, err := resolveFeatureBranch(fakeBranchChecker{branches: branches}, []string{repo}, plansDir, "20260806-login")
			require.NoError(t, err)
			assert.Equal(t, "fix/login", got)
		})
	}

	t.Run("review-only record falls back to filename derivation", func(t *testing.T) {
		repo := setupTestRepo(t)
		plansDir := filepath.Join(repo, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "20260806-login.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
		// a --review run on the base branch is the case that once produced the misleading
		// "already the base branch" error for a plan with a perfectly good feature branch
		writeProgressRecordMode(t, repo, "progress-20260806-login-review.txt", planFile, "master", "review", 1)

		got, err := resolveFeatureBranch(fakeBranchChecker{branches: []string{"master", "login"}}, []string{repo}, plansDir,
			"20260806-login")
		require.NoError(t, err)
		assert.Equal(t, "login", got)
	})

	t.Run("tasks-only and unknown modes still supply the branch", func(t *testing.T) {
		for _, mode := range []string{"tasks-only", "", "future-mode"} {
			repo := setupTestRepo(t)
			plansDir := filepath.Join(repo, "docs", "plans")
			require.NoError(t, os.MkdirAll(plansDir, 0o750))
			planFile := filepath.Join(plansDir, "20260806-login.md")
			require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
			writeProgressRecordMode(t, repo, "progress-20260806-login.txt", planFile, "fix/login", mode, 1)

			got, err := resolveFeatureBranch(fakeBranchChecker{branches: branches}, []string{repo}, plansDir, "20260806-login")
			require.NoError(t, err, "mode %q", mode)
			assert.Equal(t, "fix/login", got, "mode %q", mode)
		}
	})
}

// TestResolveFeatureBranchMatchesRecordAcrossPathSpellings covers the ways the located plan path
// and the recorded one legitimately differ. A miss falls back to deriving the branch from the
// filename, so each of these once resolved the unrelated "login" branch that --merge would then
// merge into base and delete.
func TestResolveFeatureBranchMatchesRecordAcrossPathSpellings(t *testing.T) {
	checker := fakeBranchChecker{branches: []string{"master", "login", "fix/login"}}

	t.Run("identifier typed in a different case", func(t *testing.T) {
		repo := setupTestRepo(t)
		plansDir := filepath.Join(repo, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "20260806-login.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
		if _, err := os.Stat(filepath.Join(plansDir, "20260806-LOGIN.md")); err != nil {
			t.Skip("case-sensitive filesystem")
		}
		writeProgressRecord(t, repo, "progress-login.txt", planFile, "fix/login", 1)

		got, err := resolveFeatureBranch(checker, []string{repo}, plansDir, "20260806-LOGIN")
		require.NoError(t, err)
		assert.Equal(t, "fix/login", got)
	})

	t.Run("plan present in both the active and completed directories", func(t *testing.T) {
		repo := setupTestRepo(t)
		plansDir := filepath.Join(repo, "docs", "plans")
		require.NoError(t, os.MkdirAll(filepath.Join(plansDir, "completed"), 0o750))
		planFile := filepath.Join(plansDir, "20260806-login.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(plansDir, "completed", "20260806-login.md"),
			[]byte("# plan\n"), 0o600))
		writeProgressRecord(t, repo, "progress-login.txt", planFile, "fix/login", 1)

		got, err := resolveFeatureBranch(checker, []string{repo}, plansDir, "20260806-login")
		require.NoError(t, err)
		assert.Equal(t, "fix/login", got)
	})
}

// TestResolveFeatureBranchMatchesRecordOutsideRepo covers records whose plan path names no file
// inside the repository: a supported out-of-tree plans_dir, and a checkout moved after the run.
// Resolving the plan to a repo-contained file is what the PR-metadata half of the scan needs, not
// the branch lookup, and dropping the record here silently falls back to filename derivation - so
// --merge would merge and delete the unrelated "login" branch instead of the recorded "fix/login".
func TestResolveFeatureBranchMatchesRecordOutsideRepo(t *testing.T) {
	checker := fakeBranchChecker{branches: []string{"master", "login", "fix/login"}}

	t.Run("plans dir outside the repository", func(t *testing.T) {
		repo := setupTestRepo(t)
		plansDir := filepath.Join(t.TempDir(), "shared-plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		planFile := filepath.Join(plansDir, "20260806-login.md")
		require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
		writeProgressRecord(t, repo, "progress-login.txt", planFile, "fix/login", 1)

		got, err := resolveFeatureBranch(checker, []string{repo}, plansDir, "20260806-login")
		require.NoError(t, err)
		assert.Equal(t, "fix/login", got)
	})

	t.Run("record naming a path the checkout no longer has", func(t *testing.T) {
		repo := setupTestRepo(t)
		plansDir := filepath.Join(repo, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(plansDir, "20260806-login.md"), []byte("# plan\n"), 0o600))
		// the run recorded the absolute path of a checkout that has since been moved
		stale := filepath.Join(t.TempDir(), "old-checkout", "docs", "plans", "20260806-login.md")
		writeProgressRecord(t, repo, "progress-login.txt", stale, "fix/login", 1)

		got, err := resolveFeatureBranch(checker, []string{repo}, plansDir, "20260806-login")
		require.NoError(t, err)
		assert.Equal(t, "fix/login", got)
	})
}

// TestResolveCloseoutBranchReadsProgressFromPrimaryWorktree covers a close-out invoked from a
// linked worktree, which README advertises. A run started from the primary checkout records
// there, so scanning only the invoking worktree found no association and silently derived "login".
func TestResolveCloseoutBranchReadsProgressFromPrimaryWorktree(t *testing.T) {
	repo := setupTestRepo(t)
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	planFile := filepath.Join(plansDir, "20260806-login.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "add plan")
	writeProgressRecord(t, repo, "progress-login.txt", planFile, "fix/login", 1)
	runGit(t, repo, "branch", "fix/login")
	runGit(t, repo, "branch", "login")
	linked := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", linked, "fix/login")

	svc, err := git.NewService(linked, noopLogger())
	require.NoError(t, err)
	target := closeoutTarget{identifier: "20260806-login", plansDir: filepath.Join("docs", "plans")}
	got, err := resolveCloseoutBranch(svc, target, "--merge")
	require.NoError(t, err)
	assert.Equal(t, "fix/login", got)
}

// TestResolveCloseoutBranchReadsProgressFromInvokingWorktree covers the mirror case: the run was
// started inside a linked worktree, so the progress logger resolved .loopai/progress against that
// checkout and the record never reached the primary. Anchoring the scan at the primary alone
// missed it and fell back to deriving "login" from the plan filename - the unrelated branch
// --merge would then merge into base and delete.
func TestResolveCloseoutBranchReadsProgressFromInvokingWorktree(t *testing.T) {
	repo := setupTestRepo(t)
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	planFile := filepath.Join(plansDir, "20260806-login.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "add plan")
	runGit(t, repo, "branch", "fix/login")
	runGit(t, repo, "branch", "login")
	linked := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", linked, "fix/login")
	// the record lives only in the linked worktree, mirroring a run launched from inside it
	writeProgressRecord(t, linked, "progress-login.txt", planFile, "fix/login", 1)

	svc, err := git.NewService(linked, noopLogger())
	require.NoError(t, err)
	target := closeoutTarget{identifier: "20260806-login", plansDir: filepath.Join("docs", "plans")}
	got, err := resolveCloseoutBranch(svc, target, "--merge")
	require.NoError(t, err)
	assert.Equal(t, "fix/login", got)
}

// TestResolveCloseoutBranchReadsProgressFromThirdWorktree covers a run started inside a linked
// worktree that is neither the primary nor the checkout the close-out runs from. Scanning only
// those two missed the record and fell back to deriving "login" from the plan filename - the
// unrelated branch --merge would then merge into base and delete.
func TestResolveCloseoutBranchReadsProgressFromThirdWorktree(t *testing.T) {
	repo := setupTestRepo(t)
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	planFile := filepath.Join(plansDir, "20260806-login.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "add plan")
	runGit(t, repo, "branch", "fix/login")
	runGit(t, repo, "branch", "login")
	runGit(t, repo, "branch", "scratch")
	other := filepath.Join(t.TempDir(), "wt-other")
	runGit(t, repo, "worktree", "add", other, "scratch")
	// the record lives only in that third worktree, and the close-out runs from the primary
	writeProgressRecord(t, other, "progress-login.txt", planFile, "fix/login", 1)

	svc, err := git.NewService(repo, noopLogger())
	require.NoError(t, err)
	target := closeoutTarget{identifier: "20260806-login", plansDir: filepath.Join("docs", "plans")}
	got, err := resolveCloseoutBranch(svc, target, "--merge")
	require.NoError(t, err)
	assert.Equal(t, "fix/login", got)
}

// TestRecordedBranchForPlanAcrossRoots locks the cross-root precedence: both checkouts can hold
// records for the same plan, and the newest wins regardless of which root it came from.
func TestRecordedBranchForPlanAcrossRoots(t *testing.T) {
	primary, linked := t.TempDir(), t.TempDir()
	planFile := filepath.Join(primary, "docs", "plans", "20260806-login.md")

	t.Run("record found in the second root", func(t *testing.T) {
		writeProgressRecord(t, linked, "progress-login.txt", planFile, "fix/login", 1)
		got, err := recordedBranchForPlan([]string{primary, linked}, planFile)
		require.NoError(t, err)
		assert.Equal(t, "fix/login", got)
	})

	t.Run("newest record wins across roots", func(t *testing.T) {
		writeProgressRecord(t, primary, "progress-login-rerun.txt", planFile, "fix/login-v2", 2)
		got, err := recordedBranchForPlan([]string{primary, linked}, planFile)
		require.NoError(t, err)
		assert.Equal(t, "fix/login-v2", got)
	})

	t.Run("missing progress directory is not an error", func(t *testing.T) {
		got, err := recordedBranchForPlan([]string{t.TempDir()}, planFile)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

// TestProgressRecordRoots checks that every registered worktree is scanned, with the primary
// first and the invoking checkout second, and that no directory is listed twice.
func TestProgressRecordRoots(t *testing.T) {
	repo := setupTestRepo(t)
	runGit(t, repo, "branch", "feature")
	runGit(t, repo, "branch", "other")
	linked := filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", linked, "feature")
	third := filepath.Join(t.TempDir(), "wt-third")
	runGit(t, repo, "worktree", "add", third, "other")

	t.Run("invoked from the primary still covers the linked worktrees", func(t *testing.T) {
		svc, err := git.NewService(repo, noopLogger())
		require.NoError(t, err)
		roots, err := progressRecordRoots(svc)
		require.NoError(t, err)
		require.Len(t, roots, 3)
		assert.True(t, sameProgressRoot(repo, roots[0]))
		assert.True(t, containsProgressRoot(roots, linked))
		assert.True(t, containsProgressRoot(roots, third))
	})

	t.Run("invoked from a linked worktree yields primary then invoking", func(t *testing.T) {
		svc, err := git.NewService(third, noopLogger())
		require.NoError(t, err)
		roots, err := progressRecordRoots(svc)
		require.NoError(t, err)
		require.Len(t, roots, 3)
		assert.True(t, sameProgressRoot(repo, roots[0]))
		assert.True(t, sameProgressRoot(third, roots[1]))
		assert.True(t, containsProgressRoot(roots, linked))
	})
}

// containsProgressRoot reports whether roots names dir, comparing the way progressRecordRoots
// itself dedupes so a symlinked temp directory does not fail the assertion.
func containsProgressRoot(roots []string, dir string) bool {
	for _, root := range roots {
		if sameProgressRoot(root, dir) {
			return true
		}
	}
	return false
}

func TestRecordedPlanInRepo(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	inside := filepath.Join(plansDir, "20260806-login.md")
	require.NoError(t, os.WriteFile(inside, []byte("# plan\n"), 0o600))

	t.Run("plain containment returns the path unchanged", func(t *testing.T) {
		assert.Equal(t, inside, recordedPlanInRepo(repo, inside))
	})

	t.Run("path behind a symlinked root is re-anchored at the root", func(t *testing.T) {
		link := filepath.Join(parent, "link")
		require.NoError(t, os.Symlink(repo, link))
		// spelled through the link, so a lexical test alone would reject it
		got := recordedPlanInRepo(repo, filepath.Join(link, "docs", "plans", "20260806-login.md"))
		assert.Equal(t, inside, got)
	})

	t.Run("plan genuinely outside the repository is rejected", func(t *testing.T) {
		outside := filepath.Join(parent, "elsewhere.md")
		require.NoError(t, os.WriteFile(outside, []byte("# plan\n"), 0o600))
		assert.Empty(t, recordedPlanInRepo(repo, outside))
	})

	t.Run("symlinked plan is rejected without following it", func(t *testing.T) {
		outside := filepath.Join(parent, "secret.md")
		require.NoError(t, os.WriteFile(outside, []byte("# secret\n"), 0o600))
		linked := filepath.Join(plansDir, "20260806-linked.md")
		require.NoError(t, os.Symlink(outside, linked))
		assert.Empty(t, recordedPlanInRepo(repo, linked))
	})

	t.Run("missing path is rejected", func(t *testing.T) {
		assert.Empty(t, recordedPlanInRepo(repo, filepath.Join(plansDir, "absent.md")))
	})

	t.Run("directory is rejected", func(t *testing.T) {
		assert.Empty(t, recordedPlanInRepo(repo, plansDir))
	})
}

// writeProgressRecord creates a progress file carrying the plan-to-branch association header.
// age orders the fixture's mtime explicitly so newest-record-wins tie-breaks are deterministic.
func writeProgressRecord(t *testing.T, repo, name, planFile, branch string, age int) {
	t.Helper()
	writeProgressRecordMode(t, repo, name, planFile, branch, "full", age)
}

// writeProgressRecordMode is writeProgressRecord with an explicit Mode header, covering the
// non-branch-creating runs whose Branch line names the checked-out branch rather than a feature.
func writeProgressRecordMode(t *testing.T, repo, name, planFile, branch, mode string, age int) {
	t.Helper()
	dir := filepath.Join(repo, ".loopai", "progress")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	path := filepath.Join(dir, name)
	body := fmt.Sprintf("Plan: %s\nBranch: %s\nMode: %s\n", planFile, branch, mode)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	stamp := time.Date(2026, 8, 7, 12, 0, age, 0, time.UTC)
	require.NoError(t, os.Chtimes(path, stamp, stamp))
}

// TestParseProgressAssociationStopsAtHeader locks the header-block boundary. Records written
// before the header carried Mode never satisfy a three-field stop condition, so a scan that keyed
// on collected fields ran into the log body, where executor output beginning with "Branch: " would
// replace the recorded branch - the branch --merge <plan> then merges into base and deletes.
func TestParseProgressAssociationStopsAtHeader(t *testing.T) {
	// an un-prefixed body line is what the boundary has to defend against: the current logger
	// timestamps everything it streams, but the parser must not depend on that to stay correct
	const body = "----\nBranch: other-branch\nPlan: /tmp/other.md\n"
	tests := []struct {
		name                 string
		content              string
		wantPlan, wantBranch string
		wantMode             string
	}{
		{
			name:       "complete header",
			content:    "# Loopai Progress Log\nPlan: docs/plans/login.md\nBranch: fix/login\nMode: full\n" + separatorLine() + "\n\n" + body,
			wantPlan:   "docs/plans/login.md",
			wantBranch: "fix/login",
			wantMode:   "full",
		},
		{
			name:       "legacy header without mode",
			content:    "# Loopai Progress Log\nPlan: docs/plans/login.md\nBranch: fix/login\n" + separatorLine() + "\n\n" + body,
			wantPlan:   "docs/plans/login.md",
			wantBranch: "fix/login",
		},
		{
			name:       "legacy header with crlf line endings",
			content:    "# Loopai Progress Log\r\nPlan: docs/plans/login.md\r\nBranch: fix/login\r\n" + separatorLine() + "\r\n\r\n" + body,
			wantPlan:   "docs/plans/login.md",
			wantBranch: "fix/login",
		},
		{
			name:       "truncated record with no separator",
			content:    "# Loopai Progress Log\nPlan: docs/plans/login.md\nBranch: fix/login\n",
			wantPlan:   "docs/plans/login.md",
			wantBranch: "fix/login",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			planPath, branch, mode := parseProgressAssociation(tc.content)
			assert.Equal(t, tc.wantPlan, planPath)
			assert.Equal(t, tc.wantBranch, branch)
			assert.Equal(t, tc.wantMode, mode)
		})
	}
}

// TestResolveFeatureBranchIgnoresBranchLineInLogBody is the end-to-end form of the same guarantee:
// a legacy record whose body mentions another branch must still resolve the close-out to the
// branch its header names.
func TestResolveFeatureBranchIgnoresBranchLineInLogBody(t *testing.T) {
	repo := setupTestRepo(t)
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	planFile := filepath.Join(plansDir, "20260806-login.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# plan\n"), 0o600))

	progressDir := filepath.Join(repo, ".loopai", "progress")
	require.NoError(t, os.MkdirAll(progressDir, 0o750))
	record := fmt.Sprintf("# Loopai Progress Log\nPlan: %s\nBranch: fix/login\n%s\n\nBranch: master\n",
		planFile, separatorLine())
	require.NoError(t, os.WriteFile(filepath.Join(progressDir, "progress-20260806-login.txt"), []byte(record), 0o600))

	got, err := resolveFeatureBranch(fakeBranchChecker{branches: []string{"master", "login", "fix/login"}},
		[]string{repo}, plansDir, "20260806-login")
	require.NoError(t, err)
	assert.Equal(t, "fix/login", got)
}

// separatorLine mirrors the 60-dash header terminator pkg/progress writes.
func separatorLine() string {
	return strings.Repeat("-", 60)
}

func TestResolveCloseoutBranchAnchorsPlansDirAtRepoRoot(t *testing.T) {
	dir := setupTestRepo(t)
	runGit(t, dir, "branch", "feature")
	writeFeaturePlan(t, dir)
	svc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)
	nested := filepath.Join(dir, "nested")
	require.NoError(t, os.MkdirAll(nested, 0o750))
	t.Chdir(nested)

	target := closeoutTarget{identifier: "20260807-feature", plansDir: filepath.Join("docs", "plans")}
	got, err := resolveCloseoutBranch(svc, target, "--merge")
	require.NoError(t, err)
	assert.Equal(t, "feature", got)
}

func TestValidateCloseoutFlagsFeatureArgument(t *testing.T) {
	tests := []struct {
		name    string
		opts    opts
		wantErr string
	}{
		{name: "merge with feature argument", opts: opts{mergeSet: true, PlanFile: "dynamic-review-agents"}},
		{name: "merge with base and feature argument", opts: opts{Merge: "release/13", PlanFile: "docs/plans/20260807-x.md"}},
		{name: "pr with feature argument", opts: opts{prSet: true, PlanFile: "dynamic-review-agents"}},
		{name: "plan file alone stays a run", opts: opts{PlanFile: "docs/plans/20260807-x.md"}},
		{
			name:    "merge with feature argument and mode flag",
			opts:    opts{mergeSet: true, PlanFile: "feature", Review: true},
			wantErr: "--merge cannot be combined",
		},
		{
			name:    "pr with feature argument and mode flag",
			opts:    opts{prSet: true, PlanFile: "feature", Worktree: true},
			wantErr: "--pr cannot be combined",
		},
		{
			name:    "merge and pr together",
			opts:    opts{mergeSet: true, prSet: true, PlanFile: "feature"},
			wantErr: "--pr cannot be combined",
		},
		{
			name:    "clear with feature argument",
			opts:    opts{Clear: true, PlanFile: "feature"},
			wantErr: "--clear cannot be combined",
		},
		{
			name:    "clear with merge and feature argument",
			opts:    opts{Clear: true, mergeSet: true, PlanFile: "feature"},
			wantErr: "--clear cannot be combined",
		},
		{
			// "--merge release/13 feature" parses release/13 as the feature, so accepting the
			// surplus positional would merge and delete the base branch the caller meant to set
			name:    "merge with surplus positional",
			opts:    opts{mergeSet: true, PlanFile: "release/13", extraArgs: []string{"feature"}},
			wantErr: "--merge accepts at most one feature argument, got 2; use --merge=<base>",
		},
		{
			name:    "pr with surplus positional",
			opts:    opts{prSet: true, PlanFile: "release/13", extraArgs: []string{"feature"}},
			wantErr: "--pr accepts at most one feature argument, got 2; use --pr=<base>",
		},
		{
			name:    "merge with two surplus positionals",
			opts:    opts{mergeSet: true, PlanFile: "a", extraArgs: []string{"b", "c"}},
			wantErr: "got 3",
		},
		{name: "surplus positional without closeout stays a run", opts: opts{PlanFile: "a", extraArgs: []string{"b"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFlags(tt.opts)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// writeFeaturePlan creates a plan file for the "feature" branch in repo's plans directory
// and returns the plans directory.
func writeFeaturePlan(t *testing.T, repo string) string {
	t.Helper()
	plansDir := filepath.Join(repo, "docs", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o750))
	planFile := filepath.Join(plansDir, "20260807-feature.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# plan\n\n## Overview\n\nplan body\n"), 0o600))
	return plansDir
}

func TestRunMergeCommandExplicitFeature(t *testing.T) {
	makeFeature := func(t *testing.T, dir string) *git.Service {
		t.Helper()
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		return svc
	}

	t.Run("plan basename resolves to feature branch", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		plansDir := writeFeaturePlan(t, dir)
		runGit(t, dir, "add", "docs")
		runGit(t, dir, "commit", "-m", "add plan")
		target := closeoutTarget{identifier: "20260807-feature", plansDir: plansDir}

		require.NoError(t, runMergeCommand(t.Context(), svc, "master", target, &recordingStatusClearer{}, io.Discard))
		assert.False(t, branchExists(t, dir, "feature"))
	})

	t.Run("unknown feature reports resolver error", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		plansDir := writeFeaturePlan(t, dir)
		runGit(t, dir, "add", "docs")
		runGit(t, dir, "commit", "-m", "add plan")
		clearer := &recordingStatusClearer{}

		err := runMergeCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "no-such-feature", plansDir: plansDir}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no-such-feature")
		assert.Zero(t, clearer.calls)
		assert.True(t, branchExists(t, dir, "feature"))
	})

	t.Run("merges from primary checkout without a feature worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		runGit(t, dir, "checkout", "master")
		clearer := &recordingStatusClearer{}
		var output bytes.Buffer

		require.NoError(t, runMergeCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "feature"}, clearer, &output))
		assert.Equal(t, "master", currentGitBranch(t, dir))
		assert.False(t, branchExists(t, dir, "feature"))
		assert.FileExists(t, filepath.Join(dir, "feature.txt"))
		assert.Equal(t, 1, clearer.calls)
		assert.Contains(t, output.String(), "feature into master (fast-forward)")
		assert.NotContains(t, output.String(), "worktree", "nothing was removed, so name no worktree")
	})

	t.Run("merges from primary checkout with a base merge commit", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600))
		runGit(t, dir, "add", "base.txt")
		runGit(t, dir, "commit", "-m", "advance base")
		var output bytes.Buffer

		require.NoError(t, runMergeCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "feature"}, &recordingStatusClearer{}, &output))
		assert.Equal(t, "master", currentGitBranch(t, dir))
		assert.False(t, branchExists(t, dir, "feature"))
		assert.Contains(t, output.String(), "feature into master (merge commit)")
		assert.Equal(t, "feature\n", gitOutput(t, dir, "show", "master:feature.txt"))
	})

	t.Run("merges from an unrelated worktree without a feature worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "checkout", "master")
		runGit(t, dir, "branch", "sidebar", "master")
		sidePath := filepath.Join(t.TempDir(), "sidebar")
		runGit(t, dir, "worktree", "add", sidePath, "sidebar")
		t.Cleanup(func() { _ = mainSvc.RemoveWorktree(sidePath) })
		sideSvc, err := git.NewService(sidePath, noopLogger())
		require.NoError(t, err)

		require.NoError(t, runMergeCommand(t.Context(), sideSvc, "master",
			closeoutTarget{identifier: "feature"}, &recordingStatusClearer{}, io.Discard))
		assert.False(t, branchExists(t, dir, "feature"))
		assert.Equal(t, "feature\n", gitOutput(t, dir, "show", "master:feature.txt"))
		assert.Equal(t, "master", currentGitBranch(t, dir))
		assert.Equal(t, "sidebar", currentGitBranch(t, sidePath))
	})

	t.Run("removes a registered feature worktree", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "checkout", "master")
		worktreePath := filepath.Join(dir, ".loopai", "worktrees", "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		clearer := &recordingStatusClearer{}
		var output bytes.Buffer

		require.NoError(t, runMergeCommand(t.Context(), mainSvc, "master",
			closeoutTarget{identifier: "feature"}, clearer, &output))
		assert.NoDirExists(t, worktreePath)
		assert.False(t, branchExists(t, dir, "feature"))
		assert.FileExists(t, filepath.Join(dir, "feature.txt"))
		assert.Equal(t, 1, clearer.calls)
		// with an explicit feature the removed directory is resolved from the worktree list rather
		// than being the caller's own, so name it: removal takes ignored files with it. the printed
		// path is Git's, which resolves symlinks such as macOS /var -> /private/var
		assert.Contains(t, output.String(), "deleted branch feature and worktree ")
		assert.Contains(t, output.String(), filepath.Join(".loopai", "worktrees", "feature"))
	})

	t.Run("stale feature worktree registration reports prune guidance", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "checkout", "master")
		worktreePath := filepath.Join(dir, ".loopai", "worktrees", "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		// the registration survives a hand-deleted directory until it is pruned
		require.NoError(t, os.RemoveAll(worktreePath))
		t.Cleanup(func() { runGit(t, dir, "worktree", "prune") })
		clearer := &recordingStatusClearer{}

		err := runMergeCommand(t.Context(), mainSvc, "master",
			closeoutTarget{identifier: "feature"}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "git worktree prune")
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("dirty feature worktree is refused", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "checkout", "master")
		worktreePath := filepath.Join(dir, ".loopai", "worktrees", "feature")
		runGit(t, dir, "worktree", "add", worktreePath, "feature")
		t.Cleanup(func() { _ = mainSvc.RemoveWorktree(worktreePath) })
		require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "feature.txt"), []byte("dirty\n"), 0o600))
		clearer := &recordingStatusClearer{}

		err := runMergeCommand(t.Context(), mainSvc, "master",
			closeoutTarget{identifier: "feature"}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clean feature worktree")
		assert.True(t, branchExists(t, dir, "feature"))
		assert.DirExists(t, worktreePath)
		assert.Zero(t, clearer.calls)
	})

	t.Run("dirty primary checkout holding the feature is refused", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "branch", "sidebar", "master")
		sidePath := filepath.Join(t.TempDir(), "sidebar")
		runGit(t, dir, "worktree", "add", sidePath, "sidebar")
		t.Cleanup(func() { _ = mainSvc.RemoveWorktree(sidePath) })
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("dirty\n"), 0o600))
		sideSvc, err := git.NewService(sidePath, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), sideSvc, "master",
			closeoutTarget{identifier: "feature"}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clean feature worktree")
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Equal(t, "feature", currentGitBranch(t, dir))
		assert.Zero(t, clearer.calls)
	})

	t.Run("feature resolving to the base branch is refused", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		runGit(t, dir, "checkout", "master")
		clearer := &recordingStatusClearer{}

		err := runMergeCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "master"}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already the base branch")
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("conflict aborts and keeps the feature branch", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("feature\n"), 0o600))
		runGit(t, dir, "commit", "-am", "feature change")
		runGit(t, dir, "checkout", "master")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o600))
		runGit(t, dir, "commit", "-am", "base change")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "feature"}, clearer, io.Discard)
		require.ErrorIs(t, err, git.ErrMergeConflict)
		assert.Contains(t, err.Error(), "conflicted and was aborted")
		assert.Equal(t, "master", currentGitBranch(t, dir))
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "status", "--porcelain")))
	})

	t.Run("explicit base combines with an explicit feature", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "checkout", "-b", "release/13")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "release.txt"), []byte("release\n"), 0o600))
		runGit(t, dir, "add", "release.txt")
		runGit(t, dir, "commit", "-m", "release")
		makeFeature(t, dir)
		runGit(t, dir, "checkout", "master")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		require.NoError(t, runMergeCommand(t.Context(), svc, "release/13",
			closeoutTarget{identifier: "feature"}, &recordingStatusClearer{}, io.Discard))
		assert.False(t, branchExists(t, dir, "feature"))
		assert.Equal(t, "feature\n", gitOutput(t, dir, "show", "release/13:feature.txt"))
		assert.Equal(t, "master", currentGitBranch(t, dir), "the invoking checkout must be restored")
		assert.Empty(t, strings.TrimSpace(gitOutput(t, dir, "ls-tree", "--name-only", "master", "feature.txt")),
			"master must not receive the merge when an explicit base is given")
	})

	t.Run("dirty unrelated worktree does not block the merge", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		runGit(t, dir, "checkout", "master")
		runGit(t, dir, "branch", "sidebar", "master")
		sidePath := filepath.Join(t.TempDir(), "sidebar")
		runGit(t, dir, "worktree", "add", sidePath, "sidebar")
		t.Cleanup(func() { _ = mainSvc.RemoveWorktree(sidePath) })
		require.NoError(t, os.WriteFile(filepath.Join(sidePath, "scratch.txt"), []byte("wip\n"), 0o600))
		sideSvc, err := git.NewService(sidePath, noopLogger())
		require.NoError(t, err)

		require.NoError(t, runMergeCommand(t.Context(), sideSvc, "master",
			closeoutTarget{identifier: "feature"}, &recordingStatusClearer{}, io.Discard))
		assert.False(t, branchExists(t, dir, "feature"))
		assert.Equal(t, "feature\n", gitOutput(t, dir, "show", "master:feature.txt"))
		assert.FileExists(t, filepath.Join(sidePath, "scratch.txt"))
	})

	t.Run("dirty invoking checkout holding the feature is refused", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc := makeFeature(t, dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o600))
		clearer := &recordingStatusClearer{}

		err := runMergeCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "feature"}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clean feature worktree")
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("base checked out in another worktree is refused", func(t *testing.T) {
		dir := setupTestRepo(t)
		mainSvc := makeFeature(t, dir)
		require.NoError(t, mainSvc.EnsureLocalGitignore())
		basePath := filepath.Join(t.TempDir(), "base")
		runGit(t, dir, "worktree", "add", basePath, "master")
		t.Cleanup(func() { _ = mainSvc.RemoveWorktree(basePath) })
		baseSvc, err := git.NewService(basePath, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), baseSvc, "master",
			closeoutTarget{identifier: "feature"}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `is checked out at`)
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Equal(t, "feature", currentGitBranch(t, dir))
		assert.Zero(t, clearer.calls)
	})

	t.Run("detached HEAD without an identifier names the flag", func(t *testing.T) {
		dir := setupTestRepo(t)
		makeFeature(t, dir)
		runGit(t, dir, "checkout", "--detach", "master")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		clearer := &recordingStatusClearer{}

		err = runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--merge requires a checked-out feature branch")
		assert.True(t, branchExists(t, dir, "feature"))
		assert.Zero(t, clearer.calls)
	})

	t.Run("detached primary checkout merges an explicit feature", func(t *testing.T) {
		dir := setupTestRepo(t)
		makeFeature(t, dir)
		runGit(t, dir, "checkout", "--detach", "master")
		originalHead := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD"))
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)

		require.NoError(t, runMergeCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "feature"}, &recordingStatusClearer{}, io.Discard))
		assert.False(t, branchExists(t, dir, "feature"))
		assert.Equal(t, "feature\n", gitOutput(t, dir, "show", "master:feature.txt"))
		assert.Empty(t, currentGitBranch(t, dir))
		assert.Equal(t, originalHead, strings.TrimSpace(gitOutput(t, dir, "rev-parse", "HEAD")))
	})
}

func TestRunPRCommandExplicitFeatureResolverError(t *testing.T) {
	dir := setupTestRepo(t)
	plansDir := writeFeaturePlan(t, dir)
	svc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	clearer := &recordingStatusClearer{}

	err = runPRCommand(t.Context(), svc, "master",
		closeoutTarget{identifier: "no-such-feature", plansDir: plansDir}, clearer, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-feature")
	assert.Zero(t, clearer.calls)
}

func TestRunPRCommandDetachedHeadWithoutIdentifier(t *testing.T) {
	dir := setupTestRepo(t)
	runGit(t, dir, "checkout", "--detach", "master")
	svc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	clearer := &recordingStatusClearer{}

	err = runPRCommand(t.Context(), svc, "master", closeoutTarget{}, clearer, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--pr requires a checked-out feature branch")
	assert.Zero(t, clearer.calls)
}

func TestRunPRCommandExplicitFeature(t *testing.T) {
	// setupBaseCheckout builds a repo whose primary checkout stays on master while the feature
	// branch carries the commits, mirroring a close-out started from the main checkout.
	setupBaseCheckout := func(t *testing.T) (dir, remote string, svc *git.Service) {
		t.Helper()
		dir = setupTestRepo(t)
		remote = filepath.Join(t.TempDir(), "origin.git")
		runGit(t, filepath.Dir(remote), "init", "--bare", remote)
		runGit(t, dir, "remote", "add", "origin", "https://github.com/acme/repo.git")
		runGit(t, dir, "checkout", "-b", "feature")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("one\ntwo\n"), 0o600))
		runGit(t, dir, "add", "feature.txt")
		runGit(t, dir, "commit", "-m", "feature")
		runGit(t, dir, "checkout", "master")

		plansDir := filepath.Join(dir, "docs", "plans")
		require.NoError(t, os.MkdirAll(plansDir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(plansDir, "20260802-feature.md"),
			[]byte("# Feature PR\n\n## Overview\n\nImplements feature.\n"), 0o600))

		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		t.Setenv("PR_TEST_REAL_GIT", realGit)
		t.Setenv("PR_TEST_REMOTE", remote)
		gitWrapper := filepath.Join(t.TempDir(), "git-wrapper")
		writeExecutable(t, gitWrapper, "#!/bin/sh\nif [ \"$1\" = push ]; then\n  refspec=$4\n  \"$PR_TEST_REAL_GIT\" push \"$PR_TEST_REMOTE\" \"$refspec\" || exit $?\n  exit 0\nfi\nexec \"$PR_TEST_REAL_GIT\" \"$@\"\n")
		svc, err = git.NewService(dir, noopLogger(), gitWrapper)
		require.NoError(t, err)
		return dir, remote, svc
	}

	stubGh := func(t *testing.T) (argsLog, bodyLog string) {
		t.Helper()
		binDir := t.TempDir()
		argsLog = filepath.Join(binDir, "gh-args.log")
		bodyLog = filepath.Join(binDir, "gh-body.log")
		writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nif [ \"$1\" = repo ]; then\n  printf '%s\\n' 'acme/repo'\n  exit 0\nfi\nprintf '%s\\n' \"$@\" > \"$GH_ARGS_LOG\"\ncat > \"$GH_BODY_LOG\"\nprintf '%s\\n' 'https://github.com/acme/repo/pull/7'\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("GH_ARGS_LOG", argsLog)
		t.Setenv("GH_BODY_LOG", bodyLog)
		return argsLog, bodyLog
	}

	t.Run("branch name pushes and creates the PR without checking it out", func(t *testing.T) {
		dir, remote, svc := setupBaseCheckout(t)
		argsLog, bodyLog := stubGh(t)
		clearer := &recordingStatusClearer{}
		var output bytes.Buffer

		require.NoError(t, runPRCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "feature", plansDir: filepath.Join(dir, "docs", "plans")}, clearer, &output))
		assert.Equal(t, "https://github.com/acme/repo/pull/7\n", output.String())
		assert.Equal(t, 1, clearer.calls)
		assert.Equal(t, "master", currentGitBranch(t, dir), "the primary checkout must stay on the base branch")
		assert.True(t, branchExists(t, dir, "feature"))

		args, err := os.ReadFile(argsLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Contains(t, string(args), "--base\nmaster\n--head\nfeature\n--title\nFeature PR\n")
		body, err := os.ReadFile(bodyLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Contains(t, string(body), "Implements feature.")
		assert.Contains(t, string(body), "- Files changed: 1", "diff stats must describe the named branch, not HEAD")
		assert.Contains(t, string(body), "- Additions: 2")

		localHead := strings.TrimSpace(gitOutput(t, dir, "rev-parse", "feature"))
		assert.Equal(t, localHead, strings.TrimSpace(gitOutput(t, remote, "rev-parse", "refs/heads/feature")))
	})

	t.Run("plan basename resolves to the feature branch", func(t *testing.T) {
		dir, _, svc := setupBaseCheckout(t)
		argsLog, _ := stubGh(t)

		require.NoError(t, runPRCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "20260802-feature", plansDir: filepath.Join(dir, "docs", "plans")},
			&recordingStatusClearer{}, io.Discard))
		args, err := os.ReadFile(argsLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Contains(t, string(args), "--head\nfeature\n")
	})

	t.Run("plan path resolves to the feature branch", func(t *testing.T) {
		dir, _, svc := setupBaseCheckout(t)
		argsLog, _ := stubGh(t)
		plansDir := filepath.Join(dir, "docs", "plans")

		require.NoError(t, runPRCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: filepath.Join(plansDir, "20260802-feature.md"), plansDir: plansDir},
			&recordingStatusClearer{}, io.Discard))
		args, err := os.ReadFile(argsLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Contains(t, string(args), "--head\nfeature\n")
	})

	t.Run("feature resolving to the base branch is refused", func(t *testing.T) {
		dir, _, svc := setupBaseCheckout(t)
		stubGh(t)
		clearer := &recordingStatusClearer{}

		err := runPRCommand(t.Context(), svc, "master",
			closeoutTarget{identifier: "master", plansDir: filepath.Join(dir, "docs", "plans")}, clearer, io.Discard)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already the base branch")
		assert.Contains(t, err.Error(), "name a different feature")
		assert.Zero(t, clearer.calls)
	})
}

func TestRunCloseoutCommandRoutesPositionalFeature(t *testing.T) {
	dir := setupTestRepo(t)
	plansDir := writeFeaturePlan(t, dir)
	runGit(t, dir, "add", "docs")
	runGit(t, dir, "commit", "-m", "add plan")
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "gh"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(dir)
	cfg := &config.Config{PlansDir: plansDir}

	t.Run("merge", func(t *testing.T) {
		err := runCloseoutCommand(t.Context(), opts{mergeSet: true, PlanFile: "no-such-feature"}, cfg, testColors())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no-such-feature")
	})

	t.Run("pr", func(t *testing.T) {
		err := runCloseoutCommand(t.Context(), opts{prSet: true, PlanFile: "no-such-feature"}, cfg, testColors())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no-such-feature")
	})
}

func TestStripCmuxWorkspaceArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "no args", args: nil, want: []string{}},
		{name: "flag absent", args: []string{"plan.md", "--worktree"}, want: []string{"plan.md", "--worktree"}},
		{name: "bare flag", args: []string{"--cmux-workspace"}, want: []string{}},
		{name: "value form", args: []string{"--cmux-workspace=true", "plan.md"}, want: []string{"plan.md"}},
		{name: "repeated", args: []string{"--cmux-workspace", "--cmux-workspace=false"}, want: []string{}},
		{
			name: "mixed among other args",
			args: []string{"-m", "3", "--cmux-workspace", "docs/plans/p.md", "--worktree"},
			want: []string{"-m", "3", "docs/plans/p.md", "--worktree"},
		},
		{
			name: "similar flag kept",
			args: []string{"--cmux-workspace-name", "x", "--cmux-workspace"},
			want: []string{"--cmux-workspace-name", "x"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripCmuxWorkspaceArg(tc.args))
		})
	}
}

func TestCmuxWorkspaceName(t *testing.T) {
	tests := []struct {
		name string
		o    opts
		want string
	}{
		{name: "no plan file", o: opts{}, want: "loopai"},
		{name: "plan file", o: opts{PlanFile: "docs/plans/20260807-my-feature.md"}, want: "my-feature"},
		{name: "plan file without date", o: opts{PlanFile: "docs/plans/feature.md"}, want: "feature"},
		{name: "branch override wins", o: opts{PlanFile: "docs/plans/p.md", Branch: "custom"}, want: "custom"},
		{name: "blank branch override ignored", o: opts{PlanFile: "p.md", Branch: "  "}, want: "p"},
		// a plan path whose base is nothing but the extension derives an empty branch, which would
		// title the sidebar card with nothing at all.
		{name: "empty derived name falls back", o: opts{PlanFile: "docs/plans/.md"}, want: "loopai"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cmuxWorkspaceName(tc.o))
		})
	}
}

// clearCmuxEnvOptions removes the environment-provided options for the duration of the test, so
// hand-off argv assertions do not depend on the environment the suite happens to run in.
func clearCmuxEnvOptions(t *testing.T) {
	t.Helper()
	for _, key := range cmuxEnvOptions {
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() { require.NoError(t, os.Setenv(key, value)) })
	}
}

// cmuxSpawnStub installs a cmux binary in PATH recording every argument on its own line and
// returns the log path. exitCode lets a test make the spawn fail.
func cmuxSpawnStub(t *testing.T, exitCode int) string {
	t.Helper()
	clearCmuxEnvOptions(t)
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "cmux-argv.log")
	writeExecutable(t, filepath.Join(binDir, "cmux"),
		fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> \"$CMUX_ARGV_LOG\"; done\nexit %d\n", exitCode))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CMUX_ARGV_LOG", argvLog)
	return argvLog
}

func TestHandOffToCmuxWorkspace(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)

	t.Run("hands off inside cmux", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
		dir := t.TempDir()
		t.Chdir(dir)
		cwd, wdErr := os.Getwd()
		require.NoError(t, wdErr)

		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		o := opts{CmuxWorkspace: true, PlanFile: "docs/plans/20260807-my feature.md"}
		args := []string{"--cmux-workspace", "docs/plans/20260807-my feature.md", "--worktree"}
		require.True(t, handOffToCmuxWorkspace(o, args, stdout, stderr))

		assert.Contains(t, stdout.String(), "handed off to cmux workspace my feature")
		assert.Empty(t, stderr.String())
		recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readErr)
		assert.Equal(t, []string{
			"new-workspace", "--name", "my feature", "--cwd", cwd, "--focus", "true", "--command",
			fmt.Sprintf("'%s' 'docs/plans/20260807-my feature.md' '--worktree'", exe),
		}, strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n"))
	})

	t.Run("outside cmux continues locally", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "")

		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		assert.False(t, handOffToCmuxWorkspace(opts{CmuxWorkspace: true}, nil, stdout, stderr))
		assert.Empty(t, stdout.String())
		assert.Contains(t, stderr.String(), "hand-off failed, running here")
		assert.Contains(t, stderr.String(), "not running inside cmux")
		_, statErr := os.Stat(argvLog)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("spawn failure continues locally", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 1)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		assert.False(t, handOffToCmuxWorkspace(opts{CmuxWorkspace: true, PlanFile: "p.md"}, nil, stdout, stderr))
		assert.Empty(t, stdout.String())
		assert.Contains(t, stderr.String(), "hand-off failed, running here")
		recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readErr)
		assert.Contains(t, string(recorded), "new-workspace")
	})

	t.Run("flag unset is a no-op", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		assert.False(t, handOffToCmuxWorkspace(opts{PlanFile: "p.md"}, nil, stdout, stderr))
		assert.Empty(t, stdout.String())
		assert.Empty(t, stderr.String())
		_, statErr := os.Stat(argvLog)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("unresolvable working directory continues locally", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

		// the new workspace needs a --cwd, so a working directory that no longer exists is the same
		// kind of unusable environment as a missing executable: warn and run here instead.
		dir := filepath.Join(t.TempDir(), "gone")
		require.NoError(t, os.Mkdir(dir, 0o750))
		t.Chdir(dir)
		// removing the current directory is refused on some platforms and left resolvable on others,
		// so the branch is only exercised where the environment actually becomes unusable.
		if err := os.Remove(dir); err != nil {
			t.Skipf("platform keeps the working directory: %v", err)
		}
		if _, err := os.Getwd(); err == nil {
			t.Skip("platform resolves the working directory of a removed directory")
		}

		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		assert.False(t, handOffToCmuxWorkspace(opts{CmuxWorkspace: true, PlanFile: "p.md"}, nil, stdout, stderr))
		assert.Empty(t, stdout.String())
		assert.Contains(t, stderr.String(), "hand-off skipped, running here: resolve working directory")
		_, statErr := os.Stat(argvLog)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("environment-provided options travel with the command", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
		t.Setenv("LOOPAI_CONFIG_DIR", "/custom/config dir")

		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		o := opts{CmuxWorkspace: true, PlanFile: "p.md"}
		require.True(t, handOffToCmuxWorkspace(o, []string{"p.md"}, stdout, stderr))

		assert.Empty(t, stderr.String())
		recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readErr)
		lines := strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n")
		assert.Equal(t, fmt.Sprintf("'env' 'LOOPAI_CONFIG_DIR=/custom/config dir' '%s' 'p.md'", exe), lines[len(lines)-1],
			"the new workspace shell does not inherit this process's environment")
	})

	t.Run("reset preceding a run is handed off", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

		// --reset alone is a standalone command, but combined with a run it is part of that run and
		// must happen once, in the workspace the run lands in.
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		o := opts{CmuxWorkspace: true, Reset: true, PlanFile: "docs/plans/p.md"}
		require.True(t, handOffToCmuxWorkspace(o, []string{"--reset", "docs/plans/p.md"}, stdout, stderr))

		assert.Empty(t, stderr.String())
		recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readErr)
		assert.Contains(t, string(recorded), "--reset")
	})

	t.Run("standalone commands are not handed off", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		for _, o := range []opts{
			{CmuxWorkspace: true, mergeSet: true},
			{CmuxWorkspace: true, prSet: true},
			{CmuxWorkspace: true, Clear: true},
			{CmuxWorkspace: true, Init: true},
			{CmuxWorkspace: true, DumpDefaults: t.TempDir()},
			{CmuxWorkspace: true, Reset: true},
		} {
			assert.False(t, handOffToCmuxWorkspace(o, nil, stdout, stderr))
		}
		assert.Empty(t, stdout.String())
		assert.Empty(t, stderr.String())
		_, statErr := os.Stat(argvLog)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})
}

func TestRunHandsOffBeforeConfigLoad(t *testing.T) {
	badConfigDir := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(badConfigDir, []byte("x"), 0o600))

	t.Run("successful hand-off exits before config load", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

		require.NoError(t, run(t.Context(), opts{CmuxWorkspace: true, ConfigDir: badConfigDir, PlanFile: "p.md"}))
		recorded, err := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Contains(t, string(recorded), "new-workspace")
		assert.Contains(t, string(recorded), "\np\n") // workspace named after the plan branch
		assert.NotContains(t, string(recorded), "clear-status",
			"the run happens in the new workspace, so this one's completion pill stays")
	})

	t.Run("interactive plan creation is handed off too", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

		o := opts{CmuxWorkspace: true, ConfigDir: badConfigDir, PlanDescription: "add a feature"}
		require.NoError(t, run(t.Context(), o))
		recorded, err := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Contains(t, string(recorded), "new-workspace")
		assert.Contains(t, string(recorded), "\nloopai\n") // no plan file yet, falls back to the app name
	})

	t.Run("failed hand-off continues the normal run", func(t *testing.T) {
		argvLog := cmuxSpawnStub(t, 1)
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")

		err := run(t.Context(), opts{CmuxWorkspace: true, ConfigDir: badConfigDir, PlanFile: "p.md"})
		require.ErrorContains(t, err, "load config")
		recorded, readErr := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, readErr)
		assert.Contains(t, string(recorded), "clear-status",
			"the run stayed here after all, so it takes over the stale pill even though startup failed")
	})

	t.Run("outside cmux continues the normal run", func(t *testing.T) {
		cmuxSpawnStub(t, 0)
		t.Setenv("CMUX_WORKSPACE_ID", "")

		err := run(t.Context(), opts{CmuxWorkspace: true, ConfigDir: badConfigDir, PlanFile: "p.md"})
		require.ErrorContains(t, err, "load config")
	})
}

func TestCmuxEnvOptionsCoversOptionTags(t *testing.T) {
	var tagged []string
	for field := range reflect.TypeFor[opts]().Fields() {
		if key := field.Tag.Get("env"); key != "" {
			tagged = append(tagged, key)
		}
	}

	require.NotEmpty(t, tagged, "the reflection walk must find the env-backed options")
	assert.ElementsMatch(t, tagged, cmuxEnvOptions,
		"an option readable from the environment is lost on hand-off unless cmuxEnvOptions lists it")
}

func TestCmuxHandOffArgv(t *testing.T) {
	t.Run("no environment options", func(t *testing.T) {
		clearCmuxEnvOptions(t)
		argv := cmuxHandOffArgv("/bin/loopai", []string{"--cmux-workspace", "p.md"})
		assert.Equal(t, []string{"/bin/loopai", "p.md"}, argv)
	})

	t.Run("empty value is still forwarded", func(t *testing.T) {
		clearCmuxEnvOptions(t)
		t.Setenv("LOOPAI_CONFIG_DIR", "")
		argv := cmuxHandOffArgv("/bin/loopai", nil)
		assert.Equal(t, []string{"env", "LOOPAI_CONFIG_DIR=", "/bin/loopai"}, argv,
			"an explicitly empty value means something different than an unset one")
	})

	t.Run("every environment option is forwarded", func(t *testing.T) {
		clearCmuxEnvOptions(t)
		t.Setenv("LOOPAI_CONFIG_DIR", "/cfg")
		t.Setenv("LOOPAI_WEB_HOST", "0.0.0.0")
		argv := cmuxHandOffArgv("/bin/loopai", []string{"--serve"})
		assert.Equal(t, []string{"env", "LOOPAI_CONFIG_DIR=/cfg", "LOOPAI_WEB_HOST=0.0.0.0", "/bin/loopai", "--serve"}, argv)
	})
}

func TestHandOffSucceeded(t *testing.T) {
	tests := []struct {
		name     string
		o        opts
		earlyErr error
		want     bool
	}{
		{name: "hand-off with no error", o: opts{CmuxWorkspace: true}, want: true},
		{name: "hand-off flag with an error", o: opts{CmuxWorkspace: true}, earlyErr: errors.New("reset failed")},
		{name: "flag unset", o: opts{}},
		{name: "flag unset with an error", o: opts{}, earlyErr: errors.New("init failed")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, handOffSucceeded(tc.o, tc.earlyErr))
		})
	}
}

func TestPrepareStaleCmuxStatusDefersForHandOff(t *testing.T) {
	// cmux stub shared by the subtests, each gets its own log through CMUX_ARGV_LOG.
	stub := func(t *testing.T) string {
		t.Helper()
		binDir := t.TempDir()
		argvLog := filepath.Join(binDir, "cmux-argv.log")
		writeExecutable(t, filepath.Join(binDir, "cmux"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CMUX_ARGV_LOG\"\n")
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("CMUX_WORKSPACE_ID", "ws-1")
		t.Setenv("CMUX_ARGV_LOG", argvLog)
		return argvLog
	}

	t.Run("without the flag the clear is immediate", func(t *testing.T) {
		argvLog := stub(t)
		prepareStaleCmuxStatus(opts{PlanFile: "plan.md"})
		recorded, err := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Equal(t, "clear-status loopai\n", string(recorded))
	})

	t.Run("preserved hand-off never clears", func(t *testing.T) {
		argvLog := stub(t)
		resolve := prepareStaleCmuxStatus(opts{CmuxWorkspace: true, PlanFile: "plan.md"})
		_, err := os.Stat(argvLog)
		require.ErrorIs(t, err, os.ErrNotExist, "the hand-off verdict is not known yet")

		resolve(true)
		_, err = os.Stat(argvLog)
		require.ErrorIs(t, err, os.ErrNotExist, "the run moved to another workspace")
	})

	t.Run("failed hand-off clears on resolution", func(t *testing.T) {
		argvLog := stub(t)
		resolve := prepareStaleCmuxStatus(opts{CmuxWorkspace: true, PlanFile: "plan.md"})
		resolve(false)
		recorded, err := os.ReadFile(argvLog) //nolint:gosec // path built from t.TempDir
		require.NoError(t, err)
		assert.Equal(t, "clear-status loopai\n", string(recorded))
	})
}
