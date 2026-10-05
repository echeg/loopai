package t3

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRPC struct {
	worktreeDir string
	createErr   error
	openErr     error
	writeErr    error

	created []CreateWorktreeInput
	opened  []TerminalOpenInput
	written []string
}

func (f *fakeRPC) CreateWorktree(_ context.Context, in CreateWorktreeInput) (Worktree, error) {
	f.created = append(f.created, in)
	if f.createErr != nil {
		return Worktree{}, f.createErr
	}
	return Worktree{Path: f.worktreeDir, RefName: in.NewRefName}, nil
}

func (f *fakeRPC) OpenTerminal(_ context.Context, in TerminalOpenInput) error {
	f.opened = append(f.opened, in)
	return f.openErr
}

func (f *fakeRPC) WriteTerminal(_ context.Context, threadID, terminalID, data string) error {
	f.written = append(f.written, threadID+"|"+terminalID+"|"+data)
	return f.writeErr
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test path
	require.NoError(t, err)
	return string(data)
}

type launchFixture struct {
	src, wt string
	api     *fakeDispatcher
	rpc     *fakeRPC
	req     LaunchRequest
}

func newLaunchFixture(t *testing.T) *launchFixture {
	t.Helper()
	t.Setenv("T3CODE_HOME", t.TempDir())
	src, wt := t.TempDir(), t.TempDir()
	planRel := filepath.Join("docs", "plans", "20260925-demo.md")
	writeFile(t, filepath.Join(src, planRel), "# dirty plan\n")
	writeFile(t, filepath.Join(wt, planRel), "# committed plan\n")
	writeFile(t, filepath.Join(src, ".loopai", "config"), "t3 = true\n")
	writeFile(t, filepath.Join(src, ".loopai", "prompts", "task.txt"), "local prompt\n")
	writeFile(t, filepath.Join(src, ".loopai", "agents", "custom.txt"), "local agent\n")
	writeFile(t, filepath.Join(wt, ".loopai", "agents", "custom.txt"), "tracked agent\n")
	writeFile(t, filepath.Join(src, ".loopai", "progress", "progress-x.txt"), "not carried\n")

	return &launchFixture{
		src: src, wt: wt,
		api: &fakeDispatcher{shell: projectShell(src)},
		rpc: &fakeRPC{worktreeDir: wt},
		req: LaunchRequest{
			RepoRoot: src, PlanFile: filepath.Join(src, planRel), Branch: "demo", BaseRef: "main",
			SourceHead: "abc", LoopaiPath: "/usr/local/bin/loopai", Args: []string{"--codex"},
			Executor: "codex", Model: "gpt-5:high", Shell: ShellPOSIX,
			Endpoint: Endpoint{BaseURL: "http://127.0.0.1:3773", Token: "tok"},
			HeadOf:   func(string) (string, error) { return "abc", nil },
		},
	}
}

func TestLaunch(t *testing.T) {
	tests := []struct {
		name     string
		mode     LaunchMode
		instance ProviderInstance
	}{
		{name: "default"},
		{name: "auto", mode: LaunchAuto},
		{name: "terminal", mode: LaunchTerminal},
		{name: "terminal ignores instance", mode: LaunchTerminal, instance: ProviderInstance{ID: "loopai"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchFixture(t)
			f.req.Mode, f.req.Instance = tc.mode, tc.instance

			res, err := Launch(context.Background(), f.api, f.rpc, f.req)
			require.NoError(t, err)
			assert.Equal(t, f.wt, res.WorktreePath)
			assert.Equal(t, "demo", res.Branch)
			assert.Equal(t, LaunchTerminal, res.Mode)
			require.NotEmpty(t, res.ThreadID)

			assert.Equal(t, []CreateWorktreeInput{{Cwd: f.src, RefName: "main", NewRefName: "demo"}}, f.rpc.created)

			planRel := filepath.Join("docs", "plans", "20260925-demo.md")
			assert.Equal(t, "# dirty plan\n", readFile(t, filepath.Join(f.wt, planRel)), "plan edits carry over")
			assert.Equal(t, "t3 = true\n", readFile(t, filepath.Join(f.wt, ".loopai", "config")))
			assert.Equal(t, "local prompt\n", readFile(t, filepath.Join(f.wt, ".loopai", "prompts", "task.txt")))
			assert.Equal(t, "tracked agent\n", readFile(t, filepath.Join(f.wt, ".loopai", "agents", "custom.txt")), "existing files are kept")
			assert.NoFileExists(t, filepath.Join(f.wt, ".loopai", "progress", "progress-x.txt"))

			require.Len(t, f.api.commands, 1)
			create, ok := f.api.commands[0].(*ThreadCreate)
			require.True(t, ok)
			assert.Equal(t, res.ThreadID, create.ThreadID)
			assert.Equal(t, "p1", create.ProjectID)
			assert.Equal(t, "demo · starting", create.Title)
			assert.Equal(t, ModelSelection{InstanceID: InstanceCodex, Model: "gpt-5"}, create.ModelSelection)
			require.NotNil(t, create.WorktreePath)
			assert.Equal(t, f.wt, *create.WorktreePath)

			require.Len(t, f.rpc.opened, 1)
			assert.Equal(t, TerminalOpenInput{
				ThreadID: res.ThreadID, TerminalID: "loopai", Cwd: f.wt, WorktreePath: f.wt,
				Env: map[string]string{
					"LOOPAI_T3": "true", EnvToken: "tok", EnvURL: "http://127.0.0.1:3773", EnvThreadID: res.ThreadID,
				},
			}, f.rpc.opened[0])
			wantCmd := "/usr/local/bin/loopai --t3 --codex " + posixQuote(planRel) + "\r"
			assert.Equal(t, []string{res.ThreadID + "|loopai|" + wantCmd}, f.rpc.written)
		})
	}
}

