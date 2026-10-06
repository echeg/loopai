package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/ralphex/pkg/config"
)

func TestLaunchHistoryPath(t *testing.T) {
	t.Run("explicit config dir", func(t *testing.T) {
		dir := t.TempDir()
		assert.Equal(t, filepath.Join(dir, "launch-history"), launchHistoryPath(dir))
	})
	t.Run("default config dir", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		assert.Equal(t, filepath.Join(config.DefaultConfigDir(), "launch-history"), launchHistoryPath(""))
		assert.Equal(t, filepath.Join(home, ".config", "loopai", "launch-history"), launchHistoryPath(""))
	})
}

func TestReadLaunchHistory(t *testing.T) {
	t1 := time.Date(2026, 10, 6, 12, 34, 56, 0, time.UTC)
	t2 := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)

	tests := []struct {
		name    string
		missing bool
		content string
		want    []launchEntry
	}{
		{name: "missing file", missing: true, want: nil},
		{name: "empty file", content: "", want: nil},
		{
			name: "order preserved",
			content: "2026-10-06T12:34:56Z\tt3\t--task-model claude:opus:high\n" +
				"2026-10-05T08:00:00Z\torca\t--task-model codex:gpt-6-astra --external-reviewers claude:opus\n" +
				"2026-10-04T09:30:00Z\tcli\t\n",
			want: []launchEntry{
				{When: t1, Launcher: launcherT3, Flags: "--task-model claude:opus:high"},
				{When: t2, Launcher: launcherOrca, Flags: "--task-model codex:gpt-6-astra --external-reviewers claude:opus"},
				{When: t3, Launcher: launcherCLI, Flags: ""},
			},
		},
		{
			name: "blank malformed and unknown lines skipped",
			content: "\n   \n" +
				"not a history line\n" +
				"2026-10-06T12:34:56Z\tt3\n" +
				"yesterday\tcli\t--task-model claude:opus\n" +
				"2026-10-06T12:34:56Z\tvscode\t--task-model claude:opus\n" +
				"2026-10-06T12:34:56Z\tT3\t--task-model claude:opus\n" +
				"2026-10-05T08:00:00Z\torca\t--task-model claude:sonnet\r\n",
			want: []launchEntry{{When: t2, Launcher: launcherOrca, Flags: "--task-model claude:sonnet"}},
		},
		{
			name:    "non-UTC timestamp normalized",
			content: "2026-10-06T14:34:56+02:00\tcli\t--task-model claude:opus\n",
			want:    []launchEntry{{When: t1, Launcher: launcherCLI, Flags: "--task-model claude:opus"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "launch-history")
			if !tc.missing {
				require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			}
			assert.Equal(t, tc.want, readLaunchHistory(path))
		})
	}
}

func TestRecordLaunchCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "loopai", "launch-history")
	when := time.Date(2026, 10, 6, 12, 34, 56, 0, time.FixedZone("x", 3600))

	require.NoError(t, recordLaunch(path, launchEntry{When: when, Launcher: launcherT3, Flags: "--task-model claude:opus:high"}))

	got, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "2026-10-06T11:34:56Z\tt3\t--task-model claude:opus:high\n", string(got))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	assertNoTempFiles(t, filepath.Dir(path), ".launch-history-*.tmp")
}

func TestRecordLaunchEmptyFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-history")
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	require.NoError(t, recordLaunch(path, launchEntry{When: base, Launcher: launcherCLI, Flags: "--task-model claude:opus"}))
	require.NoError(t, recordLaunch(path, launchEntry{When: base.Add(time.Minute), Launcher: launcherOrca, Flags: ""}))

	assert.Equal(t, []launchEntry{
		{When: base.Add(time.Minute), Launcher: launcherOrca, Flags: ""},
		{When: base, Launcher: launcherCLI, Flags: "--task-model claude:opus"},
	}, readLaunchHistory(path))
}

