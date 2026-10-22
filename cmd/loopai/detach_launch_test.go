package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateDetachFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string // empty when the flags are valid
	}{
		{name: "a plan", args: []string{"--detach", "docs/plans/x.md"}},
		{name: "tasks-only", args: []string{"--detach", "--tasks-only", "docs/plans/x.md"}},
		{name: "explicit worktree", args: []string{"--detach", "--worktree", "docs/plans/x.md"}},
		{name: "branch and models", args: []string{"--detach", "--branch", "b", "--task-model", "codex:gpt-6-astra",
			"--external-reviewers=", "docs/plans/x.md"}},
		{name: "the child's own arguments", args: []string{"--detached-run=x-20261022-120000-abcd", "docs/plans/x.md"}},
		{name: "chain", args: []string{"--detach", "docs/plans/a.md,docs/plans/b.md"},
			wantErr: "a detached run takes exactly one plan; plan chains are not supported"},
		{name: "surplus positional", args: []string{"--detach", "docs/plans/a.md", "docs/plans/b.md"},
			wantErr: "--detach takes exactly one plan argument, got 2"},
		{name: "review", args: []string{"--detach", "--review", "docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --review"},
		{name: "external-only", args: []string{"--detach", "--external-only", "docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --external-only"},
		{name: "commit", args: []string{"--detach", "--commit", "docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --commit: a detached run never commits into the invoking checkout"},
		{name: "serve", args: []string{"--detach", "--serve", "docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --serve"},
		{name: "watch", args: []string{"--detach", "--watch", "dir", "docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --watch"},
		{name: "cmux workspace", args: []string{"--detach", "--cmux-workspace", "docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --cmux-workspace"},
		{name: "plan creation", args: []string{"--detach", "--plan", "describe it"},
			wantErr: "--detach cannot be combined with --plan"},
		{name: "gen-agents", args: []string{"--detach", "--gen-agents"},
			wantErr: "--detach cannot be combined with --gen-agents"},
		{name: "init", args: []string{"--detach", "--init", "docs/plans/x.md"}, wantErr: "--detach cannot be combined with --init"},
		{name: "reset", args: []string{"--detach", "--reset", "docs/plans/x.md"}, wantErr: "--detach cannot be combined with --reset"},
		{name: "dump-defaults", args: []string{"--detach", "--dump-defaults", "d", "docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --dump-defaults"},
		{name: "merge", args: []string{"--detach", "--merge", "feature"}, wantErr: "--merge cannot be combined"},
		{name: "pr", args: []string{"--detach", "--pr", "feature"}, wantErr: "--pr cannot be combined"},
		{name: "report", args: []string{"--detach", "--report", "feature"}, wantErr: "--report cannot be combined"},
		{name: "clear", args: []string{"--detach", "--clear"}, wantErr: "--clear cannot be combined"},
		{name: "no plan", args: []string{"--detach"}, wantErr: "--detach requires a plan file argument"},
		{name: "the child without a plan", args: []string{"--detached-run=x-20261022-120000-abcd"},
			wantErr: "--detached-run requires a plan file argument"},
		{name: "the child with a chain", args: []string{"--detached-run=x-1", "docs/plans/a.md,docs/plans/b.md"},
			wantErr: "a detached run takes exactly one plan"},
		{name: "the child with a close-out", args: []string{"--detached-run=x-1", "--merge", "feature"},
			wantErr: "--detached-run cannot be combined with --merge"},
		{name: "an explicit empty run id", args: []string{"--detached-run=", "docs/plans/x.md"},
			wantErr: "--detached-run requires a run id"},
		{name: "a blank run id", args: []string{"--detached-run", " ", "docs/plans/x.md"},
			wantErr: "--detached-run requires a run id"},
		{name: "detach with the child's run id", args: []string{"--detach", "--detached-run=x-20261022-120000-abcd",
			"docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --detached-run: the launcher starts the detached run itself"},
		{name: "detach with the child's run id separate", args: []string{"--detached-run", "x-1", "--detach",
			"docs/plans/x.md"}, wantErr: "--detach cannot be combined with --detached-run"},
		{name: "detach with an empty child run id", args: []string{"--detach", "--detached-run=", "docs/plans/x.md"},
			wantErr: "--detach cannot be combined with --detached-run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFlags(parseTestOpts(t, tc.args...))
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}

	t.Run("a run without detach flags is not affected", func(t *testing.T) {
		require.NoError(t, validateDetachFlags(parseTestOpts(t, "--commit", "--worktree", "docs/plans/a.md,docs/plans/b.md")))
	})
}

func TestDetachMarksExecutionAndStandalone(t *testing.T) {
	o := parseTestOpts(t, "--detach", "docs/plans/x.md")
	assert.True(t, o.executionModeSet, "--detach is an execution flag close-out commands reject")
	assert.True(t, hasExecutionMode(opts{Detach: true}))
	assert.True(t, isStandaloneCommand(o), "the launcher owns no cmux card and is never handed over")
	assert.False(t, isStandaloneCommand(parseTestOpts(t, "docs/plans/x.md")))
}

func TestStripDetachArgs(t *testing.T) {
	const plan = "docs/plans/x.md"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "detach", args: []string{"--detach", "--tasks-only", plan}, want: []string{"--tasks-only", plan}},
		{name: "detached-run attached", args: []string{"--detached-run=x-1", plan}, want: []string{plan}},
		{name: "detached-run separate", args: []string{"--detached-run", "x-1", plan}, want: []string{plan}},
		{name: "t3-thread attached", args: []string{"--detach", "--t3-thread=thread:a%3Ab", plan}, want: []string{plan}},
		{name: "t3-thread separate", args: []string{"--t3-thread", "thread-1", "--detach", plan}, want: []string{plan}},
		{name: "acp-thread both forms", args: []string{"--acp-thread=t-1", "--acp-thread", "t-2", plan}, want: []string{plan}},
		{name: "acp-session both forms", args: []string{"--acp-session", "s-1", "--acp-session=s-2", "--detach", plan},
			want: []string{plan}},
		{name: "explicit empty values stay", args: []string{"--detach", "--external-reviewers=", "--codex-args=", plan},
			want: []string{"--external-reviewers=", "--codex-args=", plan}},
		{name: "an empty value of a stripped flag", args: []string{"--detached-run=", plan}, want: []string{plan}},
		{name: "-- is kept with everything after it", args: []string{"--detach", "--tasks-only", "--", "--detach"},
			want: []string{"--tasks-only", "--", "--detach"}},
		{name: "a value flag before -- consumes its value only", args: []string{"--t3-thread", "t", "--", plan},
			want: []string{"--", plan}},
		{name: "flags only named alike stay", args: []string{"--detach", "--branch=--detach", "--detachable", plan},
			want: []string{"--branch=--detach", "--detachable", plan}},
		{name: "nothing", args: nil, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripDetachArgs(tc.args))
		})
	}

	t.Run("the stripped arguments parse back to the same plan", func(t *testing.T) {
		for _, args := range [][]string{
			{"--detach", "--external-reviewers=", "--tasks-only", plan},
			{"--detached-run", "x-1", "--detach", "--branch", "b", plan},
			{"--detach", "--", plan},
		} {
			launcher := parseTestOpts(t, args...)
			var child opts
			parser := flags.NewParser(&child, flags.HelpFlag|flags.PassDoubleDash)
			rest, err := parser.ParseArgs(append([]string{"--detached-run=x-20261022-120000-abcd"}, stripDetachArgs(args)...))
			require.NoError(t, err, "%v", args)
			child.markFlagsSet(parser)
			require.NoError(t, child.applyPositionalArgs(rest))
			assert.Equal(t, launcher.PlanFile, child.PlanFile, "%v", args)
			assert.Equal(t, plan, child.PlanFile)
			assert.Empty(t, child.extraArgs, "%v", args)
			assert.False(t, child.Detach, "%v", args)
			assert.Equal(t, "x-20261022-120000-abcd", child.DetachedRun, "%v", args)
			assert.Equal(t, launcher.ExternalReviewers, child.ExternalReviewers)
			assert.Equal(t, launcher.externalReviewersSet, child.externalReviewersSet, "an explicit empty value survives")
			assert.Equal(t, launcher.Branch, child.Branch)
		}
	})
}

func TestDetectSandbox(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "none", env: nil, want: ""},
		{name: "codex sandbox", env: map[string]string{"CODEX_SANDBOX": "seatbelt"}, want: "CODEX_SANDBOX=seatbelt"},
		{name: "codex network disabled", env: map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": "1"},
			want: "CODEX_SANDBOX_NETWORK_DISABLED=1"},
		{name: "claude code sandbox", env: map[string]string{"SANDBOX_RUNTIME": "1"}, want: "SANDBOX_RUNTIME=1"},
		{name: "network enabled", env: map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": "0"}, want: ""},
		{name: "sandbox runtime off", env: map[string]string{"SANDBOX_RUNTIME": "0"}, want: ""},
		{name: "codex wins", env: map[string]string{"SANDBOX_RUNTIME": "1", "CODEX_SANDBOX": "seatbelt"},
			want: "CODEX_SANDBOX=seatbelt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, detectSandbox(func(key string) string { return tc.env[key] }))
		})
	}
}

func TestRefuseDetachInSandbox(t *testing.T) {
	require.NoError(t, refuseDetachInSandbox(func(string) string { return "" }))

	err := refuseDetachInSandbox(func(key string) string { return map[string]string{"CODEX_SANDBOX": "seatbelt"}[key] })
	require.EqualError(t, err, "refusing to start a detached run inside an agent sandbox (CODEX_SANDBOX=seatbelt): "+
		"the run and every model process it starts would inherit the sandbox; start the plan in a new loopai thread "+
		"(Mode 2 of the loopai-t3 skill) or from a shell outside the sandbox")
}

func TestSystemdScopePrefix(t *testing.T) {
	const runtimeDir = "/run/user/1000"
	found := func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	socket := func(path string) (os.FileInfo, error) {
		if path != filepath.Join(runtimeDir, "systemd", "private") {
			return nil, os.ErrNotExist
		}
		return os.Stat(os.TempDir()) // any info stands for an existing socket
	}
	noSocket := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	env := func(key string) string {
		if key == "XDG_RUNTIME_DIR" {
			return runtimeDir
		}
		return ""
	}
	noEnv := func(string) string { return "" }

	tests := []struct {
		name     string
		goos     string
		lookPath func(string) (string, error)
		stat     func(string) (os.FileInfo, error)
		getenv   func(string) string
		want     []string
	}{
		{name: "darwin", goos: "darwin", lookPath: found, stat: socket, getenv: env},
		{name: "linux without systemd-run", goos: "linux", lookPath: missing, stat: socket, getenv: env},
		{name: "linux without the private socket", goos: "linux", lookPath: found, stat: noSocket, getenv: env},
		{name: "linux without a runtime directory", goos: "linux", lookPath: found, stat: socket, getenv: noEnv},
		{name: "linux with both", goos: "linux", lookPath: found, stat: socket, getenv: env,
			want: []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, systemdScopePrefix(tc.goos, tc.lookPath, tc.stat, tc.getenv))
		})
	}
}

