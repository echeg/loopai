package runs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopMarkerLifecycle(t *testing.T) {
	dir := t.TempDir()

	requested, err := StopRequested(dir)
	require.NoError(t, err)
	assert.False(t, requested)

	require.NoError(t, RequestStop(dir))
	requested, err = StopRequested(dir)
	require.NoError(t, err)
	assert.True(t, requested)
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(filepath.Join(dir, StopFile))
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	require.NoError(t, RequestStop(dir), "a repeated request is harmless")
	require.NoError(t, ClearStop(dir))
	requested, err = StopRequested(dir)
	require.NoError(t, err)
	assert.False(t, requested)
	require.NoError(t, ClearStop(dir), "clearing a missing marker is not an error")
}

func TestStopMarkerErrors(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))

	require.ErrorContains(t, RequestStop(filepath.Join(t.TempDir(), "missing")), "write stop marker")

	_, err := StopRequested(notDir)
	require.ErrorContains(t, err, "check stop marker")

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, StopFile), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, StopFile, "x"), []byte("x"), 0o600))
	require.ErrorContains(t, ClearStop(dir), "clear stop marker")
}
