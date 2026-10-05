package t3

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSettings(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, "userdata")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o600))
}

func TestFindLoopaiInstance(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    ProviderInstance
		wantErr string
	}{
		{name: "missing file"},
		{name: "no instances", content: `{"theme":"dark"}`},
		{name: "empty instances", content: `{"providerInstances":{}}`},
		{name: "null instances", content: `{"providerInstances":null}`},
		{name: "disabled instance", content: `{"providerInstances":{"loopai":{"driver":"grok","enabled":false,"config":{"binaryPath":"loopai-acp"}}}}`},
		{name: "disabled config", content: `{"providerInstances":{"loopai":{"driver":"grok","enabled":true,"config":{"enabled":false,"binaryPath":"loopai-acp"}}}}`},
		{name: "other driver", content: `{"providerInstances":{"loopai":{"driver":"codex","config":{"binaryPath":"loopai-acp"}}}}`},
		{name: "other binary", content: `{"providerInstances":{"loopai":{"driver":"grok","config":{"binaryPath":"grok"}}}}`},
		{name: "similar binary", content: `{"providerInstances":{"loopai":{"driver":"grok","config":{"binaryPath":"other-loopai-acp"}}}}`},
		{name: "missing config", content: `{"providerInstances":{"loopai":{"driver":"grok"}}}`},
		{name: "empty id", content: `{"providerInstances":{"":{"driver":"grok","config":{"binaryPath":"loopai-acp"}}}}`},
		{
			name:    "enabled defaults and unknown fields",
			content: `{"theme":"dark","providerInstances":{"custom":{"driver":"grok","extra":42,"config":{"binaryPath":"/opt/bin/loopai-acp","extra":true}}}}`,
			want:    ProviderInstance{ID: "custom", Driver: "grok", Enabled: true, BinaryPath: "/opt/bin/loopai-acp"},
		},
		{
			name:    "exe and explicit enabled flags",
			content: `{"providerInstances":{"win":{"driver":"grok","enabled":true,"config":{"enabled":true,"binaryPath":"C:\\tools\\loopai-acp.exe"}}}}`,
			want:    ProviderInstance{ID: "win", Driver: "grok", Enabled: true, BinaryPath: `C:\tools\loopai-acp.exe`},
		},
		{
			name:    "uppercase Windows extension",
			content: `{"providerInstances":{"win":{"driver":"grok","config":{"binaryPath":"C:\\tools\\loopai-acp.EXE"}}}}`,
			want:    ProviderInstance{ID: "win", Driver: "grok", Enabled: true, BinaryPath: `C:\tools\loopai-acp.EXE`},
		},
		{
			name:    "mixed case basename",
			content: `{"providerInstances":{"custom":{"driver":"grok","config":{"binaryPath":"LoopAI-ACP"}}}}`,
			want:    ProviderInstance{ID: "custom", Driver: "grok", Enabled: true, BinaryPath: "LoopAI-ACP"},
		},
		{
			name:    "sorted first wins skipping disabled",
			content: `{"providerInstances":{"z-last":{"driver":"grok","config":{"binaryPath":"loopai-acp.exe"}},"a-disabled":{"driver":"grok","enabled":false,"config":{"binaryPath":"loopai-acp"}},"b-first":{"driver":"grok","config":{"binaryPath":"loopai-acp"}}}}`,
			want:    ProviderInstance{ID: "b-first", Driver: "grok", Enabled: true, BinaryPath: "loopai-acp"},
		},
		{name: "malformed json", content: "{", wantErr: "parse settings file"},
		{name: "trailing json", content: `{} {}`, wantErr: "parse settings file"},
		{name: "invalid instances type", content: `{"providerInstances":[]}`, wantErr: "parse settings file"},
		{name: "invalid enabled type", content: `{"providerInstances":{"loopai":{"enabled":"false"}}}`, wantErr: "parse settings file"},
		{name: "oversized file", content: `{}` + strings.Repeat(" ", maxSettingsSize-1), wantErr: "settings file exceeds"},
		{name: "exact size limit", content: `{}` + strings.Repeat(" ", maxSettingsSize-2)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv(envT3Home, home)
			if tc.content != "" {
				writeSettings(t, home, tc.content)
			}
			got, found, err := FindLoopaiInstance(os.Getenv)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.False(t, found)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want.ID != "", found)
			assert.Equal(t, tc.want, got)
			if tc.content != "" {
				data, readErr := os.ReadFile(filepath.Join(home, "userdata", "settings.json")) //nolint:gosec // test-owned temporary directory
				require.NoError(t, readErr)
				assert.Equal(t, tc.content, string(data), "settings must remain unchanged")
			}
		})
	}
}

func TestFindLoopaiInstanceHomeOverride(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
	writeSettings(t, filepath.Join(fakeHome, ".t3"), "malformed default settings")
	home := t.TempDir()
	t.Setenv(envT3Home, "  "+home+"  ")
	writeSettings(t, home, `{"providerInstances":{"override":{"driver":"grok","config":{"binaryPath":"loopai-acp"}}}}`)
	got, found, err := FindLoopaiInstance(os.Getenv)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "override", got.ID)
}

func TestFindLoopaiInstanceReadError(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envT3Home, home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "userdata", "settings.json"), 0o750))
	got, found, err := FindLoopaiInstance(os.Getenv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "settings file")
	assert.False(t, found)
	assert.Empty(t, got)
}

func TestFindLoopaiInstancePlatformPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envT3Home, home)
	binary := filepath.Join(home, "bin", "loopai-acp.exe")
	settings := map[string]any{"providerInstances": map[string]any{
		"native": map[string]any{"driver": "grok", "config": map[string]any{"binaryPath": binary}},
	}}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	writeSettings(t, home, string(data))
	got, found, err := FindLoopaiInstance(os.Getenv)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, binary, got.BinaryPath)
}