func TestLaunchPreflightErrors(t *testing.T) {
	t.Run("plan outside repository", func(t *testing.T) {
		f := newLaunchFixture(t)
		f.req.PlanFile = filepath.Join(t.TempDir(), "plan.md")
		_, err := Launch(context.Background(), f.api, f.rpc, f.req)
		require.ErrorContains(t, err, "outside the repository")
		assert.Empty(t, f.rpc.created)
	})
	t.Run("missing plan", func(t *testing.T) {
		f := newLaunchFixture(t)
		f.req.PlanFile = filepath.Join(f.src, "docs", "plans", "missing.md")
		_, err := Launch(context.Background(), f.api, f.rpc, f.req)
		require.ErrorContains(t, err, "t3: plan:")
	})
	t.Run("no project", func(t *testing.T) {
		f := newLaunchFixture(t)
		f.api.shell = projectShell("/elsewhere")
		_, err := Launch(context.Background(), f.api, f.rpc, f.req)
		require.ErrorContains(t, err, "add the repository in T3 Code first")
		assert.Empty(t, f.rpc.created)
	})
	t.Run("shell error", func(t *testing.T) {
		f := newLaunchFixture(t)
		f.api.shellErr = &APIError{Status: 401}
		_, err := Launch(context.Background(), f.api, f.rpc, f.req)
		assert.True(t, IsAuthError(err))
	})
	t.Run("worktree creation fails", func(t *testing.T) {
		f := newLaunchFixture(t)
		f.rpc.createErr = errors.New("branch exists")
		_, err := Launch(context.Background(), f.api, f.rpc, f.req)
		require.ErrorContains(t, err, "branch exists")
		var partial *PartialLaunchError
		assert.NotErrorAs(t, err, &partial)
	})
}

func TestLaunchPartialFailures(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(f *launchFixture)
		wantErr    string
		wantThread bool
	}{
		{"head mismatch", func(f *launchFixture) {
			f.req.HeadOf = func(string) (string, error) { return "def", nil }
		}, "does not match source HEAD abc", false},
		{"head error", func(f *launchFixture) {
			f.req.HeadOf = func(string) (string, error) { return "", errors.New("no git") }
		}, "read worktree HEAD", false},
		{"thread create", func(f *launchFixture) { f.api.dispErr = errors.New("rejected") }, "rejected", false},
		{"terminal open", func(f *launchFixture) { f.rpc.openErr = errors.New("no pty") }, "no pty", true},
		{"terminal write", func(f *launchFixture) { f.rpc.writeErr = errors.New("closed") }, "closed", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchFixture(t)
			tc.mutate(f)
			res, err := Launch(context.Background(), f.api, f.rpc, f.req)
			require.ErrorContains(t, err, tc.wantErr)
			var partial *PartialLaunchError
			require.ErrorAs(t, err, &partial)
			assert.Equal(t, f.wt, res.WorktreePath)
			assert.Contains(t, err.Error(), "already created: worktree "+f.wt+" (branch demo)")
			if tc.wantThread {
				assert.NotEmpty(t, res.ThreadID)
				assert.Contains(t, err.Error(), "thread "+res.ThreadID)
			} else {
				assert.Empty(t, res.ThreadID)
			}
		})
	}
}

func TestLaunchCopyFailure(t *testing.T) {
	f := newLaunchFixture(t)
	// a directory where the plan should be written makes the copy fail
	planRel := filepath.Join("docs", "plans", "20260925-demo.md")
	require.NoError(t, os.Remove(filepath.Join(f.wt, planRel)))
	require.NoError(t, os.MkdirAll(filepath.Join(f.wt, planRel), 0o750))
	_, err := Launch(context.Background(), f.api, f.rpc, f.req)
	require.ErrorContains(t, err, "copy plan")
}