func TestDetachChildArgv(t *testing.T) {
	argv := []string{"--tasks-only", "docs/plans/x.md"}
	t.Run("direct", func(t *testing.T) {
		assert.Equal(t, []string{"/bin/loopai", "--detached-run=x-1", "--tasks-only", "docs/plans/x.md"},
			detachChildArgv(nil, "/bin/loopai", "x-1", argv))
	})
	t.Run("through the systemd scope", func(t *testing.T) {
		prefix := []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--"}
		assert.Equal(t, []string{"/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", "--",
			"/bin/loopai", "--detached-run=x-1", "--tasks-only", "docs/plans/x.md"},
			detachChildArgv(prefix, "/bin/loopai", "x-1", argv))
		assert.Len(t, prefix, 6, "the prefix is not modified")
	})
	t.Run("with a helper selector", func(t *testing.T) {
		detachChildArgs = []string{"-test.run=^X$", "--"}
		t.Cleanup(func() { detachChildArgs = nil })
		assert.Equal(t, []string{"/bin/loopai", "-test.run=^X$", "--", "--detached-run=x-1", "--tasks-only", "docs/plans/x.md"},
			detachChildArgv(nil, "/bin/loopai", "x-1", argv))
	})
}

func TestDetachChildEnv(t *testing.T) {
	environ := []string{"PATH=/bin", "HOME=/home/u", "T3_ACP_MCP_ENDPOINT=http://x", "T3_ACP_MCP_AUTHORIZATION=Bearer y",
		"LOOPAI_T3_TOKEN=secret", "LOOPAI_T3_URL=http://t3", "CMUX_WORKSPACE_ID=ws", "CMUX_SOCKET_PATH=/s",
		"LOOPAI_CONFIG_DIR=/now", "T3CODE_HOME=/t3"}

	t.Run("scrubs the T3 credentials and cmux", func(t *testing.T) {
		assert.Equal(t, []string{"PATH=/bin", "HOME=/home/u", "LOOPAI_T3_URL=http://t3", "LOOPAI_CONFIG_DIR=/now",
			"T3CODE_HOME=/t3"}, detachChildEnv(environ, nil))
	})

	t.Run("recorded options fill what the environment lacks", func(t *testing.T) {
		recorded := map[string]string{"LOOPAI_CONFIG_DIR": "/then", "LOOPAI_ORCA": "1", "LD_PRELOAD": "/evil.so"}
		env := detachChildEnv(environ, recorded)
		assert.Contains(t, env, "LOOPAI_CONFIG_DIR=/now", "an explicit value wins")
		assert.NotContains(t, env, "LOOPAI_CONFIG_DIR=/then")
		assert.Contains(t, env, "LOOPAI_ORCA=1")
		assert.NotContains(t, env, "LD_PRELOAD=/evil.so", "only option variables are restored from a descriptor")
	})

	t.Run("the options it sets are recorded", func(t *testing.T) {
		env := detachChildEnv(environ, map[string]string{"LOOPAI_ORCA": "1"})
		assert.Equal(t, map[string]string{"LOOPAI_CONFIG_DIR": "/now", "LOOPAI_ORCA": "1"}, detachEnvOptions(env))
		assert.Nil(t, detachEnvOptions([]string{"PATH=/bin"}))
		assert.Equal(t, map[string]string{"LOOPAI_WEB_HOST": ""}, detachEnvOptions([]string{"LOOPAI_WEB_HOST="}),
			"a set but empty option is recorded as set")
	})
}

