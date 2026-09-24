package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
)

func TestWindowsExecutableModelProviders(t *testing.T) {
	t.Run("Claude executable allows a codex provider", func(t *testing.T) {
		cfg := &config.Config{ClaudeCommand: `C:\tools\CLAUDE.EXE`, TaskModel: "codex:gpt-6-astra:medium"}
		require.NoError(t, applyCLIOverrides(opts{}, cfg))
		assert.Equal(t, config.ExecutorCodex, cfg.TaskProvider)
		require.NoError(t, validateModelSpecs(opts{}, cfg))
	})
	for _, tt := range []struct {
		provider string
		opts     opts
		cfg      config.Config
	}{
		{"claude", opts{}, config.Config{ClaudeCommand: `C:\tools\claude.exe`, TaskModel: "claude:fable", PlanModel: "claude:gpt-6-astra"}},
		{"codex", opts{}, config.Config{CodexCommand: `C:\tools\CODEX.EXE`, TaskModel: "codex:gpt-6-astra", PlanModel: "codex:fable"}},
	} {
		t.Run(tt.provider+" executable rejects mismatched model", func(t *testing.T) {
			require.NoError(t, applyCLIOverrides(tt.opts, &tt.cfg))
			require.ErrorContains(t, validateModelSpecs(tt.opts, &tt.cfg), "model under the "+tt.provider+" provider")
		})
	}
}