func TestRecordLaunchDedupKeepsNewerLauncher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-history")
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	require.NoError(t, recordLaunch(path, launchEntry{When: base, Launcher: launcherOrca, Flags: "--task-model claude:opus"}))
	require.NoError(t, recordLaunch(path, launchEntry{When: base.Add(time.Minute), Launcher: launcherCLI, Flags: "--task-model codex:gpt-6-astra"}))
	require.NoError(t, recordLaunch(path, launchEntry{When: base.Add(2 * time.Minute), Launcher: launcherT3, Flags: "--task-model claude:opus"}))

	assert.Equal(t, []launchEntry{
		{When: base.Add(2 * time.Minute), Launcher: launcherT3, Flags: "--task-model claude:opus"},
		{When: base.Add(time.Minute), Launcher: launcherCLI, Flags: "--task-model codex:gpt-6-astra"},
	}, readLaunchHistory(path))
}

func TestRecordLaunchCapsAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-history")
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	for i := range launchHistoryLimit + 3 {
		entry := launchEntry{When: base.Add(time.Duration(i) * time.Minute), Launcher: launcherCLI, Flags: fmt.Sprintf("--task-model claude:m%d", i)}
		require.NoError(t, recordLaunch(path, entry))
	}

	got := readLaunchHistory(path)
	require.Len(t, got, launchHistoryLimit)
	assert.Equal(t, fmt.Sprintf("--task-model claude:m%d", launchHistoryLimit+2), got[0].Flags)
	assert.Equal(t, "--task-model claude:m3", got[launchHistoryLimit-1].Flags)
	content, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, launchHistoryLimit, strings.Count(string(content), "\n"))
}

func TestRecordLaunchDropsUnreadableLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-history")
	require.NoError(t, os.WriteFile(path, []byte("garbage\n2026-10-05T08:00:00Z\tcli\t--task-model claude:opus\n"), 0o600))

	when := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	require.NoError(t, recordLaunch(path, launchEntry{When: when, Launcher: launcherT3, Flags: ""}))

	got, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "2026-10-06T12:00:00Z\tt3\t\n2026-10-05T08:00:00Z\tcli\t--task-model claude:opus\n", string(got))
}

func TestRecordLaunchErrors(t *testing.T) {
	entry := launchEntry{When: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), Launcher: launcherCLI, Flags: "--task-model claude:opus"}

	t.Run("unwritable directory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("directory permissions are not enforced on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := filepath.Join(t.TempDir(), "readonly")
		require.NoError(t, os.Mkdir(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restore permissions for cleanup

		err := recordLaunch(filepath.Join(dir, "launch-history"), entry)
		require.ErrorContains(t, err, "launch history")
		assert.NoFileExists(t, filepath.Join(dir, "launch-history"))
		assertNoTempFiles(t, dir, ".launch-history-*.tmp")
	})

	t.Run("replace fails", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "launch-history")
		require.NoError(t, os.Mkdir(path, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600))

		err := recordLaunch(path, entry)
		require.ErrorContains(t, err, "replace launch history")
		assertNoTempFiles(t, dir, ".launch-history-*.tmp")
	})
}

