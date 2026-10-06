package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveJSONStateOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	value := map[string]any{"version": 1, "plan": "docs/plans/a.md", "tasks": []int{1, 2}}

	require.NoError(t, saveJSONState(path, ".state-*.tmp", "state", value))

	want, err := json.Marshal(value)
	require.NoError(t, err)
	got, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, string(want)+"\n", string(got))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	assertNoTempFiles(t, filepath.Dir(path), ".state-*.tmp")
}

func TestSaveJSONStateEncodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	err := saveJSONState(path, ".state-*.tmp", "state", func() {})
	require.ErrorContains(t, err, "write state")
	assert.NoFileExists(t, path)
	assertNoTempFiles(t, filepath.Dir(path), ".state-*.tmp")
}

func TestWriteFileAtomicReplacesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))

	require.NoError(t, writeFileAtomic(path, ".file-*.tmp", "file", func(w io.Writer) error {
		_, err := io.WriteString(w, "new")
		return err //nolint:wrapcheck // test helper
	}))
	got, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	assertNoTempFiles(t, dir, ".file-*.tmp")

	err = writeFileAtomic(path, ".file-*.tmp", "file", func(io.Writer) error { return errors.New("boom") })
	require.ErrorContains(t, err, "write file: boom")
	got, err = os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "new", string(got), "a failed write must leave the previous file intact")
	assertNoTempFiles(t, dir, ".file-*.tmp")
}

func assertNoTempFiles(t *testing.T, dir, pattern string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	require.NoError(t, err)
	assert.Empty(t, matches)
}