func TestLaunchCommand(t *testing.T) {
	tests := []struct {
		name    string
		shell   ShellKind
		program string
		args    []string
		want    string
	}{
		{"posix plain", ShellPOSIX, "/usr/bin/loopai", []string{"--t3", "--task-model", "opus:high", "docs/plans/x.md"},
			"/usr/bin/loopai --t3 --task-model opus:high docs/plans/x.md"},
		{"posix spaces and quotes", ShellPOSIX, "/opt/my tools/loopai", []string{"it's plan.md", ""},
			`'/opt/my tools/loopai' 'it'"'"'s plan.md' ''`},
		{"powershell", ShellPowerShell, `C:\Program Files\loopai\loopai.exe`, []string{"--t3", `docs\plans\it's.md`},
			`& 'C:\Program Files\loopai\loopai.exe' '--t3' 'docs\plans\it''s.md'`},
		{"powershell curly quote and dollar", ShellPowerShell, `C:\l.exe`, []string{"a\u2019b", "$env:X"},
			"& 'C:\\l.exe' 'a\u2019\u2019b' '$env:X'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, LaunchCommand(tc.shell, tc.program, tc.args))
		})
	}
}

func TestCarryInputsSkipsNonRegular(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "plan.md"), "p")
	// .loopai/config as a directory is walked; prompts missing entirely is skipped
	writeFile(t, filepath.Join(src, ".loopai", "config", "nested"), "n")
	require.NoError(t, carryInputs(src, dst, "plan.md"))
	assert.Equal(t, "n", readFile(t, filepath.Join(dst, ".loopai", "config", "nested")))
	assert.NoDirExists(t, filepath.Join(dst, ".loopai", "prompts"))
}

func TestLaunchAgentModes(t *testing.T) {
	for _, mode := range []LaunchMode{"", LaunchAuto, LaunchAgent} {
		t.Run(string(mode), func(t *testing.T) {
			f := newLaunchFixture(t)
			f.req.Mode = mode
			f.req.Instance = ProviderInstance{ID: "custom-loopai", Driver: "grok", Enabled: true, BinaryPath: "loopai-acp"}
			f.req.Args = []string{"--task-model", "codex:gpt-5:high", "--review-model", "claude:opus", "--external-reviewers", "claude:opus,codex:gpt-5"}

			res, err := Launch(context.Background(), f.api, f.rpc, f.req)
			require.NoError(t, err)
			assert.Equal(t, LaunchAgent, res.Mode)
			assert.Equal(t, f.wt, res.WorktreePath)
			assert.Equal(t, "demo", res.Branch)
			require.NotEmpty(t, res.ThreadID)
			assert.Equal(t, []CreateWorktreeInput{{Cwd: f.src, RefName: "main", NewRefName: "demo"}}, f.rpc.created)
			assert.Empty(t, f.rpc.opened)
			assert.Empty(t, f.rpc.written)

			require.Len(t, f.api.commands, 2)
			create, ok := f.api.commands[0].(*ThreadCreate)
			require.True(t, ok)
			selection := ModelSelection{InstanceID: "custom-loopai", Model: LoopaiModel}
			assert.Equal(t, "thread.create", create.Type)
			assert.Equal(t, res.ThreadID, create.ThreadID)
			assert.Equal(t, "p1", create.ProjectID)
			assert.Equal(t, "demo", create.Title)
			assert.Equal(t, selection, create.ModelSelection)
			require.NotNil(t, create.WorktreePath)
			assert.Equal(t, f.wt, *create.WorktreePath)
			require.NotNil(t, create.Branch)
			assert.Equal(t, "demo", *create.Branch)

			turn, ok := f.api.commands[1].(*ThreadTurnStart)
			require.True(t, ok)
			assert.Equal(t, "thread.turn.start", turn.Type)
			assert.Equal(t, res.ThreadID, turn.ThreadID)
			assert.Equal(t, selection, turn.ModelSelection)
			assert.Equal(t, "docs/plans/20260925-demo.md --task-model codex:gpt-5:high --review-model claude:opus --external-reviewers claude:opus,codex:gpt-5", turn.Message.Text)
			assert.Equal(t, "demo", turn.TitleSeed)
			assert.Equal(t, create.Title, turn.TitleSeed, "T3 title generation requires a matching initial title")
			assert.NotContains(t, turn.Message.Text, f.req.Endpoint.Token)
			assert.Equal(t, "# dirty plan\n", readFile(t, filepath.Join(f.wt, "docs", "plans", "20260925-demo.md")))
			assert.Equal(t, "t3 = true\n", readFile(t, filepath.Join(f.wt, ".loopai", "config")))
			assert.Equal(t, "local prompt\n", readFile(t, filepath.Join(f.wt, ".loopai", "prompts", "task.txt")))
			assert.Equal(t, "tracked agent\n", readFile(t, filepath.Join(f.wt, ".loopai", "agents", "custom.txt")))
		})
	}
}