func TestLaunchFlags(t *testing.T) {
	codexChain := externalReviewSelection{Resolved: true, Explicit: true, Reviewers: []resolvedReviewer{
		{Provider: config.ExternalReviewToolClaude, Model: "opus", Effort: "high"},
		{Provider: config.ExternalReviewToolCodex, Model: "gpt-6-astra", Effort: "high"},
	}}
	tests := []struct {
		name string
		o    opts
		cfg  *config.Config
		sel  externalReviewSelection
		want string
	}{
		{name: "nil config", o: opts{TaskModel: "claude:opus"}, want: ""},
		{name: "no flags set", cfg: &config.Config{}, want: ""},
		{name: "task only", o: opts{TaskModel: "codex:gpt-5.6-sol:high"}, cfg: &config.Config{},
			want: "--task-model codex:gpt-5.6-sol:high"},
		{name: "task from config", cfg: &config.Config{TaskModel: "claude:opus:high"},
			want: "--task-model claude:opus:high"},
		{name: "explicit review equal to task", o: opts{TaskModel: "claude:opus:high", ReviewModel: "claude:opus:high"},
			cfg: &config.Config{}, want: "--task-model claude:opus:high --review-model claude:opus:high"},
		{name: "explicit review differing", o: opts{TaskModel: "codex:gpt-5.6-sol:high"},
			cfg:  &config.Config{ReviewModel: "claude:opus:high"},
			want: "--task-model codex:gpt-5.6-sol:high --review-model claude:opus:high"},
		{name: "inherited review omitted", o: opts{TaskModel: "codex:gpt-5.6-sol"}, cfg: &config.Config{},
			want: "--task-model codex:gpt-5.6-sol"},
		{name: "review only", o: opts{ReviewModel: "claude::max"}, cfg: &config.Config{},
			want: "--review-model claude::max"},
		{name: "explicit chain with efforts", o: opts{TaskModel: "claude:fable"}, cfg: &config.Config{}, sel: codexChain,
			want: "--task-model claude:fable --external-reviewers claude:opus:high,codex:gpt-6-astra:high"},
		{name: "explicit chain without models", cfg: &config.Config{},
			sel: externalReviewSelection{Explicit: true, Reviewers: []resolvedReviewer{
				{Provider: config.ExternalReviewToolCodex},
				{Provider: config.ExternalReviewToolCodex, Effort: "medium"},
				{Provider: config.ExternalReviewToolClaude, Model: "sonnet"},
			}},
			want: "--external-reviewers codex,codex::medium,claude:sonnet"},
		{name: "explicitly empty chain omitted", cfg: &config.Config{},
			sel: externalReviewSelection{Resolved: true, Explicit: true}, want: ""},
		{name: "auto-selected chain omitted", o: opts{TaskModel: "claude:opus"}, cfg: &config.Config{},
			sel: externalReviewSelection{Resolved: true, AutoSelected: true, Reviewers: []resolvedReviewer{
				{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5", Effort: "xhigh"},
			}},
			want: "--task-model claude:opus"},
		{name: "custom reviewer omitted", o: opts{TaskModel: "claude:opus"}, cfg: &config.Config{},
			sel: externalReviewSelection{Resolved: true, Explicit: true, Reviewers: []resolvedReviewer{
				{Provider: config.ExternalReviewToolCodex, Model: "gpt-5.5"},
				{Provider: config.ExternalReviewToolCustom},
			}},
			want: "--task-model claude:opus"},
		{name: "value outside charset omitted", o: opts{TaskModel: "claude:opus high", ReviewModel: "codex:$(x)"},
			cfg: &config.Config{}, sel: codexChain,
			want: "--external-reviewers claude:opus:high,codex:gpt-6-astra:high"},
		{name: "CLI override beating config",
			o:    opts{TaskModel: "codex:gpt-5.6-sol", ReviewModel: "claude:opus:xhigh"},
			cfg:  &config.Config{TaskModel: "claude:sonnet", ReviewModel: "claude:sonnet:low"},
			want: "--task-model codex:gpt-5.6-sol --review-model claude:opus:xhigh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, launchFlags(tc.o, tc.cfg, tc.sel))
		})
	}
}

func TestLauncherFor(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{name: "nil config", want: launcherCLI},
		{name: "none", cfg: &config.Config{}, want: launcherCLI},
		{name: "orca", cfg: &config.Config{Orca: true}, want: launcherOrca},
		{name: "t3", cfg: &config.Config{T3: true}, want: launcherT3},
		{name: "both", cfg: &config.Config{Orca: true, T3: true}, want: launcherT3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, launcherFor(tc.cfg))
		})
	}
}

func TestRecordLaunchHistory(t *testing.T) {
	t.Run("records the effective flags under the given launcher", func(t *testing.T) {
		cfgDir := t.TempDir()
		var stderr strings.Builder
		recordLaunchHistory(opts{ConfigDir: cfgDir, TaskModel: "claude:opus:high"}, &config.Config{},
			externalReviewSelection{}, launcherOrca, &stderr)

		entries := readLaunchHistory(filepath.Join(cfgDir, launchHistoryFile))
		require.Len(t, entries, 1)
		assert.Equal(t, launcherOrca, entries[0].Launcher)
		assert.Equal(t, "--task-model claude:opus:high", entries[0].Flags)
		assert.WithinDuration(t, time.Now(), entries[0].When, time.Minute)
		assert.Empty(t, stderr.String())
	})

	t.Run("a failure is one warning and never panics", func(t *testing.T) {
		cfgDir := t.TempDir()
		blockLaunchHistory(t, cfgDir)
		var stderr strings.Builder
		recordLaunchHistory(opts{ConfigDir: cfgDir}, &config.Config{}, externalReviewSelection{}, launcherCLI, &stderr)

		assert.Equal(t, 1, strings.Count(stderr.String(), "\n"), stderr.String())
		assert.True(t, strings.HasPrefix(stderr.String(), "warning: launch history not recorded: "), stderr.String())
		assert.NotPanics(t, func() {
			recordLaunchHistory(opts{ConfigDir: cfgDir}, &config.Config{}, externalReviewSelection{}, launcherCLI, nil)
		})
	})
}

