package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
)

func TestWindowsExecutableModelProviders(t *testing.T) {
	t.Run("Claude executable allows Codex inference", func(t *testing.T) {
		cfg := &config.Config{ClaudeCommand: `C:\tools\CLAUDE.EXE`, TaskModel: "gpt-6-astra:medium"}
		require.NoError(t, applyCodexOverrides(opts{}, cfg, io.Discard))
		assert.Equal(t, config.ExecutorCodex, cfg.Executor)
		require.NoError(t, validateStartupModels(opts{}, cfg))
	})
	for _, tt := range []struct {
		provider string
		opts     opts
		cfg      config.Config
	}{
		{"claude", opts{}, config.Config{ClaudeCommand: `C:\tools\claude.exe`, TaskModel: "fable", PlanModel: "gpt-6-astra"}},
		{"codex", opts{}, config.Config{CodexCommand: `C:\tools\CODEX.EXE`, TaskModel: "gpt-6-astra", PlanModel: "fable"}},
	} {
		t.Run(tt.provider+" executable rejects mismatched model", func(t *testing.T) {
			require.NoError(t, applyCodexOverrides(tt.opts, &tt.cfg, io.Discard))
			require.ErrorContains(t, validateStartupModels(tt.opts, &tt.cfg), "but the executor is "+tt.provider)
		})
	}
}
