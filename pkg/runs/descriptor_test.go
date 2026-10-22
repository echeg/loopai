package runs

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDescriptorRoundTrip(t *testing.T) {
	root := t.TempDir()
	id := "feature-x-20261022-153000-a1b2"
	dir := Dir(root, id)
	require.NoError(t, os.MkdirAll(dir, 0o750))

	created := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)
	desc := &Descriptor{
		ID:          id,
		Attempt:     2,
		CreatedAt:   created,
		PID:         41231,
		PIDStart:    "1729610400.123456",
		Executable:  "/Users/u/.local/bin/loopai",
		Argv:        []string{"--task-model", "codex:gpt-6-astra:high", "--external-reviewers=", "docs/plans/20261022-feature-x.md"},
		EnvOptions:  map[string]string{"LOOPAI_CONFIG_DIR": "/Users/u/alt"},
		Cwd:         root,
		Mode:        "full",
		Plan:        "docs/plans/20261022-feature-x.md",
		Branch:      "feature-x",
		ProgressLog: filepath.Join(root, ".loopai", "progress", "progress-feature-x.txt"),
		Worktree:    filepath.Join(root, ".loopai", "worktrees", "feature-x"),
		ConsoleLog:  filepath.Join(dir, ConsoleLogFile),
		Origin: Origin{Kind: OriginT3Thread, ThreadID: "thread:provider:x%3Ay",
			ACPSessionID: "sess_123"},
	}
	before := time.Now().UTC()
	require.NoError(t, SaveDescriptor(dir, desc))

	assert.Equal(t, schemaVersion, desc.Version)
	assert.Equal(t, created, desc.CreatedAt, "an existing creation time is kept")
	assert.False(t, desc.UpdatedAt.Before(before), "the update time is stamped")

	loaded, err := LoadDescriptor(dir)
	require.NoError(t, err)
	assert.Equal(t, *desc, loaded)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, DescriptorFile))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	assertDirEntries(t, dir, DescriptorFile)
}

func TestSaveDescriptorStampsCreationTime(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plan-20261022-153000-a1b2")
	require.NoError(t, os.Mkdir(dir, 0o750))
	desc := &Descriptor{ID: "plan-20261022-153000-a1b2", Attempt: 1}
	require.NoError(t, SaveDescriptor(dir, desc))
	assert.False(t, desc.CreatedAt.IsZero())
	assert.Equal(t, desc.CreatedAt, desc.UpdatedAt)

	first := desc.CreatedAt
	require.NoError(t, SaveDescriptor(dir, desc))
	assert.Equal(t, first, desc.CreatedAt, "a later save keeps the creation time")
}

func TestDescriptorWireKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plan-1")
	require.NoError(t, os.Mkdir(dir, 0o750))
	desc := &Descriptor{ID: "plan-1", EnvOptions: map[string]string{"LOOPAI_ORCA": "1"},
		Origin: Origin{Kind: OriginT3Thread, ThreadID: "t", ACPSessionID: "s"}}
	require.NoError(t, SaveDescriptor(dir, desc))

	raw := readJSONObject(t, filepath.Join(dir, DescriptorFile))
	assert.ElementsMatch(t, []string{
		"version", "id", "attempt", "created_at", "updated_at", "pid", "pid_start", "executable",
		"argv", "env_options", "cwd", "mode", "plan", "branch", "progress_log", "worktree",
		"console_log", "origin",
	}, mapKeys(raw))
	origin, ok := raw["origin"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"kind": "t3-thread", "thread_id": "t", "acp_session_id": "s"}, origin)
}

func TestDescriptorOriginCLIOmitsThreadFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plan-1")
	require.NoError(t, os.Mkdir(dir, 0o750))
	require.NoError(t, SaveDescriptor(dir, &Descriptor{ID: "plan-1", Origin: Origin{Kind: OriginCLI}}))

	raw := readJSONObject(t, filepath.Join(dir, DescriptorFile))
	assert.Equal(t, map[string]any{"kind": "cli"}, raw["origin"])
}

func TestLoadDescriptorErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "corrupt json", content: `{"version": 1, "id": `, wantErr: "decode"},
		{name: "not an object", content: `[1, 2]`, wantErr: "decode"},
		{name: "unknown version", content: `{"version": 2, "id": "plan-1"}`, wantErr: "unsupported version 2"},
		{name: "missing version", content: `{"id": "plan-1"}`, wantErr: "unsupported version 0"},
		{name: "id of another directory", content: `{"version": 1, "id": "other-1"}`, wantErr: `id "other-1" does not match`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "plan-1")
			require.NoError(t, os.Mkdir(dir, 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(dir, DescriptorFile), []byte(tc.content), 0o600))

			_, err := LoadDescriptor(dir)
			require.ErrorContains(t, err, tc.wantErr)
			assert.NotErrorIs(t, err, fs.ErrNotExist)
		})
	}
}

func TestLoadDescriptorMissing(t *testing.T) {
	_, err := LoadDescriptor(filepath.Join(t.TempDir(), "plan-1"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestSaveDescriptorMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plan-1")
	require.ErrorContains(t, SaveDescriptor(dir, &Descriptor{ID: "plan-1"}), "save run descriptor")
	assert.NoDirExists(t, dir, "a pruned run directory is never recreated")
}

func readJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	return raw
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