// blockLaunchHistory makes the history unwritable on every platform: a non-empty directory
// at the file's path cannot be replaced by the rename.
func blockLaunchHistory(t *testing.T, cfgDir string) {
	t.Helper()
	blocked := filepath.Join(cfgDir, launchHistoryFile)
	require.NoError(t, os.MkdirAll(blocked, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(blocked, "keep"), []byte("x"), 0o600))
}

// launchRunClaude answers plan creation by writing docs/plans/launch.md and signaling
// PLAN_READY, ticks the plan's first open checkbox on each task session, and ends every other
// session with REVIEW_DONE. Paths are relative because a non-worktree run executes in place.
const launchRunClaude = `#!/bin/sh
prompt=$(cat)
plan=docs/plans/launch.md
case "$prompt" in
*"PLAN_READY"*)
  # file timestamps come from a coarse clock; without the pause the plan can predate the
  # start time runPlanMode searches from
  sleep 0.1
  mkdir -p docs/plans
  printf '%s\n' '# Launch' '' '### Task 1: Only' '- [ ] only item' > "$plan"
  printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:PLAN_READY>>>"}}'
  ;;
*"Complete ONE Task section"*)
  awk 'BEGIN{d=0} !d && /- \[ \]/ {sub(/- \[ \]/, "- [x]"); d=1} {print}' "$plan" > "$plan.tmp" && mv "$plan.tmp" "$plan"
  git add "$plan" >/dev/null && git commit -q -m "complete task" >/dev/null
  printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:ALL_TASKS_DONE>>>"}}'
  ;;
*)
  printf '%s\n' '{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:REVIEW_DONE>>>"}}'
  ;;
esac
printf '%s\n' '{"type":"result","result":""}'
`

// newLaunchConfigDir is a config directory pointing at launchRunClaude with every optional
// phase and integration off, so a run finishes quickly and offline.
func newLaunchConfigDir(t *testing.T) string {
	t.Helper()
	cfgDir := t.TempDir()
	fakeClaude := filepath.Join(t.TempDir(), "fake-claude")
	writeExecutable(t, fakeClaude, launchRunClaude)
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"), []byte("claude_command = "+fakeClaude+"\n"+
		"claude_swap_enabled = false\ncodex_enabled = false\nfinalize = none\nreport_enabled = false\n"+
		"iteration_delay_ms = 0\ntask_retry_count = 0\nkeep_awake = false\n"), 0o600))
	return cfgDir
}

// newLaunchRepo enters a fresh repository with isolated HOME, holding a committed one-task
// plan when withPlan is set.
func newLaunchRepo(t *testing.T, withPlan bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on windows
	dir := setupTestRepo(t)
	if withPlan {
		planFile := filepath.Join(dir, "docs", "plans", "launch.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o750))
		require.NoError(t, os.WriteFile(planFile, []byte("# Launch\n\n### Task 1: Only\n- [ ] only item\n"), 0o600))
		runGit(t, dir, "add", ".")
		runGit(t, dir, "commit", "-m", "add plan")
	}
	t.Chdir(dir)
	return dir
}

// runLaunch runs o through run() and returns its captured stdout and stderr, joined, with the
// run error.
func runLaunch(t *testing.T, o opts) (string, error) {
	t.Helper()
	var runErr error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() { runErr = run(t.Context(), o) })
	})
	return stdout + stderr, runErr
}

