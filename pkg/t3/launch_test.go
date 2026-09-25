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
	f := newLaunchFixture(t)

	res, err := Launch(context.Background(), f.api, f.rpc, f.req)
	require.NoError(t, err)
	assert.Equal(t, f.wt, res.WorktreePath)
	assert.Equal(t, "demo", res.Branch)
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