func TestRelativePlan(t *testing.T) {
	root := t.TempDir()
	plan := filepath.Join(root, "docs", "plans", "x.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(plan), 0o750))
	require.NoError(t, os.WriteFile(plan, []byte("# X\n"), 0o600))

	assert.Equal(t, filepath.Join("docs", "plans", "x.md"), relativePlan(root, "./docs/plans/x.md"))
	assert.Equal(t, filepath.Join("docs", "plans", "x.md"), relativePlan(root, plan), "an absolute plan in the checkout")
	outside := filepath.Join(t.TempDir(), "y.md")
	assert.Equal(t, outside, relativePlan(root, outside), "a plan outside the checkout stays absolute")
}

func TestReadDetachAck(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		answered bool
		ok       bool
		readErr  string
	}{
		{name: "ok", input: `{"ok":true,"run_id":"x-1","attempt":2}` + "\n", answered: true, ok: true},
		{name: "error", input: `{"ok":false,"error":"x"}` + "\n", answered: true},
		{name: "a line without its newline", input: `{"ok":true,"run_id":"x-1"}`, answered: true, ok: true},
		{name: "eof", input: ""},
		{name: "malformed", input: "boom\n", answered: true, readErr: `malformed answer "boom"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := readDetachAck(strings.NewReader(tc.input))
			assert.Equal(t, tc.answered, res.answered)
			assert.Equal(t, tc.ok, res.ack.OK)
			if tc.readErr != "" {
				require.ErrorContains(t, res.readErr, tc.readErr)
			} else {
				require.NoError(t, res.readErr)
			}
			assert.Equal(t, !tc.answered && tc.readErr == "", res.noHandshake())
		})
	}
}

func TestAwaitDetachAck(t *testing.T) {
	t.Run("times out", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
		res := awaitDetachAck(t.Context(), r, 50*time.Millisecond)
		require.ErrorContains(t, res.waitErr, "no answer within 50ms")
		assert.False(t, res.noHandshake(), "a launcher that stopped waiting never retries")
	})

	t.Run("stops on cancellation", func(t *testing.T) {
		r, w, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		res := awaitDetachAck(ctx, r, time.Minute)
		require.ErrorIs(t, res.waitErr, context.Canceled)
	})
}

func TestConsoleTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	lines := make([]string, 0, 30)
	for i := range 30 {
		lines = append(lines, "line "+strings.Repeat("x", i))
	}
	earlier := "an earlier attempt\n"
	require.NoError(t, os.WriteFile(path, []byte(earlier+strings.Join(lines, "\n")+"\n"), 0o600))

	tail := consoleTail(path, int64(len(earlier)), 3)
	assert.Equal(t, "  "+lines[27]+"\n  "+lines[28]+"\n  "+lines[29], tail)
	assert.NotContains(t, consoleTail(path, int64(len(earlier)), 100), "an earlier attempt",
		"only this attempt's output")
	assert.Empty(t, consoleTail(path, int64(len(earlier)+len(strings.Join(lines, "\n"))+1), 3))
	assert.Empty(t, consoleTail(filepath.Join(t.TempDir(), "missing"), 0, 3))
}