func TestRunRecordsLaunchHistory(t *testing.T) {
	const flags = "--task-model claude:sonnet --review-model claude:opus"
	launchOpts := func(cfgDir string) opts {
		return opts{ConfigDir: cfgDir, PlanFile: "docs/plans/launch.md", TaskModel: "claude:sonnet",
			ReviewModel: "claude:opus", MaxIterations: 3, NoColor: true}
	}

	for _, tc := range []struct {
		name     string
		mutate   func(*opts)
		launcher string
	}{
		{name: "cli", mutate: func(*opts) {}, launcher: launcherCLI},
		{name: "t3", mutate: func(o *opts) { o.T3 = true }, launcher: launcherT3},
		{name: "orca", mutate: func(o *opts) { o.Orca = true }, launcher: launcherOrca},
		{name: "tasks-only", mutate: func(o *opts) { o.TasksOnly = true }, launcher: launcherCLI},
	} {
		t.Run(tc.name+" run records one entry", func(t *testing.T) {
			newLaunchRepo(t, true)
			cfgDir := newLaunchConfigDir(t)
			o := launchOpts(cfgDir)
			tc.mutate(&o)

			output, err := runLaunch(t, o)
			require.NoError(t, err, output)

			entries := readLaunchHistory(filepath.Join(cfgDir, launchHistoryFile))
			require.Len(t, entries, 1)
			assert.Equal(t, tc.launcher, entries[0].Launcher)
			assert.Equal(t, flags, entries[0].Flags)
			assert.NotContains(t, output, "launch history not recorded")
		})
	}

	for _, tc := range []struct {
		name   string
		mutate func(*opts)
	}{
		{name: "review", mutate: func(o *opts) { o.Review = true }},
		{name: "external-only", mutate: func(o *opts) { o.ExternalOnly = true }},
	} {
		t.Run(tc.name+" records nothing", func(t *testing.T) {
			newLaunchRepo(t, true)
			cfgDir := newLaunchConfigDir(t)
			o := launchOpts(cfgDir)
			tc.mutate(&o)

			// on the base branch the review preflight fails, which is past the hook point
			_, err := runLaunch(t, o)
			require.ErrorContains(t, err, "nothing to review")
			assert.NoFileExists(t, filepath.Join(cfgDir, launchHistoryFile))
		})
	}

	t.Run("a repeated combination keeps one entry", func(t *testing.T) {
		cfgDir := newLaunchConfigDir(t)
		for _, orca := range []bool{false, true} {
			newLaunchRepo(t, true)
			o := launchOpts(cfgDir)
			o.Orca = orca
			output, err := runLaunch(t, o)
			require.NoError(t, err, output)
		}

		entries := readLaunchHistory(filepath.Join(cfgDir, launchHistoryFile))
		require.Len(t, entries, 1)
		assert.Equal(t, launcherOrca, entries[0].Launcher, "the newer launch wins")
		assert.Equal(t, flags, entries[0].Flags)
	})

	t.Run("a failing recorder warns and leaves the run outcome unchanged", func(t *testing.T) {
		dir := newLaunchRepo(t, true)
		cfgDir := newLaunchConfigDir(t)
		blockLaunchHistory(t, cfgDir)

		output, err := runLaunch(t, launchOpts(cfgDir))
		require.NoError(t, err, output)
		assert.Contains(t, output, "warning: launch history not recorded: ")
		assert.FileExists(t, filepath.Join(dir, "docs", "plans", "completed", "launch.md"), "the run still completes")
	})
}

func TestRunPlanModeRecordsLaunchHistoryOnContinuation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   bool
	}{
		{name: "continuing into execution records", answer: "y\n", want: true},
		{name: "stopping after plan creation records nothing", answer: "n\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newLaunchRepo(t, false)
			cfgDir := newLaunchConfigDir(t)
			withStdin(t, tc.answer)

			o := opts{ConfigDir: cfgDir, PlanDescription: "add launch", TaskModel: "claude:sonnet",
				MaxIterations: 3, NoColor: true}
			output, err := runLaunch(t, o)
			require.NoError(t, err, output)
			require.Contains(t, output, "Continue with plan implementation?", "plan creation found its plan")

			path := filepath.Join(cfgDir, launchHistoryFile)
			completed := filepath.Join(dir, "docs", "plans", "completed", "launch.md")
			if !tc.want {
				assert.NoFileExists(t, path)
				assert.NoFileExists(t, completed)
				return
			}
			entries := readLaunchHistory(path)
			require.Len(t, entries, 1, output)
			assert.Equal(t, launcherCLI, entries[0].Launcher)
			assert.Equal(t, "--task-model claude:sonnet", entries[0].Flags)
			assert.FileExists(t, completed, "execution followed plan creation")
		})
	}
}

// withStdin replaces os.Stdin with a pipe carrying input for the rest of the test.
func withStdin(t *testing.T, input string) {
	t.Helper()
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString(input)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = r.Close()
	})
}
