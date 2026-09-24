package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunModelProviderAcceptance(t *testing.T) {
	const inheritedModels = "task_model = codex:gpt-6-astra:medium\nreview_model = codex:gpt-6-astra:high\n"
	tests := []struct {
		name        string
		global      string
		local       string
		args        []string
		wantCommand string
		wantArgs    string // substring of the logged executor arguments
		wantError   string
		wantErrPart string // substring match for errors carrying temporary paths
	}{
		{
			name:      "removed codex flag names its replacement",
			args:      []string{"--codex", "--task-model", "claude:fable:high"},
			wantError: "--codex was removed; set the provider in the model spec instead, e.g. --task-model codex:gpt-6-astra:medium",
		},
		{
			name:        "global task model provider selects codex and strips the provider",
			global:      inheritedModels,
			wantCommand: "codex",
			wantArgs:    `model="gpt-6-astra"`,
		},
		{
			name:        "CLI models switch to Claude with codex reviewer",
			global:      inheritedModels,
			args:        []string{"--task-model", "claude:fable:high", "--review-model", "claude:fable:high", "--external-reviewers", "codex:gpt-6-astra:high"},
			wantCommand: "claude",
			wantArgs:    "--model fable --effort high",
		},
		{
			name:      "bare task model names the prefixed rewrite",
			args:      []string{"--task-model", "gpt-6-astra:medium"},
			wantError: `--task-model / task_model "gpt-6-astra:medium" is missing a provider prefix; write "codex:gpt-6-astra:medium"`,
		},
		{
			name:        "claude task runs under an inherited codex review model",
			global:      inheritedModels,
			args:        []string{"--task-model", "claude:fable:high", "--external-reviewers", "codex:gpt-6-astra:high"},
			wantCommand: "claude",
			wantArgs:    "--model fable --effort high",
		},
		{
			name:        "removed global executor key fails before any executor starts",
			global:      "executor = codex\ntask_model = codex:gpt-6-astra:medium\n",
			wantErrPart: "parse global config",
		},
		{
			name:        "removed local executor key fails before any executor starts",
			global:      inheritedModels,
			local:       "executor =\n",
			wantErrPart: "parse local config",
		},
		{
			name:        "codex wrapper runs a codex task",
			global:      "codex_command = codex-wrapper\ntask_model = codex:gpt-6-astra:medium\n",
			wantCommand: "codex-wrapper",
			wantArgs:    `model="gpt-6-astra"`,
		},
		{
			// the real codex binary would reject codex:fable as a claude model under codex
			name:        "codex wrapper accepts a model name of its own",
			global:      "codex_command = codex-wrapper\ntask_model = codex:fable:medium\n",
			wantCommand: "codex-wrapper",
			wantArgs:    `model="fable"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			dir := setupTestRepo(t)
			t.Chdir(dir)
			cfgDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config"), []byte(tt.global), 0o600))
			require.NoError(t, os.MkdirAll(".loopai", 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(".loopai", "config"), []byte(tt.local), 0o600))
			binDir := t.TempDir()
			invocationLog := filepath.Join(t.TempDir(), "invocations")
			t.Setenv("MODEL_PROVIDER_INVOCATIONS", invocationLog)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			for _, command := range []string{"claude", "codex", "codex-wrapper"} {
				response := "<<<RALPHEX:ALL_TASKS_DONE>>>"
				if command == "claude" {
					response = `{"type":"content_block_delta","delta":{"type":"text_delta","text":"<<<RALPHEX:ALL_TASKS_DONE>>>"}}`
				}
				writeExecutable(t, filepath.Join(binDir, command), "#!/bin/sh\n"+
					"printf '%s %s\\n' '"+command+"' \"$*\" >> \"$MODEL_PROVIDER_INVOCATIONS\"\n"+
					"printf '%s\\n' '# Acceptance' '' '### Task 1: Verify' '' '- [x] fixture task' > plan.md\n"+
					"printf '%s\\n' '"+response+"'\n")
			}
			o := parseTestOpts(t, append([]string{"--config-dir", cfgDir, "--no-color", "--no-claude-swap"}, tt.args...)...)

			// A canceled run traverses the real config merge, CLI overrides, primary and
			// reviewer validation, and dependency checks before stopping ahead of execution.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err := run(ctx, o)
			if tt.wantError != "" {
				require.EqualError(t, err, tt.wantError)
				assert.NoFileExists(t, invocationLog, "invalid models must fail before any executor starts")
				return
			}
			if tt.wantErrPart != "" {
				require.ErrorContains(t, err, tt.wantErrPart)
				require.ErrorContains(t, err, "config key executor was removed; set the provider in the model spec instead")
				assert.NoFileExists(t, invocationLog, "removed keys must fail before any executor starts")
				return
			}
			require.ErrorIs(t, err, context.Canceled)
			assert.NoFileExists(t, invocationLog)

			// Now execute a task to completion to prove the selected provider reaches
			// the runner. Review selection was validated in the full-mode run above.
			require.NoError(t, os.WriteFile("plan.md", []byte("# Acceptance\n\n### Task 1: Verify\n\n- [ ] fixture task\n"), 0o600))
			runGit(t, dir, "add", "plan.md", ".loopai/config")
			runGit(t, dir, "commit", "-m", "add acceptance fixtures")
			o.PlanFile = "plan.md"
			o.TasksOnly = true
			o.MaxIterations = 1
			require.NoError(t, run(t.Context(), o))
			invoked, readErr := os.ReadFile(invocationLog) //nolint:gosec // test-owned temporary path
			require.NoError(t, readErr)
			command, args, _ := strings.Cut(strings.TrimSuffix(string(invoked), "\n"), " ")
			assert.Equal(t, tt.wantCommand, command)
			assert.NotContains(t, args, "\n", "exactly one executor invocation")
			assert.Contains(t, args, tt.wantArgs, "the executor must receive the model without its provider")
		})
	}
}