func TestLaunchModePreflightErrors(t *testing.T) {
	tests := []struct {
		name    string
		mode    LaunchMode
		wantErr string
	}{
		{"agent without instance", LaunchAgent, "requires a loopai provider instance; see docs/t3-code.md"},
		{"invalid mode", LaunchMode("invalid"), "unknown launch mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaunchFixture(t)
			f.req.Mode = tc.mode
			_, err := Launch(context.Background(), f.api, f.rpc, f.req)
			require.ErrorContains(t, err, tc.wantErr)
			assert.Zero(t, f.api.shellHits)
			assert.Empty(t, f.api.commands)
			assert.Empty(t, f.rpc.created)
			assert.Empty(t, f.rpc.opened)
			assert.Empty(t, f.rpc.written)
		})
	}
}

func TestLaunchWhitespacePlan(t *testing.T) {
	for _, plan := range []string{"plan with spaces.md", "plan\u2003with-unicode-space.md"} {
		for _, mode := range []LaunchMode{LaunchAgent, LaunchAuto, LaunchTerminal} {
			t.Run(plan+"/"+string(mode), func(t *testing.T) {
				f := newLaunchFixture(t)
				f.req.Mode = mode
				f.req.Instance = ProviderInstance{ID: "loopai"}
				rel := filepath.Join("docs", "plans", plan)
				f.req.PlanFile = filepath.Join(f.src, rel)
				writeFile(t, f.req.PlanFile, "# spaced plan\n")

				res, err := Launch(context.Background(), f.api, f.rpc, f.req)
				if mode != LaunchTerminal {
					require.ErrorContains(t, err, "plan path without whitespace")
					assert.Zero(t, f.api.shellHits)
					assert.Empty(t, f.api.commands)
					assert.Empty(t, f.rpc.created)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, LaunchTerminal, res.Mode)
				require.Len(t, f.rpc.opened, 1)
				want := res.ThreadID + "|loopai|/usr/local/bin/loopai --t3 --codex " + posixQuote(rel) + "\r"
				assert.Equal(t, []string{want}, f.rpc.written)
				assert.Equal(t, "# spaced plan\n", readFile(t, filepath.Join(f.wt, rel)))
			})
		}
	}
}

type failedTurnDispatcher struct {
	*fakeDispatcher
	err error
}

func (f *failedTurnDispatcher) Dispatch(ctx context.Context, cmd Command) (int64, error) {
	if _, ok := cmd.(*ThreadTurnStart); ok {
		return 0, f.err
	}
	return f.fakeDispatcher.Dispatch(ctx, cmd)
}

func TestLaunchTurnPartialFailure(t *testing.T) {
	f := newLaunchFixture(t)
	f.req.Mode = LaunchAgent
	f.req.Instance = ProviderInstance{ID: "loopai"}
	turnErr := errors.New("turn rejected")
	api := &failedTurnDispatcher{fakeDispatcher: f.api, err: turnErr}

	res, err := Launch(context.Background(), api, f.rpc, f.req)
	require.ErrorIs(t, err, turnErr)
	var partial *PartialLaunchError
	require.ErrorAs(t, err, &partial)
	assert.Equal(t, res, partial.Result)
	assert.Equal(t, LaunchAgent, res.Mode)
	assert.Equal(t, f.wt, res.WorktreePath)
	require.NotEmpty(t, res.ThreadID)
	assert.Contains(t, err.Error(), "worktree "+f.wt+" (branch demo)")
	assert.Contains(t, err.Error(), "thread "+res.ThreadID)
	require.Len(t, f.api.commands, 1)
	assert.Empty(t, f.rpc.opened)
	assert.Empty(t, f.rpc.written)
}

func TestAgentPrompt(t *testing.T) {
	rel := filepath.Join("docs", "plans", "demo.md")
	assert.Equal(t, "docs/plans/demo.md", agentPrompt(rel, nil))
	assert.Equal(t, "./-demo.md", agentPrompt("-demo.md", nil))
	assert.Equal(t, "./-plans/demo.md", agentPrompt(filepath.Join("-plans", "demo.md"), nil))
	args := []string{"--task-model", "codex:gpt-5:high"}
	assert.Equal(t, "docs/plans/demo.md --task-model codex:gpt-5:high", agentPrompt(rel, args))
	assert.Equal(t, []string{"--task-model", "codex:gpt-5:high"}, args)
}
