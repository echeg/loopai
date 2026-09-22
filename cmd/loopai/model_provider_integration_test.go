package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunModelProviderAcceptance(t *testing.T) {
	const inheritedModels = "task_model = gpt-6-astra:medium\nreview_model = gpt-6-astra:high\n"
	tests := []struct {
		name        string
		global      string
		local       string
		args        []string
		wantCommand string
		wantError   string
	}{
		{
			name:      "explicit global codex rejects Claude task",
			global:    "executor = codex\n",
			args:      []string{"--task-model", "fable:high"},
			wantError: `--task-model / task_model "fable:high" is a claude model, but the executor is codex (executor = codex in config)`,
		},
		{
			name:        "global task model infers codex",
			global:      inheritedModels,
			wantCommand: "codex",
		},
		{
			name:        "CLI models switch to Claude with codex reviewer",
			global:      inheritedModels,
			args:        []string{"--task-model", "fable:high", "--review-model", "fable:high", "--external-reviewers", "codex:gpt-6-astra:high"},
			wantCommand: "claude",
		},
		{
			name:      "inherited review model conflicts with inferred Claude",
			global:    inheritedModels,
			args:      []string{"--task-model", "fable:high", "--external-reviewers", "codex:gpt-6-astra:high"},
			wantError: `--review-model / review_model "gpt-6-astra:high" is a codex model, but the executor is claude (inferred from task_model "fable:high")`,
		},
		{
			name:      "local empty executor overrides global codex and inference",
			global:    "executor = codex\ntask_model = gpt-6-astra:medium\n",
			local:     "executor =\n",
			wantError: `--task-model / task_model "gpt-6-astra:medium" is a codex model, but the executor is claude (executor = (empty) in config)`,
		},
		{
			name:        "explicit codex wrapper accepts Claude model names",
			global:      "codex_command = codex-wrapper\ntask_model = fable\n",
			args:        []string{"--codex"},
			wantCommand: "codex-wrapper",
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
					"printf '%s\\n' '"+command+"' >> \"$MODEL_PROVIDER_INVOCATIONS\"\n"+
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
			assert.Equal(t, tt.wantCommand+"\n", string(invoked))
		})
	}
}
